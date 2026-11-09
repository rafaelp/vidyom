# Component plan: `cli`

Owner of `cmd/vidyom/**`, `internal/cli/**` and `internal/apiclient/**`. This plan covers every phase of the component. Status vocabulary: **target** until committed. Decisions taken while reviewing it are in [`docs/decisions/cli.md`](../decisions/cli.md) as `I160–I179`.

## 1. Scope

`vidyom` is the static, database-free client of §5.1. It sends commands, uploads, follows logs and state, approves, queries and publishes. It holds no FFmpeg, no Whisper and no database. Definition of done is the `cli` entry of section 10 of the master plan:

- every command of §11 that maps to a route, `--json` everywhere;
- `approval show` on a publication states the tenant's responsibility for image and voice authorizations (D29);
- `secret add` takes the value through a no-echo prompt or stdin, never an argument (R36);
- Unix socket and HTTPS with fingerprint pinning (D12, D49, I18);
- resumable upload with retry and with resume after the client was killed (§5.4, I20);
- input references per R20: `original import <path>` is a client file, `--server-path` a server file;
- pairing with the credential in the OS keyring, failing clearly when there is none (§9.1, §15.6);
- `job attach` and `job logs --follow` over SSE never change a job; Ctrl+C only disconnects (§5.5, §11.5);
- `CGO_ENABLED=0`, no database package: `make check-static` passes.

Wiring entry point: `cli.Execute(ctx, args) int` (master plan, section 9).

## 2. Package layout

```text
cmd/vidyom/main.go               signal handling, cli.Execute, os.Exit
internal/apiclient/
  client.go                      Client, Options, Call, Invoke, Items: every request is built from api.ByName
  endpoint.go                    Endpoint, unix and https transports, TLS 1.3, fingerprint pin, mTLS
  ssh.go                         host-operator tunnel through the system ssh client
  query.go                       query strings from the contract's Query structs, by json tag
  sse.go                         SSE parser and reconnecting Stream with Last-Event-ID
  upload.go                      resumable upload engine and checkpoint store
  apitest/                       fake daemon built from api.Routes, test-only (imported only by tests)
internal/cli/
  cli.go                         Execute, Run, Options, root command, global flags, exit codes
  config.go                      client configuration file (no secrets)
  credentials.go                 keyring accounts, credential lookup
  keyring_os.go, keyring_testfile.go   default keyring; build-tagged file keyring for E2E only
  output.go                      JSON and tables, errors and hints, time and duration parsing
  prompt.go                      no-echo prompt, stdin secrets, confirmations
  cmd_*.go                       one file per command family of §11
```

## 3. Client configuration

`<config-dir>/config.json`, `0600`, directory `0700`, atomic rename on save. Holds servers (kind, URL or socket, pin, tenant, device ID and email) and the current server. **Never a credential**; a test scans the file. Credentials are in the OS keyring only.

## 4. Transport

| Kind | Added with | Dial | Credential |
|---|---|---|---|
| `unix` | `server add NAME --socket PATH`, or the implicit `local` server | Unix socket | Sent when one exists; otherwise peer identity (I17) |
| `https` | `server add NAME --url https://... [--fingerprint F] [--ca-file] [--client-cert --client-key]` | TLS 1.3 minimum | Required for `AuthUser` routes |
| `ssh` | `server add NAME --ssh user@host [--remote-socket PATH]` | Unix socket forwarded by the system `ssh` client | Required: pairing stays mandatory (D12) |

Rules: `http://` is refused. Redirects are never followed. A pinned certificate is matched against the SHA-256 of the leaf (I18); without a pin the normal chain and hostname checks apply. There is no "insecure" flag and no trust-on-first-use.

Every request is built from the shared route table:

- URL from `api.ByName(route).URL(...)`, path values escaped;
- `Vidyom-Tenant` by `Route.Tenant`: required (missing fails locally with `tenant_required`), optional (sent when known), none (never sent);
- `Authorization: Bearer` only on `AuthUser` routes, never on `AuthNone` routes;
- `Idempotency-Key` on every mutating route;
- non-2xx answers decode to `*errs.Error` carrying the request ID. Raw response bodies are never echoed.

## 5. Output

