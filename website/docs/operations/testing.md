---
title: "Acceptance tests"
sidebar_position: 1
---

# Acceptance criteria and end-to-end tests

## A. Ingest an original

1. Upload an original of approximately 10 GB.
2. Receive stable ID and persisted job ID; disconnect CLI mid-process.
3. Verify raw original in `archive`, working master in `master`, preview in `proxy`.
4. Verify hashes, transcript timestamps, summaries and semantic index.
5. Reconnect, inspect job logs and confirm `ready_for_clipping`.

## B. Create multiple independent clips

1. Ingest a one-hour yoga class, course or talk.
2. On day one, request a clip by semantic description; approve narrative boundaries and render.
3. Verify independent clip ID, master, proxy, archival backup and parent intervals.
4. On another day, create another clip with a different prompt.
5. Verify **zero reads from the archive role** during both clipping jobs.

## C. Source is already published on YouTube

For a Vidyom-originated upload, confirm that the published source was retained in operational storage. For an external owned upload, import an authorized file into operational storage and index only after successful local/remote persistence. Do not invoke undocumented YouTube download APIs.

## D. Switch storage provider

Rebind `master` from a GCS test implementation to a compatible mock or S3 driver; run identical JSON without workflow changes. Explicitly migrate objects and validate checksums.

## E. Resume and idempotency

Kill a worker during upload and restart the daemon. Ensure no duplicate backups, publications or clip IDs; a job resumes from the last safe checkpoint.

## F. Security

Check private-only network binding and fail-closed behavior, rejected unauthenticated/unauthorized requests, secret redaction, plugin permissions, disabled arbitrary shell and nonpublic catalog access controls.

## G. Hooks and observability

A failing optional Loki sink must not stop a job; a failing mandatory validation hook must stop before side effects. Verify audit continuity (the hash chain verifies end to end), traces/log IDs, outbox redelivery, plugin timeout, and nonduplicated event effects.

## H. Tenant isolation

1. Create two tenants, each with an asset (`ORI-001`) and a clip (`CLIP-001`).
2. As a user of tenant A, try to read, search or publish tenant B's assets by ID, path or storage prefix.
3. Verify every attempt is rejected, tenant B's database file, secrets, cache directory and public library export are untouched, and the attempts appear in the audit.
4. Verify a user belonging to both tenants only acts with the role held in the selected tenant.

## I. Unsafe nodes move to `needs_attention`

1. Run a workflow whose node has side-effect class `unsafe` and kill the worker mid-node.
2. Verify the node is not retried automatically and the job moves to `needs_attention`.
3. Resolve it as a person and confirm the job continues from the last checkpoint with no duplicate side effect.
4. Repeat with a `reconcilable` node and verify it queries the remote state before any retry.

## J. Redis loss and reconciliation

1. Start several jobs, then stop Redis and discard its data.
2. Restart the stack.
3. Verify the daemon finds jobs `queued` or `running` in the tenant database but missing from Redis and re-enqueues them.
4. Verify no duplicate clip IDs, backups or publications.

## K. Two-level approval

1. As an `editor`, approve a `clip_selection` request; verify the same editor cannot approve the `publication` request.
2. As a `publisher`, approve publication and verify a single upload.
3. Change the visibility or public metadata of the public video and verify a new publisher approval is required.
4. Verify a workflow `approvers` rule can tighten but never loosen this policy.

## L. `local_only` enforcement

1. Leave a collection at the default `aiProcessing: local_only`.
2. Run transcription, embeddings and story detection; verify no request leaves the host and an external LLM endpoint is refused.
3. With no local LLM endpoint, verify story detection is unavailable and clip selection falls back to manual review.
4. Mark an asset `external_allowed` and verify the consent record stores who authorized it and when.
