# Implementation decisions

The maintainer delegated all open decisions for the first executable version. This log records them as `I1, I2, ...`. They are **implementation decisions**, distinct from the maintainer's `D1–D59` in spec §21, which they never change or relabel. Each one states the choice, the reason and what would make us revisit it.

This log was written before the components exist, so what it says a version does is a **target** until the owning component commits it. Only I1, the contract packages and `scripts/` exist so far.

**Validate** marks a working default that rests on a §17.2 validation still unproven. Tests in this version exercise it against local fakes; it has not been proven on real targets.

Components record their own decisions in `docs/decisions/<component>.md`, using the number ranges in section 8 of the [master plan](plans/00-master-plan.md).

---

## Storage and queue

### I1. SQLite driver: `github.com/rqlite/go-sqlite3` with `sqlite-vec` static bindings

- **Choice.** cgo driver `github.com/rqlite/go-sqlite3` v1.54.0 (MIT, maintained fork of `mattn/go-sqlite3`, driver name `sqlite3`) plus `github.com/asg017/sqlite-vec-go-bindings/cgo` v0.1.6 (`vec.Auto()`). Only `vidyomd` links them. `internal/persistence/sqlitedriver` is the single place that opens a database.
- **Why this fork.** Upstream `mattn` compiles FTS5 only under `-tags sqlite_fts5`, so a plain `go test ./...` runs without FTS5 and agents or CI can silently test the wrong thing. The fork compiles FTS5 unconditionally, needs no tags. Verified by `internal/persistence/sqlitedriver`'s tests on macOS arm64 and on Linux aarch64 in `golang:1.27-bookworm`: SQLite 3.54.1, `vec_version` v0.1.6, FTS5, asset-partitioned `vec0` KNN, concurrent writers.
- **The catch, handled in one place.** The fork's compile-time defaults exist for rqlite's Raft log: `SQLITE_DEFAULT_SYNCHRONOUS=0`, `SQLITE_DEFAULT_WAL_SYNCHRONOUS=0`, `SQLITE_DEFAULT_WAL_AUTOCHECKPOINT=0`. Left alone they would lose committed job state on power failure and grow the WAL forever. `sqlitedriver` therefore sets `journal_mode=WAL`, `synchronous=FULL`, `wal_autocheckpoint=1000`, `foreign_keys=ON` and `busy_timeout` on **every connection** through `ConnectHook`, and its test asserts those values, FTS5 and sqlite-vec. Opening SQLite any other way is forbidden.
- **Cross-compilation.** `vidyom` and `vidyom-worker` are `CGO_ENABLED=0`, static, and cross-compile freely; `make check-static` fails if either links a database package. `vidyomd` needs cgo: the primary path is the Docker image, native builds need a C compiler and the `sqlite3.h` header, and release builds use one runner or `buildx` per platform rather than cross-compiling. On Debian and Ubuntu the sqlite-vec bindings need `libsqlite3-dev` for that header only, at build time: the Dockerfile and CI must install it. On macOS the SDK header works and emits harmless deprecation warnings (`make build` silences them).
- **Rejected.** `ncruces/go-sqlite3` is pure Go with FTS5, but the sqlite-vec bindings for it pin v0.17.1 from 2024, and the `vec1` it ships is SQLite's own extension (sqlite.org/vec1), not the sqlite-vec that D5 names. `modernc.org/sqlite` cannot load sqlite-vec.
- **Revisit if.** The maintainer would accept SQLite's official `vec1` in place of sqlite-vec: `ncruces/go-sqlite3` is pure Go with FTS5 and `vec1`, which would drop cgo from `vidyomd` and let all three binaries cross-compile, at the cost of Wasm-translated performance and of changing D5, so it needs a maintainer decision and a load test. Also if sqlite-vec gets first-class support in a pure-Go driver; the fork diverges or stops tracking SQLite; `sqlite-vec` bindings stagnate past a needed fix. **Validate:** sqlite-vec performance at tenant scale (D5); Windows native build with MinGW.

### I2. Redis client: `github.com/redis/go-redis/v9`

- **Choice.** go-redis v9 for Streams and consumer groups. Unit tests use `miniredis`; AOF and crash tests use real Redis 8.8 (`mise exec redis@8.8.0 -- redis-server` or Docker).
- **Why.** Maintained by Redis, pure Go, full Streams API including `XAUTOCLAIM`.
- **Revisit if.** A required command is missing. **Validate:** AOF durability and reconciliation under crash tests (D4).

### I9. Local and dev storage: a `local` driver

- **Choice.** `connectors/storage/local` stores each role under its own root directory, with the tenant prefix inside, full capability set (range reads, server-side copy, checksums), atomic rename on write, and an optional access log used to prove that clipping never reads `archive`. Roles must have disjoint roots, enforced by the same validator as GCS (R28). `usage` remains declared per instance.
- **Why.** The whole scenario must run with no cloud account. §7.2 already lists local/NAS as an extension.
- **Revisit if.** Needed: an S3 test driver for §15.4. In this version §15.4 is proven by switching `master` between two `local` instances without touching a workflow.

