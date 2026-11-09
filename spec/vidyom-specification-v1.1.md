# Vidyom — Complete Project Specification

> **Open-source AI-powered video workflow engine**  
> **Preserve · Discover · Transform · Share**  
> **Foundation document for architecture, product, and implementation**  
> Document version: **1.1 — 2026-11-08** (consistency and gap review of v1.0; see **§20 Revision log v1.1**)  
> Project origin: **MapaDeva — https://www.mapadeva.com.br/**  
> Status: **specification for implementation; does not represent features already delivered**.

---

## 0. How to read this document

This document consolidates **all decisions expressed throughout the Vidyom definition conversation** and the technical details proposed for turning them into an implementation. To avoid confusion:

- **[DECIDED]**: a requirement or choice explicitly established in the conversation.
- **[PROPOSED]**: a technical design recommended in the discussions; adjust it during detailed design, without contradicting the decided requirements.
- **[FUTURE]**: a planned extension that the architecture must make possible, but that need not be implemented in the MVP.
- **[VALIDATE]**: compatibility, limitation, cost, licensing, or behavior of an external tool that must be confirmed in a proof of concept before being assumed.

In revision v1.1, passages that were changed or added are marked **(v1.1, Rn)**, where `Rn` refers to the corresponding item in §20. Where the revision resolved an ambiguity that the conversation had not decided, the result is marked **[PROPOSED]**, never [DECIDED]. No [DECIDED] decision from v1.0 was reversed; they were only clarified or made consistent.

The command names, plugin contracts, and JSON formats in this document are **target interfaces of Vidyom**, not commands that already exist in MediaCMS, VideoVortex, FFmpeg, YouTube, or n8n. Example IDs, bucket names, URLs, paths, and times are fictitious.

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

MapaDeva will produce recordings of yoga classes, courses, meetings, talks, interviews, and other content. Each original recording may be approximately **10 GB**. The system must:

1. Preserve the raw recording with integrity and controlled storage cost.
2. Keep a good-quality working version **directly accessible** for continuous production, without restoring from backup.
3. Transcribe and index the content so it is searchable by words and by **semantics**.
4. Find stories, examples, teachings, or topics through **natural-language prompts**, even without a literal word match.
5. Create **many independent clips** from the same original, including months or years after ingestion.
6. Treat each clip as a **new asset**, with its own master, backup, metadata, indexing, and history.
7. Publish approved material to video platforms, initially YouTube, while maintaining a **public library** of selected content.
8. Automate the process through **JSON workflows**, reusing nodes and connectors, without programming to create each new workflow.
9. Operate remotely through a **long-running daemon + client CLI**, with security, auditing, and observability.
10. Not depend on a single provider: storage, publishing, AI, catalogs, and observability must be extensible through connectors and plugins.

### 1.3 Architecture principles

- **CLI-first, daemon-centric, API-driven.**
- **Declarative workflows** in JSON, with a node-and-connection architecture inspired by n8n, without requiring n8n to be installed.
- **Portability:** metadata and transcripts in open formats; export/import as a requirement.
- **Separation between source, processing, catalog, and publishing.**
- **The working master never depends on the archive backup for clips.**
- **Secure by default** and least privilege.
- **Replaceable connectors**, identified by logical roles.
- **Plugins and hooks** without changing the core for common extensions.
- **Durable execution**, with resume, idempotency, traceability, and prevention of duplicate external effects.
- **One asset, several versions, several derivatives, and several publications.**
- **Private catalog distinct from the public library**, with explicit, reviewed, and controlled publishing.

### 1.4 Out of scope of assumptions

- Do not assume that MediaCMS, VideoVortex, or Rewind provide all of the semantics specified here out of the box.
- Do not claim that GCS Standard storage is free: it has no specific retrieval fee like cold classes, but **storage, operations, and traffic/egress may cost money**.
- Do not conflate a reduction in audio file size with a proportional reduction in the **tokens** or the price of a transcription API.
- Do not assume that a YouTube API allows generic download of published files; the preferred strategy is to **keep the file uploaded to YouTube** in operational storage.
- Do not make private content available in the public library automatically.

---

## 2. Vocabulary and editorial model

### 2.1 Assets

- **Original (`original`) [DECIDED]:** an input recording, regardless of type (class, course, meeting, talk, etc.). **Definitively** replaces the old `lectures` folder/category. **(v1.1, R1)** Three distinct concepts must not be confused: the **asset type** (`kind` = `original`), the **storage prefix** (`originals/`, a connector convention), and the **content category** (`category` = `yoga_class`, `lecture`, etc., §2.2). The decision “`originals` and not `lectures`” refers to the type/prefix; “lecture” becomes just a content category (`lecture`).
- **Clip (`clip`) [DECIDED]:** material derived from one or more intervals of another asset; it receives its own identity, master, and backup.
- **Edited video (`edited`) [DECIDED]:** another editorial/derived version, traceable to the source. **(v1.1, R2) [PROPOSED]** Edited videos produced outside Vidyom (e.g., in a non-linear editor) enter through `ingest-derived.json` with parent assets **declared** by the user; the lineage is marked `lineage.method = "declared"` (in contrast with `"computed"` for clips generated by Vidyom, whose intervals are exact).
- **Raw master (`source_original`):** the input file in its best preserved form (e.g., 10 GB), kept in backup.
- **Working master (`working_master`) [DECIDED]:** a good-quality file kept in operational storage with direct, permanent access for as long as it is needed to create clips.
- **Proxy (`proxy`) [DECIDED]:** a lighter file used for playback, previews, navigation, and visual identification of segments.
- **Audio for ASR (`transcription_audio`):** a mono/compressed derivative suited to transcription; it may have temporary retention. **(v1.1, R3)** If it is discarded and a new transcription is needed after ingestion, it is regenerated from the **working master**, never from the Archive (consistent with §3.2).
- **Clip master (`clip_master`) [DECIDED]:** a new master, rendered from the source's working master, not from a degraded proxy or from the Archive.
- **Publication (`publication`) [DECIDED]:** an independent record of an asset at an external destination (YouTube, Vimeo, etc.).
- **Published file (`published_file`) (v1.1, R4) [PROPOSED]:** the exact file sent to a destination. It sits in the `master` role (permanent operational storage). If it is byte-for-byte identical to the asset's master (same SHA-256), it is not duplicated: the publication merely references the master and the hash.

### 2.2 Original categories

The category is **metadata**, not a fixed structure: `yoga_class`, `course`, `meeting`, `lecture`, `interview`, `event`, `other`, with support for new types through configuration.

### 2.3 Identification and relationships

- **[DECIDED]** A permanent identifier for each asset: examples `ORI-001`, `COR-001`.
- **[DECIDED]** Each clip stores its source asset(s), time intervals, versions, and history.
- **[DECIDED]** An original talk/class can generate N clips across multiple production moments, without rerunning the original ingestion.
- **[DECIDED]** An asset can be published to several destinations, each with its own `remote_id`, URL, metadata, and state.
- **[PROPOSED]** Use UUID/ULID as the immutable technical key and friendly IDs as human aliases.
- **(v1.1, R5) [PROPOSED] Friendly ID prefixes and format:** `ORI-` (original), `COR-` (*corte*, clip), `EDT-` (edited), `JOB-` (execution), `APR-` (approval), `PUB-` (publication), `CLIENT-` (registered client). Sequential numbering per prefix, with at least 3 digits and no upper limit (`ORI-1000` is valid). Allocation is transactional in the canonical catalog, so that two clips created at the same time never receive the same ID. A friendly ID is never reused, even after deletion. The prefixes remain in Portuguese by decision of origin (`COR` = *corte*, Portuguese for clip); the API always accepts and returns both the technical ID and the alias.
- **(v1.1, R6) [PROPOSED] Asset versions:** each asset has immutable versions `v1`, `v2`…. A new render (e.g., an adjustment to start/end after approval), a new transcode of the master, or a file replacement creates a new version; editing only metadata (title, tags) does not create a media version, but is recorded in the history. Files of a version are never overwritten; storage keys include the version (§14.1). Publications record which version was sent.
- **(v1.1, R7) Clips of clips:** since the definition of `clip` allows deriving from “another asset”, a derivative that reaches `ready_for_clipping` can be the source of a new clip. **[PROPOSED]** To avoid generation loss, the intervals are converted to the timeline of the root original and rendered from the original's master, when it exists and the edit is continuous; the lineage records the full chain.

### 2.4 Lineage example

```text
ORI-001 — Yoga Class (indexed original)
├── COR-001 — Explanation of Ujjayi breathing
│   ├── YouTube publication
│   └── Vimeo publication
├── COR-002 — Story of the 90-year-old woman
└── COR-003 — Case of the friend who never got a hug
```

### 2.5 Asset states (v1.1, R8) [PROPOSED]

v1.0 defined **job** states (§5.5) but mentioned **asset** states (`ready_for_clipping`, `editorial_review`) without defining them. Asset state, job state, and visibility are independent axes.

| State (`status`) | Applies to | Meaning |
|---|---|---|
| `registered` | all | ID allocated; no file verified yet |
| `ingesting` | all | ingestion workflow in progress |
| `ready_for_clipping` | original and derivatives | readiness conditions of §3.1 satisfied; can be a clip source |
| `ingest_failed` | all | ingestion ended with an error; never a clip source |
| `editorial_review` | derivatives | ingested and awaiting editorial production/publication approval |
| `approved` | derivatives | approved for publication |
| `rejected` | derivatives | rejected in review; kept for history |
| `published` | derivatives | at least one confirmed publication (details live in each `publication`) |
| `withdrawn` | derivatives | withdrawn from all publications and from the public library |
| `archived_only` | all | working master removed by policy; **not** a clip source until administrative restoration |

Complementary flags: `master_available` (verified periodically by `verify-storage.json`), `backup_verified`, `catalog_sync` (state of synchronization with MediaCMS/VideoVortex, §13.1). If a verification detects a missing or corrupted master, `master_available = false` and the asset is no longer accepted by `create-clip`, with no automatic restoration.

`visibility` (`private`, `unlisted`, `public`) describes only the public library and the destinations; assets are born `private`.

---

## 3. Two independent workflows and subworkflows

### 3.1 Workflow A: `ingest-original.json` [DECIDED]

**Goal:** receive an original, protect the raw file, generate working versions, extract knowledge, store and index, leaving it eligible for future clips.

