---
title: "Media processing and transcription"
sidebar_position: 1
---

# Media processing and AI

## Proposed renditions

| Artifact | Typical purpose | Example for a two-hour recording |
| --- | --- | --- |
| Raw original | Untouched historical source | About 10 GB |
| Working master | High-quality new edits / crops | Variable, depending on source and codec |
| Proxy | Preview and timeline review | Roughly 300–900 MB at 720p, content-dependent |
| Speech audio | Efficient transcription input | ~22 MB at 24 kbps for 2 h |
| Transcript | Portable text and timestamps | A few hundred KB to a few MB |

These numbers are **illustrative**, not guaranteed quality or compression targets. A master must preserve enough quality for subsequent edits, including vertical reframing and zoom; the 720p proxy is not an acceptable sole production master.

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

Smaller audio files reduce storage and transfer, but do **not automatically reduce transcription tokens or a per-minute API bill**. The billed amount depends on provider, model and pricing unit. A speech-centric pipeline avoids unnecessary video/frame processing. Voice activity detection (VAD), e.g. Silero VAD, may remove long silences, but preserve a precise time map back to the source video. Keep processing metadata and provider/model versions for reproducibility.

## AI index

Proposed processing: faster-whisper (local transcription), topic/chapter extraction, narrative boundary detection, summaries, full-text indexing in PostgreSQL and embeddings with pgvector. VideoVortex can provide CLI-oriented retrieval and clip utilities; validate its actual integration contracts before treating it as a critical dependency.

## Provider limitations

Respect provider-specific supported audio formats, maximum file size, duration limits, quotas and pricing. Chunk long audio with slight overlap as needed and reconstruct accurate timestamps. Transcripts must have a clear revision trail if humans fix names or speech recognition errors.
