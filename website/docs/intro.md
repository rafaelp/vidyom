---
title: "Welcome to Vidyom"
sidebar_position: 1
---

# Vidyom

**The open-source video editing studio run by AI agents.**  
**Preserve · Discover · Transform · Share.**

Vidyom is an open-source, vendor-independent video editing studio built for AI agents to operate. An agent takes a brief, finds the stories in your recordings, cuts clips, prepares them to publish and brings each decision to a person for approval. Vidyom does the heavy work behind every step: preserving, cataloging, searching, rendering and publishing audiovisual knowledge. It was originally designed for [Mapadeva](https://www.mapadeva.com.br/), and it is useful to anyone: yoga schools, course creators, educators, cultural institutions, companies, and personal archives.

## How an agent runs the studio

Agents connect through the [`vidyom` CLI](./reference/cli.md) or the authenticated [HTTPS API](./reference/api.md). Every action is a command or an API call with structured output, so an agent can search the library, start a job and check back later:

```bash
vidyom original search "Ujjayi breathing" --json
vidyom clip create --source ORI-001 --prompt "Explain Ujjayi breathing" --json
vidyom job status JOB-002 --json
```

The job then stops at `waiting_approval`, with no client needing to stay connected, until an editor reviews the proposed selection:

```bash
vidyom clip preview APR-001
vidyom approval approve APR-001
```

Publication has its own gate, which a publisher decides once the clip is rendered:

```bash
vidyom publish CLIP-001 --to youtube-main
vidyom clip approve CLIP-001
```

## What Vidyom solves

A recording may be 10 GB or larger and contain many distinct stories. Vidyom preserves its raw original in archival storage, keeps a production-quality working master in direct-access storage, transcribes and indexes it, and makes it available for generating clips **at any future time** without touching the archive. Each approved clip becomes its own traceable asset, with its own backup, master, metadata, catalog entry, and optional publication to multiple providers.

![Overview of the two Vidyom workflows: original ingestion builds a searchable library, and clip creation finds key moments, renders new videos and publishes them across channels.](/img/workflows-overview.webp)

*Overview of the two core workflows. The stages are described in [Workflow A — Ingest Original](./workflows/ingest-original.md) and [Workflow B — Create Semantic Clip](./workflows/create-clip.md).*

## Core principles

1. **Built for agents.** Every action is a command or API call with machine-readable output, and long jobs outlive the client session.
2. **People keep the final cut.** An editor approves every clip selection and a publisher approves what goes public.
3. **Originals first.** "Original" is a media asset category, not a synonym for lecture; content can be yoga lessons, courses, meetings, interviews, retreats, lectures, or other recordings.
4. **Two independent workflows.** Ingestion happens once; clip creation happens repeatedly and later.
5. **Never read archival backups for ordinary clipping.** Production uses the permanent working master.
6. **Configuration over code.** Compose workflows as versioned JSON graphs inspired by n8n.
7. **CLI and daemon.** The lightweight client controls persistent background work running on a server.
8. **Pluggable integrations.** Storage, publishing, catalog, transcription, AI, and observability are replaceable connectors.
9. **Extensible lifecycle.** Nodes, plugins, hooks, events, and subworkflows add behavior.
10. **Secure by default.** An encrypted per-tenant vault on the server and OS keyrings on clients, HTTPS with client pairing, private networking, least privilege, hash-chained audit logs.
11. **Portable knowledge.** Retain metadata and transcripts in open formats independent of SaaS systems.
12. **Private by default.** Only approved derived or edited material is published to the public library.
13. **Multi-tenant.** Each organization (tenant) has its own SQLite database, storage prefix, secrets, users, budget and worker cache.

## Read next

- [CLI reference](./reference/cli.md)
- [System architecture](./architecture/overview.md)
- [Ingest originals](./workflows/ingest-original.md)
- [Create semantic clips](./workflows/create-clip.md)
- [Quick start for contributors](./getting-started/development.md)
- [Source repository on GitHub](https://github.com/rafaelp/vidyom)
