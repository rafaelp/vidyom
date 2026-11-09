// Package vault is the per-tenant encrypted vault (D13) and the master key
// providers (I10).
//
// A 256-bit master key, unsealed at boot by exactly one explicitly configured
// provider, wraps one data key per tenant; the wrapped keys live in system.db.
// Secrets are sealed under a key derived from the tenant's data key and stored in
// that tenant's database as ciphertext only. There is no default provider and no
// fallthrough: any failure means no vault, and the daemon exits with no listener.
//
// Details and the crash-safety order of rotation are in
// docs/decisions/core.md, I109 to I111.
package vault
