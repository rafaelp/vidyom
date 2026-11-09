package authorization

import (
	"context"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// Options configures the authorizer.
type Options struct {
	// Metrics counts denials. Nil discards them.
	Metrics telemetry.Metrics
}

// Authorizer implements security.Authorizer over system.db.
type Authorizer struct {
	system  repo.SystemStore
	metrics telemetry.Metrics
}

var _ security.Authorizer = (*Authorizer)(nil)

// New returns an authorizer.
func New(system repo.SystemStore, opts Options) *Authorizer {
	if opts.Metrics == nil {
		opts.Metrics = telemetry.Noop{}
	}
	return &Authorizer{system: system, metrics: opts.Metrics}
}

// Resolve places a principal in a tenant. An unknown tenant, or one the principal
// does not belong to, is not found. A system admin needs a membership too:
// Principal.Can keeps system permissions and tenant access apart (D6).
func (a *Authorizer) Resolve(ctx context.Context, p domain.Principal, tenant domain.TenantID) (domain.Principal, error) {
	if err := tenant.Validate(); err != nil {
		return domain.Principal{}, errs.NotFound("tenant")
	}
	switch p.Method {
	case domain.AuthWorker:
		return domain.Principal{}, errs.Forbidden("a worker token cannot act as a user")
	case domain.AuthSystem:
		// Event and schedule triggers: only the explicit grants they carry.
		if _, err := a.system.Tenants().Get(ctx, tenant); err != nil {
			return domain.Principal{}, err
		}
		p.TenantID = tenant
		p.Role = ""
		return p, nil
	}
	if p.UserULID == "" {
		return domain.Principal{}, errs.NotFound("tenant")
	}
	m, err := a.system.Users().Membership(ctx, p.UserULID, tenant)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return domain.Principal{}, errs.NotFound("tenant")
		}
		return domain.Principal{}, err
	}
	p.TenantID = tenant
	p.Role = m.Role
	p.Grants = m.Grants
	return p, nil
}

// Require decides one permission. The error names it, so the caller can audit it.
func (a *Authorizer) Require(ctx context.Context, p domain.Principal, perm domain.Permission) error {
	if perm == "" || p.Can(perm) {
		return nil
	}
	a.metrics.Add(telemetry.AuthzDenials, 1, telemetry.Labels{"tenant_id": string(p.TenantID), "permission": string(perm)})
	return errs.Forbidden(string(perm))
}
