package tenants

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/catalog"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/persistence"
	"github.com/rafaelp/vidyom/internal/security/audit"
	"github.com/rafaelp/vidyom/internal/security/authentication"
	"github.com/rafaelp/vidyom/internal/security/authorization"
	"github.com/rafaelp/vidyom/internal/security/vault"
)

type fixedKey struct{}

func (fixedKey) Name() string                   { return "fixed" }
func (fixedKey) Sealing() security.SealingLevel { return security.SealingDev }
func (fixedKey) Unseal(context.Context) ([]byte, error) {
	return bytes.Repeat([]byte{7}, vault.MasterKeySize), nil
}

// stack is the whole core component over two real tenants, wired as vidyomd wires it.
type stack struct {
	stores  *persistence.Stores
	vault   *vault.Vault
	auditor *audit.Auditor
	auth    *authentication.Service
	authz   *authorization.Authorizer
	tenants *Service
	catalog *catalog.Service
}

func newStack(t *testing.T) *stack {
	t.Helper()
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stores.Close() })
	auditor := audit.New(stores.Tenants, audit.Options{System: stores.System})
	v, err := vault.New(stores.System, stores.Tenants, fixedKey{}, vault.Options{Auditor: auditor})
	if err != nil {
		t.Fatal(err)
	}
	return &stack{
		stores:  stores,
		vault:   v,
		auditor: auditor,
		auth:    authentication.New(stores.System, authentication.Options{Auditor: auditor}),
		authz:   authorization.New(stores.System, authorization.Options{}),
		tenants: New(stores.System, stores.Tenants, Options{Keys: v, ConfigDir: t.TempDir()}),
		catalog: catalog.New(stores.Tenants, catalog.Options{}),
	}
}

