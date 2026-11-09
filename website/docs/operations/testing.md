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

A failing optional Loki sink must not stop a job; a failing mandatory validation hook must stop before side effects. Verify audit continuity, traces/log IDs, outbox redelivery, plugin timeout, and nonduplicated event effects.
