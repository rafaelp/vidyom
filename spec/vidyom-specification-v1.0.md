# Vidyom — Complete Project Specification

> **Open-source AI-powered video workflow engine**  
> **Preserve · Discover · Transform · Share**  
> **Foundational architecture, product, and implementation document**  
> Document version: **1.0 — 2026-11-08**  
> Project origin: **MapaDeva — https://www.mapadeva.com.br/**  
> Status: **specification for implementation; does not represent features already delivered**.

---

## 0. How to read this document

This document consolidates **all decisions expressed throughout the Vidyom definition conversation** and the technical details proposed to turn them into an implementation. To avoid confusion:

- **[DECIDED]**: a requirement or choice explicitly established in the conversation.
- **[PROPOSED]**: a technical design recommended in the discussions; adjust it during detailed design, without contradicting the decided requirements.
- **[FUTURE]**: a planned extension that the architecture must make possible, but that does not need to be implemented in the MVP.
- **[VALIDATE]**: compatibility, limitation, cost, licensing, or behavior of an external tool that must be confirmed in a proof of concept before being assumed.

The command names, plugin contracts, and JSON formats in this document are **target interfaces of Vidyom**, not commands that already exist in MediaCMS, VideoVortex, FFmpeg, YouTube, or n8n. Example IDs, bucket names, URLs, paths, and times are fictional.

---

## 1. Vision and principles

### 1.1 Name, identity, and origin

- **[DECIDED] Project name: `Vidyom`.**
- **[DECIDED]** Vidyom is born to serve **MapaDeva**, but it will be **open source**, available and useful to any person or organization.
- **[DECIDED]** The system will not be specifically religious: it must also serve schools, content creators, universities, cultural institutions, companies, communities, and personal collections.
- **[PROPOSED]** International description: **Open-source AI-powered video workflow engine**.
- **[PROPOSED]** Tagline: **Preserve · Discover · Transform · Share**.
- **[DECIDED]** The executables will be named `vidyom` (CLI), `vidyomd` (daemon), and, per the proposed architecture, `vidyom-worker` (worker).
- **[PROPOSED]** The README will state “Originally built for MapaDeva”, without creating a functional or brand dependency.
- The name was inspired by **video**, **vidya** (knowledge), and **Om** (spiritual reference).

### 1.2 Problem to solve

MapaDeva will produce recordings of yoga classes, courses, meetings, talks, interviews, and other content. Each original recording can be approximately **10 GB**. The system needs to:

1. Preserve the raw recording with integrity and controlled storage cost.
2. Keep a good-quality working version **directly accessible** for ongoing production, with no recovery from backup.
3. Transcribe and index the content so it is searchable by words and by **semantics**.
4. Find stories, examples, teachings, or topics through **natural-language prompts**, even without a literal word match.
5. Create **many independent clips** from the same original, including months or years after ingestion.
6. Treat each clip as a **new asset**, with its own master, backup, metadata, indexing, and history.
7. Publish approved material to video platforms, initially YouTube, while maintaining a **public library** of selected content.
8. Automate the process through **JSON workflows**, reusing nodes and connectors, without programming to create each new workflow.
9. Operate remotely via a **long-running daemon + client CLI**, with security, auditing, and observability.
10. Not depend on a single provider: storage, publishing, AI, catalogs, and observability must be extensible through connectors and plugins.

### 1.3 Architecture principles

- **CLI-first, daemon-centric, API-driven.**
- **Declarative workflows** in JSON, with a node-and-connection architecture inspired by n8n, without requiring n8n to be installed.
- **Portability:** metadata and transcripts in open formats; export/import as a requirement.
- **Separation of source, processing, catalog, and publishing.**
- **The working master never depends on the archive backup for clips.**
- **Secure by default** and least privilege.
- **Replaceable connectors**, identified by logical roles.
- **Plugins and hooks** without changing the core for common extensions.
- **Durable execution**, with resumption, idempotency, traceability, and prevention of duplicate external effects.
- **One asset, multiple versions, multiple derivatives, and multiple publications.**
- **Private library distinct from the public one**, with explicit, reviewed, and controlled publishing.

### 1.4 Out of scope: assumptions to avoid

- Do not assume that MediaCMS, VideoVortex, or Rewind provide, out of the box, all the semantics specified here.
- Do not claim that GCS Standard storage is free: it has no specific retrieval fee like cold classes, but **storage, operations, and traffic/egress may cost money**.
- Do not confuse a reduction in audio file size with a proportional reduction in the **tokens** or the price of a transcription API.
- Do not assume that a YouTube API allows generic download of published files; the preferred strategy is to **keep the file uploaded to YouTube** in operational storage.
- Do not automatically make private content available in the public library.

---

## 2. Vocabulary and editorial model

### 2.1 Assets

- **Original (`original`) [DECIDED]:** an input recording, regardless of type (class, course, meeting, talk, etc.). It **definitively** replaces the old `lectures` folder/category.
- **Clip (`clip`) [DECIDED]:** material derived from one or more intervals of another asset; it receives its own identity, master, and backup.
- **Edited video (`edited`) [DECIDED]:** another editorial/derived version, traceable to the source.
- **Raw master (`source_original`):** the input file in its best preserved form (e.g., 10 GB), kept in backup.
- **Working master (`working_master`) [DECIDED]:** a good-quality file kept in directly accessible operational storage, permanently for as long as it is needed to create clips.
- **Proxy (`proxy`) [DECIDED]:** a lighter file used for playback, previews, navigation, and visual identification of segments.
- **Audio for ASR (`transcription_audio`):** a mono/compressed derivative suited to transcription; may have temporary retention.
- **Clip master (`clip_master`) [DECIDED]:** a new master, rendered from the source's working master, not from a degraded proxy or from the Archive.
- **Publication (`publication`) [DECIDED]:** an independent record of an asset at an external destination (YouTube, Vimeo, etc.).

### 2.2 Original categories

The category is **metadata**, not a fixed structure: `yoga_class`, `course`, `meeting`, `lecture`, `interview`, `event`, `other`, with support for new types through configuration.

### 2.3 Identification and relationships

- **[DECIDED]** Permanent identifier for each asset: examples `ORI-001`, `COR-001`.
- **[DECIDED]** Each clip stores its source asset(s), time intervals, versions, and history.
- **[DECIDED]** One original talk/class can generate N clips across multiple production moments, without re-running the original ingestion.
- **[DECIDED]** An asset can be published to multiple destinations, each with its own `remote_id`, URL, metadata, and state.
- **[PROPOSED]** Use UUID/ULID as the immutable technical key and friendly IDs as human aliases.

### 2.4 Lineage example

```text
ORI-001 — Yoga Class (indexed original)
├── COR-001 — Explanation of Ujjayi breathing
│   ├── YouTube publication
│   └── Vimeo publication
├── COR-002 — The story of the 90-year-old woman
└── COR-003 — The case of the friend who never received a hug
```

---

## 3. Two independent workflows and subworkflows

### 3.1 Workflow A: `ingest-original.json` [DECIDED]

**Goal:** receive an original, protect the raw file, generate working versions, extract knowledge, store and index it, leaving it eligible for future clips.

**Possible inputs:** a local file on the server, a CLI upload, a file already available in storage, or the user's own video published externally and imported from an authorized source. For remote uploads, the CLI must transfer the content in a resumable way, with a checksum.

**Functional steps:**

