---
title: "Catalog, search, and AI adapters"
sidebar_position: 1
---

# Catalog, search, and AI adapters

## Private catalog

**MediaCMS** is the proposed first private web catalog for organized videos, categories, tags and playback. It should consume a manageable proxy; storage of raw originals and persistent masters remains outside MediaCMS. Validate its ingestion/transcoding behavior and API before implementing the adapter.

## Video search and clipping utilities

**VideoVortex** is the proposed CLI-oriented complement for transcript search, structured retrieval, moments and clipping operations. It is not automatically the canonical index. The daemon must synchronize stable asset IDs and timestamps if VideoVortex maintains a separate local SQLite index. Exact supported CLI commands, licensing distinctions and integration behavior need proof-of-concept validation.

**Rewind** is an optional visual editor for transcript navigation and montage creation, not part of the required MVP. **Arcadeia** was considered but not selected for the core architecture.

## AI and transcription

Start with faster-whisper as a local transcription option and a configurable AI model adapter for summaries, chapter detection, narrative start/end analysis and embeddings. Providers may be local or remote. Contracts should include language, time-alignment format, model/version, confidence metadata, cost/usage metadata, input limits and privacy properties.

## Public library

A proposed **Next.js** public library consumes only approved records. It offers natural-language and keyword search over public transcripts, chapters, topics and teacher names; links to platform-native playback; and links each approved clip to its parent original where appropriate. Public access is never granted simply because a source exists in the private catalog.
