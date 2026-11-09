package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/rafaelp/vidyom/connectors/observability/loki"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// ---- fakes ----

type fakeLogSink struct {
	mu      sync.Mutex
	records []connector.LogRecord
	failing atomic.Bool
	hang    chan struct{}
	opens   int
	closes  int
	flushes int
}

func (f *fakeLogSink) Info() connector.Info {
	return connector.Info{Family: connector.FamilyLogSink, Driver: "fake", Available: true}
}

func (f *fakeLogSink) Write(ctx context.Context, batch []connector.LogRecord) error {
	if f.hang != nil {
		<-f.hang // ignores its context on purpose
	}
	if f.failing.Load() {
		return errors.New("fake sink is down, password=hunter2")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, batch...)
	return nil
}

func (f *fakeLogSink) Flush(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	return nil
}

func (f *fakeLogSink) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeLogSink) snapshot() []connector.LogRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]connector.LogRecord(nil), f.records...)
}

func (f *fakeLogSink) events() []string {
	var out []string
	for _, r := range f.snapshot() {
		out = append(out, r.Event)
	}
	return out
}

type fakeMetrics struct {
	mu     sync.Mutex
	points []connector.MetricPoint
}

func (f *fakeMetrics) Info() connector.Info {
	return connector.Info{Family: connector.FamilyMetricsExporter, Driver: "fake", Available: true}
}

func (f *fakeMetrics) Export(_ context.Context, batch []connector.MetricPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.points = append(f.points, batch...)
	return nil
}
func (f *fakeMetrics) Close(context.Context) error { return nil }
func (f *fakeMetrics) snapshot() []connector.MetricPoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]connector.MetricPoint(nil), f.points...)
}

type fakeSpans struct {
	mu    sync.Mutex
	spans []connector.SpanRecord
}

func (f *fakeSpans) Info() connector.Info {
	return connector.Info{Family: connector.FamilyTraceExporter, Driver: "fake", Available: true}
}

func (f *fakeSpans) ExportSpans(_ context.Context, spans []connector.SpanRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spans = append(f.spans, spans...)
	return nil
}
func (f *fakeSpans) Close(context.Context) error { return nil }
func (f *fakeSpans) snapshot() []connector.SpanRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]connector.SpanRecord(nil), f.spans...)
}

// fixture registers fake drivers whose instances are found by sink ID.
type fixture struct {
	reg     *connector.Registry
	mu      sync.Mutex
	logs    map[string]*fakeLogSink
	metrics map[string]*fakeMetrics
	spans   map[string]*fakeSpans
	secrets []string
}

func newFixture() *fixture {
	f := &fixture{
		reg:     connector.NewRegistry(),
		logs:    map[string]*fakeLogSink{},
		metrics: map[string]*fakeMetrics{},
		spans:   map[string]*fakeSpans{},
	}
	RegisterBuiltins(f.reg)
	f.reg.RegisterLogSink(connector.Info{Driver: "fake", Available: true}, func(ctx context.Context, cfg config.Sink, secrets connector.SecretResolver) (connector.LogSink, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if cfg.CredentialsRef != "" {
			if secrets == nil {
				return nil, errors.New("no resolver")
			}
			v, err := secrets.ResolveSecret(ctx, cfg.CredentialsRef)
			if err != nil {
				return nil, err
			}
			f.secrets = append(f.secrets, string(v))
		}
		s := f.logs[cfg.ID]
		if s == nil {
			s = &fakeLogSink{}
			f.logs[cfg.ID] = s
		}
		s.opens++
		return s, nil
	})
	f.reg.RegisterMetricsExporter(connector.Info{Driver: "fake", Available: true}, func(_ context.Context, cfg config.Sink, _ connector.SecretResolver) (connector.MetricsExporter, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s := f.metrics[cfg.ID]
		if s == nil {
			s = &fakeMetrics{}
			f.metrics[cfg.ID] = s
		}
		return s, nil
	})
	f.reg.RegisterTraceExporter(connector.Info{Driver: "fake", Available: true}, func(_ context.Context, cfg config.Sink, _ connector.SecretResolver) (connector.TraceExporter, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s := f.spans[cfg.ID]
		if s == nil {
			s = &fakeSpans{}
			f.spans[cfg.ID] = s
		}
		return s, nil
	})
	return f
}

func (f *fixture) logSink(id string) *fakeLogSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logs[id] == nil {
		f.logs[id] = &fakeLogSink{}
	}
	return f.logs[id]
}

type tenantConfigs map[domain.TenantID]config.Observability

func (t tenantConfigs) TenantConfig(_ context.Context, tenant domain.TenantID) (config.Tenant, error) {
	cfg, ok := t[tenant]
	if !ok {
		return config.Tenant{}, errs.NotFound("tenant")
	}
	return config.Tenant{Observability: cfg}, nil
}

type fakeAuditor struct {
	mu      sync.Mutex
	entries []security.AuditEntry
	err     error
}

func (a *fakeAuditor) Record(_ context.Context, e security.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.entries = append(a.entries, e)
	return nil
}
func (a *fakeAuditor) Verify(context.Context, domain.TenantID) (domain.AuditVerification, error) {
	return domain.AuditVerification{}, nil
}
func (a *fakeAuditor) Export(context.Context, domain.TenantID) (string, error) { return "", nil }

type secretMap map[string]string

func (s secretMap) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	v, ok := s[ref]
	if !ok {
		return nil, errors.New("no such secret")
	}
	return []byte(v), nil
}

// ---- helpers ----

func testTuning() Tuning {
	return Tuning{
		QueueSize:      64,
		BatchSize:      16,
		FlushInterval:  2 * time.Millisecond,
		WriteTimeout:   200 * time.Millisecond,
		OpenTimeout:    200 * time.Millisecond,
		OpenRetry:      5 * time.Millisecond,
		BackoffBase:    time.Millisecond,
		BackoffMax:     2 * time.Millisecond,
		MaxAttempts:    2,
		DrainTimeout:   500 * time.Millisecond,
		FailureEvery:   time.Hour,
		HealthInterval: 5 * time.Millisecond,
		RingSize:       128,
	}
}

func sink(id, driver string) config.Sink {
	return config.Sink{ID: id, Driver: driver, Enabled: true}
}

func newManager(t *testing.T, cfg config.Observability, opts Options) *Manager {
	t.Helper()
	if opts.Tuning == (Tuning{}) {
		opts.Tuning = testTuning()
	}
	m, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m
}