1. Receive the file and basic metadata: title, category, date, speaker/instructor, rights, language when known.
2. Register the asset with a permanent ID; validate the file, format, and SHA-256 checksum.
3. Persist the input copy during ingestion; never depend on a transient external link.
4. Send the **raw file** to the `archive` storage role and **verify** the backup.
5. Generate, with FFmpeg, a **compressed working master**, but one suited to future cuts.
6. Upload and verify the master in the `master` role (initially GCS Standard).
7. Generate a lightweight proxy in the `proxy` role (initially GCS Standard).
8. Extract audio optimized for transcription **directly from the best-quality file available**, avoiding double audio compression.
9. Optionally use VAD (voice activity detection) to exclude long silences, preserving a timestamp map between the processed audio and the video.
10. Transcribe with a configurable engine (initially faster-whisper), with timestamps; store the raw transcript and open formats.
11. Generate summaries, chapters, topics/tags, story identification, and embeddings with AI.
12. Index text and vectors in the canonical catalog (PostgreSQL + full-text search + pgvector as an initial proposal) and, where compatible, in VideoVortex.
13. Register/expose the asset in **MediaCMS**, the private visual catalog.
14. Validate the existence/integrity of the working master and the indexes.
15. Only then set the **`ready_for_clipping`** state.
16. Emit events (`original.indexed`, `asset.indexed`, `workflow.completed`) and record the audit trail.

**Mandatory readiness condition:** working master directly accessible; transcript with timestamps; valid search index; integrity checks. Without this, the asset is not a candidate for clips.

### 3.2 Workflow B: `create-clip.json` [DECIDED]

**Goal:** at any time, receive the **ID of an already indexed original + a prompt** and produce a new independent clip with a complete production and publishing cycle.

**Real input examples from the conversation:**

- “The story of the 90-year-old woman who had no remorse”.
- “The case of the friend who never received a hug”.
- “Explanation of Ujjayi breathing”.

**Functional steps:**

1. Receive `source_id`, `prompt`, and options (`complete_story`, short version up to a target duration, or best moments).
2. Resolve exclusively an **indexed asset** (`ready_for_clipping`).
3. Run lexical and semantic search **restricted to the requested asset**; do not require literal words.
4. Retrieve preceding and following context and estimate narrative boundaries (introduction, development, conclusion).
5. Generate candidates with timestamps and a confidence score; if ambiguous, request selection/review, do not cut at random.
6. Generate a preview from the proxy; allow adjusting the start/end.
7. Obtain approval of the proposed clip or proceed under an explicit editorial approval policy.
8. Read the working master **only from the `master` role**, using a temporary local cache on the worker; **do not access `archive`**.
9. Render a new clip master with FFmpeg at suitable quality and preserve a map of the intervals used (a single interval or an assembly of several).
10. Register a **new derived asset**, with ID, version, master, and lineage.
11. Call **`ingest-derived.json`** for the clip master backup, upload of the master and proxy to operational storage, and updating/indexing of text, summaries, thumbnail, and private catalog.
12. Run editorial production: title, description, subtitles, thumbnail, fact review, and permissions.
13. Request **mandatory approval by default** before public publication.
14. Call **`publish-video.json`** / `publish-youtube.json` (exact name may be parameterized) for the publishing destination(s).
15. Create/update the publication record and make the clip available **in the public library** after confirmation of the publication and its public state.
16. Allow new clips from the same source at any time, without redoing Workflow A.

**Invariant rule:** if `master` is unavailable, the execution signals an error/unavailability; it **never automatically restores from the Archive**. Restoration, if exceptionally needed, will be a separate, explicitly authorized administrative flow.

### 3.3 Reusable subworkflows [DECIDED]

- `ingest-derived.json`: handles a new clip or other derivative as an independent asset; backup, working master, proxy, metadata, indexing, integrity, catalog, and events.
- `publish-youtube.json` / `publish-video.json`: review and approval, upload, monitoring of remote processing, recording of URL/ID, copy of the published file, and public library update. Prefer the agnostic name `publish-video.json` with a connector parameter.
- **[PROPOSED]** `restore-archived.json` as a separate administrative operation, with special permission control, auditing, and cost handling.
- **[PROPOSED]** `reindex-asset.json`, `refresh-publication.json`, `verify-storage.json`, `bulk-create-clips.json`.

### 3.4 When a video is already on YouTube [DECIDED]

- If it was published by Vidyom, **retain in operational storage the same version that was uploaded**; never depend on YouTube as the master repository.
- If it exists only on YouTube, import **an authorized copy**, store/cache it persistently in the ingestion flow, and index it before accepting clips.
- Downloads/retrievals from YouTube depend on permissions, official methods, and file availability. The YouTube Data API will not be treated as a universal download API.
- The system will not fetch remote video on demand **during** clip generation.
- The worker's processing cache is temporary; a long-term operational copy is mandatory for `ready_for_clipping`.

---

## 4. Media, transcription, and computational cost

### 4.1 Media profiles [PROPOSED]

| Artifact | Suggested initial profile | Purpose |
|---|---|---|
| Raw | Unaltered, ~10 GB typical | Historical preservation in the Archive |
| Working master | H.264/H.265, preferably 1080p or the source's relevant resolution, high quality | Cuts, reframing, future rendering |
| Proxy | MP4 H.264 720p, lower quality | Player, previews, and navigation |
| Audio for ASR | Mono 16 kHz, Opus ~24–32 kbps, depending on the supported engine | Efficient transcription |
| Transcript | Structured JSON + SRT/VTT + Markdown/text | Search, editing, export |

Do not degrade the **only** production master to 720p when 1080p cuts, zoom, and vertical videos will be needed. Evaluate operational preservation of 4K depending on the source and the editorial goals.

### 4.2 FFmpeg: examples

Compatible proxy:

```bash
ffmpeg -i input.mp4 \
  -vf "scale=-2:720" \
  -c:v libx264 -preset slow -crf 28 \
  -c:a aac -b:a 96k \
  -movflags +faststart \
  proxy.mp4
```

Audio for transcription (generate directly from the best source):

```bash
ffmpeg -i input.mp4 \
  -vn -ac 1 -ar 16000 \
  -c:a libopus -b:a 24k \
  transcription.opus
```

The transcription engine may require MP3/WAV/FLAC or segmentation by API limits. The implementation must adjust formats according to the connector's actual capabilities.

### 4.3 Transcription and tokens [DECIDED: technical understanding]

- Compressing 10 GB to 20 MB does not imply a proportional reduction in transcription cost; cost may be per **minute**, per **audio token**, or local infrastructure, depending on the provider.
- Compression reduces storage, transfer, I/O, and the need to analyze visual frames.
- Reducing the actual duration sent (e.g., removing long silences via VAD) **can** reduce the cost of models billed by duration, provided intelligibility and timestamps are preserved.
- For an LLM that analyzes text, cost depends on the size of the transcript/excerpts sent, not on the size of the original video.
- Index **once** and query the text/embeddings afterward, avoiding repeated re-transcription.
- A clip's transcript can be derived from the source, but when there is assembly/editing/alteration of speech, timestamps must be reconciled and, if necessary, the altered intervals re-transcribed.

### 4.4 Semantic clips: requirements

- Robust semantic retrieval (approximate descriptions, paraphrases, incomplete names).
- Identification of the **complete story**, not just the first occurrence of a word.
- Segmentation with speech context, adjusting cuts to natural sentence boundaries.
- Preserve the time reference among original, proxy, transcript, clip, and publication.
- Support continuous cuts and assembly of multiple intervals with transparency in the lineage.
- Produce MP4, SRT/VTT, thumbnail, title/description suggestions, and an edit manifest.
- Review/approval before publication, by default.
- **[PROPOSED]** Evidence for review: text found, timestamps, and confidence score; an ambiguous clip requires human intervention.

