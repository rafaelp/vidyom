---
title: "Plugins and lifecycle hooks"
sidebar_position: 1
---

# Plugins, hooks, and events

**Decided:** Provide synchronous lifecycle hooks (`before`, `after`, `finish`, `error`, `cancel`, node-level equivalents) and an asynchronous durable Event Bus. Hooks and events are intentionally separate: a hook may block or validate an action; an event informs consumers after a state transition is committed.

## Hook lifecycle

| Hook | When | May block? |
| --- | --- | --- |
| `workflow.before` | Before workflow start | Yes, for validation and authorization |
| `workflow.start` | Workflow starts | No |
| `node.before` | Before a node | Yes, within approved policy |
| `node.after` | Node succeeds | Limited |
| `node.error` | Node fails | Defined recovery policy only |
| `node.finish` | Node attempt terminates | No |
| `workflow.after` | Successful workflow completion | Limited |
| `workflow.finish` | Any terminal outcome | No |
| `workflow.error` | Workflow failure | Defined policy only |
| `workflow.cancel` | Cancellation requested | No arbitrary veto |

`finish` is a durable finalization task designed to run even after process restart; not a guarantee under irrecoverable infrastructure failure.

## Hook scopes and order

Global policy hooks apply to all workflows and must not be disabled by untrusted JSON. Workflow hooks apply to one workflow version, and node hooks apply at an individual step. Proposed order: global → workflow → node for `before`, and reverse for `after` / `finish`. Persistent security/audit policy supersedes plugin requests.

## Example

```json
{
  "hooks": {
    "before": [
      {"id": "validate-source", "plugin": "asset-validator",
       "action": "check-ready-for-clipping", "timeoutSeconds": 15,
       "onFailure": "abort"}
    ],
    "after": [
      {"id": "notify-review", "plugin": "notifications",
       "action": "send-review-notification", "onFailure": "continue"}
    ],
    "finish": [
      {"id": "record-metrics", "plugin": "analytics",
       "action": "record-execution", "onFailure": "continue"}
    ]
  }
}
```

## Event Bus

Events such as `original.indexed`, `asset.created`, `clip.created`, `publication.completed`, `workflow.failed`, or `storage.backup.verified` may trigger plugins, webhooks and other JSON workflows. Use a transactional outbox, durable subscriptions, delivery state, retry/backoff, recursion limits and consumer idempotency keys. Model delivery as **at least once**, not exactly once.

**Decided:** Event trigger filters are written in CEL (Common Expression Language), as are conditions in `flow.if` and approval rules. CEL is side-effect free, not Turing-complete and bound by evaluation cost limits; the Go daemon evaluates it with cel-go. The `${...}` syntax is reserved for data references such as `${event.asset_id}`.

```json
{
  "id": "update-public-library",
  "trigger": {
    "type": "event",
    "event": "publication.completed",
    "filter": "event.payload.visibility == 'public'"
  },
  "nodes": [
    {"id": "sync", "type": "catalog.public.sync", "typeVersion": 1,
     "parameters": {"assetId": "${event.asset_id}"}}
  ]
}
```

## Plugin execution and packaging

**Decided:** Out-of-process plugins talk to the daemon over **JSON-RPC 2.0 on stdin/stdout** (the same transport model as LSP and MCP), which is language-neutral and needs no code generation. Remote plugins and services use JSON-RPC over authenticated HTTPS. Native Go `.so` plugins remain excluded. **Future:** WASM for specific sandboxed cases.

Plugins may be separate Go or Python executables or remote services. Each manifest describes protocol version, actions, input/output schemas, hook subscriptions, capabilities, resource quotas, permissions and compatible daemon versions. Installing a plugin never grants unrestricted keyring, filesystem, network or shell access.

Creating a new **workflow** requires JSON when node types exist; creating a new **node or connector** generally requires writing and distributing an implementation plugin.
