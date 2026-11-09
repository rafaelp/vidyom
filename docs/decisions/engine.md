# Implementation decisions: `engine`

Decisions the `engine` component took to fill gaps, in the format of [`docs/decisions.md`](../decisions.md), numbered in its range `I140`–`I159`. They never change or relabel the maintainer's `Dn`. What a decision says a version does is a **target** until the component commits it.

---

## Workflow language

### I140. `flow.if` has one output; the other branch is a second `flow.if`

- **Choice.** `connections` stays a plain map of node ID to dependents. The dependents of a `flow.if` run when `result` is true. A false branch is another `flow.if` with the negated condition, joined with `flow.merge` where needed. There are no named ports.
- **Why.** §6.2 and the catalog give `flow.if` a `condition` and a boolean `result`, and D33 only says a branch not taken is skipped. Ports would change the shape of `connections` and every tool that reads it. Two conditions are explicit and each one is logged.
- **Revisit if.** Real workflows repeat long negated conditions.

### I141. Reference grammar and absent values

- **Choice.** `${inputs.<name>[.<path>]}`, `${nodes.<id>.output[.<path>]}`, `${event.<path>}` and `${job.id|ulid|workflow|workflow_version|tenant_id|trace_id}`, nothing else: no spaces, operators or calls. A path segment that is a number indexes an array. A string that is exactly one reference keeps the referenced JSON type; a reference embedded in text must be a scalar. `$${` writes a literal `${`. A declared input with no value, or a node that was skipped or failed under `continueOnFailure`, is **absent**: an object key holding only that reference is removed, and an array element or embedded use fails the node. So optional parameters work and required ones fail the catalog schema with a clear error. `${job...}` is additive to §6.2, and I23 already gives CEL the same `job` values. In CEL, `nodes.<id>` has `output` and `status`, so a workflow can route on a `continueOnFailure` node that failed.
- **Why.** §6.2 says "restricted declarative" without a grammar. Workflows need one way to pass optional values without writing a `flow.if` for each one.
- **Revisit if.** A case needs an explicit default value operator.

### I142. D53 is checked on every workflow, not just `create-clip`

- **Choice.** A workflow with a `video.cut` node must have an `approval.request` with the literal kind `clip_selection` that dominates it on every path of the DAG, has no `continueOnFailure`, and whose approved segments feed it: `segments` is exactly `${nodes.<gate>.output.segments}`, or the `segments` output of a `catalog.resolve_render_source` that takes that value. The validator refuses anything else, for files, inline definitions and `vidyom node run video.cut`. There is no parameter, setting, hook result or API option that skips the gate. A rejected or expired gate has no `segments`, so a cut wired straight after it fails its schema instead of rendering.
- **Why.** D53: "no policy can skip it". Checking one workflow by name would leave a copied file as a way around it.
- **Revisit if.** A non-clip use of `video.cut` appears; it would need its own node.

### I143. The publication gate of `publish-video` is always on

- **Choice.** `publish-video` always creates the `publication` approval. Its input `requireApproval` exists because §6.4 passes it, defaults to `true`, and `false` skips the whole workflow rather than the gate: nothing is uploaded and the job completes with no publication. `publishing.defaults.requireApproval` does not relax this. `allowPrivateUploadBeforeApproval` (D37) is reached through a separate workflow, `publish-private-first`: `publish-upload-private` uploads to each destination, `publish.prepare` refuses that unless the destination allows it, then `publish-video` runs, adopts those records and opens visibility only after the gate. Being separate, it can require `publication.create` from whoever starts it, without making `publish-video` require it (I157).
- **Why.** §3.2 says "mandatory by default" without naming who may turn it off, and D7 says a workflow can tighten, never loosen. An editor can run a workflow; a skip switch would let an editor publish. Failing closed costs nothing until the maintainer defines the policy.
- **Revisit if.** The maintainer defines who may disable the gate.

### I144. Immutable snapshots cover the whole job tree

- **Choice.** At submission the engine resolves every `workflow.execute` and `flow.foreach` target, recursively, and stores those definitions in an additive `subworkflows` field of the snapshot, keyed `id` or `id@version` as the node names them. Child jobs start from that bundle, never from the registry. The hash covers the bundle, so provenance identifies everything that ran. A submitted definition that already carries `subworkflows` is refused; only the engine writes that field. Target names must be literal strings. Recursion among workflows is refused at validation.
- **Why.** §6.5: "new versions of the flow do not change jobs in progress". A clip waiting days for approval starts `ingest-derived` long after submission.
- **Revisit if.** Snapshots get large; a content-addressed table could replace the bundle.

