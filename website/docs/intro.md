---
title: "Welcome to Vidyom"
sidebar_position: 1
---

# Vidyom

**Open-source AI-powered video workflow engine.**  
**Preserve · Discover · Transform · Share.**

Vidyom is a proposed open-source, vendor-independent system for preserving, cataloging, searching, editing, and publishing audiovisual knowledge. It was originally designed for [MapaDeva](https://www.mapadeva.com.br/), but must remain useful to anyone: yoga schools, course creators, educators, cultural institutions, companies, and personal archives.

:::important Project maturity
This site is the **implementation specification**, not documentation for a released application. CLI commands, API endpoints, workflow node types, plugin contracts, and schemas describe the intended implementation unless explicitly stated otherwise.
:::

## What Vidyom solves

A recording may be 10 GB or larger and contain many distinct stories. Vidyom preserves its raw original in archival storage, keeps a production-quality working master in direct-access storage, transcribes and indexes it, and makes it available for generating clips **at any future time** without touching the archive. Each approved clip becomes its own traceable asset, with its own backup, master, metadata, catalog entry, and optional publication to multiple providers.

![Overview of the two Vidyom workflows: original ingestion builds a searchable library, and clip creation finds key moments, renders new videos and publishes them across channels.](/img/workflows-overview.webp)

*Illustrative overview. The specified stages are described in [Workflow A — Ingest Original](./workflows/ingest-original.md) and [Workflow B — Create Semantic Clip](./workflows/create-clip.md).*

## Core principles

1. **Originals first.** "Original" is a media asset category, not a synonym for lecture; content can be yoga lessons, courses, meetings, interviews, retreats, lectures, or other recordings.
2. **Two independent workflows.** Ingestion happens once; clip creation happens repeatedly and later.
3. **Never read archival backups for ordinary clipping.** Production uses the permanent working master.
4. **Configuration over code.** Compose workflows as versioned JSON graphs inspired by n8n.
5. **CLI and daemon.** The lightweight client controls persistent background work running on a server.
6. **Pluggable integrations.** Storage, publishing, catalog, transcription, AI, and observability are replaceable connectors.
7. **Extensible lifecycle.** Nodes, plugins, hooks, events, and subworkflows add behavior.
8. **Secure by default.** An encrypted per-tenant vault on the server and OS keyrings on clients, HTTPS with client pairing, private networking, least privilege, hash-chained audit logs.
9. **Portable knowledge.** Retain metadata and transcripts in open formats independent of SaaS systems.
10. **Private by default.** Only approved derived or edited material is published to the public library.
11. **Multi-tenant.** Each organization (tenant) has its own SQLite database, storage prefix, secrets, users, budget and worker cache.

## Read next

- [Quick start for contributors](./getting-started/development.md)
- [System architecture](./architecture/overview.md)
- [Ingest originals](./workflows/ingest-original.md)
- [Create semantic clips](./workflows/create-clip.md)
- [Source repository on GitHub](https://github.com/rafaelp/vidyom)