func flush(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func scope(tenant string) service.Scope {
	return service.Scope{Tenant: domain.TenantID(tenant), Principal: domain.Principal{Email: "admin@example.com"}, RequestID: "req-1"}
}

func adminScope(tenant string) service.Scope {
	s := scope(tenant)
	s.Principal.SystemAdmin = true
	return s
}

func noMetrics() []config.Sink {
	return []config.Sink{{ID: "off", Driver: "prometheus", Enabled: false}}
}

// ---- fan-out and isolation ----

func TestFanOutToEverySinkWithSpecFields(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("a", "fake"), sink("b", "fake")}}, Metrics: noMetrics()}, Options{Registry: f.reg, Service: "vidyomd"})
	m.Emit(context.Background(), connector.LogRecord{Event: "job.started", TenantID: "acme", JobID: "JOB-001", Level: "INFO"})
	flush(t, m)
	for _, id := range []string{"a", "b"} {
		got := f.logSink(id).snapshot()
		if len(got) != 1 {
			t.Fatalf("%s got %d records", id, len(got))
		}
		r := got[0]
		if r.Service != "vidyomd" || r.Level != "info" || r.Timestamp.IsZero() || r.Timestamp.Location() != time.UTC {
			t.Errorf("%s normalized record: %+v", id, r)
		}
	}
}

// TestDeadLokiBlocksNothing is §15.7: Loki unavailable, processing continues, other
// sinks keep receiving, and there is a failure record.
func TestDeadLokiBlocksNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused from now on
	f := newFixture()
	logDir := t.TempDir()
	params, _ := json.Marshal(loki.Parameters{URL: url})
	bigQueue := testTuning()
	bigQueue.QueueSize = 2000
	bigQueue.BatchSize = 64
	fileParams, _ := json.Marshal(map[string]any{"path": filepath.Join(logDir, "vidyomd.jsonl")})
	m := newManager(t, config.Observability{
		Logs: config.LogConfig{Sinks: []config.Sink{
			{ID: "loki", Driver: "loki", Enabled: true, Parameters: params},
			{ID: "local", Driver: "file", Enabled: true, Parameters: fileParams},
			sink("datadog-stand-in", "fake"),
		}},
		Metrics: noMetrics(),
	}, Options{Registry: f.reg, Tuning: bigQueue})

	start := time.Now()
	for i := range 500 {
		m.Emit(context.Background(), connector.LogRecord{Event: "tick", TenantID: "acme", Message: fmt.Sprint(i)})
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("emitting took %v with Loki dead", elapsed)
	}
	eventually(t, "the other sink to get everything", func() bool { return len(f.logSink("datadog-stand-in").snapshot()) >= 500 })

	var lokiStatus api.ObservabilitySink
	// Dropping a batch after its attempts is what the small test numbers make visible;
	// the outage itself never touches the other sinks.
	eventually(t, "Loki to show as degraded", func() bool {
		sinks, err := m.Sinks(context.Background(), adminScope(""))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sinks {
			if s.ID == "loki" {
				lokiStatus = s
			}
		}
		return lokiStatus.Status == "degraded" && lokiStatus.Dropped > 0
	})
	if lokiStatus.LastError == "" {
		t.Errorf("no last error: %+v", lokiStatus)
	}
	var failure bool
	eventually(t, "the failure record", func() bool {
		for _, ev := range f.logSink("datadog-stand-in").events() {
			failure = failure || ev == "observability.sink_failed"
		}
		return failure
	})
	page, err := m.Logs(context.Background(), scope("acme"), api.LogQuery{PageQuery: api.PageQuery{Limit: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 500 {
		t.Errorf("local logs are the point of §10.1, got %d", len(page.Items))
	}
}

func TestHungSinkDoesNotDelayOthersOrShutdown(t *testing.T) {
	f := newFixture()
	release := make(chan struct{})
	defer close(release)
	f.logSink("hung").hang = release
	m, err := New(config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("hung", "fake"), sink("fine", "fake")}}, Metrics: noMetrics()}, Options{Registry: f.reg, Tuning: testTuning()})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := range 2000 {
		m.Emit(context.Background(), connector.LogRecord{Event: "tick", Message: fmt.Sprint(i)})
	}
	if time.Since(start) > 2*time.Second {
		t.Error("Emit blocked on a hung sink")
	}
	eventually(t, "the healthy sink to keep receiving", func() bool { return len(f.logSink("fine").snapshot()) > 0 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	_ = m.Close(ctx) // reports the hung sink; must not wait for it
	if time.Since(start) > 2*time.Second {
		t.Errorf("Close waited %v for a hung sink", time.Since(start))
	}
}

func TestFailureRecordNeverGoesIntoTheFailingSink(t *testing.T) {
	f := newFixture()
	broken := f.logSink("broken")
	broken.failing.Store(true)
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("broken", "fake"), sink("fine", "fake")}}, Metrics: noMetrics()}, Options{Registry: f.reg})
	m.Emit(context.Background(), connector.LogRecord{Event: "first", TenantID: "acme"})
	eventually(t, "a failure record", func() bool {
		for _, r := range f.logSink("fine").snapshot() {
			if r.Event == "observability.sink_failed" {
				return true
			}
		}
		return false
	})
	for _, r := range f.logSink("fine").snapshot() {
		if r.Event == "observability.sink_failed" {
			if strings.Contains(r.Message, "hunter2") {
				t.Errorf("a failure record leaked a secret: %s", r.Message)
			}
			if r.Fields["sink"] != "broken" {
				t.Errorf("record names no sink: %+v", r)
			}
		}
	}
	broken.failing.Store(false)
	eventually(t, "only good events to reach the recovered sink", func() bool {
		m.Emit(context.Background(), connector.LogRecord{Event: "later", TenantID: "acme"})
		return len(broken.snapshot()) > 0
	})
	for _, r := range broken.snapshot() {
		if r.Event == "observability.sink_failed" {
			t.Errorf("the failing sink got its own failure record: %+v", r)
		}
	}
	eventually(t, "a recovery record", func() bool {
		for _, r := range f.logSink("fine").snapshot() {
			if r.Event == "observability.sink_recovered" {
				return true
			}
		}
		return false
	})
}

// ---- redaction ----

