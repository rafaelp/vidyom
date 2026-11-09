# First executable version: session report

A dated record of the session of 2026-11-09 that took Vidyom from a specification to its first executable version. It is not kept up to date. [`docs/status.md`](../status.md) describes the current state of the code.

## Outcome

The whole lifecycle works end to end on one host, natively and under Docker Compose: import an original, ingest, search, propose a clip, approve it, render it, publish it, update the public library and withdraw it. That run uses a fake AI engine, a mock LLM, a mock destination and local storage.

The real transcription and embedding models ran only by hand, on a small sample of synthetic English speech. Nothing has talked to real YouTube, real GCS or a real LLM vendor. Nothing has been tagged or released.

## What was asked

The maintainer asked for one agent per part of the code, each to write a plan, review it and take its own decisions on open questions, then implement every phase, with atomic commits and no stops for questions. The expected result was a first executable version.

Before that, the same session recorded decisions D53 to D59 in the specification, in commit `2981435`:

| Decision | Content |
|---|---|
| D53 | The editor's selection approval is always required |
| D54 | The REST prefix is `/api/v1` |
| D55 | Resumable uploads use the `UPL-` prefix |
| D56 | Audio for transcription always comes from the working master |
| D57 | The vault stays in Phase 1; Event Bus, budgets and audit move to a Phase 5 not yet defined |
| D58 | Query embeddings and `public.db` updates on the site's host stay **Validate** |
| D59 | `GET /workflows` is in the API |

## How it was built

A foundation came first: the [master plan](../plans/00-master-plan.md), the contracts in `internal/contract`, the schemas in `schemas/`, the shared [decision log](../decisions.md) and `scripts/commit.sh`. Nine agents then worked in parallel in one working tree on `main`, each owning its own paths, and an integration agent assembled the result.

| Component | What it delivers | Plan | Decisions |
|---|---|---|---|
| `core` | Daemon, one SQLite per tenant, `system.db`, vault, audit, identity | [plan](../plans/core.md) | [I100–I119](../decisions/core.md) |
| `api` | `/api/v1`, HTTPS and pairing, resumable uploads | [plan](../plans/api.md) | [I120–I139](../decisions/api.md) |
| `engine` | Workflows, checkpoints, approval gates, outbox, budgets | [plan](../plans/engine.md) | [I140–I159](../decisions/engine.md) |
| `cli` | The `vidyom` command | [plan](../plans/cli.md) | [I160–I179](../decisions/cli.md) |
| `media` | Storage drivers, FFmpeg nodes, media profiles | [plan](../plans/media.md) | [I180–I199](../decisions/media.md) |
| `ai` | Python worker, hybrid search, story detection, sensitive marks | [plan](../plans/ai.md) | [I200–I219](../decisions/ai.md) |
| `publishing` | Destination connectors, `public.db` export | [plan](../plans/publishing.md) | [I220–I239](../decisions/publishing.md) |
| `library` | Next.js public library | [plan](../plans/library.md) | [I240–I259](../decisions/library.md) |
| `extensibility` | Plugins, hooks, observability | [plan](../plans/extensibility.md) | [I260–I279](../decisions/extensibility.md) |
| `integration` | Wiring, acceptance suite, Compose, demo, CI, real-model proof | [plan](../plans/integration.md) | [I280–I299](../decisions/integration.md) |

Before this report the work came to 190 unpushed commits, 177 of them after the foundation. Outside `website/` and `spec/`, the tree has 132,839 lines of Go in 465 files (142 of them test files), 2,231 lines of Python and 7,782 lines of TypeScript.

## How to run it

```bash
scripts/demo.sh          # the lifecycle, about 2.5 minutes, leaves nothing behind
make e2e                 # the 13 acceptance steps and 14 extended paths
make e2e-compose         # the same against images built from deploy/
make real-model-proof    # real models, macOS only, downloads 7.7 GB
```

`make e2e-compose` and `make real-model-proof` leave the `vidyom/*` images behind, and the proof also leaves its models volume. [`docs/status.md`](../status.md) has the manual Compose walkthrough.

