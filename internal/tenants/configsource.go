package tenants

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/schemas"
)

// credentialKey matches a key that looks like an inline secret. credentialsRef, the
// only allowed way to name one, is excepted.
var credentialKey = regexp.MustCompile(`(?i)(secret|token|password|passwd|api[-_]?key|private[-_]?key|credential)`)

// TenantConfig returns a tenant's configuration, loading it on first use. An unknown
// tenant is not found. A present but invalid file is an error, never a fallback.
func (s *Service) TenantConfig(ctx context.Context, tenant domain.TenantID) (config.Tenant, error) {
	s.mu.RLock()
	cached, ok := s.config[tenant]
	s.mu.RUnlock()
	if ok {
		return cached, nil
	}
	if _, err := s.system.Tenants().Get(ctx, tenant); err != nil {
		return config.Tenant{}, err
	}
	loaded, err := s.loadConfig(tenant)
	if err != nil {
		return config.Tenant{}, err
	}
	s.mu.Lock()
	s.config[tenant] = loaded
	s.mu.Unlock()
	return loaded, nil
}

// Reload rereads every tenant's files, for SIGHUP (I34). Tenants that load cleanly are
// swapped in; a tenant with a bad file keeps its working configuration, and the error
// names it, so a bad edit never removes what runs (I115).
func (s *Service) Reload(ctx context.Context) error {
	tenants, err := s.system.Tenants().List(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, t := range tenants {
		loaded, err := s.loadConfig(t.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("tenant %s: %w", t.ID, err))
			continue
		}
		s.mu.Lock()
		s.config[t.ID] = loaded
		s.mu.Unlock()
	}
	return errors.Join(failures...)
}

func (s *Service) tenantDir(tenant domain.TenantID) string {
	return filepath.Join(s.dir, "tenants", string(tenant))
}

// loadConfig reads and validates one tenant's files. A missing file is an empty
// section. With no configuration directory, everything is empty.
func (s *Service) loadConfig(tenant domain.TenantID) (config.Tenant, error) {
	if err := tenant.Validate(); err != nil {
		return config.Tenant{}, errs.NotFound("tenant")
	}
	out := config.Tenant{AI: config.AI{DefaultProcessing: domain.AILocalOnly}}
	if s.dir == "" {
		return out, nil
	}
	dir := s.tenantDir(tenant)

	if data, err := readConfigFile(dir, "storages.json"); err != nil {
		return config.Tenant{}, err
	} else if data != nil {
		if err := validateConfig("storages.json", data); err != nil {
			return config.Tenant{}, err
		}
		if err := decodeStrict("storages.json", data, &out.Storages); err != nil {
			return config.Tenant{}, err
		}
		if err := config.ValidateStorages(out.Storages); err != nil {
			return config.Tenant{}, invalidConfig("storages.json", err)
		}
	}

	if data, err := readConfigFile(dir, "publishing.json"); err != nil {
		return config.Tenant{}, err
	} else if data != nil {
		if err := validateConfig("publishing.json", data); err != nil {
			return config.Tenant{}, err
		}
		var file config.PublishingFile
		if err := decodeStrict("publishing.json", data, &file); err != nil {
			return config.Tenant{}, err
		}
		out.Publishing = file.Publishing
	}

	if data, err := readConfigFile(dir, "ai.json"); err != nil {
		return config.Tenant{}, err
	} else if data != nil {
		wrapped, err := wrapIfBare("ai.json", "ai", data)
		if err != nil {
			return config.Tenant{}, err
		}
		if err := validateConfig("ai.json", wrapped); err != nil {
			return config.Tenant{}, err
		}
		var file struct {
			AI config.AI `json:"ai"`
		}
		if err := decodeStrict("ai.json", wrapped, &file); err != nil {
			return config.Tenant{}, err
		}
		out.AI = file.AI
		if out.AI.DefaultProcessing == "" {
			out.AI.DefaultProcessing = domain.AILocalOnly
		}
	}

	if data, err := readConfigFile(dir, "catalogs.json"); err != nil {
		return config.Tenant{}, err
	} else if data != nil {
		wrapped, err := wrapIfBare("catalogs.json", "catalogs", data)
		if err != nil {
			return config.Tenant{}, err
		}
		if err := validateConfig("catalogs.json", wrapped); err != nil {
			return config.Tenant{}, err
		}
		var file struct {
			Catalogs config.Catalogs `json:"catalogs"`
		}
		if err := decodeStrict("catalogs.json", wrapped, &file); err != nil {
			return config.Tenant{}, err
		}
		out.Catalogs = file.Catalogs
	}

	if data, err := readConfigFile(dir, "observability.json"); err != nil {
		return config.Tenant{}, err
	} else if data != nil {
		if err := validateConfig("observability.json", data); err != nil {
			return config.Tenant{}, err
		}
		var file config.ObservabilityFile
		if err := decodeStrict("observability.json", data, &file); err != nil {
			return config.Tenant{}, err
		}
		out.Observability = file.Observability
	}

	if data, err := readConfigFile(dir, "hooks.json"); err != nil {
		return config.Tenant{}, err
	} else if data != nil {
		if err := refuseInlineCredentials("hooks.json", data); err != nil {
			return config.Tenant{}, err
		}
		if err := decodeStrict("hooks.json", data, &out.Hooks); err != nil {
			return config.Tenant{}, err
		}
	}

	if data, err := readConfigFile(dir, "budget.json"); err != nil {
		return config.Tenant{}, err
	} else if data != nil {
		if err := refuseInlineCredentials("budget.json", data); err != nil {
			return config.Tenant{}, err
		}
		if err := decodeStrict("budget.json", data, &out.Budget); err != nil {
			return config.Tenant{}, err
		}
	}
	return out, nil
}

