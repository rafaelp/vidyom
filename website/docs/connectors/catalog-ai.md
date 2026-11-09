---
title: "Catalog, search, and AI adapters"
sidebar_position: 1
---

# Catalog, search, and AI adapters

## Private catalog

**Decided:** The canonical private catalog is Vidyom's tenant database. In the MVP, review happens through the CLI with proxy links.

**MediaCMS** is an optional connector, with one instance per tenant, for teams that want a web UI for organized videos, categories, tags and playback. It should consume a manageable proxy; storage of raw originals and persistent masters remains outside MediaCMS. **Validate:** its API, externally stored proxies, double transcoding and the one-instance-per-tenant model before implementing the adapter.

## Video search and clipping utilities

**VideoVortex** is the proposed CLI-oriented complement for transcript search, structured retrieval, moments and clipping operations. It is not the canonical index: the tenant database is. The daemon must synchronize stable asset IDs and timestamps with the tenant database if VideoVortex maintains a separate local SQLite index. **Validate:** the real APIs/CLI, licensing and synchronization behavior with a proof of concept.

**Decided:** **Rewind** is an optional visual editor for transcript navigation and montage creation. It is not part of the required MVP, but it is prioritized right after the MVP (after Phase 3) as an optional tool for editors. **Arcadeia** was considered but not selected for the core architecture.

## AI and transcription

**Decided:** Transcription runs locally with **faster-whisper large-v3**. Embeddings run locally with **BGE-M3** (multilingual). Story and chapter detection uses an LLM through a single **OpenAI-compatible API driver**, so any compatible endpoint works (OpenAI, OpenRouter, vLLM, Ollama and others); the hosted provider is chosen per installation. Each configured LLM endpoint is classified as `local` or `external`. **Validate:** accuracy, word timestamps and diarization of faster-whisper large-v3 on the target content.

Contracts should include language, time-alignment format, model/version, confidence metadata, cost/usage metadata, input limits and privacy properties.

**Decided:** The default is `aiProcessing: local_only`, covering transcription, embeddings and LLM calls. An admin can mark a collection or an asset as `external_allowed`; Vidyom records who authorized it and when (consent record). `local_only` collections may only use a `local` LLM endpoint, for example Ollama or vLLM on the host. Without one, story detection is unavailable for them and clip selection falls back to manual review.

**Decided:** Transcripts are private by default. Editors can mark sensitive segments (for example names, health or minors); marked segments are excluded from external AI processing, from `public.db` and from public captions, and are redacted in exports. A transcript or captions reach the public library or a destination only together with an approved public video.

## Search

**Decided:** Full-text search uses SQLite FTS5 and vector search uses sqlite-vec, both inside the tenant database. **Validate:** sqlite-vec performance and maturity; it has no approximate index yet, so asset-scoped search is the main path and tenant-wide semantic search must be load-tested.

## Public library

**Decided:** Per tenant, the daemon exports a read-only **`public.db`** (SQLite) containing only approved public records and their embeddings, and ships it to the public site's host. The daemon is never exposed publicly.

The **Next.js** public library (**Decided**) queries `public.db`, computes query embeddings on CPU for semantic search, and consumes only approved records. It offers natural-language and keyword search over public transcripts, chapters, topics and teacher names; links to platform-native playback; and links each approved clip to its parent original where appropriate. Public access is never granted simply because a source exists in the private catalog.
