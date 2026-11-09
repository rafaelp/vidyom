---
title: "Media processing and transcription"
sidebar_position: 1
---

# Media processing and AI

## Proposed renditions

| Artifact | Typical purpose | Example for a two-hour recording |
| --- | --- | --- |
| Raw original | Untouched historical source | About 10 GB |
| Working master | High-quality new edits / crops | Variable, depending on source and profile |
| Proxy | Preview and timeline review | Roughly 300–900 MB at 720p, content-dependent |
| Speech audio | Efficient transcription input | ~22 MB at 24 kbps for 2 h |
| Transcript | Portable text and timestamps | A few hundred KB to a few MB |

These numbers are **illustrative**, not guaranteed quality or compression targets. A master must preserve enough quality for subsequent edits, including vertical reframing and zoom; the 720p proxy is not an acceptable sole production master.

**Decided:** The proxy is MP4 H.264 at 720p, and the ASR audio is Opus mono at 16 kHz and 24–32 kbps. Neither is ever upscaled beyond the source.

## Master profiles

**Decided:** The profile depends on the source resolution.

| Profile | Source | Video | Audio |
| --- | --- | --- | --- |
| `master-1080p` | Up to 1080p | H.264 High, CRF 18, preset slow, max 1080p | AAC 192 kbps |
| `master-2160p-hevc` | 2160p or more | HEVC, CRF 20, 4K kept for reframing and zoom | AAC 192 kbps |

## Timeline alignment

**Decided:** The master, the proxy and the ASR audio must match the raw file's duration within max(100 ms, 1 frame). Sources with variable frame rate or broken timestamps are normalized to constant frame rate in the master before transcription, and transcripts use the master's timeline.

## FFmpeg examples

Generate a playback proxy:

```bash
ffmpeg -i original.mp4 \
  -vf 'scale=-2:720' \
  -c:v libx264 -preset slow -crf 28 \
  -c:a aac -b:a 96k -movflags +faststart \
  preview.mp4
```

Extract speech audio **directly from the best available source**, not by re-encoding an already compressed proxy:

```bash
ffmpeg -i original.mp4 -vn -ac 1 -ar 16000 \
  -c:a libopus -b:a 24k speech.opus
```

## Transcription and token cost

**Decided:** Transcription runs locally with faster-whisper large-v3, so no per-minute API bill applies by default. **Validate:** accuracy, word timestamps and diarization on the target content. Smaller audio files reduce storage and transfer, but do **not automatically reduce transcription tokens or a per-minute API bill** if an external provider is ever allowed for a collection marked `external_allowed`; the billed amount then depends on provider, model and pricing unit. A speech-centric pipeline avoids unnecessary video/frame processing. Voice activity detection (VAD), e.g. Silero VAD, may remove long silences, but preserve a precise time map back to the source video. Keep processing metadata and provider/model versions for reproducibility.

## AI index

**Decided:** Models are chosen as follows.

- Transcription: faster-whisper large-v3, local.
- Embeddings: BGE-M3 (multilingual), local.
- Story and chapter detection: an LLM through a single OpenAI-compatible API driver (OpenAI, OpenRouter, vLLM, Ollama and similar). Each endpoint is classified as `local` or `external`; `local_only` collections may only use a local endpoint, and without one story detection is unavailable and clip selection falls back to manual review.

**Decided:** Full-text search uses SQLite FTS5 and vector search uses sqlite-vec in the tenant database. **Validate:** sqlite-vec performance and maturity; it has no approximate index yet, so asset-scoped search is the main path and tenant-wide semantic search must be load-tested. VideoVortex can provide CLI-oriented retrieval and clip utilities; validate its actual integration contracts before treating it as a critical dependency.

### Transcript segmentation

**Decided:** The transcript is split into speech windows of 30–60 s, cut at sentence boundaries, with about 15% overlap and start and end timestamps on each window. Chapter summaries are embedded as a second level. When the embedding model changes, reindexing runs in the background side by side (`embedding_model` is stored per vector), and the new index replaces the old one only when complete.

## Provider limitations

Respect provider-specific supported audio formats, maximum file size, duration limits, quotas and pricing. Chunk long audio with slight overlap as needed and reconstruct accurate timestamps. Transcripts must have a clear revision trail if humans fix names or speech recognition errors.
