# Implementation decisions: `api`

Decisions of the `api` component, numbered from its range `I120–I139` (master plan, section 8). Same format as [`docs/decisions.md`](../decisions.md): the choice, the reason and what would make us revisit it. None of them changes a maintainer decision `Dn`. What this log says the daemon does is a **target** until the component's commits land; the final report states what is done and what is only tested against fakes.

## Request pipeline

### I120. Pipeline order, and how each refusal reads

- **Choice.** Fixed order: request ID and trace, authentication, tenant, permission, idempotency, audit, handler. So: no credential answers `401` whatever else is wrong; a missing `Vidyom-Tenant` answers `400 tenant_required` only to an authenticated caller; a tenant the caller does not belong to answers `404` from `Authorizer.Resolve`, the same as a tenant that does not exist; a missing permission answers `403`. A tenant header on a `TenantNone` route is ignored. A malformed slug answers `400 invalid_argument`, because a malformed value reveals nothing about existence.
- **Why.** D32 and §12.1 fix the outcomes but not their precedence. An unauthenticated caller must learn nothing about tenants or routes, and a foreign member must not be able to tell a foreign tenant from a missing one.
- **Revisit if.** Clients need `tenant_required` before authentication.

### I121. `TenantOptional` routes

- **Choice.** The routes with an optional tenant are `metrics`, `doctor`, `pairing_codes.create`, and `devices.*` and `clients.*`. With the header, the principal is resolved in that tenant (`404` on no membership) and `Route.Permission`, when set, must hold there. Without it, the principal stays unresolved: when `Route.Permission` is set only a system admin passes, otherwise `403` naming the permission; when it is empty the service decides. `system.*` permissions always go through `Authorizer.Require`. One exception: `devices.revoke` and `clients.revoke` are left entirely to `service.Devices`, because anyone may revoke their own device (I112), which the table's `device.manage` would forbid.
- **Why.** `Principal.Can` says a system admin only passes system-level permissions, since tenant access needs a membership, but D12's pairing and device management are "tenant and pairing management" that §12.1 exempts from the header. A system admin with no membership must be able to pair the first users. `service.Devices` applies the per-resource scope (own device, or admin), and a leaving employee revoking their own laptop should not need an admin.
- **Revisit if.** Device management becomes tenant-bound.

### I122. Idempotency keys are required where they can be honored

- **Choice.** Every tenant-scoped mutation with `Auth: user` requires `Idempotency-Key` and answers `400 invalid_argument` without it. Exceptions, where the header is ignored: `uploads.chunk` (idempotent by offset, and its body is not hashed), `workflows.validate` (reads only), `secrets.put` and `secrets.rotate` (idempotent by name, and hashing a request that holds a secret value would store a dictionary-attackable digest of it), and all tenant-free and tenant-optional routes, which have no tenant store. Worker routes are idempotent on task and lease (I4). For `workflows.run` and `workflows.run_inline`, the body's `idempotency_key` counts, and a different value in the header answers `400` (I15). The key is 1 to 255 printable ASCII bytes. It is stored namespaced by user. The request hash is SHA-256 of method, path and body. A repeat with the same hash replays the stored status and body with `Idempotency-Replayed: true`; a different hash answers `409 idempotency_mismatch`; a concurrent duplicate waits for the first through an in-process lock, which is enough because one daemon owns a data directory. Only `2xx` responses are remembered, and at most 1 MiB. Retention belongs to `core`'s daily maintenance.
- **Why.** §12.1 says keys are mandatory for mutations; `cli` already sends one on every mutating request (I170). A required key turns a script's accidental double submission into a clear error rather than a second job. Remembering failures would make a corrected retry collide with its own key. Responses that hold a credential, such as pairing codes, must never be written to a database in clear, and those routes have no tenant store anyway.
- **Revisit if.** Tenant-free mutations need replay, which would need a store in `system.db`.

### I123. The API layer writes an audit record for every mutation, before the handler