- `--json` on every command. Exactly one JSON document on stdout, or NDJSON for streams. Errors go to stderr as `errs.Body`.
- Human output: tables through `tabwriter`, key-value blocks for details, progress on stderr.
- Exit codes: `0` success, `1` failed, `2` usage, `130` interrupted.
- Output never holds raw credentials. Signed proxy links and transcript text appear only in the commands whose purpose is to show them (`clip preview`, `asset proxy-link`, `approval show`, `asset transcript`, `original search`).
- Human output strips control characters from server-provided text, so a title or a log line cannot drive the terminal. `--json` leaves encoding to JSON.
- Destructive or costly commands ask first: `asset delete` and `storage restore` need a confirmation, or `--yes`; without a terminal and without `--yes` they refuse. `unpublish` is immediate by D17 and does not ask.

## 6. Commands and routes

Every route in `api.Routes` is reached by a command, except the worker routes (not a client's), `metrics` (a scrape endpoint) and the `/clients` alias (I38: the CLI uses `/devices`). A test enforces this list.

| Command | Route(s) |
|---|---|
| `daemon status`, `daemon health`, `version` | `version`, `health` |
| `server add/pair/use/list/remove` | `devices.pair` for `pair`; the rest local |
| `device pairing-code create/list/revoke/rotate` | `pairing_codes.create`, `devices.*` |
| `tenant list/use/create` | `tenants.*` |
| `user add/list/grant` | `users.*` |
| `budget show/set/override` | `budget.*` |
| `original import/list/show/search` | `workflows.get`, `uploads.*`, `workflows.run`, `assets.list`, `assets.get`, `search` |
| `asset upload/list/show/update/lineage/versions/delete/set-ai/consent/transcript/proxy-link/export` | `uploads.*`, `assets.*` |
| `asset sensitive list/add/remove` | `assets.sensitive.*` |
| `collection list/set-ai` | `collections.*` |
| `clip create/preview/approve/list` | `clips.create` or `workflows.run`, `assets.proxy_link`, `approvals.list` + `approvals.approve`, `assets.list` |
| `approval list/show/approve/reject` | `approvals.*` |
| `workflow list/show/validate/run` | `workflows.*` |
| `node list/describe/run` | `nodes.*` |
| `job list/status/attach/logs/retry/pause/resume/cancel/resolve` | `jobs.*` |
| `connector list` | `connectors.list` |
| `storage list/test/verify/copy/restore` | `storages.*` |
| `publishing list` | `publishing.destinations` |
| `publish <asset> --to`, `publish status`, `publish sync` | `publications.create`, `publications.list`, `publications.sync` |
| `unpublish <asset> --all/--from`, `unpublish <PUB-nnn>` | `publications.unpublish`, `publications.withdraw` |
| `secret add/list/test/rotate/delete` | `secrets.*`; `--location client` stays in the keyring |
| `security status/audit/sessions/revoke` | `health`, `doctor`, `audit.*`, `devices.list`, `devices.revoke` |
| `logs [--follow]` | `logs.list`, `logs.stream` |
| `observability list/test/enable/disable/status` | `observability.*` |
| `doctor [--security/--network/--storage]` | `doctor` plus client-side checks |
| `plugin list/inspect/install` | `plugins.*` |
| `hook list/test` | `hooks.*` |
| `event list/watch/deliveries` | `events.*` |

List commands take `--limit`, `--cursor` and `--all`. Global flags: `--json`, `--server`, `--tenant`, `--idempotency-key`.

## 7. Uploads

Client flow, for `asset upload` and `original import`:

1. Check the workflow first when one is named, so a 10 GB transfer does not start against a workflow that does not exist.
2. Look up the checkpoint for (server, tenant, absolute path). A checkpoint is reused only if size and modification time still match; otherwise the old upload is aborted and a new one starts.
3. Hash the file, or reuse the checkpoint's hash.
4. Write the checkpoint with a fresh idempotency key **before** `POST /assets/uploads`, so a kill between the two cannot create a second upload.
5. Save the `UPL-nnn`. `GET /uploads/{id}` gives the offset to resume from. An expired, failed or mismatched upload starts over once.
6. Send chunks with `PATCH`, `Upload-Offset` and `Upload-Checksum: sha256 <b64>`. The chunk size comes from the server's record, capped to bound memory. Field names follow the contract (`size_bytes`, not the `size` in I20's prose).
7. On a network error, `429` or `5xx`: backoff with jitter, respect `Retry-After`, ask the server for the offset, continue. On `409`: ask for the offset, continue. Any progress resets the retry budget. Other `4xx`: fail.
8. `POST /uploads/{id}/complete`. A checksum rejection deletes the checkpoint and fails clearly.
9. Start the workflow with `file=upload:UPL-nnn` and an idempotency key derived from the upload, so a kill between `complete` and the run cannot start two jobs. The checkpoint is deleted only after the run was accepted.

