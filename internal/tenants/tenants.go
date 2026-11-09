package tenants

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/security/audit"
)

// KeyProvisioner creates a tenant's data key eagerly, so creating a tenant fails while
// the vault is sealed (I114). *vault.Vault satisfies it.
type KeyProvisioner interface {
	EnsureDataKey(ctx context.Context, tenant domain.TenantID) error
}

// Options configures the service.
type Options struct {
	// ConfigDir is <config-dir>; tenant files are under tenants/<tenant>/ (I115).
	ConfigDir string
	// Keys provisions data keys. Required to create tenants.
	Keys KeyProvisioner
	// Metrics records budget gauges. Nil discards them.
	Metrics telemetry.Metrics
	// Clock is used for timestamps. Nil means time.Now.
	Clock func() time.Time
}

// Service implements service.Tenants and config.TenantSource. Users and Budgets return
// the other two services (I100).
type Service struct {
	system  repo.SystemStore
	stores  repo.TenantStores
	keys    KeyProvisioner
	metrics telemetry.Metrics
	clock   func() time.Time
	dir     string

	mu     sync.RWMutex
	config map[domain.TenantID]config.Tenant
}

var (
	_ service.Tenants     = (*Service)(nil)
	_ config.TenantSource = (*Service)(nil)
)

// New returns the service.
func New(system repo.SystemStore, stores repo.TenantStores, opts Options) *Service {
	if opts.Metrics == nil {
		opts.Metrics = telemetry.Noop{}
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Service{system: system, stores: stores, keys: opts.Keys, metrics: opts.Metrics, clock: opts.Clock, dir: opts.ConfigDir, config: map[domain.TenantID]config.Tenant{}}
}

// systemAudit writes to the system chain inside a transaction (I108).
func (s *Service) systemAudit(ctx context.Context, tx repo.SystemRepos, e security.AuditEntry) error {
	provider, ok := tx.(repo.SystemAuditProvider)
	if !ok {
		return nil
	}
	e.TenantID = ""
	return audit.AppendTx(ctx, provider.SystemAudit(), e, s.clock())
}

// List returns the tenants a caller belongs to, or all of them for a system admin,
// with the caller's role where there is one.
func (s *Service) List(ctx context.Context, p domain.Principal) ([]api.TenantInfo, error) {
	memberships := map[domain.TenantID]domain.Role{}
	if p.UserULID != "" {
		list, err := s.system.Users().Memberships(ctx, p.UserULID)
		if err != nil {
			return nil, err
		}
		for _, m := range list {
			memberships[m.TenantID] = m.Role
		}
	}
	tenants, err := s.system.Tenants().List(ctx)
	if err != nil {
		return nil, err
	}
	out := []api.TenantInfo{}
	for _, t := range tenants {
		role, member := memberships[t.ID]
		if member || p.SystemAdmin {
			out = append(out, api.TenantInfo{Tenant: t, Role: role})
		}
	}
	return out, nil
}

// Create makes a tenant (system admin). The creator becomes its admin, otherwise they
// could not use it (I114).
func (s *Service) Create(ctx context.Context, p domain.Principal, req api.CreateTenantRequest) (domain.Tenant, error) {
	if !p.SystemAdmin {
		return domain.Tenant{}, errs.Forbidden(string(domain.PermTenantCreate))
	}
	if p.UserULID == "" {
		return domain.Tenant{}, errs.New(errs.CodeInvalidArgument, "a tenant needs a creating user")
	}
	return s.create(ctx, p, req)
}

// create runs the crash-safe order of I114: the system row is the commit point, and
// an orphan database file from a crash before it is adopted on the next attempt.
func (s *Service) create(ctx context.Context, p domain.Principal, req api.CreateTenantRequest) (domain.Tenant, error) {
	if err := req.ID.Validate(); err != nil {
		return domain.Tenant{}, errs.Wrap(errs.CodeInvalidArgument, err, err.Error())
	}
	if s.keys == nil {
		return domain.Tenant{}, errs.New(errs.CodeSealed, "no vault: a tenant cannot be created without its data key")
	}
	if _, err := s.system.Tenants().Get(ctx, req.ID); err == nil {
		return domain.Tenant{}, errs.Newf(errs.CodeConflict, "tenant %s already exists", req.ID)
	} else if !errs.IsCode(err, errs.CodeNotFound) {
		return domain.Tenant{}, err
	}
	if _, err := s.stores.Create(ctx, req.ID); err != nil {
		if !errs.IsCode(err, errs.CodeConflict) {
			return domain.Tenant{}, err
		}
		if _, err := s.stores.ForTenant(ctx, req.ID); err != nil {
			return domain.Tenant{}, err
		}
	}
	if err := s.keys.EnsureDataKey(ctx, req.ID); err != nil {
		return domain.Tenant{}, err
	}
	name := req.Name
	if name == "" {
		name = string(req.ID)
	}
	tenant := domain.Tenant{ID: req.ID, ULID: id.New(), Name: name, StorageRoot: config.TenantStoragePrefix(req.ID), CreatedAt: s.clock().UTC()}
	err := s.system.InTx(ctx, func(tx repo.SystemRepos) error {
		if err := tx.Tenants().Create(ctx, tenant); err != nil {
			return err
		}
		if p.UserULID != "" {
			if err := tx.Users().SetMembership(ctx, domain.Membership{UserULID: p.UserULID, TenantID: req.ID, Role: domain.RoleAdmin}); err != nil {
				return err
			}
		}
		return s.systemAudit(ctx, tx, security.AuditEntry{
			Principal:  p,
			Action:     "tenant.create",
			Permission: domain.PermTenantCreate,
			Resource:   "tenant:" + string(req.ID),
		})
	})
	if err != nil {
		return domain.Tenant{}, err
	}
	store, err := s.stores.ForTenant(ctx, req.ID)
	if err != nil {
		return domain.Tenant{}, err
	}
	err = audit.AppendTx(ctx, store.Audit(), security.AuditEntry{
		TenantID:   req.ID,
		Principal:  p,
		Action:     "tenant.create",
		Permission: domain.PermTenantCreate,
		Resource:   "tenant:" + string(req.ID),
	}, s.clock())
	if err != nil {
		return domain.Tenant{}, err
	}
	return tenant, nil
}

// ---- Users ----

// Users returns the service for the header's tenant.
func (s *Service) Users() service.Users { return userService{s} }

type userService struct{ s *Service }

func (u userService) List(ctx context.Context, scope service.Scope) ([]domain.Membership, error) {
	members, err := u.s.system.Users().ListMembers(ctx, scope.Tenant)
	if err != nil {
		return nil, err
	}
	if members == nil {
		members = []domain.Membership{}
	}
	return members, nil
}

// Add adds a person to the tenant, creating the user if they are new. Users are global
// to the daemon, with roles per tenant (D6). Adding someone already in the tenant
// changes their role and keeps their grants.
func (u userService) Add(ctx context.Context, scope service.Scope, req api.AddUserRequest) (domain.Membership, error) {
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		return domain.Membership{}, errs.New(errs.CodeInvalidArgument, "a user needs an email address")
	}
	if !req.Role.Valid() {
		return domain.Membership{}, errs.Newf(errs.CodeInvalidArgument, "unknown role %q: use viewer, editor, publisher or admin", req.Role)
	}
	var out domain.Membership
	err := u.s.system.InTx(ctx, func(tx repo.SystemRepos) error {
		if _, err := tx.Tenants().Get(ctx, scope.Tenant); err != nil {
			return err
		}
		user, err := tx.Users().GetByEmail(ctx, email)
		switch {
		case err == nil:
		case errs.IsCode(err, errs.CodeNotFound):
			user = domain.User{ULID: id.New(), Email: email, CreatedAt: u.s.clock().UTC()}
			if err := tx.Users().Create(ctx, user); err != nil {
				return err
			}
		default:
			return err
		}
		m := domain.Membership{UserULID: user.ULID, TenantID: scope.Tenant, Role: req.Role}
		if existing, err := tx.Users().Membership(ctx, user.ULID, scope.Tenant); err == nil {
			m.Grants = existing.Grants
		} else if !errs.IsCode(err, errs.CodeNotFound) {
			return err
		}
		if err := tx.Users().SetMembership(ctx, m); err != nil {
			return err
		}
		out, err = tx.Users().Membership(ctx, user.ULID, scope.Tenant)
		return err
	})
	if err != nil {
		return domain.Membership{}, err
	}
	if err := u.s.tenantAudit(ctx, scope, "user.add", domain.PermUserManage, "user:"+email, map[string]any{"role": string(req.Role)}); err != nil {
		return domain.Membership{}, err
	}
	return out, nil
}

