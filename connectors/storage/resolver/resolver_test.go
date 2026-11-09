package resolver_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/connectors/storage/local"
	"github.com/rafaelp/vidyom/connectors/storage/resolver"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

type tenants struct {
	configs map[domain.TenantID]config.Storages
}

func (t *tenants) TenantConfig(_ context.Context, tenant domain.TenantID) (config.Tenant, error) {
	s, ok := t.configs[tenant]
	if !ok {
		return config.Tenant{}, errs.NotFound("tenant")
	}
	return config.Tenant{Storages: s}, nil
}

func localRoles(t *testing.T, base string) config.Storages {
	t.Helper()
	inst := func(role string, usage config.StorageUsage) config.StorageInstance {
		return config.StorageInstance{Driver: local.Driver, Root: filepath.Join(base, role), Usage: usage}
	}
	return config.Storages{Roles: map[domain.StorageRole]config.StorageInstance{
		domain.RoleArchive: inst("archive", config.UsageBackupOnly),
		domain.RoleMaster:  inst("master", config.UsageOperational),
		domain.RoleProxy:   inst("proxy", config.UsageOperational),
	}}
}

func newResolver(t *testing.T, configs map[domain.TenantID]config.Storages, assets resolver.AssetFiles) (*resolver.Resolver, *tenants) {
	t.Helper()
	registry := connector.NewRegistry()
	local.Register(registry)
	src := &tenants{configs: configs}
	r := resolver.New(resolver.Options{Drivers: registry, Tenants: src, Assets: assets})
	t.Cleanup(func() { _ = r.Close() })
	return r, src
}

func scope(tenant domain.TenantID) service.Scope { return service.Scope{Tenant: tenant} }

func TestStorageOpensByRoleAndReusesInstances(t *testing.T) {
	r, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": localRoles(t, t.TempDir())}, nil)
	ctx := context.Background()
	a, err := r.Storage(ctx, "acme", domain.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	if a.Role() != domain.RoleMaster {
		t.Fatalf("role %s", a.Role())
	}
	b, err := r.Storage(ctx, "acme", domain.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("an unchanged configuration reopened the instance")
	}
}

// TestSwitchingAnInstanceChangesNoWorkflow is §15.4 with two local instances: the
// role keeps its name, the bytes go to the new location.
func TestSwitchingAnInstanceChangesNoWorkflow(t *testing.T) {
	base := t.TempDir()
	cfg := localRoles(t, base)
	r, src := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, nil)
	ctx := context.Background()
	s, err := r.Storage(ctx, "acme", domain.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("in first"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	second := localRoles(t, base)
	inst := second.Roles[domain.RoleMaster]
	inst.Root = filepath.Join(base, "master-second")
	second.Roles[domain.RoleMaster] = inst
	src.configs["acme"] = second
	s2, err := r.Storage(ctx, "acme", domain.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s2.Exists(ctx, "k"); err != nil || ok {
		t.Fatalf("the new instance holds data nobody migrated: %v %v", ok, err)
	}
	if _, err := s2.Put(ctx, "k", strings.NewReader("in second"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "master-second", "acme", "k")); err != nil {
		t.Fatalf("the write did not reach the new location: %v", err)
	}
}

func TestOverlappingNamespacesFailClosed(t *testing.T) {
	base := t.TempDir()
	cfg := localRoles(t, base)
	proxy := cfg.Roles[domain.RoleProxy]
	proxy.Root = filepath.Join(base, "master", "proxies")
	cfg.Roles[domain.RoleProxy] = proxy
	r, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, nil)
	if _, err := r.Storage(context.Background(), "acme", domain.RoleMaster); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("overlap accepted: %v", err)
	}
}

func TestUnknownsAreRefused(t *testing.T) {
	r, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": localRoles(t, t.TempDir())}, nil)
	ctx := context.Background()
	if _, err := r.Storage(ctx, "globex", domain.RoleMaster); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("unknown tenant: %v", err)
	}
	if _, err := r.Storage(ctx, "../acme", domain.RoleMaster); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("unsafe tenant: %v", err)
	}
	if _, err := r.Storage(ctx, "acme", "cold"); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("unknown role: %v", err)
	}
	broken := localRoles(t, t.TempDir())
	inst := broken.Roles[domain.RoleMaster]
	inst.Driver = "no-such-driver"
	broken.Roles[domain.RoleMaster] = inst
	r2, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": broken}, nil)
	if _, err := r2.Storage(ctx, "acme", domain.RoleMaster); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("unknown driver: %v", err)
	}
}

func TestListReportsRolesWithoutSecrets(t *testing.T) {
	cfg := localRoles(t, t.TempDir())
	inst := cfg.Roles[domain.RoleArchive]
	inst.CredentialsRef = "archive-credential"
	cfg.Roles[domain.RoleArchive] = inst
	r, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, nil)
	list, err := r.List(context.Background(), scope("acme"))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("%+v", list)
	}
	for _, entry := range list {
		if entry.Driver != local.Driver || entry.Usage == "" {
			t.Errorf("%+v", entry)
		}
	}
	if !list[1].Capabilities.DirectAccess || list[0].Capabilities.DirectAccess {
		t.Errorf("direct access by usage: %+v", list)
	}
	encoded, _ := json.Marshal(list)
	if strings.Contains(string(encoded), "archive-credential") {
		t.Fatal("a credential reference leaked")
	}
}

