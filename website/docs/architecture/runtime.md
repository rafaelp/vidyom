---
title: "Daemon, workers, and jobs"
sidebar_position: 1
---

# Daemon, workers, and job lifecycle

## Job states

`queued → running → waiting_approval → running → completed` is one possible path. Other states include `retrying`, `paused`, `failed`, and `cancelled`. Every state change is persisted and audited before it becomes externally visible.

## Execution contract

1. Authenticate caller and authorize workflow and inputs.
2. Resolve immutable workflow version, validate the DAG and node schemas.
3. Persist the execution record before scheduling steps.
4. Assign node work to a suitable worker with CPU/GPU/memory/disk requirements.
5. Record node outputs using references to files or structured values; avoid storing large blobs directly in the database.
6. Checkpoint, retry transient failures with backoff, and reconcile uncertain external side effects.
7. Pause for approval using persisted state rather than leaving the CLI open.
8. Finalize hooks, publish lifecycle events, and retain logs and lineage.

## Idempotency and recovery

Side-effecting nodes must have idempotency keys derived from workflow/job/node/attempt context. Upload operations verify object generations or checksums; publishing connectors reconcile remote video IDs before retrying to avoid duplicate videos. A daemon restart must not cause duplicate backups, clips, or publications.

## Worker scheduling

Resource-aware dispatch allows a CPU worker for FFmpeg and GPU worker for transcription. Initially, local workers are sufficient. Future expansion to multiple hosts must preserve the same workflow JSON and stable job API.

## Paths and uploads

When the CLI requests a remote job with `file=/data/class.mp4`, that path belongs to the **daemon host**. Use an explicit resumable `vidyom asset upload` operation for a file located on the client machine. Validate checksum at completion and prevent arbitrary server filesystem traversal.
