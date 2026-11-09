# Implementation decisions: `integration`

Decisions of the `integration` stage, numbered from its range `I280–I299` (master plan, section 8). Same format as [`docs/decisions.md`](../decisions.md): the choice, the reason and what would make us revisit it. None of them changes a maintainer decision `Dn`. The maintainer delegated these and is not available during this run.

## Review of the plan

Checked `docs/plans/integration.md` against master plan sections 1.1 and 10 and the wiring notes of each component. Changes that came out of the review:

- README and AGENTS status updates, assigned to `integration` by the master plan, stay out by instruction: another session owns those files, and `docs/status.md` carries the status.
- Daemon-pool nodes must be registered for `workflow.LoadRegistry`, while media nodes are known to the validator through the catalog. The daemon therefore registers `controlnodes`, `approvalnodes`, `catalognodes`, `ainodes` and `publishingnodes` only (I280), not the media nodes the plan first listed.
- `core`'s sketch calls `vault.AuditBoot`, and `api` says not to. `daemon.Run` writes `daemon.boot` with provider, sealing and version before any listener, so the second record would add nothing. It is not called.
- `authentication.Options.TLSFingerprint` stays nil: `daemon.Run` sets `api.TLSFingerprint` from the listener plan (`pairing_codes.create` reads that).

## Assembly

### I280. How the daemon is assembled

- **Choice.** `cmd/vidyomd/wire.go` builds in the order of the plan's section 2.1, and for `vidyomd init` (`cfg.Bootstrapping`) stops after the identity services, so `init` needs neither Redis nor the observability manager. Workers and the daemon each read `daemon.json`; the worker applies the same directory defaults as the daemon. The AI worker is a non-critical entry in `/health`: a daemon without it still serves catalog, approvals and publication. Only the node packages the daemon can run are registered in it.
- **Why.** `api`'s note that `wireDeps` also runs for `init`, and I138.
- **Revisit if.** `init` ever needs Redis, for example to validate it up front.

## Cross-component issues

### I281. `speaker` is ordinary asset metadata

- **Choice.** Additive `domain.Asset.Speaker`, a column added by a migration, `catalog.register` parameter `speaker`, `ingest-original` input `speaker`, CLI `original import --speaker` and `asset update --speaker`. A derivative with no speaker of its own takes its source's when it is registered. `publicexport` writes it to `videos.speaker`, so §13.2's search by instructor has data.
- **Why.** §3.1 step 1 lists speaker/instructor as ingest metadata, `public.db` has the column, the library already reads it, and nothing fills it.
- **Revisit if.** Several speakers per asset are wanted; `tags` carries those until then.

### I282. `ai.find_segment` is marked `paid`

- **Choice.** `schemas/node-catalog.v1.json` marks `ai.find_segment` `paid: true`, like `ai.analyze` and `ai.editorial`. Over budget, `create-clip` pauses there (I33) rather than spend another prompt. With no LLM the node would have spent nothing, but the pause is the same as for the other two, which also do not look at the endpoint class.
- **Why.** I216 flagged it. A node that can call the same LLM should stop at the same limit, and D19 says the 100% limit blocks new paid operations.
- **Revisit if.** Tenants want clip selection to keep working on retrieval alone while over budget: the check would need to know the endpoint.

### I283. D16, R11 and §15.2 agree once "selection approved" is read literally

- **Choice.** No new sweep. D16: "the master of a clip or edited video is archived when its selection is approved. Rejected or never-approved renders stay only in the `master` role". In `create-clip`, `video.cut` is dominated by the `clip_selection` gate (D53, I142), so every render that reaches `ingest-derived` has its selection approved by construction, and its backup is what D16 asks for and what R11 and §15.2 item 5 need. A render rejected later, at the publication gate, had an approved selection, so its archive copy stays, and the sweep removes its `master` copy after 30 days (`archived_only`). It leaves `archive` through `delete-asset` and D17's deferred purge. Engine's table row 1 is closed by this reading; the code is unchanged.
- **Known gap, recorded rather than fixed.** `archivePolicy` (R29) is loaded and validated, and no workflow reads it: `ingest-derived` always backs up, as if `all_versions`. With D16's default of `approved_versions` for derivatives, a tenant that sets `latest` gets no behavior from it. Listed in `docs/status.md`.
- **Why.** The reading is the plain text of D16. A sweep over `archive` would pay ARCHIVE's early deletion charges (R29) to remove something D16 says to keep.
- **Revisit if.** The maintainer means "approved for publication" where D16 says "selection".

