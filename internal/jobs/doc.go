// Package jobs is the workflow engine: durable job execution with a checkpoint per
// node, retries by side-effect class, needs_attention, pause, resume, cooperative
// cancel and reconciliation with the queue (§5.5, D4, D11). It implements
// service.Workflows, service.Jobs, service.Approvals, service.Introspection and
// workerproto.Coordinator, and builds node.DaemonEnv for pools engine and daemon.
//
// The tenant database is the source of truth. advance reads it and acts on what it
// finds, so it is safe to call any number of times and after any crash. Everything
// else only wakes it up.
package jobs
