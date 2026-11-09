# Plan: component `api`

Owner of `cmd/vidyomd/**` (except `cmd/vidyomd/wire.go`, which belongs to `integration`), `internal/daemon/**` and `internal/api/**` (package `apiserver`). Normative inputs: `spec/vidyom-specification-v1.2.md` (§5, §9.2, §9.3, §11.1, §12.1), `docs/plans/00-master-plan.md` (sections 3, 5–10), `docs/decisions.md`, `internal/contract/**`.

Status vocabulary: everything below is a **target** until its commit lands. Decisions taken while reviewing this plan are in [`docs/decisions/api.md`](../decisions/api.md) as `I120–I139`.

## 1. Deliverables

| Package | Entry point (master plan §9) | Provides |
|---|---|---|
| `internal/api` (`package apiserver`) | `apiserver.NewHandler(apiserver.Deps) http.Handler` | Every route of `api.Routes`, the request pipeline, the error model, SSE, R20 enforcement |
| `internal/api/uploads` | `uploads.New(uploads.Options)` | `service.Uploads` plus the chunk protocol (I20), spool management, expiry sweep |
| `internal/daemon` | `daemon.Run(ctx, config.Daemon, daemon.Deps) error` | Configuration loading and validation, fail-closed listeners, TLS, peer identity, data directory lock, lifecycle, `service.Doctor`, `vidyomd init` logic |
| `cmd/vidyomd` | `vidyomd init`, `vidyomd start [--detach]`, `vidyomd version`; the `wireDeps` seam | The binary |

The seam stays exactly as section 9 of the master plan declares it:

```go
var wireDeps func(ctx context.Context, cfg config.Daemon) (daemon.Deps, func(), error)
```

While it is nil, `init` and `start` exit non-zero with a clear "not wired" error and open no listener.

## 2. Request pipeline

Every route of `api.Routes` goes through one chain. Order matters because it decides what an unauthenticated or foreign caller can learn.