### I145. Failure status of the asset

- **Choice.** Additive `settings.assetStatusOnFailure`, whose only value is `ingest_failed`. When the job writes `failed` or `cancelled`, the same transaction moves the job's asset to it if the state machine allows. `job retry` moves it back to `ingesting`. `ingest-original` and `ingest-derived` set it.
- **Why.** §2.5 requires `ingest_failed`, and a DAG with no failure edges has no way to express it. An engine rule keyed on workflow names would be hidden behavior.
- **Revisit if.** Other workflows need failure handling, which then needs a general error path.

---

## Execution

### I146. Durable node state uses the existing fields

- **Choice.** `NodeExecution` keeps its contract fields. `LeaseExpiresAt` is the node's deadline: lease expiry when `running`, resume time when `waiting`, next attempt when `ready` after a failure, next reminder for an approval with `onTimeout: remind`. `Output` holds the suspension state while `waiting`, and the checkpoint while a finishing node runs its `after` hooks. `TaskID` names the current attempt. A cancel request is `Job.StatusReason` starting with `cancel_requested`. A node waiting for a person to look at it stays `failed` with error code `needs_attention`.
- **Why.** Every repository implementation must persist these fields already, and `core` stores columns, so new fields would not survive without a coordinated migration. Only the daemon reads them.
- **Revisit if.** A later schema version gives these their own columns.

### I147. Retry budget

- **Choice.** The limit is `node.retry`, else `settings.retry`, else 3, and counts retries after the first attempt. Backoff is `settings.retryBackoffSeconds` (default 5 s), doubling, capped at 5 minutes. `Attempt` grows for the whole life of the node and is part of the task ID, so old pointers are recognized. An interrupted lease counts as an attempt. A person's `job retry` or `job resolve --retry-node` grants one more attempt, then the automatic limit applies again, so it fails at once if that attempt fails and the budget is spent. Worker and daemon pool failures with `retryable: false` never retry.
- **Why.** The spec asks for "backoff and limits" with no values. Automatic retries after a person's action would hide a repeating failure.
- **Revisit if.** Operators want a fresh budget per manual retry; it needs a new field.

### I148. `OutcomeUnknown` by class

- **Choice.** `none` and `idempotent`: retry with the same idempotency key, since the class promises that is safe. `reconcilable`: `Reconcile` first. `unsafe`: `needs_attention`. `Reconcile` runs only in the daemon pool, where `publish.upload` is the one such node in the catalog; the worker protocol has no reconcile call, so a reconcilable node of another pool that fails goes to `needs_attention`. A daemon-pool node found `running` at startup was interrupted, and is handled at once. A worker-pool lease gets a new grace period at startup, because the worker may still be running and will heartbeat.
- **Why.** D11's table defines retry by class, and the master plan's "any `OutcomeUnknown` that cannot be reconciled" reads naturally this way: a retried idempotent key is the reconciliation.
- **Revisit if.** An idempotent connector proves not to be.

### I149. Hooks run before the state they announce

- **Choice.** `node.after` and `node.finish` run after the output is stored and before the node is `completed`. `workflow.after`, `workflow.error` and `workflow.finish` run before the terminal state is written, and the `workflow.*` event commits with that state. A crash re-runs them, which §8.1's idempotency requirement allows. Default `onFailure` is `abort` for `before`, `continue` for the others. `abort` at `start` or `node.after` fails the job, and the node stays checkpointed. `error`, `finish` and `cancel` can never block, and their `abort` is logged like `continue`. `retry` tries 3 times with backoff, then acts as `abort`. Order is global, workflow, node for `before` and `start`, reversed for the rest. `maxTimeoutSeconds` caps every hook's timeout; 300 s when unset. A hook blocking at `before` is audited.
- **Why.** §8.1: "`finish` runs for valid termination even after the daemon resumes". Writing the state first would need a second durable flag to know a hook is owed.
- **Revisit if.** A notification hook must see the terminal state already readable through the API.

### I150. Resolve semantics

