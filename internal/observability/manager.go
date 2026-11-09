package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/observability/redact"
)

const (
	kindLog     = "log"
	kindMetrics = "metrics"
	kindTrace   = "trace"

	daemonScope      = "_daemon"
	maxTenantPipes   = 4096
	pendingLogLimit  = 1024
	subscriberBuffer = 256
)

// tenantSlug is I16's pattern. A value that does not match gets no pipeline, because
// a client-supplied header can end up in a record's tenant_id.
var tenantSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

// inlineCredential mirrors schemas/connector-config.v1.json: a parameter that looks
// like a credential is refused, because configuration holds only references.
var inlineCredential = regexp.MustCompile(`(?i)(secret|token|password|passwd|api[-_]?key|private[-_]?key|credential)`)

// Options configure a Manager beyond the observability configuration itself.
type Options struct {
	// Service is the service field of log records. Empty means vidyomd.
	Service string
	// Registry resolves drivers. Nil means a private registry with RegisterBuiltins.
	Registry *connector.Registry
	// LogDir holds the implicit local log when no log sink is configured (D38). Empty
	// means no implicit file sink: only the in-memory ring keeps logs.
	LogDir string
	// RetentionDays is the implicit local log's retention. Zero means the default.
	RetentionDays int
	// StateFile keeps `observability enable` and `disable` across restarts (I279).
	StateFile string
	// Secrets resolves credentialsRef of daemon-level sinks. Nil refuses them.
	Secrets connector.SecretResolver
	// Tenants supplies per-tenant sinks. Nil means daemon-level sinks only.
	Tenants config.TenantSource
	// TenantSecrets resolves credentialsRef of a tenant's sinks.
	TenantSecrets func(domain.TenantID) connector.SecretResolver
	// Auditor records `observability enable` and `disable`. Nil refuses those calls,
	// because a mutation must be audited (§12.1).
	Auditor security.Auditor
	// Now is a test seam.
	Now func() time.Time
	// Tuning overrides the numbers of I273.
	Tuning Tuning
}

// OptionsFromConfig fills what comes from daemon.json. Registry, Secrets, Tenants,
// TenantSecrets and Auditor are left for wiring.
func OptionsFromConfig(cfg config.Daemon) Options {
	o := Options{RetentionDays: cfg.Retention.LogDays}
	if cfg.DataDir != "" {
		o.LogDir = filepath.Join(cfg.DataDir, "logs")
		o.StateFile = filepath.Join(cfg.DataDir, "observability-state.json")
	}
	return o
}

// Manager is the Observability Manager (§10.1). Emit and the telemetry.Metrics methods
// never block on an exporter and never fail, whatever exporters are doing.
type Manager struct {
	opts     Options
	tuning   Tuning
	reg      *connector.Registry
	redactor *redact.Redactor
	service  string
	now      func() time.Time

	closed   atomic.Bool
	minLevel atomic.Int64

	daemon atomic.Pointer[pipeline]

	mu      sync.RWMutex
	tenants map[domain.TenantID]*pipeline
	overlay map[string]bool

	subsMu sync.RWMutex
	subs   map[*subscriber]struct{}

	ring *recordRing

	tracer *sdktrace.TracerProvider

	healthStop chan struct{}
	healthDone chan struct{}
}

var (
	_ service.Observability = (*Manager)(nil)
	_ telemetry.Metrics     = (*Manager)(nil)
)