func TestSecretsReachNoSinkRingOrSubscriber(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("a", "fake")}}, Metrics: noMetrics()}, Options{Registry: f.reg})
	sub, err := m.SubscribeLogs(context.Background(), scope("acme"), api.LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	m.Redactor().AddValue("exact-secret-value")
	m.Emit(context.Background(), connector.LogRecord{
		Event:    "call",
		TenantID: "acme",
		Message:  "calling with Bearer abc.DEF-123 and exact-secret-value",
		Fields: map[string]any{
			"token":   "abc123",
			"url":     "https://media.example.internal/x?X-Goog-Signature=sig123",
			"nested":  map[string]any{"password": "hunter2", "credentialsRef": "gcs-main"},
			"struct":  struct{ APIKey string }{"plain-key-value"},
			"api_key": "plain-key-value",
		},
	})
	flush(t, m)
	check := func(where string, r connector.LogRecord) {
		t.Helper()
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, leak := range []string{"abc.DEF-123", "exact-secret-value", "abc123", "sig123", "hunter2"} {
			if strings.Contains(text, leak) {
				t.Errorf("%s leaked %q: %s", where, leak, text)
			}
		}
		if !strings.Contains(text, "gcs-main") {
			t.Errorf("%s lost a harmless reference: %s", where, text)
		}
	}
	check("sink", f.logSink("a").snapshot()[0])
	check("subscriber", <-sub)
	page, err := m.Logs(context.Background(), scope("acme"), api.LogQuery{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("logs: %v %+v", err, page)
	}
	check("ring", page.Items[0])
}

// ---- tenants ----

func TestTenantRoutingNeverCrosses(t *testing.T) {
	f := newFixture()
	tenants := tenantConfigs{
		"acme":  {Logs: config.LogConfig{Sinks: []config.Sink{sink("acme-logs", "fake")}}, Metrics: []config.Sink{sink("acme-metrics", "fake")}, Traces: []config.Sink{sink("acme-traces", "fake")}},
		"other": {Logs: config.LogConfig{Sinks: []config.Sink{sink("other-logs", "fake")}}},
	}
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("daemon-logs", "fake")}}, Metrics: []config.Sink{sink("daemon-metrics", "fake")}}, Options{Registry: f.reg, Tenants: tenants})
	ctx := context.Background()
	m.Emit(ctx, connector.LogRecord{Event: "a", TenantID: "acme"})
	m.Emit(ctx, connector.LogRecord{Event: "o", TenantID: "other"})
	m.Emit(ctx, connector.LogRecord{Event: "system"})
	m.Add(telemetry.NodeFailures, 1, telemetry.Labels{"node_type": "x", telemetry.LabelTenant: "acme"})
	m.Add(telemetry.NodeFailures, 1, telemetry.Labels{"node_type": "x", telemetry.LabelTenant: "other"})
	m.Set(telemetry.WorkerCPU, 8, telemetry.Labels{"worker": "w1"})
	flush(t, m)

	if got := fmt.Sprint(f.logSink("acme-logs").events()); got != "[a]" {
		t.Errorf("acme sink: %s", got)
	}
	if got := fmt.Sprint(f.logSink("other-logs").events()); got != "[o]" {
		t.Errorf("other sink: %s", got)
	}
	if got := f.logSink("daemon-logs").events(); len(got) != 3 {
		t.Errorf("the daemon sink sees everything: %v", got)
	}
	f.mu.Lock()
	acmeMetrics := f.metrics["acme-metrics"]
	daemonMetrics := f.metrics["daemon-metrics"]
	f.mu.Unlock()
	eventually(t, "metrics", func() bool { return len(acmeMetrics.snapshot()) > 0 && len(daemonMetrics.snapshot()) >= 3 })
	for _, p := range acmeMetrics.snapshot() {
		if p.Name == telemetry.NodeFailures && p.Labels[telemetry.LabelTenant] != "acme" {
			t.Errorf("acme's exporter got another tenant's point: %+v", p)
		}
		if p.Name == telemetry.WorkerCPU {
			t.Errorf("acme's exporter got a system point: %+v", p)
		}
	}

	page, err := m.Logs(ctx, scope("other"), api.LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Items {
		if r.TenantID != "other" {
			t.Errorf("other saw %+v", r)
		}
	}
	if len(page.Items) != 1 {
		t.Errorf("other sees %d records", len(page.Items))
	}
}

func TestTenantSinksAreLoadedLazilyWithItsOwnSecrets(t *testing.T) {
	f := newFixture()
	acme := config.Sink{ID: "t", Driver: "fake", Enabled: true, CredentialsRef: "tenant-secret"}
	tenants := tenantConfigs{"acme": {Logs: config.LogConfig{Sinks: []config.Sink{acme}}}}
	var asked []domain.TenantID
	m := newManager(t, config.Observability{Metrics: noMetrics()}, Options{
		Registry: f.reg,
		Tenants:  tenants,
		TenantSecrets: func(tenant domain.TenantID) connector.SecretResolver {
			asked = append(asked, tenant)
			return secretMap{"tenant-secret": "value-of-" + string(tenant)}
		},
	})
	m.Emit(context.Background(), connector.LogRecord{Event: "first", TenantID: "acme"})
	flush(t, m)
	if got := fmt.Sprint(f.logSink("t").events()); got != "[first]" {
		t.Errorf("records before the load finished must reach the sink: %s", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.secrets) != 1 || f.secrets[0] != "value-of-acme" {
		t.Errorf("the tenant's own resolver must be used: %v", f.secrets)
	}
	if len(asked) != 1 || asked[0] != "acme" {
		t.Errorf("asked: %v", asked)
	}
}

func TestBogusTenantValuesGetNoPipeline(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("d", "fake")}}, Metrics: noMetrics()}, Options{Registry: f.reg, Tenants: tenantConfigs{}})
	for _, tenant := range []string{"../etc", "UPPER", "has space", "a"} {
		m.Emit(context.Background(), connector.LogRecord{Event: "x", TenantID: tenant})
	}
	m.Emit(context.Background(), connector.LogRecord{Event: "x", TenantID: "ghost"}) // a valid slug, unknown tenant
	flush(t, m)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.tenants) != 1 {
		t.Errorf("pipelines: %v", m.tenants)
	}
	if len(f.logSink("d").snapshot()) != 5 {
		t.Error("the daemon's sinks still get every record")
	}
}

// lateTenants finds a tenant's configuration only after it is told to.
type lateTenants struct {
	ready atomic.Bool
}

func (l *lateTenants) TenantConfig(ctx context.Context, tenant domain.TenantID) (config.Tenant, error) {
	if !l.ready.Load() {
		return config.Tenant{}, errs.NotFound("tenant")
	}
	return tenantConfigs{"late": {Logs: config.LogConfig{Sinks: []config.Sink{sink("late-logs", "fake")}}}}.TenantConfig(ctx, tenant)
}

func TestTenantConfigFailureIsReportedAndRetried(t *testing.T) {
	f := newFixture()
	tenants := &lateTenants{}
	m := newManager(t, config.Observability{Metrics: noMetrics()}, Options{Registry: f.reg, Tenants: tenants})
	if _, err := m.Sinks(context.Background(), scope("late")); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
	// Reads are unaffected, and a retry after the file appears works.
	tenants.ready.Store(true)
	eventually(t, "the retry", func() bool {
		sinks, err := m.Sinks(context.Background(), scope("late"))
		return err == nil && len(sinks) == 1
	})
}

