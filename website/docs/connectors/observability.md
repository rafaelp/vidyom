---
title: "Observability connectors"
sidebar_position: 1
---

# Observability connectors

Logs, metrics, and traces use pluggable connectors rather than embedding vendor SDK assumptions into every workflow. OpenTelemetry provides the instrumentation conventions, but the daemon operates without an external monitoring stack. By default it writes local JSONL logs and exposes a Prometheus metrics endpoint; Loki, Tempo and Grafana are an optional Docker Compose profile. A Datadog connector is not supported yet.

## Interfaces

| Interface | Purpose | Suggested drivers |
| --- | --- | --- |
| `LogSink` | Structured event/log delivery | Local JSONL, Loki, Syslog, Google Cloud Logging |
| `MetricsExporter` | Time-series metrics | Prometheus, OTLP |
| `TraceExporter` | Distributed tracing | OTLP |

A plugin may provide all three interfaces. Multiple destinations may be active simultaneously.

## Configuration example

```json
{
  "observability": {
    "logs": {
      "level": "info",
      "sinks": [
        {"id": "local", "driver": "file", "enabled": true,
         "parameters": {"path": "/var/log/vidyom/events.jsonl", "retentionDays": 30}},
        {"id": "loki", "driver": "loki", "enabled": true,
         "credentialsRef": "loki-auth", "parameters": {"url": "https://loki.example.com"}}
      ]
    },
    "metrics": {"exporter": "prometheus", "listen": "127.0.0.1:9090"},
    "tracing": {"exporter": "otlp", "endpoint": "http://127.0.0.1:4318"}
  }
}
```

## Reliability

Operational logs are kept 30 days and workflow execution details 90 days by default, both configurable. Exports run asynchronously through bounded per-connector queues with batching, timeouts, retries, backoff and disk buffering when configured. One unavailable sink does not interrupt media processing or another sink. The daemon exposes exporter health, drops and backlogs. Required audit records are never silently discarded: audit persistence is distinct from best-effort telemetry exports.

The audit is an append-only table in each tenant database with **hash chaining** (each record includes the previous record's hash). The chain is exported daily to archive storage, and minimum retention is 1 year, configurable.

## Security

Apply secret redaction *before fan-out*. Avoid logging raw captions, private transcripts, signed media URLs, tokens, or personal data unless explicitly needed and authorized. High-cardinality asset/job IDs belong in structured logs and traces, not unbounded metric labels.
