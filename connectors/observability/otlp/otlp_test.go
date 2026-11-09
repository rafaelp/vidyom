package otlp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// mockCollector decodes real OTLP protobuf. It is not a real collector (I12).
type mockCollector struct {
	mu      sync.Mutex
	traces  []*coltracepb.ExportTraceServiceRequest
	metrics []*colmetricpb.ExportMetricsServiceRequest
	auth    []string
	status  int
}

func (m *mockCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auth = append(m.auth, r.Header.Get("Authorization"))
	if m.status != 0 {
		w.WriteHeader(m.status)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var resp proto.Message
	switch r.URL.Path {
	case "/v1/traces":
		req := &coltracepb.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.traces = append(m.traces, req)
		resp = &coltracepb.ExportTraceServiceResponse{}
	case "/v1/metrics":
		req := &colmetricpb.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(body, req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.metrics = append(m.metrics, req)
		resp = &colmetricpb.ExportMetricsServiceResponse{}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	data, _ := proto.Marshal(resp)
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Write(data)
}

func sinkConfig(endpoint, credentialsRef string) config.Sink {
	data, _ := json.Marshal(Parameters{Endpoint: endpoint, IntervalSeconds: 3600, ServiceName: "vidyomd-test"})
	return config.Sink{ID: "otlp", Driver: Driver, Enabled: true, CredentialsRef: credentialsRef, Parameters: data}
}

type secrets map[string]string

func (s secrets) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	v, ok := s[ref]
	if !ok {
		return nil, errors.New("no such secret")
	}
	return []byte(v), nil
}

func TestExportSpansSendsRealOTLP(t *testing.T) {
	mock := &mockCollector{}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	exp, err := TraceFactory(context.Background(), sinkConfig(srv.URL, "otlp-auth"), secrets{"otlp-auth": "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close(context.Background())

	now := time.Now()
	spans := []connector.SpanRecord{
		{
			TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", ParentSpanID: "fedcba9876543210",
			Name: "node.execute", Start: now, End: now.Add(time.Second), Status: "Ok",
			Attributes: map[string]any{"tenant_id": "acme", "attempt": 2, "retried": true, "cost": 1.5, "tags": []string{"a"}},
		},
		{TraceID: "not hex", SpanID: "0123456789abcdef", Name: "invalid is skipped"},
	}
	if err := exp.ExportSpans(context.Background(), spans); err != nil {
		t.Fatal(err)
	}
	if len(mock.traces) != 1 {
		t.Fatalf("requests: %d", len(mock.traces))
	}
	if mock.auth[0] != "Bearer abc123" {
		t.Errorf("auth: %q", mock.auth[0])
	}
	rs := mock.traces[0].ResourceSpans[0]
	var service string
	for _, a := range rs.Resource.Attributes {
		if a.Key == "service.name" {
			service = a.Value.GetStringValue()
		}
	}
	if service != "vidyomd-test" {
		t.Errorf("service.name: %q", service)
	}
	got := rs.ScopeSpans[0].Spans
	if len(got) != 1 || got[0].Name != "node.execute" {
		t.Fatalf("spans: %+v", got)
	}
	attrs := map[string]string{}
	for _, a := range got[0].Attributes {
		attrs[a.Key] = a.Value.String()
	}
	if !strings.Contains(attrs["tenant_id"], "acme") || !strings.Contains(attrs["attempt"], "2") || !strings.Contains(attrs["tags"], `[\"a\"]`) {
		t.Errorf("attributes: %v", attrs)
	}
	if len(got[0].ParentSpanId) != 8 {
		t.Error("the parent was lost")
	}
}

func TestSpanPushFailureIsReturned(t *testing.T) {
	mock := &mockCollector{status: http.StatusBadGateway}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	exp, err := TraceFactory(context.Background(), sinkConfig(srv.URL, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close(context.Background())
	span := connector.SpanRecord{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", Name: "x", Start: time.Now(), End: time.Now()}
	if err := exp.ExportSpans(context.Background(), []connector.SpanRecord{span}); err == nil {
		t.Error("a failed push must reach the caller's pump")
	}
	if err := exp.ExportSpans(context.Background(), nil); err != nil {
		t.Errorf("an empty batch does nothing: %v", err)
	}
}

func TestMetricsPushRealOTLPWithTenantAttribute(t *testing.T) {
	mock := &mockCollector{}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	exp, err := MetricsFactory(context.Background(), sinkConfig(srv.URL, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close(context.Background())
	ctx := context.Background()
	labels := map[string]string{telemetry.LabelTenant: "acme", "node_type": "video.cut"}
	err = exp.Export(ctx, []connector.MetricPoint{
		{Name: telemetry.NodeRetries, Kind: "counter", Value: 2, Labels: labels},
		{Name: telemetry.NodeRetries, Kind: "counter", Value: 3, Labels: labels},
		{Name: telemetry.NodeRetries, Kind: "counter", Value: -1, Labels: labels}, // a counter never goes down
		{Name: telemetry.NodeRetries, Kind: "gauge", Value: 100, Labels: labels},  // mismatch ignored
		{Name: telemetry.BudgetPercent, Kind: "gauge", Value: 80, Labels: map[string]string{telemetry.LabelTenant: "acme"}},
		{Name: telemetry.BudgetPercent, Kind: "counter", Value: 1, Labels: map[string]string{telemetry.LabelTenant: "acme"}}, // Add on a gauge
		{Name: telemetry.NodeDuration, Kind: "histogram", Value: 7200, Labels: labels},
		{Name: "not a valid name!", Kind: "counter", Value: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	tester := exp.(*MetricsExporter)
	if err := tester.TestExport(ctx); err != nil {
		t.Fatal(err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.metrics) == 0 {
		t.Fatal("nothing pushed")
	}
	found := map[string]*metricpb.Metric{}
	for _, req := range mock.metrics {
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					found[m.Name] = m
				}
			}
		}
	}
	sum := found[telemetry.NodeRetries].GetSum()
	if sum == nil || sum.DataPoints[0].GetAsDouble() != 5 || !sum.IsMonotonic {
		t.Errorf("counter: %+v", found[telemetry.NodeRetries])
	}
	tenantSeen := false
	for _, a := range sum.DataPoints[0].Attributes {
		if a.Key == telemetry.LabelTenant && a.Value.GetStringValue() == "acme" {
			tenantSeen = true
		}
	}
	if !tenantSeen {
		t.Error("tenant_id must be an attribute (D6)")
	}
	if g := found[telemetry.BudgetPercent].GetGauge(); g == nil || g.DataPoints[0].GetAsDouble() != 81 {
		t.Errorf("gauge: %+v", found[telemetry.BudgetPercent])
	}
	if h := found[telemetry.NodeDuration].GetHistogram(); h == nil || h.DataPoints[0].GetCount() != 1 || len(h.DataPoints[0].ExplicitBounds) != len(telemetry.DurationBuckets) {
		t.Errorf("histogram: %+v", found[telemetry.NodeDuration])
	}
}

func TestMetricsPushFailureIsReportedAndDataSurvives(t *testing.T) {
	mock := &mockCollector{status: http.StatusBadGateway}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	exp, err := MetricsFactory(context.Background(), sinkConfig(srv.URL, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close(context.Background())
	me := exp.(*MetricsExporter)
	ctx := context.Background()
	labels := map[string]string{telemetry.LabelTenant: "acme", "node_type": "a"}
	if err := exp.Export(ctx, []connector.MetricPoint{{Name: telemetry.NodeRetries, Kind: "counter", Value: 4, Labels: labels}}); err != nil {
		t.Fatal(err)
	}
	if err := me.TestExport(ctx); err == nil {
		t.Error("a failing push must show in a test")
	}
	failures, last := me.PushHealth()
	if failures == 0 || last == "" {
		t.Errorf("health: %d %q", failures, last)
	}
	// The collector comes back: cumulative data means nothing was lost.
	mock.mu.Lock()
	mock.status = 0
	mock.mu.Unlock()
	if err := me.TestExport(ctx); err != nil {
		t.Fatal(err)
	}
	if _, last := me.PushHealth(); last != "" {
		t.Errorf("a good push clears the error: %q", last)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.metrics) != 1 || mock.metrics[0].ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetSum().DataPoints[0].GetAsDouble() != 4 {
		t.Errorf("data after recovery: %+v", mock.metrics)
	}
}

func TestFactoryRefusalsAndRegister(t *testing.T) {
	ctx := context.Background()
	cases := []config.Sink{
		{Parameters: json.RawMessage(`{`)},
		{Parameters: json.RawMessage(`{}`)},
		{Parameters: json.RawMessage(`{"endpoint":"https://u:p@otel.example.internal"}`)},
		{CredentialsRef: "t", Parameters: json.RawMessage(`{"endpoint":"http://otel.example.internal"}`)},
		{CredentialsRef: "missing", Parameters: json.RawMessage(`{"endpoint":"https://otel.example.internal"}`)},
	}
	res := secrets{"t": "x"}
	for i, c := range cases {
		if _, err := TraceFactory(ctx, c, res); err == nil {
			t.Errorf("trace case %d should be refused", i)
		}
		if _, err := MetricsFactory(ctx, c, res); err == nil {
			t.Errorf("metrics case %d should be refused", i)
		}
	}
	reg := connector.NewRegistry()
	Register(reg)
	if _, err := reg.TraceExporter(Driver); err != nil {
		t.Error(err)
	}
	if _, err := reg.MetricsExporter(Driver); err != nil {
		t.Error(err)
	}
	if len(reg.List()) != 2 {
		t.Errorf("registry: %+v", reg.List())
	}
}

func TestInfoAndAttributesAreStable(t *testing.T) {
	exp, err := TraceFactory(context.Background(), sinkConfig("http://127.0.0.1:1", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close(context.Background())
	if info := exp.Info(); info.Family != connector.FamilyTraceExporter || !info.Available {
		t.Errorf("info: %+v", info)
	}
	if statusCode("Error") == statusCode("Ok") || statusDescription("Ok") != "" || statusDescription("Error") != "Error" {
		t.Error("status mapping")
	}
}