---

## 5. Runtime architecture: CLI + daemon + workers

### 5.1 Components [DECIDED]

- **`vidyom`**: lightweight client CLI. Sends commands, uploads, and requests, follows logs/state, approves, queries the catalog, and publishes.
- **`vidyomd`**: permanent daemon, running in the background on a local or remote server. API, authentication, authorization, scheduler, workflow engine, job management, events, hooks, plugins, connectors, and state.
- **`vidyom-worker` [PROPOSED]**: runs heavy, isolatable work (FFmpeg, ASR, AI). It can start on the same host and later scale horizontally.
- **PostgreSQL + pgvector [PROPOSED]**: canonical catalog, execution, events, metadata, auditing, text/vector indexes.
- **Operational storage and archive**: abstracted via connectors and logical roles.

### 5.2 Architectural reference

**[DECIDED]** An experience similar to **Herdr**: processes persist beyond the client session; clients connect, disconnect, and reconnect. Vidyom does not need to preserve pseudo-terminals: it preserves **jobs, checkpoints, intermediate files, and node state**.

### 5.3 Communication

- CLI and daemon on the same host: **Unix Domain Socket** on Unix/macOS [PROPOSED], with restricted permissions.
- Remote: **SSH** (tunnel to the API/socket) and/or **HTTPS/TLS 1.3**, ideally mTLS in appropriate scenarios [DECIDED: encryption and security; PROPOSED: mechanisms].
- Versioned JSON API; event/log streaming via SSE, WebSocket, or gRPC streaming [PROPOSED, choose implementation].
- Multiple simultaneous clients can follow the same job.
- The CLI can run on macOS, Linux, and Windows; the daemon initially on Linux [PROPOSED].
- Processing and external secrets stay on the server; FFmpeg or Whisper are not needed in the CLI.

### 5.4 Remote upload

Clearly distinguish:

- `--input file=/data/class.mp4` = a file already accessible to the **server**.
- `vidyom asset upload ./class.mp4` = a file on the **client**; resumable (chunked/resumable) transmission, with checkpoint and checksum; only afterward start ingestion.

### 5.5 Jobs and persistence

Proposed states: `queued`, `running`, `waiting_approval`, `retrying`, `paused`, `completed`, `failed`, `cancelled`.

- A remote command returns a `job_id` immediately; the client can attach/detach.
- Ctrl+C while following logs **disconnects**, it does not cancel the job.
- Durable per-node checkpoints; retries with backoff and limits; upload/publish idempotency.
- A daemon restart triggers reconciliation of in-flight nodes, without duplicating external effects.
- While waiting for approval, no CLI process needs to stay connected.
- Workers declare CPU, GPU, memory, disk, and capacity; the scheduler chooses a compatible worker [PROPOSED].
- **[PROPOSED]** Configurable retention of execution records and safe cleanup of temporary files.

### 5.6 Languages [PROPOSED]

- **Go** for the CLI, daemon, API, and worker coordination (simple binaries, concurrency, distribution).
- FFmpeg/ffprobe as controlled external executables.
- **Python** for transcription/AI when advantageous, run in a separate worker/process.
- Plugins in an isolated process, HTTP/gRPC, WASM in specific cases; do not depend on native Go `.so` plugins.

---

## 6. n8n-inspired JSON workflow engine

### 6.1 Principles [DECIDED]

- Workflows are **JSON** files, containing nodes, types, versions, parameters, connections, inputs, outputs, and settings.
- Create a new flow **without writing code**, as long as the necessary nodes and plugins already exist.
- Conceptual reference: **n8n** workflow exports; **do not** require binary/exact compatibility or an n8n installation.
- Plugins can add node types and connectors.
- The daemon validates the JSON files against versioned schemas and executes a directed graph (initially a DAG, with controlled `foreach` and conditions).
- Composition and reusable **subworkflows** are core requirements.

### 6.2 JSON model [PROPOSED]

Recommended fields: `id`, `name`, `version`, `description`, `inputs`, `trigger`, `nodes`, `connections`, `hooks`, `settings`, `outputs`.

- `nodes[].id`: unique per workflow.
- `nodes[].type`: versioned type name (`video.cut`, `storage.put`, `ai.transcribe`).
- `nodes[].typeVersion`: plugin/node contract.
- `nodes[].parameters`: parameters validated by JSON Schema.
- `connections`: dependency/control edges; a node runs only when all required inputs are ready.
- `workflow.execute`: invokes a subworkflow with input and output isolation.
- `flow.if`, `flow.foreach`, `flow.wait`: explicit control.
- `${inputs.foo}`, `${nodes.id.output.field}`, `${event.field}`: **restricted declarative** expression, with no arbitrary JavaScript or shell evaluation.
- Workflows can have a `trigger` via CLI, event, webhook, or schedule, once those types are implemented.
- `--dry-run`: validates syntax, schemas, connectors, permissions, resources, and order, without executing external effects.

### 6.3 Illustrative example: ingestion (proposed format)

```json
{
  "id": "ingest-original",
  "name": "Ingest Original",
  "version": "1.0.0",
  "inputs": {
    "file": { "type": "file", "required": true },
    "title": { "type": "string", "required": true },
    "category": { "type": "string", "default": "other" }
  },
  "nodes": [
    {
      "id": "register",
      "type": "catalog.register",
      "typeVersion": 1,
      "parameters": {
        "kind": "original",
        "title": "${inputs.title}",
        "category": "${inputs.category}"
      }
    },
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
    },
    {
      "id": "compress",
      "type": "video.transcode",
      "typeVersion": 1,
      "parameters": {
        "source": "${inputs.file}",
        "profile": "master-1080p"
      }
    },
    {
      "id": "store_master",
      "type": "storage.put",
      "typeVersion": 1,
      "parameters": {
        "storage": "master",
        "source": "${nodes.compress.output.file}",
        "key": "originals/${nodes.register.output.id}/master.mp4",
        "verifyChecksum": true
      }
    },
    {
      "id": "transcribe",
      "type": "ai.transcribe",
      "typeVersion": 1,
      "parameters": {
        "source": "${inputs.file}",
        "language": "pt"
      }
    },
    {
      "id": "index",
      "type": "ai.index",
      "typeVersion": 1,
      "parameters": { "transcript": "${nodes.transcribe.output}" }
    },
    {
      "id": "catalog",
      "type": "mediacms.register",
      "typeVersion": 1,
      "parameters": {
        "assetId": "${nodes.register.output.id}",
        "master": "${nodes.store_master.output}",
        "index": "${nodes.index.output}"
      }
    }
  ],
  "connections": {
    "register": ["backup", "compress", "transcribe"],
    "compress": ["store_master"],
    "transcribe": ["index"],
    "backup": ["catalog"],
    "store_master": ["catalog"],
    "index": ["catalog"]
  },
  "settings": { "retry": 3, "resumeOnFailure": true, "saveExecutionProgress": true }
}
```

**Note:** this is a minimal example to explain the format; the complete implementation must include proxy, summaries, embedding generation, validations, `ready_for_clipping`, events, states, and checksums. Registering in MediaCMS may require integration specific to the installed version.

### 6.4 Illustrative example: clip

