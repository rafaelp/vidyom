---
title: "Connector architecture"
sidebar_position: 1
---

# Connector architecture

**Decided:** No storage, publishing, catalog, AI, transcription or observability vendor may be hardcoded into core workflows. Stable domain interfaces expose operations; connector drivers implement vendor APIs; configuration maps logical names to providers.

## Distinct extension concepts

| Extension | Role |
| --- | --- |
| **Node** | Executable workflow action (`storage.put`, `video.cut`). |
| **Connector** | Provider-specific driver (`gcs`, `s3`, `youtube`, `loki`). |
| **Plugin** | Distributable package that can register nodes, connectors, hooks, or event handlers. |
| **Hook** | Synchronous interception point in a job/node lifecycle. |
| **Event** | Asynchronous notification of durable state changes. |

## Naming and credentials

Workflows reference logical aliases such as `archive`, `master`, `proxy`, `youtube-main`, and `loki-primary`. Provider config (outside workflow JSON) resolves aliases to driver, endpoint, bucket/folder and `credentialsRef`. Credentials are stored in OS keyrings and are resolved only by trusted authorized execution paths.

## Connector contracts

Every driver declares protocol version, supported operations, capabilities, accepted parameters, error taxonomy, and schemas. Capability checks must occur before execution, not only after a failed API call. Optional functionality may be emulated explicitly if safe, but the driver must not claim stronger semantics than its provider offers.

## Vendor portability

Changing a logical role's driver must not require editing workflow JSON. Actual media objects do not migrate automatically: provide a separate integrity-verified migration operation, preserve object lineage, and update canonical references only after successful copies.

## Initial and future drivers

| Domain | Initial | Future-compatible examples |
| --- | --- | --- |
| Storage | GCS Standard/Archive | AWS S3, Dropbox, SFTP, local/NAS, Azure Blob |
| Publishing | YouTube | Vimeo, PeerTube, Dailymotion |
| Catalog | MediaCMS | Other catalogs or custom adapters |
| Search/video indexing | VideoVortex and PostgreSQL | Other indexers |
| Transcription | faster-whisper | Remote audio transcription APIs |
| AI | Local or hosted model | Provider adapters |
| Logs | JSONL, Loki | Datadog, Cloud Logging, Syslog |
| Metrics | Prometheus/OpenTelemetry | Datadog |
| Traces | OTLP/OpenTelemetry | Datadog APM and other OTLP consumers |
