package observability

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// Handler returns a slog.Handler that feeds the manager. Components log with log/slog
// (package telemetry) and carry §10.1's fields as attributes: tenant_id, job_id,
// workflow_id, node_id, asset_id, trace_id, duration (or duration_ms), status and
// event. Anything else goes to Fields. It never blocks on a sink.
func (m *Manager) Handler() slog.Handler { return &logHandler{m: m} }

// Logger returns a logger on Handler.
func (m *Manager) Logger() *slog.Logger { return slog.New(m.Handler()) }

type logHandler struct {
	m      *Manager
	attrs  []slog.Attr // flattened with group prefixes
	prefix string
}

func (h *logHandler) Enabled(_ context.Context, level slog.Level) bool {
	return int64(level) >= h.m.minLevel.Load()
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := *h
	out.attrs = append(append([]slog.Attr(nil), h.attrs...), flatten(h.prefix, attrs)...)
	return &out
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	out := *h
	out.prefix = h.prefix + name + "."
	return &out
}

func flatten(prefix string, attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		value := a.Value.Resolve()
		if value.Kind() == slog.KindGroup {
			nested := prefix
			if a.Key != "" {
				nested = prefix + a.Key + "."
			}
			out = append(out, flatten(nested, value.Group())...)
			continue
		}
		if a.Key == "" {
			continue
		}
		out = append(out, slog.Attr{Key: prefix + a.Key, Value: value})
	}
	return out
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := connector.LogRecord{
		Timestamp: r.Time,
		Level:     levelName(r.Level),
		Message:   r.Message,
	}
	apply := func(a slog.Attr) {
		applyAttr(&rec, a)
	}
	for _, a := range h.attrs {
		apply(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		for _, f := range flatten(h.prefix, []slog.Attr{a}) {
			apply(f)
		}
		return true
	})
	if rec.TraceID == "" {
		if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
			rec.TraceID = sc.TraceID().String()
		}
	}
	h.m.Emit(ctx, rec)
	return nil
}

func applyAttr(rec *connector.LogRecord, a slog.Attr) {
	text := a.Value.String
	switch a.Key {
	case "tenant_id", "tenant":
		rec.TenantID = text()
	case "job_id":
		rec.JobID = text()
	case "workflow_id":
		rec.WorkflowID = text()
	case "node_id":
		rec.NodeID = text()
	case "asset_id":
		rec.AssetID = text()
	case "trace_id":
		rec.TraceID = text()
	case "status":
		rec.Status = text()
	case "event":
		rec.Event = text()
	case "service":
		rec.Service = text()
	case "duration_ms":
		if a.Value.Kind() == slog.KindInt64 {
			rec.DurationMS = a.Value.Int64()
			return
		}
		fallthrough
	case "duration":
		if a.Value.Kind() == slog.KindDuration {
			rec.DurationMS = a.Value.Duration().Milliseconds()
			return
		}
		fallthrough
	default:
		if rec.Fields == nil {
			rec.Fields = map[string]any{}
		}
		rec.Fields[a.Key] = attrValue(a.Value)
	}
}

func attrValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	default:
		return v.Any()
	}
}
