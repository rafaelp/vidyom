---
title: "Open questions and deployment checks"
sidebar_position: 1
---

# Open questions and deployment checks

## Open question

**Trademark check:** the `vidyom` name still needs a trademark search at INPI, USPTO and WIPO. The domain is `vidyom.org` and the source repository is [github.com/rafaelp/vidyom](https://github.com/rafaelp/vidyom).

## Deployment checks

Results depend on your hosts, content and providers. Check these on your own deployment before relying on them.

1. **Search and backup:** sqlite-vec performance at your tenant scale (there is no approximate index yet); Litestream with GCS.
2. **Redis:** AOF durability and reconciliation under crash tests.
3. **Master key unsealing:** unsealing per platform (TPM/systemd-creds on Linux, Keychain on macOS, DPAPI/TPM on Windows) and with a KMS on your hosts.
4. **MediaCMS:** API paths, externally stored proxies, double transcoding, one instance per tenant.
5. **VideoVortex:** its APIs and CLI, its license, and synchronization with the tenant database.
6. **Transcription:** accuracy, word timestamps and diarization of faster-whisper large-v3 on your content, including the effect of extracting the audio from the master instead of the raw file.
7. **Publishing APIs:** YouTube, Vimeo, PeerTube and Instagram (OAuth, quotas, captions, retry reconciliation).
8. **License compatibility:** review every dependency you add against Apache-2.0.
9. **Archive purge:** review with your own counsel how deferring the archive purge until the minimum storage duration ends fits the LGPD and equivalent regulations.
10. **AI on macOS:** performance of local AI (transcription and embeddings) on macOS without a GPU in Docker, on CPU or through a configured endpoint.
11. **Public library hosting:** BGE-M3 query embeddings on CPU within the size and memory limits of the site's host (Vercel included), and how an updated `public.db` reaches it.

## Non-goals of the documentation site

This documentation site is not a video player, not an interactive workflow designer and not a substitute for the runtime. Its role is to make contracts and behavior explicit.
