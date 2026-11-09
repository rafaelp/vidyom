# Implementation decisions: `publishing`

Decisions of the `publishing` component, numbered from its range `I220–I239` (master plan, section 8). Same format as [`docs/decisions.md`](../decisions.md): the choice, the reason and what would make us revisit it. None of them changes a maintainer decision `Dn`. What this log says the component does is a **target** until its commits land; the final report states what is done and what is mock-tested only.

## Publication lifecycle

### I220. Publication transitions

- **Choice.** D52 fixes the states, not the moves between them. `nodes/publishing` enforces this table in one helper:

  | From | To |
  |---|---|
  | `pending_approval` | `uploading`, `processing`, `published`, `scheduled`, `failed`, `rejected_by_destination`, `withdrawn` |
  | `uploading` | `processing`, `scheduled`, `failed`, `rejected_by_destination`, `withdrawn` |
  | `processing` | `published`, `scheduled`, `pending_approval`, `failed`, `rejected_by_destination`, `withdrawn` |
  | `scheduled` | `published`, `failed`, `rejected_by_destination`, `withdrawn` |
  | `published` | `rejected_by_destination`, `withdrawn` |
  | `failed` | `uploading`, `processing`, `rejected_by_destination`, `withdrawn` |
  | `rejected_by_destination` | `withdrawn` |
  | `withdrawn` | none |

  Repeating a state is allowed, so retries stay idempotent. Publishing again after a withdrawal creates a new `PUB-nnn`; the old record is history. A private-first upload that finished waits in `pending_approval`, which is what it is waiting for; once `publish.prepare` adopts it with an approval, `publish.await_processing` moves it back to `processing` until `publish.finalize` runs. `published` with visibility `private` is legal: the destination confirmed the video, nobody can see it, and D35 applies.
- **Why.** §12.4 says each publication is an independent record, and the audit question "what happened to this upload" needs history.
- **Revisit if.** The maintainer wants a dedicated `uploaded` state.

### I221. Approval gate

- **Choice.**
  - `publish.prepare` and `publish.upload` both check D37, so a workflow cannot skip the gate by passing a publication ID.
  - An approval counts when it is `approved`, of kind `publication`, on this asset, and its version is empty or this version. `publish.finalize` also accepts kind `visibility_change`.
  - Without an approval, only `allowPrivateUploadBeforeApproval` allows an upload. The destination's value wins over `defaults`, and the upload is `private`.
  - Any visibility other than `private` needs an approval at `publish.finalize`. Changing an already published video needs a **new** approval, different from the one it carries (D7).
  - One approval may authorize several destinations, as §12.4 shows.
  - **`requireApproval: false` does not lift the gate in this version.** `config.PublishingDefaults.RequireApproval` is a plain `bool`, so "absent" and "explicitly false" look the same, and a file with no `defaults` would turn approvals off. D7 and D37 define no path without approval. `uploadResumable` and `savePublicationRecord` are likewise not switches: uploads are resumable when the destination supports it (§7.3), and the record is always saved (§12.4).
- **Why.** Fail closed. §7.3 says "not publicly visible without approval by default" but no decision states what turning it off would mean.
- **Revisit if.** The maintainer defines a no-approval mode: it then needs a pointer field or an explicit `unsafeSkipApproval`, which is additive.

### I222. One active record per asset, version and destination

- **Choice.** `publish.prepare` looks up its idempotency key first. If another job already has a record for the same asset, version and destination in any state except `failed`, `rejected_by_destination` or `withdrawn`, it adopts that record, keeps its idempotency key, and adds its own approval if the record has none. So a second `vidyom publish` of the same version to the same destination is not a second upload. `already_published` is true when the adopted record is `published` at the requested visibility. A different visibility is false, so the flow reaches `publish.finalize`, which requires a new approval (D7). A per-publication lock in the daemon stops `publish.upload` and `publish.withdraw` running at once on the same record; a second caller gets a transient error.
  `domain.Publication` has no tags field, and §7.3's list of recorded fields has none. So `tags` on `publish.prepare` merge into the asset's tags with a history entry, and uploads send the asset's tags.
- **Why.** §15.5 forbids duplicate uploads, and YouTube has no server-side idempotency key, so the protection has to be here. Failed and rejected records stay as history, and a new attempt gets a fresh key, because the old key may have been recorded by the destination in a state that blocks it.
- **Revisit if.** Per-destination tags are needed: that is an additive field that `core` must persist.