- **Choice.** `Action` is the route name, `Permission` the route's permission, `Resource` the path parameter, `Result` `allowed`. A failure to write it aborts the request with the error, so no mutation can happen unaudited (§10.1). A non-`2xx` outcome adds a second record with `failed` and the error code; a failure writing that one is logged. A denied mutation writes `denied`. A denied read only counts in the metric. No body, query or secret is ever recorded. Tenant-free mutations are recorded with an empty tenant, which `core`'s system chain receives. Not recorded when allowed: each `uploads.chunk` (one transfer would write thousands of rows; `create`, `complete` and `abort` are recorded), `workflows.validate`, `devices.pair` (no identity exists yet, an unauthenticated caller must not be able to grow the chain, and `service.Devices` records real pairings), idempotent replays and worker routes (the engine records what they change). Services keep writing their own domain records (`service` package comment), so a mutation normally has both: the API's "who called what with which permission" and the service's "what changed".
- **Why.** §12.1 makes the audit mandatory for mutations and D48 asks which permission authorized each action. Only the API layer can guarantee coverage of every route, and only a write before the action makes it fail closed.
- **Revisit if.** Pairs of records prove noisy in `security audit`; the `api` records could then be filtered by default.

### I124. Request and trace identifiers

- **Choice.** `Vidyom-Request-Id` is always a server-generated ULID; a client's value is ignored, so nothing client-controlled reaches the audit under that name. `traceparent` is extracted with the OpenTelemetry propagator and returned on the response. With no valid `traceparent` and no tracer provider, a random trace ID is generated, so `Scope.TraceID` is never empty.
- **Why.** I15 and §10.1. Correlation across systems is what `traceparent` is for.
- **Revisit if.** Operators need client-supplied request IDs.

### I125. Bodies and decoding

- **Choice.** JSON bodies are decoded strictly: unknown fields and trailing data answer `400`, so a misspelled `start_seconds` on an approval cannot silently approve something else. Limits: 8 MiB by default, 48 MiB for `worker.tasks.complete` (the 32 MiB output cap of I35 plus envelope), 16 KiB for `devices.pair`. Over the limit answers the new `413 payload_too_large`. List responses always wrap in `{items, next_cursor}` with a non-null array. Results with no natural body answer `200 {}` rather than `204`, so a client that always decodes JSON keeps working. Status codes: `201` for created records, `202` when a job starts, `200` otherwise.
- **Why.** I15 fixes the list shape, not the rest. The new code is additive; `output_too_large` already means something else.
- **Revisit if.** Forward compatibility requires tolerating unknown fields.

### I126. Path parameters

- **Choice.** `{id}` on assets, jobs, approvals, publications, uploads, devices and clients must be a ULID or a friendly ID with the resource's prefix. A valid ID of the wrong resource answers `404`; a malformed one answers `400`. `{role}` must be a known role. Other parameters must match `^[A-Za-z0-9._@:+_-]{1,255}$`.
- **Why.** Repositories resolve per tenant already. This stops confused references early and keeps odd bytes out of logs and the audit.
- **Revisit if.** Workflow or plugin IDs need other characters.

### I127. Pairing rate limit

- **Choice.** `POST /devices/pair`, the only unauthenticated mutation, is limited per remote IP to a burst of 10 and 10 per minute, per process, and answers `429 rate_limited` with `Retry-After`. No other route is rate limited in this version.
- **Why.** A one-time code is short-lived but guessable in principle, and this route is reachable before any identity exists.
- **Revisit if.** A shared-NAT deployment hits the limit.

### I128. Access to doctor and metrics

- **Choice.** `GET /doctor` with no `check` runs everything the caller may see: any authenticated principal gets reachability and version; a system admin or a tenant `admin` also gets the rest. An explicit `check=security|network|storage` by anyone else answers `403`. The `storage` check needs a tenant. `GET /metrics` without a tenant needs a system admin and returns everything; with a tenant it needs `metrics.read` and returns only samples labeled with that `tenant_id`, in Prometheus text format, so a tenant admin never sees another tenant's numbers.
- **Why.** §12.1 says "controlled access" without details. D6 says no operation crosses tenants, and a global exposition would.
- **Revisit if.** Tenants need global metrics such as queue depth.

## Streams

### I129. SSE details

