package tenants

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/persistence"
	"github.com/rafaelp/vidyom/internal/security/audit"
	"github.com/rafaelp/vidyom/internal/security/authentication"
)

var ctx = context.Background()

type fakeKeys struct {
	err     error
	tenants []domain.TenantID
}

func (f *fakeKeys) EnsureDataKey(_ context.Context, tenant domain.TenantID) error {
	f.tenants = append(f.tenants, tenant)
	return f.err
}

type fixture struct {
	stores *persistence.Stores
	svc    *Service
	keys   *fakeKeys
	cfgDir string
	now    *time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stores.Close() })
	keys := &fakeKeys{}
	cfgDir := t.TempDir()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	clock := &now
	svc := New(stores.System, stores.Tenants, Options{ConfigDir: cfgDir, Keys: keys, Clock: func() time.Time { return *clock }})
	return &fixture{stores, svc, keys, cfgDir, clock}
}

func (f *fixture) admin(t *testing.T) domain.Principal {
	t.Helper()
	u := domain.User{ULID: id.New(), Email: "root@example.com", SystemAdmin: true}
	if err := f.stores.System.Users().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	return domain.Principal{UserULID: u.ULID, Email: u.Email, SystemAdmin: true, Method: domain.AuthDevice}
}

func TestCreateTenant(t *testing.T) {
	f := setup(t)
	root := f.admin(t)
	tn, err := f.svc.Create(ctx, root, api.CreateTenantRequest{ID: "acme", Name: "Acme Inc."})
	if err != nil {
		t.Fatal(err)
	}
	if tn.ID != "acme" || tn.StorageRoot != "acme/" || tn.Name != "Acme Inc." || tn.ULID == "" {
		t.Fatalf("tenant = %+v", tn)
	}
	if _, err := f.stores.Tenants.ForTenant(ctx, "acme"); err != nil {
		t.Fatalf("no database: %v", err)
	}
	if len(f.keys.tenants) != 1 || f.keys.tenants[0] != "acme" {
		t.Fatalf("data key = %v", f.keys.tenants)
	}
	// The creator can use it.
	m, err := f.stores.System.Users().Membership(ctx, root.UserULID, "acme")
	if err != nil || m.Role != domain.RoleAdmin {
		t.Fatalf("membership = %+v, %v", m, err)
	}
	if _, err := f.svc.Create(ctx, root, api.CreateTenantRequest{ID: "acme"}); !errs.IsCode(err, errs.CodeConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := f.svc.Create(ctx, root, api.CreateTenantRequest{ID: "../evil"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad slug: %v", err)
	}
	notAdmin := domain.Principal{UserULID: id.New(), Email: "x@example.com"}
	if _, err := f.svc.Create(ctx, notAdmin, api.CreateTenantRequest{ID: "beta"}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("not a system admin: %v", err)
	}
	if _, err := f.svc.Create(ctx, domain.Principal{SystemAdmin: true}, api.CreateTenantRequest{ID: "beta"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no user: %v", err)
	}
	// Audited in both chains.
	auditor := audit.New(f.stores.Tenants, audit.Options{System: f.stores.System})
	for _, chain := range []domain.TenantID{"", "acme"} {
		v, err := auditor.Verify(ctx, chain)
		if err != nil || !v.OK || v.Records != 1 {
			t.Fatalf("chain %q = %+v, %v", chain, v, err)
		}
	}
}

// D13: a sealed vault stops tenant creation, and nothing half-made reaches a request.
func TestCreateFailsClosedWithoutADataKey(t *testing.T) {
	f := setup(t)
	root := f.admin(t)
	f.keys.err = errs.New(errs.CodeSealed, "sealed")
	if _, err := f.svc.Create(ctx, root, api.CreateTenantRequest{ID: "acme"}); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("sealed: %v", err)
	}
	if _, err := f.stores.System.Tenants().Get(ctx, "acme"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("the tenant must not exist yet: %v", err)
	}
	// The next attempt adopts the orphan database (I114).
	f.keys.err = nil
	if _, err := f.svc.Create(ctx, root, api.CreateTenantRequest{ID: "acme"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	noKeys := New(f.stores.System, f.stores.Tenants, Options{})
	if _, err := noKeys.Create(ctx, root, api.CreateTenantRequest{ID: "beta"}); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("no vault: %v", err)
	}
}

func TestListTenants(t *testing.T) {
	f := setup(t)
	root := f.admin(t)
	for _, tn := range []domain.TenantID{"acme", "beta"} {
		if _, err := f.svc.Create(ctx, root, api.CreateTenantRequest{ID: tn}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := f.svc.List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Role != domain.RoleAdmin {
		t.Fatalf("all = %+v", all)
	}
	u := domain.User{ULID: id.New(), Email: "maria@example.com"}
	if err := f.stores.System.Users().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := f.stores.System.Users().SetMembership(ctx, domain.Membership{UserULID: u.ULID, TenantID: "beta", Role: domain.RoleEditor}); err != nil {
		t.Fatal(err)
	}
	mine, err := f.svc.List(ctx, domain.Principal{UserULID: u.ULID})
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].ID != "beta" || mine[0].Role != domain.RoleEditor {
		t.Fatalf("mine = %+v", mine)
	}
	none, err := f.svc.List(ctx, domain.Principal{Method: domain.AuthPeer})
	if err != nil {
		t.Fatal(err)
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("none = %+v", none)
	}
}

func TestUsers(t *testing.T) {
	f := setup(t)
	root := f.admin(t)
	if _, err := f.svc.Create(ctx, root, api.CreateTenantRequest{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, root, api.CreateTenantRequest{ID: "beta"}); err != nil {
		t.Fatal(err)
	}
	svc := f.svc.Users()
	scope := service.Scope{Tenant: "acme", Principal: root, RequestID: "req-1"}

	m, err := svc.Add(ctx, scope, api.AddUserRequest{Email: " Maria@Example.com ", Role: domain.RoleEditor})
	if err != nil {
		t.Fatal(err)
	}
	if m.Email != "maria@example.com" || m.Role != domain.RoleEditor || m.TenantID != "acme" {
		t.Fatalf("membership = %+v", m)
	}
	if _, err := svc.Add(ctx, scope, api.AddUserRequest{Email: "nope", Role: domain.RoleEditor}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad email: %v", err)
	}
	if _, err := svc.Add(ctx, scope, api.AddUserRequest{Email: "x@example.com", Role: "owner"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad role: %v", err)
	}
	if _, err := svc.Add(ctx, service.Scope{Tenant: "missing"}, api.AddUserRequest{Email: "x@example.com", Role: domain.RoleViewer}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}

	// A user may belong to several tenants, with one role in each (D6).
	betaScope := service.Scope{Tenant: "beta", Principal: root}
	if _, err := svc.Add(ctx, betaScope, api.AddUserRequest{Email: "maria@example.com", Role: domain.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	all, err := f.stores.System.Users().Memberships(ctx, m.UserULID)
	if err != nil || len(all) != 2 {
		t.Fatalf("memberships = %+v, %v", all, err)
	}

	// D48: an individual grant, without changing the role.
	got, err := svc.Grant(ctx, scope, "maria@example.com", api.GrantRequest{Permission: domain.PermStorageArchiveRestore})
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != domain.RoleEditor || len(got.Grants) != 1 {
		t.Fatalf("granted = %+v", got)
	}
	got, err = svc.Grant(ctx, scope, string(m.UserULID), api.GrantRequest{Permission: domain.PermStorageArchiveRestore})
	if err != nil || len(got.Grants) != 1 {
		t.Fatalf("a repeated grant must stay one: %+v, %v", got, err)
	}
	if _, err := svc.Grant(ctx, scope, "maria@example.com", api.GrantRequest{Permission: domain.PermSystemAdmin}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("system permission: %v", err)
	}
	if _, err := svc.Grant(ctx, scope, "maria@example.com", api.GrantRequest{Permission: "made.up"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("unknown permission: %v", err)
	}
	if _, err := svc.Grant(ctx, scope, "nobody@example.com", api.GrantRequest{Permission: domain.PermAssetRead}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	// Someone of another tenant is not found here.
	outsider, err := svc.Add(ctx, betaScope, api.AddUserRequest{Email: "other@example.com", Role: domain.RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grant(ctx, scope, outsider.Email, api.GrantRequest{Permission: domain.PermAssetRead}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	// Changing the role keeps the grant; revoking removes it.
	again, err := svc.Add(ctx, scope, api.AddUserRequest{Email: "maria@example.com", Role: domain.RolePublisher})
	if err != nil || again.Role != domain.RolePublisher || len(again.Grants) != 1 {
		t.Fatalf("role change = %+v, %v", again, err)
	}
	revoked, err := svc.Grant(ctx, scope, "maria@example.com", api.GrantRequest{Permission: domain.PermStorageArchiveRestore, Revoke: true})
	if err != nil || len(revoked.Grants) != 0 {
		t.Fatalf("revoked = %+v, %v", revoked, err)
	}

	members, err := svc.List(ctx, scope)
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %+v, %v", members, err)
	}
	// D48: the audit records which permission and what was granted.
	store, _ := f.stores.Tenants.ForTenant(ctx, "acme")
	records, _, err := store.Audit().List(ctx, repo.AuditFilter{Action: "user.grant"}, repo.Page{})
	if err != nil || len(records) != 2 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	if records[0].Permission != domain.PermUserManage || !strings.Contains(string(records[0].Details), string(domain.PermStorageArchiveRestore)) {
		t.Fatalf("record = %+v", records[0])
	}
}

func TestBootstrap(t *testing.T) {
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer stores.Close()
	auth := authentication.New(stores.System, authentication.Options{})
	svc := New(stores.System, stores.Tenants, Options{Keys: &fakeKeys{}})

	if _, err := svc.Bootstrap(ctx, BootstrapRequest{Tenant: "acme", AdminEmail: "not-an-email", Credentials: auth}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad email: %v", err)
	}
	if _, err := svc.Bootstrap(ctx, BootstrapRequest{Tenant: "acme", AdminEmail: "admin@example.com"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no credentials: %v", err)
	}
	res, err := svc.Bootstrap(ctx, BootstrapRequest{Tenant: "acme", TenantName: "Acme", AdminEmail: "Admin@Example.com", Credentials: auth})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tenant.ID != "acme" || !res.Admin.SystemAdmin || res.Admin.Email != "admin@example.com" || res.PairingCode == "" || res.WorkerToken == "" {
		t.Fatalf("result = %+v", res)
	}
	// The results work.
	name, err := auth.AuthenticateWorker(ctx, security.Credentials{BearerToken: res.WorkerToken})
	if err != nil || name != "default" {
		t.Fatalf("worker = %q, %v", name, err)
	}
	cred, err := auth.Pair(ctx, res.PairingCode, "first")
	if err != nil {
		t.Fatal(err)
	}
	p, err := auth.Authenticate(ctx, security.Credentials{BearerToken: cred.Token})
	if err != nil {
		t.Fatal(err)
	}
	if !p.SystemAdmin || p.Email != "admin@example.com" {
		t.Fatalf("principal = %+v", p)
	}
	m, err := stores.System.Users().Membership(ctx, p.UserULID, "acme")
	if err != nil || m.Role != domain.RoleAdmin {
		t.Fatalf("membership = %+v, %v", m, err)
	}
	// I17: it refuses an initialized data directory.
	if _, err := svc.Bootstrap(ctx, BootstrapRequest{Tenant: "other", AdminEmail: "x@example.com", Credentials: auth}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("second bootstrap: %v", err)
	}
}

// I114: a crash part way through leaves a directory the next attempt can finish.
func TestBootstrapAdoptsACrashedAttempt(t *testing.T) {
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer stores.Close()
	auth := authentication.New(stores.System, authentication.Options{})
	keys := &fakeKeys{err: errs.New(errs.CodeSealed, "sealed")}
	svc := New(stores.System, stores.Tenants, Options{Keys: keys})
	req := BootstrapRequest{Tenant: "acme", AdminEmail: "admin@example.com", Credentials: auth}
	if _, err := svc.Bootstrap(ctx, req); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("first attempt: %v", err)
	}
	keys.err = nil
	res, err := svc.Bootstrap(ctx, req)
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if res.Tenant.ID != "acme" || !res.Admin.SystemAdmin {
		t.Fatalf("result = %+v", res)
	}
	// A pre-existing ordinary user is not an attempt to adopt.
	stores2, err := persistence.Open(ctx, persistence.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer stores2.Close()
	if err := stores2.System.Users().Create(ctx, domain.User{ULID: id.New(), Email: "plain@example.com"}); err != nil {
		t.Fatal(err)
	}
	svc2 := New(stores2.System, stores2.Tenants, Options{Keys: &fakeKeys{}})
	_, err = svc2.Bootstrap(ctx, BootstrapRequest{Tenant: "acme", AdminEmail: "plain@example.com", Credentials: authentication.New(stores2.System, authentication.Options{})})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("plain user: %v", err)
	}
}

// ---- Configuration ----

func (f *fixture) writeConfig(t *testing.T, tenant, name, content string) {
	t.Helper()
	dir := filepath.Join(f.cfgDir, "tenants", tenant)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const goodStorages = `{"storages":{
	"archive":{"driver":"local","root":"/data/archive","usage":"backup_only"},
	"master":{"driver":"local","root":"/data/master","usage":"operational"},
	"proxy":{"driver":"local","root":"/data/proxy","usage":"operational"}}}`

func (f *fixture) withTenant(t *testing.T, name domain.TenantID) {
	t.Helper()
	root, err := f.stores.System.Users().GetByEmail(ctx, "root@example.com")
	if err != nil {
		f.admin(t)
		root, _ = f.stores.System.Users().GetByEmail(ctx, "root@example.com")
	}
	if _, err := f.svc.Create(ctx, domain.Principal{UserULID: root.ULID, Email: root.Email, SystemAdmin: true}, api.CreateTenantRequest{ID: name}); err != nil {
		t.Fatal(err)
	}
}

func TestTenantConfigMissingFilesAreEmpty(t *testing.T) {
	f := setup(t)
	f.withTenant(t, "acme")
	cfg, err := f.svc.TenantConfig(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.DefaultProcessing != domain.AILocalOnly || cfg.Storages.Roles != nil {
		t.Fatalf("cfg = %+v", cfg)
	}
	if _, err := f.svc.TenantConfig(ctx, "unknown"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
	if _, err := f.svc.TenantConfig(ctx, "../evil"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("traversal: %v", err)
	}
}

func TestTenantConfigReadsEveryFile(t *testing.T) {
	f := setup(t)
	f.withTenant(t, "acme")
	f.writeConfig(t, "acme", "storages.json", goodStorages)
	f.writeConfig(t, "acme", "publishing.json", `{"publishing":{"destinations":{"mock-main":{"driver":"mock","visibility":"public"}},"defaults":{"requireApproval":true}}}`)
	f.writeConfig(t, "acme", "ai.json", `{"$schema":"x","ai":{"defaultProcessing":"local_only","embeddingModel":"fake-hash-embed@1","llmEndpoints":[{"name":"local","baseUrl":"http://127.0.0.1:9000/v1","model":"m","class":"local"}]}}`)
	f.writeConfig(t, "acme", "catalogs.json", `{"catalogs":{"publicLibrary":{"driver":"local-file","parameters":{"directory":"/data/public"}}}}`)
	f.writeConfig(t, "acme", "observability.json", `{"observability":{"logs":{"level":"info","sinks":[{"id":"local","driver":"file","enabled":true}]}}}`)
	f.writeConfig(t, "acme", "hooks.json", `{"workflow":{"before":[{"id":"h","plugin":"p","action":"a"}]},"maxTimeoutSeconds":60}`)
	f.writeConfig(t, "acme", "budget.json", `{"perUnitMinor":{"ai.external":1.5}}`)
	cfg, err := f.svc.TenantConfig(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storages.Roles[domain.RoleMaster].Root != "/data/master" {
		t.Errorf("storages = %+v", cfg.Storages)
	}
	if cfg.Publishing.Destinations["mock-main"].Driver != "mock" || !cfg.Publishing.Defaults.RequireApproval {
		t.Errorf("publishing = %+v", cfg.Publishing)
	}
	if cfg.AI.EmbeddingModel != "fake-hash-embed@1" || cfg.AI.LLMEndpoints[0].Class != config.EndpointLocal {
		t.Errorf("ai = %+v", cfg.AI)
	}
	if cfg.Catalogs.PublicLibrary == nil || cfg.Catalogs.PublicLibrary.Driver != "local-file" {
		t.Errorf("catalogs = %+v", cfg.Catalogs)
	}
	if len(cfg.Observability.Logs.Sinks) != 1 {
		t.Errorf("observability = %+v", cfg.Observability)
	}
	if len(cfg.Hooks.Workflow.Before) != 1 || cfg.Hooks.MaxTimeoutSeconds != 60 {
		t.Errorf("hooks = %+v", cfg.Hooks)
	}
	if cfg.Budget.PerUnitMinor["ai.external"] != 1.5 {
		t.Errorf("budget = %+v", cfg.Budget)
	}
}

func TestTenantConfigAcceptsBareForms(t *testing.T) {
	f := setup(t)
	f.withTenant(t, "acme")
	f.writeConfig(t, "acme", "ai.json", `{"embeddingModel":"bge-m3@1"}`)
	f.writeConfig(t, "acme", "catalogs.json", `{"publicLibrary":{"driver":"local-file"}}`)
	cfg, err := f.svc.TenantConfig(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI.EmbeddingModel != "bge-m3@1" || cfg.AI.DefaultProcessing != domain.AILocalOnly || cfg.Catalogs.PublicLibrary == nil {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// §9.1: configuration never holds a secret, and a broken file is an error, not a
// silently empty section.
func TestTenantConfigRefusals(t *testing.T) {
	cases := map[string]struct{ file, content string }{
		"inline token":       {"storages.json", `{"storages":{"archive":{"driver":"gcs","bucket":"a","token":"fictional"},"master":{"driver":"gcs","bucket":"b"},"proxy":{"driver":"gcs","bucket":"c"}}}`},
		"secret in params":   {"observability.json", `{"observability":{"logs":{"sinks":[{"id":"x","driver":"loki","enabled":true,"parameters":{"api_key":"fictional"}}]}}}`},
		"secret in hooks":    {"hooks.json", `{"workflow":{},"password":"fictional"}`},
		"secret in budget":   {"budget.json", `{"perUnitMinor":{},"apiKey":"fictional"}`},
		"unknown key":        {"publishing.json", `{"publishing":{"destinations":{},"bogus":1}}`},
		"unknown top level":  {"hooks.json", `{"bogus":1}`},
		"overlapping roles":  {"storages.json", `{"storages":{"archive":{"driver":"local","root":"/data"},"master":{"driver":"local","root":"/data/master"},"proxy":{"driver":"local","root":"/other"}}}`},
		"missing role":       {"storages.json", `{"storages":{"master":{"driver":"local","root":"/a"}}}`},
		"not JSON":           {"ai.json", `{`},
		"trailing data":      {"hooks.json", `{} {}`},
		"bad endpoint class": {"ai.json", `{"ai":{"llmEndpoints":[{"name":"x","baseUrl":"http://x","model":"m","class":"remote"}]}}`},
		"bad credentialsRef": {"publishing.json", `{"publishing":{"destinations":{"x":{"driver":"mock","credentialsRef":"has space"}}}}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.withTenant(t, "acme")
			f.writeConfig(t, "acme", c.file, c.content)
			if _, err := f.svc.TenantConfig(ctx, "acme"); err == nil {
				t.Fatal("expected an error")
			} else if strings.Contains(errs.Public(err).Message, "fictional") {
				t.Fatalf("an error echoes a secret: %v", err)
			}
		})
	}
}

// I115: a bad edit never removes working configuration.
func TestReloadKeepsLastGood(t *testing.T) {
	f := setup(t)
	f.withTenant(t, "acme")
	f.withTenant(t, "beta")
	f.writeConfig(t, "acme", "budget.json", `{"perUnitMinor":{"x":1}}`)
	f.writeConfig(t, "beta", "budget.json", `{"perUnitMinor":{"y":1}}`)
	if err := f.svc.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	f.writeConfig(t, "acme", "budget.json", `{"perUnitMinor":{"x":2}}`)
	f.writeConfig(t, "beta", "budget.json", `{broken`)
	err := f.svc.Reload(ctx)
	if err == nil || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("err = %v", err)
	}
	acme, err := f.svc.TenantConfig(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := f.svc.TenantConfig(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if acme.Budget.PerUnitMinor["x"] != 2 {
		t.Fatalf("a good tenant must update: %+v", acme.Budget)
	}
	if beta.Budget.PerUnitMinor["y"] != 1 {
		t.Fatalf("a bad tenant keeps its last good configuration: %+v", beta.Budget)
	}
}

// ---- Budgets ----

func (f *fixture) budgetScope(t *testing.T) (service.Budgets, service.Scope, repo.TenantStore) {
	t.Helper()
	f.withTenant(t, "acme")
	store, err := f.stores.Tenants.ForTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	return f.svc.Budgets(), service.Scope{Tenant: "acme", Principal: domain.Principal{Email: "admin@example.com", Role: domain.RoleAdmin}, RequestID: "req-1"}, store
}

func budgetEvents(t *testing.T, store repo.TenantStore) []event.Type {
	t.Helper()
	entries, err := store.Outbox().Stream(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Type
	for _, e := range entries {
		out = append(out, e.Event.Type)
	}
	return out
}

func TestNoBudgetBlocksNothing(t *testing.T) {
	f := setup(t)
	svc, scope, _ := f.budgetScope(t)
	st, err := svc.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocked || st.Percent != 0 || st.Period != "2026-11" {
		t.Fatalf("status = %+v", st)
	}
	if err := svc.Check(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Record(ctx, "acme", []domain.UsageRecord{{Operation: "ai.external", EstimatedMinor: 1000}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Check(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
}

// D19: alert at 80%, block new paid operations at 100%, audited override.
func TestBudgetThresholds(t *testing.T) {
	f := setup(t)
	svc, scope, store := f.budgetScope(t)
	st, err := svc.Set(ctx, scope, api.SetBudgetRequest{MonthlyMinor: 1000, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Budget.MonthlyMinor != 1000 || st.Blocked {
		t.Fatalf("status = %+v", st)
	}
	if _, err := svc.Set(ctx, scope, api.SetBudgetRequest{MonthlyMinor: -1, Currency: "USD"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("negative: %v", err)
	}
	if _, err := svc.Set(ctx, scope, api.SetBudgetRequest{MonthlyMinor: 1}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no currency: %v", err)
	}

	record := func(minor int64) {
		t.Helper()
		if err := svc.Record(ctx, "acme", []domain.UsageRecord{{Operation: "ai.external", EstimatedMinor: minor}}); err != nil {
			t.Fatal(err)
		}
	}
	record(799)
	if ev := budgetEvents(t, store); len(ev) != 0 {
		t.Fatalf("below 80%%: %v", ev)
	}
	record(1)
	if ev := budgetEvents(t, store); len(ev) != 1 || ev[0] != event.BudgetThresholdReached {
		t.Fatalf("at 80%%: %v", ev)
	}
	st, _ = svc.Status(ctx, scope)
	if st.SpentMinor != 800 || st.Percent != 80 || st.Blocked {
		t.Fatalf("status = %+v", st)
	}
	if err := svc.Check(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	// Once per crossing.
	record(100)
	if ev := budgetEvents(t, store); len(ev) != 1 {
		t.Fatalf("no second alert: %v", ev)
	}
	record(100)
	ev := budgetEvents(t, store)
	if len(ev) != 2 || ev[1] != event.BudgetExceeded {
		t.Fatalf("at 100%%: %v", ev)
	}
	err = svc.Check(ctx, "acme")
	if !errs.IsCode(err, errs.CodeBudgetExceeded) {
		t.Fatalf("blocked: %v", err)
	}
	if errs.Public(err).Details["monthly_minor"] != int64(1000) {
		t.Fatalf("details = %+v", errs.Public(err).Details)
	}
	record(50)
	if ev := budgetEvents(t, store); len(ev) != 2 {
		t.Fatalf("no second exceeded: %v", ev)
	}

	// Payloads carry numbers only.
	entries, _ := store.Outbox().Stream(ctx, 0, 10)
	var payload map[string]any
	if err := json.Unmarshal(entries[1].Event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["spent_minor"] != float64(1000) || payload["currency"] != "USD" || entries[1].Event.TenantID != "acme" {
		t.Fatalf("payload = %+v", payload)
	}

	// Override: reason required, audited, with an end.
	if _, err := svc.Override(ctx, scope, api.BudgetOverrideRequest{}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no reason: %v", err)
	}
	st, err = svc.Override(ctx, scope, api.BudgetOverrideRequest{Reason: "end-of-month release"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocked || st.OverrideUntil == nil || !st.OverrideUntil.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("status = %+v", st)
	}
	if err := svc.Check(ctx, "acme"); err != nil {
		t.Fatalf("overridden: %v", err)
	}
	records, _, err := store.Audit().List(ctx, repo.AuditFilter{Action: "budget.override"}, repo.Page{})
	if err != nil || len(records) != 1 || records[0].Permission != domain.PermBudgetOverride || records[0].Actor != "admin@example.com" || !strings.Contains(string(records[0].Details), "end-of-month") {
		t.Fatalf("records = %+v, %v", records, err)
	}
	// An override with a TTL ends.
	if _, err := svc.Override(ctx, scope, api.BudgetOverrideRequest{Reason: "short", TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(2 * time.Minute)
	if err := svc.Check(ctx, "acme"); !errs.IsCode(err, errs.CodeBudgetExceeded) {
		t.Fatalf("expired override: %v", err)
	}
	st, _ = svc.Status(ctx, scope)
	if st.OverrideUntil != nil {
		t.Fatalf("status = %+v", st)
	}

	// A new month starts from zero.
	*f.now = time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC)
	if err := svc.Check(ctx, "acme"); err != nil {
		t.Fatalf("new month: %v", err)
	}
	st, _ = svc.Status(ctx, scope)
	if st.Period != "2026-12" || st.SpentMinor != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestSingleRecordCrossingBothThresholds(t *testing.T) {
	f := setup(t)
	svc, scope, store := f.budgetScope(t)
	if _, err := svc.Set(ctx, scope, api.SetBudgetRequest{MonthlyMinor: 100, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Record(ctx, "acme", []domain.UsageRecord{{Operation: "x", EstimatedMinor: 60}, {Operation: "x", EstimatedMinor: 60}}); err != nil {
		t.Fatal(err)
	}
	ev := budgetEvents(t, store)
	if len(ev) != 2 || ev[0] != event.BudgetThresholdReached || ev[1] != event.BudgetExceeded {
		t.Fatalf("events = %v", ev)
	}
}

func TestEstimatesFromTheCostTable(t *testing.T) {
	f := setup(t)
	svc, scope, store := f.budgetScope(t)
	f.writeConfig(t, "acme", "budget.json", `{"perUnitMinor":{"ai.external":2.5}}`)
	if err := f.svc.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	err := svc.Record(ctx, "acme", []domain.UsageRecord{
		{Operation: "ai.external", Quantity: 3},                      // 7.5 rounds to 8
		{Operation: "ai.external", Quantity: 1, EstimatedMinor: 100}, // the node's own estimate wins
		{Operation: "unpriced", Quantity: 1000},                      // no price, no cost
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if st.SpentMinor != 108 {
		t.Fatalf("spent = %d", st.SpentMinor)
	}
	usage, _, err := store.Budgets().ListUsage(ctx, "2026-11", repo.Page{})
	if err != nil || len(usage) != 3 {
		t.Fatalf("usage = %+v, %v", usage, err)
	}
	if err := svc.Record(ctx, "acme", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.Record(ctx, "unknown", []domain.UsageRecord{{Operation: "x"}}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
	if err := svc.Check(ctx, "unknown"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
}

func TestConcurrentRecordsFireOnce(t *testing.T) {
	f := setup(t)
	svc, scope, store := f.budgetScope(t)
	if _, err := svc.Set(ctx, scope, api.SetBudgetRequest{MonthlyMinor: 100, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Record(ctx, "acme", []domain.UsageRecord{{Operation: "x", EstimatedMinor: 10}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ev := budgetEvents(t, store)
	if len(ev) != 2 {
		t.Fatalf("events = %v", ev)
	}
}

// ---- Maintenance ----

type fakeAudit struct {
	exports []domain.TenantID
	pruned  map[domain.TenantID]int
	err     error
}

func (f *fakeAudit) Export(_ context.Context, tenant domain.TenantID) (string, error) {
	f.exports = append(f.exports, tenant)
	return "key", f.err
}

func (f *fakeAudit) Prune(_ context.Context, tenant domain.TenantID, days int) (int64, error) {
	if f.pruned == nil {
		f.pruned = map[domain.TenantID]int{}
	}
	f.pruned[tenant] = days
	return 0, nil
}

func TestMaintenance(t *testing.T) {
	f := setup(t)
	f.withTenant(t, "acme")
	f.withTenant(t, "beta")
	store, _ := f.stores.Tenants.ForTenant(ctx, "acme")
	now := time.Now()
	finish := func(age time.Duration) domain.Job {
		friendly, err := store.IDs().Next(ctx, id.PrefixJob)
		if err != nil {
			t.Fatal(err)
		}
		j := domain.Job{ID: friendly, ULID: id.New(), WorkflowID: "w"}
		if err := store.Jobs().Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		j.Status = domain.JobRunning
		if err := store.Jobs().Update(ctx, j); err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		j.Status = domain.JobCompleted
		j.FinishedAt = &at
		if err := store.Jobs().Update(ctx, j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	old := finish(100 * 24 * time.Hour)
	recent := finish(10 * 24 * time.Hour)
	if err := store.Idempotency().Put(ctx, repo.IdempotentResponse{Key: "old", RequestHash: "h", Status: 200, CreatedAt: now.Add(-8 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Idempotency().Put(ctx, repo.IdempotentResponse{Key: "new", RequestHash: "h", Status: 200, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	seq, err := store.Outbox().Append(ctx, event.Envelope{Type: event.AssetCreated, OccurredAt: now.Add(-100 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Outbox().MarkDispatched(ctx, seq); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Outbox().Append(ctx, event.Envelope{Type: event.AssetCreated, OccurredAt: now.Add(-100 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	fa := &fakeAudit{}
	m := NewMaintenance(f.stores.Tenants, MaintenanceOptions{Audit: fa, Retention: config.Retention{AuditDays: 500}, Clock: func() time.Time { return now }})
	if err := m.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Jobs().Get(ctx, id.Ref(old.ID)); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("old job: %v", err)
	}
	if _, err := store.Jobs().Get(ctx, id.Ref(recent.ID)); err != nil {
		t.Fatalf("recent job: %v", err)
	}
	if _, found, _ := store.Idempotency().Get(ctx, "old"); found {
		t.Fatal("old idempotency record must go")
	}
	if _, found, _ := store.Idempotency().Get(ctx, "new"); !found {
		t.Fatal("recent idempotency record must stay")
	}
	entries, _ := store.Outbox().Stream(ctx, 0, 10)
	if len(entries) != 1 {
		t.Fatalf("an undispatched event must survive: %+v", entries)
	}
	if len(fa.exports) != 2 || fa.pruned["acme"] != 500 {
		t.Fatalf("audit = %+v", fa)
	}

	// A failing tenant does not stop the others, and is reported.
	fa2 := &fakeAudit{err: errors.New("archive down")}
	m2 := NewMaintenance(f.stores.Tenants, MaintenanceOptions{Audit: fa2})
	err = m2.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("err = %v", err)
	}
	if len(fa2.exports) != 2 || fa2.pruned != nil {
		t.Fatalf("no prune after a failed export: %+v", fa2)
	}

	// Run sweeps at start and stops with the context.
	stop, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	reports := make(chan error, 1)
	go func() {
		NewMaintenance(f.stores.Tenants, MaintenanceOptions{}).Run(stop, func(err error) { reports <- err })
		close(done)
	}()
	if err := <-reports; err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
}