```json
{
  "id": "create-clip",
  "name": "Create Semantic Clip",
  "version": "1.0.0",
  "inputs": {
    "source_id": { "type": "string", "required": true },
    "prompt": { "type": "string", "required": true }
  },
  "nodes": [
    {
      "id": "source",
      "type": "catalog.get",
      "typeVersion": 1,
      "parameters": {
        "id": "${inputs.source_id}",
        "requiredStatus": "ready_for_clipping"
      }
    },
    {
      "id": "find_story",
      "type": "ai.find_segment",
      "typeVersion": 1,
      "parameters": {
        "asset": "${nodes.source.output}",
        "query": "${inputs.prompt}",
        "mode": "complete_story"
      }
    },
    {
      "id": "render",
      "type": "video.cut",
      "typeVersion": 1,
      "parameters": {
        "source": "${nodes.source.output.working_master_uri}",
        "segments": "${nodes.find_story.output.segments}"
      }
    },
    {
      "id": "ingest_derived",
      "type": "workflow.execute",
      "typeVersion": 1,
      "parameters": {
        "workflow": "ingest-derived",
        "file": "${nodes.render.output.file}",
        "parentId": "${inputs.source_id}"
      }
    },
    {
      "id": "publish",
      "type": "workflow.execute",
      "typeVersion": 1,
      "parameters": {
        "workflow": "publish-video",
        "assetId": "${nodes.ingest_derived.output.asset_id}",
        "requireApproval": true
      }
    }
  ],
  "connections": {
    "source": ["find_story"],
    "find_story": ["render"],
    "render": ["ingest_derived"],
    "ingest_derived": ["publish"]
  }
}
```

**`video.cut` node invariant:** the URL received must be validated as belonging to the logical `master` role (or a local cache originating from it); a silent fallback to `archive` is prohibited.

### 6.5 Versioning and safe execution

- Save an **immutable snapshot** of the JSON version with each job.
- New versions of the flow do not alter jobs in progress.
- Validate node names, dependencies, cycles, schemas, permissions, and connector capabilities.
- No strings that become shell commands through concatenation; do not expose arbitrary code evaluation by default.
- Define an explicit way to wait for approval and to time out.
- Export/import workflows and schemas without including credentials.

---

## 7. Connector and plugin architecture

### 7.1 General rule [DECIDED]

**The core defines contracts; connectors integrate external services; nodes execute operations; workflows organize nodes; plugins add nodes, connectors, hooks, and event subscribers.**

- The project must not couple the core to GCS, YouTube, Loki, MediaCMS, or any specific AI provider.
- Connector configurations live outside the workflows: logical names, parameters, capabilities, `credentialsRef`.
- Connectors must declare version, schema, capabilities, limits, and any relevant cost/semantics.
- Changing a provider, when equivalent, should not require editing every workflow JSON.
- **Migrating existing files remains a separate operation**; changing the configuration does not migrate the data automatically.

### 7.2 Storage connectors [DECIDED]

First connector: **Google Cloud Storage**. Future extensions: **AWS S3, Dropbox, SFTP, Azure Blob, local/NAS**, and others.

Logical roles:

- `archive`: preservation/backup, not used to produce clips.
- `master`: working master, direct access, permanent throughout the clip creation cycle.
- `proxy`: playback, preview, and browsing.

**Initial mapping:**

| Role | Initial connector | Class | Content |
|---|---|---|---|
| `archive` | GCS | ARCHIVE | Raw originals and final clip masters |
| `master` | GCS | STANDARD | Working masters of originals and derivatives |
| `proxy` | GCS | STANDARD | Proxies and, depending on configuration, published media |

**GCS Standard is not cost-free access:** storage, request, and egress costs may apply. Avoid unnecessary egress; when possible, run the worker close to the bucket's region. Do not use Archive for operational reads.

**Standard operations:** `put`, `get`, `stat`, `list`, `exists`, `delete`, `copy`, `move`, `checksum/verify`, `downloadRange`, `capabilities`. Operations that are not available must be declared as such or implemented through an explicit fallback subject to cost/permission.

**Optional capabilities:** versioning, range reads, server-side copy, resumable/multipart upload, checksum verification, storage classes, and restore semantics. The direct access policy must be declared on the storage instance, not inferred from the driver name alone.

**Configuration example:**

```json
{
  "storages": {
    "archive": {
      "driver": "gcs",
      "bucket": "videos-archive",
      "storageClass": "ARCHIVE",
      "credentialsRef": "gcs-main",
      "usage": "backup_only"
    },
    "master": {
      "driver": "gcs",
      "bucket": "videos-standard",
      "storageClass": "STANDARD",
      "credentialsRef": "gcs-main",
      "usage": "operational"
    },
    "proxy": {
      "driver": "gcs",
      "bucket": "videos-standard",
      "prefix": "proxies/",
      "credentialsRef": "gcs-main",
      "usage": "operational"
    }
  }
}
```

**Neutral node example:**

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

### 7.3 Publishing connectors [DECIDED]

**YouTube** first; future: **Vimeo, PeerTube, Dailymotion**, and others. An asset can have **multiple publications**, without duplicating its identity.

Capabilities contract: `upload`, `get`, `update`, `delete`, `setVisibility`, `uploadThumbnail`, `uploadCaptions`, `getStatus`, `getUrl`, `capabilities`.

- States and metadata support vary by platform; require the connector to declare them.
- Uploads must be resumable when the destination supports it.
- A publication will not be publicly visible without **approval** by default.
- Record `destination`, `remote_id`, `url`, version, state, timestamps, title/description, and visibility.
- **Reuse the uploaded master/version** from the `master` storage or operational storage to republish to another destination without restoring from backup.
- If the destination is YouTube, keep the file used for the upload in operational storage; do not rely on re-downloading it later.

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

### 7.4 Observability connectors [DECIDED]

Observability is pluggable, with the possibility of **multiple destinations active simultaneously**.

Distinct interfaces:

- `LogSink`: local JSONL, **Loki**, **Datadog**, Syslog, Google Cloud Logging, etc.
- `MetricsExporter`: Prometheus, Datadog, OTLP.
- `TraceExporter`: OTLP, Datadog APM, others.

**OpenTelemetry** is the recommended instrumentation standard; exporters are decoupled. A Loki failure must not affect Datadog or block processing.

**[PROPOSED]** Logs configuration:

```json
{
  "observability": {
    "logs": {
      "level": "info",
      "sinks": [
        {
          "id": "local",
          "driver": "file",
          "enabled": true,
          "parameters": {
            "path": "/var/log/vidyomd/events.jsonl",
            "rotation": "daily",
            "retentionDays": 30
          }
        },
        {
          "id": "loki",
          "driver": "loki",
          "enabled": true,
          "credentialsRef": "loki-auth",
          "parameters": { "url": "https://loki.example.com" }
        },
        {
          "id": "datadog",
          "driver": "datadog",
          "enabled": false,
          "credentialsRef": "datadog-api-key",
          "parameters": { "site": "datadoghq.com" }
        }
      ]
    }
  }
}
```

### 7.5 Other connectors [DECIDED as architecture; gradual implementation]

- `CatalogConnector`: initially **MediaCMS**.
- `TranscriptionConnector`: initially local **faster-whisper**; APIs and other engines in the future.
- `AIProvider`: local inference or remote API; providers are configurable and interchangeable where their capabilities are equivalent.
- **VideoVortex** integration for CLI/transcript indexing, search, and clip operations; confirm compatibility and the exact features of the installed edition.
- **Rewind** is **optional** for visual exploration/editing, not an MVP dependency.
- **Arcadeia** was considered and **not chosen** as a component of the priority architecture.
- Next.js is the proposed technology for the **public library**, consuming APIs/data published by Vidyom.

### 7.6 Plugin manifest [PROPOSED]

