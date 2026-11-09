package authorization

import (
	"context"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/persistence"
)

var ctx = context.Background()

type counter struct {
	telemetry.Noop
	denials int
}

func (c *counter) Add(name string, _ float64, _ telemetry.Labels) {
	if name == telemetry.AuthzDenials {
		c.denials++
	}
}

func setup(t *testing.T) (repo.SystemStore, *Authorizer, *counter) {
	t.Helper()
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stores.Close() })
	for _, tn := range []domain.TenantID{"acme", "beta"} {
		if err := stores.System.Tenants().Create(ctx, domain.Tenant{ID: tn, ULID: id.New()}); err != nil {
			t.Fatal(err)
		}
	}
	c := &counter{}
	return stores.System, New(stores.System, Options{Metrics: c}), c
}

func addUser(t *testing.T, sys repo.SystemStore, email string, admin bool) domain.Principal {
	t.Helper()
	u := domain.User{ULID: id.New(), Email: email, SystemAdmin: admin}
	if err := sys.Users().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	return domain.Principal{UserULID: u.ULID, Email: email, Method: domain.AuthDevice, SystemAdmin: admin}
}

func TestResolve(t *testing.T) {
	sys, a, _ := setup(t)
	p := addUser(t, sys, "maria@example.com", false)
	if err := sys.Users().SetMembership(ctx, domain.Membership{UserULID: p.UserULID, TenantID: "acme", Role: domain.RoleEditor, Grants: []domain.Permission{domain.PermAssetDelete}}); err != nil {
		t.Fatal(err)
	}
	got, err := a.Resolve(ctx, p, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != "acme" || got.Role != domain.RoleEditor || len(got.Grants) != 1 || got.Email != "maria@example.com" {
		t.Fatalf("principal = %+v", got)
	}
	// §12.1: another tenant, an unknown one and a malformed one all look the same.
	for _, tenant := range []domain.TenantID{"beta", "unknown", "../../etc", ""} {
		if _, err := a.Resolve(ctx, p, tenant); !errs.IsCode(err, errs.CodeNotFound) {
			t.Errorf("%q: %v", tenant, err)
		}
	}
	if _, err := a.Resolve(ctx, domain.Principal{Method: domain.AuthPeer}, "acme"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("no user: %v", err)
	}
}

// D6: a system admin manages tenants, and tenant access still needs a membership.
func TestSystemAdminNeedsMembership(t *testing.T) {
	sys, a, _ := setup(t)
	p := addUser(t, sys, "root@example.com", true)
	if _, err := a.Resolve(ctx, p, "acme"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("no membership: %v", err)
	}
	if err := a.Require(ctx, p, domain.PermTenantCreate); err != nil {
		t.Fatal(err)
	}
	if err := a.Require(ctx, p, domain.PermAssetRead); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("tenant permission with no tenant: %v", err)
	}
}