// New builds a Manager and starts the daemon-level sinks. A sink that is unknown or
// cannot open does not fail this call: it shows as failed in Sinks (I273). Errors
// are only for configuration that cannot mean anything: duplicate IDs, an unknown
// level or an unreadable state file.
func New(cfg config.Observability, opts ...Options) (*Manager, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	m := &Manager{
		opts:       o,
		tuning:     o.Tuning.withDefaults(),
		reg:        o.Registry,
		redactor:   redact.New(),
		service:    o.Service,
		now:        o.Now,
		tenants:    map[domain.TenantID]*pipeline{},
		overlay:    map[string]bool{},
		subs:       map[*subscriber]struct{}{},
		healthStop: make(chan struct{}),
		healthDone: make(chan struct{}),
	}
	if m.service == "" {
		m.service = "vidyomd"
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.reg == nil {
		m.reg = connector.NewRegistry()
		RegisterBuiltins(m.reg)
	}
	m.ring = newRecordRing(m.tuning.RingSize)
	if err := m.loadOverlay(); err != nil {
		return nil, err
	}
	level, err := parseLevel(cfg.Logs.Level)
	if err != nil {
		return nil, err
	}
	m.minLevel.Store(int64(level))
	m.applyDefaults(&cfg)
	daemon := &pipeline{m: m, level: level, loaded: true, ready: closedChan()}
	entries, err := m.buildEntries(cfg, daemon)
	if err != nil {
		return nil, err
	}
	daemon.logs, daemon.metrics, daemon.traces = entries.logs, entries.metrics, entries.traces
	m.daemon.Store(daemon)
	m.tracer = newTracerProvider(m)
	daemon.startAll()
	go m.healthLoop()
	return m, nil
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func parseLevel(text string) (slog.Level, error) {
	switch strings.ToLower(text) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", text)
}

func levelName(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// applyDefaults is D38: local JSONL logs and a Prometheus endpoint when nothing is
// configured. Explicit configuration is respected as it stands.
func (m *Manager) applyDefaults(cfg *config.Observability) {
	if len(cfg.Logs.Sinks) == 0 && m.opts.LogDir != "" {
		params, _ := json.Marshal(map[string]any{
			"path":          filepath.Join(m.opts.LogDir, "vidyomd.jsonl"),
			"rotation":      "daily",
			"retentionDays": m.opts.RetentionDays,
		})
		cfg.Logs.Sinks = []config.Sink{{ID: "local", Driver: "file", Enabled: true, Parameters: params}}
	}
	if len(cfg.Metrics) == 0 {
		cfg.Metrics = []config.Sink{{ID: "prometheus", Driver: "prometheus", Enabled: true}}
	}
}

type entrySet struct {
	logs, metrics, traces []*sinkEntry
}

func (m *Manager) buildEntries(cfg config.Observability, p *pipeline) (entrySet, error) {
	var set entrySet
	build := func(kind string, sinks []config.Sink) ([]*sinkEntry, error) {
		seen := map[string]bool{}
		out := make([]*sinkEntry, 0, len(sinks))
		for _, sink := range sinks {
			if sink.ID == "" {
				return nil, fmt.Errorf("a %s sink has no id", kind)
			}
			if seen[sink.ID] {
				return nil, fmt.Errorf("duplicate %s sink id %q", kind, sink.ID)
			}
			seen[sink.ID] = true
			out = append(out, m.newEntry(kind, sink, p))
		}
		return out, nil
	}
	var err error
	if set.logs, err = build(kindLog, cfg.Logs.Sinks); err != nil {
		return set, err
	}
	if set.metrics, err = build(kindMetrics, cfg.Metrics); err != nil {
		return set, err
	}
	if set.traces, err = build(kindTrace, cfg.Traces); err != nil {
		return set, err
	}
	return set, nil
}

// Emit records one log entry. It redacts, keeps the record locally and hands it to
// every interested sink without waiting for any of them.
func (m *Manager) Emit(ctx context.Context, rec connector.LogRecord) {
	if m.closed.Load() {
		return
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = m.now()
	}
	rec.Timestamp = rec.Timestamp.UTC()
	if rec.Service == "" {
		rec.Service = m.service
	}
	if level, err := parseLevel(rec.Level); err == nil {
		rec.Level = levelName(level)
	} else {
		rec.Level = "info"
	}
	if rec.Event == "" {
		rec.Event = "log"
	}
	if rec.TraceID == "" && ctx != nil {
		if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
			rec.TraceID = sc.TraceID().String()
		}
	}
	m.redactRecord(&rec)
	m.fanout(rec, nil)
}

func (m *Manager) redactRecord(rec *connector.LogRecord) {
	r := m.redactor
	rec.Message = r.String(rec.Message)
	rec.Event = r.String(rec.Event)
	rec.Status = r.String(rec.Status)
	rec.Service = r.String(rec.Service)
	rec.TenantID = r.String(rec.TenantID)
	rec.JobID = r.String(rec.JobID)
	rec.WorkflowID = r.String(rec.WorkflowID)
	rec.NodeID = r.String(rec.NodeID)
	rec.AssetID = r.String(rec.AssetID)
	rec.Fields = r.Fields(rec.Fields)
}

// fanout sends an already redacted record to the ring, subscribers and pipelines.
func (m *Manager) fanout(rec connector.LogRecord, exclude *sinkEntry) {
	m.ring.add(rec)
	m.broadcast(rec)
	level, _ := parseLevel(rec.Level)
	m.daemon.Load().pushLog(rec, level, exclude)
	if rec.TenantID != "" {
		if p := m.tenantPipe(domain.TenantID(rec.TenantID)); p != nil {
			p.pushLog(rec, level, exclude)
		}
	}
}

// failureRecord is §15.7's failure record. It never goes back into the failing sink.
func (m *Manager) failureRecord(e *sinkEntry, event, level, message string) {
	if m.closed.Load() {
		return
	}
	rec := connector.LogRecord{
		Timestamp: m.now().UTC(),
		Level:     level,
		Service:   m.service,
		Event:     event,
		Message:   message,
		TenantID:  string(e.pipeline.tenant),
		Fields:    map[string]any{"sink": e.cfg.ID, "driver": e.cfg.Driver, "kind": e.kind},
	}
	m.fanout(rec, e)
}

// ---- telemetry.Metrics ----

// Add implements telemetry.Metrics.
func (m *Manager) Add(name string, delta float64, labels telemetry.Labels) {
	m.record(telemetry.KindCounter, name, delta, labels)
}

// Set implements telemetry.Metrics.
func (m *Manager) Set(name string, value float64, labels telemetry.Labels) {
	m.record(telemetry.KindGauge, name, value, labels)
}

// Observe implements telemetry.Metrics.
func (m *Manager) Observe(name string, value float64, labels telemetry.Labels) {
	m.record(telemetry.KindHistogram, name, value, labels)
}

func (m *Manager) record(kind telemetry.Kind, name string, value float64, labels telemetry.Labels) {
	if m.closed.Load() {
		return
	}
	now := m.now()
	normalized := m.normalizeLabels(name, labels, now)
	point := connector.MetricPoint{Name: name, Kind: string(kind), Value: value, Labels: normalized, At: now}
	m.daemon.Load().pushMetric(point)
	tenant := normalized[telemetry.LabelTenant]
	if tenant != telemetry.SystemTenant {
		if p := m.tenantPipe(domain.TenantID(tenant)); p != nil {
			p.pushMetric(point)
		}
	}
}

// normalizeLabels makes every point carry tenant_id (D6) and applies the catalog's
// label set, so a stray label cannot explode cardinality (I277).
func (m *Manager) normalizeLabels(name string, labels telemetry.Labels, now time.Time) map[string]string {
	spec, known := telemetry.Spec(name)
	var out map[string]string
	if known {
		out = make(map[string]string, len(spec.Labels))
		for _, l := range spec.Labels {
			out[l] = labels[l]
		}
	} else {
		out = make(map[string]string, len(labels)+1)
		for k, v := range labels {
			out[k] = v
		}
	}
	if out[telemetry.LabelTenant] == "" {
		out[telemetry.LabelTenant] = telemetry.SystemTenant
	}
	if _, wantsMonth := out["month"]; wantsMonth && out["month"] == "" {
		out["month"] = now.UTC().Format("2006-01")
	}
	return out
}

// ---- Health ----

func (m *Manager) healthLoop() {
	defer close(m.healthDone)
	tick := time.NewTicker(m.tuning.HealthInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			m.publishHealth()
		case <-m.healthStop:
			return
		}
	}
}

// publishHealth turns pump counters into §10.3's exporter metrics. It runs on a ticker
// rather than from inside a pump, so a failing metrics exporter cannot feed itself.
func (m *Manager) publishHealth() {
	for _, p := range m.allPipelines() {
		for _, e := range p.entries() {
			if !e.isEnabled() {
				continue
			}
			st := e.stats()
			tenant := string(p.tenant)
			if tenant == "" {
				tenant = telemetry.SystemTenant
			}
			labels := telemetry.Labels{"exporter": e.cfg.ID, "kind": e.kind, telemetry.LabelTenant: tenant}
			m.Set(telemetry.ExporterBacklog, float64(st.Backlog), labels)
			m.Set(telemetry.ExporterDelay, st.Delay.Seconds(), labels)
			failures, dropped := e.takeDeltas(st)
			if failures > 0 {
				m.Add(telemetry.ExporterFailures, float64(failures), labels)
				e.recordPushFailure(st)
			}
			if dropped > 0 {
				m.Add(telemetry.ExporterDropped, float64(dropped), labels)
			}
		}
	}
}

func (m *Manager) allPipelines() []*pipeline {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pipeline, 0, len(m.tenants)+1)
	out = append(out, m.daemon.Load())
	for _, p := range m.tenants {
		out = append(out, p)
	}
	return out
}