func TestTestNeverTouchesArchiveData(t *testing.T) {
	base := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "access.jsonl")
	cfg := localRoles(t, base)
	inst := cfg.Roles[domain.RoleArchive]
	inst.Parameters, _ = json.Marshal(map[string]any{"accessLog": logPath})
	cfg.Roles[domain.RoleArchive] = inst
	r, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, nil)
	res, err := r.Test(context.Background(), scope("acme"), domain.RoleArchive)
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.Contains(line, `"class":"metadata"`) {
			t.Errorf("a test of archive did more than metadata: %s", line)
		}
	}
}

func TestTestRoundTripsOnOperationalRoles(t *testing.T) {
	base := t.TempDir()
	r, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": localRoles(t, base)}, nil)
	ctx := context.Background()
	res, err := r.Test(ctx, scope("acme"), domain.RoleProxy)
	if err != nil || !res.OK {
		t.Fatalf("%+v %v", res, err)
	}
	s, err := r.Storage(ctx, "acme", domain.RoleProxy)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.List(ctx, connector.ListOptions{})
	if err != nil || len(page.Objects) != 0 {
		t.Fatalf("the probe stayed: %+v %v", page, err)
	}
}

func TestTestReportsABrokenRole(t *testing.T) {
	cfg := localRoles(t, t.TempDir())
	inst := cfg.Roles[domain.RoleMaster]
	inst.Root = "relative"
	cfg.Roles[domain.RoleMaster] = inst
	r, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, nil)
	res, err := r.Test(context.Background(), scope("acme"), domain.RoleMaster)
	if err != nil || res.OK || res.Detail == "" {
		t.Fatalf("%+v %v", res, err)
	}
}

type fakeAssets struct {
	asset    domain.Asset
	versions []domain.AssetVersion
	err      error
}

func (f fakeAssets) Get(context.Context, service.Scope, id.Ref) (domain.Asset, error) {
	return f.asset, f.err
}

func (f fakeAssets) Versions(context.Context, service.Scope, id.Ref) ([]domain.AssetVersion, error) {
	return f.versions, f.err
}

func TestVerifyChecksEveryFileOfTheRole(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	cfg := localRoles(t, base)
	registry := connector.NewRegistry()
	local.Register(registry)
	direct := resolver.New(resolver.Options{Drivers: registry, Tenants: &tenants{configs: map[domain.TenantID]config.Storages{"acme": cfg}}})
	defer func() { _ = direct.Close() }()
	s, err := direct.Storage(ctx, "acme", domain.RoleMaster)
	if err != nil {
		t.Fatal(err)
	}
	good, err := s.Put(ctx, "good", strings.NewReader("good"), connector.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := s.Put(ctx, "bad", strings.NewReader("bad"), connector.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "master", "acme", "bad"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := func(key, sum string, role domain.StorageRole) domain.VersionFile {
		return domain.VersionFile{File: domain.MediaFile{Storage: role, Key: key, SHA256: sum}}
	}
	assets := fakeAssets{versions: []domain.AssetVersion{{Files: []domain.VersionFile{
		file("good", good.SHA256, domain.RoleMaster),
		file("bad", bad.SHA256, domain.RoleMaster),
		file("unrecorded", "", domain.RoleMaster),
		file("elsewhere", "x", domain.RoleProxy),
	}}}}
	r, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, assets)
	res, err := r.Verify(ctx, scope("acme"), domain.RoleMaster, api.StorageVerifyRequest{AssetID: "ORI-001"})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Checked != 3 || len(res.Failed) != 2 {
		t.Fatalf("%+v", res)
	}
	res, err = r.Verify(ctx, scope("acme"), domain.RoleArchive, api.StorageVerifyRequest{AssetID: "ORI-001"})
	if err != nil || res.OK || res.Checked != 0 {
		t.Fatalf("an asset with nothing to check passed: %+v %v", res, err)
	}
}

func TestProxyLink(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	cfg := localRoles(t, base)
	registry := connector.NewRegistry()
	local.Register(registry)
	direct := resolver.New(resolver.Options{Drivers: registry, Tenants: &tenants{configs: map[domain.TenantID]config.Storages{"acme": cfg}}})
	defer func() { _ = direct.Close() }()
	s, err := direct.Storage(ctx, "acme", domain.RoleProxy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "originals/ORI-001/v1/proxy.mp4", strings.NewReader("x"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	assets := fakeAssets{asset: domain.Asset{Proxy: &domain.MediaFile{Storage: domain.RoleProxy, Key: "originals/ORI-001/v1/proxy.mp4"}}}
	r, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, assets)
	link, err := r.ProxyLink(ctx, scope("acme"), "ORI-001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link.URL, "file://") || link.ExpiresAt.IsZero() {
		t.Fatalf("%+v", link)
	}
	r2, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, fakeAssets{})
	if _, err := r2.ProxyLink(ctx, scope("acme"), "ORI-001"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("no proxy: %v", err)
	}
	r3, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, fakeAssets{err: errs.NotFound("asset")})
	if _, err := r3.ProxyLink(ctx, scope("globex"), "ORI-001"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("cross-tenant: %v", err)
	}
	r4, _ := newResolver(t, map[domain.TenantID]config.Storages{"acme": cfg}, nil)
	if _, err := r4.ProxyLink(ctx, scope("acme"), "ORI-001"); err == nil {
		t.Fatal("proxy link with no catalog succeeded")
	}
}