```json
{
  "id": "vidyom.connector.storage.gcs",
  "version": "1.0.0",
  "runtime": "process",
  "provides": {
    "storageDrivers": ["gcs"]
  },
  "permissions": {
    "network": ["storage.googleapis.com"],
    "secrets": ["gcs-main"],
    "filesystem": ["job-temp"]
  },
  "contracts": {
    "storage": "1.x"
  }
}
```

Plugins can run in **isolated processes**, communicating through a versioned protocol (gRPC/JSON-RPC/HTTP), with WASM for some cases. Do not automatically grant filesystem, network, keyring, credentials, or arbitrary command execution.

---

## 8. Lifecycle hooks and Event Bus

### 8.1 Hooks [DECIDED]

Workflow and node hooks, configurable via JSON, with no need to modify the engine:

| Hook/event | Proposed semantics |
|---|---|
| `workflow.before` | Before starting: can validate/block |
| `workflow.start` | Effective start, notifications |
| `workflow.after` | Workflow success, before `finish` |
| `workflow.finish` | Terminal completion, success/error/cancellation |
| `workflow.error` | Error, controlled handling |
| `workflow.cancel` | Cancellation request/execution |
| `node.before` | Before the node, allows validation/authorized modification |
| `node.after` | Node completed successfully |
| `node.error` | Node failure |
| `node.finish` | End of the node attempt, including error |

**Levels:** global (`config/hooks.json`), workflow (`hooks` in the JSON), and node (`hooks` on the node). Proposed order: global → workflow → node on `before`, reverse order on `after`/`finish`.

**Rules:**

- Global security/audit hooks cannot be removed by workflows.
- `finish` runs for any valid termination, even after a daemon resume, within the persistence guarantees; it does not promise execution in the event of irreversible system loss.
- Failure policy per hook: `abort`, `retry`, `continue`, where applicable.
- Mandatory timeout, idempotency, explicit permissions, logs, and traces.
- A hook that modifies parameters must go through validation and authorization again.
- Notification hooks must not block the workflow indefinitely.

**Example:**

```json
{
  "hooks": {
    "before": [
      {
        "id": "validate-source",
        "plugin": "asset-validator",
        "action": "check-ready-for-clipping",
        "timeoutSeconds": 15,
        "onFailure": "abort"
      }
    ],
    "after": [
      {
        "id": "notify-review",
        "plugin": "notifications",
        "action": "send-review-notification",
        "onFailure": "continue"
      }
    ],
    "finish": [
      {
        "id": "record-metrics",
        "plugin": "analytics",
        "action": "record-execution",
        "onFailure": "continue"
      }
    ]
  }
}
```

### 8.2 Events [DECIDED]

**Asynchronous, persistent** Event Bus, with subscriptions from plugins or workflows:

`original.indexed`, `asset.created`, `asset.indexed`, `clip.created`, `storage.backup.verified`, `publication.completed`, `publication.created`, `workflow.completed`, `workflow.failed`, among others.

- Events can trigger other workflows (without changing the original workflow).
- Persist via **transactional outbox** [PROPOSED], with retries and event delivery tracking.
- **At-least-once** delivery, with idempotency keys; do not promise exactly-once to external destinations.
- Prevent infinite recursion, event loops, indiscriminate fan-out, and missing limits.
- Access control: plugins and subscriptions can only consume permitted events and properly filtered data.

**Example:**

```json
{
  "id": "update-public-library",
  "trigger": {
    "type": "event",
    "event": "publication.completed",
    "filter": { "visibility": "public" }
  },
  "nodes": [
    {
      "id": "sync",
      "type": "catalog.public.sync",
      "typeVersion": 1,
      "parameters": { "assetId": "${event.asset_id}" }
    }
  ]
}
```

---

## 9. Security and credential management

### 9.1 Secrets and keyring [DECIDED]

- Credentials **must be stored in the operating system keyring**, and may reside on the **client or the server**.
- macOS: Keychain; Windows: Credential Manager; Linux desktop: Secret Service/KWallet/GNOME Keyring; Linux headless: explicitly provisioned secure Secret Service.
- **Do not** silently fall back to a plaintext file on a server without a keyring; if the keyring is not configured, fail with a clear error or require secure provisioning.
- Workflow and configuration JSONs contain only `credentialsRef` references, never tokens, secrets, or passwords.
- `client`: secret stored in the user's keyring; may not be available to a disconnected remote daemon.
- `server`: secret stored in the daemon's keyring, preferred for long autonomous jobs.
- `delegated`: temporary/minimum-scope credential, sent to the server over an authenticated and encrypted channel, with consent/authorization; limited duration and storage.
- Secrets do not appear in CLI arguments, logs, traces, events, snapshots, manifests, exported files, or error messages.
- Rotation, revocation, access testing, and usage auditing are requirements.

### 9.2 Communication protection [DECIDED]

- Client-server communication is **encrypted and authenticated**.
- Remote connection: SSH or TLS 1.3/HTTPS with certificate validation; mTLS when appropriate.
- Local Unix Socket protected by filesystem permissions and process identity (it does not encrypt the traffic by itself).
- Authentication and **authorization per operation/asset**, regardless of VPN.
- **[PROPOSED]** Roles: `viewer`, `editor`, `publisher`, `admin`, plus granular permissions (`storage.archive.restore`, `asset.delete`, `plugin.install`, `secret.write`).

### 9.3 Private networks [DECIDED]

- Ability to run **exclusively on a Tailscale, NetBird, or equivalent interface**, with no exposure to the public internet.
- The term “tailwind” mentioned during the definition conversation was interpreted as **Tailscale** in the context of overlay networking.
- Modes: explicit bind to a VPN private IP; or `127.0.0.1` with Tailscale Serve/tunnel/authenticated proxy.
- **Do not** fall back to `0.0.0.0` when the private interface does not exist; fail closed.
- Private network firewall and access policies complement TLS, authentication, and RBAC; they do not replace them.

### 9.4 Plugin and workflow execution [DECIDED]

- Plugins with declared permissions for secrets, networks, and files.
- Verify origin, hashes, signatures where possible, and version compatibility.
- Imported workflow JSONs do not automatically inherit the permissions of all connectors.
- No arbitrary shell by default; pass FFmpeg arguments in a safe/validated way, without concatenating a command.
- Human approval before publishing or deleting, by default for publishing; destructive actions require distinct permission.
- Minimize exposure of private transcripts and sensitive content in AI and indexing logs.

---

## 10. Logs, metrics, traces, auditing, and alerts

### 10.1 Model [DECIDED]

- Structured JSON logs with `timestamp`, `level`, `service`, `event`, `job_id`, `workflow_id`, `node_id`, `asset_id`, `trace_id`, duration, and status.
- Observability via `Observability Manager`, with filters, secret redaction, buffering, batching, fan-out, and pluggable exporters.
- Distributed tracing across CLI, API, daemon, workflow, worker, and connector.
- Auditing separate from operational logs, durable and resistant to unauthorized changes.
- Local logs accessible through the CLI **even without Loki/Datadog/Grafana**.
- Unavailable destinations do not take down FFmpeg or ASR; implement isolated queues and retries. Critical audit events must not be silently discarded.

### 10.2 Initial stack [PROPOSED]

- Instrumentation: **OpenTelemetry**.
- Logs: **JSONL + Loki** initially; Datadog connector planned.
- Metrics: **Prometheus** initially.
- Traces: **OTLP / Tempo** initially.
- Dashboard and alerts: **Grafana** optional, separate from the daemon.
- Future connectors: Google Cloud Logging, Syslog, and others.

