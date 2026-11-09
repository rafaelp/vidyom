# Implementation decisions: `media`

Decisions the `media` component took to fill gaps, in the format of [`docs/decisions.md`](../decisions.md), numbered in its range `I180`–`I199`. They never change or relabel the maintainer's `Dn`. What a decision says a version does is a **target** until the component commits it. **Validate** marks a working default that rests on a §17.2 validation still unproven.

---

## Worker runtime

### I180. The worker gets its queue through a second seam

- **Choice.** `workers.Config` takes a `jobqueue.Queue`. `cmd/vidyom-worker/main.go` declares, beside `wireRegistries`, `var wireQueue func(ctx context.Context, redis config.Redis) (jobqueue.Queue, func(), error)`. Both seams exit with a clear "not wired" error while nil. `integration` sets both in `wire.go`.
- **Why.** The master plan's section 9 lists `queue.NewRedis` under `engine`, and section 8 forbids importing it from `media`. Reimplementing Redis reads here would couple two components to a stream entry format the contract does not fix. Only `wire.go` may import across components. A third seam, `wireMetrics`, is optional: left nil, the worker records no metrics, and everything else works.
- **Revisit if.** The stream entry format moves into `internal/contract/jobqueue`; a built-in consumer could then remove the seam.

### I181. Scratch cleanup: a cleanup pointer at job end, an idle reaper as the net, plaintext copies at task end

- **Choice.** Three layers.
  1. **Task end.** Plaintext copies of cached objects, the only plaintext of a cached master, live in a hidden per-task directory and are removed when the task ends, whatever the outcome.
  2. **Job end.** Additive `jobqueue.NodeTypeScratchCleanup` and `jobqueue.ScratchCleanupPointer(tenant, job, at)`. `engine` enqueues that pointer when a job ends (I153: on `completed` and `cancelled`; a `failed` job keeps its files for `job retry`). A worker reading it removes `<scratch>/<tenant>/<job-ulid>/` and acks, with no lease, because no result needs committing and no parameter travels. Tenant and ULID are validated before any path is built.
  3. **Net.** Every worker reaps job directories idle for 7 days (`--scratch-max-idle`), so a lost pointer (Redis emptied) or an unwired daemon cannot fill the disk. A lease touches its job's directory, so running jobs are never reaped.
- **Why.** I45 gives cleanup to `media`, and D22 asks for deletion at job end, but `workerproto` has no job-end signal and the daemon never touches scratch (I5). A pointer needs one line in `engine`, no new route, and fits "Redis holds pointers only". Seven days matches D22's idle period. The only scratch file a long approval wait keeps is the preview: `video.cut` reads the master after the gate, so a reaped preview loses the file shown in review and nothing else.
- **Revisit if.** Workers run on several hosts with no shared volume (I5's Future case), or jobs legitimately resume from scratch after more than 7 days.

### I182. A child job may read its ancestors' scratch, by an explicit list on the lease

- **Choice.** Additive optional `workerproto.Lease.ScratchJobs []id.ULID`: ancestor jobs whose scratch this task may read. `Scratch.Resolve` accepts the job's own files and those of listed jobs, under the leased tenant only, and refuses everything else. `engine` fills it from `JobTrigger.ParentJobID` up the chain.
- **Why.** §6.4 passes `${nodes.render.output.file}` into `ingest-derived`, a child job with its own ULID, while `node.Scratch` documents refusing other jobs' files. The contract's rule is kept and the exception is stated by the source of truth rather than inferred by the worker. Clients cannot supply `scratch://` at all (R20, I5).
- **Revisit if.** Child jobs stop passing files by reference.

### I183. `upload:` references reach workers as ULIDs

- **Choice.** The spool is named by ULID (I20) and workers have no database, so a worker resolves `upload:<ulid>` at `<spool>/<tenant>/<ulid>`. For `upload:UPL-nnn` it looks for a same-named link in the tenant's spool and otherwise fails with `failed_precondition`, naming what is missing. The daemon rewrites `upload:UPL-nnn` to the ULID form when it accepts a run, as it canonicalizes `server:` paths (R20, `api`'s I131).
- **Why.** The mapping lives only in the tenant database. A new worker route would be heavier than one rewrite in the code that already loads the upload record to validate it.
- **Revisit if.** Workers stop sharing the spool volume.