- **Choice.** Besides I3: `Last-Event-ID` must be a non-negative integer, otherwise `400`. Each keep-alive re-runs authentication and authorization, so revoking a device or a membership cuts open streams within 15 s; any failure ends the stream without a body. Each frame has a 30 s write deadline; a stalled client is dropped and resumes. `event: end` is sent only when the service closed the channel on a live request. Daemon shutdown ends streams at once, without `end`, so clients reconnect.
- **Why.** D12's revocation would otherwise stop at the next request, which a stream never makes. Nothing on these paths calls a mutating service method; a test asserts it (§5.5).
- **Revisit if.** Re-authentication cost shows up under many streams.

## Uploads and input references

### I130. Upload details beyond I20

- **Choice.** `chunk_bytes` is clamped to 1–64 MiB, default `api.DefaultChunkBytes`, and returned on the record as the maximum per `PATCH`. A chunk is all-or-nothing: written at the offset, checked against `Upload-Checksum` when sent (mismatch: `422 validation_failed`, truncated back), `fsync`ed, then the offset is committed in the database. A `PATCH` over the limit answers `413`; beyond the declared size, `409`. The database offset is the truth: a longer file from a crash is truncated on the next request, and a shorter file lowers the offset. Wrong offset: `409 conflict` with `details.offset` and the `Upload-Offset` header. Expired: `410 gone`. `complete` re-hashes the whole file with the context honored; mismatch sets `failed` and answers `422`; a repeat on a complete upload returns it. Records expire 7 days after the last activity, and a sweep at start and every hour removes them and their files, including a complete upload no job consumed; `catalog.release_input` normally removes those first. `abort` works in any state. Filenames are metadata only, never part of a path. Spool directories are `0750` and files `0640`, because workers read them; Compose must run the daemon and workers with compatible users.
- **Why.** §5.4 asks for resumable, checkpointed and checksummed transfers. All-or-nothing chunks keep the crash matrix small at the cost of at most one chunk resent.
- **Revisit if.** Parallel chunks or very slow links need partial chunks.

### I131. Where R20 is enforced, and what it rewrites

- **Choice.** At the API, before a job exists: declared `file` inputs go through `domain.ParseUserInputURI`. `server:` and bare absolute paths go through `domain.ResolveServerPath` and are rewritten to `server:<resolved>`. `upload:` must name a complete upload of this tenant and is rewritten to `upload:<ULID>`. `scratch://` is refused. Values of other inputs are scanned recursively: a string that is a `server:` absolute path, an `upload:` reference or a `scratch://` URI gets the same treatment, so a declared `string` feeding a node's `source` cannot bypass it. Inline definitions are scanned the same way across node parameters and input defaults; node parameter `source` on `POST /nodes/{type}/runs` counts as a file reference. With no `ingestRoots`, every `server:` reference is refused. This is the first of two layers: `media`'s `OpenInput` enforces `ingestRoots` again when the file is opened, which closes the window between submission and use.
- **Why.** §15.6 asks for rejection, and a job should not start only to fail later on a forbidden path. Rewriting to the ULID answers the question of how a worker with no database finds `<spool-dir>/<tenant>/<upload-ulid>` (I20): the reference it receives already names the file.
- **Revisit if.** `import:` connectors arrive; they need the same check.

### I132. Routes backed by jobs

- **Choice.** `DELETE /assets/{id}` runs workflow `delete-asset` with input `asset_id`. `POST /assets/{id}/export` runs the node `catalog.export_package` through `Workflows.RunNode`, because no export workflow exists in the engine's list. `POST /storages/restore` is two-phase, as `cli` expects (I171): `confirm: false` answers `200` with `api.StorageRestorePreview` (bytes in `archive`, and an estimate when the tenant's `budget.perUnitMinor` has `storage.archive.retrieval`, per GiB) and starts nothing; `confirm: true` runs `restore-archived`. `POST /storages/copy` needs a known role in both `from` and `to`, refuses `from: archive` (`403 forbidden_storage_role`, only `restore-archived` reads it), and runs an inline workflow with one `storage.copy` node per file of the asset's current version in `from`, under the same key. It does not change the catalog: the file records keep pointing at their original role.
- **Why.** The `service` package comment assigns these two asset routes to the workflow engine; `service.Storages` has no copy or restore. §3.3 requires the cost before confirmation, and D19 and I8 make configured estimates the only source. An inline definition keeps copying a tracked job with no new interface.
- **Revisit if.** `engine` adds an export or copy workflow, or `media` adds a copy method.

