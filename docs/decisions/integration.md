# Implementation decisions: `integration`

Decisions of the `integration` stage, numbered from its range `I280–I299` (master plan, section 8). Same format as [`docs/decisions.md`](../decisions.md): the choice, the reason and what would make us revisit it. None of them changes a maintainer decision `Dn`. The maintainer delegated these; he is not available during this run.

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