// ---- Lifecycle ----

// Flush waits until every enabled sink has written what it has queued, or ctx ends.
func (m *Manager) Flush(ctx context.Context) error {
	if m.tracer != nil {
		_ = m.tracer.ForceFlush(ctx)
	}
	m.publishHealth()
	var errsOut []error
	for _, p := range m.allPipelines() {
		_ = p.waitLoaded(ctx)
		for _, e := range p.entries() {
			if err := e.flush(ctx); err != nil {
				errsOut = append(errsOut, fmt.Errorf("sink %s: %w", e.cfg.ID, err))
			}
		}
	}
	return errors.Join(errsOut...)
}

// Close drains sinks up to Tuning.DrainTimeout or ctx, whichever ends first, and
// abandons one that will not stop. It must be the last call.
func (m *Manager) Close(ctx context.Context) error {
	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(m.healthStop)
	<-m.healthDone
	shutdownCtx, cancel := context.WithTimeout(ctx, m.tuning.DrainTimeout)
	defer cancel()
	var out []error
	if m.tracer != nil {
		if err := m.tracer.Shutdown(shutdownCtx); err != nil {
			out = append(out, fmt.Errorf("tracer: %w", err))
		}
	}
	m.closeSubscribers()
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, p := range m.allPipelines() {
		for _, e := range p.entries() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := e.stop(shutdownCtx); err != nil {
					mu.Lock()
					out = append(out, fmt.Errorf("sink %s: %w", e.cfg.ID, err))
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	return errors.Join(out...)
}

// Reload replaces the sinks after a configuration change, as on SIGHUP (I34). Tenant
// pipelines rebuild on their next record. Queued entries of old sinks are drained.
func (m *Manager) Reload(ctx context.Context, cfg config.Observability) error {
	if m.closed.Load() {
		return errors.New("observability manager is closed")
	}
	level, err := parseLevel(cfg.Logs.Level)
	if err != nil {
		return err
	}
	m.applyDefaults(&cfg)
	next := &pipeline{m: m, level: level, loaded: true, ready: closedChan()}
	entries, err := m.buildEntries(cfg, next)
	if err != nil {
		return err
	}
	next.logs, next.metrics, next.traces = entries.logs, entries.metrics, entries.traces
	m.minLevel.Store(int64(level))
	m.mu.Lock()
	old := m.daemon.Swap(next)
	oldTenants := m.tenants
	m.tenants = map[domain.TenantID]*pipeline{}
	m.mu.Unlock()
	next.startAll()
	var out []error
	stopCtx, cancel := context.WithTimeout(ctx, m.tuning.DrainTimeout)
	defer cancel()
	stopAll := func(p *pipeline) {
		for _, e := range p.entries() {
			if err := e.stop(stopCtx); err != nil {
				out = append(out, fmt.Errorf("sink %s: %w", e.cfg.ID, err))
			}
		}
	}
	stopAll(old)
	for _, p := range oldTenants {
		stopAll(p)
	}
	return errors.Join(out...)
}

// Redactor exposes the manager's redactor, so exact secret values can be registered.
func (m *Manager) Redactor() *redact.Redactor { return m.redactor }

// ---- Enable and disable overlay (I279) ----

type overlayFile struct {
	Sinks map[string]bool `json:"sinks"`
}

func overlayKey(tenant domain.TenantID, id string) string {
	scope := string(tenant)
	if scope == "" {
		scope = daemonScope
	}
	return scope + "/" + id
}

func (m *Manager) loadOverlay() error {
	if m.opts.StateFile == "" {
		return nil
	}
	data, err := os.ReadFile(m.opts.StateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read observability state: %w", err)
	}
	var file overlayFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("read observability state: %w", err)
	}
	if file.Sinks != nil {
		m.overlay = file.Sinks
	}
	return nil
}

func (m *Manager) saveOverlay() error {
	if m.opts.StateFile == "" {
		return nil
	}
	m.mu.RLock()
	file := overlayFile{Sinks: make(map[string]bool, len(m.overlay))}
	for k, v := range m.overlay {
		file.Sinks[k] = v
	}
	m.mu.RUnlock()
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.opts.StateFile), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.opts.StateFile), ".observability-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), m.opts.StateFile)
}