- **Choice.** `job resolve` is accepted only in `needs_attention`, and `retry_node` and `skip_node` must name the node in `Attention`. `retry_node` dispatches another attempt with the same idempotency key. `skip_node` marks the node `skipped`, and D33's propagation applies, so nothing dependent runs on missing data. `fail` fails the job. `cancel` and `pause` are refused in `needs_attention`. Publication nodes are types `publish.*` and `catalog.public.sync`, and need `job.resolve.publication`. Other nodes need `job.resolve`. Each is audited with the permission that authorized it.
- **Why.** §11.3 names the three actions and says resolve is the only way out, and D33 is the only skip rule the spec has.
- **Revisit if.** People need to supply the outcome of a node they verified by hand, which needs an output field on the request.

### I151. Settings that are always on, and the job timeout

- **Choice.** `resumeOnFailure` and `saveExecutionProgress` are accepted and change nothing, because D11 requires a checkpoint after each node. `settings.timeoutSeconds` is measured from the job's start, excluding time its own `approval.request` nodes spent waiting; on expiry the job is cancelled cooperatively and ends `failed` with code `timeout`. Child jobs have their own clocks.
- **Why.** A job should not time out because a person took a week to approve.
- **Revisit if.** Waits in child jobs need to count.

### I152. Timers are a sweep, not an in-memory wheel

- **Choice.** Every second a sweeper asks `advance` to look at each active job, which handles leases, backoff, `flow.wait` and approval timeouts by comparing deadlines with the clock; jobs in `waiting_approval` are looked at every 30 s. Every 30 s reconciliation re-enqueues `queued` nodes missing from Redis. Direct wake-ups cover the common paths. All intervals are options on `jobs.Deps`.
- **Why.** The database is the source of truth, so there is no timer state to rebuild after a restart. One host and SQLite make the cost negligible at this scale. Reconciling periodically and not only at startup means `FLUSHALL` under a running daemon heals too (§1.1 step 13).
- **Revisit if.** Tenants hold many thousands of active jobs.

### I153. Scratch across jobs and its cleanup

- **Choice.** `Lease.ScratchJobs` (I182) lists the job's ancestors, so `${nodes.render.output.file}` can feed `ingest-derived` as §6.4 shows. `Lease.AI` carries the tenant's AI settings for pool `ai` (I197). `ScratchCleanupPointer` (I181) is enqueued when a job reaches `completed` or `cancelled`. A `failed` job keeps its scratch, because `job retry` resumes from checkpoints whose outputs may name those files; `media`'s idle reaper removes it after 7 days, after which a retry fails on the missing file with a clear error.
- **Why.** D22 asks for deletion at job end, and a failed job can still continue.
- **Revisit if.** Failed jobs fill disks before the reaper runs.

### I154. Worker matching is checked at lease time

- **Choice.** Dispatch is pull: a worker reads a pointer and asks for a lease, so "the scheduler chooses a compatible worker" (D40) means the lease answers `409` to a worker whose declared pool or resources do not satisfy the node's `requirements`, leaving the pointer for another. A worker with no registration also gets `409`. One live lease means one, even for the worker that holds it: two entries for a task can reach one worker, and only the first may run. A worker that lost the answer to its lease request waits for that lease to expire. Dry run warns when no registered worker could take a node. `scheduler.Matcher` keeps the registrations, backed by `repo.WorkerRepo`.
- **Why.** Fits the protocol of I4 with no second dispatch path. A push scheduler would duplicate the consumer groups.
- **Revisit if.** Heterogeneous fleets need placement beyond compatibility.

### I155. Schedule triggers

- **Choice.** Time is divided into slots of `every`, aligned to the Unix epoch. A workflow starts once per slot per tenant, with the idempotency key `schedule:<id>:<slot>`, so a restart in the same slot starts nothing. A slot missed while the daemon was down starts once on boot if the workflow has run before; a tenant that never ran it waits for the next boundary, so a fresh install does no maintenance work in its first seconds. Minimum `every` is 1 minute.
- **Why.** I22 gives `every` no semantics beyond a duration.
- **Revisit if.** Cron expressions arrive.

---

## Events and permissions

### I156. Event chains and delivery

