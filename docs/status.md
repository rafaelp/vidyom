# Status of the first executable version

The honest state of this tree, for comparing with the website and the README, which another session is writing as product copy. Where they differ, this file describes what the code does today. Nothing has been pushed, tagged or released. Results below are from one Mac Studio (macOS arm64, Docker Desktop with Linux arm64 containers); no Windows host and no x86-64 host has run any of this.

The short version: **the whole lifecycle works end to end, on one host, natively and under Docker Compose, with a fake AI engine, a mock LLM, a mock destination and local storage.** Real AI models ran only by hand, on six short spoken stories (section 6). Nothing has talked to real YouTube, real GCS or a real LLM vendor.

## 1. Quick start

Needs Go 1.27, Python 3.12 or later, Node 22.13 or later, FFmpeg 5.1 or later, `sqlite3`, and Redis (or `mise exec redis@8.8.0`).

**See it run, in five minutes, with nothing left behind:**

```bash
scripts/demo.sh            # add --pause to go step by step, --keep to keep the files
```

**The acceptance suite:**

```bash
make e2e                   # all 13 steps of the master plan's §1.1, plus the extended paths
make e2e-compose           # the same suite against Docker Compose on this machine
```

**By hand under Docker Compose,** the supported deployment path (D10, D41). All values are the demo's: fake AI engine, `mock` destinations, `local` storage, mock LLM.

```bash
openssl rand -base64 32 > deploy/secrets/master.key && chmod 400 deploy/secrets/master.key
# On a Linux host also: sudo chown 10001:10001 deploy/secrets/master.key

compose() { docker compose -f deploy/docker-compose.yml --profile demo "$@"; }
compose up --detach --no-deps --wait redis
compose run --rm -T --no-deps daemon init --config-dir /etc/vidyom --tenant acme \
  --admin admin@example.com --worker-token-file /var/lib/vidyom/worker-token/worker.token
# This prints the pairing code, once, and the TLS fingerprint to pin.
compose up --detach

make vidyom                # the CLI, on this machine
bin/vidyom server add main --url https://127.0.0.1:8443 --fingerprint sha256:...
bin/vidyom server pair main --code XXXXX-XXXXX-XXXXX-XXXXX    # stores the credential in the OS keyring
bin/vidyom tenant use acme
bin/vidyom original import class.mp4 --title "Hatha Yoga Class" --type yoga_class --speaker "Maria Example"
bin/vidyom job list
bin/vidyom original search "conscious breathing"
bin/vidyom clip create --source ORI-001 --prompt "conscious breathing" --destination mock-main
bin/vidyom approval list
bin/vidyom clip preview APR-001             # downloads the candidate, to watch it
bin/vidyom approval approve APR-001 --start 10 --end 40
bin/vidyom approval approve APR-002         # the publication gate
curl 'http://127.0.0.1:3000/api/search?q=conscious%20breathing'
bin/vidyom unpublish CLIP-001 --all

compose down --volumes     # removes everything, including the data
```

`vidyom server pair` writes to your real OS keyring here. Only the suite and the demo use the test-only keyring build.

Optional profiles: `--profile observability` adds Grafana on `127.0.0.1:3001`, Loki and Tempo, then `vidyom observability enable loki` and `enable tempo`. `--profile litestream` replicates the databases to a local volume (run once, section 5).

## 2. How to verify, and the last results

Every result here is from the last run on this machine, at the commit that added this file.

| Command | What it proves | Result |
|---|---|---|
| `go test ./...` | Every component, plus `tests/integration` on the real store | 76 packages ok, 35 with no test files, 60 s |
| `go test -count=3 ./...` | The same, repeated | 76 ok, 2 min 2 s. See below under load |
| `make test-race` | The race detector over everything | 76 ok, 63 s |
| `VIDYOM_JOBS_REAL_STORE=1 go test ./internal/jobs` | The engine's whole suite on real SQLite instead of its fake | ok, 35 s |
| `make e2e` | §1.1's 13 steps, then 14 extended paths, on real processes | **Pass**, 236 s. 26 pass, 1 skipped (`x11`, Compose only) |
| `make e2e-compose` | The same steps against images built from `deploy/` | **Pass**, 335 s. 26 pass, 1 skipped (`x10`, no socket on the host) |
| `scripts/demo.sh` | The lifecycle, for a person to watch | Exit 0, about 2 min 30 s, nothing left behind |
| `make real-model-proof` | Real models, section 6 | Pass, 245 s with models cached |
| `make test-python` | The AI worker's protocol and fake engine | 75 tests ok, 2 skipped |
| `cd library && npm run typecheck && npm run lint && npm test && npm run build && npm run test:e2e` | The public library | All pass: 199 unit, 32 over HTTP |
| `make fmt-check check-static cross` | Formatting, the CLI and worker free of cgo and database code, cross-compilation for 6 targets | Pass |

