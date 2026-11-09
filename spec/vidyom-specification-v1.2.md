# Vidyom — Complete Project Specification

> **Open-source AI-powered video workflow engine**  
> **Preserve · Discover · Transform · Share**  
> **Foundation document for architecture, product, and implementation**  
> Document version: **1.2 — 2026-11-08** (maintainer decisions on the pending items of v1.1; see **§21 Revision log v1.2**)  
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

In revision v1.2, passages changed by the maintainer's decisions of 2026-11-08 are marked **(v1.2, Dn)**, where `Dn` refers to the corresponding item in §21, and those decisions are [DECIDED] unless marked otherwise.

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

- **[DECIDED]** A permanent identifier for each asset: examples `ORI-001`, `CLIP-001` **(v1.2, D1)**.
- **[DECIDED]** Each clip stores its source asset(s), time intervals, versions, and history.
- **[DECIDED]** An original talk/class can generate N clips across multiple production moments, without rerunning the original ingestion.
- **[DECIDED]** An asset can be published to several destinations, each with its own `remote_id`, URL, metadata, and state.
- **[PROPOSED]** Use UUID/ULID as the immutable technical key and friendly IDs as human aliases.
- **(v1.1, R5) [PROPOSED] Friendly ID prefixes and format:** `ORI-` (original), `CLIP-` (clip), `EDT-` (edited), `JOB-` (execution), `APR-` (approval), `PUB-` (publication), `CLIENT-` (registered client). Sequential numbering per prefix, with at least 3 digits and no upper limit (`ORI-1000` is valid). Allocation is transactional in the canonical catalog (the tenant database, **(v1.2, D5, D6)**), so that two clips created at the same time never receive the same ID. A friendly ID is never reused, even after deletion. The API always accepts and returns both the technical ID and the alias. **(v1.2, D1) [DECIDED]** v1.2 adopted English prefixes: `CLIP-` replaces `COR-`, and the other prefixes are unchanged. The immutable technical key remains a ULID and friendly IDs are aliases. Storage paths follow the prefix (`clips/CLIP-001/v1/master.mp4`).
- **(v1.1, R6) [PROPOSED] Asset versions:** each asset has immutable versions `v1`, `v2`…. A new render (e.g., an adjustment to start/end after approval), a new transcode of the master, or a file replacement creates a new version; editing only metadata (title, tags) does not create a media version, but is recorded in the history. Files of a version are never overwritten; storage keys include the version (§14.1). Publications record which version was sent.
- **(v1.1, R7) Clips of clips:** since the definition of `clip` allows deriving from “another asset”, a derivative that reaches `ready_for_clipping` can be the source of a new clip. **[PROPOSED]** To avoid generation loss, the intervals are converted to the timeline of the root original and rendered from the original's master, when it exists and the edit is continuous; the lineage records the full chain.

### 2.4 Lineage example

