# Changelog

All notable changes to this project are recorded here (D20). Vidyom has made no release yet: nothing has been tagged or published.

## Unreleased

The first executable version. It runs on one host, natively or through Docker Compose, and the whole lifecycle works end to end: import an original, ingest, search, a clip through two approval gates, publish, export `public.db`, query the public library, withdraw. [`docs/status.md`](docs/status.md) states what is real and exercised end to end, what is tested only against mocks, every skeleton, every gap and every **Validate** item.

### Added

- `vidyomd`, `vidyom-worker` and the `vidyom` CLI: multi-tenant daemon with one SQLite database per tenant, Redis job dispatch, resumable uploads, device pairing, encrypted vault, HTTPS with certificate pinning, Unix socket with peer identity.
- Workflow engine: checkpoints, retry by side-effect class, `needs_attention`, approval gates, three-level hooks, transactional outbox Event Bus, budgets, hash-chained audit.
- Media: `local` and `gcs` drivers, FFmpeg nodes, versioned profiles, encrypted per-tenant cache.
- AI: Python worker with `fake` and `real` engines, chunking, hybrid search with FTS5 and sqlite-vec, story detection through one OpenAI-compatible driver, sensitive segments.
- Publishing: `mock` and `youtube` connectors, `public.db` export, `local-file` shipping, skeleton connectors that declare themselves unavailable.
- Public library: Next.js site reading `public.db` read-only.
- Plugins over JSON-RPC stdio with OS sandboxing, structured logs, metrics and traces with `file`, `loki`, `prometheus` and `otlp` connectors.
- `deploy/`: Dockerfiles and Docker Compose with an optional observability profile. `tests/e2e`: the acceptance scenario, run with `make e2e`. `scripts/demo.sh`: the same scenario for a person to watch. CI in `.github/workflows/ci.yml`.

### Known limits

The AI engine used by tests and the demo is the fake one, which proves the path, not the quality. YouTube and GCS are exercised against local mocks only. See `docs/status.md`.