### I223. How a connector reports an unknown outcome

- **Choice.** `connector.OutcomeUnknown(err)` and `connector.IsOutcomeUnknown(err)`, additive. Connectors cannot import `node`, because `node` imports `connector`. `publish.upload` maps them to `node.Unknown`. Other errors classify by `errs` code: `unavailable`, `timeout` and `rate_limited` are transient, everything else is permanent. A record stays `uploading` on transient and unknown errors, and becomes `failed` on a permanent one, so a restart reconciles rather than assumes. A cancelled context writes `failed` with reason `upload cancelled`, except on an unknown outcome; a later retry moves it back to `uploading` only with no remote ID.
- **Why.** D11 hinges on telling "did not happen" from "might have happened". An error code cannot say that, because a timeout can be either.
- **Revisit if.** Connectors of other families need the same signal: it can move to `errs`.

### I224. Uploads are private until `publish.finalize`

- **Choice.** Every upload goes out as `private`, even with an approval for `public`. `publish.finalize` sets the thumbnail and captions first and opens visibility last. A destination that declares `Visibilities` without `private` receives the approved target directly, and with no approval it is refused. `Publication.Visibility` holds the intended visibility until `published`, and what the destination confirmed after that.
- **Why.** A video must not be public without its captions and thumbnail, and a crash between upload and finalize must not leave an unfinished public video. Setting public is the one external effect worth isolating (§7.3).
- **Revisit if.** A destination charges for visibility changes or cannot change them later.

### I236. Withdrawal details

- **Choice.** `publish.withdraw` needs `destinations` or `all: true`; neither is an error, never "all by default". A name in `destinations` that is neither configured nor present on a record is an error, so a typo does not read as success. Default `mode` is `private`, which is reversible and which §15.8 allows; a destination without a private state is deleted instead. "Not found" at the destination counts as done. A record with no remote ID that is `uploading` reconciles first, because a video may exist. `failed` or rejected records with no remote ID go to `already`. Destination failures do not stop the others, and the node then returns a transient error. With no active record left, the asset moves to `withdrawn` and `private`. No approval (D17). Each visibility change is audited.
- **Why.** D17 asks for immediate and repeatable. Deleting by default would be irreversible on a request that §15.8 satisfies with `private`.
- **Limit, stated plainly.** While a destination fails, its record stays active and the video stays in `public.db`, because the video really is still public there. A retry or a person resolves it.
- **Revisit if.** The maintainer wants the library entry to drop before the destination confirms.

## Connectors

### I225. YouTube has no URL field, so the connector composes one from a confirmed ID

- **Choice.** `videos.list` returns no URL. The connector returns `https://www.youtube.com/watch?v=<id>` only when that call confirmed `uploadStatus: processed`, using the ID from the response. Before that `URL` is empty and `GetURL` fails with `failed_precondition`. `publish.finalize` then cannot write `published`.
- **Why.** R43 says `published` needs an ID and URL from the destination; D45 says never invent links. For this API the confirmed ID plus its one documented watch form is the closest honest reading.
- **Revisit if.** Validation of the publishing APIs (§17.2) finds a case where a processed video is not reachable at that URL.

### I226. YouTube client: plain REST, not `google.golang.org/api/youtube/v3`

- **Choice.** `net/http` and `golang.org/x/oauth2` only. I32 lists the generated client, but this component does not use it, and `integration` may drop it at `go mod tidy` unless something else needs it.
- **Why.** D11 needs to know whether the request that carried the last byte got an answer, and the generated client's retry loop hides that. It also retries with its own backoff, which delays cancellation. The calls used are small JSON documents and one documented protocol.
- **Limit, stated plainly.** Nothing here has spoken to real YouTube. The wire format follows the public documentation and is tested only against a mock written from the same documentation (I12). **Validate.**
- **Revisit if.** The §17.2 validation finds wire differences: switching costs one file.

### I227. YouTube sessions and reconcile

