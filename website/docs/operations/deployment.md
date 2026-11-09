---
title: "Deployment and operations"
sidebar_position: 1
---

# Deployment and operations

## Initial topology (proposed)

- Linux host runs `vidyomd`, one or more local `vidyom-worker` processes, PostgreSQL and optional telemetry exporter.
- Separate persistent object storage: `archive` and `master` roles initially mapped to Google Cloud Storage Archive and Standard.
- Mac/Linux/Windows users run the `vidyom` CLI over SSH or a Tailscale/NetBird-protected endpoint.
- Optional MediaCMS service displays the private catalog; a separate Next.js app presents approved public media.

## Production startup

Use systemd or an equivalent service supervisor to restart the daemon and workers. Run under a non-root service account, assign durable working directories, configure database backups, rotate local logs, enforce connection timeouts, and monitor queue health. The runtime must not require an interactive shell to continue running.

## Availability controls

Readiness verifies database migration state, keyring availability, workflow/node registry health and required core storage roles. Liveness checks daemon responsiveness. A partially unavailable connector should degrade only workflows that require it, not crash unrelated jobs.

## Remote upload

`vidyom asset upload` transfers local files to the remote server using resumable chunks with checksum verification. Metadata records import origin and source permissions. YouTube-only imports must use owner-authorized copies acquired before ingestion. Do not make third-party video downloading an unverified automatic dependency.

## Backups and disaster recovery

Archive copies of originals and finalized clips are verified. Store canonical metadata and database backups independently. Disaster recovery is an explicit administrative operation; clipping never triggers archival retrieval. Test restoration in isolation without changing asset identity or lineage.

## Monitoring

Basic operation remains available through `vidyom daemon health`, `vidyom job list`, `vidyom job logs`, and `vidyom doctor` even with no Grafana/Loki/Datadog deployment.
