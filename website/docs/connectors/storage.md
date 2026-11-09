---
title: "Storage connectors"
sidebar_position: 1
---

# Storage connectors

**Decided:** GCS is the first storage connector, not a dependency of the engine. Future adapters should support AWS S3, Dropbox, SFTP, local filesystems/NAS, and others. Workflows address logical storage roles, not concrete cloud providers.

## Roles

- `archive`: untouched original recordings and approved masters retained for long-term protection. Initially **GCS Archive**.
- `master`: production-quality, permanently accessible copies used by clipping and rendering. Initially **GCS Standard**.
- `proxy`: playback copies; initially **GCS Standard**, potentially same bucket as master with different prefix.

GCS Standard does not impose an archive-style retrieval fee, but network egress, operations and storage still cost money. GCS Archive supports direct object reads but may incur retrieval and minimum-duration charges. Vidyom must **prohibit archive reads as a clipping fallback** even where technically possible.

## Archive policy and expiry

**Decided:** Raw originals are always archived, permanently. The master of a clip or edited video is archived when its selection is approved. Rejected or never-approved renders stay only in the `master` role and expire after **30 days**, configurable per tenant.

## Tenant isolation

**Decided:** Vidyom is multi-tenant. Each tenant has its own storage prefix, with an optional dedicated bucket and credentials. The example below shows a single tenant's bindings.

## Example provider binding

```json
{
  "storages": {
    "archive": {
      "driver": "gcs",
      "bucket": "videos-archive",
      "storageClass": "ARCHIVE",
      "credentialsRef": "gcs-main"
    },
    "master": {
      "driver": "gcs",
      "bucket": "videos-standard",
      "prefix": "masters/",
      "storageClass": "STANDARD",
      "credentialsRef": "gcs-main"
    },
    "proxy": {
      "driver": "gcs",
      "bucket": "videos-standard",
      "prefix": "proxies/",
      "credentialsRef": "gcs-main"
    }
  }
}
```

## Node example

```json
{
  "id": "backup",
  "type": "storage.put",
  "typeVersion": 1,
  "parameters": {
    "storage": "archive",
    "source": "${inputs.file}",
    "key": "originals/${nodes.register.output.id}/original.mp4",
    "verifyChecksum": true
  }
}
```

## Common operations

`put`, `get`, `stat`, `list`, `exists`, `delete`, `copy`, `move`, `checksum` and `downloadRange` where supported. A connector must report capabilities such as `rangeRead`, `serverSideCopy`, `versioning`, `multipartUpload`, `checksumVerification`, `storageClasses`, and whether a restore stage is required.

Some operations (e.g. server-side copy) cannot be guaranteed on Dropbox or SFTP. Remote copy emulation should clearly state potential egress, temporary disk usage and failure modes.

## Cache policy

A worker may temporarily cache a master in local disk to improve repeated operations, but the **persistent operational master is stored in the direct-access role**. **Decided:** Each tenant has its own worker cache directory, managed as an LRU with a configurable maximum size and a 7-day idle expiry. Reused files are checksum verified, temporary files are deleted at the end of each job, and the cache is **encrypted at rest with the tenant key**. Uploads/downloads should be resumable where feasible.

## Security

Use least-privilege service identities, validate object keys, avoid arbitrary URL fetching, and log only logical references rather than credentials or signed private URLs. Verify hashes and provider object generations after upload. Never silently migrate to another storage class when doing so violates `ready_for_clipping` access guarantees.
