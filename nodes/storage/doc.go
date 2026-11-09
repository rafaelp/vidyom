// Package storagenodes implements the storage.* nodes (§7.2). They name roles, never
// buckets. Reading the archive role is refused unless the job holds
// storage.archive.restore and the node asks for it by parameter: only restore-archived
// does (§3.2, D42).
package storagenodes
