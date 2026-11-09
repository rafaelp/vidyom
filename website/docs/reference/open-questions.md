---
title: "Open questions and validation"
sidebar_position: 1
---

# Open questions and validations

The architecture is settled at a high level. One decision is still open, and the rest are validations (proofs of concept) to complete before implementation.

**Decisions:**

1. **Trademark check:** the `vidyom` name still needs a trademark search at INPI, USPTO and WIPO. The domain is `vidyom.org` and the source repository is [github.com/rafaelp/vidyom](https://github.com/rafaelp/vidyom).

**Validations:**

2. **Search and backup:** **Validate:** sqlite-vec performance at tenant scale (no approximate index yet); Litestream with GCS.
3. **Redis:** **Validate:** AOF durability and reconciliation under crash tests.
4. **Master key unsealing:** **Validate:** unsealing per platform (TPM/systemd-creds on Linux, Keychain on macOS, DPAPI/TPM on Windows) and with a KMS on the target hosts.
5. **MediaCMS:** **Validate:** API paths, externally stored proxies, double transcoding, one instance per tenant.
6. **VideoVortex:** **Validate:** real APIs/CLI, license, synchronization with the tenant database.
7. **Transcription:** **Validate:** accuracy, word timestamps and diarization of faster-whisper large-v3 on the target content.
8. **Publishing APIs:** **Validate:** YouTube, Vimeo, PeerTube and Instagram (OAuth, quotas, captions, retry reconciliation).
9. **License compatibility:** **Validate:** every dependency against Apache-2.0.
10. **Archive purge:** **Validate:** LGPD legal review of deferring the archive purge until the minimum storage duration ends.
11. **AI on macOS:** **Validate:** performance of local AI (transcription and embeddings) on macOS without a GPU in Docker, on CPU or through a configured endpoint.

## Non-goals of the documentation site

This documentation site is not a video player, not an interactive workflow designer and not a substitute for the runtime. Its role is to make contracts and decisions explicit for implementation and review.
