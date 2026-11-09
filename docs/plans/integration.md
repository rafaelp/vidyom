# Plan: component `integration`

Last stage of the first executable version: assemble the nine components, prove the §1.1 scenario with real binaries, ship it through Compose, try the real models once, and say honestly what is real. Normative inputs: `spec/vidyom-specification-v1.2.md`, `docs/plans/00-master-plan.md` (sections 1.1 and 10), `docs/decisions.md`, each component's plan and decisions. Decisions taken here are in [`docs/decisions/integration.md`](../decisions/integration.md) as `I280`–`I299`.

Status vocabulary: everything below is a **target** until its commit lands. `docs/status.md` is the record of what actually works.

## 1. Deliverables

| # | Path | Done when |
|---|---|---|
| 1 | `cmd/vidyomd/wire.go`, `cmd/vidyom-worker/wire.go`, `go mod tidy` | Both binaries start with every real component; `vidyomd init` works with no Redis; `make check-static` still passes for the worker |
| 2 | `tests/e2e`, `make e2e` | All 13 steps of §1.1 pass with real binaries, real API, real Redis, real FFmpeg, the real Python worker process (fake engine), `local` storage, `mock` destination, mock LLM, real Next.js server, each step asserting its evidence |
| 3 | `scripts/demo.sh` | Runs the lifecycle visibly, cleans up after itself, also when interrupted |
| 4 | `deploy/`, `config/` | Images built on this host; the scenario passes against Compose under project `vidyom-dev-integration`; optional observability profile starts; torn down with its volumes |
| 5 | Real-model proof, `tests/e2e/realmodels_test.go`, `make real-model-proof` | Ingest and search with faster-whisper large-v3 and BGE-M3 on CPU in the AI worker image, on real speech, with paraphrase queries. Real numbers reported, whatever they are |
| 6 | `.github/workflows/ci.yml`, `CHANGELOG.md` | Go, Python, static checks, library, e2e. `docs.yml` and the dirty templates untouched |
| 7 | `docs/status.md` | What is real, mock-only, skeleton, gapped, **Validate**; how to run it; quick start |

## 2. Wiring

### 2.1 `vidyomd` order

Two phases, because `wireDeps` also runs for `vidyomd init` with `cfg.Bootstrapping`:

1. `persistence.Open`, `RequireExisting: !cfg.Bootstrapping`.
2. `vault.ProviderFor` then `audit.New(stores.Tenants, {System})`, `vault.New(... {Auditor})`. The vault fails construction when sealed, which is the D13 path, so a missing key leaves no listener. `daemon.ValidateConfig` and `daemon.Run` check again.
3. Observability manager. `slog.SetDefault`, `InstallGlobals`. Skipped for `Bootstrapping`: `init` is a short command and its log file would fight the daemon's.
4. `authentication`, `authorization`, `tenants`, `catalog`. `Deps.Bootstrap` over `tenants.Bootstrap`.
5. If `Bootstrapping`: return now, with no Redis.
6. Connector registry: storage (`local`, `gcs`), AI (`fasterwhisper`, `bgem3`, `openai`), publishing (`mock`, `youtube`, skeletons), catalog (`public`, skeletons), observability built-ins.
7. Node registry: `controlnodes`, `approvalnodes`, `catalognodes`, `ainodes`, `publishingnodes`. The validator knows the media nodes from the catalog (I280).
8. `storage resolver`, `auditor.SetStorage(resolver)`; `publicexport`; `retrieval` wrapped in `catalog.NewSearchGuard`, required by I118.
9. Plugin runtime, hooks dispatcher, Redis client and queue, workflow registry, scheduler, Event Bus, engine.
10. `publishingnodes.NewService`, connectors service, `daemon.Deps` with engine, maintenance and health checks.

`cleanup` closes in reverse: plugin runtime before the manager, the manager last.

### 2.2 `vidyom-worker`

`wireRegistries`: `storagenodes`, `videonodes`, `audionodes`, `ainodes`, `local`, `gcs`, `fasterwhisper`, `bgem3`. Never `publishingnodes`. `wireQueue` over `queue.NewRedis`. `wireMetrics` over the observability manager restricted to metrics exporters. Stays `CGO_ENABLED=0`.

### 2.3 Early smoke

Before writing any assertions: start everything by hand against a temporary directory, run the §1.1 lifecycle with the CLI, and fix what breaks. Components were tested against fakes, so most integration bugs should surface here. Each fix is a small commit in the owning path.

## 3. Cross-component issues

