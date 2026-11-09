---
title: "Open questions and validation"
sidebar_position: 1
---

# Open questions and validations

The architecture is settled at a high level. Two decisions are still open, and the rest are validations (proofs of concept) to complete before implementation.

**Decisions:**

1. **Trademark check:** the `vidyom` name still needs a trademark search at INPI, USPTO and WIPO. The domain is `vidyom.org` and the source repository is [github.com/rafaelp/vidyom](https://github.com/rafaelp/vidyom).
2. **Phase 5:** the scope and order of the [roadmap phase](../operations/roadmap.md) that holds the Event Bus, budgets and the audit.

**Validations:**

3. **Search and backup:** **Validate:** sqlite-vec performance at tenant scale (no approximate index yet); Litestream with GCS.
4. **Redis:** **Validate:** AOF durability and reconciliation under crash tests.
5. **Master key unsealing:** **Validate:** unsealing per platform (TPM/systemd-creds on Linux, Keychain on macOS, DPAPI/TPM on Windows) and with a KMS on the target hosts.
6. **MediaCMS:** **Validate:** API paths, externally stored proxies, double transcoding, one instance per tenant.
7. **VideoVortex:** **Validate:** real APIs/CLI, license, synchronization with the tenant database.
8. **Transcription:** **Validate:** accuracy, word timestamps and diarization of faster-whisper large-v3 on the target content, including the effect of extracting the audio from the master instead of the raw file.
9. **Publishing APIs:** **Validate:** YouTube, Vimeo, PeerTube and Instagram (OAuth, quotas, captions, retry reconciliation).
10. **License compatibility:** **Validate:** every dependency against Apache-2.0.
11. **Archive purge:** **Validate:** LGPD legal review of deferring the archive purge until the minimum storage duration ends.
12. **AI on macOS:** **Validate:** performance of local AI (transcription and embeddings) on macOS without a GPU in Docker, on CPU or through a configured endpoint.
13. **Public library hosting:** **Validate:** BGE-M3 query embeddings on CPU within the size and memory limits of the site's host (Vercel included), and how an updated `public.db` reaches it.

## Non-goals of the documentation site

This documentation site is not a video player, not an interactive workflow designer and not a substitute for the runtime. Its role is to make contracts and decisions explicit for implementation and review.
