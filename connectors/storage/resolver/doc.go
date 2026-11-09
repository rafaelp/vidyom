// Package resolver turns a tenant and a role into an open storage instance (§7.2,
// D6), and implements service.Storages for the daemon. Workflows name roles, never
// buckets, so changing a role's instance in a tenant's storages.json changes no
// workflow (§15.4). Moving the data stays a separate operation (§7.1).
package resolver