## Listeners

### I133. HTTPS on loopback for Docker Desktop (D49)

- **Choice.** New optional field `listeners.https.loopbackPublished`. Inside a container, a process bound to loopback cannot receive a port Docker publishes, so Compose runs `{"interface": "eth0", "port": 8443, "loopbackPublished": true, "tls": {"mode": "auto"}}` and publishes it as `127.0.0.1:8443:8443`. The field allows `mode: auto` on an address that is **private** (RFC 1918 or ULA), never on an unspecified or public one, and it is reported as a `warn` by `doctor --network`, since the daemon cannot see how the port was published. Natively, `bind: 127.0.0.1` with `mode: auto` needs no flag. Certificate SANs cover `localhost`, `127.0.0.1`, `::1` and the listener's addresses; clients pin the fingerprint anyway.
- **Why.** I18 and `config.TLSAuto` say loopback only, and I19 requires a real certificate on `0.0.0.0`, but D49 requires HTTPS on host loopback from inside Docker Desktop. Taken literally those cannot be satisfied at once. This keeps both rules for every other case, and the exposure stays authenticated and pinned even if someone publishes the port wider.
- **Revisit if.** Docker Desktop host networking becomes the supported path.

### I134. HTTPS binding rules beyond I19

- **Choice.** `bind` must be an IP literal; a host name is refused, because it could resolve to anything. `interface` binds every non-link-local unicast address of that interface, each its own socket. A missing interface, one that is down, or one with no usable address aborts startup. All interfaces, certificates and paths are checked before the first bind; then all sockets are bound before the first is served, so a failed listener means nothing was ever served. A `Serve` failure at runtime stops the daemon. `mode: files` certificates are loaded at start, and rotating them needs a restart.
- **Why.** §9.3 and §15.6, I19.
- **Revisit if.** Interfaces need to come and go at runtime.

### I135. Automatic certificate

- **Choice.** ECDSA P-256, valid for 10 years, in `<data-dir>/tls/server.{crt,key}` (`0600`, directory `0700`), reused across restarts so pins survive, and regenerated only when expired or unreadable, which clients see as a pin mismatch. The fingerprint is `sha256:` followed by lowercase hex of the SHA-256 of the leaf's DER, the form both `cli` (I164) and `media`'s worker client accept. `POST /pairing-codes` returns it when a listener uses `mode: auto`; `vidyomd init` prints it.
- **Why.** I18. A pin is only useful if it is stable.
- **Revisit if.** An internal CA replaces it.

### I136. Unix socket rules

- **Choice.** Mode defaults to `0600`; a mode with any `other` bit, or lacking owner read and write, is refused. The parent is created as `0700` when missing; an existing parent that is writable by group or others, or owned by someone else, aborts startup. A symlink or non-socket at the path aborts. A socket that answers a connection aborts; one that refuses is stale and replaced. A path longer than the platform's limit gets its own message. Peer UID comes from `SO_PEERCRED` (Linux) or `LOCAL_PEERCRED` (macOS) per connection; if it cannot be read the connection has no identity. On other platforms a socket listener is refused. When **no** listener is configured at all, the daemon uses a Unix socket at the same default as `cli` (I162): `/run/vidyom/vidyomd.sock` on Linux, `~/.vidyom/vidyomd.sock` on macOS; on Windows it refuses to start.
- **Why.** §5.3, §9.2, I17, I19. A world-accessible socket or a writable parent would make peer identity meaningless.
- **Revisit if.** An operator needs a group-shared directory that is group-writable.

### I137. One daemon per data directory