// ---- defaults and configuration ----

func TestDefaultsAreLocalJSONLAndPrometheus(t *testing.T) {
	dir := t.TempDir()
	m := newManager(t, config.Observability{}, Options{LogDir: dir})
	m.Emit(context.Background(), connector.LogRecord{Event: "hello", TenantID: "acme"})
	m.Set(telemetry.BudgetPercent, 42, telemetry.Labels{telemetry.LabelTenant: "acme"})
	flush(t, m)
	data, err := os.ReadFile(filepath.Join(dir, "vidyomd.jsonl"))
	if err != nil || !strings.Contains(string(data), `"event":"hello"`) {
		t.Fatalf("D38 local JSONL: %v %s", err, data)
	}
	sinks, err := m.Sinks(context.Background(), adminScope(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(sinks) != 2 || sinks[0].ID != "local" || sinks[1].ID != "prometheus" || sinks[0].Status != "ok" {
		t.Errorf("sinks: %+v", sinks)
	}
	text := scrape(t, m, "")
	if !strings.Contains(text, `vidyom_budget_consumption_percent{tenant_id="acme"} 42`) {
		t.Errorf("metrics:\n%s", text)
	}
}

func TestOptionsFromConfig(t *testing.T) {
	o := OptionsFromConfig(config.Daemon{DataDir: "/var/lib/vidyom", Retention: config.Retention{LogDays: 7}})
	if o.LogDir != "/var/lib/vidyom/logs" || o.RetentionDays != 7 || o.StateFile != "/var/lib/vidyom/observability-state.json" {
		t.Errorf("options: %+v", o)
	}
	if o := OptionsFromConfig(config.Daemon{}); o.LogDir != "" || o.StateFile != "" {
		t.Errorf("no data directory: %+v", o)
	}
}

func TestExplicitConfigurationIsRespected(t *testing.T) {
	dir := t.TempDir()
	f := newFixture()
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("only", "fake")}}, Metrics: noMetrics()}, Options{Registry: f.reg, LogDir: dir})
	m.Emit(context.Background(), connector.LogRecord{Event: "x"})
	flush(t, m)
	if _, err := os.Stat(filepath.Join(dir, "vidyomd.jsonl")); err == nil {
		t.Error("an explicit sink list must not gain an implicit file sink")
	}
	rec := httptest.NewRecorder()
	m.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled Prometheus answers %d", rec.Code)
	}
}

func TestSpecExampleConfigurationLoads(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "schemas", "testdata", "config-observability-7.4.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file config.ObservabilityFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for i, s := range file.Observability.Logs.Sinks {
		if s.Driver == "file" {
			params, _ := json.Marshal(map[string]any{"path": filepath.Join(dir, "vidyomd.jsonl"), "rotation": "daily", "retentionDays": 30})
			file.Observability.Logs.Sinks[i].Parameters = params
		}
	}
	m := newManager(t, file.Observability, Options{Secrets: secretMap{"loki-auth": "svc:example"}})
	sinks, err := m.Sinks(context.Background(), adminScope(""))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]api.ObservabilitySink{}
	for _, s := range sinks {
		byID[s.ID] = s
	}
	if byID["datadog"].Status != "disabled" || byID["datadog"].LastError != "" {
		t.Errorf("a disabled Future driver is inert: %+v", byID["datadog"])
	}
	if byID["local"].Driver != "file" {
		t.Errorf("local: %+v", byID["local"])
	}
}

func TestConfigurationRefusals(t *testing.T) {
	if _, err := New(config.Observability{Logs: config.LogConfig{Level: "loud"}}); err == nil {
		t.Error("an unknown level must fail")
	}
	dup := config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("a", "file"), sink("a", "file")}}}
	if _, err := New(dup); err == nil {
		t.Error("duplicate IDs must fail")
	}
	noID := config.Observability{Traces: []config.Sink{{Driver: "otlp", Enabled: true}}}
	if _, err := New(noID); err == nil {
		t.Error("a sink with no ID must fail")
	}
	dir := t.TempDir()
	badState := filepath.Join(dir, "state.json")
	os.WriteFile(badState, []byte("{"), 0o640)
	if _, err := New(config.Observability{}, Options{StateFile: badState}); err == nil {
		t.Error("an unreadable state file must fail")
	}
}

func TestBadSinkEntriesDoNotStopTheDaemon(t *testing.T) {
	f := newFixture()
	inline := config.Sink{ID: "inline", Driver: "fake", Enabled: true, Parameters: json.RawMessage(`{"api_key":"abc"}`)}
	m := newManager(t, config.Observability{
		Logs:    config.LogConfig{Sinks: []config.Sink{sink("unknown", "datadog"), inline, sink("fine", "fake")}},
		Metrics: noMetrics(),
	}, Options{Registry: f.reg})
	m.Emit(context.Background(), connector.LogRecord{Event: "x"})
	flush(t, m)
	if len(f.logSink("fine").snapshot()) != 1 {
		t.Error("a good sink must keep working")
	}
	sinks, err := m.Sinks(context.Background(), adminScope(""))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sinks {
		switch s.ID {
		case "unknown":
			if s.Status != "failed" || !strings.Contains(s.LastError, "datadog") {
				t.Errorf("unknown driver: %+v", s)
			}
		case "inline":
			if s.Status != "failed" || !strings.Contains(s.LastError, "inline credential") {
				t.Errorf("inline credential: %+v", s)
			}
			if strings.Contains(s.LastError, "abc") {
				t.Errorf("value leaked: %+v", s)
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logs["inline"] != nil && f.logs["inline"].opens != 0 {
		t.Error("a refused sink must not open")
	}
}

func TestDaemonSinkWithCredentialNeedsResolver(t *testing.T) {
	f := newFixture()
	withRef := config.Sink{ID: "needs", Driver: "fake", Enabled: true, CredentialsRef: "x"}
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{withRef}}, Metrics: noMetrics()}, Options{Registry: f.reg})
	eventually(t, "open failure to show", func() bool {
		sinks, _ := m.Sinks(context.Background(), adminScope(""))
		for _, s := range sinks {
			if s.ID == "needs" {
				return s.Status == "failed" && s.LastError != ""
			}
		}
		return false
	})
}

// ---- metrics ----

