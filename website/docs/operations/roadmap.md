---
title: "Implementation roadmap"
sidebar_position: 1
---

# Implementation roadmap

This is a **proposed sequencing plan**, not a commitment to particular dates or a claim that components are already built.

## Phase 0 — Contracts and feasibility

Define canonical data model, JSON workflow schema, node and connector interfaces. Spike sqlite-vec at tenant scale, Litestream with GCS, Redis AOF reconciliation under crash tests, TPM/systemd-creds and KMS unsealing, VideoVortex indexing/clip boundaries, GCS behavior, YouTube API permissions, and faster-whisper large-v3 accuracy on the target content. MediaCMS proxy import is spiked only for the optional connector.

**Exit criteria:** reproducible proof of concept for a 10 GB recording; validated external assumptions and documented limitations.

## Phase 1 — Secure runtime + original ingest

Implement `vidyom`, `vidyomd`, local worker, Docker Compose deployment, per-tenant SQLite databases plus `system.db`, Redis job queue with startup reconciliation, multi-tenancy (tenants, roles, storage prefixes, budgets), HTTPS with client pairing, the encrypted secrets vault with master key unsealing, GCS connector, FFmpeg transcode, faster-whisper transcription and search indexing (FTS5, sqlite-vec). The private catalog is the tenant database, reviewed through the CLI with proxy links. Build complete `ingest-original.json` and logs/health.

**Exit criteria:** restart-safe ingest, verified backup, permanent Standard master, searchable transcript and `ready_for_clipping` status.

## Phase 2 — Semantic clipping

Implement story finding, timestamp validation, proxy preview, review/approval, master-based render, lineage, `ingest-derived.json`, and idempotency. Test several clips from the same original created on different days.

**Exit criteria:** repeatable clip generation without archive reads, each clip with its own verified backup and catalog entry.

## Phase 3 — Publishing and public library

Implement YouTube connector, generic publishing interface, `publish-video.json`, independent publication records, two-level approval policy and Next.js public site fed by the per-tenant read-only `public.db` export. Support re-publication to a second destination via a mock connector.

**Exit criteria:** approved clip is uploaded once, publication status reconciles, and only public data appears in search.

## After Phase 3 — Rewind

Prioritized right after the MVP: the Rewind integration (visual transcript navigation and montage) as an optional tool for editors.

## Later destinations and connectors

After YouTube: Vimeo, PeerTube and Instagram Reels (vertical clips reframed from the 4K master). Spotify is not planned. Other destinations remain possible community connectors but are not on the roadmap. MediaCMS is an optional catalog connector, one instance per tenant. Kubernetes is **Future**.

## Phase 4 — Extensibility and production operations

Plugin runtime, global/workflow/node hooks, durable Event Bus, observability fan-out and retries, distributed workers, more connectors, advanced CLI/MCP access and operational dashboards.

**Exit criteria:** a new workflow is composed from JSON; a new provider is installed without changing core workflows; a plugin reacts safely to lifecycle events.