**Possible inputs:** a local file on the server, a CLI upload, a file previously available in storage, or an own video published externally and imported from an authorized source. For remote uploads, the CLI must transfer the content in a resumable way, with a checksum. How to reference each type of input is in §5.4.

**Functional steps:**

1. Receive the file and basic metadata: title, category, date, speaker/instructor, rights, language when known.
2. Register the asset with a permanent ID; validate the file, format, and SHA-256 checksum.
3. Persist the input copy during ingestion; never depend on a transient external link. **(v1.1, R9)** The local input copy may be deleted only after the backup in `archive` **and** the master in `master` have been verified by checksum.
4. Send the **raw** file to the `archive` storage role and **verify** the backup.
5. Generate, with FFmpeg, a **compressed working master** that is nonetheless suitable for future clips. **(v1.1, R10)** The master, the proxy, and the ASR audio must keep the **same timeline** as the raw file (no trimming of start/end, same zero instant, normalized timestamps). After generating each one, `ffprobe` compares its duration with that of the raw file and rejects a difference above a configurable tolerance (e.g., 1 frame or 100 ms). This is what allows the transcript timestamps to be used directly on the master and the proxy.
6. Send and verify the master in the `master` role (initially GCS Standard).
7. Generate a lightweight proxy in the `proxy` role (initially GCS Standard).
8. Extract audio optimized for transcription **directly from the best-quality file available**, avoiding double audio compression.
9. Optionally use VAD (voice activity detection) to exclude long silences, preserving a timestamp map between the processed audio and the video.
10. Transcribe with a configurable engine (initially faster-whisper), with timestamps; store the raw transcript and open formats.
11. Generate summaries, chapters, topics/tags, story identification, and embeddings with AI.
12. Index text and vectors in the canonical catalog (PostgreSQL + full-text search + pgvector as the initial proposal) and, where compatible, in VideoVortex.
13. Register/expose the asset in **MediaCMS**, the private visual catalog.
14. Validate the existence/integrity of the working master and the indexes.
15. Only then set the **`ready_for_clipping`** state.
16. Emit events (`original.indexed`, `asset.indexed`, `workflow.completed`) and record the audit.

**Mandatory readiness condition:** a directly accessible working master; a transcript with timestamps; a valid search index; integrity checks. Without these, the asset is not a candidate for clipping.

**(v1.1, R11) Clarifications on readiness:**

- The list above did not include the backup, although step 4 and §15.1 require it. **The verified backup in `archive` becomes part of the readiness condition** (consistent with the decision “preserve the raw file in backup”).
- The required index is that of the **canonical catalog** (PostgreSQL). Synchronization with MediaCMS (step 13) and VideoVortex (step 12) **does not block** `ready_for_clipping`: failures there are recorded as `catalog_sync = failed`, with retry and an alert. This applies the rule in §13.1 that neither of these products is a single source of truth.
- The proxy is needed for the clip preview (§3.2, step 6), so it is also part of the condition.

Consolidated condition: `backup_verified` ∧ `master_available` (verified) ∧ proxy available ∧ transcript with timestamps ∧ valid text and vector indexes in the canonical catalog ∧ validated timeline (R10).

### 3.2 Workflow B: `create-clip.json` [DECIDED]

**Goal:** at any time, receive an **ID of an already-indexed original + a prompt** and produce a new independent clip with a complete production and publishing cycle.

**Real input examples from the conversation:**

- “Story of the 90-year-old woman who had no remorse”.
- “Case of the friend who never got a hug”.
- “Explanation of Ujjayi breathing”.

**Functional steps:**

1. Receive `source_id`, `prompt`, and options (`complete_story`, a short version up to a target duration, or best moments).
2. Resolve exclusively an **indexed asset** (`ready_for_clipping` and `master_available`). **(v1.1, R12)** In the MVP, `create-clip` takes **a single source**. The definition of a clip (§2.1) allows “one or more” source assets; assembly from multiple sources is left as **[FUTURE]**, but the lineage model already records the source of each interval (§12.3).
3. Run lexical and semantic search **restricted to the requested asset**; do not require literal words.
4. Retrieve the preceding and following context and estimate narrative boundaries (introduction, development, conclusion).
5. Generate candidates with timestamps and confidence scores; if ambiguous, request selection/review, do not cut at random.
6. Generate a preview from the proxy; allow adjusting start/end.
7. Obtain approval of the proposed clip or proceed under an explicit editorial approval policy. **(v1.1, R13)** This is the **selection gate** (`approval.kind = "clip_selection"`), distinct from the **publication gate** of step 13 (`approval.kind = "publication"`). The two are separate approval records, with an owner, date, and approved version. **[PROPOSED]** If a request generates intervals that substantially overlap an existing clip of the same source, the system warns and shows the existing clip before proceeding; it does not block.
8. Read the working master **only from the `master` role**, caching temporarily on the worker's local disk; **do not access `archive`**.
9. Render a new clip master with FFmpeg at suitable quality and preserve a map of the intervals used (a single interval or an assembly of several).
10. Register a **new derived asset**, with ID, version, master, and lineage.
11. Call **`ingest-derived.json`** for backup of the clip master, upload of master and proxy to operational storage, update/indexing of text, summaries, thumbnail, and private catalog.
12. Run editorial production: title, description, captions, thumbnail, fact review, and permissions.
13. Request **approval, mandatory by default,** before public publication.
14. Call **`publish-video.json`** with the `destinations` parameter (e.g., `["youtube-main"]`). **(v1.1, R14)** The canonical name becomes `publish-video`; `publish-youtube` is no longer a separate workflow (at most an alias that calls `publish-video` with `destinations = ["youtube-main"]`).
15. Create/update the publication record and make the clip available **in the public library** after confirmation of the publication and of the public state.
16. Allow new clips from the same source at any time, without redoing Workflow A.

**Invariant rule:** if `master` is unavailable, the execution signals an error/unavailability; it **never restores automatically from the Archive**. Restoration, if exceptionally necessary, will be a separate, explicitly authorized administrative flow.

### 3.3 Reusable subworkflows [DECIDED]

- `ingest-derived.json`: treats a new clip or other derivative as an independent asset; backup, working master, proxy, metadata, indexing, integrity, catalog, and events.
- `publish-video.json`: review and approval, upload, monitoring of remote processing, recording of URL/ID, copy of the published file, and update of the public library. Takes `destinations` as a parameter (v1.1, R14).
- **[PROPOSED]** `restore-archived.json` as a separate administrative operation, with special permission control, auditing, and cost.
- **[PROPOSED]** `reindex-asset.json`, `refresh-publication.json`, `verify-storage.json`, `bulk-create-clips.json`.
- **(v1.1, R15) [PROPOSED]** `unpublish-video.json`: withdraws the publication from one or more destinations (or changes visibility to private), updates the record, and removes the asset from the public library (event `publication.withdrawn`). v1.0 provided no withdrawal path, but one is needed to honor removal requests (§17, item 6).
- **(v1.1, R16) [PROPOSED]** `delete-asset.json`: controlled deletion (permission `asset.delete`), which requires withdrawing the publications first, respects the Archive's minimum retention (§7.2), and keeps in the audit a record that the asset existed.

### 3.4 When a video is already on YouTube [DECIDED]

- If it was published by Vidyom, **retain in operational storage the same version that was sent**; never depend on YouTube as a master repository.
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
| Working master | H.264/H.265, preferably 1080p or the source's relevant resolution, high quality | Clips, reframing, future rendering |
| Proxy | MP4 H.264 720p, lower quality | Player, previews, and navigation |
| Audio for ASR | Mono 16 kHz, Opus ~24–32 kbps, depending on supported engine | Efficient transcription |
| Transcript | Structured JSON + SRT/VTT + Markdown/text | Search, editing, export |

Do not downgrade the **sole** production master to 720p when 1080p clips, zoom, and vertical videos will be needed. Evaluate operational preservation of 4K depending on the source and the editorial goals.

**(v1.1, R17) Complementary profile rules:**

- **Never upscale:** the `master-1080p` profile produces at most 1080p; a 720p source produces a 720p master. The profile name indicates the ceiling, not a fixed value.
- **Master audio** in high quality (e.g., AAC ≥ 192 kbps stereo, or a copy of the original audio when the container allows), because published clips inherit that audio. The table above specified only the video.
- Preserve the source's frame rate and color space, unless the profile says otherwise.
- Keep the source's timeline (R10).
- Profiles are versioned (`profiles/*.json` with `version`); the asset records which profile version generated each file.

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

- Compressing from 10 GB to 20 MB does not imply a proportional reduction in transcription cost; cost may be per **minute**, per **audio token**, or local infrastructure, depending on the provider.
- Compression reduces storage, transfer, I/O, and the need to analyze visual frames.
- Reducing the actual duration sent (e.g., removing long silences via VAD) **may** reduce the cost of models billed by duration, provided intelligibility and timestamps are preserved.
- For an LLM that analyzes text, cost depends on the size of the transcript/passages sent, not on the size of the original video.
- Index **once** and query text/embeddings afterward, avoiding repeated retranscription.
- A clip's transcript can be derived from the source, but when there is assembly/editing/alteration of speech, timestamps must be reconciled and, if necessary, the altered intervals retranscribed.

### 4.4 Semantic clips: requirements

- Robust semantic retrieval (approximate descriptions, paraphrases, incomplete names).
- Identification of the **complete story**, not just the first occurrence of a word.
- Segmentation with speech context, adjusting cuts to natural sentence boundaries.
- Preserve the time reference among original, proxy, transcript, clip, and publication.
- Support continuous cuts and assembly of multiple intervals with transparency in the lineage.
- Produce MP4, SRT/VTT, thumbnail, title/description suggestions, and an edit manifest.
- Review/approval before publication, by default.
- **[PROPOSED]** Evidence for review: text found, timestamps, and confidence score; an ambiguous clip requires human intervention.
- **(v1.1, R18) [PROPOSED] Vector index:** each vector stores the identifier and version of the embedding model and the segmentation strategy (`chunking`) used. Changing the model requires reindexing (`reindex-asset.json`) and cannot mix vectors from different models in the same search. The segmentation strategy (by sentence, a window of N seconds with overlap, or by chapter) had not been defined and goes into §17, item 4.

---

## 5. Runtime architecture: CLI + daemon + workers

