package connector

import (
	"context"
	"net/http"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
)

// LogRecord is a structured log entry (§10.1). tenant_id is set on anything
// tenant-scoped (D6). Secrets are redacted before a sink sees a record.
type LogRecord struct {
	Timestamp  time.Time      `json:"timestamp"`
	Level      string         `json:"level"`
	Service    string         `json:"service"`
	Event      string         `json:"event"`
	Message    string         `json:"message,omitempty"`
	TenantID   string         `json:"tenant_id,omitempty"`
	JobID      string         `json:"job_id,omitempty"`
	WorkflowID string         `json:"workflow_id,omitempty"`
	NodeID     string         `json:"node_id,omitempty"`
	AssetID    string         `json:"asset_id,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Status     string         `json:"status,omitempty"`
	Fields     map[string]any `json:"fields,omitempty"`
}

// LogSink is a log destination. A failing sink must not affect others or block
// processing; the manager isolates each behind its own queue (§7.4).
type LogSink interface {
	Info() Info
	Write(ctx context.Context, batch []LogRecord) error
	Flush(ctx context.Context) error
	Close(ctx context.Context) error
}

// LogQuery selects records from a sink that keeps them locally. An empty TenantID
// means every record and is used only for daemon-level access; tenant-scoped
// reads always set it.
type LogQuery struct {
	TenantID string
	Since    time.Time // zero means no lower bound
	MinLevel string    // debug, info, warn, error; empty means all
	JobID    string
	Limit    int
	Cursor   string // opaque, from LogPage.NextCursor
}

// LogPage is one page, newest record first.
type LogPage struct {
	Records    []LogRecord
	NextCursor string
}

// LogQuerier is an optional capability of a LogSink that keeps records locally,
// used by GET /logs so logs stay available with no external sink (§10.1).
type LogQuerier interface {
	QueryLogs(ctx context.Context, q LogQuery) (LogPage, error)
}

// MetricPoint is a push exporter's unit. Value is a delta for a counter, the
// current value for a gauge and one observation for a histogram.
type MetricPoint struct {
	Name   string
	Kind   string // "counter", "gauge" or "histogram"
	Value  float64
	Labels map[string]string
	At     time.Time
}

// MetricsExporter receives metrics. A pull-based exporter such as Prometheus
// also implements MetricsHandler.
type MetricsExporter interface {
	Info() Info
	Export(ctx context.Context, batch []MetricPoint) error
	Close(ctx context.Context) error
}

// MetricsHandler is an optional capability served at GET /metrics.
type MetricsHandler interface {
	Handler() http.Handler
}

// SpanRecord is a finished span, independent of the OpenTelemetry SDK.
type SpanRecord struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	Start        time.Time
	End          time.Time
	Attributes   map[string]any
	Status       string
}

// TraceExporter receives finished spans.
type TraceExporter interface {
	Info() Info
	ExportSpans(ctx context.Context, spans []SpanRecord) error
	Close(ctx context.Context) error
}

// LogSinkFactory opens a sink from its configuration entry.
type LogSinkFactory func(ctx context.Context, cfg config.Sink, secrets SecretResolver) (LogSink, error)

// MetricsExporterFactory opens a metrics exporter.
type MetricsExporterFactory func(ctx context.Context, cfg config.Sink, secrets SecretResolver) (MetricsExporter, error)

// TraceExporterFactory opens a trace exporter.
type TraceExporterFactory func(ctx context.Context, cfg config.Sink, secrets SecretResolver) (TraceExporter, error)
