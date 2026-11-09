---
title: "Core concepts"
sidebar_position: 2
---

# Core concepts

| Concept | Definition |
| --- | --- |
| **Tenant** | An organization with its own database, storage prefix, secrets, users and roles, budget and worker cache. Users can belong to several tenants, with roles per tenant. |
| **Original** | Ingested source recording of any type: course, yoga class, talk, meeting, etc. |
| **Derived asset / clip** | A new independently identified asset rendered from one or more segments of another asset. |
| **Raw original** | Unmodified imported source, preserved in archival backup storage. |
| **Working master** | Permanently accessible, production-quality compressed file used to produce clips. |
| **Proxy** | Smaller, easily playable rendition used for preview and review. |
| **Transcript** | Time-aligned speech transcription, exported as JSON/SRT/VTT. |
| **Semantic index** | Searchable text and embeddings built from transcripts and metadata. |
| **Workflow** | Versioned JSON graph of nodes and connections. |
| **Node** | Typed, schema-validated execution step. |
| **Connector** | Provider-specific implementation of a stable integration interface. |
| **Plugin** | Packaged extension exposing nodes, connectors, hooks, or event handlers. |
| **Job** | Persisted daemon-managed execution, independent of any client connection. State lives in the tenant database; Redis only dispatches. |
| **Publication** | Per-destination representation of an asset on YouTube, Vimeo, etc. |
| **Storage role** | Logical storage purpose (`archive`, `master`, `proxy`) resolved to a configured connector. |

## Asset lineage

For example, an original yoga class `ORI-001` can produce `CLIP-001` and `CLIP-002` on different days. Each clip stores its parent ID, time ranges, render provenance, independent master, backup receipt, status, and publication records. Multiple clips can point to the same source; a clip may contain multiple ordered segments. Friendly IDs use `CLIP-` for clips (the immutable technical key is a ULID), alongside `ORI-`, `EDT-`, `JOB-`, `APR-`, `PUB-`, `CLIENT-` and `UPL-`.

## Definition of ready for clipping

An asset is `ready_for_clipping` only when its working master can be read directly, transcript timestamps are valid, the semantic index is available, and the relevant integrity checks have passed. The clipping workflow must **never silently fall back to archive**.