### 5.1 Components [DECIDED]

- **`vidyom`**: lightweight client CLI. Sends commands, uploads, and requests, follows logs/state, approves, queries the catalog, and publishes.
- **`vidyomd`**: permanent daemon, running in the background on a local or remote server. API, authentication, authorization, scheduler, workflow engine, job management, events, hooks, plugins, connectors, and state.
- **`vidyom-worker` [PROPOSED]**: runs heavy, isolatable work (FFmpeg, ASR, AI). It can start on the same host and later scale horizontally.
- **PostgreSQL + pgvector [PROPOSED]**: canonical catalog, execution, events, metadata, audit, text/vector indexes.
- **Operational storage and archive**: abstracted via connectors and logical roles.

### 5.2 Architectural reference

**[DECIDED]** An experience similar to **Herdr**: processes persist beyond the client session; clients connect, disconnect, and reconnect. Vidyom does not need to preserve pseudo-terminals: it preserves **jobs, checkpoints, intermediate files, and node state**.

### 5.3 Communication

- CLI and daemon on the same host: **Unix Domain Socket** on Unix/macOS [PROPOSED], with restricted permissions.
- Remote: **SSH** (tunnel to the API/socket) and/or **HTTPS/TLS 1.3**, ideally mTLS in appropriate scenarios [DECIDED: encryption and security; PROPOSED: mechanisms].
- Versioned API with JSON; event/log streaming via SSE, WebSocket, or gRPC streaming [PROPOSED, choose implementation].
- Multiple simultaneous clients can follow the same job.
- The CLI can run on macOS, Linux, and Windows; the daemon initially on Linux [PROPOSED].
- Processing and external secrets stay on the server; FFmpeg or Whisper is not needed in the CLI.
- **(v1.1, R19) [PROPOSED] Client identity:** v1.0 required authentication and mentioned `security sessions` / `security revoke CLIENT-001`, but did not say how a client registers. Proposal: on the local Unix socket, identify the user by the process credentials (`SO_PEERCRED`) and map them to a role. Remotely, `vidyom server add` performs a **pairing**: the admin generates a single-use code with a short validity on the server; the client generates a key pair, whose private key stays in the client's keyring, and receives a client certificate (mTLS) or a session token with defined validity and scope. Each paired client becomes a revocable `CLIENT-nnn`. Over SSH, the SSH identity is mapped to a Vidyom user and pairing remains mandatory.

### 5.4 Remote upload

Clearly distinguish:

- `--input file=/data/class.mp4` = a file already accessible to the **server**.
- `vidyom asset upload ./class.mp4` = a file on the **client**; resumable (chunked/resumable) transmission, with checkpoint and checksum; only afterward start ingestion.

**(v1.1, R20) [PROPOSED] Unified input references.** v1.0 used `--input file=/data/class.mp4` (server), `asset upload` (client), and `original import yoga-class.mp4` (location unspecified). To avoid ambiguity, every workflow file input is a URI:

| Form | Meaning |
|---|---|
| `server:/data/class.mp4` | a file on the server's filesystem |
| `upload:UPL-001` | a resumable upload that is already complete and verified |
| `storage://<role>/<key>` | an object in a configured storage role (never `archive` in `create-clip`) |
| `import:<connector>/<id>` | an authorized import from an external source (e.g., an own video on YouTube, §3.4) |

- `vidyom original import <path>` treats `<path>` as a **client file** (uploads and then ingests); `--server-path` indicates a server file.
- `server:` paths are restricted to input directories (`ingestRoots`) configured in the daemon, to prevent arbitrary reads of server files; paths with `..` or symbolic links that leave those roots are rejected.
- A path without a prefix in `--input file=` remains accepted as `server:`, for compatibility with the examples.

### 5.5 Jobs and persistence

Proposed states: `queued`, `running`, `waiting_approval`, `retrying`, `paused`, `completed`, `failed`, `cancelled`.

- A remote command returns `job_id` immediately; the client can attach/detach.
- Ctrl+C while following logs **disconnects**, it does not cancel the job.
- Durable checkpoints per node; retries with backoff and limits; upload/publish idempotency.
- A daemon restart triggers reconciliation of in-flight nodes, without duplicating external effects.
- While waiting for approval, no CLI process needs to stay connected.
- Workers declare CPU, GPU, memory, disk, and capacity; the scheduler chooses a compatible worker [PROPOSED].
- **[PROPOSED]** Configurable retention of execution records and safe cleanup of temporaries.
- **(v1.1, R21) Permanent provenance:** job retention (§10.4, 90 days) cannot delete the lineage. Each asset version permanently stores a **provenance summary**: job ID, workflow ID/version/hash, node types and versions, media profile version, FFmpeg/ASR/AI model versions, and input and output checksums. This summary belongs to the asset, not to the job history.
- **(v1.1, R22)** `paused` needs commands to enter and leave the state: `vidyom job pause` and `vidyom job resume` (§11.3). Pausing does not interrupt an external effect that has already started; it finishes and the job pauses at the next checkpoint. `waiting_approval` is left via `approval approve|reject` or by policy timeout.
- **(v1.1, R23)** `JOB-nnn` IDs follow the rule in R5 (a friendly alias over a ULID).

### 5.6 Languages [PROPOSED]

- **Go** for the CLI, daemon, API, and worker coordination (simple binaries, concurrency, distribution).
- FFmpeg/ffprobe as controlled external executables.
- **Python** for transcription/AI when advantageous, run in a separate worker/process.
- Plugins in an isolated process, HTTP/gRPC, WASM in specific cases; do not depend on native Go `.so` plugins.

---

## 6. JSON workflow engine inspired by n8n

### 6.1 Principles [DECIDED]

- Workflows are **JSON** files containing nodes, types, versions, parameters, connections, inputs, outputs, and settings.
- Create a new flow **without writing code**, as long as the required nodes and plugins already exist.
- Conceptual reference: **n8n** workflow exports; do **not** require binary/exact compatibility or an n8n installation.
- Plugins can add node types and connectors.
- The daemon validates the JSON files against versioned schemas and executes a directed graph (initially a DAG, with controlled `foreach` and conditions).
- Composition and reusable **subworkflows** are core requirements.

### 6.2 JSON model [PROPOSED]

Recommended fields: `id`, `name`, `version`, `description`, `inputs`, `trigger`, `nodes`, `connections`, `hooks`, `settings`, `outputs`.

- `nodes[].id`: unique per workflow.
- `nodes[].type`: versioned name of the type (`video.cut`, `storage.put`, `ai.transcribe`).
- `nodes[].typeVersion`: plugin/node contract.
- `nodes[].parameters`: parameters validated by JSON Schema.
- `connections`: dependency/control edges; a node runs only when all of its required inputs are ready.
- `workflow.execute`: invokes a subworkflow with input and output isolation.
- `flow.if`, `flow.foreach`, `flow.wait`: explicit control.
- `${inputs.foo}`, `${nodes.id.output.field}`, `${event.field}`: **restricted declarative** expression, with no arbitrary JavaScript or shell evaluation.
- **(v1.1, R24)** A `${nodes.X...}` reference in node Y **requires** X to be an ancestor of Y in `connections`; the validator rejects a reference to a node that is not an ancestor (in the §6.3 example, `backup` uses `register`, so `register → backup` is mandatory). To write `${` literally, use `$${`.
- **(v1.1, R25) [PROPOSED]** `approval.request` node (human gate): parameters `kind` (`clip_selection` | `publication` | other), `assetId`, `approvers` (role or users), `timeoutSeconds`, and `onTimeout` (`reject` | `fail` | `remind`). It puts the job in `waiting_approval` and creates an `APR-nnn` record. v1.0 mentioned `requireApproval` and `waiting_approval`, but had no node that implemented them.
- Workflows can have a `trigger` by CLI, event, webhook, or schedule, once those types are implemented.
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

**Note:** this is a minimal example to explain the format; the full implementation must include the proxy, summaries, embedding generation, validations, `ready_for_clipping`, events, states, and checksums. Registration in MediaCMS may require integration specific to the installed version.

**(v1.1, R26) Differences between the example and §3.1 that the implementation must resolve:**

