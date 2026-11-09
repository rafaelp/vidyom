---
title: "Assets and canonical data"
sidebar_position: 1
---

# Canonical asset model

The canonical catalog must outlive any individual vendor integration. **Decided:** The canonical catalog is Vidyom's tenant database. MediaCMS is an optional connector (one instance per tenant) for teams that want a web UI, not a source of truth. In the MVP, review happens through the CLI with proxy links.

## Storage

**Decided:** Each tenant has one SQLite database file holding its assets, jobs, checkpoints, transcripts, search indexes, audit log and encrypted secrets. A global `system.db` holds tenants, users, roles, device credentials, the plugin registry and wrapped tenant data keys. Full-text search uses FTS5 and vector search uses sqlite-vec. **Validate:** sqlite-vec has no approximate index yet, so asset-scoped search is the main path and tenant-wide semantic search must be load-tested. Databases run in WAL mode with continuous Litestream backup to the archive storage provider, and only the daemon opens them.

**Decided:** Friendly IDs use the prefixes `ORI-`, `CLIP-`, `EDT-`, `JOB-`, `APR-`, `PUB-` and `CLIENT-`. They are aliases for an immutable ULID technical key, and storage paths follow the prefix (for example `clips/CLIP-001/v1/master.mp4`).

## Main entities

- `Tenant`: organization with its own database, storage prefix, secrets, budget and roles per user.
- `Asset`: immutable ID, kind (`original` or `clip`), category, title, description, visibility and lifecycle state.
- `AssetVersion`: distinct rendered media version with hash, codec, dimensions and provenance.
- `MediaLocation`: logical role (`archive`, `master`, `proxy`), connector, object key, generation/version, checksum, availability.
- `Transcript`: time-aligned words/segments with source rendition and language, on the master's timeline.
- `Chapter`: title, start/end time and summary.
- `SearchDocument`: transcript chunks, vector embeddings (with the `embedding_model` stored per vector), metadata and access policy.
- `Lineage`: parent asset ID, ordered source segments and edit/render provenance.
- `Publication`: provider, remote video ID, URL, visibility, caption and upload status.
- `WorkflowExecution` / `NodeExecution`: immutable workflow version, job state, timing, outputs, retries, tracing IDs.
- `Approval`: approver identity, decision, revision, time and audit entry.

**Decided:** The audit table is append-only and hash-chained (each record includes the previous record's hash). The chain is exported daily to archive storage, with a minimum retention of one year, configurable. Per tenant, the daemon also exports a read-only `public.db` (SQLite) with only approved public records and their embeddings for the public site.

## Example original record

```json
{
  "id": "ORI-001",
  "kind": "original",
  "category": "yoga_class",
  "title": "Hatha Yoga — Foundations",
  "status": "ready_for_clipping",
  "visibility": "private",
  "storage": {
    "archive": "storage://archive/originals/ORI-001/original.mp4",
    "master": "storage://master/originals/ORI-001/master.mp4",
    "proxy": "storage://proxy/originals/ORI-001/proxy.mp4"
  },
  "transcript": "ORI-001/transcript.json",
  "parents": [],
  "publications": []
}
```

## Example clip lineage

```json
{
  "id": "CLIP-001",
  "kind": "clip",
  "source": {
    "asset_id": "ORI-001",
    "segments": [{"start_seconds": 648, "end_seconds": 1080}]
  },
  "visibility": "private",
  "publication_status": "draft"
}
```

## Portable exports

Export metadata as JSON, transcript as time-aligned JSON plus SRT/VTT, summary as Markdown, thumbnails as JPG/PNG, and media as ordinary MP4/MOV. Preserve content IDs, hashes, source intervals, publication mappings and ownership/license metadata. CSV catalog exports are an optional convenience; no proprietary database export should be required for an archive migration.
