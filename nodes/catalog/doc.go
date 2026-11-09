// Package catalognodes implements the catalog.* nodes the node catalog assigns to
// component engine. They run in pool daemon (I7), because they need the tenant
// database, and write state and events in one transaction (§8.2). catalog.public.sync
// belongs to component publishing.
package catalognodes
