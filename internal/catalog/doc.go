// Package catalog is the asset service over the tenant database, the canonical catalog
// (D24): metadata with history (D35), the external AI policy with its consent record
// (D8), sensitive segments (D30) and lineage (§2.3). Every mutation commits with its
// audit record in one transaction. Decisions are in docs/decisions/core.md, I104, I117
// and I118.
package catalog