// Grant grants or revokes one permission without changing the role (D48). userRef is a
// member's email or ULID; anyone else is not found. System-level permissions are not
// grantable in a tenant.
func (u userService) Grant(ctx context.Context, scope service.Scope, userRef string, req api.GrantRequest) (domain.Membership, error) {
	if req.Permission.SystemLevel() {
		return domain.Membership{}, errs.New(errs.CodeForbidden, "system permissions cannot be granted in a tenant")
	}
	if !domain.RoleAdmin.Has(req.Permission) {
		return domain.Membership{}, errs.Newf(errs.CodeInvalidArgument, "unknown permission %q", req.Permission)
	}
	var out domain.Membership
	err := u.s.system.InTx(ctx, func(tx repo.SystemRepos) error {
		var (
			user domain.User
			err  error
		)
		if id.IsULID(userRef) {
			parsed, _ := id.ParseULID(userRef)
			user, err = tx.Users().Get(ctx, parsed)
		} else {
			user, err = tx.Users().GetByEmail(ctx, userRef)
		}
		if err != nil {
			return err
		}
		m, err := tx.Users().Membership(ctx, user.ULID, scope.Tenant)
		if err != nil {
			return err
		}
		grants := make([]domain.Permission, 0, len(m.Grants)+1)
		for _, g := range m.Grants {
			if g != req.Permission {
				grants = append(grants, g)
			}
		}
		if !req.Revoke {
			grants = append(grants, req.Permission)
		}
		m.Grants = grants
		if err := tx.Users().SetMembership(ctx, m); err != nil {
			return err
		}
		out, err = tx.Users().Membership(ctx, user.ULID, scope.Tenant)
		return err
	})
	if err != nil {
		return domain.Membership{}, err
	}
	action := "user.grant"
	if req.Revoke {
		action = "user.revoke_grant"
	}
	if err := u.s.tenantAudit(ctx, scope, action, domain.PermUserManage, "user:"+out.Email, map[string]any{"granted_permission": string(req.Permission)}); err != nil {
		return domain.Membership{}, err
	}
	return out, nil
}

