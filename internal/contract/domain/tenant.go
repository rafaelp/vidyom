package domain

import (
	"fmt"
	"regexp"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/id"
)

// TenantID is a tenant slug (I16). It is the Vidyom-Tenant header value and the
// directory name of the tenant's data.
type TenantID string

var tenantPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// Validate rejects anything that is not a safe slug, which also excludes path
// traversal through the header.
func (t TenantID) Validate() error {
	if !tenantPattern.MatchString(string(t)) {
		return fmt.Errorf("invalid tenant %q: expected a slug matching %s", string(t), tenantPattern)
	}
	return nil
}

func (t TenantID) String() string { return string(t) }

// Tenant is an organization (D6).
type Tenant struct {
	ID          TenantID  `json:"id"`
	ULID        id.ULID   `json:"ulid"`
	Name        string    `json:"name"`
	StorageRoot string    `json:"storage_prefix"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
}

// User is a person, global to the daemon, with roles per tenant.
type User struct {
	ULID        id.ULID   `json:"ulid"`
	Email       string    `json:"email"`
	SystemAdmin bool      `json:"system_admin"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
}

// Membership is a user's role in a tenant, plus individual grants (D48).
type Membership struct {
	UserULID id.ULID      `json:"user_ulid"`
	Email    string       `json:"email,omitempty"`
	TenantID TenantID     `json:"tenant_id"`
	Role     Role         `json:"role"`
	Grants   []Permission `json:"grants,omitempty"`
}

// DeviceStatus is the state of a paired client.
type DeviceStatus string

const (
	DeviceActive  DeviceStatus = "active"
	DeviceRevoked DeviceStatus = "revoked"
)

// Device is a paired client, CLIENT-nnn (D12). CLIENT numbering is system-wide
// because devices live in system.db (I14). The credential is never returned
// after issuance and only its hash is stored.
type Device struct {
	ID         id.Friendly  `json:"id"`
	ULID       id.ULID      `json:"ulid"`
	UserULID   id.ULID      `json:"user_ulid"`
	Email      string       `json:"email,omitempty"`
	Name       string       `json:"name,omitempty"`
	Status     DeviceStatus `json:"status"`
	LastSeenAt *time.Time   `json:"last_seen_at,omitempty"`
	RotatedAt  *time.Time   `json:"rotated_at,omitempty"`
	RevokedAt  *time.Time   `json:"revoked_at,omitempty"`
	CreatedAt  time.Time    `json:"created_at,omitzero"`
	// CredentialHash is the stored hash of the credential; never serialized.
	CredentialHash string `json:"-"`
}

// AuthMethod says how a request authenticated.
type AuthMethod string

const (
	AuthDevice AuthMethod = "device"
	AuthPeer   AuthMethod = "peer" // Unix socket peer credentials (R19)
	AuthWorker AuthMethod = "worker"
	AuthSystem AuthMethod = "system" // internal: event and schedule triggers
)

// Principal is the authenticated caller resolved for one tenant. It is plain
// data; decisions go through Can and through the security.Authorizer.
type Principal struct {
	UserULID    id.ULID      `json:"user_ulid,omitempty"`
	Email       string       `json:"email,omitempty"`
	DeviceID    id.Friendly  `json:"device_id,omitempty"`
	Method      AuthMethod   `json:"method"`
	SystemAdmin bool         `json:"system_admin,omitempty"`
	TenantID    TenantID     `json:"tenant_id,omitempty"`
	Role        Role         `json:"role,omitempty"`
	Grants      []Permission `json:"grants,omitempty"`
}

// Can reports whether the principal holds a permission in its resolved tenant,
// through its role bundle or an individual grant (D48). A system admin only
// passes system-level permissions here; tenant access still needs a membership.
func (p Principal) Can(perm Permission) bool {
	if perm.SystemLevel() {
		return p.SystemAdmin
	}
	if p.Role.Has(perm) {
		return true
	}
	for _, g := range p.Grants {
		if g == perm {
			return true
		}
	}
	return false
}

// Actor returns the audit identity of the principal.
func (p Principal) Actor() string {
	switch {
	case p.Email != "":
		return p.Email
	case p.DeviceID != "":
		return string(p.DeviceID)
	default:
		return string(p.Method)
	}
}
