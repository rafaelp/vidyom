---
title: "Daemon, workers, and jobs"
sidebar_position: 1
---

# Daemon, workers, and job lifecycle

## Job states

`queued → running → waiting_approval → running → completed` is one possible path. Other states include `retrying`, `paused`, `failed`, `cancelled`, and `needs_attention`. A job moves to `needs_attention` when a node with an `unsafe` side-effect class fails or is interrupted, and it stays there until a person resolves it. Every state change is persisted and audited before it becomes externally visible.

## Execution contract

1. Authenticate caller and authorize workflow and inputs.
2. Resolve immutable workflow version, validate the DAG and node schemas.
3. Persist the execution record before scheduling steps.
4. Dispatch node work through the Redis queue to a suitable worker with CPU/GPU/memory/disk requirements. The tenant database remains the source of truth for job state.
5. Record node outputs using references to files or structured values; avoid storing large blobs directly in the database.
6. Persist a checkpoint after each completed node, retry transient failures with backoff according to the node's side-effect class, and reconcile uncertain external side effects.
7. Pause for approval using persisted state rather than leaving the CLI open.
8. Finalize hooks, publish lifecycle events, and retain logs and lineage.

## Idempotency and recovery

Side-effecting nodes must have idempotency keys derived from workflow/job/node/attempt context. Upload operations verify object generations or checksums; publishing connectors reconcile remote video IDs before retrying to avoid duplicate videos. A daemon restart must not cause duplicate backups, clips, or publications. On startup, the daemon reconciles jobs that are `queued` or `running` in the database but missing from Redis, and re-enqueues them.

**Decided:** Every node type declares a side-effect class:

| Class | Retry behavior |
| --- | --- |
| `none` | Pure; retried freely. |
| `idempotent` | Retried with its idempotency key. |
| `reconcilable` | Must query the remote state before any retry. |
| `unsafe` | Never retried automatically; the job moves to `needs_attention`. |

Cancellation is cooperative between nodes: the running node finishes, or aborts if it supports aborting.

## Worker scheduling

Resource-aware dispatch allows a CPU worker for FFmpeg and GPU worker for transcription. Initially, local workers are sufficient. Future expansion to multiple hosts must preserve the same workflow JSON and stable job API. Workers never open database files; they talk to the daemon through its API or Redis.

## Worker cache

**Decided:** Each tenant has its own cache directory with LRU eviction, a configurable maximum size and a 7-day idle expiry. Checksums are verified on reuse, temporary files are deleted at the end of each job, and the cache is encrypted at rest with the tenant key.

## Paths and uploads

When the CLI requests a remote job with `file=/data/class.mp4`, that path belongs to the **daemon host**. Use an explicit resumable `vidyom asset upload` operation for a file located on the client machine. Validate checksum at completion and prevent arbitrary server filesystem traversal.
