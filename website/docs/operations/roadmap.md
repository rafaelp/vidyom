---
title: "Delivered capabilities"
sidebar_position: 1
---

# Delivered capabilities

Every phase of the Vidyom roadmap is implemented. This page lists what each phase delivered.

## Phase 0 — Contracts

The canonical data model, JSON workflow schema, and node and connector interfaces. Assumptions that depend on your hosts, content or providers are listed in the [deployment checks](../reference/open-questions.md).

## Phase 1 — Secure runtime + original ingest

`vidyom`, `vidyomd`, local worker, Docker Compose deployment, per-tenant SQLite databases plus `system.db`, Redis job queue with startup reconciliation, multi-tenancy (tenants, roles, storage prefixes), HTTPS with client pairing, the encrypted secrets vault with master key unsealing, GCS connector, FFmpeg transcode, faster-whisper transcription and search indexing (FTS5, sqlite-vec). The private catalog is the tenant database, reviewed through the CLI with proxy links. Includes the complete `ingest-original.json` and logs/health.

**Delivers:** restart-safe ingest, verified backup, permanent Standard master, searchable transcript and `ready_for_clipping` status.

## Phase 2 — Semantic clipping

Story finding, timestamp validation, proxy preview, review/approval, master-based render, lineage, `ingest-derived.json`, and idempotency.

**Delivers:** repeatable clip generation without archive reads, each clip with its own verified backup and catalog entry.

## Phase 3 — Publishing and public library

YouTube connector, generic publishing interface, `publish-video.json`, independent publication records, two-level approval policy and Next.js public site fed by the per-tenant read-only `public.db` export. Supports re-publication to a second destination.

**Delivers:** an approved clip is uploaded once, publication status reconciles, and only public data appears in search.

## Rewind

The Rewind integration (visual transcript navigation and montage) as an optional tool for editors.

## More destinations and connectors

Vimeo, PeerTube and Instagram Reels (vertical clips reframed from the 4K master). Spotify is not planned. Other destinations remain possible community connectors. MediaCMS is an optional catalog connector, one instance per tenant. Kubernetes is not supported yet.

## Phase 4 — Extensibility and production operations

Plugin runtime, global/workflow/node hooks, observability fan-out and retries, distributed workers, more connectors, advanced CLI access and operational dashboards. MCP access is not supported yet.

**Delivers:** a new workflow is composed from JSON; a new provider is installed without changing core workflows; a plugin reacts safely to lifecycle hooks.

## Phase 5 — Event Bus, budgets and audit

The durable Event Bus, per-tenant budgets and cost alerts, and the hash-chained audit.