### I31. Litestream as an optional sidecar

- **Choice.** The Compose file offers Litestream as a sidecar profile replicating `system.db` and each `tenant.db`. It is not part of tests and not required to run. Replica configuration is generated per tenant at deploy time by `integration`.
- **Why.** D5 requires continuous backup, but it needs a target provider and tenant list. **Validate:** Litestream with GCS (D5).
- **Revisit if.** The validation fails or the tenant list makes generated configuration unworkable.

### I34. Tenant configuration lives in files

- **Choice.** `<config-dir>/daemon.json` holds daemon settings. `<config-dir>/tenants/<tenant>/{storages,publishing,ai,observability,hooks}.json` holds per-tenant settings (§14.2 names, D6). The daemon loads them at start and on `SIGHUP`, and validates them (disjoint namespaces, no secrets, only `credentialsRef`). Workers receive only what a lease needs.
- **Why.** Matches §14.2 and §7.1 ("connector settings live outside the workflows"), is reviewable and exportable without credentials.
- **Revisit if.** Tenant admins need to change connectors through the API.

---

## Runtime and protocols

### I3. Job event streaming: Server-Sent Events

- **Choice.** SSE on `GET /jobs/{id}/events` and `GET /events/stream`. `id:` is a per-job (or per-tenant) monotonic sequence, `Last-Event-ID` resumes from it, comments every 15 s keep connections alive.
- **Why.** Plain HTTP/1.1 over both Unix sockets and TLS, no new dependency, reconnection built in, one-way is all §5.3 needs. WebSocket adds a bidirectional channel nobody uses, gRPC adds code generation.
- **Revisit if.** Clients need to send on the same stream.

### I4. Worker-to-daemon protocol

- **Choice.** Dispatch: Redis Streams, one stream per pool (`vidyom:tasks:<pool>`), consumer group per pool, pointers only. Everything else: JSON over HTTP on the daemon's normal listener under `/api/v1/worker/...`, with a bearer **worker token** (`vidyomd init` generates it, `system.db` stores a hash, Compose mounts it as a secret). A task's life: `lease` → `heartbeat`* (progress, returns cancel flag) → `complete` or `fail`, then `XACK`. Workers re-deliver `complete` until acknowledged; it is idempotent on task ID and lease. Types in `internal/contract/workerproto`.
- **Why.** D5 lets workers use the API or Redis. Having the daemon commit results over HTTP gives one writer and a synchronous acknowledgment, so a crash between "done" and "recorded" re-delivers rather than loses. The lease makes a duplicated stream entry harmless, which is what "no job lost or executed twice" (§15.5) needs after a reconciliation.
- **Revisit if.** Remote workers need a different trust model than a shared token (per-worker tokens are already possible in the schema).

### I5. Files between nodes: shared scratch volume and `scratch://` URIs

- **Choice.** Nodes that produce files write under `<scratch-dir>/<tenant>/<job-ulid>/<node-id>/` and return `scratch://<job-ulid>/<node-id>/<file>`. The tenant is taken from the lease, never from the URI. All workers mount the same volume. Files are deleted at job end (D22). `scratch://` is internal: the API rejects it as a user input (R20 lists only four forms).
- **Why.** On one host (D41), a shared volume avoids re-uploading every intermediate. Node outputs stay small references.
- **Revisit if.** Workers run on several hosts: then artifacts go through the `proxy` role with a temporary prefix behind the same URI. That is **Future**.

### I6. AI worker transport and method set

- **Choice.** JSON-RPC 2.0, one JSON object per line (NDJSON). Transports: Unix domain socket for the service (`/run/vidyom/ai.sock`, mode 0600), stdio for tests, loopback TCP with a required `initialize` token for native Windows. Methods: `initialize`, `shutdown`, `health`, `transcribe`, `embed`; notifications `$/progress`, `$/cancelRequest`. Defined in `schemas/ai-worker-rpc.v1.json` and `internal/contract/aiworker`. Files by local path, never bytes on the wire. Both `vidyom-worker --pools ai` (transcription, batch embeddings) and `vidyomd` (query embeddings for `GET /search`) are clients of the same server.
- **Why.** D31 says the Python worker talks JSON-RPC. Python stays free of Redis, storage and secrets. A separate process keeps heavy models loaded once. Chunking (D15) and the LLM driver (D9) stay in Go.
- **Revisit if.** Embeddings need batching across clients or the Python worker needs to scale apart from the Go pool.

### I7. Where each node runs: pools

