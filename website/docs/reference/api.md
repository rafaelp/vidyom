---
title: "Daemon API and protocol"
sidebar_position: 1
---

# Daemon API and protocol (proposed)

The daemon must expose a stable, versioned client API over local Unix domain sockets or HTTPS (TLS 1.3), which is the default remote transport. SSH tunneling remains available only for host operators. Requests and responses are structured JSON; long-running job and event streams may use an authenticated streaming protocol such as SSE/WebSocket or gRPC streaming.

## Endpoint concepts

| Method | Endpoint | Purpose |
| --- | --- | --- |
| `GET` | `/v1/health` | Health and readiness |
| `GET` | `/v1/workflows` | List permitted workflows |
| `POST` | `/v1/workflows/{id}/run` | Submit a version-pinned job |
| `GET` | `/v1/jobs/{jobId}` | Current state and progress |
| `GET` | `/v1/jobs/{jobId}/events` | Stream progress |
| `POST` | `/v1/jobs/{jobId}/cancel` | Request cancellation |
| `POST` | `/v1/jobs/{jobId}/resolve` | Resolve a `needs_attention` job |
| `POST` | `/v1/pairing/codes` | Admin generates a one-time pairing code |
| `POST` | `/v1/pairing/exchange` | Exchange a pairing code for a per-device credential |
| `GET` | `/v1/devices` | List paired devices |
| `DELETE` | `/v1/devices/{deviceId}` | Revoke a device credential |
| `POST` | `/v1/devices/current/rotate` | Rotate the calling device credential |
| `GET` | `/v1/tenants` | Tenants the caller belongs to |
| `GET` | `/v1/budget` | Budget and usage of the active tenant |
| `POST` | `/v1/assets/uploads` | Initiate resumable client upload |
| `GET` | `/v1/assets/{assetId}` | Canonical metadata |
| `GET` | `/v1/connectors` | Registered connector capabilities |
| `GET` | `/v1/plugins` | Installed plugin metadata |
| `GET` | `/v1/audit` | Authorized audit history |

These paths are **design suggestions**, not published live endpoints. API versioning must protect old CLI clients against silent behavioral changes; mutating requests use idempotency keys; every method enforces resource-level authorization.

## Tenant scoping

**Decided:** Vidyom is multi-tenant, so every resource request runs in one tenant. The client selects it with a tenant identifier (for example an `X-Vidyom-Tenant` header, **Proposed**), and the daemon checks that the caller's role in that tenant allows the operation. IDs such as `ORI-001` and `JOB-002` are only unique within a tenant, and a request can never read or write another tenant's data.

## Pairing

**Decided:** Remote clients enroll through pairing. An admin creates a one-time code; the CLI exchanges it at `/v1/pairing/exchange` for a per-device credential that can be revoked and is rotated. **Proposed:** the exchange is the only unauthenticated mutating call, and it accepts each code once.

## Jobs that need attention

**Decided:** A node with an `unsafe` side-effect class is never retried automatically. The job moves to the `needs_attention` state until a person resolves it, and `GET /v1/jobs/{jobId}` reports that state with the node and reason. Resolution options (**Proposed**) are to retry, skip or cancel.

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

Local Unix sockets depend on OS file permissions and peer identity checks. Remote HTTPS requires TLS 1.3 with properly validated server identity and a revocable per-device credential obtained through pairing. SSH can tunnel the local daemon socket for host operators without exposing a public API port. Private overlay networks add a network boundary but do not replace API authorization.
