---
title: "Deployment and operations"
sidebar_position: 1
---

# Deployment and operations

## Topology

- Docker Compose is the supported deployment. It runs the daemon (`vidyomd`), the workers (with FFmpeg and the Python AI worker packaged) and Redis, plus an optional Compose profile with Grafana, Loki and Tempo (see [observability](./observability.md)).
- The whole project runs on a single host running Linux, macOS or Windows. Docker Compose is the supported path on all three (Docker Desktop or WSL2 on macOS and Windows), and the Go binaries are built for all three. Docker on macOS has no GPU access, so local AI runs on CPU or uses a configured local or external endpoint. Test AI performance on macOS without a GPU on your own hardware before relying on it.
- CLI-to-daemon transport on the same host is a Unix domain socket on Linux and HTTPS on loopback when the daemon runs inside Docker Desktop (macOS and Windows), because socket files cannot cross the VM boundary.
- Redis is a required service. It is the job dispatch queue, with AOF persistence enabled. The tenant database stays the source of truth for job state.
- Data lives in SQLite: one database file per tenant plus a global `system.db`. There is no PostgreSQL. Only the daemon opens database files; workers talk to it through its API or Redis.
- Separate persistent object storage: `archive` and `master` roles are mapped to Google Cloud Storage Archive and Standard. Each tenant has its own storage prefix, with an optional dedicated bucket and credentials.
- Mac/Linux/Windows users run the `vidyom` CLI over HTTPS (TLS 1.3) after pairing the device. SSH tunneling is only for host operators, and binding to a private Tailscale/NetBird interface remains supported.
- MediaCMS is an optional connector (one instance per tenant) for teams that want a web UI. By default, review happens through the CLI with proxy links. A separate Next.js app presents approved public media.
- Kubernetes is not supported yet.

## Production startup

Run the Compose stack with a restart policy. The Go binaries can still run natively under systemd, but that is not the primary path. Run under a non-root service account, assign durable working directories, rotate local logs, enforce connection timeouts, and monitor queue health. The runtime does not require an interactive shell to continue running.

On startup, the daemon reconciles jobs that are `queued` or `running` in the database but missing from Redis, and re-enqueues them.

## Remote access and pairing

The remote API is HTTPS with TLS 1.3. An admin generates a one-time pairing code; the CLI exchanges it for a per-device credential that is revocable and rotated. The local Unix socket remains for same-host use. Private-interface binding keeps the same fail-closed rules.

## Secrets and master key

Server-side secrets live in an encrypted vault inside each tenant database, using envelope encryption: one data key per tenant, wrapped by a master key. The master key is unsealed at boot with no manual passphrase: through TPM/systemd-creds on Linux, Keychain on macOS, DPAPI/TPM on Windows, or a KMS on any platform. Client machines keep using the OS keyring. A missing or sealed master key makes the daemon fail closed.

Test master key unsealing on your own hosts, on each platform you use (TPM/systemd-creds, Keychain, DPAPI/TPM) and with a KMS, before relying on it.

## Availability controls

Readiness verifies database migration state, master key availability, Redis connectivity, workflow/node registry health and required core storage roles. Liveness checks daemon responsiveness. A partially unavailable connector degrades only the workflows that require it and does not crash unrelated jobs.

## Remote upload

`vidyom asset upload` transfers local files to the remote server using resumable chunks with checksum verification. Metadata records import origin and source permissions. YouTube-only imports must use owner-authorized copies acquired before ingestion. Do not make third-party video downloading an unverified automatic dependency.

## Backups and disaster recovery

SQLite databases run in WAL mode and are continuously backed up with Litestream to the archive storage provider. Test Litestream with GCS, and Redis AOF durability and reconciliation under crash tests, on your own deployment before relying on them.

Archive copies of originals and finalized clips are verified. Store canonical metadata and database backups independently. Disaster recovery is an explicit administrative operation; clipping never triggers archival retrieval. Test restoration in isolation without changing asset identity or lineage.

Retrieving an archived original is the separate administrative workflow `restore-archived`: it needs a special permission, is audited and shows the cost before confirmation. Maintenance workflows `reindex-asset`, `refresh-publication`, `verify-storage` and `bulk-create-clips` are also separate workflows.

## Monitoring

Basic operation remains available through `vidyom daemon health`, `vidyom job list`, `vidyom job logs`, and `vidyom doctor` even with no Grafana/Loki deployment.