### 10.3 Minimum metrics

- Jobs `queued`, `running`, `waiting_approval`, `failed`, and `completed`.
- Execution time per node, workflow, plugin, and connector.
- Retries, failures, and connector latency.
- Transfer and storage per logical role/provider.
- CPU, GPU, RAM, disk space, and worker queue.
- Estimated AI time/cost, when measurable.
- Originals/derivatives without verified backup.
- Clips awaiting editorial review.
- Authentication/authorization failures and security anomalies.
- Failures, delay, drops, and backlog of telemetry exporters.

### 10.4 Retention [PROPOSED]

| Type | Suggested initial value |
|---|---|
| Operational logs | 30 days |
| Job/execution history | 90 days or administrative policy |
| Auditing | >= 1 year, depending on applicable requirements |
| Metrics and traces | configurable per environment and cost |

---

## 11. Specified CLI (target interface)

All commands below are **proposed for the Vidyom to be implemented**, not current features of third parties.

### 11.1 Daemon, servers, and connection

```bash
vidyomd start --detach
vidyom daemon status
vidyom daemon health
vidyom server add production --ssh user@server.example.com
vidyom server use production
vidyom server list
```

### 11.2 Originals and clips

```bash
vidyom original import yoga-class.mp4 \
  --type yoga_class \
  --title "Hatha Yoga Class" \
  --workflow ingest-original

vidyom asset upload ./yoga-class.mp4 \
  --server production \
  --workflow ingest-original

vidyom original list
vidyom original show ORI-001
vidyom original search "mindful breathing"

vidyom clip create \
  --source ORI-001 \
  --prompt "Story of the 90-year-old woman who had no regrets" \
  --workflow create-clip

vidyom clip preview COR-001
vidyom clip approve COR-001
vidyom clip list --source ORI-001
vidyom asset lineage COR-001
```

### 11.3 Workflows, nodes, jobs

```bash
vidyom workflow list
vidyom workflow validate ./workflows/create-clip.json
vidyom workflow run ./workflows/create-clip.json \
  --input source_id=ORI-001 \
  --input prompt="The case of the friend who never got a hug"
vidyom workflow run ingest-original --dry-run
vidyom workflow resume JOB-001

vidyom node list
vidyom node describe video.cut
vidyom node run ai.transcribe --asset ORI-001

vidyom job list --status running
vidyom job status JOB-001
vidyom job attach JOB-001
vidyom job logs JOB-001 --follow
vidyom job retry JOB-001
vidyom job cancel JOB-001
```

### 11.4 Connectors, storage, and publications

```bash
vidyom connector list
vidyom storage list
vidyom storage test master
vidyom storage verify archive --asset ORI-001
vidyom storage copy --from master --to another-storage --asset ORI-001

vidyom publishing list
vidyom publish COR-001 --to youtube-main
vidyom publish COR-001 --to youtube-main,vimeo-main
vidyom publish status COR-001
vidyom publish sync COR-001
```

### 11.5 Credentials, security, observability, plugins, hooks

```bash
vidyom secret add gcs-main --location server
vidyom secret add youtube-oauth --location client
vidyom secret list
vidyom secret test youtube-oauth
vidyom secret rotate youtube-oauth
vidyom secret delete youtube-oauth

vidyom security status
vidyom security audit --since 24h
vidyom security sessions
vidyom security revoke CLIENT-001

vidyom logs --follow
vidyom job logs JOB-001 --follow
vidyom observability list
vidyom observability test loki
vidyom observability enable datadog
vidyom observability disable loki
vidyom observability status
vidyom doctor --security
vidyom doctor --network
vidyom doctor --storage

vidyom plugin list
vidyom plugin inspect notifications
vidyom plugin install ./notifications-plugin
vidyom hook list --workflow create-clip
vidyom hook test validate-source --asset ORI-001
vidyom event list --since 24h
vidyom event watch --type clip.created
vidyom event deliveries --status failed
```

The final UX must define uniform names (`connector`, `publishing`, `publish`) and an output format supporting JSON for scripts (`--json`). Logs/attach must not change the job state unless an explicit command is used.

---

## 12. API and canonical data models

### 12.1 Daemon API [PROPOSED]

Initial version `/api/v1` or versioned gRPC, covering:

- `POST /workflows/{id}/runs`: run a workflow.
- `GET /jobs/{id}`: state, progress, result.
- `GET /jobs/{id}/events`: SSE streaming or equivalent.
- `POST /jobs/{id}/cancel`, `POST /jobs/{id}/retry`.
- `POST /assets/uploads`: start a resumable upload.
- `GET /assets/{id}`, `GET /assets/{id}/lineage`.
- `POST /clips`: request a semantic clip.
- `POST /approvals/{id}/approve` / `reject`.
- `POST /publications`: request a publication.
- `GET /nodes`, `GET /connectors`, `GET /plugins`.
- `GET /health`, `GET /metrics` (controlled access).

Authentication, authorization, idempotency keys, and an audit trail are mandatory for mutations.

### 12.2 Example of an original asset

```json
{
  "id": "ORI-001",
  "kind": "original",
  "category": "yoga_class",
  "title": "Hatha Yoga Class",
  "visibility": "private",
  "status": "ready_for_clipping",
  "duration_seconds": 3600,
  "language": "pt",
  "source_original": {
    "storage": "archive",
    "key": "originals/ORI-001/original.mp4",
    "sha256": "<real-hash-at-runtime>",
    "verified": true
  },
  "working_master": {
    "storage": "master",
    "key": "originals/ORI-001/master.mp4",
    "verified": true
  },
  "proxy": {
    "storage": "proxy",
    "key": "originals/ORI-001/proxy.mp4"
  },
  "transcript": {
    "format": "json+srt",
    "indexed": true,
    "timestamp_basis": "source_original"
  }
}
```

The string `<real-hash-at-runtime>` is a didactic placeholder, not a valid SHA-256.

### 12.3 Example of a clip

```json
{
  "id": "COR-001",
  "kind": "clip",
  "title": "The 90-year-old woman who had no remorse",
  "parent_asset_id": "ORI-001",
  "lineage": {
    "source_asset_id": "ORI-001",
    "segments": [
      { "start_seconds": 648, "end_seconds": 1080 }
    ]
  },
  "visibility": "private",
  "status": "editorial_review",
  "master": {
    "storage": "master",
    "key": "clips/COR-001/master.mp4"
  },
  "backup": {
    "storage": "archive",
    "key": "clips/COR-001/original.mp4",
    "verified": true
  },
  "publications": []
}
```

The timestamps above are examples; in real execution they will be calculated by indexing.

### 12.4 Example of independent publications

```json
{
  "asset_id": "COR-001",
  "publications": [
    {
      "destination": "youtube-main",
      "remote_id": "illustrative-youtube-id",
      "url": null,
      "status": "published",
      "visibility": "public"
    },
    {
      "destination": "vimeo-main",
      "remote_id": "illustrative-vimeo-id",
      "url": null,
      "status": "published",
      "visibility": "public"
    }
  ]
}
```

Fill in URLs only with confirmed API responses. Do not invent publication links.

### 12.5 Collection portability [DECIDED]

**[PROPOSED]** Each asset can be exported as a metadata package (without having to export the masters on every query):

```text
ORI-001/
├── metadata.json
├── summary.md
├── transcript.json
├── transcript.srt
├── chapters.json
├── thumbnail.jpg
├── preview.mp4            # optional
└── publications.json
```