func scrape(t *testing.T, m *Manager, tenant string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if tenant != "" {
		req.Header.Set(api.HeaderTenant, tenant)
	}
	rec := httptest.NewRecorder()
	m.MetricsHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape answered %d: %s", rec.Code, rec.Body.String())
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

// TestEveryMetricOfTheSpecIsExposed is §10.3 and D6, through the public facade.
func TestEveryMetricOfTheSpecIsExposed(t *testing.T) {
	m := newManager(t, config.Observability{}, Options{})
	for _, spec := range telemetry.Catalog {
		labels := telemetry.Labels{telemetry.LabelTenant: "acme"}
		for _, l := range spec.Labels {
			if l != telemetry.LabelTenant {
				labels[l] = "x"
			}
		}
		switch spec.Kind {
		case telemetry.KindCounter:
			m.Add(spec.Name, 1, labels)
		case telemetry.KindGauge:
			m.Set(spec.Name, 1, labels)
		case telemetry.KindHistogram:
			m.Observe(spec.Name, 1, labels)
		}
	}
	flush(t, m)
	text := scrape(t, m, "acme")
	for _, spec := range telemetry.Catalog {
		if !strings.Contains(text, "# TYPE "+spec.Name+" "+string(spec.Kind)) {
			t.Errorf("%s is not exposed", spec.Name)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "vidyom_") && !strings.Contains(line, `tenant_id="acme"`) {
			t.Errorf("sample without the tenant: %s", line)
		}
	}
}

func TestMetricLabelsAreNormalized(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Metrics: []config.Sink{sink("m", "fake")}}, Options{Registry: f.reg, Now: func() time.Time { return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) }})
	m.Add(telemetry.NodeRetries, 1, telemetry.Labels{"node_type": "x", "job_id": "JOB-001"})
	m.Add(telemetry.UsageCostEstimate, 5, telemetry.Labels{"operation": "transcription", telemetry.LabelTenant: "acme"})
	m.Add("vidyom_custom_total", 1, telemetry.Labels{"pool": "media"})
	m.Set(telemetry.WorkerCPU, 8, nil)
	flush(t, m)
	f.mu.Lock()
	exp := f.metrics["m"]
	f.mu.Unlock()
	eventually(t, "points", func() bool { return len(exp.snapshot()) >= 4 })
	byName := map[string]connector.MetricPoint{}
	for _, p := range exp.snapshot() {
		byName[p.Name] = p
	}
	retries := byName[telemetry.NodeRetries]
	if _, ok := retries.Labels["job_id"]; ok || retries.Labels[telemetry.LabelTenant] != telemetry.SystemTenant || retries.Kind != "counter" {
		t.Errorf("retries: %+v", retries)
	}
	if got := byName[telemetry.UsageCostEstimate].Labels["month"]; got != "2026-11" {
		t.Errorf("month must be filled (§10.3 per month): %q", got)
	}
	if got := byName["vidyom_custom_total"].Labels; got["pool"] != "media" || got[telemetry.LabelTenant] != telemetry.SystemTenant {
		t.Errorf("custom: %+v", got)
	}
	if byName[telemetry.WorkerCPU].Labels[telemetry.LabelTenant] != telemetry.SystemTenant {
		t.Errorf("a system metric: %+v", byName[telemetry.WorkerCPU])
	}
}

func TestExporterHealthMetricsAreExposed(t *testing.T) {
	f := newFixture()
	broken := f.logSink("broken")
	broken.failing.Store(true)
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("broken", "fake")}}}, Options{Registry: f.reg})
	for range 20 {
		m.Emit(context.Background(), connector.LogRecord{Event: "x"})
		time.Sleep(time.Millisecond)
	}
	eventually(t, "health metrics", func() bool {
		m.publishHealth()
		flushCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = m.Flush(flushCtx)
		text := scrape(t, m, "")
		return strings.Contains(text, `vidyom_exporter_failures_total{exporter="broken",kind="log",tenant_id="_system"}`) &&
			strings.Contains(text, `vidyom_exporter_dropped_total{exporter="broken",kind="log",tenant_id="_system"}`) &&
			strings.Contains(text, `vidyom_exporter_backlog{exporter="broken",kind="log",tenant_id="_system"}`) &&
			strings.Contains(text, `vidyom_exporter_delay_seconds{exporter="prometheus",kind="metrics",tenant_id="_system"}`)
	})
}

// ---- slog ----

func TestSlogHandlerMapsSpecFields(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Logs: config.LogConfig{Level: "info", Sinks: []config.Sink{sink("a", "fake")}}, Metrics: noMetrics()}, Options{Registry: f.reg, Service: "vidyomd"})
	log := m.Logger().With("tenant_id", "acme", slog.Group("ctx", "attempt", 2)).WithGroup("g")
	log.Info("node finished",
		slog.Group("n", "x", 1),
		"event", "node.finished",
	)
	m.Logger().Warn("slow", "job_id", "JOB-001", "workflow_id", "create-clip", "node_id", "cut", "asset_id", "ORI-001", "trace_id", "t-1", "status", "ok", "duration", 1500*time.Millisecond, "password", "hunter2")
	m.Logger().Info("ms", "duration_ms", int64(7), "tenant", domain.TenantID("acme"), "when", time.Unix(0, 0))
	m.Logger().Debug("below the level")
	flush(t, m)
	got := f.logSink("a").snapshot()
	if len(got) != 3 {
		t.Fatalf("records: %+v", got)
	}
	first := got[0]
	if first.TenantID != "acme" || first.Level != "info" || first.Message != "node finished" || first.Fields["ctx.attempt"] != int64(2) || first.Fields["g.n.x"] != int64(1) {
		t.Errorf("groups and with: %+v", first)
	}
	second := got[1]
	if second.Level != "warn" || second.JobID != "JOB-001" || second.WorkflowID != "create-clip" || second.NodeID != "cut" || second.AssetID != "ORI-001" ||
		second.TraceID != "t-1" || second.Status != "ok" || second.DurationMS != 1500 || second.Event != "log" {
		t.Errorf("fields: %+v", second)
	}
	if second.Fields["password"] != "[REDACTED]" {
		t.Errorf("slog must not bypass redaction: %+v", second.Fields)
	}
	third := got[2]
	if third.DurationMS != 7 || third.TenantID != "acme" || !strings.HasPrefix(fmt.Sprint(third.Fields["when"]), "1970-01-01") {
		t.Errorf("kinds: %+v", third)
	}
}

// ---- service ----

