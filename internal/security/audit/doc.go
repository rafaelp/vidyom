// Package audit is the append-only hash-chained audit (D23). The repository writes
// the chain, this package verifies it, exports it to archive storage and prunes
// exported records past retention. Records state the fact of an action, never
// content or secrets. Details are in docs/decisions/core.md, I107 and I108.
package audit
