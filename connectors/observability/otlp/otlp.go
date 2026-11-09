package otlp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkinstr "go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rafaelp/vidyom/connectors/observability/internal/remote"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// Driver is this connector's name in observability.json.
const Driver = "otlp"

const (
	version         = "1.0.0"
	scopeName       = "github.com/rafaelp/vidyom"
	defaultInterval = 15 * time.Second
	defaultTimeout  = 10 * time.Second
)

// Parameters is the exporter's `parameters` object.
type Parameters struct {
	// Endpoint is the collector's base URL; /v1/traces and /v1/metrics are appended.
	Endpoint       string `json:"endpoint"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	// IntervalSeconds is the metrics push interval. Zero means 15.
	IntervalSeconds int `json:"intervalSeconds,omitempty"`
	// ServiceName is the service.name resource attribute. Empty means vidyomd.
	ServiceName string `json:"serviceName,omitempty"`
}

// Register adds the driver for both families.
func Register(r connector.Registrar) {
	r.RegisterMetricsExporter(infoFor(connector.FamilyMetricsExporter), MetricsFactory)
	r.RegisterTraceExporter(infoFor(connector.FamilyTraceExporter), TraceFactory)
}

func infoFor(family connector.Family) connector.Info {
	return connector.Info{
		Family:       family,
		Driver:       Driver,
		Version:      version,
		Available:    true,
		Capabilities: map[string]bool{"push": true},
	}
}

type settings struct {
	params   Parameters
	base     string
	headers  map[string]string
	timeout  time.Duration
	interval time.Duration
	resource *resource.Resource
}

func parse(ctx context.Context, cfg config.Sink, secrets connector.SecretResolver) (settings, error) {
	var params Parameters
	if len(cfg.Parameters) > 0 {
		if err := json.Unmarshal(cfg.Parameters, &params); err != nil {
			return settings{}, errs.Wrap(errs.CodeValidationFailed, err, "invalid otlp parameters")
		}
	}
	u, err := remote.ParseEndpoint(params.Endpoint, cfg.CredentialsRef != "")
	if err != nil {
		return settings{}, err
	}
	auth, err := remote.Authorization(ctx, secrets, cfg.CredentialsRef)
	if err != nil {
		return settings{}, err
	}
	s := settings{params: params, base: u.String(), timeout: defaultTimeout, interval: defaultInterval}
	if auth != "" {
		s.headers = map[string]string{"Authorization": auth}
	}
	if params.TimeoutSeconds > 0 {
		s.timeout = time.Duration(params.TimeoutSeconds) * time.Second
	}
	if params.IntervalSeconds > 0 {
		s.interval = time.Duration(params.IntervalSeconds) * time.Second
	}
	service := params.ServiceName
	if service == "" {
		service = "vidyomd"
	}
	s.resource = resource.NewSchemaless(attribute.String("service.name", service))
	return s, nil
}

// ---- Traces ----

// TraceExporter sends finished spans. A failed push is returned to the caller, whose
// pump retries, so the exporter's own retries are off.
type TraceExporter struct {
	exporter sdktrace.SpanExporter
	resource *resource.Resource
}

var _ connector.TraceExporter = (*TraceExporter)(nil)

// TraceFactory opens a trace exporter. Nothing is contacted yet.
func TraceFactory(ctx context.Context, cfg config.Sink, secrets connector.SecretResolver) (connector.TraceExporter, error) {
	s, err := parse(ctx, cfg, secrets)
	if err != nil {
		return nil, err
	}
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(s.base + "/v1/traces"),
		otlptracehttp.WithTimeout(s.timeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	}
	if s.headers != nil {
		opts = append(opts, otlptracehttp.WithHeaders(s.headers))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	return &TraceExporter{exporter: exp, resource: s.resource}, nil
}

// Info implements connector.TraceExporter.
func (e *TraceExporter) Info() connector.Info { return infoFor(connector.FamilyTraceExporter) }

// ExportSpans implements connector.TraceExporter.
func (e *TraceExporter) ExportSpans(ctx context.Context, spans []connector.SpanRecord) error {
	stubs := make(tracetest.SpanStubs, 0, len(spans))
	for i := range spans {
		stub, ok := e.stub(&spans[i])
		if ok {
			stubs = append(stubs, stub)
		}
	}
	if len(stubs) == 0 {
		return nil
	}
	return e.exporter.ExportSpans(ctx, stubs.Snapshots())
}

func (e *TraceExporter) stub(s *connector.SpanRecord) (tracetest.SpanStub, bool) {
	traceBytes, err := hex.DecodeString(s.TraceID)
	if err != nil || len(traceBytes) != 16 {
		return tracetest.SpanStub{}, false
	}
	spanBytes, err := hex.DecodeString(s.SpanID)
	if err != nil || len(spanBytes) != 8 {
		return tracetest.SpanStub{}, false
	}
	var traceID trace.TraceID
	var spanID trace.SpanID
	copy(traceID[:], traceBytes)
	copy(spanID[:], spanBytes)
	stub := tracetest.SpanStub{
		Name:                 s.Name,
		SpanContext:          trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled}),
		StartTime:            s.Start,
		EndTime:              s.End,
		Attributes:           attributes(s.Attributes),
		Status:               sdktrace.Status{Code: statusCode(s.Status), Description: statusDescription(s.Status)},
		Resource:             e.resource,
		InstrumentationScope: sdkinstr.Scope{Name: scopeName},
	}
	if parentBytes, err := hex.DecodeString(s.ParentSpanID); err == nil && len(parentBytes) == 8 {
		var parent trace.SpanID
		copy(parent[:], parentBytes)
		stub.Parent = trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: parent, TraceFlags: trace.FlagsSampled})
	}
	return stub, true
}

func statusCode(status string) codes.Code {
	switch status {
	case "Ok", "ok":
		return codes.Ok
	case "Error", "error":
		return codes.Error
	}
	return codes.Unset
}

func statusDescription(status string) string {
	if statusCode(status) == codes.Error {
		return status
	}
	return ""
}

func attributes(in map[string]any) []attribute.KeyValue {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]attribute.KeyValue, 0, len(in))
	for _, k := range keys {
		switch v := in[k].(type) {
		case string:
			out = append(out, attribute.String(k, v))
		case bool:
			out = append(out, attribute.Bool(k, v))
		case int:
			out = append(out, attribute.Int(k, v))
		case int64:
			out = append(out, attribute.Int64(k, v))
		case float64:
			out = append(out, attribute.Float64(k, v))
		default:
			data, err := json.Marshal(v)
			if err == nil {
				out = append(out, attribute.String(k, string(data)))
			}
		}
	}
	return out
}

// Close implements connector.TraceExporter.
func (e *TraceExporter) Close(ctx context.Context) error { return e.exporter.Shutdown(ctx) }

// ---- Metrics ----

// recordingExporter keeps what the periodic reader's pushes did, which the reader
// would otherwise only hand to a global error handler.
type recordingExporter struct {
	sdkmetric.Exporter
	failures atomic.Int64
	mu       sync.Mutex
	lastErr  string
}

func (r *recordingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := r.Exporter.Export(ctx, rm)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.failures.Add(1)
		r.lastErr = err.Error()
	} else {
		r.lastErr = ""
	}
	return err
}

// MetricsExporter feeds SDK instruments and lets a periodic reader push them.
//
// Export only records, so it cannot fail and a retry by the caller could never double
// count. The data is cumulative, so a push that fails loses nothing: the next one
// carries it. Push failures are reported through PushHealth.
type MetricsExporter struct {
	provider *sdkmetric.MeterProvider
	meter    metric.Meter
	pushes   *recordingExporter

	mu          sync.Mutex
	counters    map[string]metric.Float64Counter
	gauges      map[string]metric.Float64Gauge
	histograms  map[string]metric.Float64Histogram
	gaugeValues map[string]float64 // Add on a gauge needs its current value
}

var _ connector.MetricsExporter = (*MetricsExporter)(nil)

// MetricsFactory opens a metrics exporter. Nothing is contacted until the first push.
func MetricsFactory(ctx context.Context, cfg config.Sink, secrets connector.SecretResolver) (connector.MetricsExporter, error) {
	s, err := parse(ctx, cfg, secrets)
	if err != nil {
		return nil, err
	}
	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(s.base + "/v1/metrics"),
		otlpmetrichttp.WithTimeout(s.timeout),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
	}
	if s.headers != nil {
		opts = append(opts, otlpmetrichttp.WithHeaders(s.headers))
	}
	exp, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP metrics exporter: %w", err)
	}
	pushes := &recordingExporter{Exporter: exp}
	reader := sdkmetric.NewPeriodicReader(pushes, sdkmetric.WithInterval(s.interval), sdkmetric.WithTimeout(s.timeout))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(s.resource))
	return &MetricsExporter{
		provider:    provider,
		meter:       provider.Meter(scopeName),
		pushes:      pushes,
		counters:    map[string]metric.Float64Counter{},
		gauges:      map[string]metric.Float64Gauge{},
		histograms:  map[string]metric.Float64Histogram{},
		gaugeValues: map[string]float64{},
	}, nil
}

// Info implements connector.MetricsExporter.
func (e *MetricsExporter) Info() connector.Info { return infoFor(connector.FamilyMetricsExporter) }

// Export implements connector.MetricsExporter. See the type's comment.
func (e *MetricsExporter) Export(ctx context.Context, batch []connector.MetricPoint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range batch {
		p := &batch[i]
		attrs := make([]attribute.KeyValue, 0, len(p.Labels))
		keys := make([]string, 0, len(p.Labels))
		var seriesKey strings.Builder
		seriesKey.WriteString(p.Name)
		for k := range p.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			attrs = append(attrs, attribute.String(k, p.Labels[k]))
			seriesKey.WriteString(strconv.Quote(k + "=" + p.Labels[k]))
		}
		opt := metric.WithAttributes(attrs...)
		// The catalog's kind decides the instrument; the point's kind says how it arrived.
		arrived := telemetry.Kind(p.Kind)
		kind := arrived
		if spec, ok := telemetry.Spec(p.Name); ok {
			kind = spec.Kind
		}
		switch {
		case kind == telemetry.KindGauge && arrived == telemetry.KindGauge:
			if g, ok := e.gauge(p.Name); ok {
				e.gaugeValues[seriesKey.String()] = p.Value
				g.Record(ctx, p.Value, opt)
			}
		case kind == telemetry.KindGauge && arrived == telemetry.KindCounter:
			if g, ok := e.gauge(p.Name); ok {
				e.gaugeValues[seriesKey.String()] += p.Value
				g.Record(ctx, e.gaugeValues[seriesKey.String()], opt)
			}
		case kind == telemetry.KindCounter && arrived == telemetry.KindCounter && p.Value >= 0:
			if c, ok := e.counter(p.Name); ok {
				c.Add(ctx, p.Value, opt)
			}
		case kind == telemetry.KindHistogram && arrived == telemetry.KindHistogram:
			if h, ok := e.histogram(p.Name); ok {
				h.Record(ctx, p.Value, opt)
			}
		}
	}
	return nil
}

func (e *MetricsExporter) counter(name string) (metric.Float64Counter, bool) {
	if c, ok := e.counters[name]; ok {
		return c, true
	}
	c, err := e.meter.Float64Counter(name)
	if err != nil {
		return nil, false
	}
	e.counters[name] = c
	return c, true
}

func (e *MetricsExporter) gauge(name string) (metric.Float64Gauge, bool) {
	if g, ok := e.gauges[name]; ok {
		return g, true
	}
	g, err := e.meter.Float64Gauge(name)
	if err != nil {
		return nil, false
	}
	e.gauges[name] = g
	return g, true
}

func (e *MetricsExporter) histogram(name string) (metric.Float64Histogram, bool) {
	if h, ok := e.histograms[name]; ok {
		return h, true
	}
	h, err := e.meter.Float64Histogram(name, metric.WithExplicitBucketBoundaries(telemetry.DurationBuckets...))
	if err != nil {
		return nil, false
	}
	e.histograms[name] = h
	return h, true
}

// PushHealth reports push failures so far and the last push's error, for the manager
// to show; an empty error means the last push worked.
func (e *MetricsExporter) PushHealth() (failures int64, lastError string) {
	e.pushes.mu.Lock()
	defer e.pushes.mu.Unlock()
	return e.pushes.failures.Load(), e.pushes.lastErr
}

// TestExport pushes now and returns what happened, for `observability test`.
func (e *MetricsExporter) TestExport(ctx context.Context) error {
	return e.provider.ForceFlush(ctx)
}

// Close implements connector.MetricsExporter, with a last push.
func (e *MetricsExporter) Close(ctx context.Context) error { return e.provider.Shutdown(ctx) }