## 8. Streams

SSE per I3. `job attach` and `job logs --follow` read `GET /jobs/{id}/events`; `logs --follow` reads `GET /logs/stream`; `event watch` reads `GET /events/stream`. Reconnection uses `Last-Event-ID` with backoff, retries through daemon restarts, stops on `end`, on a non-retryable error or on interruption. A watchdog drops a connection silent for three keep-alive periods. These paths issue GETs only, and a test asserts no mutation is sent on either path, including on Ctrl+C.

## 9. Secrets and credentials

- `secret add` and `secret rotate` have no value flag and refuse extra arguments. A terminal gets a no-echo prompt; anything else is read from stdin. Flag parsing errors on those commands never echo what was typed.
- `--location client` stores the value in the OS keyring and never calls the API, as the contract's `PutSecretRequest` requires. Only the secret's name goes in the client configuration, so `secret list` can show it.
- `server pair`: probe the keyring with a write **before** exchanging the code, so a client without a keyring does not burn the one-time code. The token is never printed, logged or written to the config.
- `device rotate` of this client's own device stores the new credential in the keyring. Rotating another device discards the returned credential unless `--reveal` asks for it (I167).
- On a Unix socket a missing keyring is not an error, because the peer identity authenticates the call (I161). On `https` and `ssh` it fails with `security.ErrKeyringUnavailable`.
- No credential is ever read from an environment variable or a file in a normal build. A separate build tag provides a file keyring for E2E, so tests never touch a real OS keyring.

## 10. Tests

Against an in-process fake daemon built from `api.Routes` (`internal/apiclient/apitest`), which enforces the tenant rule, authentication and idempotency keys as the table declares them, over a real Unix socket and over real TLS 1.3.

- Table test: each command, in human and `--json` mode, hits the expected route with the expected query and body. The route coverage check fails when a route has no command.
- TLS: pin match, pin mismatch, unpinned self-signed refused, TLS 1.2 server refused, `http://` refused, redirect not followed.
- Keyring: pairing stores only in the keyring; no keyring fails clearly without spending the code; config and output never hold the token.
- Secrets: value from stdin reaches the body, is not in output; no value flag; client secrets never reach the API.
- Upload: fault injection on every step; real `SIGKILL` of a re-executed CLI process during a transfer, resumed with no repeated bytes.
- Streams: reconnection with `Last-Event-ID`, `end` handling, cancellation; no mutation is recorded.
- Parsers: SSE, timestamps, budget amounts, inputs, fingerprints.
- `cmd/vidyom`: built with `CGO_ENABLED=0`, dependency list free of forbidden packages.

Only `go test -race ./cmd/vidyom/... ./internal/cli/... ./internal/apiclient/...` and `make check-static` are the component's gates; `./...` runs before the final report and failures of other components are reported, not fixed.

## 11. Phases

1. **Plan and decisions** (this file, then `docs/decisions/cli.md`).
2. **`apiclient` core:** route-built requests, errors, transports, fingerprint pin, query encoder. `apitest` fake.
3. **`cli` core:** configuration, keyring access, root command, output, `server`, `device`, `tenant`, `daemon`, `version`.
4. **Read commands and control:** everything that is a plain route call: assets, jobs, approvals, workflows, nodes, storage, publishing, secrets, security, observability, plugins, hooks, events, budgets, users.
5. **Streams:** SSE client, `job attach`, `job logs --follow`, `logs --follow`, `event watch`.
6. **Uploads:** engine, checkpoints, `asset upload`, `original import`; kill test.
7. **Hardening:** `ssh` transport, D29 notice paths, doctor client checks, route coverage test, `make check-static`, cross-compilation of `cmd/vidyom`, reconcile with what `api` and `engine` actually landed.

## 12. Dependencies on other components

- `core`: `internal/security/keyring.New()`. Until it lands, the code depends only on `security.Keyring`, and the default build fails closed.
- `api`: the real server. Everything is tested against the fake; points the contract leaves open are listed in the decisions log so `integration` can check them.
- `engine`: input names of `ingest-original` (spec §6.3: `file`, `title`, `category`).