### I284. Sensitive marks reach public paths through the whole lineage

- **Choice.** `publicexport.SensitiveIntervals` walks the lineage recursively, as `catalog.effectiveMarks` and `nodes/ai.sensitiveMarks` already do, so a mark added to a grandparent after a clip of a clip was cut is excluded from `public.db` and captions. A source that cannot be read is an error, which withholds that video's transcript, chapters and embeddings (I231). The report in I207 was right and I231's claim was only true one level up. A cross-component test on the real store and the §1.1 suite prove it.
- **Why.** D30 says sensitive text never reaches `public.db`, public captions or external AI.
- **Revisit if.** The three walks should become one function, in `domain`; they differ in what they do on a missing source.

### I285. A reviewer gets a playable preview through the API

- **Choice.**
  - `create-clip` copies the preview to the `proxy` role under `previews/<job-ulid>/preview.mp4` (node `store_preview`), the approval's `preview` is that `storage://proxy/...` URI, and `drop_preview` deletes it once the gate is decided.
  - New routes: `GET /approvals/{id}/preview/content` and `GET /assets/{id}/proxy/content`, which stream the file from the `proxy` role only, with the permission of the corresponding read. The daemon reads only `proxy`, never scratch (I5) and never `archive`.
  - `vidyom clip preview APR-nnn` downloads the candidate to `APR-nnn-preview.mp4` or `--output`. `vidyom clip preview CLIP-nnn` still prints the link, and `--output` downloads instead. A `file://` link printed to a client not on a Unix socket carries a warning, because under Compose it names a path inside a container.
- **Why.** §3.2 step 6 is a core product step, and a `scratch://` URI on a shared worker volume is not something a reviewer can open. I198's `file://` link also stops working the moment the daemon runs in a container. Storing the preview in `proxy` matches "generate a preview from the proxy", needs no new worker protocol, and works with every driver.
- **Limit.** A job cancelled while waiting leaves its preview in `proxy`, because `drop_preview` does not run. It is small and listed in `docs/status.md`.
- **Revisit if.** Remote review needs range requests in a browser; the content routes only serve whole files with `Content-Length` in this version.

## Testing

### I286. The acceptance suite is one ordered Go test over real processes

- **Choice.** `tests/e2e`, build tag `e2e`, so `go test ./...` stays self-contained. One test walks the §1.1 steps in order as subtests, stopping at the first failure, because each step needs the state of the one before. Processes and evidence go through a small `stack` interface with two implementations: `native` (real binaries on this host) and `compose` (`VIDYOM_E2E_TARGET=compose`, containers from `deploy/`). Every step uses the CLI. Direct HTTP is used only when the evidence is about HTTP itself (no `Vidyom-Tenant`, the library's API), direct file reads only for evidence (access log, mock state, `public.db`, audit tampering).
- **Why.** The scenario is a story, and running it against Compose too means Compose is proven by the same assertions rather than by a thinner copy.
- **Revisit if.** The suite grows enough to want independent scenarios.

### I287. Cross-component tests that need no processes live in `tests/integration`

- **Choice.** `tests/integration` runs in plain `go test ./...` against the real store and real services, in process: sensitive marks through a clip of a clip, the exporter reusing stored vectors with an embedder that fails when called, and `internal/jobs`' engine smoke on the real SQLite store (recreated from the engine's uncommitted smoke tests).
- **Why.** Component tests were not allowed to import each other. The rules are gone, and these checks belong in the fast suite.
- **Revisit if.** They get slow.

## Found while assembling

### I288. A person's `--retry-node` settles an outcome the destination cannot report

- **Choice.** `job resolve --retry-node` on a `reconcilable` node whose connector cannot reconcile runs the node, instead of moving it back to `needs_attention`. The engine marks the attempt (`Lease.PersonRetry`, `node.Input.PersonRetry`) and only `capability_unsupported` is forgiven: a destination that can answer is still asked first, and an outage still stops. The attempt keeps its idempotency key, so a destination that honors keys makes no duplicate.
- **Why.** §11.3 offers retry as a way out of `needs_attention`, and D11 says a person resolves it. Before this, a destination with no reconcile looped: retry, reconcile fails, `needs_attention`, forever, leaving only skip or fail. Found by the suite's `mock-unknown` destination.
- **Revisit if.** Destinations need a stronger confirmation than a note on the resolve.