- the `audio.extract` node (step 8) is missing between the source and `ai.transcribe`;
- the key `originals/<id>/original.mp4` hardcodes the `.mp4` extension, but the raw file may be MOV/MKV/MXF, etc. The key must preserve the original extension (`original.<ext>`) and the version (§14.1);
- the `catalog` node (MediaCMS) must not be a precondition for `ready_for_clipping` (R11); the node that changes the state to `ready_for_clipping` is missing, and it must depend on `backup`, `store_master`, the proxy, and `index`.

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
      "id": "preview",
      "type": "video.preview",
      "typeVersion": 1,
      "parameters": {
        "source": "${nodes.source.output.proxy_uri}",
        "segments": "${nodes.find_story.output.segments}"
      }
    },
    {
      "id": "approve_selection",
      "type": "approval.request",
      "typeVersion": 1,
      "parameters": {
        "kind": "clip_selection",
        "assetId": "${inputs.source_id}",
        "evidence": "${nodes.find_story.output}",
        "preview": "${nodes.preview.output.file}",
        "allowAdjust": true
      }
    },
    {
      "id": "render",
      "type": "video.cut",
      "typeVersion": 1,
      "parameters": {
        "source": "${nodes.source.output.working_master_uri}",
        "segments": "${nodes.approve_selection.output.segments}"
      }
    },
    {
      "id": "ingest_derived",
      "type": "workflow.execute",
      "typeVersion": 1,
      "parameters": {
        "workflow": "ingest-derived",
        "file": "${nodes.render.output.file}",
        "kind": "clip",
        "lineage": {
          "method": "computed",
          "source_asset_id": "${inputs.source_id}",
          "segments": "${nodes.approve_selection.output.segments}"
        }
      }
    },
    {
      "id": "publish",
      "type": "workflow.execute",
      "typeVersion": 1,
      "parameters": {
        "workflow": "publish-video",
        "assetId": "${nodes.ingest_derived.output.asset_id}",
        "destinations": ["youtube-main"],
        "requireApproval": true
      }
    }
  ],
  "connections": {
    "source": ["find_story"],
    "find_story": ["preview"],
    "preview": ["approve_selection"],
    "approve_selection": ["render"],
    "render": ["ingest_derived"],
    "ingest_derived": ["publish"]
  }
}
```

**(v1.1, R27)** Compared with v1.0, the example now includes the preview from the proxy (§3.2, step 6), the selection gate with start/end adjustment (step 7), the **approved** segments, rather than the suggested ones, as the render input, and the full lineage passed to `ingest-derived` (previously only `parentId` was passed, and the intervals were lost). Editorial production (step 12) and the publication gate stay inside `publish-video`.

**`video.cut` node invariant:** the received URL must be validated as belonging to the logical role `master` (or a local cache derived from it); a silent fallback to `archive` is forbidden. **(v1.1, R28)** For this validation to be possible, roles must occupy **disjoint namespaces**: two roles cannot point to the same bucket with overlapping prefixes (in v1.0, `master` used `videos-standard` with no prefix and `proxy` used `videos-standard/proxies/`, meaning every proxy was also "inside" the master). The configuration validator rejects overlap (§7.2). The worker's local cache stores the role and the origin checksum, and `video.cut` checks both.

### 6.5 Versioning and safe execution

- Save an **immutable snapshot** of the JSON version with each job.
- New versions of the flow do not change jobs in progress.
- Validate node names, dependencies, cycles, schemas, permissions, and connector capabilities.
- No strings that become shell commands through concatenation; do not expose arbitrary code evaluation by default.
- Define an explicit way to wait for approval and handle timeout.
- Export/import workflows and schemas without including credentials.
- **(v1.1, R21)** The workflow snapshot is also included, by hash, in the permanent provenance summary of the generated asset.

---

## 7. Connector and plugin architecture

### 7.1 General rule [DECIDED]

**The core defines contracts; connectors integrate external services; nodes execute operations; workflows organize nodes; plugins add nodes, connectors, hooks, and event subscribers.**

- The project must not couple the core to GCS, YouTube, Loki, MediaCMS, or a specific AI provider.
- Connector settings live outside the workflows: logical names, parameters, capabilities, `credentialsRef`.
- Connectors must declare version, schema, capabilities, limits, and any relevant cost/semantics.
- Changing a provider, when equivalent, should not require editing every workflow JSON.
- **Migrating existing files remains a separate operation**; changing the configuration does not migrate the data automatically.

### 7.2 Storage connectors [DECIDED]

First connector: **Google Cloud Storage**. Future extensions: **AWS S3, Dropbox, SFTP, Azure Blob, local/NAS**, and others.

Logical roles:

- `archive`: preservation/backup, not used to produce clips.
- `master`: working master, direct access, permanent throughout the clip-creation cycle.
- `proxy`: playback, preview, and navigation.

**Initial mapping:**

| Role | Initial connector | Class | Content |
|---|---|---|---|
| `archive` | GCS | ARCHIVE | Raw originals and final masters of clips |
| `master` | GCS | STANDARD | Working masters of originals and derivatives, and published files (v1.1, R4) |
| `proxy` | GCS | STANDARD | Proxies, thumbnails, and previews |

**(v1.1, R4)** In v1.0, the table placed "published media" in `proxy`, while §3.4 and §7.3 required storing the published file in permanent operational storage and §14.1 placed it alongside the master. It was decided that the published file lives in `master`, which has the permanence guarantee; `proxy` may have a shorter retention policy.

**GCS Standard is not cost-free access:** there may be costs for storage, requests, and egress traffic. Avoid unnecessary egress; when possible, run the worker close to the bucket's region. Do not use Archive for operational reads.

**(v1.1, R29) [VALIDATE] Archive costs:** the GCS ARCHIVE class has a **minimum storage duration** (365 days, according to the current public documentation) with prorated charges for early deletion, plus a retrieval cost. Consequences for the design:

- backup copies of clips are only created **after** the selection gate (the §3.2 flow already guarantees this: rendering comes after step 7); discarded previews and renders never go to `archive`;
- each new media version (R6) creates a new object in Archive; it is recommended to back up only the versions **approved for publication** or the latest version, according to the `archivePolicy` policy (`all_versions` | `approved_versions` | `latest`, default **[PROPOSED]** `approved_versions` for derivatives and `all_versions` for originals);
- `delete-asset.json` (R16) reports the early-deletion cost before executing.

**Standard operations:** `put`, `get`, `stat`, `list`, `exists`, `delete`, `copy`, `move`, `checksum/verify`, `downloadRange`, `capabilities`. Unavailable operations must be declared as such or implemented through an explicit fallback subject to cost/permission.

**Optional capabilities:** versioning, range reads, server-side copy, resumable/multipart upload, checksum verification, storage classes, and restore semantics. The direct-access policy must be declared on the storage instance, not inferred from the driver name alone.

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
      "prefix": "masters/",
      "storageClass": "STANDARD",
      "credentialsRef": "gcs-main",
      "usage": "operational"
    },
    "proxy": {
      "driver": "gcs",
      "bucket": "videos-standard",
      "prefix": "proxies/",
      "storageClass": "STANDARD",
      "credentialsRef": "gcs-main",
      "usage": "operational"
    }
  }
}
```

**(v1.1, R28)** `master` now has the prefix `masters/`, so that `master` and `proxy` do not overlap in the same bucket. Using separate buckets is also valid.

**(v1.1, R30) Role vs. instance in records:** the asset references the **role** (`storage: "master"`) and the key, as in v1.0, and adds an immutable `location` field, written at write time (`driver`, `bucket`, `prefix`, object `generation`/version when available). That way, after a migration (§15.4), the audit still knows where each file was, and the migration tool can verify what has already been copied.

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

**YouTube** first; future: **Vimeo, PeerTube, Dailymotion**, and others. One asset can have **multiple publications**, without duplicating identity.

Capabilities contract: `upload`, `get`, `update`, `delete`, `setVisibility`, `uploadThumbnail`, `uploadCaptions`, `getStatus`, `getUrl`, `capabilities`.

- States and metadata support vary by platform; require the connector to declare them.
- Uploads must be resumable when the destination supports it.
- A publication will not be publicly visible without **approval** by default.
- **(v1.1, R31) [PROPOSED] Approval timing:** the example configuration uses `visibility: "private"` as the destination default, and §12.4 shows `public` publications. To reconcile them: by default, publication approval happens **before the upload**, because sending already hands the content to a third party. With the option `allowPrivateUploadBeforeApproval: true`, it is allowed to upload as `private` first and require approval only to change the visibility to `unlisted`/`public`. Changing the visibility to public is always an idempotent external effect, recorded and audited.
- Record `destination`, `remote_id`, `url`, version, state, timestamps, title/description, and visibility.
- **Reuse the uploaded master/version** from the `master` storage or operational storage to republish to another destination without retrieving a backup.
- If the destination is YouTube, keep the file used for the upload in operational storage; do not rely on a later re-download.

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
      "allowPrivateUploadBeforeApproval": false,
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

**[PROPOSED]** Log configuration:

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
            "path": "/var/log/vidyomd/vidyomd.jsonl",
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

**(v1.1, R32)** The local log file changed from `events.jsonl` to `vidyomd.jsonl`, so it is not confused with the Event Bus (§8.2), which is persisted in the database, not in a log file.

### 7.5 Other connectors [DECIDED as architecture; gradual implementation]

- `CatalogConnector`: initially **MediaCMS**.
- `TranscriptionConnector`: initially local **faster-whisper**; APIs and other engines in the future.
- `AIProvider`: local inference or remote API; configurable and interchangeable providers where their capabilities are equivalent.
- **VideoVortex** integration for CLI/transcript indexing, search, and clip operations; confirm compatibility and exact features of the installed edition.
- **Rewind** is **optional** for visual exploration/editing, not an MVP dependency.
- **Arcadeia** was considered and **not chosen** as a component of the priority architecture.
- Next.js is the proposed technology for the **public library**, consuming APIs/data published by Vidyom (see §13.2 on how this works with a daemon on a private network).
- **(v1.1, R33)** Status corrected: MediaCMS catalog = **DECIDED** (decision 17). VideoVortex = **PROPOSED AND PREFERRED** (decision 18; in §13.1 it appeared under a [DECIDED] heading). Rewind and Arcadeia = **PROPOSED** (decision 19). **[VALIDATE]** MediaCMS and VideoVortex licenses and their effect on the Vidyom license: integrating through API/CLI as a separate service tends not to contaminate Vidyom's code, but this must be confirmed together with item 1 of §17.

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

Plugins can run in **isolated processes** communicating over a versioned protocol (gRPC/JSON-RPC/HTTP), with WASM for some cases. Do not automatically grant filesystem, network, keyring, credentials, or arbitrary command execution.

---

## 8. Lifecycle hooks and Event Bus

### 8.1 Hooks [DECIDED]

Workflow and node hooks, configurable through JSON, with no need to modify the engine:

| Hook/event | Proposed semantics |
|---|---|
| `workflow.before` | Before starting: can validate/block |
| `workflow.start` | Effective start, notifications |
| `workflow.after` | Workflow success, before `finish` |
| `workflow.finish` | Terminal finalization, success/error/cancellation |
| `workflow.error` | Error, controlled handling |
| `workflow.cancel` | Cancellation request/execution |
| `node.before` | Before the node, allows validation/authorized modification |
| `node.after` | Node completed successfully |
| `node.error` | Node failure |
| `node.finish` | End of the node attempt, including error |

**Levels:** global (`config/hooks.json`), workflow (`hooks` in the JSON), and node (`hooks` in the node). Proposed order: global → workflow → node for `before`, reverse order for `after`/`finish`.

**(v1.1, R34) Names and the difference between hook and event:**

- Inside the `hooks` object, keys use the short name (`before`, `start`, `after`, `finish`, `error`, `cancel`); the scope comes from the position: `hooks` at the top of the workflow = `workflow.*`; `hooks` inside a node = `node.*`. For nodes, `start` and `cancel` do not apply.
- A **hook** is synchronous and part of the execution: it can block (`before`) and has a timeout. An **event** (§8.2) is asynchronous, goes to the outbox, and never blocks the workflow. `workflow.after` (hook) and `workflow.completed` (event) are different things: the event is emitted after the terminal state is written.
- **Default timeout [PROPOSED]:** 30 s when `timeoutSeconds` is omitted (v1.0 required a timeout, but the `after`/`finish` examples did not declare one). Notification hooks cannot exceed a configurable global limit.

**Rules:**

