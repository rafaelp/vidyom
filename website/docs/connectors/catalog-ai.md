---
title: "Catalog, search, and AI adapters"
sidebar_position: 1
---

# Catalog, search, and AI adapters

## Private catalog

The canonical private catalog is Vidyom's tenant database. Review happens through the CLI with proxy links.

**MediaCMS** is an optional connector, with one instance per tenant, for teams that want a web UI for organized videos, categories, tags and playback. It consumes a manageable proxy; storage of raw originals and persistent masters remains outside MediaCMS. Before relying on it, test the MediaCMS API, externally stored proxies, double transcoding and the one-instance-per-tenant model on your own deployment.

## Video search and clipping utilities

**VideoVortex** is the CLI-oriented complement for transcript search, structured retrieval, moments and clipping operations. It is not the canonical index: the tenant database is. If VideoVortex maintains a separate local SQLite index, the daemon synchronizes stable asset IDs and timestamps with the tenant database. Check the VideoVortex APIs/CLI, licensing and synchronization behavior on your own deployment.

**Rewind** is an optional visual editor for transcript navigation and montage creation. **Arcadeia** was considered but not selected for the core architecture.

## AI and transcription

Transcription runs locally with **faster-whisper large-v3**. Embeddings run locally with **BGE-M3** (multilingual). Story and chapter detection uses an LLM through a single **OpenAI-compatible API driver**, so any compatible endpoint works (OpenAI, OpenRouter, vLLM, Ollama and others); the hosted provider is chosen per installation. Each configured LLM endpoint is classified as `local` or `external`. Test the accuracy, word timestamps and diarization of faster-whisper large-v3 on your own content.

Contracts include language, time-alignment format, model/version, confidence metadata, cost/usage metadata, input limits and privacy properties.

The default is `aiProcessing: local_only`, covering transcription, embeddings and LLM calls. An admin can mark a collection or an asset as `external_allowed`; Vidyom records who authorized it and when (consent record). `local_only` collections may only use a `local` LLM endpoint, for example Ollama or vLLM on the host. Without one, story detection is unavailable for them and clip selection falls back to manual review.

Transcripts are private by default. Editors can mark sensitive segments (for example names, health or minors); marked segments are excluded from external AI processing, from `public.db` and from public captions, and are redacted in exports. A transcript or captions reach the public library or a destination only together with an approved public video.

## Search

Full-text search uses SQLite FTS5 and vector search uses sqlite-vec, both inside the tenant database. sqlite-vec has no approximate index yet, so asset-scoped search is the main path; load-test tenant-wide semantic search on your own deployment.

## Public library

Per tenant, the daemon exports a read-only **`public.db`** (SQLite) containing only approved public records and their embeddings, and ships it to the public site's host. The daemon is never exposed publicly.

The **Next.js** public library queries `public.db`, computes query embeddings on CPU for semantic search, and consumes only approved records. It offers natural-language and keyword search over public transcripts, chapters, topics and teacher names; links to platform-native playback; and links each approved clip to its parent original where appropriate. Public access is never granted simply because a source exists in the private catalog.
