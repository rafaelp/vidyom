package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
)

// GrantFileName is the tenant file holding plugin grants, beside the other per-tenant
// settings of I34.
const GrantFileName = "plugins.json"

var tenantSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// Grant is what one tenant allows one plugin (I261).
type Grant struct {
	Enabled     bool               `json:"enabled"`
	Permissions plugin.Permissions `json:"permissions"`
}

// GrantFile is the document in <config-dir>/tenants/<tenant>/plugins.json.
type GrantFile struct {
	Plugins map[string]Grant `json:"plugins"`
}

// GrantSource supplies tenant grants. found is false when the tenant says nothing
// about this plugin, which means it does not run there.
type GrantSource interface {
	PluginGrant(ctx context.Context, tenant domain.TenantID, pluginID string) (grant Grant, found bool, err error)
}

// FileGrants reads grants from the configuration directory, on each call, so an edit
// takes effect at the next use without a restart.
type FileGrants struct {
	ConfigDir string
}

// PluginGrant implements GrantSource. An unreadable or malformed file is an error, and
// the caller refuses: a typo must not grant or silently revoke anything unnoticed.
func (f FileGrants) PluginGrant(_ context.Context, tenant domain.TenantID, pluginID string) (Grant, bool, error) {
	if f.ConfigDir == "" {
		return Grant{}, false, nil
	}
	if !tenantSlug.MatchString(string(tenant)) {
		return Grant{}, false, errs.New(errs.CodeInvalidArgument, "invalid tenant")
	}
	data, err := os.ReadFile(filepath.Join(f.ConfigDir, "tenants", string(tenant), GrantFileName))
	if errors.Is(err, os.ErrNotExist) {
		return Grant{}, false, nil
	}
	if err != nil {
		return Grant{}, false, fmt.Errorf("read plugin grants: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var file GrantFile
	if err := dec.Decode(&file); err != nil {
		return Grant{}, false, errs.Wrap(errs.CodeFailedPrecondition, err, "the tenant's "+GrantFileName+" is not valid")
	}
	grant, found := file.Plugins[pluginID]
	return grant, found, nil
}

// effectivePermissions is the intersection the runtime enforces. A grant naming what the
// manifest does not declare is a configuration error, so that a typo fails loudly
// rather than narrowing silently (I261).
func effectivePermissions(m plugin.Manifest, g Grant) (plugin.Permissions, error) {
	check := func(kind string, granted, declared []string) ([]string, error) {
		out := make([]string, 0, len(granted))
		for _, item := range granted {
			if !slices.Contains(declared, item) {
				return nil, errs.Newf(errs.CodeFailedPrecondition, "the tenant grants %s %q, which plugin %s does not declare", kind, item, m.ID)
			}
			out = append(out, item)
		}
		sort.Strings(out)
		return slices.Compact(out), nil
	}
	var p plugin.Permissions
	var err error
	if p.Network, err = check("network", g.Permissions.Network, m.Permissions.Network); err != nil {
		return p, err
	}
	if p.Secrets, err = check("secret", g.Permissions.Secrets, m.Permissions.Secrets); err != nil {
		return p, err
	}
	if p.Filesystem, err = check("filesystem", g.Permissions.Filesystem, m.Permissions.Filesystem); err != nil {
		return p, err
	}
	if p.Events, err = check("event", g.Permissions.Events, m.Permissions.Events); err != nil {
		return p, err
	}
	return p, nil
}

func permissionsKey(p plugin.Permissions) string {
	data, _ := json.Marshal(p)
	return string(data)
}

// overlaps reports whether a and b are the same path or one contains the other.
func overlaps(a, b string) bool {
	if a == b {
		return true
	}
	sep := string(filepath.Separator)
	return hasPathPrefix(a, b, sep) || hasPathPrefix(b, a, sep)
}

func hasPathPrefix(path, prefix, sep string) bool {
	if prefix == sep {
		return true
	}
	return len(path) > len(prefix) && path[:len(prefix)] == prefix && path[len(prefix):len(prefix)+1] == sep
}

// checkForbiddenPaths refuses a filesystem permission that reaches the daemon's own
// directories (I264).
func checkForbiddenPaths(p plugin.Permissions, forbidden []string) error {
	for _, entry := range p.Filesystem {
		path, _, err := parseFilesystemEntry(entry)
		if err != nil {
			return err
		}
		if path == "" {
			continue
		}
		for _, f := range forbidden {
			if overlaps(path, f) {
				return errs.Newf(errs.CodeValidationFailed, "filesystem permission %q overlaps a directory of the daemon", entry)
			}
		}
	}
	return nil
}
