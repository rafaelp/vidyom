---
title: "Editorial and publication workflows"
sidebar_position: 1
---

# Editorial and publication workflows

A new edited video or clip is a standalone canonical asset. Its creation, backup, catalog entry, and public publication are separate lifecycle stages with persistent statuses.

## Proposed `publish-video` subworkflow

1. Locate approved asset and the source master from logical `master` storage.
2. Prepare title, description, tags, captions, thumbnail and requested output profile.
3. Request editorial approval and **persist** `waiting_approval`; the CLI may disconnect.
4. Render and verify the final publishable file. Retain a copy in operational storage.
5. Call the selected publishing connector (`youtube-main`, `vimeo-main`, etc.).
6. Poll or reconcile remote processing and visibility as supported.
7. Store per-provider remote ID, URL, state, published version and metadata.
8. Publish only approved metadata and public URLs into the public catalog.

## One asset, multiple destinations

```json
{
  "asset_id": "CLIP-001",
  "publications": [
    {"destination": "youtube-main", "remote_id": "example-youtube-id", "status": "published"},
    {"destination": "vimeo-main", "remote_id": "example-vimeo-id", "status": "published"}
  ]
}
```

Provider remote IDs are independent of canonical asset IDs. Uploads must be resumable when the destination supports it, reconciled before retrying, and not duplicated when jobs restart. Supported thumbnail, captions, scheduling and privacy settings vary by platform and must be feature-detected.

## Public library

A proposed Next.js site displays only approved public assets, searchable by theme, chapter, story, teacher and transcript. It can embed or link to YouTube and later Vimeo/other platforms, with deep links to timecodes when supported. Private originals, unapproved clips, backing storage addresses and private transcripts must not leak through the public index.