// tenantAudit writes one record to the scope's tenant chain.
func (s *Service) tenantAudit(ctx context.Context, scope service.Scope, action string, perm domain.Permission, resource string, details map[string]any) error {
	store, err := s.stores.ForTenant(ctx, scope.Tenant)
	if err != nil {
		return err
	}
	return audit.AppendTx(ctx, store.Audit(), security.AuditEntry{
		TenantID:   scope.Tenant,
		Principal:  scope.Principal,
		Action:     action,
		Permission: perm,
		Resource:   resource,
		RequestID:  scope.RequestID,
		Details:    details,
	}, s.clock())
}

// ---- Bootstrap ----

// BootstrapRequest is `vidyomd init --tenant acme --admin admin@example.com` (I17).
type BootstrapRequest struct {
	Tenant     domain.TenantID
	TenantName string
	AdminEmail string
	// Credentials issue the first pairing code and the worker token.
	// *authentication.Service satisfies it.
	Credentials BootstrapCredentials
	// WorkerTokenName names the token. Empty means "default".
	WorkerTokenName string
}

// BootstrapCredentials is what bootstrap needs from authentication.
type BootstrapCredentials interface {
	CreateCode(ctx context.Context, by domain.Principal, userEmail string, ttlSeconds int) (string, error)
	IssueWorkerToken(ctx context.Context, name string) (string, error)
}

// BootstrapResult is shown once. The pairing code and the worker token exist nowhere
// else: only their hashes are stored.
type BootstrapResult struct {
	Tenant      domain.Tenant
	Admin       domain.User
	PairingCode string
	WorkerToken string
}

// Bootstrap initializes an empty daemon: the first tenant, a user who is both system
// admin and tenant admin, one pairing code and a worker token. It refuses a system
// database that already has a tenant (I17).
func (s *Service) Bootstrap(ctx context.Context, req BootstrapRequest) (BootstrapResult, error) {
	email := strings.ToLower(strings.TrimSpace(req.AdminEmail))
	if email == "" || !strings.Contains(email, "@") {
		return BootstrapResult{}, errs.New(errs.CodeInvalidArgument, "bootstrap needs an admin email address")
	}
	if req.Credentials == nil {
		return BootstrapResult{}, errs.New(errs.CodeInvalidArgument, "bootstrap needs a credential issuer")
	}
	existing, err := s.system.Tenants().List(ctx)
	if err != nil {
		return BootstrapResult{}, err
	}
	if len(existing) > 0 {
		return BootstrapResult{}, errs.New(errs.CodeFailedPrecondition, "this data directory is already initialized")
	}
	// No tenant yet, so a user here can only come from an attempt that crashed part
	// way: adopt it rather than leave the directory stuck (I114).
	admin, err := s.system.Users().GetByEmail(ctx, email)
	switch {
	case err == nil:
		if !admin.SystemAdmin {
			return BootstrapResult{}, errs.New(errs.CodeFailedPrecondition, "this data directory is already initialized")
		}
	case errs.IsCode(err, errs.CodeNotFound):
		admin = domain.User{ULID: id.New(), Email: email, SystemAdmin: true, CreatedAt: s.clock().UTC()}
		if err := s.system.Users().Create(ctx, admin); err != nil {
			return BootstrapResult{}, err
		}
	default:
		return BootstrapResult{}, err
	}
	principal := domain.Principal{UserULID: admin.ULID, Email: email, Method: domain.AuthSystem, SystemAdmin: true}
	tenant, err := s.create(ctx, principal, api.CreateTenantRequest{ID: req.Tenant, Name: req.TenantName})
	if err != nil {
		return BootstrapResult{}, err
	}
	code, err := req.Credentials.CreateCode(ctx, principal, email, 0)
	if err != nil {
		return BootstrapResult{}, err
	}
	name := req.WorkerTokenName
	if name == "" {
		name = "default"
	}
	token, err := req.Credentials.IssueWorkerToken(ctx, name)
	if err != nil {
		return BootstrapResult{}, err
	}
	return BootstrapResult{Tenant: tenant, Admin: admin, PairingCode: code, WorkerToken: token}, nil
}
