---
title: "Implementation roadmap"
sidebar_position: 1
---

# Implementation roadmap

This is a **proposed sequencing plan**, not a commitment to particular dates or a claim that components are already built.

## Phase 0 — Contracts and feasibility

Define canonical data model, JSON workflow schema, node and connector interfaces. Spike MediaCMS proxy import, VideoVortex indexing/clip boundaries, GCS behavior, YouTube API permissions, keyring in headless Linux, and the secure daemon connection model.

**Exit criteria:** reproducible proof of concept for a 10 GB recording; validated external assumptions and documented limitations.

## Phase 1 — Secure runtime + original ingest

Implement `vidyom`, `vidyomd`, local worker, persisted jobs, auth, keyring, GCS connector, FFmpeg transcode, faster-whisper transcription, search indexing and MediaCMS catalog adapter. Build complete `ingest-original.json` and logs/health.

**Exit criteria:** restart-safe ingest, verified backup, permanent Standard master, searchable transcript and `ready_for_clipping` status.

## Phase 2 — Semantic clipping

Implement story finding, timestamp validation, proxy preview, review/approval, master-based render, lineage, `ingest-derived.json`, and idempotency. Test several clips from the same original created on different days.

**Exit criteria:** repeatable clip generation without archive reads, each clip with its own verified backup and catalog entry.

## Phase 3 — Publishing and public library

Implement YouTube connector, generic publishing interface, `publish-video.json`, independent publication records, approval policy and Next.js public catalog. Support re-publication to a second destination via a mock connector or pilot Vimeo driver.

**Exit criteria:** approved clip is uploaded once, publication status reconciles, and only public data appears in search.

## Phase 4 — Extensibility and production operations

Plugin runtime, global/workflow/node hooks, durable Event Bus, observability fan-out and retries, distributed workers, more connectors, advanced CLI/MCP access and operational dashboards.

**Exit criteria:** a new workflow is composed from JSON; a new provider is installed without changing core workflows; a plugin reacts safely to lifecycle events.
