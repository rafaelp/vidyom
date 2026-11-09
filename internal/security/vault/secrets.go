package vault

import (
	"context"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

// Delegated secrets have a limited duration (§9.1, I110).
const (
	DefaultDelegatedTTL = time.Hour
	MaxDelegatedTTL     = 24 * time.Hour
)

// Secrets returns the service the API calls. It only ever returns metadata (R41).
func (v *Vault) Secrets() service.Secrets { return secretService{v} }

type secretService struct{ v *Vault }

func (s secretService) audit(ctx context.Context, scope service.Scope, action string, perm domain.Permission, name string, details map[string]any) error {
	if s.v.auditor == nil {
		return nil
	}
	return s.v.auditor.Record(ctx, security.AuditEntry{
		TenantID:   scope.Tenant,
		Principal:  scope.Principal,
		Action:     action,
		Permission: perm,
		Resource:   "secret:" + name,
		Result:     "allowed",
		RequestID:  scope.RequestID,
		Details:    details,
	})
}

func (s secretService) List(ctx context.Context, scope service.Scope) ([]domain.SecretMeta, error) {
	return s.v.List(ctx, scope.Tenant)
}

func (s secretService) Put(ctx context.Context, scope service.Scope, req api.PutSecretRequest) (domain.SecretMeta, error) {
	meta := domain.SecretMeta{Name: req.Name, Location: req.Location, CreatedBy: scope.Principal.Actor()}
	switch {
	case req.Location == domain.SecretDelegated:
		ttl := DefaultDelegatedTTL
		if req.TTLSeconds > 0 {
			ttl = time.Duration(req.TTLSeconds) * time.Second
		}
		if ttl > MaxDelegatedTTL {
			return domain.SecretMeta{}, errs.Newf(errs.CodeInvalidArgument, "a delegated secret lasts at most %s", MaxDelegatedTTL)
		}
		expires := s.v.clock().UTC().Add(ttl)
		meta.ExpiresAt = &expires
	case req.TTLSeconds > 0:
		expires := s.v.clock().UTC().Add(time.Duration(req.TTLSeconds) * time.Second)
		meta.ExpiresAt = &expires
	}
	if err := validateMeta(meta); err != nil {
		return domain.SecretMeta{}, err
	}
	if err := s.v.store(ctx, scope.Tenant, meta, []byte(req.Value), putCreate); err != nil {
		return domain.SecretMeta{}, err
	}
	if err := s.audit(ctx, scope, "secret.add", domain.PermSecretWrite, req.Name, map[string]any{"location": string(req.Location)}); err != nil {
		return domain.SecretMeta{}, err
	}
	return s.meta(ctx, scope, req.Name)
}

func (s secretService) meta(ctx context.Context, scope service.Scope, name string) (domain.SecretMeta, error) {
	store, err := s.v.tenants.ForTenant(ctx, scope.Tenant)
	if err != nil {
		return domain.SecretMeta{}, err
	}
	rec, err := store.Secrets().Get(ctx, name)
	if err != nil {
		return domain.SecretMeta{}, err
	}
	return rec.Meta, nil
}

func (s secretService) Delete(ctx context.Context, scope service.Scope, name string) error {
	if err := s.v.Delete(ctx, scope.Tenant, name); err != nil {
		return err
	}
	return s.audit(ctx, scope, "secret.delete", domain.PermSecretWrite, name, nil)
}

// Test checks that a value opens, without counting as a use or returning anything of
// the value (§9.1, §11.5).
func (s secretService) Test(ctx context.Context, scope service.Scope, name string) (api.SecretTestResponse, error) {
	store, err := s.v.tenants.ForTenant(ctx, scope.Tenant)
	if err != nil {
		return api.SecretTestResponse{}, err
	}
	value, _, err := s.v.open(ctx, store, scope.Tenant, name)
	resp := api.SecretTestResponse{OK: err == nil}
	switch {
	case err == nil:
		wipe(value)
		resp.Detail = "the value opens"
	case errs.IsCode(err, errs.CodeNotFound):
		return api.SecretTestResponse{}, err
	default:
		resp.Detail = errs.Public(err).Message
	}
	if auditErr := s.audit(ctx, scope, "secret.test", domain.PermSecretRead, name, map[string]any{"ok": resp.OK}); auditErr != nil {
		return api.SecretTestResponse{}, auditErr
	}
	return resp, nil
}

// Rotate replaces a value under the active data key. The name, location and expiry
// stay (§9.1). A secret deleted meanwhile stays deleted.
func (s secretService) Rotate(ctx context.Context, scope service.Scope, name string, req api.RotateSecretRequest) (domain.SecretMeta, error) {
	if err := validateSecretName(name); err != nil {
		return domain.SecretMeta{}, err
	}
	if err := s.v.store(ctx, scope.Tenant, domain.SecretMeta{Name: name}, []byte(req.Value), putRotate); err != nil {
		return domain.SecretMeta{}, err
	}
	if err := s.audit(ctx, scope, "secret.rotate", domain.PermSecretWrite, name, nil); err != nil {
		return domain.SecretMeta{}, err
	}
	return s.meta(ctx, scope, name)
}
