---
title: "Editorial and publication workflows"
sidebar_position: 1
---

# Editorial and publication workflows

A new edited video or clip is a standalone canonical asset. Its creation, backup, catalog entry, and public publication are separate lifecycle stages with persistent statuses.

## Proposed `publish-video` subworkflow

1. Locate approved asset and the source master from logical `master` storage.
2. Prepare title, description, tags, captions, thumbnail and requested output profile.
3. Request publication approval (kind `publication`) and **persist** `waiting_approval`; the CLI may disconnect.
4. Render and verify the final publishable file. Retain a copy in operational storage.
5. Call the selected publishing connector (`youtube-main`, `vimeo-main`, etc.).
6. Poll or reconcile remote processing and visibility as supported.
7. Store per-provider remote ID, URL, state, published version and metadata.
8. Publish only approved metadata and public URLs into the public catalog.

## Approval

**Decided:** Obtaining authorization from speakers and participants for their image and voice is the tenant's responsibility. Vidyom does not track or enforce these authorizations, and the publication approval screen states that the tenant is responsible.

**Decided:** Approval has two levels. An `editor` approves the clip selection (kind `clip_selection`); a `publisher` or `admin` approves publication (kind `publication`). Changing the visibility or the public metadata of an already public video requires a new publisher approval. A workflow can tighten the rule through `approvers`, never loosen it.

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

**Decided:** YouTube is the first destination. **Future:** Vimeo, PeerTube and Instagram Reels (vertical clips reframed from the 4K master) follow on the roadmap; Spotify is not planned. Other destinations, such as Dailymotion, remain possible community connectors but are not on the roadmap.

Provider remote IDs are independent of canonical asset IDs. Uploads must be resumable when the destination supports it, reconciled before retrying, and not duplicated when jobs restart. Supported thumbnail, captions, scheduling and privacy settings vary by platform and must be feature-detected.

## Public library

**Decided:** The public library is a Next.js site (server rendering for SEO, API routes for semantic search over `public.db`, deployable on Vercel). It displays only approved public assets, searchable by theme, chapter, story, teacher and transcript. It can embed or link to YouTube and later the other destinations, with deep links to timecodes when supported. Private originals, unapproved clips, backing storage addresses and private transcripts must not leak through the public index.

**Decided:** Transcripts are private by default. A transcript or its captions reach the public library or a destination only together with an approved public video. Editors can mark sensitive segments (for example names, health or minors); marked segments are excluded from `public.db` and from public captions.

**Decided:** Per tenant, the daemon exports a read-only `public.db` (SQLite) containing only approved public records and their embeddings, and ships it to the public site's host. The site queries it and computes query embeddings on CPU for semantic search. The daemon is never exposed publicly.

## Withdrawal and deletion

**Decided:** Withdrawal, which unpublishes from every destination and from the public library, is immediate and can be done by a publisher or admin. Full deletion is done by an admin; the archive copy is purged when the archive's minimum storage duration ends (365 days on GCS Archive) and stays inaccessible until then. The audit keeps the fact of the deletion without the content. **Validate:** legal review under the LGPD of deferring the archive purge.
