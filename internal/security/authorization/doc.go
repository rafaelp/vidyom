// Package authorization resolves a principal in a tenant and decides permissions
// (D6, D7, D48). Roles are bundles of granular permissions and an admin can grant one
// more without changing the role. A principal with no membership gets not_found, so
// cross-tenant access answers 404 (§12.1). Edge cases are in
// docs/decisions/core.md, I113.
package authorization
