---
title: "Assets and canonical data"
sidebar_position: 1
---

# Canonical asset model

The canonical catalog must outlive any individual vendor integration. MediaCMS is a user interface and catalog adapter, not the only source of truth.

## Main entities

- `Asset`: immutable ID, kind (`original` or `clip`), category, title, description, visibility and lifecycle state.
- `AssetVersion`: distinct rendered media version with hash, codec, dimensions and provenance.
- `MediaLocation`: logical role (`archive`, `master`, `proxy`), connector, object key, generation/version, checksum, availability.
- `Transcript`: time-aligned words/segments with source rendition and language.
- `Chapter`: title, start/end time and summary.
- `SearchDocument`: transcript chunks, vector embeddings, metadata and access policy.
- `Lineage`: parent asset ID, ordered source segments and edit/render provenance.
- `Publication`: provider, remote video ID, URL, visibility, caption and upload status.
- `WorkflowExecution` / `NodeExecution`: immutable workflow version, job state, timing, outputs, retries, tracing IDs.
- `Approval`: approver identity, decision, revision, time and audit entry.

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