The suite natively ran on macOS arm64 only. Linux has run only as the containers of `make e2e-compose`, on arm64. **`.github/workflows/ci.yml` has never run**, because nothing has been pushed.

**Reliability under load** (I292). The failures reported from the parallel stage were traced and fixed in `internal/plugins`, `connectors/storage/local`, `internal/observability` and `nodes/video`. `internal/workers`' earlier failures did not come back in any run here, and are not claimed fixed.

**HEVC of one or two frames** (I296). With B-frames, `libx265` sometimes gives the first packet of a stream that short a garbage DTS, and the MP4 muxer refuses it: `pts/dts pair unsupported`, exit code 176. Under 24 parallel encodes with x265 4.3 and FFmpeg 9.0.2 on this machine:

| Stream | Failures |
|---|---|
| 1 frame | 45 of 300 |
| 2 frames | 20 of 600 |
| 2 frames, `bframes=0` | 0 of 600 |
| 3, 4 and 6 frames | 0 of 600 each |

Such a source fails its transcode as a permanent error, and the code does not work around it, because no real recording is that short. The test that hit it now has three frames and passed 400 parallel runs.

## 3. Real and exercised end to end

Every row runs through the real binaries over the real API, in `make e2e` and `make e2e-compose`.

| Area | What ran | Spec |
|---|---|---|
| Daemon | `vidyomd init`, refusal with no master key or a missing one (non-zero exit, no listener), HTTPS TLS 1.3 with a pinned auto certificate, Unix socket with peer identity, `/health` | D13, I10, I17, §9.3 |
| Identity | Pairing, one-time codes, device revocation, `Vidyom-Tenant` rule, viewer, editor, publisher and admin roles, D7's two levels | D7, D12, D32, D48 |
| Tenancy | One SQLite per tenant, a second tenant cannot see the first through the API or search, non-members get `404` | D6, §15.6 |
| Upload | Resumable with client `SIGKILL` and resume on the same upload, SHA-256 mismatch refused, `server:` inputs, paths outside `ingestRoots` refused | §5.4, R20 |
| Ingest | All 30 nodes of `ingest-original` with real FFmpeg, raw in `archive`, master in `master`, proxy in `proxy`, timeline within tolerance, ASR audio from the master, readiness only after the consolidated condition | §3.1, R10, R11, D21, D56 |
| Search | FTS5 and sqlite-vec, hybrid by rank fusion, asset-scoped, through the viewer guard for sensitive text | §12.1, D5, I43, I118 |
| Clips | `create-clip` with selection gate, adjusted timestamps, preview downloadable through the API, render from `master` only (the access log shows no `archive` read), own master, backup and proxy, lineage, clip of a clip, rejection renders nothing | §3.2, R13, R27, D36, D53 |
| Publication | Publication gate, `mock` destination, `published` only with remote ID and URL, same key no second upload, lost answer reconciled, destination rejection recorded apart from failure, `needs_attention` and `job resolve` | §3.3, R43, D7, D11, D52 |
| Public library | Event-triggered export, `public.db` free of storage keys, ULIDs, internal IDs and private original data, sensitive text absent from the file and from captions, real Next.js search with lexical and hybrid modes, works with the daemon dead, withdrawal gone at once, speaker search | D18, D30, D34, R44, §13.2, §15.8 |
| Audit | Hash chain verifies, an edited row makes it fail | D23 |
| Resilience | `kill -9` of `vidyomd` during a transcode: job resumes, no node runs twice. `FLUSHALL` with a job queued and under live workers: reconciliation re-enqueues | §15.5, D4 |
| Hooks and plugins | Sample plugin as a real sandboxed process (Seatbelt natively, Landlock in the container), `before` rejects and is audited, `finish` runs on both outcomes, nothing runs without a tenant grant, event delivery to the plugin | D3, §8.1, §15.7 |
| Events | Outbox, event triggers, idempotent deliveries | §8.2 |
| Budgets | Over budget pauses at a paid node, audited override, resume | D19, I33 |
| Operations | Pause, resume, cancel, `verify`, `reindex-asset`, export package, `restore-archived` with confirmation, `delete-asset`, `bulk-create-clips`, `verify-storage`, `expire-unapproved-renders` on demand | §3.3, §11 |
| Observability | JSONL logs, `/metrics` with per-tenant filtering, connector metrics, node spans in the job's trace. Under Compose: logs reach a real Loki, traces a real Tempo, Grafana has the data sources (`x11`). A dead log sink shows its failure and does not stop jobs | D38, §10, §15.7 |
| Compose | Images build and the whole suite passes against them | D10, D41 |

