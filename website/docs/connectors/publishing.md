---
title: "Publishing connectors"
sidebar_position: 1
---

# Publishing connectors

Publishing is provider-neutral. Destinations are YouTube, **Vimeo, PeerTube and Instagram Reels** (vertical clips reframed from the 4K master). Spotify is not planned. Other destinations, for example Dailymotion, can be added as community connectors.

## Example configuration

```json
{
  "publishing": {
    "destinations": {
      "youtube-main": {
        "driver": "youtube",
        "credentialsRef": "youtube-oauth",
        "visibility": "private"
      },
      "vimeo-main": {
        "driver": "vimeo",
        "credentialsRef": "vimeo-oauth",
        "visibility": "private"
      }
    },
    "defaults": {
      "requireApproval": true,
      "uploadResumable": true,
      "savePublicationRecord": true
    }
  }
}
```

## Operations and capabilities

The common interface proposes `upload`, `get`, `update`, `delete`, `setVisibility`, `uploadThumbnail`, `uploadCaptions`, `getStatus`, `getUrl`, and `capabilities`. Drivers advertise which operations they implement and specific platform constraints such as privacy modes, schedule support, title length, subtitle formats and thumbnail APIs.

## Multi-destination publication

One canonical `CLIP-001` may be published to `youtube-main` today and `vimeo-main` months later, without re-rendering from archive. Each publication has provider-specific remote ID, URL, status, visibility, uploaded version and timestamps. Public library sync reads the canonical publication records, not provider tables directly.

## Safety and approvals

Uploads are private/unlisted by default where supported, require approval before public visibility, and record the actor who approved. Uncertain outcomes are reconciled before an upload is retried, which prevents duplicates. Destructive remote deletes require independent authorization and do not cascade to the canonical original or backup.

### Two-level approval

Approval has two levels. An `editor` approves the clip selection (`approval.request` of kind `clip_selection`); a `publisher` (or `admin`) approves publication (kind `publication`). Changing the visibility or the public metadata of an already public video requires a new publisher approval. A workflow can tighten the rule through `approvers`, never loosen it. Roles are per tenant: `viewer`, `editor`, `publisher`, `admin`.

### Withdrawal and deletion

Withdrawal, which unpublishes from every destination and from the public library, is immediate and can be done by a publisher or admin. Full deletion is done by an admin. The archive copy is purged when the archive's minimum storage duration ends (365 days on GCS Archive) and stays inaccessible until then. The audit keeps the fact of the deletion without the content. Review the deferred archive purge under the LGPD with your own counsel.
