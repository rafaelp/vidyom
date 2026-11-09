---
title: "Publishing connectors"
sidebar_position: 1
---

# Publishing connectors

**Decided:** Publishing must be provider-neutral. YouTube is the first destination; Vimeo, PeerTube, Dailymotion and others are future pluggable connectors.

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

Uploads are private/unlisted by default where supported, require approval before public visibility, and record the actor who approved. Reconcile uncertain outcomes before retrying uploads to prevent duplicates. Destructive remote deletes require independent authorization and must not cascade to the canonical original or backup.