1. **Envelope.** New ULID request ID in `Vidyom-Request-Id`, `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, panic recovery into the error body, access log (route name, status, duration, tenant, actor, device; never the query, body or `Authorization`).
2. **Trace.** Extract `traceparent` with the OpenTelemetry propagator, start a span named after the route, set `Scope.TraceID`, and return `traceparent`. With no tracer provider configured, a trace ID is still generated so jobs carry one.
3. **Authentication** by `Route.Auth`. `none`: skip. `worker`: `Authenticator.AuthenticateWorker`. `user`: `Authenticator.Authenticate` with the bearer token, the Unix peer UID and the remote address. A failure answers `401` and counts in `vidyom_auth_failures_total`. Requests with no credential at all are refused without calling the authenticator.
4. **Tenant** by `Route.Tenant` (D32). `required` without the header answers `400 tenant_required`; a malformed slug answers `400 invalid_argument`; `none` ignores the header. `Authorizer.Resolve` turns no-membership into `404`, so a foreign tenant is indistinguishable from a missing one (§12.1).
5. **Permission.** `Authorizer.Require` with `Route.Permission` when set. A denial answers `403`, counts in `vidyom_authz_denials_total`, and on a mutating route writes an audit record with result `denied`.
6. **Idempotency.** On tenant-scoped mutations with a key: same key and same request replays the stored response, same key with a different request answers `409 idempotency_mismatch`, a concurrent duplicate waits for the first. Only `2xx` responses are remembered.
7. **Audit.** On mutations, one record with result `allowed` is written **before** the handler runs; a failure aborts the request, so no mutation can happen unaudited. A non-`2xx` outcome adds a record with result `failed`.
8. **Handler** with a `service.Scope`. Coded errors become `{"error": {...}}` through `errs.Public`; uncoded errors become a generic `internal` and are logged with the request ID.

A missing path or method answers the JSON error body with `404` or `405`.

## 3. Route handlers

One registration per `api.Routes` name, grouped by service. A nil service answers `501 not_implemented` naming the missing service, so partial wiring is visible rather than a crash. A test fails when a route in the table has no handler.

| Group | Service | Notes |
|---|---|---|
| Health, version | Internal | `/health`: `status`, `version`, `sealing`, component status without detail; `503` when a critical component is down. Checks are cached for 2 s because the route is unauthenticated |
| Metrics | `Observability.MetricsHandler` | System admin sees everything; a tenant admin sees only samples of their tenant |
| Doctor | `service.Doctor` | Implemented in `internal/daemon` |
| Workflows, nodes run, clips | `Workflows` | `202` with the job; `200` for a dry run. Inputs pass R20 enforcement first |
| Jobs | `Jobs` | Reads never change state; `Subscribe` over SSE |
| Uploads | `internal/api/uploads` | Section 5 |
| Assets, collections | `Assets` | `assets.delete` runs `delete-asset`; `assets.export` runs `catalog.export_package`; `assets.proxy_link` calls `Storages` |
| Search | `Search` | |
| Approvals | `Approvals` | The service applies D7 |
| Publications | `Publications` | `202` where a workflow starts |
| Introspection | `Introspection`, `Connectors`, `Plugins` | |
| Events, logs | `Events`, `Observability` | SSE for the two streams |
| Storage | `Storages`, `Workflows` | `storages.restore` runs `restore-archived` and needs `confirm`; `storages.copy` is backed by a job |
| Secrets | `Secrets` | Metadata only in responses; `location: client` refused |
| Devices, pairing | `Devices` | `/clients` is an alias of `/devices` (I38). `pairing_codes.create` adds the TLS fingerprint. `devices.pair` is rate limited |
| Tenants, users, budget, audit, observability | `Tenants`, `Users`, `Budgets`, `Audit`, `Observability` | |
| Worker | `workerproto.Coordinator` | Tenant comes from the lease. Bodies up to the 32 MiB output cap plus overhead |

Path parameters are validated before any service call: friendly IDs must carry the prefix of the resource (`/jobs/ORI-001` answers `404`), `{role}` must be a known role, other segments must match a safe pattern.

## 4. Server-Sent Events (I3)

`GET /jobs/{id}/events`, `GET /events/stream`, `GET /logs/stream`.

- Frames carry `id:` where a sequence exists; `Last-Event-ID` resumes; a malformed value answers `400`.
- Keep-alive comment every `api.SSEKeepAlive`. At each keep-alive the caller is re-authenticated and re-authorized, so a revoked device loses its open stream.
- A write deadline per frame drops a stalled client, which then resumes with `Last-Event-ID`.
- A closed service channel with a live request sends `event: end` and returns.
- A disconnect only cancels the request context of the subscription. No path from a stream reaches a job mutation; a test asserts it.
- Daemon shutdown closes streams, so `Shutdown` is not held up by them.

## 5. Uploads (I20)

`internal/api/uploads` implements `service.Uploads` and `AppendChunk`.

- **Create.** Validates a base-name filename, the size and the SHA-256; allocates `UPL-nnn` and the record in one transaction; creates `<spool-dir>/<tenant>/<upload-ulid>` with restricted modes. The chunk size is clamped and returned on the record.
- **Chunk.** Per-upload lock. `Upload-Offset` must equal the durable offset, otherwise `409` carrying the current offset. Bytes are written, checked against `Upload-Checksum`, `fsync`ed, then the offset is committed. A failed or short chunk truncates back, so a chunk is all-or-nothing. On a crash the record is the truth: longer files are truncated on the next request.
- **Complete.** Requires all bytes, hashes the whole file against the declared SHA-256 and marks `complete`, or marks `failed` on mismatch. Repeating `complete` on a complete upload returns it unchanged.
- **Abort.** Removes the file and the record. The friendly ID stays burned.
- **Expiry.** `Sweep` removes expired uploads across tenants; it runs on a timer in the daemon.

Tenant isolation comes from the per-tenant repository plus the validated slug in the path.

## 6. Input references (R20)

`POST /workflows/{id}/runs`, `POST /workflows/runs`, `POST /nodes/{type}/runs`:

- Declared `file` inputs are parsed with `domain.ParseUserInputURI`. `server:` and bare absolute paths go through `domain.ResolveServerPath(ingestRoots, path)` and are rewritten in canonical form with the resolved path. `upload:` must name a complete upload of this tenant. `scratch://` is refused.
- All other input values are scanned recursively for `server:` and `scratch://` strings under the same rules, so a reference cannot enter through an untyped input.
- Inline workflow definitions get the same scan over node parameters and input defaults.
- With no `ingestRoots` configured, every `server:` reference is refused.

## 7. Listeners (§9.3, I18, I19, D49)

`daemon.Run` prepares and binds everything before serving anything; any failure aborts with no listener left.

- **Before any bind:** configuration valid, required dependencies present, data directory locked, master key unsealed through `security.MasterKeyProvider`. Sealed or absent means exit non-zero with no listener (D13).
- **Unix socket.** Default mode `0600`, parent directory `0700`. A parent writable by others, a non-socket at the path, or a live daemon at the path aborts startup. A stale socket is replaced. Peer UID comes from `SO_PEERCRED` on Linux and `LOCAL_PEERCRED` on macOS, attached per connection. On platforms without peer credentials the socket listener is refused.
- **HTTPS.** TLS 1.3 minimum, no plaintext path. Exactly one of `bind` (an IP literal) or `interface`. A missing or down interface aborts. `0.0.0.0` and `::` need `allowPublic` and `mode: files`. `mode: auto` only on loopback, with a self-signed certificate kept in the data directory and its fingerprint reported through pairing. `clientCAFile` turns on mTLS.
- **Docker Desktop (D49).** Inside a container, loopback cannot receive a port published by Docker, so a dedicated rule is needed for HTTPS on host loopback; decided in the decisions log.
- **Runtime.** Any `Serve` failure stops the daemon with an error rather than running on the remaining listeners.

## 8. Daemon lifecycle