func TestSystemAndWorkerPrincipals(t *testing.T) {
	_, a, _ := setup(t)
	sysP := domain.Principal{Method: domain.AuthSystem, Grants: []domain.Permission{domain.PermStorageArchiveRestore}, Role: domain.RoleAdmin}
	got, err := a.Resolve(ctx, sysP, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != "" || !got.Can(domain.PermStorageArchiveRestore) || got.Can(domain.PermAssetRead) {
		t.Fatalf("a system principal holds only its grants: %+v", got)
	}
	if _, err := a.Resolve(ctx, sysP, "unknown"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
	if _, err := a.Resolve(ctx, domain.Principal{Method: domain.AuthWorker}, "acme"); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("worker: %v", err)
	}
}

// D48 and D7, and §15.6's "an unauthorized client does not install a plugin, restore
// the Archive, delete assets, or publish".
func TestRolesAndGrants(t *testing.T) {
	sys, a, c := setup(t)
	matrix := []struct {
		role  domain.Role
		allow []domain.Permission
		deny  []domain.Permission
	}{
		{domain.RoleViewer,
			[]domain.Permission{domain.PermAssetRead, domain.PermJobRead, domain.PermSearch},
			[]domain.Permission{domain.PermApprovalDecideClipSelection, domain.PermApprovalDecidePublication, domain.PermPluginInstall, domain.PermStorageArchiveRestore, domain.PermAssetDelete, domain.PermPublicationCreate, domain.PermAssetAIConsent}},
		{domain.RoleEditor,
			[]domain.Permission{domain.PermAssetWrite, domain.PermApprovalDecideClipSelection, domain.PermJobResolve, domain.PermAssetSensitive},
			[]domain.Permission{domain.PermApprovalDecidePublication, domain.PermPublicationCreate, domain.PermPluginInstall, domain.PermStorageArchiveRestore, domain.PermAssetDelete, domain.PermJobResolvePublication}},
		{domain.RolePublisher,
			[]domain.Permission{domain.PermApprovalDecidePublication, domain.PermPublicationWithdraw, domain.PermApprovalDecideClipSelection, domain.PermJobResolvePublication},
			[]domain.Permission{domain.PermPluginInstall, domain.PermAssetDelete, domain.PermStorageArchiveRestore, domain.PermSecretWrite}},
		{domain.RoleAdmin,
			[]domain.Permission{domain.PermPluginInstall, domain.PermAssetDelete, domain.PermStorageArchiveRestore, domain.PermSecretWrite, domain.PermBudgetOverride, domain.PermAuditExport, domain.PermAssetAIConsent},
			[]domain.Permission{domain.PermTenantCreate, domain.PermSystemAdmin}},
	}
	for _, row := range matrix {
		p := addUser(t, sys, string(row.role)+"@example.com", false)
		if err := sys.Users().SetMembership(ctx, domain.Membership{UserULID: p.UserULID, TenantID: "acme", Role: row.role}); err != nil {
			t.Fatal(err)
		}
		resolved, err := a.Resolve(ctx, p, "acme")
		if err != nil {
			t.Fatal(err)
		}
		for _, perm := range row.allow {
			if err := a.Require(ctx, resolved, perm); err != nil {
				t.Errorf("%s should hold %s: %v", row.role, perm, err)
			}
		}
		for _, perm := range row.deny {
			err := a.Require(ctx, resolved, perm)
			if !errs.IsCode(err, errs.CodeForbidden) {
				t.Errorf("%s should not hold %s: %v", row.role, perm, err)
			}
		}
	}
	if c.denials == 0 {
		t.Fatal("denials must be counted (§10.3)")
	}

	// D48: an individual grant, without changing the role.
	p := addUser(t, sys, "granted@example.com", false)
	if err := sys.Users().SetMembership(ctx, domain.Membership{UserULID: p.UserULID, TenantID: "acme", Role: domain.RoleViewer, Grants: []domain.Permission{domain.PermStorageArchiveRestore}}); err != nil {
		t.Fatal(err)
	}
	resolved, err := a.Resolve(ctx, p, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Require(ctx, resolved, domain.PermStorageArchiveRestore); err != nil {
		t.Fatal(err)
	}
	if err := a.Require(ctx, resolved, domain.PermAssetDelete); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("grant must not widen: %v", err)
	}
	if err := a.Require(ctx, resolved, ""); err != nil {
		t.Fatalf("a route with no permission: %v", err)
	}
	// The forbidden error names the permission, for the audit.
	err = a.Require(ctx, resolved, domain.PermAssetDelete)
	if pub := errs.Public(err); pub.Details["permission"] != string(domain.PermAssetDelete) {
		t.Fatalf("details = %+v", pub.Details)
	}
}

// §15.6: a user of tenant A resolved in tenant B gets not found, not forbidden.
func TestMembershipIsPerTenant(t *testing.T) {
	sys, a, _ := setup(t)
	p := addUser(t, sys, "both@example.com", false)
	if err := sys.Users().SetMembership(ctx, domain.Membership{UserULID: p.UserULID, TenantID: "acme", Role: domain.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if err := sys.Users().SetMembership(ctx, domain.Membership{UserULID: p.UserULID, TenantID: "beta", Role: domain.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	acme, err := a.Resolve(ctx, p, "acme")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := a.Resolve(ctx, p, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Require(ctx, acme, domain.PermAssetDelete); err != nil {
		t.Fatal(err)
	}
	if err := a.Require(ctx, beta, domain.PermAssetDelete); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("a role is per tenant: %v", err)
	}
}