The engine's suite also runs on the real SQLite store (`VIDYOM_JOBS_REAL_STORE=1`).

## 4. Tested only against mocks or fakes

| What | Against | Note |
|---|---|---|
| AI quality in tests and the demo | `fake` engine: canned transcript, word-hash embeddings | Proves the path. It finds shared words only and says nothing about real search. See section 6 for the one real run |
| LLM (`ai.analyze`, `ai.find_segment`, `ai.editorial`) | `vidyom-mock-llm`, picks passages by word overlap and labels everything `[mock]` | The driver is real; no real model or vendor has ever answered it. Prompts are untested against a real LLM, and every real-LLM behavior is unknown |
| YouTube | `httptest` mock written from the same documentation as the connector | Upload, reconcile, quotas, processing and rejection reasons: **Validate**. No browser OAuth (I12): the token JSON goes in through `vidyom secret add` |
| GCS | `fake-gcs-server` | Signed URLs and ARCHIVE behavior: **Validate** |
| Loki, OTLP connectors in `go test` | `httptest` | Real Loki and Tempo only in the Compose profile |
| macOS Keychain, systemd-creds, TPM | Stubs, or `VIDYOM_TEST_REAL_KEYCHAIN=1` not run here | `file` is the provider actually exercised (I10), below D13's target and reported as such |
| The CLI's OS keyring | Unit tests with a stub. The suite and the demo use a plaintext test keyring build | Real Keychain path not exercised, by instruction |
| SSH transport of the CLI | A fake `ssh` | No real SSH server (`docs/decisions/cli.md`) |
| Mock destination | It is the mock | `mock-*` must stay out of a real configuration |

## 5. Skeletons and gaps

**Skeletons** that compile, report `Available: false` and return `not_implemented` (I11): Vimeo, PeerTube, Instagram Reels, MediaCMS, VideoVortex, Rewind.

**Not implemented:**

- `import:` inputs: the URI validates, opening it answers `not_implemented` (§3.4, §15.3 not demonstrated).
- `gcp-kms` unseal: answers `not_implemented`. `dpapi` compiles, never run.
- `webhook` workflow triggers, cron expressions.
- Remote plugins over HTTPS, plugin-provided nodes and drivers: refused at validation (I42).
- VAD before transcription (I11). Multi-source clips, declared lineage, externally edited videos: **Future**.
- `system.db`'s audit chain cannot be exported, because the system database has no `archive` role (I108).
- The library's real query embedder: `ai-worker` client is mock-tested; no real BGE-M3 has served the site, so with real vectors it answers lexical unless configured (D58).

- **§15.2 item 9, re-cutting a published clip into `v2`:** `catalog.next_version` exists and is tested, and no workflow or command reaches it. Today, a new cut is a new clip. Replacing a version's files in place before publication has no flow either.
- `mediacms.register` is in no shipped workflow, by design until a tenant enables it (D24, I159), so §15.1 item 9 is proven against a failing fake only.
- A publisher's metadata edit on a public video reaches the catalog and `public.db`, and is **not pushed to the destination**: `connector.Update` is only used for scheduling.

**Known limits, each decided and recorded:**