- **Choice.** `node.Descriptor.Pool` is `engine` (inline: `flow.*`, `workflow.execute`, `approval.request`, `event.emit`), `daemon` (needs the database or the vault: `catalog.*`, `ai.index`, `ai.find_segment`, `ai.analyze`, `ai.editorial`, `ai.derive_transcript`, all `publish.*`, `catalog.public.sync`, `mediacms.register`), `media` (`storage.*`, `video.*`, `audio.*`), `ai` (`ai.transcribe`, `ai.embed`, `ai.export_transcript`). All except `engine` go through Redis, including `daemon`, so reconciliation has one model. The authoritative mapping is `schemas/node-catalog.v1.json`.
- **Why.** D5 forbids workers from opening databases. §5.1 reserves the worker for heavy isolatable work; uploads are I/O bound and need vault credentials, and running them in the daemon avoids giving workers publishing tokens.
- **Revisit if.** A large number of concurrent uploads hurts the daemon; a `publish` pool can then be added additively.

### I35. Node outputs are JSON with a 32 MiB cap

- **Choice.** Outputs are JSON stored with the node execution. Transcripts and vectors travel inline because the spec's own example passes `${nodes.transcribe.output}`. A 3-hour recording's transcript with word timestamps is a few MB, and its vectors about the same. Beyond the cap the task fails with `output_too_large`.
- **Why.** The daemon never reads scratch (I5), and §5 asks for files by reference, not for structured data.
- **Revisit if.** Long recordings exceed the cap; `ai.transcribe` can then write through a new worker artifact route.

### I36. Idempotency keys do not include the attempt

- **Choice.** `node.IdempotencyKey(tenant, jobULID, nodeID)`: SHA-256 hex, stable across retries. A `flow.foreach` item runs as its own child job, which gives it its own key. Connectors receive it for uploads and publications.
- **Why.** The website (`architecture/runtime.md`) says keys derive from "workflow/job/node/attempt context". An attempt-specific key would make every retry look new and allow duplicates, which is the failure §15.5 forbids. The website wording is flagged below.
- **Revisit if.** A destination needs a per-attempt nonce; it can be added beside the key.

### I42. Plugin protocol framing: NDJSON

- **Choice.** D3 fixes JSON-RPC 2.0 over stdio. Framing is one JSON object per line, as MCP does, not LSP's `Content-Length` headers. Method set in `internal/contract/plugin`. Remote plugins over HTTPS are a documented gap in this version: `runtime: "remote"` validates and refuses to start.
- **Why.** Easiest for any language; Roadmap Phase 4 names stdio only.
- **Revisit if.** A plugin needs payloads containing raw newlines (JSON encoding already escapes them).

### I22. Workflow packaging and trigger types

- **Choice.** `workflows/*.json` are embedded in `vidyomd`; `<config-dir>/workflows/` adds or overrides by `id` and `version`. Clients can also submit an inline definition (`vidyom workflow run ./file.json`), subject to the same validation and permissions (§9.4). Triggers: `cli`, `event` and `schedule` (`every: "24h"`) work; `webhook` validates and is refused with `not_implemented`, as §6.2 allows ("once those types are implemented"). One trigger per workflow, so withdrawal and visibility changes have their own small workflows that run the same `catalog.public.sync` node.
- **Why.** Keeps the schema of §6.2 (`trigger` is one object). Cron parsing is not needed for the maintenance workflows.
- **Revisit if.** Cron expressions are requested (`cron` is reserved in the schema).

### I23. CEL environment

- **Choice.** CEL conditions see `inputs`, `nodes` (with `.output`), `event`, `job` (read only, no secrets), evaluated with `cel-go`, cost limit 100 000, no macros that could iterate beyond the limit, 10 ms-scale evaluation. Compile errors fail validation; evaluation errors fail the node.
- **Why.** D2 sets the language and asks for cost limits, not their values.
- **Revisit if.** Real workflows hit the limit.

### I33. A blocked budget pauses the job

- **Choice.** When a node marked `paid` in the catalog is about to dispatch and the tenant is over 100% with no active override, the job moves to `paused` with reason `budget_exceeded`, not `failed` and not `needs_attention`. `vidyom budget override` then `vidyom job resume` continues it.
- **Why.** D19 blocks new paid operations; the job is healthy and resumable, and `needs_attention` (D11) means an unknown external outcome.
- **Revisit if.** Operators prefer a distinct state.

### I41. Approval details

- **Choice.** Kinds: `clip_selection`, `publication`, `visibility_change`, `metadata_change` (the last two are `publisher`-level, per D7) and `other`, which requires an explicit `approvers`. Default `timeoutSeconds` is none, default `onTimeout` is `fail`. `approvers` is checked against the kind's floor when the workflow validates and again when a decision arrives; a list that would loosen the rule is a validation error. A rejected approval completes the node with `decision: "rejected"`, the workflow's `flow.if` routes it, so the job can finish `completed` with the asset `rejected` rather than `failed`.
- **Why.** §6.2 lists parameters but no defaults. Rejection is a business outcome, as the `flow.if` example in §6.2 shows.
- **Revisit if.** Rejections should count as failed jobs in metrics.