- **Choice.**
  - Without `stateDir`, session URIs stay in memory. A restart abandons the session, which can never create a video, and starts over.
  - With `stateDir`, sessions persist per idempotency key in `<stateDir>/<tenant>/<destination>/`, mode `0600`, sealed with AES-256-GCM under `HKDF-SHA256(credential JSON, "vidyom-youtube-session-v1")`. A session URI works as a bearer capability, so it never sits in plaintext on disk. Rotating the credential makes old records unreadable, which is treated as "no record", except that reconcile then scans.
  - `Reconcile` order: a record with a video ID → that video. A record whose session answers `308` → nil, and the next `Upload` resumes it. `200/201` → that video. Session gone → scan. No record with `stateDir` and a readable store → nil, because a session is saved before its first byte. Otherwise scan.
  - The scan reads the channel's uploads playlist, newest first, up to `reconcileScanLimit` videos (default 500), for the tag `vidyom-<first 32 hex of key>`. Finding nothing with pages left is an `unavailable` error, which the node treats as unknown, so the job moves to `needs_attention`.
- **Why.** Reconcile must be exact or admit it cannot be (D11). The tag is an opaque hash and reveals nothing. The playlist is eventually consistent, which is why a persisted session is the primary answer and the scan the fallback.
- **Limit, stated plainly.** Without `stateDir`, a crash in the instant after finalization, combined with a playlist lag, could produce a second private video. It would never be public, since uploads are private (I224). **Validate** the lag.
- **Revisit if.** The maintainer wants sessions in the vault, which needs a write path `connector.SecretResolver` does not have.

### I230. YouTube credential format and base URLs

- **Choice.** `credentialsRef` resolves, at first use and not at factory time, to JSON with `client_id`, `client_secret`, `refresh_token`, optional `access_token` and `expiry`. This is the documented gap of I12: `vidyom secret add youtube-oauth --location server` takes it through stdin, with no browser flow. `token_uri` in the JSON is ignored; only the destination's `oauthUrl` parameter can change the token endpoint, so a secret cannot redirect a refresh token. `oauthUrl`, `apiBaseUrl` and `uploadBaseUrl` default to Google's and exist for tests; each must be HTTPS unless its host is loopback. Parameter names avoid `token` because `connector-config.v1.json` rejects such keys. Errors never carry the credential, an access token or a session URI: `*url.Error` is unwrapped because its text contains the URL.
- **Why.** §9.1, §15.6. A connector that opens without credentials also lets `public.db` read its public links with no vault access.
- **Revisit if.** The browser flow lands (R36).

### I234. Mock destination details (extends I30)

- **Choice.** Parameters: `stateDir` (required, no default, so state cannot silently vanish), `processingSeconds`, `reconcile` (default true; false returns `capability_unsupported`, to exercise `needs_attention`), `inject` with `fail_upload`, `outcome_unknown` (stored, answer lost), `outcome_unknown_lost` (not stored, answer lost), `reject`, `fail_remove`. Counters are consumed in the state file, so an injection fires once across restarts. A `fail_upload` happens midway, and the next attempt resumes from a persisted offset. Captions are stored in the state file so tests can scan what reached the destination; the video bytes are hashed and discarded. `PublicLinks` returns an embed URL under `media.example.internal`, supports timestamps, and has no thumbnail.
- **Why.** I30 asks for persistence and injection; `integration`'s E2E needs evidence it can read from a file.
- **Revisit if.** Never for production; it stays out of default configuration (I30).

### I235. External catalog failures never stop a job

- **Choice.** `mediacms.register` always completes. With no instance of driver `mediacms` configured, `catalog_sync` stays as it is and the output says `none`. Otherwise it tries up to three times on transient errors, writes `catalog_sync` as `synced` or `failed`, logs a warning on failure, and returns success. A skeleton answers `not_implemented`, so a configured MediaCMS records `failed`. A workflow's `continueOnFailure` remains harmless.
- **Why.** R11 and §15.1 item 9 require that no catalog failure affects readiness. Relying on every workflow author to remember an option would leave that guarantee to chance.
- **Revisit if.** The real connector lands and needs longer retries: `catalog_sync: failed` is the hook for a retry workflow.

## Captions and the public dataset

### I228. Public captions are always rendered from the stored transcript

- **Choice.** `publish.finalize`'s `captions` parameter is `{ "language", "format" }`, with `format` `vtt` (default) or `srt`. The node reads the version's transcript and sensitive marks and renders captions itself. It accepts no file. It uploads captions only for an approved `public` target, and a destination without the capability is skipped and reported in the output. No transcript is a skip, not an error.
- **Why.** D30 requires public captions without sensitive segments, and I5 forbids the daemon from reading scratch, where `ai.export_transcript` writes. Rendering here makes the guarantee independent of how a workflow was written.
- **Revisit if.** Hand-edited captions need to be published: they would be stored as a `captions` file on the version and still checked against the marks.