- Global security/audit hooks cannot be removed by workflows.
- `finish` runs for valid termination even after the daemon resumes, within the persistence guarantees, without promising execution in the event of irreversible system loss.
- Failure policy per hook: `abort`, `retry`, `continue`, when applicable.
- Mandatory timeout, idempotency, explicit permissions, logs, and traces.
- A hook that modifies parameters must go through validation and authorization again.
- Notification hooks cannot block the workflow indefinitely.

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
        "timeoutSeconds": 10,
        "onFailure": "continue"
      }
    ],
    "finish": [
      {
        "id": "record-metrics",
        "plugin": "analytics",
        "action": "record-execution",
        "timeoutSeconds": 10,
        "onFailure": "continue"
      }
    ]
  }
}
```

### 8.2 Events [DECIDED]

**Asynchronous, persistent** Event Bus, with subscriptions from plugins or workflows:

`original.indexed`, `asset.created`, `asset.indexed`, `clip.created`, `storage.backup.verified`, `publication.completed`, `publication.created`, `workflow.completed`, `workflow.failed`, among others.

**(v1.1, R35) Semantics of the events listed [PROPOSED]:**

| Event | When it is emitted |
|---|---|
| `asset.created` | ID allocated (`registered`), for any type |
| `clip.created` | specialization of `asset.created` for `kind = clip`; both are emitted |
| `asset.indexed` | valid canonical index, for any type |
| `original.indexed` | specialization of `asset.indexed` for originals; both are emitted |
| `asset.ready_for_clipping` | readiness condition satisfied (R11), which is different from "indexed" |
| `storage.backup.verified` | backup checksum verified |
| `approval.requested` / `approval.decided` | human gates (R25) |
| `publication.created` | publication record created, before or during upload |
| `publication.completed` | destination confirmed processing; the payload carries the current `visibility` |
| `publication.updated` | visibility or metadata changed at the destination |
| `publication.withdrawn` | publication removed or made private (R15) |
| `workflow.completed` / `workflow.failed` / `workflow.cancelled` | terminal job state written |

**Minimum event envelope:** `event_id` (ULID, also used as the idempotency key), `type`, `schema_version`, `occurred_at` (UTC, RFC 3339), `job_id`, `workflow_id`, `asset_id`, `trace_id`, `causation_id` (event or job that originated it), and `payload`. The `causation_id` makes it possible to measure chain depth and **prevent loops** (configurable depth limit, e.g., 5).

- Events can trigger other workflows (without changing the original workflow).
- Persist via **transactional outbox** [PROPOSED], with retries and event delivery tracking.
- **At-least-once** delivery, with idempotency keys; do not promise exactly-once at external destinations.
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

**(v1.1, R15)** A complementary workflow subscribed to `publication.withdrawn` and `publication.updated` (when visibility stops being public) removes the asset from the public library. Without it, a video taken down from YouTube would remain listed.

---

## 9. Security and credential management

### 9.1 Secrets and keyring [DECIDED]

- Credentials **must be in the operating system keyring**, and may reside on the **client or server**.
- macOS: Keychain; Windows: Credential Manager; Linux desktop: Secret Service/KWallet/GNOME Keyring; Linux headless: explicitly provisioned secure Secret Service.
- **No** silent fallback to a plaintext file on a server without a keyring; if the keyring is not configured, fail with a clear error or require secure provisioning.
- Workflow JSONs and configurations contain only `credentialsRef` references, never tokens, secrets, or passwords.
- `client`: secret stored in the user's keyring; may be unavailable to a disconnected remote daemon.
- `server`: secret stored in the daemon's keyring, preferred for long autonomous jobs.
- `delegated`: temporary, minimum-scope credential, sent to the server over an authenticated and encrypted channel, with consent/authorization; limited duration and storage.
- Secrets do not appear in CLI arguments, logs, traces, events, snapshots, manifests, exported files, or error messages. **(v1.1, R36)** For this reason `vidyom secret add` reads the value through a no-echo prompt or through stdin, never through an argument or an environment variable shown in the history. OAuth flows (YouTube) are done through the browser, and the resulting token goes directly to the keyring of the chosen destination.
- **(v1.1, R37)** Jobs that run without a connected client (publication after approval, events, schedules) **can only use valid `server` or `delegated` credentials**. The validator (`--dry-run`) warns when a workflow with an event or schedule trigger references a `client` credential. The example in §11.5 was adjusted: `youtube-oauth` became `--location server`, because publication happens after approval, possibly without the CLI connected.
- Rotation, revocation, access tests, and usage auditing are requirements.

### 9.2 Communication protection [DECIDED]

- Client-server communication **encrypted and authenticated**.
- Remote connection: SSH or TLS 1.3/HTTPS with certificate validation; mTLS when appropriate.
- Local Unix socket protected by filesystem permissions and process identity (it does not encrypt the traffic by itself).
- Authentication and **authorization per operation/asset**, regardless of VPN.
- **[PROPOSED]** Roles: `viewer`, `editor`, `publisher`, `admin`, plus granular permissions (`storage.archive.restore`, `asset.delete`, `plugin.install`, `secret.write`).

### 9.3 Private networks [DECIDED]

- Possibility of running **exclusively on a Tailscale, NetBird, or equivalent interface**, with no exposure to the public internet.
- The term "tailwind" mentioned during the discussion was interpreted as **Tailscale** in the context of overlay networking.
- Modes: explicit bind to a private VPN IP; or `127.0.0.1` with Tailscale Serve/tunnel/authenticated proxy.
- **No** fallback to `0.0.0.0` when the private interface does not exist; fail closed.
- **(v1.1, R38)** Decision 29 in §18 said "operation restricted to the Tailscale/NetBird interface", but this section (and the definition conversation itself) treats that as a **possibility**. Decision text corrected to "**possibility** of operation restricted…". The supported modes are: local socket only; private network (overlay); public TLS with authentication. In all of them, authentication and RBAC remain mandatory.
- Private-network firewall and access policies complement TLS, authentication, and RBAC; they do not replace them.

### 9.4 Plugin and workflow execution [DECIDED]

- Plugins with declared permissions for secrets, networks, and files.
- Verify origin, hashes, signatures when possible, and version compatibility.
- Imported JSON workflows do not automatically inherit the permissions of all connectors.
- No arbitrary shell by default; pass FFmpeg arguments in a safe/validated way, do not concatenate a command.
- Human approval before publishing or deleting, by default for publication; destructive actions with differentiated permission.
- Minimize exposure of private transcripts and sensitive content in AI logs and indexing.
- **(v1.1, R39)** Transcripts and audio of identifiable people are **personal data** under the LGPD (MapaDeva operates in Brazil) and equivalent regulations in other countries. Sending a transcript or audio to an external AI provider is processing by a third party and must be configurable **per asset or per collection** (`aiProcessing: local_only | external_allowed`). The **[PROPOSED]** default is `local_only` whenever a local engine is available. The topic is part of §17, item 6.

---

## 10. Logs, metrics, traces, auditing, and alerts

### 10.1 Model [DECIDED]

- Structured JSON logs with `timestamp`, `level`, `service`, `event`, `job_id`, `workflow_id`, `node_id`, `asset_id`, `trace_id`, duration, and status.
- Observability through the `Observability Manager`, with filters, secret redaction, buffering, batching, fan-out, and pluggable exporters.
- Distributed tracing across CLI, API, daemon, workflow, worker, and connector.
- Auditing separate from operational logs, durable and resistant to unauthorized changes.
- Local logs accessible through the CLI **even without Loki/Datadog/Grafana**.
- Unavailable destinations must not bring down FFmpeg or ASR; implement isolated queues and retries. Critical audit events cannot be silently dropped.

### 10.2 Initial stack [PROPOSED]

- Instrumentation: **OpenTelemetry**.
- Logs: **JSONL + Loki** initially; Datadog connector planned.
- Metrics: **Prometheus** initially.
- Traces: **OTLP / Tempo** initially.
- Dashboard and alerts: optional **Grafana**, separate from the daemon.
- Future connectors: Google Cloud Logging, Syslog, and others.

### 10.3 Minimum metrics

- Jobs `queued`, `running`, `waiting_approval`, `failed`, and `completed`.
- Execution time per node, workflow, plugin, and connector.
- Retries, failures, and connector latency.
- Transfer and storage per logical role/provider.
- Worker CPU, GPU, RAM, disk space, and queue.
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
| Auditing | >= 1 year, as required by applicable rules |
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
vidyom original search "conscious breathing"

vidyom clip create \
  --source ORI-001 \
  --prompt "The story of the 90-year-old woman who had no remorse" \
  --workflow create-clip

vidyom clip preview COR-001
vidyom clip approve COR-001
vidyom clip list --source ORI-001
vidyom asset lineage COR-001

# (v1.1, R40) generic approvals, adjustment, and withdrawal
vidyom approval list --status pending
vidyom approval show APR-001
vidyom approval approve APR-001 --start 00:10:48 --end 00:18:00
vidyom approval reject APR-001 --reason "incomplete excerpt"
vidyom asset versions COR-001
vidyom asset delete COR-001          # requires the asset.delete permission
```

**(v1.1, R40)** `clip approve COR-001` was not enough: the selection gate (R13) happens **before** the clip exists as an asset, so there is no `COR-nnn` to approve yet. Gates are now handled by `approval …` with `APR-nnn`; `clip approve COR-nnn` remains as a shortcut for the pending publication approval of that clip.

### 11.3 Workflows, nodes, jobs

```bash
vidyom workflow list
vidyom workflow validate ./workflows/create-clip.json
vidyom workflow run ./workflows/create-clip.json \
  --input source_id=ORI-001 \
  --input prompt="The case of the friend who never got a hug"
vidyom workflow run ingest-original --dry-run

vidyom node list
vidyom node describe video.cut
vidyom node run ai.transcribe --asset ORI-001

vidyom job list --status running
vidyom job status JOB-001
vidyom job attach JOB-001
vidyom job logs JOB-001 --follow
vidyom job retry JOB-001
vidyom job pause JOB-001
vidyom job resume JOB-001
vidyom job cancel JOB-001
```

**(v1.1, R22)** `workflow resume JOB-001` became `job resume JOB-001`: what is resumed is an execution, not the definition of a workflow. `job pause` was added, since the `paused` state existed without a command to reach it.

### 11.4 Connectors, storage, and publications