---

## API and identity

### I13. Resource JSON: `id` is the friendly ID, `ulid` is the technical key

- **Choice.** `{"id": "ORI-001", "ulid": "01J..."}`. Paths accept either. Where only one is possible (references between records), the friendly ID is used in JSON, the ULID in database foreign keys.
- **Why.** §12.2 shows `"id": "ORI-001"` and R5 requires returning both.
- **Revisit if.** Clients need stability across tenants (ULIDs already give that).

### I14. `CLIENT-` numbering is system-wide

- **Choice.** Devices live in `system.db`, numbered by one global sequence. All other prefixes are per tenant.
- **Why.** D46 says sequences are per prefix and per tenant, but D12 stores devices in `system.db` and pairing is tenant-free, so a device has no tenant to number it in. D46's other prefixes are unaffected.
- **Revisit if.** Devices become tenant-bound.

### I15. REST conventions

- **Choice.** Request and response bodies are `snake_case`, as §12 resources are. Workflow JSON and config files stay `camelCase`, as §6 and §7 show. `Idempotency-Key` header on mutations; a run request's `idempotency_key` field is accepted too and must match when both are sent. List responses are `{ "items": [...], "next_cursor": "..." }` with `limit` and `cursor` parameters. `Vidyom-Request-Id` on every response. W3C `traceparent` accepted and propagated (§10.1). Errors: `{ "error": { "code", "message", "request_id", "details" } }`.
- **Why.** The spec is silent or mixed. The website's example mixes `workflowVersion` with `job_id`; flagged below.
- **Revisit if.** Clients prove the mix matters.

### I16. Tenant identifier is a slug

- **Choice.** `tenant_id` and the `Vidyom-Tenant` value are a slug matching `^[a-z][a-z0-9-]{1,62}$`, also used as the directory name.
- **Why.** §12.2 shows `"tenant_id": "acme"`, §11.1 shows `vidyom tenant use acme`. Path traversal is excluded by the pattern.
- **Revisit if.** Tenants must be renamed (would need an indirection).

### I17. Bootstrap: `vidyomd init` and first admin

- **Choice.** `vidyomd init --tenant acme --admin admin@example.com` creates `system.db`, the first tenant, a system-admin user, the worker token and prints one pairing code. It refuses to run on an initialized data directory. On the Unix socket, the peer UID (`SO_PEERCRED`, `LOCAL_PEERCRED` on macOS) maps through `daemon.json` `localUsers` to a user; unmapped UIDs get nothing. Everyone else pairs. Bearer credentials are `vdm_<ulid>_<secret>`, stored as SHA-256 of a 256-bit secret. The spec has no bootstrap command; `init` is additive to §11.1.
- **Why.** Pairing needs an admin (D12), and a first admin must come from somewhere. Host access to the data directory is the natural root of trust.
- **Revisit if.** Hosted setups need an installer flow.

### I18. TLS for the HTTPS listener

- **Choice.** Minimum TLS 1.3, no plaintext fallback. For loopback (Docker Desktop, D49), `tls: {"mode": "auto"}` generates a self-signed certificate in the data directory; `vidyom device pairing-code create` prints its SHA-256 fingerprint and `vidyom server add --fingerprint` pins it. For other interfaces, `tls: {"mode": "files"}` with a certificate is required, optional `clientCAFile` for mTLS (§9.2).
- **Why.** D12 requires certificate validation, and a loopback daemon has no CA path.
- **Revisit if.** An internal CA story is wanted.

### I19. Listener rules are fail-closed

- **Choice.** No listener is configured by default except the Unix socket (mode 0600 by default, parent directory 0700). HTTPS needs an explicit `bind` IP or an `interface` name; a missing interface aborts startup. `0.0.0.0` and `::` need `allowPublic: true` and a real certificate. Vault sealed or missing means no listener at all. Any listener failure at startup aborts the daemon rather than running on the rest.
- **Why.** §9.3, §15.6, D13.
- **Revisit if.** Never loosened without a maintainer decision.

### I20. Upload protocol

- **Choice.** `POST /assets/uploads` (`size`, `sha256`, `filename`) → `UPL-nnn`. `PATCH /uploads/{id}` with `Upload-Offset` and optional `Upload-Checksum: sha256 <b64>`, raw bytes, returns the new offset; a wrong offset answers `409` with the current one (tus-like). `GET /uploads/{id}` reports the offset. `POST /uploads/{id}/complete` verifies size and SHA-256. Data lands in `<spool-dir>/<tenant>/<upload-ulid>` on a volume workers can read. `/uploads/{id}` rather than `/assets/uploads/{id}` because `GET /assets/uploads/{id}` would conflict with `GET /assets/{id}/lineage` in the route table; `POST /assets/uploads` stays as §12.1 lists it.
- **Why.** §5.4 requires resumable, checkpointed, checksummed. A full tus server adds nothing here.
- **Revisit if.** Large files need parallel chunks.

