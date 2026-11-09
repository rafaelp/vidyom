---
title: "Observability connectors"
sidebar_position: 1
---

# Observability connectors

**Decided:** Logs, metrics, and traces must use pluggable connectors rather than embedding vendor SDK assumptions into every workflow. OpenTelemetry provides the intended instrumentation conventions, but the daemon operates without an external monitoring stack. **Decided:** by default it writes local JSONL logs and exposes a Prometheus metrics endpoint; Loki, Tempo and Grafana are an optional Docker Compose profile. A Datadog connector is **Future**.

## Interfaces

| Interface | Purpose | Suggested drivers |
| --- | --- | --- |
| `LogSink` | Structured event/log delivery | Local JSONL, Loki, Datadog (**Future**), Syslog, Google Cloud Logging |
| `MetricsExporter` | Time-series metrics | Prometheus, OTLP, Datadog |
| `TraceExporter` | Distributed tracing | OTLP, Datadog APM |

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
         "credentialsRef": "loki-auth", "parameters": {"url": "https://loki.example.com"}},
        {"id": "datadog", "driver": "datadog", "enabled": false,
         "credentialsRef": "datadog-api-key", "parameters": {"site": "datadoghq.com"}}
      ]
    },
    "metrics": {"exporter": "prometheus", "listen": "127.0.0.1:9090"},
    "tracing": {"exporter": "otlp", "endpoint": "http://127.0.0.1:4318"}
  }
}
```

## Reliability

**Decided:** Operational logs are kept 30 days and workflow execution details 90 days by default, both configurable. Export asynchronously through bounded per-connector queues with batching, timeouts, retries, backoff and disk buffering when configured. One unavailable sink must not interrupt media processing or another sink. The daemon should expose exporter health, drops and backlogs. Never silently discard required audit records: audit persistence is distinct from best-effort telemetry exports.

**Decided:** The audit is an append-only table in each tenant database with **hash chaining** (each record includes the previous record's hash). The chain is exported daily to archive storage, and minimum retention is 1 year, configurable.

## Security

Apply secret redaction *before fan-out*. Avoid logging raw captions, private transcripts, signed media URLs, tokens, or personal data unless explicitly needed and authorized. High-cardinality asset/job IDs belong in structured logs and traces, not unbounded metric labels.