### I194. Lease tolerance, cancellation and failure codes

- **Choice.**
  - A failed task heartbeat does not stop the node. The daemon enforces the lease when the result arrives, through `lease_id`. After `DaemonOutageLimit` (default 1 h) of consecutive failures the node is aborted. `410` on a heartbeat aborts at once and drops the result.
  - `complete` and `fail` are re-delivered with capped backoff until acknowledged. `410` means another owner: ack and drop. Nothing else acks, so a `404`, which the engine never answers for its own tasks, is retried as a misrouted request. After `DaemonOutageLimit` delivery gives up, leaving the entry pending, so a poisoned task cannot hold a slot forever.
  - Plaintext inputs go when the node ends, before delivery, which can last an outage.
  - The heartbeat's cancel flag cancels the node's context only when its descriptor has `SupportsAbort`. Otherwise the node finishes (D11). A cancelled node is reported with `Code: "cancelled"`, not retryable.
  - Graceful shutdown waits `--shutdown-grace` (default 30 s), then cancels nodes and reports `unavailable`, retryable.
  - Stale claiming takes batches of 16, so entries of long tasks cannot hide a dead consumer's, and skips tasks running here. Claiming uses twice the lease period as minimum idle, and a live lease elsewhere answers through the daemon.
  - A node heartbeat answered `not_found` means the daemon lost this worker (`engine`'s I154), so it registers again.
  - The job's stream, which clients read, gets a failure's public reason only. The internal cause and a panic's stack go to the worker's own log (I190).
- **Why.** §1.1 step 13 kills `vidyomd` during a transcode. Self-fencing on a heartbeat timeout would throw away a long transcode because of a short daemon restart, while the daemon's lease check is what actually prevents a double commit.
- **Revisit if.** Workers run across a network that can partition them from the daemon while another worker still reaches it: a wasted duplicate run is then possible, still never a double commit.

### I195. Metering happens in the environment, not in each node

- **Choice.** The task environment wraps every storage it opens and counts bytes. On completion the runner adds `domain.UsageRecord{Operation: "storage.egress" | "storage.ingress", Provider: <driver>, Quantity: <GB>, Unit: "GB"}` per driver, with `EstimatedMinor` left at 0 for the daemon to price (D19, I8). `telemetry.StorageBytes` is recorded the same way. `workers.Config.Metrics` is optional and defaults to `telemetry.Noop`.
- **Why.** `video.cut` is not marked paid and yet may download a large master. Counting in one place covers every node, including the AI nodes that read through `OpenInput`.
- **Revisit if.** Pricing needs per-role or per-class records: the wrapper already knows both.

### I196. Worker configuration

- **Choice.** `vidyom-worker --config <config-dir>/daemon.json --pools media` reads `cacheDir`, `scratchDir`, `spoolDir`, `ingestRoots`, `redis`, `aiWorker`, `retention` and the Unix listener path from the same file the daemon reads. Flags override each field. `--daemon` takes `unix:///path` or `https://host:port`, and HTTPS takes `--tls-fingerprint sha256:<hex>` (I18's pin) or `--tls-ca-file`, TLS 1.3 minimum, no plaintext. The worker token comes only from `--token-file` or `VIDYOM_WORKER_TOKEN_FILE`, never from a flag value or the environment (§9.1). FFmpeg and ffprobe default to `PATH`, overridable with `--ffmpeg` and `--ffprobe`. Resources are detected and overridable; `--max-concurrency` defaults to CPU cores divided by 4, at least 1.
- **Why.** I34 keeps tenant configuration on the daemon, but directories and the Redis URL are host facts shared with the daemon. One file avoids disagreement between them.
- **Revisit if.** Workers run away from the daemon's configuration directory.

### I197. The AI pool's environment

- **Choice.** Pool `ai` runs through the same loop with `node.AIEnv`. Drivers default to `faster-whisper` and `bge-m3`, overridable with `--transcriber` and `--embedder`, built with `connector.AIParams{Tenant, AI, Daemon}`. `AI` comes from the additive optional `Lease.AI`; when the daemon does not fill it, the connector's defaults apply, which is the known limit of this version for tenants that override `transcriptionModel` or `embeddingModel`.
- **Why.** `Connection` covers storage roles only. A field on the lease is the smallest way to give a task its tenant's model names, and those are not secrets.
- **Revisit if.** The AI pool needs credentials: it would then take a route like `Connection`.

---

## Media

### I184. Profile selection and the resolution ceiling use the short side

- **Choice.** "Effective height" is the shorter side of the display dimensions, after rotation. It selects the profile and is what the ceiling limits. `auto:master`: up to 1439 selects `master-1080p`, from 1440 selects `master-2160p-hevc`. Output dimensions are computed in Go, scaled only downward, and rounded down to even.
- **Why.** D14 names "up to 1080p" and "2160p or more" and leaves the range between open. `schemas/testdata/profile-master-1080p.json` already draws the line at 1439. A portrait 1080×1920 phone recording is 1080p by any reasonable reading; capping its height at 1080 would turn it into 608×1080, against R17's intent and D14's reframing goal. A 1440p source becomes HEVC at 1440p, since profiles never upscale.
- **Revisit if.** The maintainer wants 1440p kept in H.264.

### I185. Scratch files carry their provenance in a sidecar

- **Choice.** `storage.get`, `video.transcode` and `video.cut` write `<file>.origin.json` with `role`, `key`, `sha256`, `profile` and `family` (`master`, `proxy`). `OpenInput` on a `scratch://` reference returns `LocalInput.Role` and `SHA256` from it and, when a role is recorded, recomputes the checksum. `video.cut` accepts only role `master` with a matching checksum. `audio.extract` accepts role `master` or family `master`. A re-encode of anything derived from a proxy stays family `proxy`, whatever its target profile. `storage.put` copies `profile` into its output. Only scratch has sidecars: one beside a `server:` or `upload:` file is ignored. Nothing a client sends can create one in scratch.
- **Why.** §6.4 requires "the worker's local cache stores the role and the origin checksum, and `video.cut` checks both", and D56 requires knowing that an audio source is the master, not a proxy. A scratch URI alone carries neither.
- **Revisit if.** Scratch moves to object storage (I5's Future case): the same fields become object metadata.

### I190. FFmpeg invocation

- **Choice.** `exec.CommandContext` with an argv array, never a shell (§6.5). Minimum FFmpeg 5.1, for `-fps_mode`, checked at startup; an unparsable version string, such as a git build, only warns. Every input gets `-protocol_whitelist file` and a demuxer `-format_whitelist` covering MOV/MP4, Matroska/WebM, AVI, MXF, MPEG-TS, MPEG-PS, ASF, FLV, Ogg, WAV, FLAC, MP3, AAC, DV. Output drops global metadata and chapters (`-map_metadata -1`, `-map_chapters -1`), and takes the first video and first audio stream. On cancel, `SIGTERM`, then kill after 5 s. FFmpeg's stderr stays in internal logs, bounded, and never reaches a client message.
- **Why.** An uploaded file named `.mp4` can really be an HLS playlist or a concat script, the known way to make FFmpeg read local files or reach the network: a real risk with tenants' uploads in a shared worker. Camera metadata can carry GPS and device data that must not travel into proxies or published clips; the raw file in `archive` keeps it.
- **Revisit if.** A legitimate recording format is refused: extend the list.

### I191. Timeline rules

- **Choice.**
  - **Tolerance:** max(100 ms, 1 frame of the reference), 100 ms for a reference without video (D21).
  - **Duration:** the container duration, or a packet scan of the longer of video and audio when it is missing or the timestamps are broken. An output keeps the longer stream, so a video-only figure would reject a healthy file.
  - **VFR:** more than 0.5% of timestamp deltas (at least 2) deviate from the median by more than 10% and 1.5 ms. The floor sits above Matroska's millisecond rounding.
  - **Rate of a stream that declares none:** measured from its timestamps. `r_frame_rate` is not used, because it names the smallest tick rather than the frames held.
  - **Broken timestamps:** a packet with neither PTS nor DTS, non-monotonic DTS, or a gap larger than 5 s and 100 medians. A stream missing only some PTS is measured by its DTS, since B-frames in AVI and MPEG-PS normally have none.
  - **Normalization:** `video.transcode` always writes CFR with an explicit rate. A CFR source keeps its exact rational rate. A VFR or broken source takes the nearest standard rate (23.976, 24, 25, 29.97, 30, 50, 59.94, 60) within 1.5% of its average, else the next one above it, else its rounded average when above 60, capped at 120. Broken inputs also get `-fflags +genpts+igndts`. Audio goes through `aresample=async=1:first_pts=0`.
  - **Rejection:** `video.validate_timeline` fails with permanent `validation_failed` above tolerance, with the durations in the message, which is what reaches the job's record, and reports `valid: true` otherwise. `video.transcode`, `video.cut` and `audio.extract` also check their own output, so a bad file never reaches a later node.
- **Why.** D21 sets the tolerance and the CFR rule, not how to detect VFR nor which rate to pick. R10 says "rejects". The catalog's `valid` output stays true on success so a workflow can read it. Thresholds are set against generated fixtures. **Validate:** on real camera files.
- **Revisit if.** Real recordings are misclassified.

### I192. Clip and preview rendering

- **Choice.**
  - `video.cut` always re-encodes, for frame accuracy, with per-segment input seeking and a concat filter. It uses the source's master profile by `auto:master` unless `profile` is given, never upscaled. Up to 64 segments. A segment beyond the video stream's end by more than the tolerance is `invalid_argument`, within it is clamped: where audio outlasts video, a clip needs video. Output duration must match the segment sum within max(100 ms, one frame per segment).
  - `video.cut` requires `DirectAccess` on the `master` instance (§7.2), answering `capability_unsupported` otherwise, and role `master` plus a checksum on the resolved input.
  - `video.preview` uses fixed parameters, not a profile: H.264 `veryfast`, CRF 30, at most 480p, AAC 64 kbps. It reads `proxy` or scratch, and refuses `master` and `archive`, because §3.2 says from the proxy and a preview must not pull the master from storage.
  - `video.thumbnail` writes JPEG at most 1280 wide, never upscaled. `atSeconds` defaults to a tenth of the duration and is clamped into it.
- **Why.** Stream copy cuts only at keyframes, and §3.2 approves exact timestamps. Previews are disposable, so they are not versioned artifacts and need no profile. Clamping a thumbnail time is harmless for a short clip, while clamping a clip's segment would silently change what was approved.
- **Revisit if.** Previews should match a published look.

### I193. ASR audio extraction

- **Choice.** `audio.extract` defaults to `speech-opus`. Sample rate is the largest Opus rate not above both the profile's and the source's. Bitrate is capped at the source's when known. Channels are downmixed to mono. A track shorter than the master is padded with silence so the file has the master's timeline. A source with no audio stream fails with permanent `failed_precondition`.
- **Why.** D40 says never upscaled, D56 says exactly the master's timeline. A silent recording cannot produce the transcript that readiness requires (R11), so an explicit failure is more honest than an empty transcript.
- **Revisit if.** Silent recordings must be ingested without a transcript, which would need a readiness change.

---

## Cache

### I189. Cache format and defaults

- **Choice.** `<cache-dir>/<tenant>/<entry-id>.vcache`. Entry ID: HMAC-SHA-256 under the tenant key of role, key and origin SHA-256. Format: magic and version, random 32-byte salt, sealed header (role, key, origin SHA-256), then 1 MiB chunks in AES-256-GCM under a key from HKDF-SHA-256 (tenant key, salt, `"vidyom-cache-file-v1"`). The nonce holds the chunk index and a final flag, and the AAD binds the magic and salt, so a truncated, reordered or foreign file fails. Last use is the file's modification time, touched on every reuse, so sweeps need no key. Defaults: 7 idle days (`cacheIdleDays`, D22), 50 GiB per tenant (`cacheMaxBytes`). A failed checksum or authentication on reuse deletes the entry and downloads again.
- **Why.** I39 fixes AES-256-GCM chunks and gives each lease only its tenant's key. Plaintext must exist for FFmpeg, which is why reuse writes a task-scoped copy (I181) and hashes it while decrypting: "checksum on every reuse" (D22) then costs no extra read. D22 sets the idle period and leaves the size to configuration; 50 GiB holds a few dozen long masters.
- **Limit.** The scratch volume holds plaintext while a task or job runs. Encrypting that volume is a deployment choice.
- **Revisit if.** Tenants need different maximums: `CacheMaxBytes` would become per-tenant configuration.

---

## Storage

### I186. Checksums on GCS without paying for reads

- **Choice.** GCS has no SHA-256. On write the driver computes SHA-256 and CRC32C over the stream. A mismatch with `PutOptions.SHA256` cancels the upload before commit, so no object appears. After commit it compares its CRC32C with the server's and deletes the object on a difference. Both values are stored as object metadata (`vidyom-sha256`, `vidyom-crc32c`). A failure to record them deletes the new generation, so a failed write leaves no object it could not verify. A copy names the source's metadata explicitly, since a rewrite with a non-empty body may not carry it over on real GCS. `Verify` checks the wanted SHA-256 against the recorded one and the recorded CRC32C against the server's current CRC32C, with no read. An object with no recorded metadata is streamed and hashed. `storage.put` with `verifyChecksum` relies on the driver's write-time check plus a `Stat`, so it never reads the role it just wrote, on any driver.
- **Why.** §7.2 says not to use Archive for operational reads, and R29 warns about retrieval costs, yet §3.1 requires a verified backup. The server's CRC32C describes the bytes it stored, and the local CRC32C was computed over the same stream as the SHA-256. A read-back of a 10 GB raw file would cost real money on every ingest for little more assurance.
- **Revisit if.** An audit requires a periodic deep read: `verify-storage` could stream on request. **Validate:** on real GCS (I12), `fake-gcs-server` only imitates it.

### I187. What "full capability set" means

- **Choice.** Capabilities are declared honestly per driver.

  | Capability | `local` | `gcs` |
  |---|---|---|
  | `versioning` | yes: generations retained, on by default, `parameters.versioning: false` turns it off | declared by `parameters.versioning`, a bucket setting |
  | `range_reads`, `server_side_copy`, `checksum_verification` | yes | yes |
  | `resumable_upload` | yes: after a failed attempt, a retry with the same idempotency key and a seekable source continues from the verified partial file. A killed process starts over, since each attempt owns its file and only a failing one parks it | yes: chunked upload within one `Put`; a retried node uploads again |
  | `storage_classes` | no: recorded in metadata, no physical effect | yes |
  | `restore_semantics` | no: nothing needs restoring | no: `ARCHIVE` reads immediately |
  | `direct_access` | from `usage` | from `usage` |

  `direct_access` is true only for `usage: operational`. An undeclared `usage` is false, so `video.cut` fails closed with a message naming the field. `local` also takes `parameters.disableCapabilities` to prove §15.4's "declares the limitation", making the matching operation answer `capability_unsupported`.
- **Why.** The master plan asks for a "full capability set", and I9 explains it as range reads, server-side copy and checksums. Declaring storage classes or restore on one disk would be a claim with nothing behind it (§7.2: unavailable operations must be declared as such). Every operation of §7.2 is implemented on both.
- **Revisit if.** A workflow needs a storage class to behave differently on `local`.

### I188. Local access log classes

- **Choice.** `parameters.accessLog` names a JSONL file. Each line has `at`, `tenant`, `role`, `op`, `class`, `key`, `bytes`. Classes: `read` (`get`, `range`), `verify` (`checksum`, `verify`), `write` (`put`, `copy`, `move`, `delete`) and `metadata` (`stat`, `exists`, `list`). A write-time verification counts as `write`. Appends are single `O_APPEND` writes, so several workers can share a file. "No archive read" (§1.1 step 6) means no line with `role: "archive"` and `class: "read"`. A failure to write the log fails the operation.
- **Why.** Clipping does write to `archive`, because `ingest-derived` backs up the clip master (D16). The proof has to separate reading media from writing a backup. A proof that can silently stop logging proves nothing.
- **Revisit if.** Another driver needs the same proof.

### I198. A `local` proxy link is a `file://` URL

- **Choice.** `local` implements `connector.LinkProvider` by returning `file://<absolute path>`, with `ExpiresAt` only nominal. It works only for a client on the same host, and access control is the file system. `gcs` signs a V4 URL with the service account's key, and answers `capability_unsupported` for credentials without one.
- **Why.** D24's review needs proxy links, and `local` is the only driver the demo uses. No server fronts local files, and adding one for a dev driver is not worth it. The limit is stated rather than disguised as a real signed link.
- **Revisit if.** Remote review on `local` is needed: the daemon could serve signed ranges.

### I199. GCS credentials and how the driver is tested

- **Choice.** `credentialsRef` is required, the value is the service account JSON resolved at open, and it stays in the client's memory. There is no fallback to ambient application-default credentials. `parameters.endpoint` points at an emulator and is the only way to run without credentials. Tests run one conformance suite against `fake-gcs-server` twice: in a container (`fsouza/fake-gcs-server`, name `vidyom-dev-media-*`, non-default port, removed at the end) when Docker answers, and in-process through its `fakestorage` package otherwise and always. Real GCS is never contacted (I12).
- **Why.** Ambient credentials would be a silent fallback, picking up whatever the host has (§15.6's spirit). A container proves the driver over real HTTP against the server's released image, and the in-process run keeps a plain `go test` meaningful with no Docker.
- **Outages.** Calls that move no media have a 2-minute deadline and 4 attempts (`parameters.retryAttempts`). Transfers can last hours, so they get a stall timeout instead: 5 minutes with no progress (`parameters.stallTimeoutSeconds`). Otherwise an outage would hang a node while its heartbeat kept it looking alive, and it would never reach `node.Transient`.
- **Revisit if.** Workload identity is wanted: an explicit `parameters.ambientCredentials` could allow it.

---

## Disagreements found

Pointed out per `AGENTS.md`, implementation as stated.

| # | Where | Disagreement | This implementation |
|---|---|---|---|
| 1 | §14.2 vs I9 | `connectors/storage/local` marked `# future` | I9 |
| 2 | §14.2 vs I25 | `profiles/` lists three files, `speech-opus` is absent | Four profiles (I25) |
| 3 | §4.1 vs §6.3 | §4.1 says the audio comes from the master; the §6.3 example passes `compress` output straight to `ai.transcribe` (R26 already notes the missing `audio.extract`) | `audio.extract` accepts the scratch master through its sidecar (I185) |
| 4 | `node.Scratch.Resolve` comment vs §6.4 | "Refusing other jobs' files" vs a child job reading its parent's render | Explicit list on the lease (I182) |
| 5 | `website/examples/profiles/` vs `schemas/media-profile.v1.json` | The website's examples have no `version` and `speech-opus` no `container`, which the schema requires | The schema |

## Open for the maintainer

- **`ingestRoots` are daemon-wide.** Any tenant with permission to run a workflow can read any `server:` path inside them. §5.4 restricts server paths to input directories and says nothing about tenants, while D6 isolates tenants everywhere else. This version follows the text as written: a multi-tenant host should keep its ingest roots to files all its tenants may see, and per-tenant roots would need a configuration change in `api` and `media`.
