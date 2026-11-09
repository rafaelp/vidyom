---
title: "JSON workflow engine"
sidebar_position: 1
---

# JSON workflow engine

**Decided:** Workflows are JSON configuration, modeled after n8n's concepts of nodes, parameters, connections and workflow settings. The runtime must support creating new workflows **without writing code** when all required node types already exist. Vidyom is *not* required to execute n8n JSON directly in the first release.

## Workflow contract (proposed)

- `id`, `name`, semantic `version`, optional `description`.
- JSON-schema-compatible `inputs` and `outputs`.
- `nodes[]`: stable node ID, `type`, `typeVersion`, `parameters`, optional credentials and hooks.
- `connections`: explicit DAG edges and branches.
- `settings`: retries, timeouts, progress, concurrency, error handling.
- `hooks` and optional event trigger configuration.
- Data references such as `${inputs.title}`, `${nodes.transcribe.output.file}` and `${event.field}` resolved by a **restricted expression system**, not arbitrary JavaScript or shell evaluation. `${...}` is used only for data references.
- Conditions (`flow.if`, event trigger filters, approval rules) written in **CEL** (Common Expression Language).

## Example: minimal ingest graph

```json
{
  "id": "ingest-original",
  "name": "Ingest Original",
  "version": "1.0.0",
  "inputs": {
    "file": {"type": "file", "required": true},
    "title": {"type": "string", "required": true},
    "category": {"type": "string", "default": "other"}
  },
  "nodes": [
    {"id": "register", "type": "catalog.register", "typeVersion": 1,
     "parameters": {"kind": "original", "title": "${inputs.title}"}},
    {"id": "backup", "type": "storage.put", "typeVersion": 1,
     "parameters": {"storage": "archive", "source": "${inputs.file}", "verifyChecksum": true}},
    {"id": "compress", "type": "video.transcode", "typeVersion": 1,
     "parameters": {"source": "${inputs.file}", "profile": "master-1080p"}},
    {"id": "storeMaster", "type": "storage.put", "typeVersion": 1,
     "parameters": {"storage": "master", "source": "${nodes.compress.output.file}"}},
    {"id": "transcribe", "type": "ai.transcribe", "typeVersion": 1,
     "parameters": {"source": "${inputs.file}", "language": "pt"}},
    {"id": "index", "type": "ai.index", "typeVersion": 1,
     "parameters": {"transcript": "${nodes.transcribe.output}"}}
  ],
  "connections": {
    "register": ["backup", "compress", "transcribe"],
    "compress": ["storeMaster"],
    "transcribe": ["index"]
  },
  "settings": {"retry": 3, "resumeOnFailure": true}
}
```

This is intentionally a **partial example**, not the full production ingest workflow. The complete workflow additionally includes proxy creation, stories/chapters, catalog synchronization, checksums and readiness gating.

## Conditions

**Decided:** Conditions are written in CEL (Common Expression Language): side-effect free, not Turing-complete and subject to evaluation cost limits. The Go daemon evaluates them with cel-go. Data references stay in `${...}` form, while conditions read the same values as plain CEL variables.

```json
{"id": "publishIfApproved", "type": "flow.if",
 "parameters": {"condition": "nodes.review.output.decision == 'approved' && inputs.visibility == 'public'"}}
```

## Merge and branching

**Proposed:** A node runs when all of its required predecessors have completed. Nodes on a branch not taken by `flow.if` are marked `skipped`, and the skip propagates to their descendants unless they are joined through `flow.merge`. `flow.merge` has `mode: all | any`: `all` waits for every non-skipped input, and `any` continues with the first completed input.

## Subworkflows

`workflow.execute` can call a separately versioned workflow such as `ingest-derived` or `publish-video`. The engine must validate parent-child invocation, authorization, input mapping, idempotency and recursion limits. Workflow versions are immutable for submitted jobs, so an edit does not silently alter running jobs.

## Scheduling and errors

Execution is dependency driven: independent nodes may run in parallel, while joins wait for all required predecessors. Built-in node retry must be bounded with backoff and idempotency. `--dry-run` validates the graph, parameters, permissions and connector capabilities without performing external side effects.

### Side-effect classes

**Decided:** Every node type declares a side-effect class that controls retry:

| Class | Retry behavior |
| --- | --- |
| `none` | Pure; retried freely. |
| `idempotent` | Retried with its idempotency key. |
| `reconcilable` | Must query the remote state before any retry. |
| `unsafe` | Never retried automatically; the job moves to the `needs_attention` state until a person resolves it. |

A checkpoint is persisted after each completed node. Cancellation is cooperative between nodes: the running node finishes, or aborts if it supports aborting.
