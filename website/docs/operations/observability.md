---
title: "Logs, metrics, tracing, and audits"
sidebar_position: 1
---

# Logs, metrics, tracing, and audits

**Decided:** All daemon, workers, plugins, connectors and workflows emit structured telemetry. Observability uses provider-neutral interfaces, with support for local JSONL, Loki, Datadog, Prometheus and OTLP.

## Correlation

Every trace and operational event should carry the relevant `job_id`, `workflow_id`, `node_id`, `asset_id` and `trace_id`, plus safe timestamps, severity, service, attempt number, elapsed time and outcome. Avoid promoting unbounded IDs to metric labels.

```json
{
  "timestamp": "2026-11-08T12:00:00Z",
  "level": "info",
  "service": "vidyom-worker",
  "event": "video.transcode.completed",
  "job_id": "JOB-001",
  "workflow_id": "ingest-original",
  "node_id": "compress",
  "asset_id": "ORI-001",
  "duration_ms": 428000,
  "input_bytes": 10737418240,
  "output_bytes": 1258291200,
  "status": "success"
}
```

## Metrics

- Jobs by state and age; job and node duration histograms.
- Failures/retries by connector, workflow and node type.
- CPU, GPU, memory and temporary storage availability.
- Storage transfer bytes and request failures.
- Transcription duration, processing model, API usage/cost estimates.
- Per-tenant monthly spend against budget, recorded per operation.
- Jobs in `needs_attention`.
- Assets missing verified archive backup or direct-access master.
- Clips waiting for editorial approval.
- Authentication failures and suspicious operation rates.
- Telemetry exporter queue length, retry rate and dropped-event count.

## Stack

**Decided:** Instrumentation uses OpenTelemetry. By default the daemon writes local JSONL logs and exposes a Prometheus metrics endpoint. Loki, Tempo and Grafana are an optional Docker Compose profile. **Future:** a Datadog connector. Syslog and Cloud Logging can be connector alternatives. The daemon **must not require** a monitoring cluster to operate.

## Data retention

**Decided:** Retention is configurable, with these defaults.

| Class | Default retention |
| --- | --- |
| Operational logs | 30 days |
| Workflow execution details | 90 days |
| Security audit | At least 1 year (configurable) |

An unavailable external log destination must never block FFmpeg processing, but audit events must use a durable store, not an unbounded volatile queue. Redact secrets, private signed URLs and sensitive transcript text before export.

## Audit

**Decided:** Each tenant database holds an append-only audit table with hash chaining: each record includes the previous record's hash. The chain is exported daily to archive storage. Minimum retention is 1 year, configurable. A deletion keeps the fact of the deletion in the audit, without the content.

## Cost budgets and alerts

**Decided:** Each tenant has a monthly estimated budget covering storage operations and egress, external AI, transcription and publishing. Vidyom alerts at 80% and blocks new paid operations at 100%, with an admin override. Usage is recorded per operation.