- Export/import indexes and documents in a human-readable form, without depending on MediaCMS's proprietary database.
- The canonical catalog is independent of any external UI.
- Guarantee UTF-8, explicit timestamps, hashes, and source metadata.

---

## 13. Private catalog and public library

### 13.1 Private [DECIDED]

- **MediaCMS**: private visual interface, proxy videos, tags/categories/collections, browsing, and API integration when compatible.
- **VideoVortex (`vvx`)**: indexing tool, text search, and segment/clip operations, CLI and AI integration, according to the validated capabilities of the deployed version.
- **PostgreSQL + pgvector [PROPOSED]**: canonical source and semantic search (embeddings) + full-text search.
- **Rewind [OPTIONAL]**: visual editing/review of transcripts and clips.
- **Arcadeia**: evaluated, not a component recommended for this architecture.
- Verify how MediaCMS receives or references an already processed proxy; avoid unnecessarily duplicating transcoding and storage.
- MediaCMS and VideoVortex may keep their own indexes; Vidyom must synchronize by stable IDs and prevent either of these products from becoming the sole source of truth.

### 13.2 Public [DECIDED]

- Separate public library (proposed technology: **Next.js**).
- Includes only **published and approved** assets, mainly edited videos and clips.
- Embedded player or link to YouTube, Vimeo, or other destinations.
- Search by topic, instructor, chapter, story, and spoken content.
- Timestamps and video position links when supported by the platform.
- May display relationships between clips and the source recording without exposing the private original.
- Full transcripts or excerpts only if reviewed and explicitly authorized for publication.
- No automatic public synchronization for assets without an approved/published state.

---

## 14. File and repository organization

### 14.1 Logical storage layout

```text
gs://videos-archive/
  originals/
    ORI-001/original.mp4
  clips/
    COR-001/original.mp4

gs://videos-standard/
  originals/
    ORI-001/master.mp4
    ORI-001/proxy.mp4
  clips/
    COR-001/master.mp4
    COR-001/proxy.mp4
    COR-001/published.mp4
```

**These paths are initial conventions of the GCS connector**. Workflows must reference the logical roles `archive`, `master`, `proxy`, not buckets directly.

### 14.2 Code structure [PROPOSED]

```text
vidyom/
├── cmd/
│   ├── vidyom/                  # CLI
│   ├── vidyomd/                 # Daemon
│   └── vidyom-worker/          # Worker
├── internal/
│   ├── api/
│   ├── auth/
│   ├── daemon/
│   ├── workflow/
│   │   ├── schema/
│   │   ├── validation/
│   │   └── executor/
│   ├── jobs/
│   ├── scheduler/
│   ├── workers/
│   ├── persistence/
│   ├── catalog/
│   ├── hooks/
│   ├── events/
│   │   ├── outbox/
│   │   └── subscriptions/
│   ├── plugins/
│   ├── security/
│   │   ├── keyring/
│   │   ├── authentication/
│   │   ├── authorization/
│   │   ├── transport/
│   │   └── audit/
│   └── observability/
│       ├── logging/
│       ├── metrics/
│       ├── tracing/
│       └── health/
├── nodes/
│   ├── storage/
│   ├── video/
│   ├── audio/
│   ├── ai/
│   ├── catalog/
│   ├── control/
│   └── integrations/
├── connectors/
│   ├── storage/
│   │   ├── gcs/
│   │   ├── s3/                  # future
│   │   ├── dropbox/             # future
│   │   ├── sftp/                # future
│   │   └── local/               # future
│   ├── publishing/
│   │   ├── youtube/
│   │   ├── vimeo/               # future
│   │   ├── peertube/            # future
│   │   └── dailymotion/         # future
│   ├── observability/
│   │   ├── file/
│   │   ├── loki/
│   │   ├── datadog/             # future
│   │   ├── prometheus/
│   │   └── otlp/
│   ├── catalog/mediacms/
│   ├── transcription/faster-whisper/
│   └── ai/
├── plugins/
├── workflows/
│   ├── ingest-original.json
│   ├── ingest-derived.json
│   ├── create-clip.json
│   └── publish-video.json
├── profiles/
│   ├── master-1080p.json
│   └── proxy-720p.json
├── schemas/
├── config/
│   ├── storages.json
│   ├── publishing.json
│   ├── observability.json
│   ├── hooks.json
│   └── servers.json
├── docs/
└── tests/
```

---

## 15. Tests and acceptance criteria

### 15.1 E2E scenario: original

**Given** a 10 GB original and a title/category, **when** it is sent by the remote CLI to the daemon, **then**:

1. the upload is resumable and verified by hash;
2. the backup of the raw file in `archive` is validated;
3. the working master and proxy are generated and stored in `master`/`proxy`;
4. the transcript and chapters are searchable with timestamps;
5. media and metadata appear in MediaCMS or a compatible interface;
6. the job is observable from the CLI even after disconnection/reconnection;
7. the asset only becomes `ready_for_clipping` when the mandatory conditions are met.

### 15.2 E2E scenario: two clips on different days

**Given** `ORI-001` already indexed, **when** the prompts “The story of the 90-year-old woman who had no remorse” and, days later, “The case of the friend who never got a hug” are submitted, **then**:

1. the index of `ORI-001` locates relevant candidates without exact words;
2. the AI proposes the start and end of each narrative, with review;
3. a worker downloads only the **working master**, never the raw file from the Archive;
4. two new assets `COR-001` and `COR-002` are created, with correct lineage;
5. each clip receives a backup, working master, proxy, derived/reviewed transcript, and index;
6. each clip can be published independently and appear in the public library after approval;
7. no step of Workflow A for `ORI-001` needs to run again.

### 15.3 Scenario: YouTube as source

- An existing own video that exists only on YouTube is imported through an authorized channel and stored in operational storage.
- Transcription and clips are made from the persistent local/operational copy.
- After ingestion, creating a clip does not query YouTube or the Archive to obtain the video.

### 15.4 Scenario: pluggable storage

- Switch the logical `master` configuration from GCS to a test S3 connector **without changing the workflow**.
- Before switching in production, migrate the data and validate checksums/versions.
- A driver without direct read or range support declares the limitation; the workflow fails in an explainable way when it requires an incompatible capability.

### 15.5 Scenario: resumption and idempotency

- Kill/restart the daemon during FFmpeg/upload; the job returns to a consistent state, with a checkpoint and no duplicate publication.
- A CLI failure does not interrupt the job.
- Two equivalent calls with the same idempotency key do not generate two uploads/publications.

### 15.6 Scenario: security

- No secret in JSON, arguments, logs, traces, or exported files.
- A daemon configured for Tailscale/NetBird does not start a public listener if the interface fails.
- An unauthorized client cannot install a plugin, restore the Archive, delete assets, or publish.
- Headless Linux without a provisioned keyring fails explicitly, with no silent plaintext.

### 15.7 Scenario: hooks and observability

- A `before` hook can reject an invalid source; the error is audited.
- A `finish` hook records the end on success as well as on error/cancellation.
- Loki unavailable: jobs continue, events are buffered with limits and a failure record; Datadog can keep receiving.
- A `publication.completed` event triggers synchronization of the public library, with loop prevention and idempotency.

---

## 16. Suggested implementation roadmap

**The stages below are proposals, not contracted deadlines.** The design must guarantee future compatibility without requiring all plugins to be implemented in the first release.

### Phase 0 — Contracts and integration prototypes

