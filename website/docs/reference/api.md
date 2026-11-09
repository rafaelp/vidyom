---
title: "Daemon API and protocol"
sidebar_position: 1
---

# Daemon API and protocol (proposed)

The daemon must expose a stable, versioned client API over local Unix domain sockets, SSH tunneling or HTTPS. Requests and responses are structured JSON; long-running job and event streams may use an authenticated streaming protocol such as SSE/WebSocket or gRPC streaming.

## Endpoint concepts

| Method | Endpoint | Purpose |
| --- | --- | --- |
| `GET` | `/v1/health` | Health and readiness |
| `GET` | `/v1/workflows` | List permitted workflows |
| `POST` | `/v1/workflows/{id}/run` | Submit a version-pinned job |
| `GET` | `/v1/jobs/{jobId}` | Current state and progress |
| `GET` | `/v1/jobs/{jobId}/events` | Stream progress |
| `POST` | `/v1/jobs/{jobId}/cancel` | Request cancellation |
| `POST` | `/v1/assets/uploads` | Initiate resumable client upload |
| `GET` | `/v1/assets/{assetId}` | Canonical metadata |
| `GET` | `/v1/connectors` | Registered connector capabilities |
| `GET` | `/v1/plugins` | Installed plugin metadata |
| `GET` | `/v1/audit` | Authorized audit history |

These paths are **design suggestions**, not published live endpoints. API versioning must protect old CLI clients against silent behavioral changes; mutating requests use idempotency keys; every method enforces resource-level authorization.

## Example job submission

```json
{
  "workflow": "create-clip",
  "workflowVersion": "1.0.0",
  "inputs": {
    "source_id": "ORI-001",
    "prompt": "The story about the friend who never received a hug"
  },
  "idempotencyKey": "user-generated-stable-key"
}
```

## Example response

```json
{
  "job_id": "JOB-002",
  "status": "queued",
  "workflow": "create-clip",
  "workflowVersion": "1.0.0"
}
```

## Transport semantics

Local Unix sockets depend on OS file permissions and peer identity checks. Remote HTTPS requires TLS 1.3 with properly validated server identity and configured client authentication. SSH can tunnel the local daemon socket without exposing a public API port. Private overlay networks add a network boundary but do not replace API authorization.