```text
ORI-001 — Yoga Class (indexed original)
├── CLIP-001 — Explanation of Ujjayi breathing
│   ├── YouTube publication
│   └── Vimeo publication
├── CLIP-002 — Story of the 90-year-old woman
└── CLIP-003 — Case of the friend who never got a hug
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
5. Generate, with FFmpeg, a **compressed working master** that is nonetheless suitable for future clips. **(v1.1, R10)** The master, the proxy, and the ASR audio must keep the **same timeline** as the raw file (no trimming of start/end, same zero instant, normalized timestamps). After generating each one, `ffprobe` compares its duration with that of the raw file and rejects a difference above max(100 ms, 1 frame) **(v1.2, D21)**. Sources with variable frame rate or broken timestamps are normalized to constant frame rate in the master before transcription, and transcripts use the master's timeline **(v1.2, D21)**. This is what allows the transcript timestamps to be used directly on the master and the proxy.
6. Send and verify the master in the `master` role (initially GCS Standard).
7. Generate a lightweight proxy in the `proxy` role (initially GCS Standard).
8. Extract audio optimized for transcription **directly from the best-quality file available**, avoiding double audio compression.
9. Optionally use VAD (voice activity detection) to exclude long silences, preserving a timestamp map between the processed audio and the video.
10. Transcribe locally with **faster-whisper large-v3** **(v1.2, D9)**, with timestamps; store the raw transcript and open formats.
11. Generate summaries, chapters, topics/tags, story identification, and embeddings with AI. **(v1.2, D9, D15, D8)** Embeddings run locally with BGE-M3 over speech windows of 30–60 s cut at sentence boundaries (about 15% overlap), with chapter summaries embedded as a second level. Story and chapter detection uses an LLM through a single OpenAI-compatible driver; a `local_only` collection may only use an endpoint classified as `local` (otherwise story detection is unavailable and clip selection falls back to manual review). External processing requires `external_allowed` (D8).
12. Index text and vectors in the canonical catalog (**(v1.2, D5)** the tenant's SQLite database: FTS5 for text and sqlite-vec for vectors [VALIDATE: performance and maturity; no approximate index yet, so asset-scoped search is the main path and tenant-wide semantic search must be load-tested]) and, where compatible, in VideoVortex.
13. **(v1.2, D24)** In the MVP the canonical private catalog is the tenant database and review happens through the CLI with proxy links. Optionally, when the **MediaCMS** connector (one instance per tenant) is enabled, register/expose the asset there for teams that want a web UI.
14. Validate the existence/integrity of the working master and the indexes.
15. Only then set the **`ready_for_clipping`** state.
16. Emit events (`original.indexed`, `asset.indexed`, `workflow.completed`) and record the audit.

**Mandatory readiness condition:** a directly accessible working master; a transcript with timestamps; a valid search index; integrity checks. Without these, the asset is not a candidate for clipping.

**(v1.1, R11) Clarifications on readiness:**

- The list above did not include the backup, although step 4 and §15.1 require it. **The verified backup in `archive` becomes part of the readiness condition** (consistent with the decision “preserve the raw file in backup”).
- The required index is that of the **canonical catalog** (the tenant database **(v1.2, D5)**). Synchronization with MediaCMS (optional connector, step 13 **(v1.2, D24)**) and VideoVortex (step 12) **does not block** `ready_for_clipping`: failures there are recorded as `catalog_sync = failed`, with retry and an alert. This applies the rule in §13.1 that neither of these products is a single source of truth.
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
7. Obtain approval of the proposed clip or proceed under an explicit editorial approval policy. **(v1.1, R13)** This is the **selection gate** (`approval.kind = "clip_selection"`), distinct from the **publication gate** of step 13 (`approval.kind = "publication"`). The two are separate approval records, with an owner, date, and approved version. **(v1.2, D7)** An `editor` approves the selection; a `publisher` (or `admin`) approves publication (step 13). **[PROPOSED]** If a request generates intervals that substantially overlap an existing clip of the same source, the system warns and shows the existing clip before proceeding; it does not block.
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
- **(v1.1, R15) [PROPOSED]** `unpublish-video.json`: withdraws the publication from one or more destinations (or changes visibility to private), updates the record, and removes the asset from the public library (event `publication.withdrawn`). v1.0 provided no withdrawal path, but one is needed to honor removal requests (§17, item 6). **(v1.2, D17) [DECIDED]** Withdrawal (unpublishing from every destination and from the public library) is immediate and can be done by a `publisher` or `admin`.
- **(v1.1, R16) [PROPOSED]** `delete-asset.json`: controlled deletion (permission `asset.delete`), which requires withdrawing the publications first, respects the Archive's minimum retention (§7.2), and keeps in the audit a record that the asset existed. **(v1.2, D17) [DECIDED]** Full deletion is done by an `admin`; the archive copy is purged when the archive's minimum storage duration ends (365 days on GCS Archive) and stays inaccessible until then; the audit keeps the fact of the deletion without the content. [VALIDATE] Legal review under the LGPD of deferring the archive purge.

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
| Working master **(v1.2, D14)** | Sources up to 1080p: H.264 High, CRF 18, preset slow, max 1080p (`master-1080p`); sources of 2160p or more: HEVC, CRF 20, keeps 4K (`master-2160p-hevc`) | Clips, reframing, future rendering |
| Proxy | MP4 H.264 720p, lower quality | Player, previews, and navigation |
| Audio for ASR | Mono 16 kHz, Opus ~24–32 kbps, depending on supported engine | Efficient transcription |
| Transcript | Structured JSON + SRT/VTT + Markdown/text | Search, editing, export |

Do not downgrade the **sole** production master to 720p when 1080p clips, zoom, and vertical videos will be needed. **(v1.2, D14) [DECIDED]** Sources of 2160p or more keep 4K in HEVC, to allow vertical reframing and zoom.

**(v1.1, R17) Complementary profile rules:**

- **Never upscale:** the `master-1080p` profile produces at most 1080p; a 720p source produces a 720p master. The profile name indicates the ceiling, not a fixed value.
- **Master audio** in high quality (AAC 192 kbps **(v1.2, D14)**), because published clips inherit that audio. The table above specified only the video.
- Preserve the source's frame rate and color space, unless the profile says otherwise.
- Keep the source's timeline (R10), within max(100 ms, 1 frame) of the raw duration **(v1.2, D21)**.
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
- **(v1.1, R18) [PROPOSED] Vector index:** each vector stores the identifier and version of the embedding model and the segmentation strategy (`chunking`) used. Changing the model requires reindexing (`reindex-asset.json`) and cannot mix vectors from different models in the same search. **(v1.2, D15, D9) [DECIDED]** The segmentation strategy is now defined: speech windows of 30–60 s, cut at sentence boundaries, with about 15% overlap, each with start and end timestamps; chapter summaries are embedded as a second level. Embeddings run locally with BGE-M3 (multilingual). When the embedding model changes, reindexing runs in the background side by side (`embedding_model` stored per vector) and the new index replaces the old one only when complete.

---

## 5. Runtime architecture: CLI + daemon + workers

### 5.1 Components [DECIDED]

- **`vidyom`**: lightweight client CLI. Sends commands, uploads, and requests, follows logs/state, approves, queries the catalog, and publishes.
- **`vidyomd`**: permanent daemon, running in the background on a local or remote server. API, authentication, authorization, scheduler, workflow engine, job management, events, hooks, plugins, connectors, and state.
- **`vidyom-worker` [PROPOSED]**: runs heavy, isolatable work (FFmpeg, ASR, AI). It can start on the same host and later scale horizontally. **(v1.2, D5)** Workers never open database files; they talk to the daemon through its API or Redis (also for future distributed workers).
- **(v1.2, D5) [DECIDED] SQLite, one database file per tenant**: assets, jobs, checkpoints, transcripts, search indexes, audit, and that tenant's encrypted secrets; plus a global **`system.db`** (tenants, users, roles, device credentials, plugin registry, wrapped tenant data keys). Full-text search uses FTS5; vector search uses sqlite-vec [VALIDATE: performance and maturity; no approximate index yet, so asset-scoped search is the main path and tenant-wide semantic search must be load-tested]. Databases run in WAL mode and are continuously backed up with Litestream to the archive storage provider. **Only the daemon opens database files.** A repository layer keeps PostgreSQL possible later. This replaces “PostgreSQL + pgvector” (v1.1, proposed).
- **(v1.2, D4) [DECIDED] Redis**: job dispatch queue (e.g., Redis Streams with consumer groups), AOF persistence enabled; a required service. The tenant database remains the source of truth for job state, checkpoints, and the transactional outbox.
- **(v1.2, D6) [DECIDED] Multi-tenancy from the MVP**: each tenant (organization) has its own database file, storage prefix (optional dedicated bucket and credentials), secrets (tenant data key), users and roles, budgets, worker cache directory, and public library export. Users belong to one or more tenants, with roles per tenant.
- **(v1.2, D10) [DECIDED] Deployment**: Docker Compose is the supported MVP deployment (daemon, workers with FFmpeg and the Python AI worker packaged, and Redis). The Go binaries can still run natively under systemd, but that is not the primary path. Kubernetes is [FUTURE].
- **Operational storage and archive**: abstracted via connectors and logical roles.

### 5.2 Architectural reference

**[DECIDED]** An experience similar to **Herdr**: processes persist beyond the client session; clients connect, disconnect, and reconnect. Vidyom does not need to preserve pseudo-terminals: it preserves **jobs, checkpoints, intermediate files, and node state**.

### 5.3 Communication

- CLI and daemon on the same host: **Unix Domain Socket** on Unix/macOS [PROPOSED], with restricted permissions.
- Remote: [DECIDED: encryption and security; PROPOSED: mechanisms]. **(v1.2, D12) [DECIDED]** The remote API is **HTTPS (TLS 1.3)** with client pairing (R19); this replaces SSH as the default remote transport. The local Unix socket remains for same-host use. SSH tunneling remains available only for host operators. Binding only to a private interface (Tailscale/NetBird) remains supported, with the same fail-closed rules.
- Versioned API with JSON; event/log streaming via SSE, WebSocket, or gRPC streaming [PROPOSED, choose implementation].
- Multiple simultaneous clients can follow the same job.
- The CLI can run on macOS, Linux, and Windows; the daemon initially on Linux [PROPOSED].
- Processing and external secrets stay on the server; FFmpeg or Whisper is not needed in the CLI.
- **(v1.1, R19) [PROPOSED] Client identity:** v1.0 required authentication and mentioned `security sessions` / `security revoke CLIENT-001`, but did not say how a client registers. Proposal: on the local Unix socket, identify the user by the process credentials (`SO_PEERCRED`) and map them to a role. Remotely, `vidyom server add` performs a **pairing**. **(v1.2, D12) [DECIDED]** An admin generates a one-time pairing code; the CLI exchanges it for a **per-device credential** that is revocable and rotated, and that stays in the client's OS keyring. Each paired client becomes a revocable `CLIENT-nnn`. Pairing remains mandatory also for host operators who use SSH tunneling (the SSH identity is mapped to a Vidyom user).

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

Proposed states: `queued`, `running`, `waiting_approval`, `retrying`, `paused`, `completed`, `failed`, `cancelled`, and **(v1.2, D11) [DECIDED]** `needs_attention`.

- **(v1.2, D11) [DECIDED]** Every node type declares a **side-effect class**: `none` (pure; retried freely), `idempotent` (retried with its idempotency key), `reconcilable` (must query the remote state before any retry), `unsafe` (never retried automatically; the job moves to `needs_attention` until a person resolves it). Cancellation is cooperative between nodes: the running node finishes, or aborts if it supports aborting. A checkpoint is persisted after each completed node.
- **(v1.2, D4) [DECIDED]** On startup, the daemon reconciles jobs that are `queued` or `running` in the database but missing from Redis, and re-enqueues them.

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
- Plugins in an isolated process; do not depend on native Go `.so` plugins. **(v1.2, D3) [DECIDED]** Out-of-process plugins talk to the daemon over **JSON-RPC 2.0 on stdin/stdout** (the same transport model as LSP and MCP): language-neutral and without code generation. Remote plugins and services use JSON-RPC over authenticated HTTPS. WASM remains possible for specific sandboxed cases [FUTURE].

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
- **(v1.2, D2) [PROPOSED]** Merge and branching rules: a node runs when all of its required predecessors have completed. Nodes on a branch not taken by `flow.if` are marked `skipped`, and the skip propagates to their descendants unless they are joined through `flow.merge`. `flow.merge` has `mode: all | any` (`all` waits for every non-skipped input; `any` continues with the first completed input).
- `workflow.execute`: invokes a subworkflow with input and output isolation.
- `flow.if`, `flow.foreach`, `flow.wait`, `flow.merge` **(v1.2, D2)**: explicit control.
- `${inputs.foo}`, `${nodes.id.output.field}`, `${event.field}`: **restricted declarative** expression, with no arbitrary JavaScript or shell evaluation. **(v1.2, D2)** `${...}` is used **only for data references**.
- **(v1.2, D2)** Conditions (`flow.if`, event trigger filters, approval rules) are written in **CEL (Common Expression Language)**: side-effect free, not Turing-complete, with evaluation cost limits; the Go daemon evaluates it with cel-go. Example: `{"id": "publishIfApproved", "type": "flow.if", "parameters": {"condition": "nodes.review.output.decision == 'approved' && inputs.visibility == 'public'"}}`.
- **(v1.1, R24)** A `${nodes.X...}` reference in node Y **requires** X to be an ancestor of Y in `connections`; the validator rejects a reference to a node that is not an ancestor (in the §6.3 example, `backup` uses `register`, so `register → backup` is mandatory). To write `${` literally, use `$${`.
- **(v1.1, R25) [PROPOSED]** `approval.request` node (human gate): parameters `kind` (`clip_selection` | `publication` | other), `assetId`, `approvers` (role or users), `timeoutSeconds`, and `onTimeout` (`reject` | `fail` | `remind`). It puts the job in `waiting_approval` and creates an `APR-nnn` record. v1.0 mentioned `requireApproval` and `waiting_approval`, but had no node that implemented them.
  - **(v1.2, D7)** Two-level approval: kind `clip_selection` is approved by an `editor`; kind `publication` is approved by a `publisher` (or `admin`). Changing the visibility or the public metadata of an already public video requires a new publisher approval. A workflow can tighten the rule through `approvers`, never loosen it. Roles per tenant: `viewer`, `editor`, `publisher`, `admin`.
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

**Note:** this is a minimal example to explain the format; the full implementation must include the proxy, summaries, embedding generation, validations, `ready_for_clipping`, events, states, and checksums. Registration in MediaCMS may require integration specific to the installed version. **(v1.2, D24)** The canonical catalog is the tenant database; the `mediacms.register` node belongs to the optional MediaCMS connector (one instance per tenant) and appears only in workflows of tenants that enabled it. **(v1.2, D6)** Storage keys are relative to the tenant's storage prefix. **(v1.2, D14)** Profile `master-1080p` (H.264 High, CRF 18, preset slow, max 1080p) applies to sources up to 1080p; sources of 2160p or more use `master-2160p-hevc` (HEVC, CRF 20, 4K kept); audio is AAC 192 kbps.

**(v1.1, R26) Differences between the example and §3.1 that the implementation must resolve:**

- the `audio.extract` node (step 8) is missing between the source and `ai.transcribe`;
- the key `originals/<id>/original.mp4` hardcodes the `.mp4` extension, but the raw file may be MOV/MKV/MXF, etc. The key must preserve the original extension (`original.<ext>`) and the version (§14.1);
- the `catalog` node (MediaCMS, an optional connector **(v1.2, D24)**) must not be a precondition for `ready_for_clipping` (R11); the node that changes the state to `ready_for_clipping` is missing, and it must depend on `backup`, `store_master`, the proxy, and `index`.

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
        "approvers": ["editor"],
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

**`video.cut` node invariant:** the received URL must be validated as belonging to the logical role `master` (or a local cache derived from it); a silent fallback to `archive` is forbidden. **(v1.1, R28)** For this validation to be possible, roles must occupy **disjoint namespaces**: two roles cannot point to the same bucket with overlapping prefixes (in v1.0, `master` used `videos-standard` with no prefix and `proxy` used `videos-standard/proxies/`, meaning every proxy was also "inside" the master). The configuration validator rejects overlap (§7.2). The worker's local cache stores the role and the origin checksum, and `video.cut` checks both. **(v1.2, D22)** The cache is per tenant (LRU with a configurable maximum size and a 7-day idle expiry), the checksum is verified on every reuse, temporary files are deleted at the end of each job, and the cache is encrypted at rest with the tenant key.

### 6.5 Versioning and safe execution

- Save an **immutable snapshot** of the JSON version with each job.
- New versions of the flow do not change jobs in progress.
- Validate node names, dependencies, cycles, schemas, permissions, and connector capabilities.
- No strings that become shell commands through concatenation; do not expose arbitrary code evaluation by default.
- Define an explicit way to wait for approval and handle timeout.
- **(v1.2, D11)** Every node type declares a **side-effect class**: `none` (pure; retried freely), `idempotent` (retried with its idempotency key), `reconcilable` (must query the remote state before any retry), `unsafe` (never retried automatically; the job moves to the new state **`needs_attention`** until a person resolves it).
- **(v1.2, D11)** Cancellation is cooperative between nodes: the running node finishes, or aborts if it supports aborting. A checkpoint is persisted after each completed node.
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
| `archive` | GCS | ARCHIVE | Raw originals (always, permanently) and masters of clips/edited videos once their selection is approved (v1.2, D16) |
| `master` | GCS | STANDARD | Working masters of originals and derivatives, and published files (v1.1, R4); rejected or never-approved renders expire after 30 days, configurable per tenant (v1.2, D16) |
| `proxy` | GCS | STANDARD | Proxies, thumbnails, and previews |

**(v1.1, R4)** In v1.0, the table placed "published media" in `proxy`, while §3.4 and §7.3 required storing the published file in permanent operational storage and §14.1 placed it alongside the master. It was decided that the published file lives in `master`, which has the permanence guarantee; `proxy` may have a shorter retention policy.

**GCS Standard is not cost-free access:** there may be costs for storage, requests, and egress traffic. Avoid unnecessary egress; when possible, run the worker close to the bucket's region. Do not use Archive for operational reads.

**(v1.1, R29) [VALIDATE] Archive costs:** the GCS ARCHIVE class has a **minimum storage duration** (365 days, according to the current public documentation) with prorated charges for early deletion, plus a retrieval cost. Consequences for the design:

- backup copies of clips are only created **after** the selection gate (the §3.2 flow already guarantees this: rendering comes after step 7); discarded previews and renders never go to `archive`;
- each new media version (R6) creates a new object in Archive; it is recommended to back up only the versions **approved for publication** or the latest version, according to the `archivePolicy` policy (`all_versions` | `approved_versions` | `latest`, default **[PROPOSED]** `approved_versions` for derivatives and `all_versions` for originals);
- `delete-asset.json` (R16) reports the early-deletion cost before executing.
- **(v1.2, D16)** Raw originals are always archived, permanently (not subject to `archivePolicy`). The master of a clip or edited video is archived when its selection is approved. Rejected or never-approved renders stay only in the `master` role and expire after **30 days** (configurable per tenant).

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

**(v1.2, D6)** Storage is per tenant: each tenant has its own storage prefix, with an optional dedicated bucket and credentials. The roles and the namespace-disjointness rule (R28) apply within each tenant; the example above shows one tenant's configuration.

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

**YouTube** first; **(v1.2, D25)** roadmap after it: **Vimeo, PeerTube and Instagram Reels** (vertical clips reframed from the 4K master). Spotify is not planned. Other destinations (for example Dailymotion) remain possible community connectors but are not on the roadmap. One asset can have **multiple publications**, without duplicating identity.

Capabilities contract: `upload`, `get`, `update`, `delete`, `setVisibility`, `uploadThumbnail`, `uploadCaptions`, `getStatus`, `getUrl`, `capabilities`.

- States and metadata support vary by platform; require the connector to declare them.
- Uploads must be resumable when the destination supports it.
- A publication will not be publicly visible without **approval** by default. **(v1.2, D7)** Publication approval is given by a `publisher` (or `admin`); changing the visibility or public metadata of an already public video requires a new publisher approval.
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

- `CatalogConnector`: **(v1.2, D24)** the canonical catalog is Vidyom's tenant database; in the MVP, review happens through the CLI with proxy links. **MediaCMS is an optional connector** (one instance per tenant) for teams that want a web UI.
- `TranscriptionConnector`: initially local **faster-whisper** **(v1.2, D9)** with the large-v3 model; APIs and other engines in the future.
- `AIProvider`: local inference or remote API; configurable and interchangeable providers where their capabilities are equivalent. **(v1.2, D9)** Embeddings run locally with BGE-M3; story and chapter detection uses an LLM through a single OpenAI-compatible API driver (OpenAI, OpenRouter, vLLM, Ollama...). **(v1.2, D8)** Each configured LLM endpoint is classified as `local` or `external`; with the default `aiProcessing: local_only`, a collection or asset may only use local endpoints unless an admin marked it `external_allowed` (consent record: who and when).
- **VideoVortex** integration for CLI/transcript indexing, search, and clip operations; confirm compatibility and exact features of the installed edition.
- **Rewind** is **optional** for visual exploration/editing, not an MVP dependency. **(v1.2, D26)** The Rewind integration (visual transcript navigation and montage) is prioritized right after the MVP (after Phase 3) as an optional tool for editors.
- **Arcadeia** was considered and **not chosen** as a component of the priority architecture.
- Next.js is the proposed technology for the **public library**, consuming APIs/data published by Vidyom (see §13.2 on how this works with a daemon on a private network). **(v1.2, D18)** The daemon exports a read-only per-tenant `public.db` (SQLite) with approved public records and their embeddings; the site queries it and the daemon is never exposed publicly.
- **(v1.1, R33)** Status corrected: MediaCMS catalog = **DECIDED** (decision 17) **(v1.2, D24: MediaCMS is now an optional connector, no longer the first private catalog)**. VideoVortex = **PROPOSED AND PREFERRED** (decision 18; in §13.1 it appeared under a [DECIDED] heading). Rewind and Arcadeia = **PROPOSED** (decision 19). **[VALIDATE]** MediaCMS and VideoVortex licenses and their effect on the Vidyom license: integrating through API/CLI as a separate service tends not to contaminate Vidyom's code, but this must be confirmed together with item 1 of §17.

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

Plugins can run in **isolated processes** communicating over a versioned protocol, with WASM for some cases. **(v1.2, D3)** Out-of-process plugins talk to the daemon over **JSON-RPC 2.0 on stdin/stdout** (the same transport model as LSP and MCP): language-neutral and without code generation. Remote plugins and services use JSON-RPC over authenticated HTTPS. WASM remains possible for specific sandboxed cases [FUTURE]. Native Go `.so` plugins remain excluded. Do not automatically grant filesystem, network, keyring, credentials, or arbitrary command execution.

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
- **(v1.2, D3)** Hooks implemented by out-of-process plugins are invoked over **JSON-RPC 2.0 on stdin/stdout** (the plugin protocol); hooks served by remote plugins or services use JSON-RPC over authenticated HTTPS. Native Go `.so` plugins are excluded; WASM is possible only for specific sandboxed cases [FUTURE].
- **(v1.2, D11)** Hooks are not a way around a node's side-effect class: a `node.error` hook cannot retry an `unsafe` node; the job moves to `needs_attention` (§5, §6).

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
| `job.needs_attention` | **(v1.2, D11)** a job moved to `needs_attention` (an `unsafe` node failed or its outcome is unknown); not a terminal state, a person must resolve it |
| `budget.threshold_reached` / `budget.exceeded` | **(v1.2, D19)** tenant monthly budget reached 80% / 100% (new paid operations blocked until an admin override) |

**Minimum event envelope:** `event_id` (ULID, also used as the idempotency key), `type`, `schema_version`, `occurred_at` (UTC, RFC 3339), **`tenant_id` (v1.2, D6)**, `job_id`, `workflow_id`, `asset_id`, `trace_id`, `causation_id` (event or job that originated it), and `payload`. The `causation_id` makes it possible to measure chain depth and **prevent loops** (configurable depth limit, e.g., 5).

- Events can trigger other workflows (without changing the original workflow).
- Persist via **transactional outbox** [PROPOSED], with retries and event delivery tracking. **(v1.2, D5)** The outbox lives in the tenant's SQLite database, so event and state change commit together; only the daemon opens it.
- **(v1.2, D6)** Events are tenant-scoped: subscriptions, plugins and filters only see events of their own tenant.
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
    "filter": "event.payload.visibility == 'public'"
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

**(v1.2, D2)** Event trigger filters are **CEL** expressions (side-effect free, with evaluation cost limits, evaluated by the daemon with cel-go), as in the example above (`filter` is a CEL string, replacing the JSON object of v1.1). `${...}` is used only for data references such as `${event.asset_id}`.

**(v1.1, R15)** A complementary workflow subscribed to `publication.withdrawn` and `publication.updated` (when visibility stops being public) removes the asset from the public library. Without it, a video taken down from YouTube would remain listed. **(v1.2, D17)** Withdrawal is immediate and can be done by a `publisher` or `admin`; full deletion is an `admin` operation and emits its own audited event without content.

---

## 9. Security and credential management

### 9.1 Secrets and keyring [DECIDED]

- **(v1.2, D13)** **This changes the v1.1 decision** that credentials, on the client or the server, must live in the operating system keyring. Now: **client machines** keep using the OS keyring; **server** secrets are stored in an **encrypted vault inside each tenant database**, not in the server host's keyring.
- **(v1.2, D13)** Server vault: envelope encryption with **one data key per tenant** (the wrapped keys are kept in `system.db`), wrapped by a **master key unsealed at boot via TPM/systemd-creds or a cloud KMS** (no manual passphrase). A missing or sealed master key makes the daemon fail closed. The worker cache of the tenant is encrypted with the same tenant key (D22).
- Client keyring: macOS: Keychain; Windows: Credential Manager; Linux desktop: Secret Service/KWallet/GNOME Keyring; Linux headless client: explicitly provisioned secure Secret Service.
- **No** silent fallback to a plaintext file: on a client without a keyring, fail with a clear error or require secure provisioning; on the server, fail closed when the vault cannot be unsealed. **(v1.2, D13)**
- Workflow JSONs and configurations contain only `credentialsRef` references, never tokens, secrets, or passwords.
- `client`: secret stored in the user's keyring; may be unavailable to a disconnected remote daemon.
- `server`: **(v1.2, D13)** secret stored in the tenant's encrypted vault on the server (was: the daemon's keyring), preferred for long autonomous jobs.
- `delegated`: temporary, minimum-scope credential, sent to the server over an authenticated and encrypted channel, with consent/authorization; limited duration and storage.
- Secrets do not appear in CLI arguments, logs, traces, events, snapshots, manifests, exported files, or error messages. **(v1.1, R36)** For this reason `vidyom secret add` reads the value through a no-echo prompt or through stdin, never through an argument or an environment variable shown in the history. OAuth flows (YouTube) are done through the browser, and the resulting token goes directly to the keyring (client) or the tenant vault (server) of the chosen destination **(v1.2, D13)**. Secrets also never appear in workflow JSON or job payloads.
- **(v1.1, R37)** Jobs that run without a connected client (publication after approval, events, schedules) **can only use valid `server` or `delegated` credentials**. The validator (`--dry-run`) warns when a workflow with an event or schedule trigger references a `client` credential. The example in §11.5 was adjusted: `youtube-oauth` became `--location server`, because publication happens after approval, possibly without the CLI connected.
- Rotation, revocation, access tests, and usage auditing are requirements. **(v1.2, D13)** They include rotation of the tenant data key and re-wrapping by the master key.

### 9.2 Communication protection [DECIDED]

- Client-server communication **encrypted and authenticated**.
- Remote connection: **(v1.2, D12)** **HTTPS (TLS 1.3)** with certificate validation is the remote API transport (it replaces SSH as the default); mTLS when appropriate.
- **(v1.2, D12) Client pairing:** an `admin` generates a one-time pairing code; the CLI exchanges it for a per-device credential (`CLIENT-nnn`) that is revocable and rotated. Device credentials are stored in `system.db` (hashed) and in the client's OS keyring.
- Local Unix socket protected by filesystem permissions and process identity (it does not encrypt the traffic by itself); remains for same-host use. **(v1.2, D12)** SSH tunneling remains available only for host operators.
- Authentication and **authorization per operation/asset**, regardless of VPN.
- **(v1.2, D7)** Roles per tenant **[DECIDED]**: `viewer`, `editor`, `publisher`, `admin`. **[PROPOSED]** Plus granular permissions (`storage.archive.restore`, `asset.delete`, `plugin.install`, `secret.write`).
- **(v1.2, D6)** Multi-tenancy: users (in `system.db`) belong to one or more tenants, with a role per tenant. Every API request is scoped to one tenant, checked against the caller's role in that tenant; each tenant has its own database file, storage prefix, secrets, budgets and cache, and no operation crosses tenants.
- **(v1.2, D7)** Approval policy has two levels: an `editor` approves the clip selection (`approval.request` kind `clip_selection`); a `publisher` (or `admin`) approves publication (kind `publication`). Changing the visibility or public metadata of an already public video requires a new publisher approval. A workflow can tighten the rule through `approvers`, never loosen it.

### 9.3 Private networks [DECIDED]

- Possibility of running **exclusively on a Tailscale, NetBird, or equivalent interface**, with no exposure to the public internet.
- The term "tailwind" mentioned during the discussion was interpreted as **Tailscale** in the context of overlay networking.
- Modes: explicit bind to a private VPN IP; or `127.0.0.1` with Tailscale Serve/tunnel/authenticated proxy.
- **No** fallback to `0.0.0.0` when the private interface does not exist; fail closed. **(v1.2, D12)** Unchanged: binding only to a private interface remains supported with the same fail-closed rules, and pairing plus HTTPS still apply on top of it.
- **(v1.1, R38)** Decision 29 in §18 said "operation restricted to the Tailscale/NetBird interface", but this section (and the definition conversation itself) treats that as a **possibility**. Decision text corrected to "**possibility** of operation restricted…". The supported modes are: local socket only; private network (overlay); public TLS with authentication. In all of them, authentication and RBAC remain mandatory.
- Private-network firewall and access policies complement TLS, authentication, and RBAC; they do not replace them.

### 9.4 Plugin and workflow execution [DECIDED]

- Plugins with declared permissions for secrets, networks, and files.
- Verify origin, hashes, signatures when possible, and version compatibility.
- Imported JSON workflows do not automatically inherit the permissions of all connectors.
- No arbitrary shell by default; pass FFmpeg arguments in a safe/validated way, do not concatenate a command.
- Human approval before publishing or deleting, by default for publication; destructive actions with differentiated permission. **(v1.2, D7)** Two-level approval by role (clip selection: `editor`; publication: `publisher` or `admin`; see §9.2).
- **(v1.2, D17)** Withdrawal (unpublishing from every destination and from the public library) is immediate and done by a `publisher` or `admin`. Full deletion is done by an `admin`; the archive copy is purged when the archive's minimum storage duration ends (365 days on GCS Archive) and stays inaccessible until then. The audit keeps the fact of the deletion without the content. [VALIDATE] Legal review under the LGPD of deferring the archive purge.
- **(v1.2, D3)** Plugins run out of process and talk to the daemon over JSON-RPC 2.0 (stdin/stdout, or authenticated HTTPS for remote plugins); native Go `.so` plugins are excluded.
- Minimize exposure of private transcripts and sensitive content in AI logs and indexing.
- **(v1.1, R39)** Transcripts and audio of identifiable people are **personal data** under the LGPD (MapaDeva operates in Brazil) and equivalent regulations in other countries. Sending a transcript or audio to an external AI provider is processing by a third party and must be configurable **per asset or per collection** (`aiProcessing: local_only | external_allowed`). The **[PROPOSED]** default is `local_only` whenever a local engine is available. The topic is part of §17, item 6. **(v1.2, D8)** **[DECIDED]** The default is `aiProcessing: local_only`. An `admin` can mark a collection or an asset `external_allowed`; Vidyom records who authorized it and when (**consent record**, kept in the tenant database and the audit). This applies to transcription, embeddings and LLM calls; `local_only` content may only use endpoints classified `local` (D9).

---

## 10. Logs, metrics, traces, auditing, and alerts

### 10.1 Model [DECIDED]

- Structured JSON logs with `timestamp`, `level`, `service`, `event`, `job_id`, `workflow_id`, `node_id`, `asset_id`, `trace_id`, duration, and status.
- Observability through the `Observability Manager`, with filters, secret redaction, buffering, batching, fan-out, and pluggable exporters.
- Distributed tracing across CLI, API, daemon, workflow, worker, and connector.
- Auditing separate from operational logs, durable and resistant to unauthorized changes. **(v1.2, D23)** The audit is an **append-only table in each tenant database** with **hash chaining** (each record includes the previous record's hash); the chain is **exported daily to archive storage**, and `vidyom security audit --verify` checks it. It records consent changes, approvals, withdrawals, deletions (without content) and budget overrides.
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

- Jobs `queued`, `running`, `waiting_approval`, `needs_attention` **(v1.2, D11)**, `failed`, and `completed`.
- Execution time per node, workflow, plugin, and connector.
- Retries, failures, and connector latency.
- Transfer and storage per logical role/provider.
- Worker CPU, GPU, RAM, disk space, and queue.
- Estimated AI time/cost, when measurable.
- **(v1.2, D19)** Usage and estimated cost per operation, per tenant and per month (storage operations and egress, external AI, transcription, publishing), and budget consumption percentage.
- **(v1.2, D6)** All metrics and logs carry a `tenant_id` label/field.
- Originals/derivatives without verified backup.
- Clips awaiting editorial review.
- Authentication/authorization failures and security anomalies.
- Failures, delay, drops, and backlog of telemetry exporters.

### 10.4 Retention [PROPOSED]

| Type | Suggested initial value |
|---|---|
| Operational logs | 30 days |
| Job/execution history | 90 days or administrative policy |
| Auditing | **(v1.2, D23)** 1 year minimum, configurable (hash-chained; daily export to archive storage); was ">= 1 year, as required by applicable rules" |
| Metrics and traces | configurable per environment and cost |

### 10.5 Budgets and cost alerts [DECIDED] **(v1.2, D19)**

- Each tenant has a monthly estimated **budget** covering storage operations and egress, external AI, transcription, and publishing. Usage is recorded per operation.
- Vidyom alerts at **80%** of the budget (event `budget.threshold_reached`, notification to tenant admins) and **blocks new paid operations at 100%** (event `budget.exceeded`), with an `admin` override that is audited.
---

## 11. Specified CLI (target interface)

All commands below are **proposed for the Vidyom to be implemented**, not current features of third parties.

### 11.1 Daemon, servers, and connection

```bash
vidyomd start --detach
vidyom daemon status
vidyom daemon health
vidyom server add production --url https://vidyom.example.com   # (v1.2, D12) HTTPS + pairing, replaces --ssh as default
vidyom server pair production --code <one-time-code>             # (v1.2, D12) exchanges the code for a per-device credential
vidyom server add ops-host --ssh user@server.example.com         # (v1.2, D12) SSH only for host operators
vidyom server use production
vidyom server list

# (v1.2, D12) admin: pairing codes and devices
vidyom device pairing-code create --ttl 15m     # one-time code
vidyom device list
vidyom device revoke CLIENT-001
vidyom device rotate CLIENT-001

# (v1.2, D6) tenants and users with roles per tenant
vidyom tenant list
vidyom tenant use acme
vidyom tenant create acme                       # system admin
vidyom user add maria@example.com --tenant acme --role editor    # viewer | editor | publisher | admin

# (v1.2, D19) budgets and cost alerts
vidyom budget show
vidyom budget set --monthly 500 --currency USD
vidyom budget override --reason "end-of-month release"           # admin; audited
```

**(v1.2, D6, D12, D19)** The commands in this block are target interfaces. After `tenant use`, every command runs against that tenant; `--tenant` overrides it per command.

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

vidyom clip preview CLIP-001
vidyom clip approve CLIP-001
vidyom clip list --source ORI-001
vidyom asset lineage CLIP-001

# (v1.1, R40) generic approvals, adjustment, and withdrawal
vidyom approval list --status pending
vidyom approval show APR-001
vidyom approval approve APR-001 --start 00:10:48 --end 00:18:00
vidyom approval reject APR-001 --reason "incomplete excerpt"
vidyom asset versions CLIP-001
vidyom asset delete CLIP-001          # requires the asset.delete permission

# (v1.2, D17) withdrawal vs. full deletion
vidyom unpublish CLIP-001 --all       # publisher or admin; immediate; also leaves the public library
vidyom asset delete CLIP-001          # admin; archive copy purged when the minimum storage duration ends

# (v1.2, D8) external AI processing and consent record
vidyom collection set-ai --name yoga external_allowed   # admin; records who and when
vidyom asset set-ai ORI-001 local_only
vidyom asset consent ORI-001                            # shows the consent record
```

**(v1.1, R40)** `clip approve CLIP-001` was not enough: the selection gate (R13) happens **before** the clip exists as an asset, so there is no `CLIP-nnn` to approve yet. Gates are now handled by `approval …` with `APR-nnn`; `clip approve CLIP-nnn` remains as a shortcut for the pending publication approval of that clip. **(v1.2, D7)** `approval approve` checks the caller's role in the tenant: `clip_selection` needs `editor` or higher, `publication` needs `publisher` or `admin`, and a workflow's `approvers` can only tighten that.

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

# (v1.2, D11) jobs in needs_attention (an unsafe node failed or has an unknown outcome)
vidyom job list --status needs_attention
vidyom job resolve JOB-001 --retry-node publish   # after checking the remote state; or --skip-node, --fail
```

**(v1.2, D11)** `job resolve` is a target interface and is the only way out of `needs_attention`; it requires `editor` or higher (`publisher` for publication nodes) and is audited.

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
vidyom publish CLIP-001 --to youtube-main
vidyom publish CLIP-001 --to youtube-main,vimeo-main
vidyom publish status CLIP-001
vidyom publish sync CLIP-001
vidyom unpublish CLIP-001 --from youtube-main          # (v1.1, R15)
```

### 11.5 Credentials, security, observability, plugins, hooks

```bash
vidyom secret add gcs-main --location server          # value read via prompt/stdin (R36); (v1.2, D13) stored in the tenant vault
vidyom secret add youtube-oauth --location server     # (v1.1, R37) was "client"
vidyom secret add vimeo-oauth --location delegated --ttl 2h
vidyom secret list
vidyom secret test youtube-oauth
vidyom secret rotate youtube-oauth
vidyom secret delete youtube-oauth

vidyom security status
vidyom security audit --since 24h
vidyom security audit --verify                        # (v1.2, D23) verifies the hash chain
vidyom security audit --export                        # (v1.2, D23) on demand; the daily export is automatic
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
- `GET /jobs/{id}`: state, progress, result. **(v1.2, D11)** States include `needs_attention`.
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

- **(v1.2, D6, D12, D19, D11)** Tenant, pairing, budget, and resolution endpoints [PROPOSED]: `GET /tenants`, `GET|POST /tenants/{tenant}/users` (role per tenant), `POST /pairing-codes` (admin) and `POST /devices/pair` (unauthenticated, one-time code), `GET /devices`, `POST /devices/{id}/revoke|rotate`, `GET|PUT /budget` and `POST /budget/override`, `PUT /assets/{id}/ai-processing` (consent record), `POST /jobs/{id}/resolve` (from `needs_attention`), `GET /audit/verify`.
- **(v1.2, D6)** **Tenant scoping:** every request other than tenant and pairing management is scoped to a tenant (path prefix `/api/v1/tenants/{tenant}/...` or a tenant header [PROPOSED]) and authorized by the caller's role in that tenant; cross-tenant access returns `404`.
- **(v1.2, D12)** The remote API is served over HTTPS (TLS 1.3) with per-device credentials; the local Unix socket serves same-host clients.
- **(v1.2, D5)** Persistence behind the API (replacing PostgreSQL + pgvector of v1.1): one SQLite database file per tenant (assets, jobs, checkpoints, transcripts, FTS5 and sqlite-vec indexes, audit, encrypted secrets, outbox) plus a global `system.db` (tenants, users, roles, device credentials, plugin registry, wrapped tenant data keys). Only the daemon opens database files; workers use this API or the job queue, never the files. A repository layer keeps PostgreSQL possible later.

Authentication, authorization, idempotency keys, and an audit trail are mandatory for mutations. **(v1.2, D7)** Authorization is by role in the tenant; approval endpoints check the approval kind (`clip_selection`: `editor`+; `publication`: `publisher`/`admin`).

### 12.2 Original asset example

```json
{
  "id": "ORI-001",
  "tenant_id": "acme",
  "kind": "original",
  "category": "yoga_class",
  "title": "Hatha Yoga Class",
  "visibility": "private",
  "ai_processing": "local_only",
  "ai_consent": null,
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

The string `<real-hash-at-runtime>` is a didactic placeholder, not a valid SHA-256. **(v1.1, R6, R8, R21, R30)** Compared with v1.0, version, state flags, profile, models, and provenance were added; keys now include the version; the raw file keeps its original extension (`.mov` in the example). The `location` field (R30) was omitted here to shorten the example. **(v1.2, D6, D8)** `tenant_id`, `ai_processing` (default `local_only`) and `ai_consent` were added; when an admin allows external processing, `ai_consent` holds `{ "authorized_by": "<user>", "authorized_at": "<RFC 3339>", "scope": "asset|collection" }` (the consent record).

### 12.3 Clip example

```json
{
  "id": "CLIP-001",
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
    "key": "clips/CLIP-001/v1/master.mp4"
  },
  "backup": {
    "storage": "archive",
    "key": "clips/CLIP-001/v1/master.mp4",
    "verified": true
  },
  "publications": []
}
```

The timestamps above are examples; in a real run they will be computed by the indexing. **(v1.1, R12, R42)** Each segment now indicates its source and the source's version (preparing for assembly from multiple sources) and accepts fractions of a second, because clips cut at a sentence boundary need more precision than whole seconds. The clip's backup key changed from `original.mp4` to `master.mp4`: what goes to the Archive is the **clip's master**, and calling it "original" was confusing with the raw file of an `original`.

### 12.4 Independent publications example

```json
{
  "asset_id": "CLIP-001",
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
- The canonical catalog is independent of any external UI. **(v1.2, D5, D24)** It is the tenant's SQLite database; MediaCMS is an optional connector (one instance per tenant), not the catalog.
- Ensure UTF-8, explicit timestamps, hashes, and origin metadata.

---

## 13. Private catalog and public library

### 13.1 Private [DECIDED]

- **Tenant database (SQLite) [DECIDED] (v1.2, D5, D24)**: the canonical catalog is Vidyom's own tenant database (one SQLite file per tenant, plus the global `system.db`). Full-text search uses **FTS5**; vector search uses **sqlite-vec** [VALIDATE: performance and maturity; it has no approximate index yet, so asset-scoped search is the main path and tenant-wide semantic search must be load-tested]. Databases run in WAL mode and are continuously backed up with **Litestream** to the archive storage provider. Only the daemon opens the database files. A repository layer keeps PostgreSQL possible later. **(v1.2, D5)** This replaces "PostgreSQL + pgvector [PROPOSED]" (v1.1).
- **Review in the MVP (v1.2, D24)**: happens through the CLI, with links to proxy videos.
- **MediaCMS [OPTIONAL CONNECTOR] (v1.2, D24)**: private web UI for teams that want one (proxy videos, tags/categories/collections, browsing, API integration when compatible), with one instance per tenant. It replaces "MediaCMS as the first private catalog".
- **VideoVortex (`vvx`) [PROPOSED AND PREFERRED, see R33]**: tool for indexing, text search, and segment/clip operations, with a CLI and AI integration, according to the validated capabilities of the deployed version.
- **Local AI models (v1.2, D9)**: transcription with faster-whisper large-v3 and embeddings with BGE-M3 (multilingual), both run locally; the embedding model is stored per vector.
- **Per tenant (v1.2, D6)**: each tenant has its own catalog database, so catalog data is never shared across tenants.
- **Rewind [PROPOSED: optional]**: visual editing/review of transcripts and clips. **(v1.2, D26)** Prioritized right after the MVP (after Phase 3) as an optional tool for editors.
- **Arcadeia [PROPOSED: not a priority]**: evaluated; not a recommended component of this architecture.
- **(v1.1, R11)** Synchronization failures with MediaCMS/VideoVortex are recorded in `catalog_sync`, with retry; they block neither readiness nor publication. **(v1.2, D24)** MediaCMS applies only when its optional connector is configured.
- When the optional MediaCMS connector is used, verify how MediaCMS receives or references an already processed proxy; avoid duplicating transcoding and storage unnecessarily **(v1.2, D24)**.
- MediaCMS and VideoVortex may maintain their own indexes; Vidyom must synchronize through stable IDs and prevent either product from becoming the sole source of truth. **(v1.2, D24)** The tenant database is the canonical source.

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

- The public library **never** accesses `vidyomd`. The `catalog.public.sync` node **pushes** an already filtered public dataset to its own destination: a bucket with JSON/static files or a separate read-only database, configured as a `PublicCatalogConnector` connector. **(v1.2, D18)** The format is a read-only `public.db` (SQLite), see below.
- This dataset contains only released fields (title, description, chapters, tags, links/embeds, authorized transcript excerpts) and **no** storage path, internal technical ID, bucket key, or data from the private original.
- Public semantic search uses embeddings exported in this same dataset (only for published content) or the library's own index. It never queries the private index.
- Withdrawals (R15) remove the item from this dataset idempotently.
- **(v1.2, D18, D6, D9) Format of the public dataset [DECIDED].** Per tenant, the daemon exports a read-only **`public.db`** (SQLite) containing only approved public records and their embeddings (BGE-M3), and ships it to the public site's host. The Next.js site queries it and computes query embeddings on CPU for semantic search. The daemon is never exposed publicly.

---

## 14. File and repository organization

### 14.1 Logical storage layout

```text
gs://videos-archive/                       # archive role
  originals/
    ORI-001/v1/original.<ext>              # raw, extension preserved
  clips/
    CLIP-001/v1/master.mp4                 # clip master (v1.2, D1)
  edited/
    EDT-001/v1/master.<ext>
  litestream/<db>/                         # (v1.2, D5) continuous SQLite replicas
  audit/                                   # (v1.2, D23) daily export of the audit hash chain

gs://videos-standard/masters/              # master role
  originals/
    ORI-001/v1/master.mp4
  clips/
    CLIP-001/v1/master.mp4
    CLIP-001/v1/published-youtube-main.mp4 # only if different from the master (R4)

gs://videos-standard/proxies/              # proxy role
  originals/
    ORI-001/v1/proxy.mp4
    ORI-001/v1/thumbnail.jpg
  clips/
    CLIP-001/v1/proxy.mp4
```

**These paths are initial conventions of the GCS connector**. Workflows must reference the logical roles `archive`, `master`, `proxy`, not buckets directly.

**(v1.2, D6) Per-tenant prefixes.** The tree above shows the keys of one tenant. Each tenant has its own storage prefix inside each role (for example `<tenant>/originals/…`), with an optional dedicated bucket and credentials; tenants never share a prefix. **(v1.2, D1)** Storage paths follow the `CLIP-` prefix; the technical key remains a ULID.

**(v1.2, D5, D6, D18, D22) Database and cache files** (initial conventions, on the host running the daemon):

```text
<data-dir>/
  system.db                    # tenants, users, roles, device credentials, plugin registry, wrapped tenant data keys
  tenants/<tenant>/
    tenant.db                  # assets, jobs, checkpoints, transcripts, indexes, audit, encrypted secrets (WAL)
    public.db                  # read-only public library export (D18)
<cache-dir>/<tenant>/          # worker cache, encrypted with the tenant key (D22)
```

Only the daemon opens these files; workers use the daemon API or Redis.

**(v1.1, R4, R6, R28, R42) Corrections relative to v1.0:** the v1.0 layout put the proxy in `videos-standard/originals/…`, without the `proxies/` prefix that the §7.2 configuration itself defined; keys had no version (a new render would overwrite the previous file); the clip backup was named `original.mp4`; there was no place for `edited` assets; and the published file was duplicated even when identical to the master. Keys inside a role (`originals/…`) are relative to the role's prefix.

### 14.2 Code structure [PROPOSED]

```text
vidyom/
├── CHANGELOG.md                 # (v1.2, D20)
├── deploy/                      # (v1.2, D10)
│   ├── docker-compose.yml       # daemon, workers (FFmpeg + Python AI worker), Redis
│   └── Dockerfile.*             # images published to GHCR
├── website/                     # documentation website (same repository)
├── spec/                        # specification (same repository)
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
│   ├── persistence/             # (v1.2, D5) SQLite repository layer: tenant databases + system.db
│   ├── queue/                   # (v1.2, D4) Redis dispatch and startup reconciliation
│   ├── tenants/                 # (v1.2, D6) tenant lifecycle, per-tenant keys and prefixes
│   ├── catalog/
│   ├── hooks/
│   ├── events/
│   │   ├── outbox/
│   │   └── subscriptions/
│   ├── plugins/
│   ├── security/
│   │   ├── keyring/             # client OS keyring
│   │   ├── vault/               # (v1.2, D13) per-tenant encrypted vault, master key via TPM/systemd-creds or KMS
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
│   │   ├── vimeo/               # roadmap after YouTube (v1.2, D25)
│   │   ├── peertube/            # roadmap after YouTube (v1.2, D25)
│   │   ├── instagram-reels/     # roadmap after YouTube (v1.2, D25)
│   │   └── dailymotion/         # possible community connector, not on the roadmap (v1.2, D25)
│   ├── observability/
│   │   ├── file/
│   │   ├── loki/
│   │   ├── datadog/             # future
│   │   ├── prometheus/
│   │   └── otlp/
│   ├── catalog/mediacms/        # optional connector, one instance per tenant (v1.2, D24)
│   ├── catalog/public/          # (v1.1, R44) destination of the public dataset (v1.2, D18: per-tenant public.db)
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
│   ├── master-2160p-hevc.json   # (v1.2, D14)
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
5. media and metadata appear in the tenant catalog and can be reviewed through the CLI with proxy links, and in MediaCMS when its optional connector is configured **(v1.2, D24)**;
6. the job is observable through the CLI even after disconnection/reconnection;
7. the asset only becomes `ready_for_clipping` when the mandatory conditions are met;
8. **(v1.1)** the duration of the master, the proxy, and the ASR audio matches that of the raw file within tolerance (R10); **(v1.2, D21)** the tolerance is max(100 ms, 1 frame), and a source with variable frame rate or broken timestamps is normalized to constant frame rate in the master before transcription, so transcripts use the master's timeline;
9. **(v1.1)** with MediaCMS (optional connector) down, the asset still reaches `ready_for_clipping` and is left with `catalog_sync = failed` (R11, v1.2, D24).

### 15.2 E2E scenario — two clips on different days

**Given** `ORI-001` already indexed, **when** the prompts "Story of the 90-year-old woman who had no remorse" and, days later, "The case of the friend who never got a hug" are submitted, **then**:

1. the `ORI-001` index finds relevant candidates without exact words;
2. the AI proposes the start and end of each narrative, with review;
3. a worker downloads only the **working master**, never the raw file from the Archive;
4. two new assets, `CLIP-001` and `CLIP-002`, are created, with correct lineage **(v1.2, D1)**;
5. each clip receives a backup, working master, proxy, derived/reviewed transcript, and index;
6. each clip can be published independently and appear in the public library after approval;
7. no step of Workflow A for `ORI-001` needs to run again;
8. **(v1.1)** IDs are allocated without collision even if the two requests run at the same time (R5). The numbers `CLIP-001`/`CLIP-002` are illustrative: if `CLIP-001` already exists (as in the example in §2.4), the new clips receive the next free numbers;
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
- **(v1.2, D4)** Redis loss: with Redis emptied or restarted without its data, on startup the daemon re-enqueues every job that is `queued` or `running` in the tenant database but missing from Redis; no job is lost or executed twice.
- **(v1.2, D11)** A node of side-effect class `unsafe` that fails or is interrupted is never retried automatically: the job moves to `needs_attention` until a person resolves it. A `reconcilable` node queries the remote state before any retry.

### 15.6 Scenario — security

- No secret in the JSON, arguments, logs, traces, and exported files.
- A daemon configured for Tailscale/NetBird does not start a public listener if the interface fails.
- An unauthorized client does not install a plugin, restore the Archive, delete assets, or publish.
- Headless Linux without a provisioned keyring fails explicitly, with no silent plaintext. **(v1.2, D13)** This applies to client machines; the server uses the per-tenant encrypted vault instead of the host keyring.
- **(v1.1)** `server:` with `..` or a symbolic link pointing outside `ingestRoots` is rejected (R20).
- **(v1.1)** The public dataset contains no storage keys, internal IDs, unauthorized transcripts, or data from private originals (R44). **(v1.2, D18)** This applies to each tenant's `public.db`, which holds only approved public records and their embeddings.
- **(v1.2, D6)** Tenant isolation: a user of tenant A cannot read or change tenant B's assets, jobs, secrets, storage prefix, cache directory, budget, or public export; queries and API calls with another tenant's IDs are rejected, and a worker for tenant A cannot open tenant B's cache or database.
- **(v1.2, D7)** Approval levels: an `editor` can approve `clip_selection` but not `publication`; a `publisher` or `admin` approves `publication`; a `viewer` approves nothing; changing the visibility or public metadata of an already public video requires a new publisher approval; a workflow's `approvers` can tighten but never loosen the rule.
- **(v1.2, D13)** With a missing or sealed master key, the daemon fails closed and starts no listener.
- **(v1.1)** `--dry-run` warns when an event-triggered workflow uses a `client` credential (R37).

### 15.8 Scenario — publication withdrawal (v1.1, R15)

- `vidyom unpublish CLIP-001 --from youtube-main` makes the video private or removes it at the destination, marks the publication as `withdrawn`, emits `publication.withdrawn`, and removes the clip from the public library.
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

- Define the contribution policy. The license (Apache-2.0) and the single repository (`github.com/rafaelp/vidyom`) were decided before v1.2.
- Define the JSON Schema for workflows, plugins, connectors, and job states.
- Optional MediaCMS connector PoC with a pre-processed proxy and metadata synchronization: API, externally stored proxies, double transcoding, one instance per tenant **(v1.2, D24)**.
- VideoVortex PoC with transcription, search, and clipping, verifying version/license capabilities and restrictions.
- FFmpeg PoC for the working master from the raw file and a real clip test.
- Validate integration with GCS, the client keyring, and the private Tailscale/NetBird mode.
- **(v1.2, D5, D4, D13, D9)** Validation PoCs: sqlite-vec performance at tenant scale and Litestream with GCS; Redis AOF durability and reconciliation under crash tests; TPM/systemd-creds and KMS unsealing on the target hosts; accuracy, word timestamps and diarization of faster-whisper large-v3 on the target content.

### Phase 1 — Functional core and ingestion

- Local/remote CLI and daemon, authentication, and durable jobs.
- **(v1.2, D5, D6, D4)** SQLite per tenant plus `system.db` (WAL, Litestream), multi-tenancy from the first release, and the Redis job queue with startup reconciliation; only the daemon opens the database files.
- **(v1.2, D10)** Docker Compose deployment (daemon, workers with FFmpeg and the Python AI worker, Redis); the Go binaries can still run natively under systemd.
- **(v1.2, D12, D11)** HTTPS remote API with client pairing; node side-effect classes and the `needs_attention` job state.
- **(v1.2, D20)** Release process: Semantic Versioning (`0.x` until stable), tags `vX.Y.Z`, GitHub Releases with CLI binaries, Docker images on GHCR, and `CHANGELOG.md`.
- Minimal JSON workflow, `storage.put/get/verify`, `video.transcode`, `audio.extract`, `ai.transcribe`, `ai.index`.
- GCS connector with `archive/master/proxy` and generation of portable metadata.
- JSONL logs and job status/logs API.
- Complete `ingest-original.json` with `ready_for_clipping`.

### Phase 2 — Semantic clips and subworkflows

- Lexical+vector retrieval (FTS5 + sqlite-vec, asset-scoped first), identification of a complete narrative **(v1.2, D5)**.
- Preview, review (through the CLI with proxy links, **v1.2, D24**), timestamp adjustment, and rendering with FFmpeg.
- `create-clip.json` and `ingest-derived.json` with lineage, backup, and reindexing.
- Ensure that no clip reads from the Archive.
- **(v1.1)** `approval.request` node, `approval …` commands, asset versions, and validation of disjoint namespaces (R6, R25, R28).

### Phase 3 — Publishing and public catalog

- YouTube connector; OAuth stored in the tenant vault on the server **(v1.2, D13)**; resumable upload, status, thumbnails, captions.
- Persistent approval with two levels (editor: selection; publisher or admin: publication) **(v1.2, D7)**; idempotent publishing; published files in the operational storage.
- Next.js library with search and access only to approved assets.
- **(v1.1)** Public dataset sent by push (R44) and `unpublish-video.json` with removal from the library (R15). **(v1.2, D18)** The dataset is the per-tenant read-only `public.db`.

### Phase 3.1 — Rewind integration (v1.2, D26)

- Rewind (visual transcript navigation and montage) as an optional tool for editors, prioritized right after Phase 3 and before Phase 4.

### Phase 4 — Plugins, hooks, and full observability

- Isolated plugin runtime (JSON-RPC 2.0 over stdin/stdout, **v1.2, D3**), permissions manifest, registry.
- Global/workflow/node hooks; persistent Event Bus.
- OpenTelemetry, Loki, Prometheus, optional Tempo/Grafana, and sink interface.
- **(v1.2, D25)** Publishing destinations after YouTube: Vimeo, PeerTube, and Instagram Reels (vertical clips reframed from the 4K master). Spotify is not planned; other destinations (for example Dailymotion) may be community connectors but are not on the roadmap.
- Future connectors: S3, Dropbox, SFTP, Datadog, etc.

---

## 17. Pending decisions and mandatory validations

The definition conversation set out the architecture and the high-level interfaces. **(v1.2)** The maintainer decisions of 2026-11-08 (D1–D26, §21) resolved most of the items that were pending in v1.1. Items still open **have not yet been explicitly decided** and must not be silently filled in by the team.

### 17.1 Resolved in v1.2

Numbering follows the v1.1 list.

1. License: **Apache-2.0**, decided before v1.2.
2. Deployment model: Docker Compose for the MVP; Kubernetes is future **(v1.2, D10)**.
3. Working master policy: `master-1080p` / `master-2160p-hevc`, AAC 192 kbps **(v1.2, D14)**; retention and archive of masters **(v1.2, D16)**.
4. Embeddings, LLM and ASR: faster-whisper large-v3, BGE-M3, and an OpenAI-compatible LLM driver classified `local` or `external` **(v1.2, D9)**; transcription accuracy on the target content remains a validation (§17.2).
5. Approval policy: two levels, `editor` for selection and `publisher`/`admin` for publication **(v1.2, D7)**.
6. Copyright and privacy: resolved in part, with external AI processing in D8 and withdrawal in D17; the rest remains open (§17.2).
7. Retry, cancellation and checkpoint: side-effect classes and `needs_attention` **(v1.2, D11)**.
8. Plugin protocol: JSON-RPC 2.0 over stdin/stdout, HTTPS for remote **(v1.2, D3)**.
9. Workflow expressions: `${...}` for data, CEL for conditions **(v1.2, D2)**; merge and branching rules remain [PROPOSED].
10. Multi-tenancy: from the MVP **(v1.2, D6)**.
11. Secrets on the server: per-tenant encrypted vault, master key unsealed via TPM/systemd-creds or KMS **(v1.2, D13)**; unsealing on the target hosts remains a validation (§17.2).
12. Local cache: per tenant, LRU, encrypted at rest **(v1.2, D22)**.
13. Synchronization model: the tenant database is canonical and MediaCMS is an optional connector **(v1.2, D24)**; the VideoVortex validation remains open (§17.2).
14. Cost limits: monthly budget per tenant, alert at 80%, block at 100% **(v1.2, D19)**.
15. Publishing destinations: YouTube first, then Vimeo, PeerTube and Instagram Reels **(v1.2, D25)**; API validation remains open (§17.2).
16. Repository: single repository `github.com/rafaelp/vidyom`, decided before v1.2; domain, package names and trademark remain open (§17.2).
17. Embedding segmentation and reindexing: **(v1.2, D15)**.
18. Archive version policy: **(v1.2, D16)**.
19. Deletion and withdrawal: **(v1.2, D17)**; the LGPD review is [VALIDATE] (§17.2).
20. External AI processing: default `local_only`, consent record **(v1.2, D8)**.
21. Client pairing: HTTPS with one-time pairing code and per-device credential **(v1.2, D12)**.
22. Public dataset: per-tenant `public.db` **(v1.2, D18)**.
23. Timeline alignment: max(100 ms, 1 frame), constant frame rate normalization **(v1.2, D21)**.
24. ID prefixes: `CLIP-` replaces `COR-`; other prefixes unchanged **(v1.2, D1)**.

Also decided in v1.2: job queue (D4), database (D5), release process (D20), audit (D23), private catalog (D24), and Rewind priority (D26).

### 17.2 Still open

None of these should be decided silently.

**Decisions**

- Copyright and privacy: permissions for speakers and participants, publication authorizations, handling of sensitive transcripts.
- Domain, package/registry names and trademark check.

**Validations (proofs of concept)**

- sqlite-vec performance at tenant scale; Litestream with GCS (D5).
- Redis AOF durability and reconciliation under crash tests (D4).
- TPM/systemd-creds and KMS unsealing on the target hosts (D13).
- MediaCMS: API, externally stored proxies, double transcoding, one instance per tenant (D24).
- VideoVortex: real APIs/CLI, license, synchronization with the tenant database.
- Transcription: accuracy, word timestamps and diarization of faster-whisper large-v3 on the target content (D9).
- Publishing APIs: YouTube, Vimeo, PeerTube and Instagram (OAuth, quotas, captions, retry reconciliation) (D25).
- License compatibility of every dependency with Apache-2.0.
- LGPD legal review of the deferred archive purge (D17).

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
| 17 | Private catalog: **MediaCMS** as the main interface | SUPERSEDED by #72 (v1.2) |
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
| 28 | Credentials in the OS keyring, client **or** server (v1.2, D13: the server part is replaced; client machines keep the OS keyring) | SUPERSEDED by #61 (v1.2) |
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
| 40 | PostgreSQL + pgvector as the canonical catalog, jobs, and search | SUPERSEDED by #53 (v1.2) |
| 41 | (v1.1) Verified backup is part of `ready_for_clipping`; synchronization with MediaCMS does not block (R11) | PROPOSED (resolves inconsistency) |
| 42 | (v1.1) Published file kept in the `master` role (R4) | PROPOSED (resolves inconsistency) |
| 43 | (v1.1) Canonical `publish-video` publishing workflow with `destinations` (R14) | PROPOSED (resolves inconsistency) |
| 44 | (v1.1) Two approval gates: selection and publication (R13, R25) | PROPOSED |
| 45 | (v1.1) Immutable asset versions; storage keys with version (R6) | PROPOSED |
| 46 | (v1.1) Public library receives data by push and never accesses the daemon (R44) | PROPOSED |
| 47 | (v1.1) Storage roles with disjoint namespaces (R28) | PROPOSED |
| 48 | (v1.1) Publication withdrawal (`unpublish-video`) and controlled deletion (R15, R16) | PROPOSED |
| 49 | (v1.2, D1) Friendly clip IDs use `CLIP-` instead of `COR-`; other prefixes unchanged; ULID remains the technical key | DECIDED |
| 50 | (v1.2, D2) `${...}` only for data references; conditions in CEL (cel-go) | DECIDED (merge and branching rules: PROPOSED) |
| 51 | (v1.2, D3) Plugin protocol: JSON-RPC 2.0 over stdin/stdout; HTTPS for remote; Go `.so` excluded | DECIDED (WASM: FUTURE) |
| 52 | (v1.2, D4) Redis as the job dispatch queue (AOF); tenant database is the source of truth; startup reconciliation | DECIDED |
| 53 | (v1.2, D5) SQLite per tenant plus global `system.db`; FTS5, sqlite-vec, WAL, Litestream; only the daemon opens the files | DECIDED (sqlite-vec: VALIDATE) |
| 54 | (v1.2, D6) Multi-tenant from the MVP: database, storage prefix, secrets, budgets, cache and public export per tenant | DECIDED |
| 55 | (v1.2, D7) Two approval levels (`editor`: selection; `publisher`/`admin`: publication); roles `viewer`, `editor`, `publisher`, `admin` | DECIDED |
| 56 | (v1.2, D8) External AI processing: default `local_only`; `external_allowed` per collection or asset with consent record | DECIDED |
| 57 | (v1.2, D9) faster-whisper large-v3, BGE-M3, and an OpenAI-compatible LLM driver classified `local` or `external` | DECIDED |
| 58 | (v1.2, D10) Docker Compose is the supported MVP deployment; Kubernetes is future | DECIDED |
| 59 | (v1.2, D11) Node side-effect classes and the `needs_attention` job state; cooperative cancellation; checkpoint per node | DECIDED |
| 60 | (v1.2, D12) Remote API over HTTPS (TLS 1.3) with client pairing; SSH tunneling only for host operators | DECIDED |
| 61 | (v1.2, D13) Server secrets in an encrypted per-tenant vault with envelope encryption; master key unsealed via TPM/systemd-creds or KMS | DECIDED |
| 62 | (v1.2, D14) Working master: H.264 CRF 18 up to 1080p; HEVC CRF 20 at 2160p or more; AAC 192 kbps | DECIDED |
| 63 | (v1.2, D15) Transcript windows of 30–60 s with about 15% overlap; chapter summaries as a second level; side-by-side reindexing | DECIDED |
| 64 | (v1.2, D16) Raw originals archived permanently; approved masters archived; unapproved renders expire after 30 days | DECIDED |
| 65 | (v1.2, D17) Immediate withdrawal; deletion by admin with deferred archive purge; audit keeps the fact of deletion | DECIDED (LGPD review of the deferred purge: VALIDATE) |
| 66 | (v1.2, D18) Per-tenant read-only `public.db` exported to the public site's host; the daemon is never exposed publicly | DECIDED |
| 67 | (v1.2, D19) Monthly budget per tenant; alert at 80%, block at 100% with admin override | DECIDED |
| 68 | (v1.2, D20) Semantic Versioning, tags `vX.Y.Z`, GitHub Releases, GHCR images, `CHANGELOG.md` | DECIDED |
| 69 | (v1.2, D21) Timeline tolerance max(100 ms, 1 frame); constant frame rate normalization before transcription | DECIDED |
| 70 | (v1.2, D22) Per-tenant worker cache: LRU, 7-day idle expiry, checksum on reuse, encrypted with the tenant key | DECIDED |
| 71 | (v1.2, D23) Append-only audit with hash chaining, exported daily; minimum retention of 1 year | DECIDED |
| 72 | (v1.2, D24) Tenant database is the canonical catalog; CLI review in the MVP; MediaCMS is an optional connector | DECIDED |
| 73 | (v1.2, D25) YouTube first; then Vimeo, PeerTube and Instagram Reels; Spotify not planned | DECIDED |
| 74 | (v1.2, D26) Rewind prioritized right after Phase 3 as an optional tool for editors | DECIDED |

**(v1.2)** Rows 49–74 record decisions D1–D26 of 2026-11-08 (§21). Rows 17, 28 and 40 are kept for history and marked as superseded.

---

## 19. Expected outcome

Vidyom must allow any user to install a daemon (with its workers and Redis, through Docker Compose, **v1.2, D10**) on their own server, serving one or more tenants **(v1.2, D6)**, configure storages and publishing platforms with securely stored credentials, control all the work through the CLI, and define media pipelines in JSON.

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

---

## 21. Revision log v1.2

Maintainer decisions of 2026-11-08 on the items that were pending in v1.1 (§17). Each decision is identified by its ID (D1–D26) and marked **(v1.2, Dn)** in the text; v1.1 passages and §20 are preserved as history.

| # | Topic | Sections | Before (v1.1) | Decision (v1.2) |
|---|---|---|---|---|
| D1 | Clip ID prefix | §2.1, §14.1, §15, §18 | `COR-` (Portuguese prefix, R5) | `CLIP-` (for example `CLIP-001`); other prefixes unchanged; storage paths follow |
| D2 | Workflow expressions | §6.2 | Final format open; `${...}` for everything | `${...}` only for data; conditions in CEL; merge and branching rules [PROPOSED] |
| D3 | Plugin protocol | §5, §7.5, §16 | gRPC, JSON-RPC, HTTP or a mix, undecided | JSON-RPC 2.0 over stdin/stdout; HTTPS for remote; WASM [FUTURE]; no Go `.so` |
| D4 | Job queue | §5, §15.5 | Not defined; database-backed jobs | Redis (AOF) dispatch queue; tenant database is the source of truth; startup reconciliation |
| D5 | Database | §5, §12, §13.1, §14.1, §16 | PostgreSQL + pgvector [PROPOSED] | SQLite per tenant plus `system.db`, FTS5, sqlite-vec [VALIDATE], Litestream; only the daemon opens the files |
| D6 | Multi-tenancy | §5, §9, §12, §14.1, §15.6 | Open: MVP or later | Multi-tenant from the MVP: database, prefix, secrets, budget, cache and public export per tenant |
| D7 | Approval policy | §6.2, §9, §15.6, §16 | Levels and defaults open | `editor` approves selection; `publisher`/`admin` approves publication; re-approval on public changes |
| D8 | External AI processing | §9 | Default and consent open (R39) | `local_only` by default; `external_allowed` per collection or asset with consent record |
| D9 | AI models | §4, §13.1, §17 | Engines and models open | faster-whisper large-v3, BGE-M3, OpenAI-compatible LLM driver classified `local`/`external` |
| D10 | Deployment | §5, §14.2, §16, §19 | Docker Compose/systemd open | Docker Compose is the MVP path; systemd possible; Kubernetes future |
| D11 | Retry, cancellation, checkpoints | §5, §6, §15.5 | Semantics open | Side-effect classes; `needs_attention` state; cooperative cancellation; checkpoint per node |
| D12 | Remote access | §5, §9 | SSH as the default remote transport | HTTPS (TLS 1.3) with client pairing; SSH only for host operators; private-interface mode kept |
| D13 | Server-side secrets | §9, §14.2, §15.6, §16 | Server secrets in the host OS keyring | Per-tenant encrypted vault; master key via TPM/systemd-creds or KMS; client keyring unchanged |
| D14 | Working master | §4, §14.2 | Policy open | H.264 CRF 18 up to 1080p; HEVC CRF 20 at 2160p+; AAC 192 kbps |
| D15 | Transcript segmentation | §4, §17 | Strategy open (R18) | 30–60 s windows, about 15% overlap, chapter level; side-by-side reindexing |
| D16 | Archive policy | §7.2, §17 | `archivePolicy` [VALIDATE] (R29) | Originals permanent; approved masters archived; unapproved renders expire in 30 days |
| D17 | Withdrawal and deletion | §3, §9, §17 | Responsibilities and timing open (R15, R16) | Immediate withdrawal; admin deletion; deferred archive purge; LGPD review [VALIDATE] |
| D18 | Public library | §13.2, §14.1, §15.6, §16 | Public dataset format open (R44) | Per-tenant read-only `public.db`; CPU query embeddings in Next.js; daemon never exposed |
| D19 | Costs | §10, §17 | Limits open | Monthly budget per tenant; alert at 80%, block at 100% with admin override |
| D20 | Releases | §16, §14.2 | Not defined | SemVer (`0.x`), tags `vX.Y.Z`, GitHub Releases, GHCR, `CHANGELOG.md` |
| D21 | Timeline alignment | §3, §4, §15.1 | Tolerance open (R10) | max(100 ms, 1 frame); constant frame rate normalization in the master |
| D22 | Worker cache | §7, §14.1, §17 | Policy open | Per-tenant LRU cache, 7-day idle expiry, checksum on reuse, encrypted with the tenant key |
| D23 | Audit | §9, §10, §14.1 | Audit without integrity guarantee | Append-only table with hash chaining, daily export to archive, 1-year minimum retention |
| D24 | Private catalog | §7.5, §13.1, §15.1, §16 | MediaCMS as the first private catalog | Tenant database is canonical; CLI review in the MVP; MediaCMS optional connector per tenant |
| D25 | Publishing destinations | §7, §14.2, §16 | YouTube, Vimeo and others, unordered | YouTube first, then Vimeo, PeerTube, Instagram Reels; Spotify not planned |
| D26 | Rewind | §7.5, §13.1, §16 | Rewind [PROPOSED: optional] | Prioritized right after Phase 3 as an optional tool for editors |

**New sections:** §17.1 and §17.2 (split of the pending items), rows 49–74 of §18, and this §21. Still open: see §17.2.
