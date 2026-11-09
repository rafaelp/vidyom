// Package scheduler matches tasks to workers by declared resources (D40, I154) and
// starts workflows with a schedule trigger (I22, I155).
//
// Dispatch is pull: a worker reads a pointer from Redis and asks for a lease, and the
// lease is where compatibility is decided. There is no second dispatch path.
package scheduler
