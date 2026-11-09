// Package security defines the security contracts: master key unsealing and the
// tenant vault (D13), authentication and authorization (D7, D12, D48), the audit
// trail (D23), the client keyring (§9.1) and derived keys (D22, I29). Component
// core implements them.
package security

import (
	"context"
	"errors"
	"net"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
)

// SealingLevel states honestly how the master key is protected (I10). It is
// shown by `vidyom doctor --security`, /health and the audit.
type SealingLevel string

const (
	SealingHardware SealingLevel = "hardware" // TPM through systemd-creds
	SealingKMS      SealingLevel = "kms"
	SealingOS       SealingLevel = "os"   // Keychain, DPAPI
	SealingFile     SealingLevel = "file" // below D13's target; explicit
	SealingDev      SealingLevel = "dev"  // env; below D13's target; explicit
)

// MasterKeyProvider unseals the master key at boot (D13, D41). Any error means
// the daemon exits with no listener. A provider never falls through to another
// and never creates a key.
type MasterKeyProvider interface {
	Name() string
	Sealing() SealingLevel
	// Unseal returns the 32-byte master key. Callers keep it only in memory.
	Unseal(ctx context.Context) ([]byte, error)
}

// Vault is the per-tenant encrypted vault: envelope encryption with one data key
// per tenant, wrapped by the master key (D13).
type Vault interface {
	Put(ctx context.Context, tenant domain.TenantID, meta domain.SecretMeta, value []byte) error
	// Get returns a value for use inside the daemon or a lease. Never expose it
	// through the API; `secret` routes return metadata only (R41).
	Get(ctx context.Context, tenant domain.TenantID, name string) ([]byte, error)
	List(ctx context.Context, tenant domain.TenantID) ([]domain.SecretMeta, error)
	Delete(ctx context.Context, tenant domain.TenantID, name string) error
	// RotateDataKey creates a new tenant data key and re-encrypts that tenant's
	// secrets (D13). RewrapAll re-wraps all data keys with a new master key.
	RotateDataKey(ctx context.Context, tenant domain.TenantID) error
	RewrapAll(ctx context.Context, newMaster MasterKeyProvider) error
}

// DerivedKeys derives purpose-bound keys from the tenant data key, which itself
// never leaves the daemon.
type DerivedKeys interface {
	// CacheKey is handed to a worker holding a lease on this tenant (I39, D22).
	CacheKey(ctx context.Context, tenant domain.TenantID) ([]byte, error)
	// PublicIDKey derives opaque public_ids for public.db (I29).
	PublicIDKey(ctx context.Context, tenant domain.TenantID) ([]byte, error)
}

// Credentials is what a transport extracted from a request.
type Credentials struct {
	BearerToken string
	// PeerUID is the Unix socket peer identity, nil on other transports (R19).
	PeerUID    *int
	RemoteAddr net.Addr
}

// Authenticator resolves who is calling. The result has no tenant yet.
type Authenticator interface {
	Authenticate(ctx context.Context, c Credentials) (domain.Principal, error)
	AuthenticateWorker(ctx context.Context, c Credentials) (workerName string, err error)
}

// Authorizer resolves a principal in a tenant and decides permissions (D48). The
// audit records which permission authorized each action. A principal with no
// membership in the tenant gets errs.CodeNotFound, so cross-tenant access answers
// 404 (§12.1).
type Authorizer interface {
	Resolve(ctx context.Context, p domain.Principal, tenant domain.TenantID) (domain.Principal, error)
	Require(ctx context.Context, p domain.Principal, perm domain.Permission) error
}

// AuditEntry is what callers supply. The auditor adds time and chain fields.
type AuditEntry struct {
	TenantID   domain.TenantID
	Principal  domain.Principal
	Action     string
	Permission domain.Permission
	Resource   string
	Result     string // "allowed", "denied", "failed"
	RequestID  string
	Details    map[string]any // never content or secrets
}

// Auditor writes to the hash-chained audit. Critical audit events are never
// silently dropped (§10.1): a failure propagates to the caller.
type Auditor interface {
	Record(ctx context.Context, e AuditEntry) error
	Verify(ctx context.Context, tenant domain.TenantID) (domain.AuditVerification, error)
	// Export writes the chain since the last export to archive storage (D23) and
	// returns the storage key.
	Export(ctx context.Context, tenant domain.TenantID) (string, error)
}

// IssuedCredential is returned once, at pairing or rotation (D12).
type IssuedCredential struct {
	Device domain.Device
	Token  string
}

// Pairing issues one-time codes and per-device credentials (D12).
type Pairing interface {
	CreateCode(ctx context.Context, by domain.Principal, userEmail string, ttlSeconds int) (code string, err error)
	Pair(ctx context.Context, code, deviceName string) (IssuedCredential, error)
	Revoke(ctx context.Context, by domain.Principal, device id.Ref) error
	Rotate(ctx context.Context, by domain.Principal, device id.Ref) (IssuedCredential, error)
}

// ErrKeyringUnavailable means the client has no usable OS keyring. The CLI fails
// clearly; it never falls back to a plaintext file (§9.1, §15.6).
var ErrKeyringUnavailable = errors.New("no usable OS keyring on this client: provision one; Vidyom never stores credentials in plaintext")

// ErrKeyringNotFound means the entry does not exist.
var ErrKeyringNotFound = errors.New("keyring entry not found")

// Keyring is the client machine's OS keyring (§9.1).
type Keyring interface {
	Get(service, account string) (string, error)
	Set(service, account, secret string) error
	Delete(service, account string) error
}
