// Package apiserver is the HTTP server for the routes in internal/contract/api.
// NewHandler registers every route of api.Routes behind one pipeline: request and
// trace identifiers, authentication, the Vidyom-Tenant rule (D32), the route's
// permission, idempotency keys, the audit, then the handler (docs/decisions/api.md,
// I120). Services are reached only through their contracts, so a handler has no
// knowledge of SQLite, Redis or any connector.
package apiserver