`daemon.Deps` carries `apiserver.Deps`, the master key provider, background services, an optional queue for health, reload and bootstrap hooks.

Order: validate → lock → unseal → preflight hooks → bind listeners → serve → background services and maintenance timers → ready signal. `SIGHUP` calls the reload hook (I34). Shutdown: close streams, `http.Server.Shutdown` with a timeout, stop background services, run cleanup, release the lock. Boot writes the sealing level to the audit (I10).

## 9. `vidyomd` commands

- `vidyomd init --tenant T --admin E [--config-dir D] [--worker-token-file F] [--generate-master-key P] [--json]`: per I10 and I17. Refuses an initialized data directory. Prints one pairing code once.
- `vidyomd start [--config-dir D] [--detach]`. `--detach` re-executes itself in a new session with logs redirected and waits for a readiness pipe, so a daemon that failed closed makes the parent exit non-zero with the reason. A pid file records the running daemon.
- `vidyomd version`.

Configuration: `<config-dir>/daemon.json` (I34), strictly decoded so a misspelled key fails closed. `--config-dir`, then `VIDYOM_CONFIG_DIR`, are required; there is no implicit location.

## 10. `service.Doctor`

Implemented in `internal/daemon`, because it asks both the listeners and the services.

- No filter: reachability, version, sealing level, data directories writable, queue reachable.
- `security`: sealing level (`warn` for `file` and `dev`), socket and data directory modes, TLS mode and certificate expiry, mTLS.
- `network`: each listener and whether it is public, interface state, Redis, AI worker.
- `storage`: each role through `Storages.List` and `Test` for the header's tenant, free space on the local volumes.

## 11. Test strategy

- **Handlers against fake services.** A table runs over `api.Routes`: every route is served; each `AuthUser` route refuses no credential; each required-tenant route refuses a missing header; a foreign tenant gets `404`; a missing permission gets `403`; arguments reach the fake decoded. Error model, request ID, idempotency, audit order, input enforcement and SSE have their own tests.
- **Uploads against a fake `repo.TenantStores`,** with real files under `t.TempDir`: resume, wrong offset, bad checksum, crash recovery, concurrent chunks, isolation.
- **Listeners with real sockets and real TLS:** socket mode and parent checks, stale socket, peer UID, TLS 1.2 refused, loopback auto certificate pinned, missing interface aborts with nothing listening, public bind refused, sealed provider leaves nothing listening.
- **`cmd/vidyomd`:** the built binary without wiring exits non-zero; with test wiring through an in-package seam, `start` serves over the socket and exits cleanly on `SIGTERM`.
- `go test -race ./cmd/vidyomd/... ./internal/daemon/... ./internal/api/...`, `go vet`, `gofmt`.

Real services are not imported. Replacing the fakes is `integration`'s work in `wire.go`.

## 12. Phases

1. **Plan and review** (this file, then `docs/decisions/api.md`).
2. **`apiserver` core:** deps, envelope, errors, decoding, authentication, tenant, permission; registration from the route table; health and version.
3. **Pipeline completion:** idempotency, audit, tracing, metrics.
4. **Handlers:** every group of section 3.
5. **SSE.**
6. **R20 enforcement.**
7. **Uploads package.**
8. **Daemon:** configuration, listeners, TLS, peer identity, lock, lifecycle, doctor.
9. **`cmd/vidyomd`:** seam, `init`, `start`, `--detach`.
10. **Hardening:** rate limit on pairing, reconcile with what `core`, `engine`, `cli` and `media` landed, `./...` before the final report.

## 13. Dependencies on other components

- `core`: `security.Authenticator`, `Authorizer`, `Auditor`, `repo.TenantStores`, bootstrap for `init`, master key provider format. Faked in tests.
- `engine`: `service.Workflows`, `Jobs`, `Approvals`, `Events`, `Introspection`, `workerproto.Coordinator`, workflow input names for `delete-asset` and `restore-archived`.
- `media`: `service.Storages`; the spool path convention of I20 for reading `upload:` inputs.
- `cli`: the upload protocol and SSE as its plan describes them.
- `integration`: `wire.go`, Compose publishing rule for D49.

## 14. Definition of done

Master plan §10 `api`, repeated as a checklist:

- [ ] Every route in `api.Routes` is served, worker routes included.
- [ ] `Vidyom-Tenant` rule, idempotency keys, permission checks from the route table, cross-tenant `404`, error model.
- [ ] Unix socket with restricted mode and peer identity; HTTPS TLS 1.3; HTTPS on loopback for Docker Desktop.
- [ ] No listener when sealed or when the configured interface is missing; never a `0.0.0.0` fallback.
- [ ] SSE with `Last-Event-ID` resume; disconnecting never affects a job.
- [ ] Resumable uploads with checksum; `server:` enforcement against `ingestRoots`.
- [ ] `vidyomd init`, `vidyomd start`, `service.Uploads`, `service.Doctor`.
- [ ] Handler tests with fake services; own paths build, `go vet` clean, tests pass with `-race`.