### I38. `/clients` and `/devices` are the same resource

- **Choice.** §12.1 lists `GET /clients`, `POST /clients/{id}/revoke` (R41) and `GET /devices`, `POST /devices/{id}/revoke|rotate` (D45). Both route to the same handlers. The CLI uses `/devices`; `/clients` stays as an alias.
- **Why.** Each is decided text, neither supersedes the other.
- **Revisit if.** The maintainer drops one.

---

## Security

### I10. Master key unsealing

- **Choice.** `daemon.json` must name a provider in `vault.unseal.provider`. **There is no default.** Absent, unknown, or failing means the daemon exits non-zero with no listener (D13). No key is ever created at startup; generating one is an explicit `vidyomd init --generate-master-key <path>`.

  | Provider | Where | Sealing level | Target in this version |
  |---|---|---|---|
  | `systemd-creds` | Linux native; reads `$CREDENTIALS_DIRECTORY/vidyom-master-key` | hardware when sealed with TPM | Implement, **Validate** on target hosts |
  | `macos-keychain` | macOS native | OS | Implement, **Validate** |
  | `dpapi` | Windows native | OS | Implement, compile-check only, **Validate** |
  | `gcp-kms` | any | KMS | Interface only, returns `not_implemented`, **Validate** |
  | `file` | Docker: `/run/secrets/vidyom_master_key` via a Compose secret; needs `acknowledgeFileSealed: true`; file must be mode 0400 or 0440 | **file** (plaintext on the host) | Implement; the working default for Compose |
  | `env` | Dev only: `VIDYOM_MASTER_KEY_B64`; needs also `VIDYOM_INSECURE_DEV_UNSEAL=1` | **dev** | Implement; never in Compose |

- **Why.** D13 requires TPM, systemd-creds or KMS and no manual passphrase, and D41 adds per-platform options. Containers on Docker Desktop have no TPM or Keychain, and KMS needs an account, so Compose needs something that works on one host. `file` is explicit, acknowledged in configuration, reported by `vidyom doctor --security` and `/health` as `sealing: file`, and recorded in the audit on each boot. It is below D13's target, and the docs must say so. It is never a silent fallback: no provider ever falls through to another.
- **Revisit if.** KMS or TPM is validated on the target hosts: Compose docs then recommend it.

### I39. Worker credentials and cache keys

- **Choice.** Workers get connector credentials for a leased task from `POST /worker/tasks/{id}/connection` and a tenant cache key from `POST /worker/tasks/{id}/cache-key`, both in memory only and refused without a live lease. The cache key is `HKDF(tenant data key, "vidyom-cache-v1")`, so the vault key never leaves the daemon. Cache chunks use AES-256-GCM.
- **Limit, stated plainly.** Workers are trusted processes shared by tenants on one host. §15.6's "a worker for tenant A cannot open tenant B's cache" holds in the code path (no key, no directory access outside the leased tenant) and in tests, not as OS-level isolation.
- **Revisit if.** Tenants are mutually hostile: then one worker pool per tenant.

### I40. Trademark check stays open

Not decided here. INPI, USPTO and WIPO checks for `Vidyom` (§17.2) remain the maintainer's. Non-blocking for this work.

---

## Scope of external integrations

### I8. Phase 5 scope and order

- **Choice.** Order: **(1) audit, (2) Event Bus, (3) budgets**, all in this version.
  1. **Audit** (`core`): append-only table per tenant, hash chain, `audit verify`, on-demand and daily export to `archive`, 1-year minimum retention (D23). First because mutating endpoints must write it from day one (§12.1) and retrofitting a chain is impossible.
  2. **Event Bus** (`engine`): transactional outbox per tenant, event triggers, subscriptions for plugins, delivery tracking with retries, `causation_id` depth limit 5, per-tenant filtering (§8.2). Second because the public library sync depends on it.
  3. **Budgets** (`core`, enforced by `engine`): monthly estimate per tenant from a configured cost table, usage per operation, alert at 80%, block at 100% with audited override (D19). Last because it needs both of the above.
- **Why.** D57 left scope and order open, and the maintainer delegated it. The end-to-end scenario needs the first two.
- **Revisit if.** The maintainer sets a different scope. Automatic cost discovery from providers is excluded.

### I11. Rewind, VideoVortex, MediaCMS and later destinations: skeletons

