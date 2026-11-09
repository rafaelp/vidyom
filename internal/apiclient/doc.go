// Package apiclient is the HTTP client of the daemon API. Every request is built
// from the route table in internal/contract/api, which also decides the tenant
// header, the credential and the idempotency key, so the client cannot drift from
// the contract. It adds the three transports of §5.3 (Unix socket, HTTPS with
// fingerprint pinning, SSH tunnel for host operators), SSE streaming with
// reconnection and resumable uploads.
//
// The package is free of cgo and of database packages (make check-static).
//
// Owner: component cli (docs/plans/cli.md). Decisions: docs/decisions/cli.md.
package apiclient
