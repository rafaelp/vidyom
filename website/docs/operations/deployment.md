---
title: "Deployment and operations"
sidebar_position: 1
---

# Deployment and operations

## Initial topology

- **Decided:** Docker Compose is the supported MVP deployment. It runs the daemon (`vidyomd`), the workers (with FFmpeg and the Python AI worker packaged) and Redis, plus an optional telemetry exporter.
- **Decided:** Redis is a required service. It is the job dispatch queue, with AOF persistence enabled. The tenant database stays the source of truth for job state.
- **Decided:** Data lives in SQLite: one database file per tenant plus a global `system.db`. There is no PostgreSQL. Only the daemon opens database files; workers talk to it through its API or Redis.
- **Decided:** Separate persistent object storage: `archive` and `master` roles initially mapped to Google Cloud Storage Archive and Standard. Each tenant has its own storage prefix, with an optional dedicated bucket and credentials.
- **Decided:** Mac/Linux/Windows users run the `vidyom` CLI over HTTPS (TLS 1.3) after pairing the device. SSH tunneling is only for host operators, and binding to a private Tailscale/NetBird interface remains supported.
- **Decided:** MediaCMS is an optional connector (one instance per tenant) for teams that want a web UI. By default, review happens through the CLI with proxy links. A separate Next.js app presents approved public media.
- **Future:** Kubernetes.

## Production startup

Run the Compose stack with a restart policy. The Go binaries can still run natively under systemd, but that is not the primary path. Run under a non-root service account, assign durable working directories, rotate local logs, enforce connection timeouts, and monitor queue health. The runtime must not require an interactive shell to continue running.

On startup, the daemon reconciles jobs that are `queued` or `running` in the database but missing from Redis, and re-enqueues them.

## Remote access and pairing

**Decided:** The remote API is HTTPS with TLS 1.3. An admin generates a one-time pairing code; the CLI exchanges it for a per-device credential that is revocable and rotated. The local Unix socket remains for same-host use. Private-interface binding keeps the same fail-closed rules.

## Secrets and master key

**Decided:** Server-side secrets live in an encrypted vault inside each tenant database, using envelope encryption: one data key per tenant, wrapped by a master key. The master key is unsealed at boot through TPM/systemd-creds or a cloud KMS, with no manual passphrase. Client machines keep using the OS keyring. A missing or sealed master key makes the daemon fail closed.

**Validate:** TPM/systemd-creds and KMS unsealing on the target hosts.

## Availability controls

Readiness verifies database migration state, master key availability, Redis connectivity, workflow/node registry health and required core storage roles. Liveness checks daemon responsiveness. A partially unavailable connector should degrade only workflows that require it, not crash unrelated jobs.

## Remote upload

`vidyom asset upload` transfers local files to the remote server using resumable chunks with checksum verification. Metadata records import origin and source permissions. YouTube-only imports must use owner-authorized copies acquired before ingestion. Do not make third-party video downloading an unverified automatic dependency.

## Backups and disaster recovery

**Decided:** SQLite databases run in WAL mode and are continuously backed up with Litestream to the archive storage provider. **Validate:** Litestream with GCS, and Redis AOF durability and reconciliation under crash tests.

Archive copies of originals and finalized clips are verified. Store canonical metadata and database backups independently. Disaster recovery is an explicit administrative operation; clipping never triggers archival retrieval. Test restoration in isolation without changing asset identity or lineage.

## Monitoring

Basic operation remains available through `vidyom daemon health`, `vidyom job list`, `vidyom job logs`, and `vidyom doctor` even with no Grafana/Loki/Datadog deployment.