func TestLogsAreTenantScopedPagedAndValidated(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Metrics: noMetrics(), Logs: config.LogConfig{Sinks: []config.Sink{sink("a", "fake")}}}, Options{Registry: f.reg})
	ctx := context.Background()
	for i := range 5 {
		m.Emit(ctx, connector.LogRecord{Event: fmt.Sprintf("a%d", i), TenantID: "acme", JobID: "JOB-001", Level: "info"})
	}
	m.Emit(ctx, connector.LogRecord{Event: "b", TenantID: "other", JobID: "JOB-001", Level: "error"})
	m.Emit(ctx, connector.LogRecord{Event: "sys", Level: "error"})
	m.Emit(ctx, connector.LogRecord{Event: "e", TenantID: "acme", Level: "error"})

	page, err := m.Logs(ctx, scope("acme"), api.LogQuery{PageQuery: api.PageQuery{Limit: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 4 || page.Items[0].Event != "e" || page.NextCursor == "" {
		t.Fatalf("first page: %+v", page)
	}
	rest, err := m.Logs(ctx, scope("acme"), api.LogQuery{PageQuery: api.PageQuery{Limit: 4, Cursor: page.NextCursor}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Items) != 2 || rest.Items[1].Event != "a0" || rest.NextCursor != "" {
		t.Errorf("second page: %+v", rest)
	}
	errorsOnly, _ := m.Logs(ctx, scope("acme"), api.LogQuery{Level: "error"})
	if len(errorsOnly.Items) != 1 {
		t.Errorf("level filter: %+v", errorsOnly)
	}
	job, _ := m.Logs(ctx, scope("acme"), api.LogQuery{JobID: "JOB-001"})
	if len(job.Items) != 5 {
		t.Errorf("job filter: %d", len(job.Items))
	}
	recent, _ := m.Logs(ctx, scope("acme"), api.LogQuery{Since: "1h"})
	if len(recent.Items) != 6 {
		t.Errorf("since: %d", len(recent.Items))
	}
	absolute, _ := m.Logs(ctx, scope("acme"), api.LogQuery{Since: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if len(absolute.Items) != 0 || absolute.Items == nil {
		t.Errorf("a future since is an empty list, not null: %+v", absolute)
	}
	if _, err := m.Logs(ctx, scope(""), api.LogQuery{}); !errs.IsCode(err, errs.CodeTenantRequired) {
		t.Errorf("no tenant: %v", err)
	}
	if _, err := m.Logs(ctx, scope("acme"), api.LogQuery{Since: "yesterday"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("bad since: %v", err)
	}
	if _, err := m.Logs(ctx, scope("acme"), api.LogQuery{Level: "loud"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("bad level: %v", err)
	}
}

func TestLogsReadFromFileAcrossManagerRestarts(t *testing.T) {
	dir := t.TempDir()
	m1, err := New(config.Observability{}, Options{LogDir: dir, Tuning: testTuning()})
	if err != nil {
		t.Fatal(err)
	}
	m1.Emit(context.Background(), connector.LogRecord{Event: "before restart", TenantID: "acme"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m1.Close(ctx); err != nil {
		t.Fatal(err)
	}
	m2 := newManager(t, config.Observability{}, Options{LogDir: dir})
	eventually(t, "the local sink to open", func() bool {
		q, _ := m2.querier(context.Background(), "acme")
		return q != nil
	})
	page, err := m2.Logs(context.Background(), scope("acme"), api.LogQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Event != "before restart" {
		t.Errorf("local logs must survive: %v %+v", err, page)
	}
	if _, err := m2.Logs(context.Background(), scope("acme"), api.LogQuery{PageQuery: api.PageQuery{Cursor: "!!!"}}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("a bad cursor must be refused: %v", err)
	}
}

func TestSubscribeLogs(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Metrics: noMetrics()}, Options{Registry: f.reg})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Emit(ctx, connector.LogRecord{Event: "history", TenantID: "acme"})
	live, err := m.SubscribeLogs(ctx, scope("acme"), api.LogQuery{Level: "warn"})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := m.SubscribeLogs(ctx, scope("acme"), api.LogQuery{Since: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-replay; got.Event != "history" {
		t.Errorf("since should replay recent records: %+v", got)
	}
	m.Emit(ctx, connector.LogRecord{Event: "other tenant", TenantID: "other", Level: "error"})
	m.Emit(ctx, connector.LogRecord{Event: "low", TenantID: "acme", Level: "info"})
	m.Emit(ctx, connector.LogRecord{Event: "high", TenantID: "acme", Level: "error"})
	if got := <-live; got.Event != "high" {
		t.Errorf("filters: %+v", got)
	}
	// A reader that never reads loses records and slows nothing.
	start := time.Now()
	for range subscriberBuffer * 4 {
		m.Emit(ctx, connector.LogRecord{Event: "flood", TenantID: "acme", Level: "error"})
	}
	if time.Since(start) > 2*time.Second {
		t.Error("a slow subscriber delayed Emit")
	}
	cancel()
	eventually(t, "the channel to close", func() bool {
		for {
			select {
			case _, ok := <-live:
				if !ok {
					return true
				}
			default:
				return false
			}
		}
	})
	if _, err := m.SubscribeLogs(context.Background(), scope(""), api.LogQuery{}); !errs.IsCode(err, errs.CodeTenantRequired) {
		t.Errorf("no tenant: %v", err)
	}
}

func TestSinksVisibility(t *testing.T) {
	f := newFixture()
	tenants := tenantConfigs{"acme": {Logs: config.LogConfig{Sinks: []config.Sink{sink("acme-logs", "fake")}}, Metrics: []config.Sink{sink("prom", "prometheus")}}}
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("daemon-logs", "fake")}}}, Options{Registry: f.reg, Tenants: tenants, Auditor: &fakeAuditor{}})
	ctx := context.Background()
	got, err := m.Sinks(ctx, scope("acme"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]api.ObservabilitySink{}
	for _, s := range got {
		ids[s.ID] = s
	}
	if _, ok := ids["daemon-logs"]; ok {
		t.Errorf("a tenant admin must not see the daemon's sinks: %+v", got)
	}
	if ids["prom"].Status != "via daemon endpoint" {
		t.Errorf("a tenant prometheus entry: %+v", ids["prom"])
	}
	all, _ := m.Sinks(ctx, adminScope("acme"))
	if len(all) != len(got)+2 {
		t.Errorf("a system admin sees all: %+v", all)
	}
	// A daemon-level sink does not exist for a tenant admin.
	if err := m.TestSink(ctx, scope("acme"), "daemon-logs"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("test: %v", err)
	}
	if err := m.SetSinkEnabled(ctx, scope("acme"), "daemon-logs", false); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("disable: %v", err)
	}
	if err := m.TestSink(ctx, scope("other"), "acme-logs"); !errs.IsCode(err, errs.CodeUnavailable) && !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("another tenant's sink: %v", err)
	}
}

func TestTestSink(t *testing.T) {
	f := newFixture()
	broken := f.logSink("broken")
	broken.failing.Store(true)
	tuning := testTuning()
	disabled := sink("off", "fake")
	disabled.Enabled = false
	m := newManager(t, config.Observability{
		Logs:    config.LogConfig{Sinks: []config.Sink{sink("fine", "fake"), sink("broken", "fake"), disabled, sink("nope", "datadog")}},
		Metrics: []config.Sink{sink("prometheus", "prometheus")},
		Traces:  []config.Sink{sink("traces", "fake")},
	}, Options{Registry: f.reg, Tuning: tuning})
	ctx := context.Background()
	eventually(t, "sinks to open", func() bool {
		sinks, _ := m.Sinks(ctx, adminScope(""))
		for _, s := range sinks {
			if s.ID == "fine" && s.Status != "ok" || s.ID == "traces" && s.Status != "ok" {
				return false
			}
		}
		return true
	})
	if err := m.TestSink(ctx, adminScope(""), "fine"); err != nil {
		t.Errorf("fine: %v", err)
	}
	if got := f.logSink("fine").events(); len(got) != 1 || got[0] != "observability.test" {
		t.Errorf("the test record: %v", got)
	}
	err := m.TestSink(ctx, adminScope(""), "broken")
	if !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("broken: %v", err)
	}
	if strings.Contains(errs.Public(err).Message, "hunter2") {
		t.Errorf("a test failure leaked a secret: %s", errs.Public(err).Message)
	}
	if err := m.TestSink(ctx, adminScope(""), "off"); err != nil {
		t.Errorf("a disabled sink is tested with a temporary instance: %v", err)
	}
	if err := m.TestSink(ctx, adminScope(""), "nope"); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Errorf("unknown driver: %v", err)
	}
	if err := m.TestSink(ctx, adminScope(""), "ghost"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("unknown sink: %v", err)
	}
	if err := m.TestSink(ctx, adminScope(""), "prometheus"); err != nil {
		t.Errorf("prometheus: %v", err)
	}
	if err := m.TestSink(ctx, adminScope(""), "traces"); err != nil {
		t.Errorf("traces: %v", err)
	}
	f.mu.Lock()
	spanExp := f.spans["traces"]
	f.mu.Unlock()
	if got := spanExp.snapshot(); len(got) != 1 || got[0].Name != "observability.test" {
		t.Errorf("test span: %+v", got)
	}
}

func TestSetSinkEnabledIsAuditedAndPersists(t *testing.T) {
	f := newFixture()
	dir := t.TempDir()
	state := filepath.Join(dir, "state", "observability.json")
	audit := &fakeAuditor{}
	tenants := tenantConfigs{"acme": {Logs: config.LogConfig{Sinks: []config.Sink{sink("mine", "fake")}}}}
	opts := Options{Registry: f.reg, Tenants: tenants, Auditor: audit, StateFile: state}
	m := newManager(t, config.Observability{Metrics: noMetrics()}, opts)
	ctx := context.Background()
	s := scope("acme")
	m.Emit(ctx, connector.LogRecord{Event: "one", TenantID: "acme"})
	flush(t, m)
	if err := m.SetSinkEnabled(ctx, s, "mine", false); err != nil {
		t.Fatal(err)
	}
	m.Emit(ctx, connector.LogRecord{Event: "two", TenantID: "acme"})
	flush(t, m)
	if got := f.logSink("mine").events(); fmt.Sprint(got) != "[one]" {
		t.Errorf("a disabled sink gets nothing more: %v", got)
	}
	sinks, _ := m.Sinks(ctx, s)
	if sinks[0].Status != "disabled" || sinks[0].Enabled {
		t.Errorf("status: %+v", sinks[0])
	}
	audit.mu.Lock()
	if len(audit.entries) != 1 || audit.entries[0].Action != "observability.disable" || audit.entries[0].Permission != domain.PermObservabilityManage || audit.entries[0].TenantID != "acme" {
		t.Errorf("audit: %+v", audit.entries)
	}
	audit.mu.Unlock()

	// A restart keeps the state: the overlay wins over the file's enabled: true.
	m2 := newManager(t, config.Observability{Metrics: noMetrics()}, opts)
	sinks, err := m2.Sinks(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if sinks[0].Enabled || sinks[0].Status != "disabled" {
		t.Errorf("a restart must not silently undo a disable: %+v", sinks[0])
	}
	if err := m2.SetSinkEnabled(ctx, s, "mine", true); err != nil {
		t.Fatal(err)
	}
	m2.Emit(ctx, connector.LogRecord{Event: "three", TenantID: "acme"})
	flush(t, m2)
	if got := f.logSink("mine").events(); fmt.Sprint(got) != "[one three]" {
		t.Errorf("enabled again: %v", got)
	}
}

func TestSetSinkEnabledNeedsAudit(t *testing.T) {
	f := newFixture()
	daemon := config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("a", "fake")}}, Metrics: noMetrics()}
	m := newManager(t, daemon, Options{Registry: f.reg})
	if err := m.SetSinkEnabled(context.Background(), adminScope(""), "a", false); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Errorf("no auditor: %v", err)
	}
	audit := &fakeAuditor{err: errors.New("audit is down")}
	m2 := newManager(t, daemon, Options{Registry: f.reg, Auditor: audit})
	if err := m2.SetSinkEnabled(context.Background(), adminScope(""), "a", false); err == nil {
		t.Error("an audit failure must stop the change")
	}
	sinks, _ := m2.Sinks(context.Background(), adminScope(""))
	if !sinks[0].Enabled {
		t.Error("the change must not have happened")
	}
}

