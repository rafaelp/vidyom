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

Workflows reference logical aliases such as `archive`, `master`, `proxy`, `youtube-main`, and `loki-primary`. Provider config (outside workflow JSON) resolves aliases to driver, endpoint, bucket/folder and `credentialsRef`. **Decided:** On the server, credentials are stored in an encrypted vault inside each tenant database (envelope encryption with one data key per tenant); client machines keep using the OS keyring. Credentials are resolved only by trusted authorized execution paths and never appear in workflow JSON, logs or job payloads.

## Connector contracts

Every driver declares protocol version, supported operations, capabilities, accepted parameters, error taxonomy, and schemas. Capability checks must occur before execution, not only after a failed API call. Optional functionality may be emulated explicitly if safe, but the driver must not claim stronger semantics than its provider offers.

## Vendor portability

Changing a logical role's driver must not require editing workflow JSON. Actual media objects do not migrate automatically: provide a separate integrity-verified migration operation, preserve object lineage, and update canonical references only after successful copies.

## Initial and future drivers

| Domain | Initial | Future-compatible examples |
| --- | --- | --- |
| Storage | GCS Standard/Archive | AWS S3, Dropbox, SFTP, local/NAS, Azure Blob |
| Publishing | YouTube | Roadmap after YouTube: Vimeo, PeerTube, Instagram Reels. Spotify is not planned; other destinations (for example Dailymotion) may come as community connectors |
| Catalog | Vidyom's tenant database (canonical); MediaCMS as an optional connector | Other catalogs or custom adapters |
| Search/video indexing | SQLite FTS5 and sqlite-vec in the tenant database; VideoVortex as a proposed complement | Other indexers, PostgreSQL via the repository layer |
| Transcription | faster-whisper large-v3 (local) | Remote audio transcription APIs |
| Embeddings | BGE-M3 (local, multilingual) | Other embedding models |
| AI (LLM) | A single OpenAI-compatible API driver (OpenAI, OpenRouter, vLLM, Ollama, ...); each endpoint is classified `local` or `external` | Provider adapters |
| Logs | JSONL, Loki | Datadog, Cloud Logging, Syslog |
| Metrics | Prometheus/OpenTelemetry | Datadog |
| Traces | OTLP/OpenTelemetry | Datadog APM and other OTLP consumers |