// login pairs a user and returns the principal resolved in a tenant, going through the
// real credential flow.
func (s *stack) login(t *testing.T, root domain.Principal, tenant domain.TenantID, email string, role domain.Role) domain.Principal {
	t.Helper()
	if _, err := s.tenants.Users().Add(ctx, service.Scope{Tenant: tenant, Principal: root}, api.AddUserRequest{Email: email, Role: role}); err != nil {
		t.Fatal(err)
	}
	code, err := s.auth.CreateCode(ctx, root, email, 0)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := s.auth.Pair(ctx, code, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.auth.Authenticate(ctx, security.Credentials{BearerToken: cred.Token})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.authz.Resolve(ctx, p, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func wantNotFound(t *testing.T, what string, err error) {
	t.Helper()
	if !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("%s: got %v, want not_found", what, err)
	}
}

// §15.6: a user of tenant A cannot read or change tenant B's assets, jobs, secrets,
// storage prefix, cache, budget or public export; queries with another tenant's IDs
// are rejected.
func TestTenantIsolation(t *testing.T) {
	s := newStack(t)
	boot, err := s.tenants.Bootstrap(ctx, BootstrapRequest{Tenant: "acme", AdminEmail: "root@example.com", Credentials: s.auth})
	if err != nil {
		t.Fatal(err)
	}
	root := domain.Principal{UserULID: boot.Admin.ULID, Email: boot.Admin.Email, SystemAdmin: true, Method: domain.AuthDevice}
	betaTenant, err := s.tenants.Create(ctx, root, api.CreateTenantRequest{ID: "beta"})
	if err != nil {
		t.Fatal(err)
	}

	alice := s.login(t, root, "acme", "alice@example.com", domain.RoleAdmin)
	bob := s.login(t, root, "beta", "bob@example.com", domain.RoleAdmin)
	aliceScope := service.Scope{Tenant: "acme", Principal: alice, RequestID: "a"}
	bobScope := service.Scope{Tenant: "beta", Principal: bob, RequestID: "b"}

	acme, err := s.stores.Tenants.ForTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := s.stores.Tenants.ForTenant(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}

	// Membership: across tenants the answer is 404, not 403 (§12.1).
	wantNotFound(t, "alice in beta", errOf(s.authz.Resolve(ctx, alice, "beta")))
	wantNotFound(t, "bob in acme", errOf(s.authz.Resolve(ctx, bob, "acme")))
	// Even an admin's token resolves to nothing in a tenant they do not belong to.
	list, err := s.tenants.List(ctx, alice)
	if err != nil || len(list) != 1 || list[0].ID != "acme" {
		t.Fatalf("alice's tenants = %+v, %v", list, err)
	}

	// Each tenant has its own storage prefix (D6).
	acmeTenant, err := s.stores.System.Tenants().Get(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if acmeTenant.StorageRoot == betaTenant.StorageRoot || acmeTenant.StorageRoot != "acme/" || betaTenant.StorageRoot != "beta/" {
		t.Fatalf("prefixes = %q, %q", acmeTenant.StorageRoot, betaTenant.StorageRoot)
	}

	// The same friendly ID names unrelated records in each tenant (D46).
	create := func(store repo.TenantStore, title string) domain.Asset {
		t.Helper()
		friendly, err := store.IDs().Next(ctx, id.PrefixOriginal)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Assets().Create(ctx, domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindOriginal, Title: title}); err != nil {
			t.Fatal(err)
		}
		if err := store.Versions().Create(ctx, domain.AssetVersion{AssetID: friendly, Version: "v1"}); err != nil {
			t.Fatal(err)
		}
		a, err := store.Assets().Get(ctx, id.Ref(friendly))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	acmeAsset := create(acme, "Acme's class")
	betaAsset := create(beta, "Beta's class")
	if acmeAsset.ID != "ORI-001" || betaAsset.ID != "ORI-001" {
		t.Fatalf("IDs = %s, %s", acmeAsset.ID, betaAsset.ID)
	}
	got, err := s.catalog.Get(ctx, bobScope, "ORI-001")
	if err != nil || got.Title != "Beta's class" {
		t.Fatalf("bob's ORI-001 = %+v, %v", got, err)
	}

	// Assets through the service: a ULID of the other tenant does not resolve, for reads
	// or for changes.
	other := id.Ref(acmeAsset.ULID)
	title := "Changed by bob"
	wantNotFound(t, "get", errOf(s.catalog.Get(ctx, bobScope, other)))
	wantNotFound(t, "update", errOf(s.catalog.Update(ctx, bobScope, other, api.UpdateAssetRequest{Title: &title})))
	wantNotFound(t, "lineage", errOf(s.catalog.Lineage(ctx, bobScope, other)))
	wantNotFound(t, "versions", errOf(s.catalog.Versions(ctx, bobScope, other)))
	wantNotFound(t, "set-ai", errOf(s.catalog.SetAIProcessing(ctx, bobScope, other, api.SetAIProcessingRequest{AIProcessing: domain.AIExternalAllowed})))
	wantNotFound(t, "consent", errOf(s.catalog.Consent(ctx, bobScope, other)))
	wantNotFound(t, "transcript", errOf(s.catalog.Transcript(ctx, bobScope, other, "")))
	wantNotFound(t, "sensitive", s.catalog.AddSensitive(ctx, bobScope, other, api.SensitiveSegmentRequest{StartSeconds: 1, EndSeconds: 2}))
	wantNotFound(t, "source filter", errOf(s.catalog.List(ctx, bobScope, api.AssetQuery{SourceID: string(acmeAsset.ULID)})))

	// Jobs.
	job := domain.Job{ID: "JOB-001", ULID: id.New(), WorkflowID: "w", AssetID: acmeAsset.ID}
	if err := acme.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, "job", errOf(beta.Jobs().Get(ctx, id.Ref(job.ULID))))
	wantNotFound(t, "job by ULID", errOf(beta.Jobs().GetByULID(ctx, job.ULID)))
	jobs, _, err := beta.Jobs().List(ctx, repo.JobFilter{}, repo.Page{})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("beta's jobs = %+v, %v", jobs, err)
	}

	// Secrets: the same name is two different values, and neither tenant reaches the other.
	secrets := s.vault.Secrets()
	if _, err := secrets.Put(ctx, aliceScope, api.PutSecretRequest{Name: "gcs-main", Location: domain.SecretServer, Value: "fictional-acme"}); err != nil {
		t.Fatal(err)
	}
	if got, err := secrets.List(ctx, bobScope); err != nil || len(got) != 0 {
		t.Fatalf("beta's secrets = %+v, %v", got, err)
	}
	wantNotFound(t, "secret", errOf(s.vault.Get(ctx, "beta", "gcs-main")))
	wantNotFound(t, "secret resolver", errOf(s.vault.Resolver("beta").ResolveSecret(ctx, "gcs-main")))
	wantNotFound(t, "secret delete", secrets.Delete(ctx, bobScope, "gcs-main"))
	if _, err := secrets.Put(ctx, bobScope, api.PutSecretRequest{Name: "gcs-main", Location: domain.SecretServer, Value: "fictional-beta"}); err != nil {
		t.Fatal(err)
	}
	if v, err := s.vault.Get(ctx, "acme", "gcs-main"); err != nil || string(v) != "fictional-acme" {
		t.Fatalf("acme's secret = %q, %v", v, err)
	}
	if v, err := s.vault.Get(ctx, "beta", "gcs-main"); err != nil || string(v) != "fictional-beta" {
		t.Fatalf("beta's secret = %q, %v", v, err)
	}
	// One tenant's ciphertext does not open in the other, even with database access.
	stolen, err := acme.Secrets().Get(ctx, "gcs-main")
	if err != nil {
		t.Fatal(err)
	}
	if err := beta.Secrets().Put(ctx, repo.SecretRecord{Meta: domain.SecretMeta{Name: "stolen", Location: domain.SecretServer}, Ciphertext: stolen.Ciphertext, KeyVersion: stolen.KeyVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.vault.Get(ctx, "beta", "stolen"); err == nil {
		t.Fatal("a ciphertext opened in another tenant")
	}

	// Cache: a worker holding tenant A's key cannot open tenant B's chunks (D22, I39).
	// This holds in the code path and the keys, not as OS isolation: see I39.
	acmeCache, err := s.vault.CacheKey(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	betaCache, err := s.vault.CacheKey(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(acmeCache, betaCache) {
		t.Fatal("cache keys must differ per tenant")
	}
	chunk := seal(t, betaCache, []byte("beta's cached media"))
	if _, err := open(acmeCache, chunk); err == nil {
		t.Fatal("tenant A's cache key opened tenant B's chunk")
	}
	if _, err := open(betaCache, chunk); err != nil {
		t.Fatal(err)
	}

	// Budget: one tenant at its limit does not block the other, and neither sees it.
	budgets := s.tenants.Budgets()
	if _, err := budgets.Set(ctx, aliceScope, api.SetBudgetRequest{MonthlyMinor: 10, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	if err := budgets.Record(ctx, "acme", []domain.UsageRecord{{Operation: "ai.external", EstimatedMinor: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := budgets.Check(ctx, "acme"); !errs.IsCode(err, errs.CodeBudgetExceeded) {
		t.Fatalf("acme: %v", err)
	}
	if err := budgets.Check(ctx, "beta"); err != nil {
		t.Fatalf("beta: %v", err)
	}
	st, err := budgets.Status(ctx, bobScope)
	if err != nil || st.SpentMinor != 0 || st.Budget.MonthlyMinor != 0 {
		t.Fatalf("beta's budget = %+v, %v", st, err)
	}

	// Public export: separate sources, and opaque IDs keyed per tenant (D18, I29).
	acmePublic, err := s.vault.PublicIDKey(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	betaPublic, err := s.vault.PublicIDKey(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(acmePublic, betaPublic) {
		t.Fatal("public ID keys must differ per tenant")
	}
	now := acmeAsset.CreatedAt
	pub := domain.Publication{ID: "PUB-001", ULID: id.New(), AssetID: acmeAsset.ID, Destination: "mock", AssetVersion: "v1", Status: domain.PubPublished, Visibility: domain.VisibilityPublic, RemoteID: "r", URL: "https://media.example.internal/watch/r", PublishedAt: &now}
	if err := acme.Publications().Create(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if public, err := beta.Publications().ListPublic(ctx); err != nil || len(public) != 0 {
		t.Fatalf("beta's public source = %+v, %v", public, err)
	}

	// Audit: each tenant's chain holds only its own events, and each verifies.
	records, err := s.auditor.Service().List(ctx, bobScope, api.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records.Items {
		if r.TenantID != "beta" {
			t.Fatalf("a record of %s in beta's audit", r.TenantID)
		}
		blob, _ := json.Marshal(r)
		if strings.Contains(string(blob), "alice@") {
			t.Fatalf("beta's audit mentions acme's user: %s", blob)
		}
	}
	for _, chain := range []domain.TenantID{"", "acme", "beta"} {
		v, err := s.auditor.Verify(ctx, chain)
		if err != nil || !v.OK {
			t.Fatalf("chain %q = %+v, %v", chain, v, err)
		}
	}

	// Configuration: a tenant's files are read only for that tenant, and the header
	// cannot traverse paths.
	wantNotFound(t, "traversal", errOf(s.tenants.TenantConfig(ctx, "../acme")))
	wantNotFound(t, "traversal in stores", errOf(s.stores.Tenants.ForTenant(ctx, "../acme")))
	wantNotFound(t, "unknown tenant", errOf(s.stores.Tenants.ForTenant(ctx, "gamma")))

	// Users: a tenant admin cannot reach another tenant's member (D6).
	wantNotFound(t, "grant across tenants", errOf(s.tenants.Users().Grant(ctx, bobScope, "alice@example.com", api.GrantRequest{Permission: domain.PermAssetRead})))
	members, err := s.tenants.Users().List(ctx, bobScope)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.Email == "alice@example.com" {
			t.Fatalf("beta's members list acme's user: %+v", members)
		}
	}
	// A pairing code cannot be minted for another tenant's user.
	wantNotFound(t, "code across tenants", errOf(s.auth.CreateCode(ctx, bob, "alice@example.com", 0)))

	// Nothing bob tried changed acme's asset.
	final, err := s.catalog.Get(ctx, aliceScope, id.Ref(acmeAsset.ID))
	if err != nil || final.Title != "Acme's class" || final.AIProcessing != domain.AILocalOnly {
		t.Fatalf("acme's asset = %+v, %v", final, err)
	}
}

// §15.6: an unauthorized client does not install a plugin, restore the Archive, delete
// assets, or publish; approval levels hold. Decisions go through the real authorizer.
func TestUnauthorizedClientsCannotActThroughTheStack(t *testing.T) {
	s := newStack(t)
	boot, err := s.tenants.Bootstrap(ctx, BootstrapRequest{Tenant: "acme", AdminEmail: "root@example.com", Credentials: s.auth})
	if err != nil {
		t.Fatal(err)
	}
	root := domain.Principal{UserULID: boot.Admin.ULID, Email: boot.Admin.Email, SystemAdmin: true, Method: domain.AuthDevice}
	viewer := s.login(t, root, "acme", "viewer@example.com", domain.RoleViewer)
	editor := s.login(t, root, "acme", "editor@example.com", domain.RoleEditor)
	publisher := s.login(t, root, "acme", "publisher@example.com", domain.RolePublisher)

	for _, perm := range []domain.Permission{domain.PermPluginInstall, domain.PermStorageArchiveRestore, domain.PermAssetDelete, domain.PermPublicationCreate} {
		if err := s.authz.Require(ctx, viewer, perm); !errs.IsCode(err, errs.CodeForbidden) {
			t.Errorf("viewer %s: %v", perm, err)
		}
	}
	for _, perm := range []domain.Permission{domain.PermPluginInstall, domain.PermStorageArchiveRestore, domain.PermAssetDelete, domain.PermPublicationCreate} {
		if err := s.authz.Require(ctx, editor, perm); !errs.IsCode(err, errs.CodeForbidden) {
			t.Errorf("editor %s: %v", perm, err)
		}
	}
	// D7: an editor approves selection, not publication; a viewer approves nothing; a
	// workflow's approvers can tighten, never loosen.
	cases := []struct {
		p         domain.Principal
		kind      domain.ApprovalKind
		approvers []string
		want      bool
	}{
		{viewer, domain.ApprovalClipSelection, nil, false},
		{viewer, domain.ApprovalPublication, nil, false},
		{editor, domain.ApprovalClipSelection, nil, true},
		{editor, domain.ApprovalPublication, nil, false},
		{publisher, domain.ApprovalPublication, nil, true},
		{publisher, domain.ApprovalClipSelection, nil, true},
		{editor, domain.ApprovalClipSelection, []string{"publisher"}, false},
		{editor, domain.ApprovalPublication, []string{"editor"}, false},
		{publisher, domain.ApprovalPublication, []string{"publisher@example.com"}, true},
		{publisher, domain.ApprovalPublication, []string{"other@example.com"}, false},
	}
	for _, c := range cases {
		if got := domain.MayDecide(c.p, c.kind, c.approvers); got != c.want {
			t.Errorf("%s deciding %s with %v = %v", c.p.Role, c.kind, c.approvers, got)
		}
	}
	// A revoked device stops at the door.
	code, err := s.auth.CreateCode(ctx, root, "viewer@example.com", 0)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := s.auth.Pair(ctx, code, "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.auth.Revoke(ctx, root, id.Ref(cred.Device.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.auth.Authenticate(ctx, security.Credentials{BearerToken: cred.Token}); !errs.IsCode(err, errs.CodeUnauthenticated) {
		t.Fatalf("revoked: %v", err)
	}
}

// §15.6, D13: with a missing or sealed master key nothing is served.
func TestSealedVaultFailsClosed(t *testing.T) {
	s := newStack(t)
	if _, err := s.tenants.Bootstrap(ctx, BootstrapRequest{Tenant: "acme", AdminEmail: "root@example.com", Credentials: s.auth}); err != nil {
		t.Fatal(err)
	}
	if err := s.vault.Put(ctx, "acme", domain.SecretMeta{Name: "x", Location: domain.SecretServer}, []byte("fictional")); err != nil {
		t.Fatal(err)
	}
	s.vault.Close()
	if _, err := s.vault.Get(ctx, "acme", "x"); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("get: %v", err)
	}
	if _, err := s.vault.CacheKey(ctx, "acme"); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("cache key: %v", err)
	}
	root, err := s.stores.System.Users().GetByEmail(ctx, "root@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.tenants.Create(ctx, domain.Principal{UserULID: root.ULID, Email: root.Email, SystemAdmin: true}, api.CreateTenantRequest{ID: "beta"}); !errs.IsCode(err, errs.CodeSealed) {
		t.Fatalf("create tenant: %v", err)
	}
}

func errOf[T any](_ T, err error) error { return err }

func seal(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	return gcm.Seal(nonce, nonce, plain, nil)
}

func open(key, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}
