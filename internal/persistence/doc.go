// Package persistence implements internal/contract/repo over SQLite (D5): one
// tenant.db per tenant plus a global system.db, with migrations, WAL, FTS5 and
// sqlite-vec, opened only through sqlitedriver.
//
// Only the daemon links this package; the CLI and the worker never do (make
// check-static). Repositories are small structs over a querier, so the same code
// runs inside and outside a transaction. Inside InTx, use only the repositories the
// callback receives (docs/decisions/core.md, I102).
package persistence