### I229. `publish.await_processing` polls and does not suspend

- **Choice.** It polls inside the task with backoff (2 s to 60 s, overridable for tests), default `timeoutSeconds` 21600. `node.Suspension` is designed for approvals and `flow.wait`, and this component cannot assume the engine resumes a daemon-pool task. A timeout is transient and leaves the record `processing`; `publish.refresh` can pick it up.
- **Why.** Simple, cancellable, restart-safe: class `none` runs again and re-reads remote state.
- **Revisit if.** Long processing holds too many task slots: a `flow.wait` loop in the workflow or a suspension can replace it.

### I231. What `public.db` withholds

- **Choice.** A record enters only if: `Publication.IsPublic()`; its `approval_id` resolves to an approved approval; the asset is not deleted; its visibility is `public`; a derivative is `published`. An original needs no `published` status, because §2.5 reserves that state for derivatives while the schema allows `kind = 'original'`.
  - Marks count from the asset version itself and from each lineage segment's source version, mapped live onto the clip's timeline. So a mark added after the clip was derived still applies, closing the limit stated in I207.
  - An overlapping transcript segment, chapter or vector is dropped whole: no partial segments, and a chapter's title or summary might carry what was marked.
  - If marks cannot be read for a video, the video stays listed with no transcript, chapters or embeddings.
  - The publication approval is what authorizes releasing the transcript. §13.2 says "reviewed and explicitly authorized", D30 says "together with an approved public video", and no separate gate exists.
- **Why.** R44, D18 and D30, failing closed at every step.
- **Revisit if.** The maintainer wants a separate transcript-release approval: `approval show` already has to state what a publication approval covers.

### I232. Where exported vectors come from

- **Choice.** Three levels, in order.
  1. `repo.EmbeddingLister`, a new optional interface on `SearchRepo`, returns stored vectors of the active model.
  2. If the store lacks it, `connector.Embedder` embeds the released text now: windows of 30 to 60 s with about 15% overlap, plus chapters. The text has already passed the sensitive filter.
  3. If no embedder works, no vectors are written and `embedding_model` is empty, so the site reports lexical mode (R18).

  One file holds one model. A vector with no matching model is skipped, never mixed.
- **Why.** `SearchRepo` has no read path for vectors, and `core` may not have the optional interface when this lands. Re-embedding the whole tenant on every sync is slow with a real model, so level 1 is what production should use. **`core` / `integration`:** implement `repo.EmbeddingLister`.
- **Revisit if.** Export time matters before level 1 lands: cache vectors by text hash from the previous `public.db`.

### I233. `public.db` file details

- **Choice.**
  - **IDs.** `public_id` is `HMAC-SHA256(PublicIDKey, "public-id\0" + asset ULID)`, lowercase base32 with no padding, first 24 characters (120 bits). `source_group` uses the label `source-group\0` over the root asset's friendly ID, empty when there is no lineage. Lowercase and length 24 mean it can never match the ULID pattern, so the scan test stays exact.
  - **Rows.** One row per asset. Version is the newest among public publications. `published_at` is the earliest publication date. Title and description come from the newest publication, else the asset. `speaker` is empty, since no contract field holds it.
  - **Destinations.** `destination` is the driver name, not the logical name: `mock-main` is configuration and stays private. `PublicLinks` comes from the new optional `connector.PublicLinker`, without network calls. Missing means no embed and no timestamps.
  - **Thumbnails.** `thumbnail_url` comes only from `PublicLinks`. YouTube returns its `i.ytimg.com` form built from the confirmed ID, **Validate**; `mock` returns none. Never a bucket.
  - **File.** Written with one connection, `wal_checkpoint(TRUNCATE)`, `journal_mode=DELETE`, `integrity_check`, closed, then checked to have a DELETE-mode header and no side files, `fsync`, SHA-256, rename, `fsync` of the directory. Data-directory copy mode `0600`.
  - **Guard.** Before writing, every released value of a video is checked for its asset's ULID, its stored files' keys, `storage://`, and any ULID-shaped word (URLs are exempt from the shape check only, since a platform may use such IDs). A hit drops **that video** and reports it in `Result.Withheld` without the value. It does not fail the export, because one bad title must not stop a withdrawal from taking effect on `public.db`. Friendly IDs are checked by tests only, because a person may legitimately write "CLIP-001" in a description. The raw-byte scan for keys, ULIDs and private text lives in the test suite.
  - **Empty set.** An empty file is still written and shipped, so a withdrawal of the last video takes effect (R15). `Clear` is for removing a tenant.