| Issue | Action |
|---|---|
| Speaker field | Add `Speaker` to `domain.Asset` additively: migration, repo, catalog, `catalog.register` parameter, workflow input, CLI `--speaker`, exporter fills `videos.speaker` |
| `internal/plugins`, `internal/workers` flakes | `go test -count=3 ./...`; find real causes (fixed timeouts under load, process-start budget), fix them |
| `ai.find_segment` budget | Decide; catalog `paid: true` if it can spend (I216) |
| D16 vs R11 and §15.2 | Read the workflows and record the conclusion |
| Sensitive marks through lineage | Test a clip of a clip through `publicexport`, in the real stack. Fix if the mark does not reach it |
| `repo.EmbeddingLister` | Prove by test that a sync does not call the embedder when vectors are stored |
| Reviewing a clip for real | Make `vidyom clip preview` return something playable with `local`; `scratch://` on an approval is not. Decide the mechanism |
| Metrics and tracing gaps | `wireMetrics`, connector metrics where cheap, engine spans if cheap. The rest goes in `docs/status.md` |
| Smoke tests | Recreate what is useful under `tests/` |

## 4. Acceptance suite

`tests/e2e`, Go test package, build tag `e2e` so `go test ./...` stays fast and needs nothing installed. `make e2e` builds the library and the binaries and runs it serially. `make e2e-compose` runs the same steps against Compose.

Harness:

- Builds `vidyomd`, `vidyom-worker`, `vidyom-mock-llm` and the CLI with `-tags vidyom_test_keyring`. One `VIDYOM_CONFIG_DIR` and `VIDYOM_TEST_KEYRING_FILE` per simulated user.
- `mktemp`-style root. On macOS the Unix socket path limit is short, so the root stays under a short path.
- Real Redis on a non-default port, with `--appendonly yes`.
- `vidyomd`, two workers, Python worker, mock LLM and the Next.js production server, as processes owned by the harness. Each process's output goes to a file printed on failure.
- Every step goes through the CLI. Direct HTTP only where a step's evidence requires it (a request without `Vidyom-Tenant`, the library's API) and direct file reads only for evidence (`access.log`, mock state, `public.db`, `tenant.db` tampering).
- Steps run in the §1.1 order inside one test, each as a subtest. A failed step stops the rest, since later steps need earlier state.

Evidence per step is the table in §1.1. Step 13 uses `kill -9` on `vidyomd` during a transcode, restart, and asserts one completion per node from the node execution records. `FLUSHALL` while a job is queued (workers stopped so it stays queued), then workers back. Extended coverage beyond the table: §15.7 hook scenario with `plugins/sample-notifier`, tenant isolation with a second tenant, sensitive mark through a clip of a clip.

The same scenario must also run against Compose: the harness takes `VIDYOM_E2E_TARGET=compose`, where processes are containers, the CLI talks HTTPS with a pinned fingerprint, and evidence files are read from the bind-mounted data root.

## 5. `scripts/demo.sh`

Bash, `set -euo pipefail`, trap for cleanup. Prints each step and the commands it runs. Uses the same binaries and config templates as the suite, on ports in a distinct range, and leaves nothing behind unless `--keep`.

## 6. `deploy/`

Images: `Dockerfile.vidyomd` (cgo, `libsqlite3-dev` at build time), `Dockerfile.worker` (static binary, FFmpeg), `Dockerfile.aiworker` (Python 3.12, targets `fake` and `real`), `Dockerfile.library` (Next.js standalone). `docker-compose.yml`: Redis with AOF, daemon, media worker, AI worker, library. Named volumes for data, scratch, spool, cache, storage roles and sockets. Master key and worker token as files. HTTPS on `127.0.0.1` per I133. Profile `observability`: Grafana, Loki, Tempo, on non-default ports. Sidecar Litestream profile is I31 and stays optional.

`config/`: sample `daemon.json` and tenant files with fictional values, valid by test.

## 7. Real-model proof

`make real-model-proof`, a test sharing the suite's Compose harness: generate speech with `say`, build the `real` AI worker image, cache models in a Docker volume outside the repository, run the daemon and workers, ingest, search with paraphrases sharing no word with the speech. Write the transcription, timings and ranks to a report. Not part of CI (I291).

## 8. Order of work

1. This plan, then its review in `docs/decisions/integration.md`.
2. `wire.go` for both binaries. Early smoke by hand.
3. Fix what breaks, one commit per cause.
4. `tests/e2e` steps 1 to 13.
5. Cross-component issues, each with a test.
6. Flakes: `go test -count=3 ./...`.
7. Demo.
8. Deploy, images, scenario against Compose, observability profile.
9. Real-model proof.
10. CI, changelog, status.
11. Final pass: `go test ./...`, `-count=3`, `make e2e`, demo, Compose. Final report.

## 9. Definition of done

Master plan section 10 `integration`, except README, AGENTS and website, which another session owns and which stay untouched by instruction: status goes in `docs/status.md`.
