---
title: "Open questions and validation"
sidebar_position: 1
---

# Open questions and validations

The architecture is settled at a high level, but these detailed choices should be validated before implementation:

1. **License compatibility:** Vidyom is licensed under Apache-2.0; audit external component licenses separately for compatibility.
2. **Deployment:** service topology and supported OS versions. The runtime and the documentation website share one repository.
3. **Workflow schema:** stable JSON Schema, expression restrictions, branching and loops, compensation/rollback and schema migration policy.
4. **Job queue:** PostgreSQL-based queue vs dedicated broker; concurrency and long-running task lease semantics.
5. **Headless keyring:** unattended secure unlock, service user provisioning and credential rotation policy.
6. **TLS/private networking:** mTLS vs SSH default; Tailscale Serve/NetBird interface binding; device identity and RBAC.
7. **MediaCMS:** exact API paths, how to register externally stored proxies, whether double transcoding can be disabled safely.
8. **VideoVortex:** verify indexing APIs/CLI, licensing, local index sync, moment retrieval, clip export and return formats.
9. **Rewind:** decide if optional visual review integration contributes enough value to prioritize.
10. **Audio transcription:** exact provider formats, upload limits, word timestamp accuracy, diarization and pricing policies.
11. **Master quality:** precise profiles for 1080p/4K, audio, aspect-ratio variants, and vertical crops.
12. **Archival retention:** raw originals, final edited masters, legal retention, immutability/versioning and cost policy.
13. **Publishing:** YouTube OAuth and upload quotas, video privacy, captions and retry reconciliation; Vimeo permissions and features.
14. **Public site:** auth boundaries between public search and private asset database, indexing pipeline, captions privacy and caching.
15. **Plugins:** process protocol, signing, lifecycle, sandboxing and compatibility guarantees.
16. **Audit retention:** storage backend, tamper evidence and compliance requirements.
17. **Domain:** a project domain has not been established. The source repository is [github.com/rafaelp/vidyom](https://github.com/rafaelp/vidyom).
18. **Cost optimization:** co-locate compute with GCS Standard where beneficial; account for egress and operation fees.

## Non-goals of the documentation site

This documentation site is not a video player, not an interactive workflow designer and not a substitute for the runtime. Its role is to make contracts and decisions explicit for implementation and review.