- `archivePolicy` (R29) is read and validated, and **no workflow uses it**: `ingest-derived` always backs up. So `latest` and `approved_versions` change nothing yet (I283).
- A job cancelled while waiting at the selection gate leaves its preview in `proxy` (I285). The content routes serve whole files, with no range requests.
- `local` proxy links are `file://` paths on the daemon's host; under Compose use `clip preview --output` (I198, I285).
- Workers share the scratch and spool volumes, so they must be on one host (I5, D41). Workers are trusted processes shared by tenants (I39).
- Not emitted: `vidyom_assets_without_verified_backup`, `vidyom_clips_in_editorial_review`, `vidyom_storage_stored_bytes`, `vidyom_ai_time_estimate_seconds_total`. Inline `flow.*` and `approval.request` nodes have no span.
- `vidyom_connector_*` covers storage in workers, transcription, embedding, LLM and publishing. Storage calls the daemon makes itself (`storage test`, proxy content) and catalog connectors are not counted.
- Windows: the CLI and worker cross-compile, the daemon is **Validate**; Compose is the path (I44). Not run.
- Litestream profile (I31): ran once, for seconds, writing snapshots and WAL segments of `system.db` and `tenant.db` to a local volume. Restoring from them was not tried, and its file lists only tenant `acme`.
- Image and voice authorizations are the tenant's responsibility (D29); the approval says so, and Vidyom tracks nothing.

## 6. The real-model proof

Everything above uses the fake engine. By hand, never in CI, the real models ran with `make real-model-proof` (`tests/e2e/realmodels_test.go`, not in CI): faster-whisper large-v3 (int8) and BGE-M3 on CPU, in the AI worker image under Docker Desktop on this Mac Studio. The speech is macOS `say`, six short stories (214.6 s) with no LLM configured. Each story has a paraphrase query sharing **no word** with anything said, including the small ones; the test refuses to start otherwise.

Numbers from the last run, 2026-11-09, on an otherwise quiet machine:

| Measure | Result |
|---|---|
| Models | `faster-whisper/large-v3`, `bge-m3@1`. 7.7 GB downloaded in about 75 s on the first run, from a volume in 11 s after that |
| Transcription | 177 s for 214.6 s of audio, about 0.83x real time, on the 24 vCPUs Docker Desktop has here |
| Embedding | 7.3 s for 5 windows, including loading the model |
| Whole ingest | 211 s |
| Word error rate against the script | 4.4% (51 segments). Two other runs of the same audio gave 6.2% and 6.9%, so runs differ. Numbers spoken as digits count as errors |
| Query latency, end to end through the CLI | 45 to 192 ms |

The transcript is close and not perfect. Its visible errors: "go out to **see** hungry" for sea, "her **rote** partner" for rope, "barely any" with "enough flour" lost, and the last two sentences repeated. In an earlier, shorter fixture it also invented "My grandfather kept a brook in his house."

Search, with `--limit 20`, over the 5 windows the chunker made. A window spans two stories, hence two ranks:

| Story | Paraphrase, no word in common | Story at | Top hit | Rank, first hit touching it | Rank, window holding most of it | Hybrid, same two ranks | Lexical hits |
|---|---|---|---|---|---|---|---|
| lost dog | `beloved pet reappears safely following severe weather` | 0–38 s | 0–50 s | 1 | 1 | 1 / 1 | 0 |
| baker | `elderly woman giving free loaves honored, grateful community` | 40–73 s | 45–93 s | 1 | 1 | 1 / 1 | 0 |
| violin | `concealed correspondence discovered, antique musical instrument, family secret revealed` | 75–110 s | 85–133 s | 1 | 1 | 1 / 1 | 0 |
| climber | `mountaineer survives blizzard overnight, rescuers airlift stranded alpinist` | 112–144 s | 129–178 s | 1 | 2 | 1 / 2 | 0 |
| chess | `shy pupil becomes champion, strategy game, credits mentor` | 145–178 s | 129–178 s | 1 | 1 | 1 / 1 | 0 |
| lighthouse | `coastal beacon guardian rescued shipwreck survivors, resisted automation` | 180–213 s | 172–213 s | 1 | 1 | 1 / 1 | 0 |

A window touching the right story came first for all six, and in five of six it was the window holding most of the story. For the climber, the top window held 14 s of that story and 33 s of the next, while the runner-up held 21 s of it. With 5 windows and about two touching any story, chance would give roughly 2 in 5 per query, so this is well above chance and still a very small sample. Semantic and hybrid agree because a paraphrase gives the lexical list nothing to add.

Clip proposal for the baker's paraphrase, with no LLM: 39.6 to 98.1 s for a story at 40.0 to 73.4 s, marked `ambiguous: true`, confidence 0.5. It holds the story and runs 25 s into the next, which is what the editor's adjustment is for. No LLM was configured in this run.

What this does and does not show:

- It shows the real path works: real transcription with word timestamps on the master's timeline, real vectors in sqlite-vec, query embeddings through the daemon, and semantic search that finds a passage with no shared word, which is what §15.2 item 1 asks. Lexical search returned nothing for all six, as it should.
- It is one small sample of clean synthetic speech, six stories in five windows. It is no accuracy measurement. Real recordings have noise, accents and Portuguese. **Transcription accuracy and word timestamps on target content stay Validate** (§17.2, D9, D56).
- Windows of 30 to 60 s are coarse against stories this short, so "rank" means which window, and a window touches two stories. That is why two ranks are reported.
- Query embedding latency is the daemon calling the Python worker. Nothing here measured load.
- The first real inference found real bugs (I295): an unpinned PyAV broke `faster-whisper`, and the clip proposal without an LLM was 137 s for a 33 s story. Both are fixed. With an LLM, the prompts are still untested against a real model.
- The public library has not served real vectors: its AI worker client remains mock-tested (D58).

To run it again: `make real-model-proof`. It writes `real-model-report.json` (ignored by Git). Models are cached in the Docker volume `vidyom-dev-integration-models`, 7.7 GB; remove it with `docker volume rm` when done. This proof and `make e2e-compose` also leave the `vidyom/*` images, about 3.8 GB, which `docker image rm` removes. A run also needs macOS for `say`.

## 7. Validate: §17.2

Nothing here closes a §17.2 validation. Each has a working default, and most have only been exercised on this host.

| Validation | State in this version |
|---|---|
| sqlite-vec performance at tenant scale; Litestream with GCS (D5) | Small data only. Tenant-wide scan capped at 50 000 vectors (I43), never load-tested. Litestream ran once to a local volume, never to GCS, never restored |
| Redis AOF and reconciliation under crash (D4) | `FLUSHALL` with queued jobs and under live workers passes, natively and in Compose. AOF replay after an abrupt Redis kill was not specifically run end to end |
| TPM, systemd-creds, KMS on target hosts (D13) | Not run. `file` is what ran. `gcp-kms` is not implemented |
| Master key per platform (D41) | `file` only. Keychain has a real-path test not run here; DPAPI compile-checked |
| AI on macOS without GPU in Docker (D41) | One sample, section 6: transcription about 0.83x real time on 24 vCPUs |
| MediaCMS (D24), VideoVortex | Skeletons |
| Transcription accuracy, word timestamps, diarization, audio from master (D9, D56) | One sample on synthetic English speech, section 6. Diarization not run. Master-derived audio is what ran |
| Publishing APIs (D25) | YouTube against a mock only; others skeleton |
| License compatibility | Not audited |
| LGPD review of deferred archive purge (D17) | Implemented as stated; review open |
| Public library hosting (D18, D34, D58) | `local-file` shipping, Next.js in a container. Vercel not tried. Real BGE-M3 for query embeddings in the site not run |
| Trademark check | Open, not blocking |
| Plugin sandbox on real Linux hosts | Landlock passes inside Docker Desktop's Linux on arm64 (`isolation: landlock`). Other kernels and `linux/amd64` not run |

## 8. Decisions you would most want to check

The full list is in `docs/decisions/integration.md` (I280–I295), and each component's log.

- **`ai.find_segment` is `paid`** (I282), so a tenant over budget pauses at clip selection.
- **D16 read literally** (I283): a render always has an approved selection, so its archive copy stays even if publication is rejected, and no new sweep was added. If you meant "approved for publication", this is the row to change. `archivePolicy` is still not wired.
- **Previews live in `proxy`** and the API streams them (I285). New routes `GET /approvals/{id}/preview/content` and `GET /assets/{id}/proxy/content`: additive to §12.1.
- **A person's `--retry-node` overrides a destination that cannot reconcile** (I288). Without that, `needs_attention` was a loop on such destinations. It can duplicate an upload on a destination that honors no key; the person chose that.
- **Compose's master key is a plaintext file** (I10, I290): below D13's target and reported as such by `doctor` and `/health`.
- **The worker can be scraped directly** with `--metrics-addr` (I294). `GET /metrics` on the daemon shows the daemon's process only.
- **Metadata edits on a public video reach `public.db` but not the destination** (section 5). There is no flow pushing a new title to the platform.
- **The website and README describe every phase as implemented**, by the other session's choice. This file is what the code does; I did not touch those files.

## 9. Disagreements left open

Found while assembling, not resolved in the sources: §15.2 item 9 asks for a `v2` when a published clip's cut is adjusted, and nothing reaches `catalog.next_version` (section 5). `docs/decisions.md` and each component's log list the others.
