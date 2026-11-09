package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/daemon"
	"github.com/rafaelp/vidyom/internal/persistence"
	"github.com/rafaelp/vidyom/internal/tenants"
)

// Every sample in config/ must load through the validators a daemon uses, because a
// misspelled key fails closed at start (I138, I115).
func TestConfigSamplesLoad(t *testing.T) {
	root := filepath.Join("..", "..", "config")
	for _, set := range []string{"local", "compose", "samples"} {
		t.Run(set, func(t *testing.T) {
			dir := materialize(t, filepath.Join(root, set))
			cfg, err := daemon.LoadConfig(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := daemon.ValidateConfig(cfg); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			stores, err := persistence.Open(ctx, persistence.Options{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer stores.Close()
			if _, err := stores.Tenants.Create(ctx, "acme"); err != nil {
				t.Fatal(err)
			}
			if err := stores.System.Tenants().Create(ctx, domain.Tenant{ID: "acme", Name: "Acme"}); err != nil {
				t.Fatal(err)
			}
			service := tenants.New(stores.System, stores.Tenants, tenants.Options{ConfigDir: dir})
			tenant, err := service.TenantConfig(ctx, domain.TenantID("acme"))
			if err != nil {
				t.Fatal(err)
			}
			for _, role := range domain.StorageRoles {
				if tenant.Storages.Roles[role].Driver == "" {
					t.Errorf("role %s has no driver", role)
				}
			}
			if tenant.Storages.Roles[domain.RoleMaster].Usage != "operational" || tenant.Storages.Roles[domain.RoleProxy].Usage != "operational" {
				t.Error("master and proxy must declare usage operational, or video.cut fails closed")
			}
		})
	}
}

func TestComposeRefusalVariantsLoad(t *testing.T) {
	root := filepath.Join("..", "..", "config", "compose")
	if _, err := daemon.LoadConfig(filepath.Join(root, "no-provider")); err != nil {
		t.Fatal(err)
	}
	cfg, err := daemon.LoadConfig(filepath.Join(root, "no-provider"))
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.ValidateConfig(cfg); err == nil {
		t.Error("no-provider should not validate: it exists to prove the refusal")
	}
	cfg, err = daemon.LoadConfig(filepath.Join(root, "missing-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.ValidateConfig(cfg); err != nil {
		t.Errorf("missing-key must validate, and fail later at unsealing: %v", err)
	}
}

// materialize renders the templates of config/local with harmless values.
func materialize(t *testing.T, from string) string {
	t.Helper()
	to := t.TempDir()
	replace := strings.NewReplacer("@ROOT@", "/var/tmp/vidyom-sample", "@REDIS_PORT@", "16379", "@HTTPS_PORT@", "18443", "@MOCK_LLM_PORT@", "18111")
	err := filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, []byte(replace.Replace(string(data))), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return to
}