- **Why.** I29; the library's plan relies on a single checkpointed file renamed into place. A `WAL` header would need a writable directory on the site's host.
- **Revisit if.** D58's validation needs another shape.

### I237. `local-file` layout

- **Choice.** Parameter `directory`, absolute, required. Target `<directory>/<tenant>/public.db`, matching I29's `library/data/<tenant>/public.db`. Copy to a temporary file beside the target, verify SHA-256 and size, `fsync`, rename, `fsync` the directory. Directories `0755`, file `0644`, since the content is public. `ShipReceipt.Destination` is the driver name, not the path. `Clear` removes the file and treats absence as success. The library names one file through `VIDYOM_PUBLIC_DB` (I240 range), which `integration` points at this target.
- **Why.** I29. A reader holding the old file keeps a valid snapshot through the rename.
- **Revisit if.** D58 picks another way to reach the site's host: that is a new driver.

## Wiring

### I238. `Register` takes options, and the service lives with the nodes

- **Choice.** `publishingnodes.Register(reg node.Registrar, opts ...Options)`, with `Options{Exporter, Connectors, PollMin, PollMax}`. The call shape of the master plan's section 9 still compiles. With no options every node registers, and `catalog.public.sync` fails with `failed_precondition` saying it is not wired; `mediacms.register` records `catalog_sync = failed` as I235 requires. `node.DaemonEnv` has no public catalog or external catalog accessor, and adding one would change an interface `engine` implements. `service.Publications` is `publishingnodes.NewService(ServiceDeps)`, because it shares the state rules with the nodes. `connector.PublishingResolver` is `connectors/publishing/resolver`, mirroring `connectors/storage/resolver`.
- **Why.** Section 7 forbids adding methods to `DaemonEnv`; section 9 allows extra options.
- **Revisit if.** `integration` prefers these accessors on the environment.

### I239. Budget usage for uploads

- **Choice.** `publish.upload` reports one `UsageRecord`: operation `publish.upload`, provider the driver, quantity in `GiB`, priced from `config.BudgetCosts.PerUnitMinor` when listed and zero otherwise, recorded only when an upload actually happened. Nothing on reconcile or repeat.
- **Why.** The catalog marks the node `paid` (D19). `ai` prices its operations the same way.
- **Revisit if.** Real costs are known; D19 stays an estimate.

## Disagreements found, not resolved in the sources

Per `AGENTS.md` these are pointed out. The implementation's reading is listed.

| # | Where | Disagreement | This implementation |
|---|---|---|---|
| 1 | I32 vs D11 | I32 lists `google.golang.org/api/youtube/v3`; D11 needs outcome detail that client hides | Plain REST (I226) |
| 2 | §7.3 `requireApproval` vs D7, D37 | The option exists but no decision says what `false` means, and a `bool` cannot tell absent from false | Not honored (I221) |
| 3 | R43 vs the YouTube API | "the destination returns an ID and URL"; the API returns only an ID | URL composed from a confirmed ID (I225) |
| 4 | §13.2 vs D30 | "explicitly authorized" transcript vs "together with an approved public video" | The publication approval authorizes it (I231) |
| 5 | §2.5 vs `schemas/public-db/v1.sql` | `published` is for derivatives; `videos.kind` allows `original` | Originals skip the status check (I231) |
| 6 | Catalog `publish.finalize` `captions` vs I5 | The object was unspecified, and `ai.export_transcript` writes to scratch, which the daemon never reads | Rendered from the stored transcript (I228) |
| 7 | Master plan section 9 vs `node.DaemonEnv` | `publishingnodes.Register(node.Registrar)`, but `catalog.public.sync` needs a public catalog and exporter the environment does not offer | Variadic options (I238) |
| 8 | §13.2 "search by instructor" vs contracts | No contract field holds a speaker | `videos.speaker` empty (I233) |
| 9 | `repo.SearchRepo` vs D18 | Vectors must be exported but cannot be read back | Optional `EmbeddingLister` with fallback (I232) |
