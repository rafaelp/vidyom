---
title: "Open questions and validation"
sidebar_position: 1
---

# Open questions and validations

The architecture is settled at a high level. These items are still open: the first two are decisions, the rest are validations (proofs of concept) to complete before implementation.

**Decisions:**

1. **Copyright and privacy:** permissions for speakers and participants, publication authorizations and handling of sensitive transcripts.
2. **Domain and names:** a project domain has not been established; package/registry names and a trademark check are pending. The source repository is [github.com/rafaelp/vidyom](https://github.com/rafaelp/vidyom).

**Validations:**

3. **Search and backup:** **Validate:** sqlite-vec performance at tenant scale (no approximate index yet); Litestream with GCS.
4. **Redis:** **Validate:** AOF durability and reconciliation under crash tests.
5. **Master key unsealing:** **Validate:** TPM/systemd-creds and KMS unsealing on the target hosts.
6. **MediaCMS:** **Validate:** API paths, externally stored proxies, double transcoding, one instance per tenant.
7. **VideoVortex:** **Validate:** real APIs/CLI, license, synchronization with the tenant database.
8. **Transcription:** **Validate:** accuracy, word timestamps and diarization of faster-whisper large-v3 on the target content.
9. **Publishing APIs:** **Validate:** YouTube, Vimeo, PeerTube and Instagram (OAuth, quotas, captions, retry reconciliation).
10. **License compatibility:** **Validate:** every dependency against Apache-2.0.
11. **Archive purge:** **Validate:** LGPD legal review of deferring the archive purge until the minimum storage duration ends.

## Non-goals of the documentation site

This documentation site is not a video player, not an interactive workflow designer and not a substitute for the runtime. Its role is to make contracts and decisions explicit for implementation and review.
