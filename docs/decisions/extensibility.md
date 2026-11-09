# Implementation decisions: component `extensibility`

Decisions taken while reviewing [`docs/plans/extensibility.md`](../plans/extensibility.md), numbered from this component's range (I260–I279, master plan section 8). They fill gaps in the spec. They never change or relabel the maintainer's `Dn` decisions. Each states the choice, the reason and what would make us revisit it.

**Validate** marks a working default that rests on something unproven on real targets.

---

## Plugins

### I260. A plugin process runs under an OS sandbox, or it does not run

- **Choice.** A process plugin starts under Landlock plus seccomp on Linux and Seatbelt (`sandbox-exec`) on macOS. If the host cannot provide it (old kernel, Landlock disabled, Windows), the plugin is refused with a clear error. The only way past that is the explicit `plugins.allowUnsandboxed: true` in `daemon.json`, which starts it with `isolation: none`. That value is reported by `GET /plugins`, logged and audited at each start. No path falls through silently. It is the same pattern as `file` unsealing in I10.
- **What each level proves.** Linux and macOS enforce filesystem, network and exec allow-lists on the child and everything it spawns. On Linux, Landlock also denies `ptrace` of the daemon, and on ABI 6 or newer also signals and abstract sockets outside its domain. Seccomp denies `socket` for anything other than TCP (nothing at all with no network grant), `io_uring`, `keyctl`, `ptrace` and `process_vm_*`. At `isolation: none`, only the scrubbed environment, the stdio-only channel and daemon-side checks hold; a plugin could read any file its Unix user can read.
- **Why.** §7.6 says filesystem, network, keyring, credentials and arbitrary command execution are not granted automatically. Every plugin process runs as the daemon's Unix user, and `tenant.db` files are not encrypted on disk, so a manifest check with no OS enforcement would grant all of that automatically. §5.6 asks for an isolated process. Both mechanisms ship with the OS and add no dependency. Landlock's thread-scoped restriction lets the Go daemon sandbox only the child, by applying it on a locked OS thread that is then discarded. A probe on Docker Desktop's LinuxKit kernel with the default seccomp profile reports Landlock ABI 8, so the Compose path gets real isolation there.
- **Limits, stated plainly.** Seatbelt is deprecated by Apple, though it still works on this host and is the only built-in option. Landlock port rules are per port with no destination, so a plugin with a network grant can reach any host on the proxy's random port number. Plugins of one tenant can still contend for CPU and memory, because there are no resource limits in this version.
- **Revisit if.** A plugin needs resources limits, or a stronger boundary (per-plugin Unix user, containers, WASM). **Validate:** Landlock on the Linux hosts operators actually use; Seatbelt on later macOS versions.

### I261. The manifest declares, the tenant grants

- **Choice.** `permissions` in the manifest is the most a plugin can ever hold. A tenant grants explicitly in `<config-dir>/tenants/<tenant>/plugins.json`:

  ```json
  {
    "plugins": {
      "sample-notifier": {
        "enabled": true,
        "permissions": { "secrets": ["notifier-token"], "filesystem": ["job-temp"], "events": ["publication.completed"] }
      }
    }
  }
  ```

  Effective permissions are the intersection. A grant naming something the manifest does not declare is a configuration error and the plugin does not run for that tenant, rather than quietly narrowing. With no entry, or `enabled: false`, the plugin does not run for that tenant at all: hooks get `forbidden` and no event is delivered. The file is read through `plugins.GrantSource`, default `plugins.FileGrants`, so nothing in `core`'s configuration loader needs to change. A grant change restarts the process, because the sandbox of a running process cannot be widened.
- **Why.** §7.6 and §9.4: "declared permissions" and "not automatically". The registry is one per host (`system.db`, §5.1), but D6 says a tenant's secrets and events are its own. Without a per-tenant grant, a plugin installed by anyone could read a same-named secret from any tenant whose workflow names it. Files keep it reviewable, as I34 asks of connector settings.
- **Revisit if.** Tenant admins need to change grants through the API; that would add routes.

### I262. One process per tenant and plugin

