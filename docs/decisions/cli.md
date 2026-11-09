# Implementation decisions: `cli`

Decisions of the `cli` component, numbered from its range `I160–I179` (master plan, section 8). Same format as [`docs/decisions.md`](../decisions.md): the choice, the reason and what would make us revisit it. None of them changes a maintainer decision `Dn`. What this log says the CLI does is a **target** until the component's commits land; the final report states what is done and what is only tested against a fake daemon.

## Configuration and identity

### I160. Client configuration holds no secret

- **Choice.** `<config-dir>/config.json`, where `<config-dir>` is `VIDYOM_CONFIG_DIR` or `os.UserConfigDir()/vidyom`. Mode `0600`, directory `0700`, written through a temporary file and a rename. It holds servers (kind, endpoint, TLS pin, CA and client certificate paths, current tenant, device ID and email), the current server and the names of client-location secrets. Credentials are in the OS keyring only.
- **Why.** §9.1 forbids a plaintext fallback, and a config file is the first place one would creep in. Tests scan the file for the token.
- **Revisit if.** Config needs a system-wide layer.

### I161. Keyring layout, and when a credential is required

- **Choice.** Service `vidyom`. Device credential: account `device/<server>`. Client-location secret: account `secret/<server>/<tenant>/<name>`. `https` and `ssh` servers **require** a credential on `AuthUser` routes: no entry says "not paired" and points to `server pair`, no keyring returns `security.ErrKeyringUnavailable`. `unix` servers send a credential when one exists and otherwise rely on the peer identity (I17), so a missing keyring there is not an error. No normal build reads a credential from an environment variable or a file.
- **Why.** §9.1 and §15.6 ask for a clear failure with no plaintext. The Unix socket has its own identity (R19), and refusing it on a headless host would make the host operator's path depend on a desktop keyring that the transport does not need.
- **Revisit if.** The daemon stops honoring peer identity.

### I162. Implicit `local` server

- **Choice.** With no `--server` and no current server, the CLI talks to an implicit server named `local`: `VIDYOM_SOCKET` when set, otherwise the platform default (`/run/vidyom/vidyomd.sock` on Linux, `~/.vidyom/vidyomd.sock` on macOS, none on Windows, which uses HTTPS on loopback per I44). `tenant use` persists it as a real entry. The first `server add` becomes current; later ones do not change it.
- **Why.** §5.3 makes the socket the same-host default, and a user on the host should not have to name it. **`integration`:** `daemon.json`'s `listeners.unix.path` has no contract default, so the Compose and demo configuration must either match this default or set `VIDYOM_SOCKET`.
- **Revisit if.** The daemon defines a different default socket.

### I163. Server names and endpoints fail closed

- **Choice.** `--url` must be `https://host[:port]` with no user information and no path; `http://` is refused with no flag to override it. A re-`add` of an existing name needs `--force`, and `--force` with a changed endpoint deletes the stored credential, so a token is never sent to a new endpoint under an old name. Redirects are never followed. `server remove` deletes the keyring entry.
- **Why.** D12 requires HTTPS. A redirect or a reused name are the two ways a bearer token could leave for another host.
- **Revisit if.** The daemon is deployed behind a path prefix.

### I164. TLS: chain validation or an explicit pin, never neither

- **Choice.** TLS 1.3 minimum. With `--fingerprint`, the SHA-256 of the **leaf certificate's DER** must match, checked in `VerifyConnection` so session resumption cannot bypass it; the pin replaces chain and hostname validation, since it exists for the self-signed loopback certificate of I18. Without a pin, normal validation against the system roots or `--ca-file`. Fingerprints are accepted as hex, with or without colons, in either case, with an optional `sha256:` prefix, and stored as lowercase hex. No trust-on-first-use and no skip-verify flag. `--client-cert` and `--client-key` store **paths** for mTLS (§9.2); the key stays where the operator keeps it.
- **Why.** I18 prints a fingerprint and asks `server add --fingerprint` to pin it. A pinned mismatch is reported as a possible interception, with its own `details.reason`, and is not retried.
- **Revisit if.** An internal CA story replaces self-signed loopback certificates.

### I165. SSH transport uses the system `ssh` client