## Verification

After the integration agent finished, every check below was run again, in sequence so one step's load could not affect the next. All ran on one Mac Studio (macOS arm64, Docker Desktop with Linux arm64 containers), at commit `6c1bb95` unless noted. Later commits changed one test fixture and documentation only.

| Check | Result |
|---|---|
| `go build ./...`, `go vet ./...` | Pass |
| `make fmt-check check-static` | Pass |
| `make cross` | Pass |
| `go test -count=1 ./...` | 76 packages ok, 35 with no tests, 57 s |
| `make test-race`, at `baa64c0` | 76 packages ok |
| `VIDYOM_JOBS_REAL_STORE=1 go test ./internal/jobs`, at `baa64c0` | Pass, 35 s |
| Three `go test -count=3 ./...` at once, at `baa64c0` | 9 full passes, no failure |
| `make test-python` | 75 ok, 2 skipped |
| Library: typecheck, lint, unit tests, build, tests over HTTP | Pass: 199 unit, 32 over HTTP |
| `make e2e` | 26 pass, 1 skipped (`x11`, Compose only), 244 s |
| `make e2e-compose` | 26 pass, 1 skipped (`x10`, no socket on the host), 336 s |
| `scripts/demo.sh` | Exit 0, 138 s |
| `make real-model-proof` | Pass, 329 s, including 86 s to download and load the models |

Windows and x86-64 have run none of this. The earlier failures in `internal/workers` never came back, and they are not claimed fixed.

### The one failure with no known cause

The integration agent left `TestAutoMasterOf2160pIsHEVCAt2160p` failing about once in 100 parallel runs, with its cause unknown. Parallel encodes with a verbose log traced it: `libx265` sometimes gives the first packet of a very short stream a garbage DTS, and the MP4 muxer refuses it.

| Stream, 24 parallel encodes | Failures |
|---|---|
| 1 frame | 45 of 300 |
| 2 frames, the test's old fixture | 20 of 600 |
| 2 frames, `bframes=0` | 0 of 600 |
| 3, 4 and 6 frames | 0 of 600 each |

The test now has three frames and passed 400 parallel runs. The product keeps no workaround, because real recordings are not that short ([I296](../decisions/integration.md)). Why x265 produces that value was inferred from these results, not read in its source.

## What the real models showed

faster-whisper large-v3 and BGE-M3, on CPU in the AI worker image, on 214.6 s of six stories spoken with macOS `say`, with no LLM configured. Each search query is a paraphrase sharing no word with the speech.

| Measure | Result |
|---|---|
| Transcription | 176 s, about 0.83x real time, on 24 vCPUs |
| Word error rate | 6.2% in this run; 4.4% and 6.9% in earlier runs of the same audio |
| Lexical search | No hits for any of the six queries |
| Semantic search, top hit touches the right story | 6 of 6 |
| Semantic search, top hit holds most of the story | 5 of 6 |