```bash
vidyom connector list
vidyom storage list
vidyom storage test master
vidyom storage verify archive --asset ORI-001
vidyom storage copy --from master --to another-storage --asset ORI-001
vidyom storage restore --asset ORI-001 --to master   # (v1.1) admin; storage.archive.restore; shows cost and asks for confirmation

vidyom publishing list
vidyom publish COR-001 --to youtube-main
vidyom publish COR-001 --to youtube-main,vimeo-main
vidyom publish status COR-001
vidyom publish sync COR-001
vidyom unpublish COR-001 --from youtube-main          # (v1.1, R15)
```

### 11.5 Credentials, security, observability, plugins, hooks

```bash
vidyom secret add gcs-main --location server          # value read via prompt/stdin (R36)
vidyom secret add youtube-oauth --location server     # (v1.1, R37) was "client"
vidyom secret add vimeo-oauth --location delegated --ttl 2h
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

The final UX must define uniform names (`connector`, `publishing`, `publish`) and an output format that supports JSON for scripts (`--json`). Viewing logs/attaching must not change the job state unless an explicit command is used.

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
- **(v1.1, R41)** Endpoints were missing for operations the CLI already had: `GET /search` (lexical+semantic search, with optional per-asset filter), `GET /assets` (listing/filters), `GET /assets/{id}/versions`, `GET /approvals` and `GET /approvals/{id}`, `POST /jobs/{id}/pause` and `POST /jobs/{id}/resume`, `DELETE /publications/{id}` (withdrawal), `GET /events` and `GET /events/deliveries`, `GET|POST|DELETE /secrets` (metadata only; the value is never returned), `GET /clients` and `POST /clients/{id}/revoke`, `GET /audit`.

Authentication, authorization, idempotency keys, and an audit trail are mandatory for mutations.

### 12.2 Original asset example

```json
{
  "id": "ORI-001",
  "kind": "original",
  "category": "yoga_class",
  "title": "Hatha Yoga Class",
  "visibility": "private",
  "status": "ready_for_clipping",
  "current_version": "v1",
  "master_available": true,
  "backup_verified": true,
  "catalog_sync": "synced",
  "duration_seconds": 3600,
  "language": "pt",
  "source_original": {
    "storage": "archive",
    "key": "originals/ORI-001/v1/original.mov",
    "sha256": "<real-hash-at-runtime>",
    "verified": true
  },
  "working_master": {
    "storage": "master",
    "key": "originals/ORI-001/v1/master.mp4",
    "sha256": "<real-hash-at-runtime>",
    "profile": "master-1080p@1",
    "verified": true
  },
  "proxy": {
    "storage": "proxy",
    "key": "originals/ORI-001/v1/proxy.mp4",
    "profile": "proxy-720p@1"
  },
  "transcript": {
    "format": "json+srt",
    "indexed": true,
    "timestamp_basis": "source_original",
    "engine": "faster-whisper/<model>",
    "embedding_model": "<model>@<version>"
  },
  "provenance": {
    "job_id": "JOB-001",
    "workflow": "ingest-original@1.0.0",
    "workflow_sha256": "<real-hash-at-runtime>"
  }
}
```

The string `<real-hash-at-runtime>` is a didactic placeholder, not a valid SHA-256. **(v1.1, R6, R8, R21, R30)** Compared with v1.0, version, state flags, profile, models, and provenance were added; keys now include the version; the raw file keeps its original extension (`.mov` in the example). The `location` field (R30) was omitted here to shorten the example.

### 12.3 Clip example

```json
{
  "id": "COR-001",
  "kind": "clip",
  "title": "The 90-year-old woman who had no remorse",
  "parent_asset_id": "ORI-001",
  "current_version": "v1",
  "lineage": {
    "method": "computed",
    "source_asset_id": "ORI-001",
    "root_asset_id": "ORI-001",
    "selection_approval_id": "APR-001",
    "segments": [
      {
        "source_asset_id": "ORI-001",
        "source_version": "v1",
        "start_seconds": 648.0,
        "end_seconds": 1080.0
      }
    ]
  },
  "visibility": "private",
  "status": "editorial_review",
  "master": {
    "storage": "master",
    "key": "clips/COR-001/v1/master.mp4"
  },
  "backup": {
    "storage": "archive",
    "key": "clips/COR-001/v1/master.mp4",
    "verified": true
  },
  "publications": []
}
```

The timestamps above are examples; in a real run they will be computed by the indexing. **(v1.1, R12, R42)** Each segment now indicates its source and the source's version (preparing for assembly from multiple sources) and accepts fractions of a second, because clips cut at a sentence boundary need more precision than whole seconds. The clip's backup key changed from `original.mp4` to `master.mp4`: what goes to the Archive is the **clip's master**, and calling it "original" was confusing with the raw file of an `original`.

### 12.4 Independent publications example

```json
{
  "asset_id": "COR-001",
  "publications": [
    {
      "id": "PUB-001",
      "destination": "youtube-main",
      "asset_version": "v1",
      "published_file_sha256": "<real-hash-at-runtime>",
      "remote_id": "youtube-id-illustrative",
      "url": "<url-returned-by-api>",
      "status": "published",
      "visibility": "public",
      "approval_id": "APR-002"
    },
    {
      "id": "PUB-002",
      "destination": "vimeo-main",
      "asset_version": "v1",
      "published_file_sha256": "<real-hash-at-runtime>",
      "remote_id": "vimeo-id-illustrative",
      "url": "<url-returned-by-api>",
      "status": "published",
      "visibility": "public",
      "approval_id": "APR-002"
    }
  ]
}
```

Fill in URLs only with confirmed API responses. Do not invent publication links. **(v1.1, R43)** In v1.0, the examples had `status: "published"` with `url: null`, a combination that must not exist: `published` is only written after the destination returns an ID and URL. While there is no URL, the state is `processing`. Publication states **[PROPOSED]:** `pending_approval`, `uploading`, `processing`, `published`, `failed`, `withdrawn`. Each publication records the asset version, the hash of the uploaded file (R4), and the approval that authorized it.

### 12.5 Collection portability [DECIDED]

**[PROPOSED]** Each asset can be exported as a metadata package (without needing to export the masters on every query):

```text
ORI-001/
├── metadata.json
├── summary.md
├── transcript.json
├── transcript.srt
├── chapters.json
├── thumbnail.jpg
├── preview.mp4            # optional
├── lineage.json           # (v1.1) lineage and provenance (R21)
├── versions.json          # (v1.1) versions and checksums (R6)
└── publications.json
```

- Export/import indexes and documents in a readable form; do not depend on MediaCMS's proprietary database.
- The canonical catalog is independent of any external UI.
- Ensure UTF-8, explicit timestamps, hashes, and origin metadata.

---

## 13. Private catalog and public library

### 13.1 Private [DECIDED]

- **MediaCMS**: private visual interface, proxy videos, tags/categories/collections, browsing, and API integration when compatible.
- **VideoVortex (`vvx`) [PROPOSED AND PREFERRED, see R33]**: tool for indexing, text search, and segment/clip operations, with a CLI and AI integration, according to the validated capabilities of the deployed version.
- **PostgreSQL + pgvector [PROPOSED]**: canonical source and semantic search (embeddings) + full-text search.
- **Rewind [PROPOSED: optional]**: visual editing/review of transcripts and clips.
- **Arcadeia [PROPOSED: not a priority]**: evaluated; not a recommended component of this architecture.
- **(v1.1, R11)** Synchronization failures with MediaCMS/VideoVortex are recorded in `catalog_sync`, with retry; they block neither readiness nor publication.
- Verify how MediaCMS receives or references an already processed proxy; avoid duplicating transcoding and storage unnecessarily.
- MediaCMS and VideoVortex may maintain their own indexes; Vidyom must synchronize through stable IDs and prevent either product from becoming the sole source of truth.

### 13.2 Public [DECIDED]

- Separate public library (proposed technology: **Next.js**).
- Brings together only **published and approved** assets, mainly edited videos and clips.
- Embedded player or link to YouTube, Vimeo, or other destinations.
- Search by topic, instructor, chapter, story, and spoken content.
- Timestamps and video position links when supported by the platform.
- May display relationships between clips and the source recording without exposing the private original.
- Full transcripts or excerpts only if reviewed and explicitly authorized for publication.
- No automatic public synchronization for assets without an approved/published state.

**(v1.1, R44) [PROPOSED] How the public library obtains its data.** v1.0 said that the Next.js library "consumes Vidyom APIs" and, at the same time, that the daemon may be restricted to a private network (§9.3), with no access from the internet. The two are incompatible if the library calls the daemon directly. Resolution:

- The public library **never** accesses `vidyomd`. The `catalog.public.sync` node **pushes** an already filtered public dataset to its own destination: a bucket with JSON/static files or a separate read-only database, configured as a `PublicCatalogConnector` connector.
- This dataset contains only released fields (title, description, chapters, tags, links/embeds, authorized transcript excerpts) and **no** storage path, internal technical ID, bucket key, or data from the private original.
- Public semantic search uses embeddings exported in this same dataset (only for published content) or the library's own index. It never queries the private index.
- Withdrawals (R15) remove the item from this dataset idempotently.

---

## 14. File and repository organization

### 14.1 Logical storage layout

```text
gs://videos-archive/                       # archive role
  originals/
    ORI-001/v1/original.<ext>              # raw, extension preserved
  clips/
    COR-001/v1/master.mp4                  # clip master
  edited/
    EDT-001/v1/master.<ext>

gs://videos-standard/masters/              # master role
  originals/
    ORI-001/v1/master.mp4
  clips/
    COR-001/v1/master.mp4
    COR-001/v1/published-youtube-main.mp4  # only if different from the master (R4)

gs://videos-standard/proxies/              # proxy role
  originals/
    ORI-001/v1/proxy.mp4
    ORI-001/v1/thumbnail.jpg
  clips/
    COR-001/v1/proxy.mp4
