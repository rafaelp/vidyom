// Package queue implements the dispatch queue on Redis Streams with consumer groups
// (D4, I2). Redis holds pointers only: the tenant database stays the source of
// truth, and internal/jobs re-enqueues whatever Redis has lost (§15.5).
package queue