- **Choice.** A running process serves exactly one tenant, so it never holds two tenants' data, and its temp directory and sandbox are per tenant. Started on first use, stopped after 5 minutes idle. In-flight limit 8 per process, 64 processes per daemon; beyond that a call answers `unavailable`.
- **Why.** D6: "plugins and filters only see events of their own tenant". Simple to reason about. A shared process would need the plugin itself to keep tenants apart.
- **Revisit if.** Process counts become a real cost on hosts with many tenants.

### I263. Network grants go through a per-process egress proxy

- **Choice.** `permissions.network` entries are `host` (meaning port 443 only), `host:port`, or `*.suffix` with an optional port. If there is at least one effective entry, the runtime starts an HTTP proxy on a random loopback port for that process, authenticated by a per-process token, and sets `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy` and `https_proxy`. The sandbox allows outbound TCP only to that port. The proxy allows `CONNECT` and absolute-URI requests only to matching destinations, refuses link-local resolved addresses unless the rule is that IP literal, and logs each denial. With no effective entry there is no proxy, and the sandbox denies the network outright. Plugins that ignore the proxy variables simply cannot connect.
- **Why.** §7.6's example grants a hostname (`storage.googleapis.com`). Neither Landlock nor Seatbelt can express that. A small proxy can, in a way any HTTP client in any language supports. This makes the grant enforceable rather than documentary.
- **Limits.** Only HTTP and TLS through `CONNECT`, with no UDP and no raw TCP protocols in this version. At `isolation: none` the proxy is advisory.
- **Revisit if.** A plugin needs another protocol.

### I264. Filesystem grant vocabulary

- **Choice.** `job-temp` means a private temp directory for this process, given in `InitializeParams.TempDir`, `TMPDIR` and `HOME`, and deleted at exit. Any other entry must be an absolute path, read-write, or `ro:/absolute/path` for read-only. A path overlapping the daemon's data, config, spool, cache or scratch directories or the plugin store is refused at install and at grant. System paths needed to run (`/usr/lib`, certificates, resolver files and so on) are readable by enumeration. `/etc` as a whole is not, since config could live there.
- **Why.** §7.6 shows `job-temp` without defining it, and `Permissions.Filesystem` says "job-temp and declared paths". Since one process serves many jobs (I262), a per-job directory cannot be in a sandbox fixed at start. Hooks receive no files in this version, so a private directory is enough.
- **Revisit if.** Plugins need to read job scratch; a tenant-wide scratch grant could be added.

### I265. Command execution is not grantable in this version

- **Choice.** `plugin.Permissions` has no exec entry, and none is added. The sandbox allows executing only the entrypoint, the plugin's own directory and `plugins.runtimePaths`, an operator-level list for interpreters (for example the Python install a script plugin's shebang names). `/bin/sh` and `/usr/bin` are readable but not executable, unless the operator lists them. `manifest.entrypoint.command` is relative to the plugin directory with no shell, so `python3` as a bare command is refused; a script with a shebang is how interpreted plugins run.
- **Why.** §7.6, §9.4: "no arbitrary shell", "not arbitrary command execution". Interpreters are an operator's decision about the host, not something a plugin asks for.
- **Revisit if.** A plugin legitimately needs a helper tool. It should ship the tool in its own directory, which is already allowed.

### I266. Secrets reach a plugin only through `host.secret`

- **Choice.** New reverse request `host.secret {name}`. It succeeds only for a name in the effective `permissions.secrets`, resolved through that tenant's `connector.SecretResolver`, and is audited with the name and never the value. A denial is audited too. Values are never in environment variables, arguments or `initialize`. The runtime remembers values handed to a process and scrubs them from that process's stderr, `host.log` and returned error messages and reasons. Hook and event data pass through the redactor on the way out.
- **Why.** The contract's method set was one-way, and `NodeExecuteParams` says secrets are never included. A reverse call means one permission check, at the moment of use, as `connector.SecretResolver` already describes.
- **Revisit if.** A plugin connector needs delegated, short-lived credentials instead.

### I267. Two hashes, verified at every start