This is one small sample of clean synthetic speech. It shows that the real path works and that semantic search finds a passage with no shared word. It is not an accuracy measurement: Portuguese, noise and accents remain untested. [Section 6 of the status file](../status.md#6-the-real-model-proof) has the details.

## Problems found by assembling the parts

Each component passed its own tests against fakes. These surfaced only once the parts ran together:

- The worker's directory defaults differed from the daemon's.
- `speaker` never reached the catalog or `public.db`. Fixed from ingest through export ([I281](../decisions/integration.md)).
- `publicexport` looked one level up the lineage, so a sensitive mark added later to a grandparent stayed in `public.db` and captions ([I284](../decisions/integration.md)).
- `publish-one` never asked for captions, so no destination received any.
- Job-level hooks received no inputs, so the sample plugin blocked every job, and `finish` did not run when `before` blocked a job ([I289](../decisions/integration.md)).
- `job resolve --retry-node` looped forever on a destination with no reconcile ([I288](../decisions/integration.md)).
- A publisher's metadata edit never reached `public.db`.
- `vidyom clip preview` had nothing a reviewer could play ([I285](../decisions/integration.md)).
- An unpinned PyAV broke `faster-whisper` outright, and without an LLM paraphrase hits chained into a 137 s proposal for a 33 s story ([I295](../decisions/integration.md)). Only real models could reveal these two.

## What is not proven

[Sections 4 and 5 of the status file](../status.md#4-tested-only-against-mocks-or-fakes) have the full list. In short:

- **Mock only:** YouTube, GCS and every LLM answer. The `file` master key is what ran; Keychain and systemd-creds did not.
- **Skeletons:** Vimeo, PeerTube, Instagram Reels, MediaCMS, VideoVortex and Rewind.
- **Not implemented:** `import:` inputs, `gcp-kms`, webhook and cron triggers, remote plugins, plugin-provided nodes and connectors, VAD and browser OAuth for YouTube.
- **Specification gaps:** §15.2 item 9, because re-cutting a published clip does not produce a `v2`. `archivePolicy` is validated and no workflow reads it. A metadata edit on a public video does not reach the destination.
- **Validate:** every item in §17.2 stays open.

## Decisions for the maintainer to review

Each was taken without asking, as instructed, and each can be changed.

| Decision | Choice | Why review it |
|---|---|---|
| [I283](../decisions/integration.md) | D16 read literally: a render rejected at publication keeps its archive copy | Change it if D16 meant "approved for publication" |
| [I249](../decisions/library.md) | Hosts without an AI worker beside the site get lexical search only, Vercel included | Deviates from D18 |
| [I1](../decisions.md) | The cgo fork `rqlite/go-sqlite3` | `ncruces/go-sqlite3` could drop cgo, but changes D5 |
| [I10](../decisions.md), [I290](../decisions/integration.md) | Compose's master key is a plaintext file | Below D13's target, and reported as such by `doctor` and `/health` |
| [I288](../decisions/integration.md) | A person's retry overrides a destination that cannot reconcile | Can duplicate an upload |
| [I211](../decisions/ai.md) | A search with no hit fails the node | An editor cannot pick a passage by hand |
| [I8](../decisions.md) | Phase 5 built in this version, in the order audit, Event Bus, budgets | D57 left Phase 5 undefined |
| [I285](../decisions/integration.md) | Two new preview routes | Additive to §12.1 |
| [I269](../decisions/extensibility.md) | Plugin-provided nodes and connectors are refused | Includes §7.6's own example |
| `ingestRoots` ([media log](../decisions/media.md)) | Daemon-wide, not per tenant | Left open |

## Disclosures

- **Next.js telemetry.** An early spike in `library` ran plain `next build`, which probably sent telemetry to Vercel once or twice. Later controlled runs would have made the npm advisory lookup. Project builds now disable telemetry.
- **Commits that do not build alone.** `8462a1d` is fixed by `6d6d0a0`. `fb1ed7a` is configuration only, and the build returns at `1edf884`.
- **The website describes every phase as implemented,** by the maintainer's decision on its voice. Compare it with [`docs/status.md`](../status.md) before relying on any claim about what runs. The README keeps that voice and has a Status section that says what is not ready.
- **The CLI reference disagrees with §11** on `job resolve --action`, `device pair-code`, `budget status` and `asset verify`, in `website/docs/reference/cli.md`.
- **Another session's work went out in the same push.** Commits `5d753fa` and `e1f7256` rewrote the website, the README, the contribution files and `AGENTS.md`. They arrived after the checks above and change no runtime code. The website's own CI covers them, and the checks above do not.

## Next steps

1. Check the first run of `.github/workflows/ci.yml`, which is also the first time this code runs on x86-64 Linux.
2. Run a real recording in Portuguese through the real models. This is what shows whether transcription and search work on the content Vidyom is for.
3. Try a real LLM, then real YouTube and GCS, closing their **Validate** items.
4. Close the gap in §15.2 item 9.
5. Review the decisions above.