func TestReloadSwapsSinks(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("old", "fake")}}, Metrics: noMetrics()}, Options{Registry: f.reg})
	ctx := context.Background()
	m.Emit(ctx, connector.LogRecord{Event: "before"})
	if err := m.Reload(ctx, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("new", "fake")}}, Metrics: noMetrics()}); err != nil {
		t.Fatal(err)
	}
	m.Emit(ctx, connector.LogRecord{Event: "after"})
	flush(t, m)
	if got := fmt.Sprint(f.logSink("old").events()); got != "[before]" {
		t.Errorf("old sink should drain and stop: %s", got)
	}
	if got := fmt.Sprint(f.logSink("new").events()); got != "[after]" {
		t.Errorf("new sink: %s", got)
	}
	if err := m.Reload(ctx, config.Observability{Logs: config.LogConfig{Level: "loud"}}); err == nil {
		t.Error("a bad configuration must be refused")
	}
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(ctx, config.Observability{}); err == nil {
		t.Error("a closed manager cannot reload")
	}
	if err := m.Close(ctx); err != nil {
		t.Errorf("a second Close is a no-op: %v", err)
	}
	m.Emit(ctx, connector.LogRecord{Event: "ignored"})
	m.Add(telemetry.NodeRetries, 1, nil)
}