- **Choice.** Connector skeletons, each compiling and registering, reporting `Available: false`, returning `errs.NotImplemented`, with the gap stated in the package comment:
  - **Rewind** (`connectors/catalog/rewind`, Phase 3.1): real interface unvalidated, optional tool for editors (D26).
  - **VideoVortex** (`connectors/catalog/videovortex`): real API, CLI and license unvalidated (§17.2).
  - **MediaCMS** (`connectors/catalog/mediacms`, node `mediacms.register`): API, external proxies, double transcoding unvalidated (D24). `catalog_sync = failed` with no impact on readiness (R11, §15.1 item 9) is proven with a failing mock catalog connector.
  - **Vimeo, PeerTube, Instagram Reels** (`connectors/publishing/*`, D25).
  - S3, Dropbox, SFTP, Datadog: no directory at all; they remain Future as §16 lists them.
  - **`import:` inputs (§3.4, §15.3):** the URI parses and is validated, but no import connector exists, so `node.Env.OpenInput` returns `not_implemented` and §15.3 is not demonstrated. Authorized YouTube import depends on the same unvalidated publishing APIs.
  - **VAD (§3.1 step 9, optional):** not implemented. Transcription runs on the whole audio, which keeps the timeline trivially correct.
- **Why.** Writing clients against unvalidated APIs would present guesses as integrations. Each is optional or post-MVP in the spec, and the catalog is the tenant database (D24).
- **Revisit if.** The validation PoC of §17.2 runs.

### I12. What is real and what is mock-tested only

- **Real in tests:** SQLite (FTS5, sqlite-vec), Redis (miniredis plus real server for crash tests), FFmpeg and ffprobe, the `local` driver, the `mock` destination, the Python worker process with its `fake` engine, Next.js with a fixture database, Docker Compose where Docker is available.
- **Mock-tested only:** GCS (`fake-gcs-server`), YouTube Data API (`httptest`), OpenAI-compatible LLM (`httptest` plus a local mock server for the demo), Loki and OTLP (`httptest`), macOS Keychain and systemd-creds with stubs where the host lacks them.
- **Implemented but not exercised against the real thing:** `faster-whisper` and BGE-M3 engines (optional extras, run only where installed), DPAPI, YouTube browser OAuth, which is a documented gap: `vidyom secret add youtube-oauth --location server` takes the token JSON through stdin in this version (R36 asks for a browser flow later).
- **Why.** No real paid or external calls are allowed; §17.2 validations stay open.
- **Revisit if.** Each §17.2 validation is run.

### I27. Fake AI engine and `fake-hash-embed@1`

- **Choice.** The Python worker has an explicit engine setting: `--engine fake` or `--engine real`, with no fallback in either direction. `fake` is stdlib-only. Its transcription returns the fixture transcript named by `VIDYOM_AI_FAKE_TRANSCRIPT`, or a canned one scaled to the audio's duration, containing the spec's example stories. Its embeddings use the algorithm in `schemas/ai-worker-rpc.v1.json#/$defs/fakeHashEmbed` (lowercased word tokens, FNV-1a feature hashing into 1024 dims, signed, L2 normalized), implemented identically in Python and in the library's TypeScript, stored as model `fake-hash-embed@1`. Vectors from different models are never mixed (R18).
- **Why.** Host Python is 3.15 and has no ML wheels; tests and the demo cannot download gigabytes of models. Fake semantic search only finds shared words, so the suite tests the path, not the quality: "no literal match required" (§15.2 item 1) stays **Validate** with real models.
- **Revisit if.** A tiny real model becomes usable in CI.

### I28. No usable LLM means ambiguous candidates, not failure

- **Choice.** With no endpoint allowed for the asset (D8) or none configured, `ai.analyze` returns `available: false`, and `ai.find_segment` still produces candidates from hybrid retrieval, with sentence-boundary expansion, marked `ambiguous: true`. The editor always approves (D53). `ai.editorial` falls back to the prompt and source title as draft fields.
- **Why.** D8 states this fallback for `local_only` collections.
- **Revisit if.** A local default LLM ships with the project.

### I29. `public.db` details

- **Choice.** Schema in `schemas/public-db/v1.sql`, version in the `meta` table. Rebuilt whole on each sync, written to a temporary file, `fsync`ed, checksummed, renamed. `public_id` is `HMAC-SHA256(tenant key, asset ULID)` truncated and base32-encoded, so the database has no internal IDs. Vectors are little-endian `float32` BLOBs in a plain table, so the site needs no extension and brute-forces cosine. Shipping goes through `connector.PublicCatalog`; the first driver is `local-file`, copying to a configured directory such as `library/data/<tenant>/public.db`. Query embeddings in the site: BGE-M3 on CPU when configured, otherwise lexical-only, and the API response reports the mode.
- **Why.** D18 sets the format, R44 the exclusions. **Validate:** D58, how the file reaches and runs on the site's host.
- **Revisit if.** D58's result requires another shipping driver or query embedder.

### I30. Mock publishing destination

