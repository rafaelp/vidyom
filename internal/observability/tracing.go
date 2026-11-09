package observability

import (
	"context"
	"crypto/rand"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// tracerName is the instrumentation scope of spans started by these helpers.
const tracerName = "github.com/rafaelp/vidyom"

var propagator = propagation.TraceContext{}

// newTracerProvider returns a provider that always samples, whether or not an exporter
// is configured, because trace_id is needed in logs and events anyway (I278).
func newTracerProvider(m *Manager) *sdktrace.TracerProvider {
	return sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", m.service))),
		sdktrace.WithSpanProcessor(&spanFanout{m: m}),
	)
}

// TracerProvider returns the SDK provider, for components that take one explicitly.
func (m *Manager) TracerProvider() trace.TracerProvider { return m.tracer }

// InstallGlobals sets the provider and the W3C propagator as the OpenTelemetry globals,
// for components that use the API directly. Call it once, from wiring.
func (m *Manager) InstallGlobals() {
	otel.SetTracerProvider(m.tracer)
	otel.SetTextMapPropagator(propagator)
}

// spanFanout converts finished spans to connector.SpanRecord and hands them to pumps.
type spanFanout struct{ m *Manager }

func (f *spanFanout) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (f *spanFanout) Shutdown(context.Context) error                  { return nil }
func (f *spanFanout) ForceFlush(context.Context) error                { return nil }

func (f *spanFanout) OnEnd(s sdktrace.ReadOnlySpan) {
	m := f.m
	if m.closed.Load() {
		return
	}
	attrs := make(map[string]any, len(s.Attributes()))
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	record := connector.SpanRecord{
		TraceID:    s.SpanContext().TraceID().String(),
		SpanID:     s.SpanContext().SpanID().String(),
		Name:       m.redactor.String(s.Name()),
		Start:      s.StartTime(),
		End:        s.EndTime(),
		Attributes: m.redactor.Fields(attrs),
		Status:     s.Status().Code.String(),
	}
	if s.Parent().HasSpanID() {
		record.ParentSpanID = s.Parent().SpanID().String()
	}
	m.daemon.Load().pushSpan(record)
	if tenant, ok := attrs[telemetry.LabelTenant].(string); ok && tenant != "" {
		if p := m.tenantPipe(domain.TenantID(tenant)); p != nil {
			p.pushSpan(record)
		}
	}
}

// StartSpan starts a span on the global provider. attrs carry tenant_id, job_id and so
// on as string pairs; a value holding a secret is redacted before export.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartJobSpan starts a span in a job's trace, on any process that holds its trace ID
// (§10.1). A job crosses processes and lives longer than any request, so there is no
// parent span in hand, only the ID it was given when it was submitted.
func StartJobSpan(ctx context.Context, traceID, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if id, err := trace.TraceIDFromHex(traceID); err == nil {
		var parent trace.SpanID
		if _, err := rand.Read(parent[:]); err == nil {
			ctx = trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: id, SpanID: parent, TraceFlags: trace.FlagsSampled, Remote: true,
			}))
		}
	}
	return otel.Tracer(tracerName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// EndSpan records err, if any, and ends the span.
func EndSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "error")
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// TraceID returns the context's trace ID, or an empty string.
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

// ExtractTraceParent continues a trace from a W3C traceparent header (§10.1, I15).
func ExtractTraceParent(ctx context.Context, h http.Header) context.Context {
	return propagator.Extract(ctx, propagation.HeaderCarrier(h))
}

// InjectTraceParent writes the context's trace to a header.
func InjectTraceParent(ctx context.Context, h http.Header) {
	propagator.Inject(ctx, propagation.HeaderCarrier(h))
}

// TraceMiddleware starts a server span for each request, continuing the caller's
// trace, so CLI, API and daemon share one trace (§10.1).
func TraceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := ExtractTraceParent(r.Context(), r.Header)
		ctx, span := otel.Tracer(tracerName).Start(ctx, r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)),
		)
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TracingTransport sends the context's trace with each request, for clients.
type TracingTransport struct {
	// Base is the real transport. Nil means http.DefaultTransport.
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper. It clones the request, as the contract asks.
func (t TracingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	clone := r.Clone(r.Context())
	InjectTraceParent(r.Context(), clone.Header)
	return base.RoundTrip(clone)
}

// ObserveDuration returns a function that records the elapsed seconds on a histogram,
// for the §10.3 duration metrics: defer observability.ObserveDuration(m, name, labels)().
func ObserveDuration(m telemetry.Metrics, name string, labels telemetry.Labels) func() {
	start := time.Now()
	return func() { m.Observe(name, time.Since(start).Seconds(), labels) }
}