- **Choice.** `<data-dir>/vidyomd.lock` is held with `flock` for the life of the process, taken before the master key is read and before any listener. A second daemon fails at once. Not enforced on Windows in this version (I44).
- **Why.** §5.1 says only the daemon opens the files, and two daemons would break the in-process idempotency lock and the upload locks, and SQLite's per-database writer gate in `core`.
- **Revisit if.** Windows native becomes supported.

## Commands

### I138. `vidyomd` configuration and bootstrap

- **Choice.** The configuration directory comes from `--config-dir`, then `VIDYOMD_CONFIG_DIR`, with no implicit location. `VIDYOM_CONFIG_DIR` already means the client's directory (I160). `<config-dir>/daemon.json` is decoded strictly, so a misspelled key fails closed. Defaults inside it: `spoolDir`, `scratchDir` and `cacheDir` under `dataDir`. `config.Daemon` gains `ConfigDir` (`json:"-"`) so `wire.go` can reach the tenant files. `redis.url` and `vault.unseal.provider` are required to start. `vidyomd init` sets `config.Daemon.Bootstrapping`, so `wire.go` can leave `core`'s `RequireExisting` off for that call alone (I102), and runs `Deps.Bootstrap`, supplied by `wire.go` over `tenants.Bootstrap`, which refuses a directory that already has tenants or users (I114). The worker token goes to `--worker-token-file`, default `<data-dir>/secrets/worker.token` (`0600`), because Compose mounts it as a secret. That file is reserved before the bootstrap, since the token is returned only once: an existing file refuses the run, and a failed bootstrap releases it. The pairing code is printed once on stdout, with `--json` for scripts. `--generate-master-key <path>` writes 32 random bytes as standard base64 with mode `0400`, the format of `core`'s `file` provider (I111), refuses to overwrite, and without `--tenant` does only that.
- **Why.** I10 and I17 name the commands and leave these details open. The key must exist before `wire.go` can build the vault, so generating it cannot go through the wiring.
- **Revisit if.** An installer flow replaces `init`.

### I139. `vidyomd start --detach`

- **Choice.** Re-executes itself in a new session, with stdout and stderr appended to `<data-dir>/logs/vidyomd.log` (`0600`) and a pipe on an inherited descriptor. The child writes to the pipe only after every listener is bound, then the parent prints the pid and exits `0`. A pipe closed without that makes the parent exit `1` and point to the log, so a daemon that failed closed is never reported as started. `<data-dir>/vidyomd.pid` records the pid while running. On Windows `--detach` is refused in this version (I44); Compose is the path there. Stopping is `SIGTERM`: streams end, in-flight requests get 30 s, the lock is released. `SIGHUP` calls `Deps.Reload` (I34).
- **Why.** §11.1 shows `vidyomd start --detach`. A detached start that cannot report failure would hide exactly the fail-closed cases §15.6 cares about.
- **Revisit if.** Native Windows support is validated.

## Disagreements found, not resolved in the sources

Per `AGENTS.md`. Implementation follows the spec.

| # | Where | Disagreement | This implementation |
|---|---|---|---|
| 1 | D49 vs I18, I19 and `config.TLSAuto` | HTTPS on host loopback from inside Docker Desktop vs automatic certificates on loopback only and a real certificate on any public bind | I133 |
| 2 | I20 vs `api.CreateUploadRequest` | `size` vs `size_bytes` | The contract |
| 3 | `service` package comment vs master plan §10 `engine` | "Exporting starts a workflow", but no export workflow is listed | `catalog.export_package` through `RunNode` (I132) |
| 4 | §11.4 example vs `api.StorageCopyRequest` | `--to another-storage` names an instance; the field is a `StorageRole` | The contract: an unknown role answers `400` (I132) |
| 5 | §12.1 vs website `reference/api.md` | "Mandatory" idempotency keys vs an example that shows them in the body in `camelCase` | Header required on mutations (I122), `idempotency_key` in the body of a run (I15) |
| 6 | `Route.Mutating()` comment vs I122 and I123 | "These take idempotency keys and write an audit record" vs the exceptions named there | The exceptions, each with its reason |
| 7 | `Route.Permission` on `devices.revoke` vs I112 | The table says `device.manage`, checked before the handler; `core` lets anyone revoke their own device | `core`'s rule (I121) |