- **Choice.** `connectors/publishing/mock`, driver `mock`, persists state in a JSON file, deterministic `remote_id` from the idempotency key, URLs under `https://media.example.internal/watch/`, configurable processing delay and failure injection (`fail_upload`, `outcome_unknown`, `reject`).
- **Why.** The scenario needs a destination that survives restarts for idempotency tests.
- **Revisit if.** Never for production; it must stay out of default config.

### I32. Libraries

- **Choice.** Standard library `net/http` with Go 1.22 routing and `log/slog`. `github.com/spf13/cobra` for the CLI. `cel.dev/cel-go` (D2; the project moved there from `github.com/google/cel-go`). `github.com/santhosh-tekuri/jsonschema/v6`. `github.com/oklog/ulid/v2`. `github.com/zalando/go-keyring`, pure Go, for the client keyring. `cloud.google.com/go/storage`, `google.golang.org/api/youtube/v3`, `golang.org/x/oauth2`. `go.opentelemetry.io/otel` and `github.com/prometheus/client_golang`. Test-only: `miniredis`, `fake-gcs-server`. Pre-added in `internal/tools/deps.go` so `go mod tidy` keeps them.
- **Why.** Maintained, license-compatible at a glance. **Validate:** license compatibility of every dependency with Apache-2.0 (§17.2).
- **Revisit if.** The license audit objects.

### I37. Python versions

- **Choice.** The AI worker's core (protocol, `fake` engine) is stdlib-only and must run on Python 3.12 through 3.15. Real engines are optional extras (`vidyom[asr,embeddings]`) and the Docker image uses Python 3.12, because `ctranslate2` wheels lag new Python versions. Packaged with plain `pyproject.toml` and `pip`, since `uv` is not available here.
- **Why.** This host has only 3.15. **Validate:** real engines on the Docker image.
- **Revisit if.** Wheels for newer Python exist.

### I43. Search ranking

- **Choice.** `GET /search` runs FTS5 BM25 and sqlite-vec KNN and combines them with reciprocal rank fusion (`k = 60`). Asset-scoped is the main path; tenant-wide lexical is always on; tenant-wide semantic is on but capped at 50 000 vectors scanned per query until load-tested. Only vectors of the tenant's active `embedding_model` are searched (D15).
- **Why.** D5's validation warns that sqlite-vec has no approximate index. **Validate:** D5.
- **Revisit if.** Load tests set a different cap.

### I44. Native platform support level in this version

- **Choice.** macOS arm64 and Linux: daemon, worker and CLI run natively and are tested. Windows: CLI and worker cross-compile; daemon is expected to build with MinGW but is **Validate**; Compose is the Windows path (D41). Windows native uses HTTPS loopback rather than a Unix socket.
- **Why.** No Windows host here.
- **Revisit if.** A Windows runner is added to CI.

---

## Spec interpretation

### I21. Clip eligibility beyond the `ready_for_clipping` state

- **Choice.** `create-clip` accepts an asset whose `master_available` holds and whose readiness flags satisfy the consolidated condition (R11) when its status is `ready_for_clipping`, or for a derivative also `editorial_review`, `approved` or `published`. `domain.Asset.ClipEligible()` encodes it. `ingest-derived` passes through `ready_for_clipping` before `editorial_review`.
- **Why.** §2.5 has one state axis, and a derivative moves past `ready_for_clipping` right after ingest, which would make R7/D36 (clips of clips) nearly unusable if taken literally. The flags, not the state, carry readiness.
- **Revisit if.** The maintainer wants readiness as a separate axis.

### I24. Code layout deviations from §14.2

- **Choice.** Added: `internal/contract/**` (shared contracts), `internal/apiclient`, `internal/cli`, `internal/retrieval`, `internal/publicexport`, `aiworker/`, `library/`, `nodes/publishing`, `docs/decisions/`. Not created: `internal/auth` (covered by `internal/security/authentication` and `authorization`, also in §14.2), `nodes/integrations` (`mediacms.register` lives in `nodes/publishing`), and the Future connector directories of I11. `docs/` at the root holds plans and decisions, the documentation site stays in `website/`.
- **Why.** Parallel work needs one contract location with clear ownership; §14.2 had no home for the CLI's internals or the public library.
- **Revisit if.** A reviewer prefers the contracts in `pkg/`, which would only matter for third-party Go importers, and plugins speak JSON-RPC anyway.

### I25. Media profiles

