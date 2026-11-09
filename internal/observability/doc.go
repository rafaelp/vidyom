// Package observability is the Observability Manager (§10.1, D38): structured logs
// through log/slog, metrics through telemetry.Metrics and spans through OpenTelemetry,
// with secret redaction and fan-out to pluggable exporters (§7.4).
//
// Each exporter sits behind its own bounded queue and goroutine (pump), so a dead Loki
// never blocks processing or another exporter (§15.7).
//
// Owner: component extensibility (docs/plans/00-master-plan.md, section 5).
package observability