### I289. The finish hook also runs when a `before` hook blocks a job

- **Choice.** A job refused by `workflow.before` or `workflow.start` runs its `workflow.error` and `workflow.finish` hooks before the failed state is written. A `workflow.before` hook also receives the job's inputs as `{"inputs": {...}}`, the counterpart of `{"parameters": {...}}` at `node.before`.
- **Why.** §8.1 lists `workflow.finish` as "terminal finalization, success/error/cancellation" and §15.7 asks for it on error. Without inputs, a job-level hook had nothing to judge, so the sample plugin's `check-source` blocked every job.
- **Revisit if.** A hook wants to tell "never started" from "failed": the outcome is in the data, and the job's error code says `hook_blocked`.

## Deployment

### I290. How Compose is put together

- **Choice.**
  - **Master key:** the `file` provider through a Compose secret from `deploy/secrets/master.key` (ignored by Git), as I10 says. Docker Desktop maps ownership itself, a Linux host needs `chown 10001`. A missing file makes `docker compose up` refuse, which is the fail-closed outcome D13 wants.
  - **Workers reach the daemon over the Unix socket** on a shared volume, all containers as UID 10001, so mode `0600` holds and no certificate pin has to be passed around. The CLI on the host uses HTTPS on loopback with the pin (D49, I133).
  - **Worker token:** a shared volume written by `vidyomd init --worker-token-file`, read-only in the workers, because the data directory stays private to the daemon.
  - **Logs:** `config/compose/daemon.json` adds a second `file` sink on `/dev/stderr`, so `docker compose logs daemon` shows what the JSONL holds, after redaction. Natively the daemon's stderr stays quiet except for failures: `<data-dir>/logs/vidyomd.jsonl` has the log.
  - **Mock LLM:** a `demo` profile. It refuses non-loopback addresses unless started with `--any-interface`, and only Compose does that, on a private network. The endpoint is classified `local` and the driver's dial check accepts the private address (D8).
  - **Library:** mounts the directory, read-only, never the file.
  - **Observability profile:** Grafana, Loki and Tempo on loopback, with data sources provisioned. Vidyom's `loki` and `tempo` sinks are declared `enabled: false`, and `vidyom observability enable` turns them on, so a daemon without the profile does not log a failure a minute.
  - **Litestream profile:** per I31, replicating to a volume. Run once, by hand: it writes snapshots and WAL segments. Restoring and GCS are **Validate**. Its image has no directory for the volume to take ownership from, so the daemon's image provides it, and the daemon mounts the volume for that reason alone.