- **Choice.** `profiles/` holds `master-1080p`, `master-2160p-hevc`, `proxy-720p` (§14.2) and `speech-opus` (from `website/examples/profiles/`, needed for §4.1's ASR row and absent from §14.2). `video.transcode` accepts `profile: "auto:master"`, which picks by source height (D14), and records the concrete `id@version` in its output.
- **Why.** D14 chooses by height, and a `flow.if` per ingest is noise.
- **Revisit if.** More profile families appear.

### I26. `working_master` and `backup` for every kind

- **Choice.** Asset JSON uses `source_original` (originals only), `working_master`, `proxy` and `backup` (derivatives only). §12.3's `master` is therefore returned as `working_master`.
- **Why.** §12.2 and §12.3 use different names for the same role, and §2.1 defines "working master" for all derivatives via `clip_master`. One field name lets clients and `catalog.get` stay uniform: `working_master_uri` in §6.4 uses it too.
- **Revisit if.** The maintainer prefers `master`.

### I45. Retention sweeps

- **Choice.** Defaults per §10.4 and D16 in `config`: logs 30 days, job records 90 days, audit 365 days minimum, unapproved renders 30 days. `core` sweeps job records and logs on a daily timer, keeping the asset's provenance summary (R21). The scheduled workflow `expire-unapproved-renders` (engine) deletes expired renders from `master`. Scratch cleanup at job end belongs to `media`.
- **Why.** D40 and D16 state policy without saying who runs it.
- **Revisit if.** Sweeps cost too much on large tenants.

---

## Disagreements found, not resolved in the sources

Per AGENTS.md these are pointed out. Implementation follows the spec, and the spec itself stays as the maintainer wrote it. Website rows were checked against commit `2981435`; another session is rewriting `website/` in the shared tree while this was written, so re-check them against what lands.

| # | Where | Disagreement | This implementation |
|---|---|---|---|
| 1 | `website/examples/workflows/create-clip.json` vs §6.4 | The website example has no preview or approval nodes, uses `master_uri`, `parentId`, `findStory`; the spec requires the selection gate and `working_master_uri` | Spec §6.4 (R27) |
| 2 | `website/docs/architecture/repository.md` vs §14.2 | `nodes/flow` vs `nodes/control` + `nodes/approval` | §14.2 (D45) |
| 3 | `website/docs/reference/api.md` example vs §12.2 | `workflowVersion`, `idempotencyKey` in `camelCase` beside `job_id` | `snake_case` (I15) |
| 4 | `website/docs/architecture/data-model.md` vs §2.1, §12.2 | `kind` "`original` or `clip`", unversioned keys, `storage` object; the spec has `edited`, versioned keys, `working_master` | Spec |
| 5 | `website/docs/architecture/runtime.md` vs §15.5 | Idempotency key includes "attempt" | Not included (I36) |
| 6 | `website/docs/reference/api.md` vs §11.3 | Resolution options "retry, skip or cancel" vs `--retry-node`, `--skip-node`, `--fail` | §11.3 |
| 7 | `website/examples/config/publishing.json` vs §7.3 | Missing `allowPrivateUploadBeforeApproval` | §7.3 |
| 8 | §12.2 vs §12.3 | `working_master` vs `master`, `source_original` vs `backup` | I26 |
| 9 | §12.1 R41 vs D45 | `/clients` vs `/devices` | Both (I38) |
| 10 | §14.2 internal | `internal/auth` beside `internal/security/authentication` | I24 |
| 11 | `website/examples/profiles/` vs §14.2 | `speech-opus` exists only on the website; §14.2 has no ASR profile | Both kept (I25) |
| 12 | D46 vs D12 | Per-tenant sequences vs devices in `system.db` | I14 |
| 13 | §14.1 "Only the daemon opens these files" vs D18 | `public.db` is read by the site | The daemon opens the data directory's file; the site opens only its shipped copy (I29) |
| 14 | §5.1 D13 vs D41 on Docker Desktop | Unsealing options listed do not exist inside a container without a KMS account | `file` provider, explicit and below target (I10) |

---

## §17.2 validations and their working defaults

| Validation (§17.2) | Working default in this version | Decision |
|---|---|---|
| sqlite-vec performance; Litestream with GCS (D5) | sqlite-vec, asset-scoped first, capped tenant-wide; Litestream sidecar optional | I1, I31, I43 |
| Redis AOF and reconciliation under crash (D4) | Reconciliation tested with `FLUSHALL` and kill on real Redis on this host | I2 |
| TPM, systemd-creds, KMS on targets (D13) | `file` in Compose, others explicit | I10 |
| Master key per platform (D41) | Same | I10 |
| AI on macOS without GPU (D41) | CPU via real engine untested here; `fake` for tests | I27, I37 |
| MediaCMS (D24) | Skeleton | I11 |
| VideoVortex | Skeleton | I11 |
| Transcription accuracy (D9, D56) | Not measured | I27 |
| Publishing APIs (D25) | YouTube mock-tested; others skeleton | I11, I12 |
| License compatibility | Reviewed at a glance only; audit is open | I32 |
| LGPD review of deferred archive purge (D17) | Implemented as D17 states, review open | Open |
| Public library hosting (D18, D34, D58) | `local-file` shipping, lexical plus optional embedder | I29 |
| Trademark check | Open, non-blocking | I40 |
