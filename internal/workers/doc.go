// Package workers is the worker loop of vidyom-worker: it registers with the daemon,
// reads task pointers from a pool's stream, leases each task, runs its node with a
// task environment, sends heartbeats that carry progress and the cancel flag, and
// reports the outcome before acknowledging the stream entry (I4). It opens no
// database file (D5): everything a task needs comes from its lease.
//
// The package never imports a node or connector implementation. Those arrive through
// the registries that cmd/vidyom-worker passes in.
package workers