- **Choice.** `server add NAME --ssh user@host [--remote-socket PATH]` records the target. On the first request of a command, the CLI starts `ssh -N -T -o ExitOnForwardFailure=yes -L <private-dir>/s:<remote-socket> -- user@host`, waits for the local socket, dials it and kills the process at exit. A credential is **required**, as on HTTPS, because D12 keeps pairing mandatory for host operators. The target is validated, so it cannot be read as an `ssh` option.
- **Why.** D12 keeps SSH only for host operators. Delegating to the system client reuses their keys, agent and `known_hosts`, with no new dependency, and keeps the binary free of an SSH implementation with its own trust store.
- **Limit.** Tested with a fake `ssh` that forwards the socket, not against a real SSH server. Windows' OpenSSH Unix-socket forwarding is **Validate**.
- **Revisit if.** Hosts without an `ssh` client need it.

### I166. Pairing

- **Choice.** `server pair NAME --code <code>` as §11.1 shows it, and when `--code` is omitted the code is read through a no-echo prompt or stdin. The keyring is probed with a write of a throwaway entry **before** the exchange, so a client with no keyring fails without spending the code. If the final write still fails, the error says the device was issued and must be revoked, and never prints the token. The exchange is not retried automatically.
- **Why.** The spec's example passes the code as an argument. R36 rules out secrets in arguments, but it names `secret add`, and a one-time code with a short TTL, spent on first use, is a weaker secret than the credential. Both paths exist, so the cautious one is available. `device pairing-code create` prints the TLS fingerprint when the server reports one (I18).
- **Revisit if.** Long-lived pairing codes are introduced.

### I167. Device rotation

- **Choice.** `device rotate [CLIENT-nnn]`, defaulting to this client's device. For this client's own device, the new credential replaces the keyring entry and is not printed. For another device, the returned credential is discarded and the output says that device must pair again, unless `--reveal` asks for it to be printed once, with a warning on stderr. `device revoke` of this client's own device also deletes the keyring entry. Rotation is manual in this version: nothing rotates automatically.
- **Why.** The route returns a credential once (`DeviceCredentialResponse`), and an admin's terminal scrollback is the wrong default place for someone else's token.
- **Revisit if.** D12's "rotated" comes to mean scheduled rotation.

### I168. A file keyring exists only behind a build tag

- **Choice.** `-tags vidyom_test_keyring` builds a CLI whose keyring is the JSON file named by `VIDYOM_TEST_KEYRING_FILE` (mode `0600`). With the tag and no variable it fails as `ErrKeyringUnavailable`. It warns on stderr on every use. Default builds, including `make vidyom` and `make check-static`, do not contain the code, and a test asserts it. **`integration`:** build the E2E CLI with this tag and give each simulated user its own `VIDYOM_CONFIG_DIR` and keyring file.
- **Why.** The E2E scenario pairs several users on one host with the real binary, and tests must never write to a person's real keyring. An environment switch in the production binary would be the silent plaintext path §9.1 forbids, so the switch is not compiled in.
- **Revisit if.** A CI keyring service makes the tag unnecessary.

## Requests and output

### I169. Output and exit codes

- **Choice.** `--json` on every command: one document on stdout, NDJSON on streams, and errors on stderr as `{"error": {...}}`. Commands with no response body emit a small object saying what happened. Exit codes: `0` success; `1` an operation failed, including a validation result of `valid: false`, a failed audit verification, a failed doctor check, an unhealthy `daemon health` and a `job attach` that ended `failed` or `cancelled`; `2` usage; `130` interrupted.
- **Why.** §11.5 asks for JSON for scripts, and the website's automation notes ask for predictable exit codes. The spec fixes no numbers.
- **Revisit if.** Scripts need distinct codes per error class; the JSON error's `code` already gives that.

### I170. Idempotency keys

- **Choice.** Every mutating request carries `Idempotency-Key`: random per command by default; `--idempotency-key` sets it, for scripts that retry. Three are derived so a retried command reuses them: upload creation (a random key saved in the checkpoint before the request), `complete` (from the upload ULID) and the workflow run after an upload (SHA-256 of upload ULID, workflow and canonical inputs). JSON calls are not retried automatically; only uploads and streams retry.
- **Why.** §12.1 makes keys mandatory for mutations and §5.4 asks for resumption. An automatic retry of a call whose outcome is unknown needs a dedup guarantee the fake cannot prove, and a person or a script re-running it can decide.
- **Revisit if.** `api`'s idempotency window is too short for slow retries.

### I171. Open points in the contract, decided on the client side