- **Choice.** `manifest.sha256`, optional as in the schema, is the entrypoint's hash, as its contract comment says. It is checked at install when present. `PluginRecord.SHA256` is always computed at install, as a digest over every file's path, executable bit and content, so a changed Python module or manifest is caught too. Both are verified at every process start. A mismatch sets `Enabled: false` in the registry, audits, and refuses the start; a fresh install is what re-enables it. Signatures are not implemented. Origin is recorded only as the install source path in the audit.
- **Why.** §9.4: "Verify origin, hashes, signatures when possible, and version compatibility". An entrypoint-only hash protects little for script plugins. There is no signing infrastructure to verify against.
- **Limit.** A small window remains between hashing and exec. The store is read-only and not writable from inside any sandbox, so this only matters to a same-user process outside the sandbox, which could equally alter the daemon.
- **Revisit if.** Plugins are distributed from a registry, which is when signatures become meaningful.

### I268. Install rules

- **Choice.** `Install` requires, beyond the route's `plugin.install`, `Principal.SystemAdmin`. The path must be absolute and resolve inside one of `plugins.installRoots`; with none configured, install fails with `failed_precondition`. Files are copied to `<dir>/<id>/<version>-<hash12>/` and made read-only. Symlinks, devices, setuid bits, more than 20 000 files and more than 1 GiB are refused. Same ID, version and digest is an idempotent no-op; same ID and version with a different digest is `conflict`. A different version replaces the record and restarts running processes. Non-admins do not see `installed_by`. `service.Plugins` has no enable or uninstall because `api.Routes` has none.
- **Why.** A plugin is code on the host, reaching every tenant that enables it, so a tenant admin alone must not be able to add it. The system admin check is additive to the route's permission and changes no contract. The roots stop `plugin install` from being a way to execute any directory the daemon can read.
- **Revisit if.** Tenants get dedicated hosts. Uninstall arrives with a route.

### I269. What a plugin can provide in this version

- **Choice.** `provides.hookActions` and `provides.eventSubscriptions` work. A manifest with `nodeTypes`, `storageDrivers`, `publishingDrivers`, `catalogDrivers` or `logSinkDrivers` validates against the schema, as §7.6's own example must, and is refused by `plugins.Validate` and install with `not_implemented`, naming the field. `runtime: remote` is handled the same way and is also refused if a record somehow exists at start. This reconciles I42's "validates and refuses to start" with the master plan's "refused at validation": it passes the schema and fails the semantic validation.
- **Why.** The component's scope is hooks and event subscribers. Accepting a connector plugin and silently not activating it would present an unimplemented thing as working. Refusing is what section 8 rule 6 asks for. Protocol types `node.*` and `connector.invoke` stay in the contract, unused in this version.
- **Revisit if.** A real need for plugin-provided nodes or connectors appears. A node adapter is the smaller step.

### I270. How the Event Bus reaches plugins

- **Choice.** New contract interface `plugin.EventSubscriber`: `EventSubscribers(ctx, tenant, type) ([]string, error)` and `DeliverEvent(ctx, pluginID, envelope) error`, implemented by `*plugins.Runtime`. Engine records deliveries as `plugin:<id>` and retries on error; the runtime checks the grant again at each delivery. Event names are exact, with no wildcards. `provides.eventSubscriptions` must be covered by `permissions.events` in the manifest.
- **Why.** The master plan's wiring table lists `hook.ActionRunner` but nothing for events. §8.2 assigns retries and tracking to the bus, and access control to subscriptions.
- **Revisit if.** `engine` lands an equivalent interface. An adapter in `wire.go` then closes the gap, and this is flagged in the final report either way.

### I271. Protocol details

- **Choice.** `initialize` has 10 s. The answer must have `protocol_version` 1, the manifest's `plugin_id`, and `provides` within the manifest; otherwise the process is killed and the failure counts. `contracts` entries must name a known family (`plugin`, `hook`, `event`, `storage`, `publishing`, `catalog`, `log_sink`, `node`) and a compatible major (`1.x`, `1`, `1.2`). `RunHookAction` applies `hook.timeoutSeconds` or 30 s (D40) on top of the caller's context. Three consecutive timeouts restart the process, because a plugin can ignore `$/cancelRequest`. A crash gives the next start a backoff of 1 s doubling to 60 s; `unavailable` is answered meanwhile. Shutdown: `shutdown` with 3 s, then SIGTERM, then SIGKILL 2 s later, always to the process group. A plugin sending more than 1 000 messages per second to the daemon is killed as a flood; `Conn.Run` would otherwise start one goroutine per message. `hook.Invocation` gains `idempotency_key` (additive, derived from tenant, job, node, point and hook ID), so a hook retried by `onFailure: retry` looks the same to the plugin, as §8.1 asks.
- **Why.** §8.1 requires timeout, idempotency, logs and traces per hook; these numbers are not in the spec. Limits follow §8.2's "missing limits" warning.
- **Revisit if.** Real plugins hit them.

