---
title: "Assets and canonical data"
sidebar_position: 1
---

# Canonical asset model

The canonical catalog must outlive any individual vendor integration. **Decided:** The canonical catalog is Vidyom's tenant database. MediaCMS is an optional connector (one instance per tenant) for teams that want a web UI, not a source of truth. In the MVP, review happens through the CLI with proxy links.

## Storage

**Decided:** Each tenant has one SQLite database file holding its assets, jobs, checkpoints, transcripts, search indexes, audit log and encrypted secrets. A global `system.db` holds tenants, users, roles, device credentials, the plugin registry and wrapped tenant data keys. Full-text search uses FTS5 and vector search uses sqlite-vec. **Validate:** sqlite-vec has no approximate index yet, so asset-scoped search is the main path and tenant-wide semantic search must be load-tested. Databases run in WAL mode with continuous Litestream backup to the archive storage provider, and only the daemon opens them.

**Decided:** Friendly IDs use the prefixes `ORI-`, `CLIP-`, `EDT-`, `JOB-`, `APR-`, `PUB-`, `CLIENT-` and `UPL-`. Each tenant has its own sequence per prefix (at least 3 digits, never reused), so a friendly ID is resolved within the tenant given by the `Vidyom-Tenant` header. They are aliases for an immutable, globally unique ULID technical key, and storage paths follow the prefix (for example `clips/CLIP-001/v1/master.mp4`).

## Asset states

**Decided:** An asset has one lifecycle state: `registered`, `ingesting`, `ready_for_clipping`, `ingest_failed`, `editorial_review`, `approved`, `rejected`, `published`, `withdrawn` or `archived_only`. The state is independent of job state and of visibility. `published` requires a confirmed publication with a URL taken from the destination's API response.

## Versions

**Decided:** A media version becomes immutable once it is published (at least one confirmed publication). Before that, the files of a version can be replaced in place. After publication, any new render, transcode or file replacement creates a new version (`v2`, `v3`, and so on). Editing only metadata never creates a media version, but the change is recorded in the history. Storage keys include the version, and each publication records which version was sent.

**Decided:** When the published file is byte-identical to the master, `published_file` points to the master instead of duplicating it.

## Input URIs

**Decided:** Workflow file inputs use one URI form: `server:` (a path on the daemon host), `upload:` (a completed client upload), `storage://role/key` (a logical storage location) or `import:connector/id` (an item from an import connector).

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

## Clips of clips

**Decided:** A derivative that reaches `ready_for_clipping` can be the source of a new clip. Intervals are converted to the root original's timeline and, when the edit is continuous, rendered from the root original's master. The lineage records the full chain of parents.

## Portable exports

**Decided:** A per-asset metadata export package is available, without the masters. Export metadata as JSON, transcript as time-aligned JSON plus SRT/VTT, summary as Markdown, thumbnails as JPG/PNG, and media as ordinary MP4/MOV. Preserve content IDs, hashes, source intervals, publication mappings and ownership/license metadata. CSV catalog exports are an optional convenience; no proprietary database export should be required for an archive migration.