The contract leaves these open. The CLI takes this position and is lenient where it costs nothing. They were checked once against `api`'s real handler with an uncommitted test (real pipeline and upload service, small fake services), because a component's committed tests may not import another's implementation; `integration` covers them for good.

- `since` parameters: `24h` and `7d` become RFC 3339 timestamps on the client, RFC 3339 passes through.
- List routes decode `{items, next_cursor}` per I15 and I125, and also a bare array.
- `PATCH /uploads/{id}` reads the new offset from the `Upload-Offset` header, then from `received_bytes` in a JSON body, then from `GET /uploads/{id}`. A `409` always re-reads the offset through `GET`. A chunk answered `422` (digest mismatch, I130) is resent. A `complete` refused with `details.offset` goes back to sending from there.
- `GET /health` may answer `503` with the same body (`api`'s handler does), so `daemon health` can still say which component is down.
- `POST /storages/restore` is two-phase, as `api` landed it (I132): first `confirm: false`, which answers `api.StorageRestorePreview` and starts nothing, then, after a confirmation or `--yes`, `confirm: true`. Without a terminal and without `--yes`, `--json` prints the preview and the command fails, so a script sees the cost. Each phase has its own idempotency key, because they are different requests.
- `GET /doctor` without `check` means all checks. `POST /assets/{id}/export` takes an empty object. Query names come from the contract's `json` tags through one encoder.
- Field names follow the contract where I20's prose differs (`size_bytes`, not `size`).
- **Revisit if.** `api` lands something else; these are all on one small surface.

### I172. Upload checkpoints

- **Choice.** One file per (server, tenant, absolute path) under `<config-dir>/uploads/`, with the upload ID, the idempotency key, size, modification time and SHA-256, and no file content. Reuse needs size and modification time to match; the hash is reused then, because hashing 10 GB on every resume is minutes of work and `complete` verifies it on the server anyway. A changed file aborts the old upload and starts a new one. The checkpoint is removed only after the workflow was accepted, or after `complete` when no workflow was asked for. An expired or failed upload restarts once, then fails. Retry budget: 8 attempts without progress, exponential backoff from 1 s to 30 s with jitter, `Retry-After` honored up to 5 minutes, and only an accepted chunk resets the budget. Chunk size comes from the server, capped at 256 MiB to bound memory. One `PATCH` may take 15 minutes before it counts as failed, and `complete` two hours, because the server hashes the whole file.
- **Why.** §5.4 asks for checkpoint and checksum. Keeping the checkpoint until the run is accepted is what makes a kill at any step safe (I170).
- **Revisit if.** Parallel chunks are added (I20).

### I173. Input references and typed inputs

- **Choice.** `original import <path>` and `asset upload <path>` are always client files, and a name like `server:/x` is looked up as a local file. `--server-path` sends `server:<abs>`. Whether a server path is absolute is judged by its shape, not by this client's operating system, because a Windows client names paths on a Linux server. For `workflow run`, values are coerced by the workflow's declared `InputSpec`: `GET /workflows/{id}`, or the file itself. `file` inputs go through `domain.ParseUserInputURI`, so a bare absolute path becomes `server:` (R20), a relative path is an error, and `scratch://` is refused. `number`, `boolean`, `array` and `object` parse as JSON. Undeclared inputs are sent as strings, and the server validates them. `--type` maps to the `category` input of §6.3, and the title defaults to the file's name. The workflow is fetched before any upload starts.
- **Why.** R20 is explicit about each case. Coercion lets `--input count=3` satisfy a `number` input without a second flag.
- **Revisit if.** `engine`'s `ingest-original` renames an input.

### I174. Reattaching and interruption

- **Choice.** Streams reconnect forever with `Last-Event-ID` and 1–10 s backoff, including through daemon restarts, with a note on stderr; a `4xx` other than `408` and `429`, a `501` and any TLS failure stop them. Silence for 45 s, three keep-alive periods, drops the connection, and a server that accepts a stream and answers nothing for a minute counts as a lost connection. A delay a server asks for extends the backoff, capped at 5 minutes. Ctrl+C cancels only the stream, prints that the job keeps running and how to reattach, and exits `130`. The second Ctrl+C kills the process at once. A no-echo prompt restores the terminal itself on interruption. Streams use GET only.
- **Why.** §5.2 and §5.5: clients connect, disconnect and reconnect, and Ctrl+C disconnects.
- **Revisit if.** Someone needs a bounded wait; `--timeout` on `attach` is additive.

### I175. Budget amounts

- **Choice.** `budget set --monthly 500 --currency USD` means 500 **major** units, as a person reads §11.1. It is parsed as a decimal without floating point and converted to `monthly_minor` by ISO 4217 exponent: 0 for JPY-like currencies, 3 for KWD-like, 2 otherwise. More decimal places than the currency has is an error. `budget show` converts back.
- **Why.** The spec's example has a plain amount and the contract stores minor units.
- **Revisit if.** The daemon starts reporting the exponent.

### I176. Approvals and the D29 notice

- **Choice.** `approval show`, `approval approve` and `clip approve` on kinds `publication` and `visibility_change` state: "Image and voice authorizations from speakers and participants are the tenant's responsibility. Vidyom does not track or enforce them." In `--json` the same text is in a `notices` array. `approval approve` reads the approval first, and refuses `--start`, `--end` or `--segment` when `allow_adjust` is false, because the server would ignore them and the reviewer would have approved something else. `clip approve CLIP-nnn` looks up the asset's pending `publication` approval and refuses when there is none or more than one, listing them.
- **Why.** D29 names the "publication approval screen". `visibility_change` can expose a video the same way. §11.2 defines `clip approve` as a shortcut for the single pending approval, and guessing among several is not a shortcut.
- **Revisit if.** D29's text changes.

### I177. Secret location

- **Choice.** `--location` is required on `secret add`, with no default. `client` stores in the keyring under I161's account, and `--ttl` is refused there. `delegated` requires `--ttl`, per §9.1's limited duration. `secret list` merges server metadata with local client-secret names. `secret test`, `rotate` and `delete` act locally when the name is in the local index and `--location` was not given, otherwise through the API. An empty value is an error. A trailing newline from stdin is trimmed once, and the value is otherwise taken byte for byte. The size limit is 1 MiB.
- **Why.** R37 moved examples to `server` deliberately, so a default would hide a choice that matters for autonomous jobs. The contract keeps client secrets away from the route.
- **Revisit if.** Delegated secrets get a server-side default TTL.

### I178. Commands beyond §11, and paths on the server

- **Choice.** Added so every route has a command: `asset list/show/update/transcript/proxy-link/export`, `asset sensitive list/add/remove`, `collection list`, `workflow show`, `user list/grant`, `server remove`, `version`. `plugin install <path>` names a directory **on the server** (the contract's `InstallPluginRequest`): on a `unix` server a relative path resolves against the working directory, since it is the same host; on `https` and `ssh` it must be absolute. Excluded: worker routes, `metrics` (a scrape target with controlled access, §12.1) and the `/clients` alias (I38). Confirmation is asked on `asset delete` and `storage restore`, as `--yes` allows scripts to skip it.
- **Why.** The route table says routes beyond §12.1 back CLI commands, and §10 of the master plan measures coverage by route. **`integration`:** `asset delete` in a script needs `--yes`.
- **Revisit if.** A route gets a better home in another command.

## Disagreements found, not resolved in the sources

Per `AGENTS.md`. The implementation follows the spec. Website rows were checked against the working tree on 2026-11-09, where another session has uncommitted edits, so re-check them against what lands.

| # | Where | Disagreement | This implementation |
|---|---|---|---|
| 1 | `website/docs/reference/cli.md` vs §11.3 | `workflow resume JOB-001` vs `job resume` (R22) | §11.3 |
| 2 | Same vs §11.3 | `job resolve --action retry\|skip` vs `--retry-node`, `--skip-node`, `--fail` | §11.3 |
| 3 | Same vs §11.1 | `device pair-code`, `device pair --server` vs `device pairing-code create`, `server pair` | §11.1 |
| 4 | Same vs §11.1 | `device revoke laptop-01` vs `device revoke CLIENT-001` | §11.1 |
| 5 | Same vs §11.1 | `budget status` vs `budget show` | §11.1 |
| 6 | Same vs §11 | `asset verify CLIP-001` has no spec command or route; `storage verify` does | Not added |
| 7 | I20 vs `api.CreateUploadRequest` | `size` vs `size_bytes` | The contract |
| 8 | §11.4 example vs `api.StorageCopyRequest` | `--to another-storage` names an instance; the field's type is `StorageRole` | Value sent as given, the server validates |