- **Why.** D10 and D41 make Compose the supported path on all three platforms, so everything host-specific had to be either an environment variable or absent.
- **Revisit if.** Workers move to another host (I5's Future case): then HTTPS with a pinned fingerprint, which the worker already supports.

### I291. Real models run once, by hand, and report instead of assert

- **Choice.** `make real-model-proof` (`tests/e2e/realmodels_test.go`, tags `e2e realmodels`, not in CI) speaks six short stories with macOS `say`, ingests them through the real AI worker image, searches each with a paraphrase that shares no word with the script (the test refuses to start otherwise), and writes a JSON report: transcript, word error rate against the script, per-node timings, each query's rank, and the clip proposal with no LLM. It asserts only that real models ran. Quality is what the report states. Models live in the Docker volume `vidyom-dev-integration-models`, which survives the run.
- **Why.** §15.2's "no literal match" is the claim the fake engine cannot test. A pass or fail threshold invented here would dress up one sample as a validation; §17.2 keeps accuracy open.
- **Revisit if.** A labeled evaluation set exists: then thresholds mean something.

## Testing, continued

### I292. Reliability of the suite

- **Choice.** Root causes fixed rather than retried:
  - `internal/plugins`' "the plugin did not answer in time": the hook's timeout covered starting the process, and the first exec of a freshly copied binary can take seconds on a loaded macOS host (the kernel and `syspolicyd` check a new binary). Starting now has its own budget (`InitTimeout`), in production as well as in tests, while a dispatcher's deadline still bounds the whole call.
  - `connectors/storage/local`'s conformance test failed under `-count=3` as "tenant globex sees acme's object". The driver was right: the test cached a removed root by name, so the third run saw the second run's files.
  - `internal/observability`'s `TestDeadLokiBlocksNothing` read the logs before the file sink's pump had opened it, which answers from the in-memory ring by design. It now waits for all 500 records, and still requires all of them.
  - `internal/workers`' earlier failures could not be reproduced: 8 parallel processes, 12 runs each under load, and repeated full `-count=3` runs. `docs/status.md` records the runs. They are not claimed fixed.
- **Why.** A suite that fails under load teaches people to ignore it.
- **Revisit if.** They recur, with the output kept this time.

### I293. Engine suite on the real store

- **Choice.** `VIDYOM_JOBS_REAL_STORE=1 go test ./internal/jobs` runs the engine's whole suite on `core`'s SQLite store rather than its in-memory fake, and CI does that too. One smoke runs on the real store in every plain `go test`. The engine's uncommitted smoke tests for media and publishing are not recreated, because `tests/e2e` covers them with real processes.
- **Why.** `engine`'s plan named this as what proves the fake's fidelity. It passed unchanged.
- **Revisit if.** It gets slow.

## Observability, continued

### I294. Metrics and spans wired here, and what stays out

- **Choice.** `vidyom_connector_duration_seconds` and `_failures_total` come from thin wrappers: storage and AI connectors in the worker, the LLM in the engine's environment, publishing in its resolver. The worker has its own manager with the daemon's metric and trace exporters and no log sinks, plus `--metrics-addr` for a Prometheus scraper, because `GET /metrics` on the daemon can only show the daemon's process. A span per node execution, in daemon and worker, uses the job's trace ID as a remote parent, so a trace runs from the API request through the workers though the job outlives the request. `SIGHUP` also reloads observability, or a tenant's new sink never took effect. `vidyom_usage_quantity_total` and `vidyom_ai_cost_estimate_minor_total` come from where usage is recorded.
- **Left out, listed in `docs/status.md`.** Gauges needing a periodic sweep over each tenant (`vidyom_assets_without_verified_backup`, `vidyom_clips_in_editorial_review`, `vidyom_storage_stored_bytes`), `vidyom_ai_time_estimate_seconds_total`, spans for inline engine nodes, storage calls made by the daemon itself.
- **Why.** The brief said to wire what is cheap and list the rest. Wrappers keep every connector unaware of metrics.
- **Revisit if.** Operators need those gauges: `tenants.Maintenance` is the natural place.

### I295. What the real models changed

- **Choice.** Two fixes came from the first real run, and neither could have come from the fake engine. `asr` pins `av<16`, because `faster-whisper` 1.2 passes an argument that PyAV 16 and later refuse. `storyProposals` stops merging touching hits unless both lists found them: on a paraphrase, fused scores differ only by rank, so `clusterScoreShare` never separated anything, and windows chained into one proposal of 137 s for a 33 s story. The real engine now logs a failure's cause, which the client never sees.
- **Why.** The fake engine shares words with every query, so hits were always corroborated, and it never touches PyAV.
- **Revisit if.** Editors prefer a longer proposal over several short ones when no LLM is configured.

### I296. HEVC of one or two frames is a known limit

- **Choice.** `TestAutoMasterOf2160pIsHEVCAt2160p` uses three frames instead of two, and the product does not work around the cause. A one- or two-frame source on the HEVC profile can fail its transcode, as a permanent error that a rerun usually clears. The failing first packet has `pts` 0 and a `dts` such as `-17592186048512` where a healthy run has 0, and the rate falls to none of 600 at three frames or with `bframes=0`. The values fit an uninitialized value in x265's B-frame delay, but x265's source was not read, so that part is a reading of the evidence. The numbers are in `docs/status.md`.
- **Why.** Sources this short do not occur in real use: only sources of 1440p or more take this profile, and one frame of video is not a recording. A branch to set `bframes=0` by frame count would exist only for them.
- **Revisit if.** Short clips cut from 4K masters ever hit it, or a later x265 changes the behavior.