func invalidConfig(name string, err error) error {
	return errs.Wrap(errs.CodeValidationFailed, err, fmt.Sprintf("%s: %v", name, err))
}

func readConfigFile(dir, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return data, nil
}

// validateConfig applies the connector schema, which refuses inline credentials and
// unknown keys (§9.1).
func validateConfig(name string, data []byte) error {
	if err := refuseInlineCredentials(name, data); err != nil {
		return err
	}
	if err := schemas.Validate(schemas.ConnectorConfig, data); err != nil {
		return invalidConfig(name, err)
	}
	return nil
}

// refuseInlineCredentials walks every key of a configuration file.
func refuseInlineCredentials(name string, data []byte) error {
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return invalidConfig(name, err)
	}
	var walk func(v any) error
	walk = func(v any) error {
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if k != "credentialsRef" && credentialKey.MatchString(k) {
					return fmt.Errorf("key %q looks like an inline credential: use credentialsRef", k)
				}
				if err := walk(t[k]); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range t {
				if err := walk(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(doc); err != nil {
		return invalidConfig(name, err)
	}
	return nil
}

// wrapIfBare accepts both forms of I115: the wrapped one the schema validates and the
// bare one, which it wraps. The wrapped form is canonical.
func wrapIfBare(name, key string, data []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, invalidConfig(name, err)
	}
	if _, wrapped := doc[key]; wrapped {
		return data, nil
	}
	delete(doc, "$schema")
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, invalidConfig(name, err)
	}
	return json.Marshal(map[string]json.RawMessage{key: body})
}

// decodeStrict decodes one JSON value and refuses keys the type does not know. The
// schemas' optional $schema key is dropped first.
func decodeStrict(name string, data []byte, v any) error {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return invalidConfig(name, err)
	}
	if _, ok := doc["$schema"]; ok {
		delete(doc, "$schema")
		stripped, err := json.Marshal(doc)
		if err != nil {
			return invalidConfig(name, err)
		}
		data = stripped
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalidConfig(name, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalidConfig(name, errors.New("trailing data after the JSON value"))
	}
	return nil
}