- Choose the open source license, contribution policy, and repository name.
- Define the JSON Schema for workflows, plugins, connectors, and job states.
- MediaCMS PoC with pre-processed proxy and metadata synchronization.
- VideoVortex PoC with transcription, search, and clipping, verifying version/license capabilities and restrictions.
- FFmpeg PoC for the working master from the raw file and a real clipping test.
- Validate integration with GCS, headless Linux keyring, and the private Tailscale/NetBird mode.

### Phase 1 — Functional core and ingestion

- Local/remote CLI and daemon, authentication, and durable jobs.
- Minimal JSON workflow, `storage.put/get/verify`, `video.transcode`, `audio.extract`, `ai.transcribe`, `ai.index`.
- GCS connector with `archive/master/proxy` and portable metadata generation.
- JSONL logs and job status/logs API.
- Complete `ingest-original.json` with `ready_for_clipping`.

### Phase 2 — Semantic clips and subworkflows

- Lexical+vector retrieval, identification of the complete narrative.
- Preview, review, timestamp adjustment, and rendering with FFmpeg.
- `create-clip.json` and `ingest-derived.json` with lineage, backup, and reindexing.
- Ensure that no clip reads from the Archive.

### Phase 3 — Publishing and public catalog

- YouTube connector; OAuth in the keyring; resumable upload, status, thumbnails, captions.
- Persistent approval; idempotent publishing; published files in operational storage.
- Next.js library with search and access to approved assets only.

### Phase 4 — Plugins, hooks, and complete observability

- Isolated plugin runtime, permissions manifest, registry.
- Global/workflow/node hooks; persistent Event Bus.
- OpenTelemetry, Loki, Prometheus, optional Tempo/Grafana, and sink interface.
- Future connectors: Vimeo, S3, Dropbox, SFTP, Datadog, etc.

---

## 17. Pending decisions and mandatory validations

The definition conversation set the architecture and the high-level interfaces, but some points **have not yet been explicitly decided** and must not be silently filled in by the team:

1. **Vidyom license:** MIT, Apache-2.0, AGPL, or another; verify dependency compatibilities, especially integrated copyleft products.
2. **Deployment model:** initial Docker Compose/systemd; Kubernetes and multiworker if needed.
3. **Exact working master policy:** codec, bitrate/CRF, and resolution per class of original (1080p/4K), target size, and retention.
4. **Which embeddings engine/LLM and which ASR models** to use in each environment; offline requirements and costs.
5. **Approval policy:** levels, user/editor, publication default, and review flows for already public videos.
6. **Copyright and privacy policies:** permissions for speakers and participants; content removal, sensitive transcripts, and publication authorizations.
7. **Final semantics of retry, cancellation, and checkpoint** for each node type, especially external and non-idempotent ones.
8. **Definitive plugin protocol:** gRPC, JSON-RPC, HTTP, or a combination.
9. **Final workflow expression format** and merge and branching rules.
10. **Multi-user and multi-tenant:** whether needed from the MVP or at a later stage.
11. **Secrets on headless Linux:** keyring unlock strategy at boot without reducing security.
12. **Retention/security policy for the local cache** of masters and temporary files.
13. **MediaCMS/VideoVortex synchronization model**, failure handling, and metadata export.
14. **Cost limits and policies** for GCS transfers, transcription, and publishing.
15. **Publishing API compatibility**: policies, OAuth, limits, uploads, and exact metadata for each platform.
16. **Availability and names of `vidyom` packages/identifiers** in registries, domain, and repositories, plus trademark verification.

---

## 18. Consolidated register of decisions from the conversation

This section is a verification index so the team does not lose decisions.

| # | Decision | Status |
|---|---|---|
| 1 | Official name **Vidyom** | DECIDED |
| 2 | Project initially for **MapaDeva**, but **open source** and general-purpose | DECIDED |
| 3 | Category **`originals`**, not `lectures` | DECIDED |
| 4 | Content: yoga classes, courses, gatherings, talks, interviews, etc. | DECIDED |
| 5 | Preserve the raw file as a backup and use a compressed operational master | DECIDED |
| 6 | Initial backup in **GCS Archive** | DECIDED |
| 7 | Initial operational master in **GCS Standard** | DECIDED |
| 8 | Lightweight operational video proxy and dedicated audio for AI | DECIDED |
| 9 | Avoid the Archive throughout the clipping flow | DECIDED |
| 10 | Import/cache beforehand own videos that exist only on YouTube | DECIDED |
| 11 | Transcription, chapters, summary, tags, timestamps, embeddings, and semantic search | DECIDED |
| 12 | Two independent workflows: original ingestion and clip creation | DECIDED |
| 13 | Clip input = **previously indexed original asset + prompt** | DECIDED |
| 14 | AI identifies the start/end of complete stories | DECIDED |
| 15 | Each clip becomes a **new asset**, with master, backup, index, and lineage | DECIDED |
| 16 | Editorial production, approval, and public library for published clips | DECIDED |
| 17 | Private catalog: **MediaCMS** as the main interface | DECIDED |
| 18 | **VideoVortex** as a supporting tool for search/CLI/clips | PROPOSED AND PREFERRED |
| 19 | **Rewind** optional and **Arcadeia** not a priority | PROPOSED |
| 20 | Public library initially via **Next.js**, using approved/published content | PROPOSED AND PREFERRED |
| 21 | CLI with per-stage commands | DECIDED |
| 22 | Workflows in JSON, inspired by n8n, without writing code to compose existing nodes | DECIDED |
| 23 | Configurable, reusable subworkflows | DECIDED |
| 24 | `vidyom` client CLI + persistent `vidyomd` daemon | DECIDED |
| 25 | Separate `vidyom-worker`, initially co-located | PROPOSED |
| 26 | Persistent session architecture similar to Herdr | DECIDED |
| 27 | Encrypted and authenticated local/remote communication | DECIDED |
| 28 | Credentials in the OS keyring, client **or** server | DECIDED |
| 29 | Operation restricted to the **Tailscale/NetBird** interface | DECIDED |
| 30 | Logs, metrics, traces, auditing, alerts, and OTel | DECIDED |
| 31 | Pluggable Storage connectors: GCS first; S3/Dropbox/SFTP, etc. | DECIDED |
| 32 | Pluggable Observability connectors: Loki/Datadog, etc. | DECIDED |
| 33 | Pluggable Publishing connectors: YouTube/Vimeo/etc. | DECIDED |
| 34 | An asset can be published to multiple destinations | DECIDED |
| 35 | Hooks `before`, `after`, `finish`, errors, and per-node hooks | DECIDED |
| 36 | Plugins with permissions and persistent asynchronous events | DECIDED |
| 37 | Extensible architecture for catalog, ASR, and AI providers | DECIDED |
| 38 | Metadata portability/export and import in open formats | DECIDED |
| 39 | Go in the core and Python/FFmpeg in the workers as needed | PROPOSED |
| 40 | PostgreSQL + pgvector as canonical catalog, jobs, and search | PROPOSED |

---

## 19. Expected outcome

Vidyom must allow any user to install a daemon on their own server, configure storages and publishing platforms with securely stored credentials, control all the work through the CLI, and define media pipelines in JSON.

A 10 GB recording can be archived, converted, transcribed, summarized, and indexed **once**. From it, the user can request clips by topic or story at any point in the future. The daemon will locate the segment, retrieve **only the working master** from the accessible storage, render a new video, create another complete asset with backup and metadata, make it available for review, and publish it to the selected destinations. All of this with logs, auditing, hooks, plugins, and connectors that are independent of vendors.

**This is the product and architecture contract from which the implementation of Vidyom must be planned, split into issues, and validated by acceptance tests.**