---

## Observability

### I272. Two pipeline levels, with tenant routing

- **Choice.** The daemon pipeline comes from `config.Daemon.Observability` and receives everything. A tenant pipeline comes from `config.Tenant.Observability`, loaded lazily off the record path, and receives only records and points whose `tenant_id` is that tenant. `Sinks`, `TestSink` and `SetSinkEnabled` show a tenant its own sinks; daemon-level sinks also appear only for a system admin, and are `not_found` to anyone else. `Logs` and `SubscribeLogs` always filter on `Scope.Tenant`, and records with no tenant are visible to no tenant-scoped query. A tenant-level `prometheus` entry creates no instance and reports `via daemon endpoint`, because there is one `/metrics`. `MetricsHandler` filters to the tenant named in `Vidyom-Tenant`; a request without it gets every series. The API layer must therefore set that header for anyone but a system admin (flagged for `api` and `integration`). The API plan states the same rule.
- **Why.** I34 has `observability.json` per tenant and `config.Daemon` has its own, and §7.4 says multiple destinations are active at once. D6 says a tenant sees only its own. The contract gives a handler no principal, so the header is the only signal it has.
- **Revisit if.** Sinks need richer routing rules than tenant.

### I273. Pump numbers and drop policy

- **Choice.** Per sink: queue 10 000 entries, batch 256 or 1 s (100 ms for in-memory exporters), write timeout 10 s, backoff 500 ms doubling to 30 s, 5 attempts per batch, then the batch is dropped and counted. Queue overflow drops the oldest. Close drains for at most 5 s. Exporter health metrics are published from counters every 5 s. A sink that fails to open is retried every 30 s with its queue still filling. `Emit` and `telemetry.Metrics` never block on a sink and never return an error. A configured sink that is unknown, or cannot open, does not stop the daemon; it shows `status: failed` and a redacted `last_error`.
- **Why.** §10.1 and §15.7 ask for buffering with limits and a failure record, and that exporters never affect processing. Dropping the oldest favors recent data during an outage. Audit records never pass through here: core writes them synchronously, so "critical audit events cannot be silently dropped" is not at stake.
- **Revisit if.** Operators need a persistent on-disk queue for exporters.

### I274. Redaction rules

- **Choice.** Replacement text is `[REDACTED]`. Keys are matched case-insensitively for: `secret`, `token`, `password`, `passwd`, `api_key`, `apikey`, `private_key`, `credential`, `authorization`, `cookie`, `signed_url`, `signature`, `transcript`, `captions` (except `credentialsRef`-style names ending in `ref`, which are references and are kept). Values are scanned for: `Bearer …`, `Basic …`, `vdm_…` device credentials, JWTs, `AKIA…`, `AIza…`, `ya29.…`, `sk-…`, PEM private key blocks, query parameters such as `X-Goog-Signature`, `X-Amz-Signature`, `sig`, `token`, `access_token`, `key`, and `user:password@` in URLs. Exact values can be registered with `Redactor.AddValue`. Message, event, status and every field are covered. Long strings are truncated at 64 KiB.
- **Why.** §10.1 names secret redaction in the manager, and §9.4 asks to minimize exposure of transcripts in logs. A pattern list cannot catch every secret, which is why callers must not log them in the first place: this is a second layer.
- **Revisit if.** Real logs show false positives on `key` in URLs; it is the most likely.

### I275. File sink details and the default local log

- **Choice.** Parameters as in §7.4 (`path`, `rotation`, `retentionDays`). Rotation is on the UTC day. The active file `vidyomd.jsonl` is renamed to `vidyomd-YYYY-MM-DD.jsonl`, named for the day its records belong to, with `-N` added on a collision. Retention deletes rotated files older than `retentionDays`, at open and at each rotation. Directory mode 0750, file 0640. A batch is one `write` call; `Flush` is an `fsync`. With no sink configured, an implicit `local` sink uses `<log-dir>/vidyomd.jsonl`, where `Options.LogDir` is set by `wire.go` (normally `<dataDir>/logs`). If `LogDir` is empty the ring is all there is, and `Sinks` says so. Each process owns its file: the daemon and each worker need different paths. This sink owns retention of JSONL logs; I45's sweep by `core` concerns job records in the database.
- **Why.** D38 sets JSONL by default, §7.4 names the parameters but not their detail.
- **Revisit if.** Files grow large enough that `GET /logs`, which scans them, is too slow; it reads in bounded memory but linear time.