// ---- tracing ----

func TestSpansReachExportersRedactedAndRouted(t *testing.T) {
	f := newFixture()
	tenants := tenantConfigs{"acme": {Traces: []config.Sink{sink("acme-traces", "fake")}}, "other": {Traces: []config.Sink{sink("other-traces", "fake")}}}
	m := newManager(t, config.Observability{Traces: []config.Sink{sink("daemon-traces", "fake")}, Metrics: noMetrics()}, Options{Registry: f.reg, Tenants: tenants})
	m.tenantPipe("acme")
	m.tenantPipe("other")
	flush(t, m)
	tracer := m.TracerProvider().Tracer("test")
	ctx, parent := tracer.Start(context.Background(), "workflow", trace.WithAttributes(attribute.String("tenant_id", "acme")))
	_, child := tracer.Start(ctx, "node", trace.WithAttributes(attribute.String("tenant_id", "acme"), attribute.String("token", "abc123"), attribute.String("note", "Bearer abc.DEF-123"), attribute.Int("attempt", 2)))
	child.End()
	parent.End()
	flush(t, m)
	f.mu.Lock()
	daemon, acme, other := f.spans["daemon-traces"], f.spans["acme-traces"], f.spans["other-traces"]
	f.mu.Unlock()
	eventually(t, "spans", func() bool { return len(daemon.snapshot()) == 2 && len(acme.snapshot()) == 2 })
	if len(other.snapshot()) != 0 {
		t.Errorf("another tenant got spans: %+v", other.snapshot())
	}
	var node connector.SpanRecord
	for _, s := range daemon.snapshot() {
		if s.Name == "node" {
			node = s
		}
	}
	if node.ParentSpanID == "" || len(node.TraceID) != 32 || node.End.Before(node.Start) {
		t.Errorf("span: %+v", node)
	}
	data, _ := json.Marshal(node.Attributes)
	if strings.Contains(string(data), "abc123") || strings.Contains(string(data), "abc.DEF-123") {
		t.Errorf("a span attribute leaked: %s", data)
	}
	if !strings.Contains(string(data), `"attempt":2`) {
		t.Errorf("attributes: %s", data)
	}
}

func TestLogsCarryTheTraceID(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("a", "fake")}}, Metrics: noMetrics()}, Options{Registry: f.reg})
	ctx, span := m.TracerProvider().Tracer("test").Start(context.Background(), "op")
	defer span.End()
	m.Logger().InfoContext(ctx, "in a span")
	m.Emit(ctx, connector.LogRecord{Event: "direct"})
	flush(t, m)
	want := span.SpanContext().TraceID().String()
	for _, r := range f.logSink("a").snapshot() {
		if r.TraceID != want {
			t.Errorf("record %s has trace %q, want %q", r.Event, r.TraceID, want)
		}
	}
}

func TestTraceHelpersRoundTrip(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Traces: []config.Sink{sink("t", "fake")}, Metrics: noMetrics()}, Options{Registry: f.reg})
	previous, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	m.InstallGlobals()
	defer func() {
		otel.SetTracerProvider(previous)
		otel.SetTextMapPropagator(prevProp)
	}()

	ctx, span := StartSpan(context.Background(), "client", attribute.String("tenant_id", "acme"))
	id := TraceID(ctx)
	if len(id) != 32 {
		t.Fatalf("trace id: %q", id)
	}
	var seen string
	var seenHeader string
	srv := httptest.NewServer(TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = TraceID(r.Context())
		seenHeader = r.Header.Get(api.HeaderTraceParent)
	})))
	defer srv.Close()
	client := &http.Client{Transport: TracingTransport{}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if req.Header.Get(api.HeaderTraceParent) != "" {
		t.Error("RoundTrip must not change the caller's request")
	}
	if seen != id || !strings.Contains(seenHeader, id) {
		t.Errorf("the server must continue the trace: %q %q want %q", seen, seenHeader, id)
	}
	EndSpan(span, nil)
	_, failed := StartSpan(context.Background(), "failing")
	EndSpan(failed, errors.New("boom"))
	flush(t, m)
	f.mu.Lock()
	exp := f.spans["t"]
	f.mu.Unlock()
	eventually(t, "spans", func() bool { return len(exp.snapshot()) >= 3 })
	statuses := map[string]string{}
	for _, s := range exp.snapshot() {
		statuses[s.Name] = s.Status
	}
	if statuses["client"] != "Ok" || statuses["failing"] != "Error" {
		t.Errorf("statuses: %v", statuses)
	}
	if TraceID(context.Background()) != "" {
		t.Error("no span, no trace ID")
	}
}

func TestObserveDuration(t *testing.T) {
	f := newFixture()
	m := newManager(t, config.Observability{Metrics: []config.Sink{sink("m", "fake")}}, Options{Registry: f.reg})
	done := ObserveDuration(m, telemetry.WorkflowDuration, telemetry.Labels{"workflow": "create-clip", telemetry.LabelTenant: "acme"})
	done()
	flush(t, m)
	f.mu.Lock()
	exp := f.metrics["m"]
	f.mu.Unlock()
	eventually(t, "the observation", func() bool {
		for _, p := range exp.snapshot() {
			if p.Name == telemetry.WorkflowDuration && p.Kind == "histogram" && p.Labels["workflow"] == "create-clip" {
				return true
			}
		}
		return false
	})
}

// ---- concurrency ----

func TestConcurrentUse(t *testing.T) {
	f := newFixture()
	tenants := tenantConfigs{"acme": {Logs: config.LogConfig{Sinks: []config.Sink{sink("t", "fake")}}}}
	m := newManager(t, config.Observability{Logs: config.LogConfig{Sinks: []config.Sink{sink("a", "fake")}}}, Options{Registry: f.reg, Tenants: tenants, Auditor: &fakeAuditor{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				tenant := "acme"
				if j%2 == 0 {
					tenant = "other"
				}
				m.Logger().Info("work", "tenant_id", tenant, "n", j)
				m.Add(telemetry.NodeRetries, 1, telemetry.Labels{"node_type": fmt.Sprint(i), telemetry.LabelTenant: tenant})
				_, _ = m.Sinks(ctx, adminScope(tenant))
				_, _ = m.Logs(ctx, scope("acme"), api.LogQuery{})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			_ = m.SetSinkEnabled(ctx, adminScope(""), "a", i%2 == 0)
			sub, _ := m.SubscribeLogs(ctx, scope("acme"), api.LogQuery{})
			_ = sub
		}
	}()
	wg.Wait()
	scrape(t, m, "acme")
}
