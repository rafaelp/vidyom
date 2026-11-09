---
title: "Workflow A: ingest original"
sidebar_position: 1
---

# Workflow A — Ingest Original

**Decided:** Ingestion runs first and is independent of clip creation. The input is any original recording (yoga class, course, meeting, lecture, interview, etc.). After processing, the original is permanently available for future semantic clipping.

```mermaid
flowchart TD
 A[Import original media] --> B[Register asset + hash]
 B --> C[Archive unmodified source]
 B --> D[Create production master]
 D --> E[Store master in direct-access storage]
 D --> F[Generate preview proxy]
 B --> G[Extract clean speech audio]
 G --> H[Transcribe with timestamps]
 H --> I[Chapters, topics, story boundaries, embeddings]
 E --> J{Validate assets and index}
 F --> J
 I --> J
 C --> J
 J --> K[Private catalog: ready_for_clipping]
```

## Detailed stages

1. **Import:** accept a local file, resumable upload, or an authorized imported copy from an existing platform. Calculate SHA-256; create stable `ORI-*` ID and metadata.
2. **Archive:** save an untouched copy to logical storage role `archive`; verify checksum and record receipt. The initial connector is Google Cloud Storage Archive.
3. **Master:** generate the working MP4 and store it permanently under role `master` (initially GCS Standard). **Decided:** the profile depends on the source. Sources up to 1080p use `master-1080p` (H.264 High, CRF 18, preset slow, max 1080p). Sources of 2160p or more use `master-2160p-hevc` (HEVC, CRF 20, 4K kept to allow vertical reframing and zoom). Audio is AAC 192 kbps in both. Sources with variable frame rate or broken timestamps are normalized to constant frame rate in the master before transcription.
4. **Proxy:** produce smaller playback/preview MP4 (suggested 720p H.264) stored under role `proxy`.
5. **Audio:** derive mono speech audio directly from the raw original, avoiding double lossy conversion. Suggest Opus 16 kHz, 24–32 kbps where accepted by the transcription engine. **Decided:** the master, the proxy and the speech audio must match the raw file's duration within max(100 ms, 1 frame); a larger drift fails the stage.
6. **AI:** transcribe with timestamps; extract chapters, narratives, topics, summaries and embeddings. **Decided:** transcription runs locally with faster-whisper large-v3 and embeddings run locally with BGE-M3. Story and chapter detection uses an LLM through the OpenAI-compatible driver; a collection marked `local_only` (the default) may only use a local endpoint, and without one story detection is skipped for it. Transcripts use the master's timeline, which matches the raw timeline within the tolerance above.
7. **Catalog:** write the canonical record to the tenant database, update the text and semantic index, and store canonical metadata; verify accessible master/index, then mark `ready_for_clipping`. **Decided:** the tenant database is the canonical catalog, and review in the MVP happens through the CLI with proxy links. MediaCMS is an optional connector for teams that want a web UI, not a requirement of this stage.

## Storage policy

- `archive`: long-term protection, never a normal working source. **Decided:** raw originals are always archived, permanently.
- `master`: durable, frequent-access, production-quality video. **Not a temporary cache.**
- `proxy`: playable reduced video; may be regenerated if the working master remains accessible.
- `transcripts`, `chapters`, `metadata`, `embeddings`: persistent and portable.
- Transcription-only audio and transient render files may be deleted after integrity verification and successful processing.

## YouTube import

If Vidyom itself published the media, **retain the exact uploaded source** in operational storage, so future edits never need to download from YouTube. If an owned video exists only on YouTube, first obtain an authorized local copy and cache/persist it as an imported asset before indexing. Do not assume the YouTube Data API provides a general video-file download endpoint or that the download has the same quality as the original.

## Acceptance criteria

A new original can be ingested end to end, searchable by keyword and semantic prompt, and clipped without archive access. Any failure in backup, transcription, index, or storage verification leaves the asset in a visible incomplete state rather than claiming readiness.