- **Choice.** An event's `causation_depth` is its emitting job's `Trigger.Depth`, and a job started by an event of depth `d` has depth `d+1`; child jobs inherit it. Deliveries of an event deeper than the limit (`event.MaxCausationDepth`, configurable) are written `dropped` with the reason. A filter miss is `dropped` too. At most 32 subscribers per event are served; the rest are `dropped`, so a configuration mistake stays visible. A delivery is retried with backoff from 2 s to 5 min, 8 attempts, then `failed`. A workflow delivery starts a job keyed `event:<event_id>:<workflow>`. `event.emit` uses an event ID derived from its idempotency key through the additive `event.Draft.ID`, so a re-run emits nothing new. `causation_id` is the triggering event when there is one, else the job's ID.
- **Why.** §8.2 asks for limits, tracking and loop prevention with "e.g., 5" as the only number.
- **Revisit if.** Real fan-out exceeds the limit.

### I157. Permissions a job needs

- **Choice.** `workflow.RequiredPermissions` walks the definition and its subworkflows. `catalog.delete` needs `asset.delete`. `publish.withdraw` needs `publication.withdraw`. A literal `allowArchive: true` needs `storage.archive.restore`. `publish.prepare`, `publish.upload` and `publish.finalize` need `publication.create` unless an `approval.request` of kind `publication` dominates them in the submitted tree, because the publisher's decision is what authorizes them (D7). The submitter must hold everything, or the run answers `403`; the job records exactly that set in `Permissions`, and children inherit it. Event and schedule jobs get the set too, except `asset.delete` and `storage.archive.restore`, which only a person can start. Hooks returning parameters are checked against the job's permissions. A submitter holding `asset.ai_consent` passes it to the job too, because `catalog.register`'s `aiProcessing` input records that person's consent (D8).
- **Why.** §9.4: an imported workflow does not inherit all permissions. An editor must be able to run `create-clip`, whose publication step a publisher approves. Who may press "delete" is not something an event should decide.
- **Revisit if.** Plugins declare node permissions of their own.

### I158. Approval details beyond I41

- **Choice.** Approving a `clip_selection` keeps the evidence's `segments` unless `allow_adjust` is set and the request gives `segments`, or `start_seconds` and `end_seconds` for a single interval. Evidence marked `ambiguous: true` must be approved with an explicit choice, since §3.2 says not to cut at random. A selection with no segments cannot be approved. Cancelling a job marks its pending approvals `expired`. `onTimeout: remind` re-emits `approval.requested` with `reminder: true` each period. `fail` expires the record and fails the node. `reject` records a rejection by `system`. `approvers` is re-checked at decision time, and the decision is audited with the permission used.
- **Why.** I41 leaves these open, and D51 requires alternatives to be real choices.
- **Revisit if.** Reviewers want ambiguity accepted with one click.

---

## Workflow files

### I159. Files, inputs and helpers beyond the master plan's list

- **Choice.** Input names follow §6.3 and §6.4: `ingest-original` takes `file`, `title`, `category`, `create-clip` takes `source_id`, `prompt`, `publish-video` takes `assetId`, `destinations`, `requireApproval`, and `delete-asset` takes `asset_id` as `api` calls it (I132). Helpers added: `publish-private-first` and `publish-upload-private` (D37), `verify-asset` (one asset of `verify-storage`), `expire-render` (one asset of `expire-unapproved-renders`) and `purge-deleted-archives` with `purge-archive-object` (D17's deferred purge). Catalog entries added: `catalog.get_transcript` for `reindex-asset`, `catalog.list_purgeable_archives` for the purge. `ingest-original` omits `mediacms.register`, since §6.3 says it appears only for tenants that enabled it, and they override the file (I22, D24). `public-library-on-updated` has no filter: `catalog.public.sync` rebuilds from approved records, so a metadata change is reflected too, not only a visibility drop (R15). A derivative's backup is written at ingest, because R11 makes a verified backup part of readiness and §3.2 step 11 and §15.2 item 5 ask for it, although D16 says approved masters are what is archived; `expire-unapproved-renders` removes only the `master` copy (I45), so the archive keeps a rejected render until its class allows deleting it, which is flagged rather than resolved here.
- **Why.** `flow.foreach` takes a workflow, and §10 lists only the outer ones. The purge has no other home, and D17 states it as decided.
- **Revisit if.** The maintainer wants these named differently.