```

**These paths are initial conventions of the GCS connector**. Workflows must reference the logical roles `archive`, `master`, `proxy`, not buckets directly.

**(v1.1, R4, R6, R28, R42) Corrections relative to v1.0:** the v1.0 layout put the proxy in `videos-standard/originals/…`, without the `proxies/` prefix that the §7.2 configuration itself defined; keys had no version (a new render would overwrite the previous file); the clip backup was named `original.mp4`; there was no place for `edited` assets; and the published file was duplicated even when identical to the master. Keys inside a role (`originals/…`) are relative to the role's prefix.

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
│   ├── approval/                # (v1.1, R25)
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
│   ├── catalog/public/          # (v1.1, R44) destination of the public dataset
│   ├── transcription/faster-whisper/
│   └── ai/
├── plugins/
├── workflows/
│   ├── ingest-original.json
│   ├── ingest-derived.json
│   ├── create-clip.json
│   ├── publish-video.json
│   └── unpublish-video.json      # (v1.1, R15)
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

### 15.1 E2E scenario — original

**Given** a 10 GB original and a title/category, **when** sent by the remote CLI to the daemon, **then**:

1. the upload is resumable and verified by hash;
2. the raw backup in `archive` is validated;
3. the working master and proxy are generated and stored in `master`/`proxy`;
4. the transcript and chapters are searchable with timestamps;
5. media and metadata appear in MediaCMS or a compatible interface;
6. the job is observable through the CLI even after disconnection/reconnection;
7. the asset only becomes `ready_for_clipping` when the mandatory conditions are met;
8. **(v1.1)** the duration of the master, the proxy, and the ASR audio matches that of the raw file within tolerance (R10);
9. **(v1.1)** with MediaCMS down, the asset still reaches `ready_for_clipping` and is left with `catalog_sync = failed` (R11).

### 15.2 E2E scenario — two clips on different days

**Given** `ORI-001` already indexed, **when** the prompts "Story of the 90-year-old woman who had no remorse" and, days later, "The case of the friend who never got a hug" are submitted, **then**:

1. the `ORI-001` index finds relevant candidates without exact words;
2. the AI proposes the start and end of each narrative, with review;
3. a worker downloads only the **working master**, never the raw file from the Archive;
4. two new assets, `COR-001` and `COR-002`, are created, with correct lineage;
5. each clip receives a backup, working master, proxy, derived/reviewed transcript, and index;
6. each clip can be published independently and appear in the public library after approval;
7. no step of Workflow A for `ORI-001` needs to run again;
8. **(v1.1)** IDs are allocated without collision even if the two requests run at the same time (R5). The numbers `COR-001`/`COR-002` are illustrative: if `COR-001` already exists (as in the example in §2.4), the new clips receive the next free numbers;
9. **(v1.1)** an adjustment of the start/end after rendering produces `v2` of the clip without overwriting `v1` (R6).

### 15.3 Scenario — YouTube as the source

- An existing own video that is only on YouTube is imported through an authorized channel and stored in the operational storage.
- The transcript and clips are made from the persistent local/operational copy.
- After ingestion, creating a clip does not query YouTube or the Archive to obtain the video.

### 15.4 Scenario — pluggable storage

- Switch the logical `master` configuration from GCS to a test S3 connector **without changing the workflow**.
- Before the switch in production, migrate the data and validate checksums/versions.
- A driver without direct read or range support declares the limitation; the workflow fails in an explainable way when it requires an incompatible capability.

### 15.5 Scenario — resumption and idempotency

- Kill/restart the daemon during FFmpeg/upload; the job returns to a coherent state, with a checkpoint and without duplicate publication.
- A CLI failure does not interrupt the job.
- Two equivalent calls with the same idempotency key do not generate two uploads/publications.

### 15.6 Scenario — security

- No secret in the JSON, arguments, logs, traces, and exported files.
- A daemon configured for Tailscale/NetBird does not start a public listener if the interface fails.
- An unauthorized client does not install a plugin, restore the Archive, delete assets, or publish.
- Headless Linux without a provisioned keyring fails explicitly, with no silent plaintext.
- **(v1.1)** `server:` with `..` or a symbolic link pointing outside `ingestRoots` is rejected (R20).
- **(v1.1)** The public dataset contains no storage keys, internal IDs, unauthorized transcripts, or data from private originals (R44).
- **(v1.1)** `--dry-run` warns when an event-triggered workflow uses a `client` credential (R37).

### 15.8 Scenario — publication withdrawal (v1.1, R15)

- `vidyom unpublish COR-001 --from youtube-main` makes the video private or removes it at the destination, marks the publication as `withdrawn`, emits `publication.withdrawn`, and removes the clip from the public library.
- Repeating the command produces no error or duplicate effect.

### 15.7 Scenario — hooks and observability

- A `before` hook can reject an invalid source; the error is audited.
- A `finish` hook records the end on success as well as on error/cancellation.
- Loki unavailable: jobs continue, events are buffered with limits and a failure record; Datadog can keep receiving.
- The `publication.completed` event triggers synchronization of the public library, with loop prevention and idempotency.

---

## 16. Suggested implementation roadmap

**The stages below are proposals, not contracted deadlines.** The design must guarantee future compatibility without requiring all plugins to be implemented in the first release.

### Phase 0 — Contracts and integration prototypes

- Choose the open source license, contribution policy, and repository name.
- Define the JSON Schema for workflows, plugins, connectors, and job states.
- MediaCMS PoC with a pre-processed proxy and metadata synchronization.
- VideoVortex PoC with transcription, search, and clipping, verifying version/license capabilities and restrictions.
- FFmpeg PoC for the working master from the raw file and a real clip test.
- Validate integration with GCS, headless Linux keyring, and the private Tailscale/NetBird mode.

### Phase 1 — Functional core and ingestion

- Local/remote CLI and daemon, authentication, and durable jobs.
- Minimal JSON workflow, `storage.put/get/verify`, `video.transcode`, `audio.extract`, `ai.transcribe`, `ai.index`.
- GCS connector with `archive/master/proxy` and generation of portable metadata.
- JSONL logs and job status/logs API.
- Complete `ingest-original.json` with `ready_for_clipping`.

### Phase 2 — Semantic clips and subworkflows

- Lexical+vector retrieval, identification of a complete narrative.
- Preview, review, timestamp adjustment, and rendering with FFmpeg.
- `create-clip.json` and `ingest-derived.json` with lineage, backup, and reindexing.
- Ensure that no clip reads from the Archive.
- **(v1.1)** `approval.request` node, `approval …` commands, asset versions, and validation of disjoint namespaces (R6, R25, R28).

### Phase 3 — Publishing and public catalog

- YouTube connector; OAuth in the keyring; resumable upload, status, thumbnails, captions.
- Persistent approval; idempotent publishing; published files in the operational storage.
- Next.js library with search and access only to approved assets.
- **(v1.1)** Public dataset sent by push (R44) and `unpublish-video.json` with removal from the library (R15).

### Phase 4 — Plugins, hooks, and full observability

- Isolated plugin runtime, permissions manifest, registry.
- Global/workflow/node hooks; persistent Event Bus.
- OpenTelemetry, Loki, Prometheus, optional Tempo/Grafana, and sink interface.
- Future connectors: Vimeo, S3, Dropbox, SFTP, Datadog, etc.

---

## 17. Pending decisions and mandatory validations

The definition conversation set out the architecture and the high-level interfaces, but some points **have not yet been explicitly decided** and must not be silently filled in by the team:

1. **Vidyom license:** MIT, Apache-2.0, AGPL, or another; check dependency compatibility, especially integrated copyleft products.
2. **Deployment model:** initial Docker Compose/systemd; Kubernetes and multiworker if needed.
3. **Exact working master policy:** codec, bitrate/CRF, and resolution per class of original (1080p/4K), target size, and retention.
4. **Which embeddings engine/LLM and which ASR models** to use in each environment; offline requirements and costs.
5. **Approval policy:** levels, user/editor, publication default, and review flows for videos that are already public.
6. **Copyright and privacy policies:** permissions for speakers and participants; content withdrawal, sensitive transcripts, and publication authorizations.
7. **Final semantics of retry, cancellation, and checkpoint** for each node type, especially external and non-idempotent ones.
8. **Definitive plugin protocol:** gRPC, JSON-RPC, HTTP, or a combination.
9. **Final format of workflow expressions** and merge and branching rules.
10. **Multi-user and multi-tenant:** whether needed from the MVP or at a later stage.
11. **Secrets on headless Linux:** keyring unlock strategy at boot without reducing security.
12. **Retention/security policy for the local cache** for masters and temporary files.
13. **MediaCMS/VideoVortex synchronization model**, failure handling, and metadata export.
14. **Cost limits and policies** for GCS transfers, transcription, and publishing.
15. **Publishing API compatibility**: policies, OAuth, limits, uploads, and exact metadata for each platform.
16. **Availability and names of `vidyom` packages/identifiers** in registries, domain, and repositories, as well as trademark verification.

**Items added in v1.1** (gaps found in the review; none should be decided silently):

17. **Segmentation strategy for embeddings** (sentence, overlapping window, chapter) and reindexing policy when switching models (R18). Complements item 4.
18. **Archive version policy** (`archivePolicy`, R29) and what to do with versions of clips that were never published.
19. **Deletion and withdrawal policy** (R15, R16): who can do it, timeframes, what happens to backups under minimum retention, and how to handle removal requests (LGPD/image rights). Complements item 6.
20. **Processing by external AI** (R39): default `local_only` or `external_allowed`, per asset or collection, and recording of consent.
21. **Client pairing flow** (R19): mTLS or token, validity, renewal, and recovery in case of device loss.
22. **Format and hosting of the public dataset** (R44): static JSON, a separate read-only database, or the library's own API; and how to do public semantic search.
23. **Timeline alignment tolerance** (R10) and how to handle sources with broken timestamps or variable frame rate.
24. **Portuguese ID prefixes** (`COR-`, `ORI-`) in an international project (R5): keep or allow configuration. For now they are kept, by decision of origin.

---

## 18. Consolidated log of decisions from the definition conversation

This section is a verification index so the team does not lose decisions.

| # | Decision | Status |
|---|---|---|
| 1 | Official name **Vidyom** | DECIDED |
| 2 | Project initially for **MapaDeva**, but **open source** and general-purpose | DECIDED |
| 3 | Asset type/prefix **`originals`**, not `lectures`; "lecture" becomes just a content category (`lecture`) (v1.1, R1) | DECIDED |
| 4 | Content: yoga classes, courses, meetings, talks, interviews, etc. | DECIDED |
| 5 | Preserve the raw file in backup and use a compressed working master | DECIDED |
| 6 | Initial backup in **GCS Archive** | DECIDED |
| 7 | Initial working master in **GCS Standard** | DECIDED |
| 8 | Lightweight operational video proxy and dedicated audio for AI | DECIDED |
| 9 | Avoid Archive throughout the clip flow | DECIDED |
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
| 23 | Configurable and reusable subworkflows | DECIDED |
| 24 | `vidyom` client CLI + persistent `vidyomd` daemon | DECIDED |
| 25 | Separate `vidyom-worker` worker, initially co-located | PROPOSED |
| 26 | Persistent session architecture similar to Herdr | DECIDED |
| 27 | Encrypted and authenticated local/remote communication | DECIDED |
| 28 | Credentials in the OS keyring, client **or** server | DECIDED |
| 29 | **Possibility** of operation restricted to the **Tailscale/NetBird** interface (v1.1, R38) | DECIDED |
| 30 | Logs, metrics, traces, auditing, alerts, and OTel | DECIDED |
| 31 | Pluggable Storage connectors: GCS first; S3/Dropbox/SFTP, etc. | DECIDED |
| 32 | Pluggable Observability connectors: Loki/Datadog, etc. | DECIDED |
| 33 | Pluggable Publishing connectors: YouTube/Vimeo/etc. | DECIDED |
| 34 | An asset can be published to multiple destinations | DECIDED |
| 35 | `before`, `after`, `finish` hooks, error hooks, and per-node hooks | DECIDED |
| 36 | Plugins with permissions and persistent asynchronous events | DECIDED |
| 37 | Extensible architecture for catalog, ASR, and AI providers | DECIDED |
| 38 | Metadata portability/export and import in open formats | DECIDED |
| 39 | Go in the core and Python/FFmpeg in the workers as needed | PROPOSED |
| 40 | PostgreSQL + pgvector as the canonical catalog, jobs, and search | PROPOSED |
| 41 | (v1.1) Verified backup is part of `ready_for_clipping`; synchronization with MediaCMS does not block (R11) | PROPOSED (resolves inconsistency) |
| 42 | (v1.1) Published file kept in the `master` role (R4) | PROPOSED (resolves inconsistency) |
| 43 | (v1.1) Canonical `publish-video` publishing workflow with `destinations` (R14) | PROPOSED (resolves inconsistency) |
| 44 | (v1.1) Two approval gates: selection and publication (R13, R25) | PROPOSED |
| 45 | (v1.1) Immutable asset versions; storage keys with version (R6) | PROPOSED |
| 46 | (v1.1) Public library receives data by push and never accesses the daemon (R44) | PROPOSED |
| 47 | (v1.1) Storage roles with disjoint namespaces (R28) | PROPOSED |
| 48 | (v1.1) Publication withdrawal (`unpublish-video`) and controlled deletion (R15, R16) | PROPOSED |

---

## 19. Expected outcome

Vidyom must allow any user to install a daemon on their own server, configure storages and publishing platforms with securely stored credentials, control all the work through the CLI, and define media pipelines in JSON.

A 10 GB recording can be archived, converted, transcribed, summarized, and indexed **once**. From it, the user can request clips by topic or story at any future time. The daemon will locate the segment, retrieve **only the working master** from the accessible storage, render a new video, create another complete asset with backup and metadata, make it available for review, and publish it to the selected destinations. All of this with logs, auditing, hooks, plugins, and connectors that are independent of any vendor.

**This is the product and architecture contract from which the implementation of Vidyom must be planned, broken down into issues, and validated by acceptance tests.**

---

## 20. Revision log v1.1

Consistency and gap review performed on v1.0 (`spec/vidyom-specification-v1.0.md`, preserved unchanged). Types: **I** = inconsistency between parts of the document; **L** = gap (something necessary that was not specified); **C** = wording clarification.

| # | Type | Where | Problem in v1.0 | Fix in v1.1 |
|---|---|---|---|---|
| R1 | C | §2.1, §18 #3 | "The `originals` category" mixed asset type, storage prefix, and content category (§2.2) | The three concepts were separated; "lecture" became just a content category (`lecture`) |
| R2 | L | §2.1 | `edited` defined with no entry flow | Enters through `ingest-derived` with `declared` lineage |
| R3 | L | §2.1 | Temporary ASR audio, with no regeneration rule | Regenerated from the master, never from the Archive |
| R4 | I | §2.1, §3.4, §7.2, §7.3, §14.1 | The published file was sometimes in `proxy`, sometimes in the "operational" storage, sometimes alongside the master | Lives in `master`; not duplicated when identical to the master |
| R5 | L | §2.3 | ID prefix for `edited`/approval/publication, format, and concurrency not defined | Prefixes, uncapped numbering, and transactional allocation |
| R6 | L | §1.3, §2.3, §14.1 | "Multiple versions" with no model; keys without version (overwriting) | Immutable `vN` versions; keys with version |
| R7 | L | §2.1 | Clip of a clip not addressed | Allowed; rendering from the root original when possible |
| R8 | L | §2, §5.5 | Asset states used but not defined | State and flag table (§2.5) |
| R9 | L | §3.1 | When to delete the input copy not defined | Only after archive and master are verified |
| R10 | L | §3.1, §12.2 | Nothing guaranteed that transcript timestamps were valid for the master and proxy | Same timeline, validated by `ffprobe` |
| R11 | I | §3.1, §13.1, §15.1 | Readiness condition omitted backup and proxy; MediaCMS was implicitly blocking, contradicting "no product is the sole source" | Consolidated condition; catalog synchronization does not block |
| R12 | I | §2.1, §3.2 | Clip from "one or more" assets × `create-clip` with a single source | MVP with a single source; multiple sources as [FUTURE]; per-segment lineage |
| R13 | C | §3.2 | Two approvals (steps 7 and 13) with no distinction | `clip_selection` and `publication` gates; overlap warning |
| R14 | I | §3.2, §3.3 | `publish-video` × `publish-youtube` | Canonical `publish-video` with `destinations` |
| R15 | L | §3.3, §8.2, §13.2 | No withdrawal flow; library did not remove items | `unpublish-video`, `publication.withdrawn` event, `unpublish` command |
| R16 | L | §3.3, §9.2 | `asset.delete` permission with no flow | Controlled `delete-asset.json` |
| R17 | L | §4.1 | No rule against upscaling resolution; master audio not specified | Resolution ceiling, high-quality audio, versioned profiles |
| R18 | L | §4.4, §17 #4 | Embeddings model and segmentation not versioned | Vectors with model/version; reindexing mandatory on model change |
| R19 | L | §5.3, §9.2, §11.5 | Authentication required with no client registration flow | Proposed pairing, local `SO_PEERCRED`, `CLIENT-nnn` |
| R20 | I/L | §5.4, §11.2 | `original import <file>` without saying whether the file is on the client or the server; no path limit | `server:`/`upload:`/`storage://`/`import:` URIs; `ingestRoots` |
| R21 | I | §5.5, §6.5, §10.4 | Jobs retained for 90 days × permanent lineage | Permanent provenance summary on the asset |
| R22 | I | §5.5, §11.3 | `paused` state with no command; `workflow resume JOB-…` acting on a job | `job pause`/`job resume` |
| R23 | C | §11 | `JOB-001` with no ID rule | Same rule as R5 |
| R24 | L | §6.2 | Reference to a non-ancestor node and `${` escape not defined | Ancestry validation; `$${` |
| R25 | L | §6.2, §7.3 | `requireApproval`/`waiting_approval` with no node implementing them | `approval.request` node |
| R26 | I | §6.3 | Example without `audio.extract`, with a fixed `.mp4` and MediaCMS before readiness | Differences listed for the implementation |
| R27 | I | §6.4 | Clip example without preview/approval; lineage lost (`parentId` only) | Example rewritten |
| R28 | I | §6.4, §7.2 | `master` without prefix and `proxy` in `proxies/` in the same bucket: proxy "inside" the master, preventing role validation | `masters/` prefix; disjoint namespaces mandatory |
| R29 | L | §7.2 | Minimum duration and retrieval cost of the ARCHIVE class ignored for clip versions | `archivePolicy`; [VALIDATE] |
| R30 | L | §7.1, §15.4 | Registering by role alone lost the physical location after a migration | Immutable `location` field |
| R31 | I | §7.3, §12.4 | Default `private` visibility × approval "before public publication" with no defined moment | Approval before upload; option of prior private upload |
| R32 | C | §7.4 | `events.jsonl` log could be confused with the Event Bus | `vidyomd.jsonl` |
| R33 | I | §7.5, §13.1, §18 | VideoVortex under a [DECIDED] heading × "PROPOSED AND PREFERRED" in §18; licenses not evaluated | Statuses aligned; [VALIDATE] licenses |
| R34 | I/L | §8.1 | Short hook keys × `workflow.*` names; hook × event; examples without the mandatory timeout | Mapping, distinction, and default timeout |
| R35 | L | §8.2 | Events without semantics (`original.indexed` × `asset.indexed`) and without an envelope; loop prevention without a mechanism | Semantics table, envelope, `causation_id` |
| R36 | L | §9.1, §11.5 | How `secret add` receives the value without exposing it | No-echo prompt or stdin; OAuth through the browser |
| R37 | I | §9.1, §11.5 | `youtube-oauth --location client` × autonomous publishing after approval | `server`; warning in `--dry-run` |
| R38 | I | §9.3, §18 #29 | "Possibility" became "restricted operation" in the table | Wording corrected |
| R39 | L | §9.4, §17 #6 | LGPD and sending to external AI not addressed | `aiProcessing` per asset or collection |
| R40 | I | §11.2, §12.1 | `clip approve COR-…` before the clip exists; no approval commands | `approval …`, `asset versions`, `asset delete` |
| R41 | L | §12.1 | API without search, approvals, events, secrets, clients, auditing | Endpoints added |
| R42 | I | §12.3, §14.1 | Clip backup named `original.mp4`; segments without a source and only in whole seconds | `master.mp4`; segment with source/version; fractions of a second |
| R43 | I | §12.4 | `status: published` with `url: null` | `published` only with a URL; publication states |
| R44 | I | §7.5, §9.3, §13.2 | Public library "consumes the API" × daemon restricted to the private network | Push of a filtered public dataset; the library never accesses the daemon |

**New sections:** §2.5 (asset states), §15.8 (publication withdrawal), items 17–24 of §17, decisions 41–48 of §18, and this §20.