### I276. Loki details

- **Choice.** `POST <url>/loki/api/v1/push`, JSON with gzip, up to 1 MiB per request, split as needed. Stream labels are `service`, `level` and `tenant_id` plus configured static `labels`; everything else stays in the line. `credentialsRef` holds either `user:password` (Basic) or a token (Bearer). `parameters.orgId` becomes `X-Scope-OrgID`. A URL with user information is refused, because configuration holds no secrets. Credentials over `http://` to a non-loopback host are refused too; plain `http://` without credentials is allowed, for the Compose profile's internal network.
- **Why.** The spec names Loki only. Low-cardinality labels are Loki's own guidance, and `job_id` belongs in the line.
- **Revisit if.** The optional Compose profile needs structured metadata. Mock-tested only (I12).

### I277. Metric catalog, `tenant_id` and labels

- **Choice.** `telemetry.Catalog` in the contract lists every metric with kind, help, labels and buckets. **Every** metric has `tenant_id`, including host-level ones, for which the value is `telemetry.SystemTenant` (`_system`, which cannot collide with a slug, I16). An empty label in Prometheus means absent, which would not satisfy D6. Label sets are normalized to the catalog: unknown labels are dropped, which also stops accidental cardinality, and missing ones become empty. A metric not in the catalog is registered on first use with `tenant_id` added. Each metric is capped at 10 000 series. §10.3 items with no constant get additive ones: `vidyom_ai_time_estimate_seconds_total`, `vidyom_usage_quantity_total`, `vidyom_storage_stored_bytes`, `vidyom_plugin_failures_total`, `vidyom_security_anomalies_total`. `vidyom_usage_cost_estimate_minor_total` carries `month` (§10.3 "per month"), filled by the manager when missing. Go and process collectors are included on an unfiltered scrape, and have no tenant.
- **Why.** The facade takes free-form labels, but Prometheus vectors need fixed ones, and D6 and §10.3 say all metrics carry `tenant_id`.
- **Revisit if.** `_system` confuses dashboards.

### I278. OTLP through the official exporters

- **Choice.** Traces: `otlptracehttp`, with `SpanRecord` turned into an SDK snapshot. Metrics: `otlpmetrichttp` (a new dependency, same OpenTelemetry release as the rest), feeding SDK instruments from `MetricPoint`s with a periodic reader. `parameters.endpoint` is the base URL. Same credential rules as I276, with `credentialsRef` sent as a Bearer token. Exporter retries are off, since the pump retries. Semantics of `MetricPoint.Value`, not stated in the contract: a counter is a delta, a gauge is the current value, a histogram is one observation. The manager installs an SDK `TracerProvider` with a parent-based, always-on sampler whether or not an exporter is configured, because `trace_id` is needed in logs and events anyway. `Manager.InstallGlobals` sets it and the W3C propagator for components that use the OpenTelemetry API directly.
- **Why.** D38 makes OpenTelemetry the instrumentation standard. Hand-written OTLP would be a protocol to maintain.
- **Revisit if.** Binary size matters for the worker. Mock-tested only (I12).

### I279. `observability enable` and `disable` persist

- **Choice.** `SetSinkEnabled` writes an overlay file (`Options.StateFile`, tenant and sink ID to a bool), audited, applied over the configuration at load. With no `StateFile`, it applies until restart and `SetSinkEnabled` says so in its audit details. `TestSink` writes one record, point or span directly, serialized with the pump, opening a temporary instance when the sink is disabled, and answers `unavailable` with a redacted message on failure.
- **Why.** §11.5 has those commands and I34 keeps configuration in reviewable files the daemon must not rewrite. A disable that silently reverts at the next restart would be worse than either.
- **Revisit if.** Tenant admins can edit connector configuration through the API.
