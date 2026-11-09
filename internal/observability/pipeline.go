package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// pipeline is one level of routing: the daemon's, or one tenant's (I272).
type pipeline struct {
	m      *Manager
	tenant domain.TenantID
	level  slog.Level

	mu      sync.RWMutex
	loaded  bool
	loading bool
	loadErr string
	failed  time.Time
	ready   chan struct{}
	pending []connector.LogRecord
	logs    []*sinkEntry
	metrics []*sinkEntry
	traces  []*sinkEntry
}

func (p *pipeline) entries() []*sinkEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*sinkEntry, 0, len(p.logs)+len(p.metrics)+len(p.traces))
	out = append(out, p.logs...)
	out = append(out, p.metrics...)
	return append(out, p.traces...)
}

func (p *pipeline) startAll() {
	for _, e := range p.entries() {
		e.start()
	}
}

func (p *pipeline) pushLog(rec connector.LogRecord, level slog.Level, exclude *sinkEntry) {
	p.mu.Lock()
	if !p.loaded {
		// Off the record path a tenant's sinks are still loading: keep a little.
		if len(p.pending) >= pendingLogLimit {
			p.pending = p.pending[1:]
		}
		p.pending = append(p.pending, rec)
		p.mu.Unlock()
		return
	}
	sinks := p.logs
	threshold := p.level
	p.mu.Unlock()
	if level < threshold {
		return
	}
	for _, e := range sinks {
		if e != exclude {
			e.pushLog(rec)
		}
	}
}

func (p *pipeline) pushMetric(point connector.MetricPoint) {
	p.mu.RLock()
	sinks := p.metrics
	p.mu.RUnlock()
	for _, e := range sinks {
		e.pushMetric(point)
	}
}

func (p *pipeline) pushSpan(span connector.SpanRecord) {
	p.mu.RLock()
	sinks := p.traces
	p.mu.RUnlock()
	for _, e := range sinks {
		e.pushSpan(span)
	}
}

// waitLoaded waits for a tenant's sinks to be known.
func (p *pipeline) waitLoaded(ctx context.Context) error {
	p.mu.RLock()
	ready := p.ready
	p.mu.RUnlock()
	select {
	case <-ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.loaded {
		return errors.New(p.loadErr)
	}
	return nil
}

// tenantPipe returns the tenant's pipeline, starting a load off the caller's path. A
// slug that cannot be a tenant, or too many of them, gets none: records still reach
// the daemon's sinks.
func (m *Manager) tenantPipe(tenant domain.TenantID) *pipeline {
	if m.opts.Tenants == nil || !tenantSlug.MatchString(string(tenant)) {
		return nil
	}
	m.mu.RLock()
	p := m.tenants[tenant]
	m.mu.RUnlock()
	if p != nil {
		p.maybeRetry()
		return p
	}
	m.mu.Lock()
	if m.closed.Load() {
		m.mu.Unlock()
		return nil
	}
	if p = m.tenants[tenant]; p == nil {
		if len(m.tenants) >= maxTenantPipes {
			m.mu.Unlock()
			return nil
		}
		p = &pipeline{m: m, tenant: tenant, loading: true, ready: make(chan struct{})}
		m.tenants[tenant] = p
		go p.load()
	}
	m.mu.Unlock()
	return p
}

func (p *pipeline) maybeRetry() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded || p.loading || p.failed.IsZero() || p.m.now().Sub(p.failed) < p.m.tuning.OpenRetry {
		return
	}
	p.loading = true
	p.ready = make(chan struct{})
	go p.load()
}

func (p *pipeline) load() {
	m := p.m
	ctx, cancel := context.WithTimeout(context.Background(), m.tuning.OpenTimeout)
	defer cancel()
	entries, level, err := p.fetch(ctx)
	if err == nil && m.closed.Load() {
		err = errors.New("observability manager is closed")
	}

	p.mu.Lock()
	ready := p.ready
	p.loading = false
	if err != nil {
		p.loadErr = m.redactor.String(err.Error())
		p.failed = m.now()
		p.mu.Unlock()
		close(ready)
		return
	}
	p.level = level
	p.logs, p.metrics, p.traces = entries.logs, entries.metrics, entries.traces
	p.loaded = true
	pending := p.pending
	p.pending = nil
	p.mu.Unlock()

	if int64(level) < m.minLevel.Load() {
		m.minLevel.Store(int64(level))
	}
	p.startAll()
	for _, rec := range pending {
		recLevel, _ := parseLevel(rec.Level)
		p.pushLog(rec, recLevel, nil)
	}
	close(ready)
	if m.closed.Load() {
		// Close ran while this was loading and could not see these sinks.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), m.tuning.DrainTimeout)
		defer stopCancel()
		for _, e := range p.entries() {
			_ = e.stop(stopCtx)
		}
	}
}

func (p *pipeline) fetch(ctx context.Context) (set entrySet, level slog.Level, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tenant configuration panicked: %v", r)
		}
	}()
	cfg, err := p.m.opts.Tenants.TenantConfig(ctx, p.tenant)
	if err != nil {
		return set, 0, err
	}
	level, err = parseLevel(cfg.Observability.Logs.Level)
	if err != nil {
		return set, 0, err
	}
	set, err = p.m.buildEntries(cfg.Observability, p)
	return set, level, err
}

// sinkEntry is one configured sink in one pipeline.
type sinkEntry struct {
	m        *Manager
	pipeline *pipeline
	kind     string
	cfg      config.Sink
	secrets  connector.SecretResolver

	mu      sync.RWMutex
	enabled bool
	failure string // why it cannot run: unknown driver or refused configuration
	note    string // a state that is not a failure
	running bool

	logPump    *pump[connector.LogRecord]
	metricPump *pump[connector.MetricPoint]
	spanPump   *pump[connector.SpanRecord]

	openMu   sync.RWMutex
	logSink  connector.LogSink
	exporter connector.MetricsExporter
	traceExp connector.TraceExporter

	published struct {
		failures int64
		dropped  int64
	}
	lastPushRecord time.Time
}

func (m *Manager) newEntry(kind string, cfg config.Sink, p *pipeline) *sinkEntry {
	e := &sinkEntry{m: m, pipeline: p, kind: kind, cfg: cfg, enabled: cfg.Enabled}
	if p.tenant == "" {
		e.secrets = m.opts.Secrets
	} else if m.opts.TenantSecrets != nil {
		e.secrets = m.opts.TenantSecrets(p.tenant)
	}
	m.mu.RLock()
	if enabled, ok := m.overlay[overlayKey(p.tenant, cfg.ID)]; ok {
		e.enabled = enabled
	}
	m.mu.RUnlock()
	if err := m.checkDriver(kind, cfg.Driver); err != nil {
		e.failure = m.redactor.Error(err)
	}
	var params map[string]any
	if len(cfg.Parameters) > 0 && json.Unmarshal(cfg.Parameters, &params) == nil {
		for key := range params {
			if inlineCredential.MatchString(key) {
				e.failure = fmt.Sprintf("parameter %q looks like an inline credential: use credentialsRef", key)
			}
		}
	}
	if p.tenant != "" && kind == kindMetrics && m.isPullExporter(cfg.Driver) {
		e.note = "via daemon endpoint"
	}
	return e
}

func (m *Manager) checkDriver(kind, driver string) error {
	var err error
	switch kind {
	case kindLog:
		_, err = m.reg.LogSink(driver)
	case kindMetrics:
		_, err = m.reg.MetricsExporter(driver)
	case kindTrace:
		_, err = m.reg.TraceExporter(driver)
	}
	return err
}

func (m *Manager) isPullExporter(driver string) bool {
	for _, info := range m.reg.List() {
		if info.Family == connector.FamilyMetricsExporter && info.Driver == driver {
			return info.Capabilities["pull"]
		}
	}
	return false
}

func (e *sinkEntry) isEnabled() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.enabled
}

func (e *sinkEntry) sanitize(s string) string { return e.m.redactor.String(s) }

func (e *sinkEntry) tuningFor() Tuning {
	t := e.m.tuning
	if e.kind == kindMetrics && e.m.isPullExporter(e.cfg.Driver) {
		// In memory and read at scrape time: keep it fresh.
		t.FlushInterval = min(t.FlushInterval, 100*time.Millisecond)
	}
	return t
}

// start begins delivering, if this sink is enabled and able to run.
func (e *sinkEntry) start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running || !e.enabled || e.failure != "" || e.note != "" {
		return
	}
	switch e.kind {
	case kindLog:
		e.startLogPump()
	case kindMetrics:
		e.startMetricPump()
	case kindTrace:
		e.startSpanPump()
	}
	e.running = true
}

func wireFailure[T any](e *sinkEntry, p *pump[T]) {
	p.onFailure = func(message string) {
		e.m.failureRecord(e, "observability.sink_failed", "warn", message)
	}
	p.onRecover = func() {
		e.m.failureRecord(e, "observability.sink_recovered", "info", "sink is delivering again")
	}
}

func (e *sinkEntry) startLogPump() {
	factory, _ := e.m.reg.LogSink(e.cfg.Driver)
	p := newPump(pumpFuncs[connector.LogRecord]{
		open: func(ctx context.Context) error {
			sink, err := factory(ctx, e.cfg, e.secrets)
			if err != nil {
				return err
			}
			e.openMu.Lock()
			e.logSink = sink
			e.openMu.Unlock()
			return nil
		},
		write: func(ctx context.Context, batch []connector.LogRecord) error {
			return e.currentLogSink().Write(ctx, batch)
		},
		flush: func(ctx context.Context) error { return e.currentLogSink().Flush(ctx) },
		close: func(ctx context.Context) error {
			sink := e.currentLogSink()
			e.openMu.Lock()
			e.logSink = nil
			e.openMu.Unlock()
			return sink.Close(ctx)
		},
	}, e.tuningFor(), e.m.now, e.sanitize)
	wireFailure(e, p)
	e.logPump = p
	p.start()
}

func (e *sinkEntry) currentLogSink() connector.LogSink {
	e.openMu.RLock()
	defer e.openMu.RUnlock()
	return e.logSink
}

func (e *sinkEntry) startMetricPump() {
	factory, _ := e.m.reg.MetricsExporter(e.cfg.Driver)
	p := newPump(pumpFuncs[connector.MetricPoint]{
		open: func(ctx context.Context) error {
			exp, err := factory(ctx, e.cfg, e.secrets)
			if err != nil {
				return err
			}
			e.openMu.Lock()
			e.exporter = exp
			e.openMu.Unlock()
			return nil
		},
		write: func(ctx context.Context, batch []connector.MetricPoint) error {
			return e.currentExporter().Export(ctx, batch)
		},
		close: func(ctx context.Context) error {
			exp := e.currentExporter()
			e.openMu.Lock()
			e.exporter = nil
			e.openMu.Unlock()
			return exp.Close(ctx)
		},
	}, e.tuningFor(), e.m.now, e.sanitize)
	wireFailure(e, p)
	e.metricPump = p
	p.start()
}

func (e *sinkEntry) currentExporter() connector.MetricsExporter {
	e.openMu.RLock()
	defer e.openMu.RUnlock()
	return e.exporter
}

func (e *sinkEntry) startSpanPump() {
	factory, _ := e.m.reg.TraceExporter(e.cfg.Driver)
	p := newPump(pumpFuncs[connector.SpanRecord]{
		open: func(ctx context.Context) error {
			exp, err := factory(ctx, e.cfg, e.secrets)
			if err != nil {
				return err
			}
			e.openMu.Lock()
			e.traceExp = exp
			e.openMu.Unlock()
			return nil
		},
		write: func(ctx context.Context, batch []connector.SpanRecord) error {
			return e.currentTraceExporter().ExportSpans(ctx, batch)
		},
		close: func(ctx context.Context) error {
			exp := e.currentTraceExporter()
			e.openMu.Lock()
			e.traceExp = nil
			e.openMu.Unlock()
			return exp.Close(ctx)
		},
	}, e.tuningFor(), e.m.now, e.sanitize)
	wireFailure(e, p)
	e.spanPump = p
	p.start()
}

func (e *sinkEntry) currentTraceExporter() connector.TraceExporter {
	e.openMu.RLock()
	defer e.openMu.RUnlock()
	return e.traceExp
}

func (e *sinkEntry) pushLog(rec connector.LogRecord) {
	e.mu.RLock()
	p := e.logPump
	running := e.running
	e.mu.RUnlock()
	if running && p != nil {
		p.push(rec)
	}
}

func (e *sinkEntry) pushMetric(point connector.MetricPoint) {
	e.mu.RLock()
	p := e.metricPump
	running := e.running
	e.mu.RUnlock()
	if running && p != nil {
		p.push(point)
	}
}

func (e *sinkEntry) pushSpan(span connector.SpanRecord) {
	e.mu.RLock()
	p := e.spanPump
	running := e.running
	e.mu.RUnlock()
	if running && p != nil {
		p.push(span)
	}
}

// pushHealth is implemented by an exporter whose failures do not come back through
// Export: the otlp metrics exporter's periodic pushes.
type pushHealth interface {
	PushHealth() (failures int64, lastError string)
}

func (e *sinkEntry) stats() pumpStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var st pumpStats
	switch {
	case e.logPump != nil:
		st = e.logPump.stats()
	case e.metricPump != nil:
		st = e.metricPump.stats()
		if h, ok := e.currentExporter().(pushHealth); ok {
			failures, last := h.PushHealth()
			st.Failures += failures
			if last != "" {
				st.Failing = true
				st.LastError = e.sanitize(last)
			}
		}
	case e.spanPump != nil:
		st = e.spanPump.stats()
	}
	return st
}

func (e *sinkEntry) takeDeltas(st pumpStats) (failures, dropped int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// A restarted pump's counters begin again from zero.
	if st.Failures < e.published.failures {
		e.published.failures = 0
	}
	if st.Dropped < e.published.dropped {
		e.published.dropped = 0
	}
	failures = st.Failures - e.published.failures
	dropped = st.Dropped - e.published.dropped
	e.published.failures = st.Failures
	e.published.dropped = st.Dropped
	return failures, dropped
}

// recordPushFailure writes the failure record for a failure the pump never saw,
// because it happened in an exporter's own periodic push. Same rate as the pump's.
func (e *sinkEntry) recordPushFailure(st pumpStats) {
	if _, ok := e.currentExporter().(pushHealth); !ok || st.LastError == "" {
		return
	}
	now := e.m.now()
	e.mu.Lock()
	due := now.Sub(e.lastPushRecord) >= e.m.tuning.FailureEvery
	if due {
		e.lastPushRecord = now
	}
	e.mu.Unlock()
	if due {
		e.m.failureRecord(e, "observability.sink_failed", "warn", st.LastError)
	}
}

func (e *sinkEntry) flush(ctx context.Context) error {
	e.mu.RLock()
	lp, mp, sp, running := e.logPump, e.metricPump, e.spanPump, e.running
	e.mu.RUnlock()
	if !running {
		return nil
	}
	switch {
	case lp != nil:
		return lp.flushCtx(ctx)
	case mp != nil:
		return mp.flushCtx(ctx)
	case sp != nil:
		return sp.flushCtx(ctx)
	}
	return nil
}

func (e *sinkEntry) stop(ctx context.Context) error {
	e.mu.Lock()
	lp, mp, sp := e.logPump, e.metricPump, e.spanPump
	e.logPump, e.metricPump, e.spanPump = nil, nil, nil
	e.running = false
	e.mu.Unlock()
	switch {
	case lp != nil:
		return lp.stop(ctx)
	case mp != nil:
		return mp.stop(ctx)
	case sp != nil:
		return sp.stop(ctx)
	}
	return nil
}

func (e *sinkEntry) setEnabled(ctx context.Context, enabled bool) error {
	e.mu.Lock()
	e.enabled = enabled
	e.mu.Unlock()
	if enabled {
		e.start()
		return nil
	}
	return e.stop(ctx)
}

// status is the state GET /observability reports.
func (e *sinkEntry) status() (status string, st pumpStats) {
	e.mu.RLock()
	enabled, failure, note, running := e.enabled, e.failure, e.note, e.running
	e.mu.RUnlock()
	switch {
	case failure != "":
		if !enabled {
			return "disabled", st
		}
		st.LastError = failure
		return "failed", st
	case !enabled:
		return "disabled", st
	case note != "":
		return note, st
	case !running:
		return "stopped", st
	}
	st = e.stats()
	switch {
	case st.Stuck:
		return "stuck", st
	case !st.Opened && st.LastError != "":
		return "failed", st
	case !st.Opened:
		return "starting", st
	case st.Failing:
		return "degraded", st
	}
	return "ok", st
}

// exporterTester is implemented by a metrics exporter whose Export does not touch the
// network, so a test has to ask it to push.
type exporterTester interface {
	TestExport(ctx context.Context) error
}

func newTraceID() (traceID, spanID string) {
	var t [16]byte
	var s [8]byte
	_, _ = rand.Read(t[:])
	_, _ = rand.Read(s[:])
	return hex.EncodeToString(t[:]), hex.EncodeToString(s[:])
}

// test writes one record, point or span. A running sink's pump serializes it with its
// own writes; a disabled sink opens a temporary instance (I279).
func (e *sinkEntry) test(ctx context.Context) error {
	e.mu.RLock()
	failure, note, running := e.failure, e.note, e.running
	lp, mp, sp := e.logPump, e.metricPump, e.spanPump
	e.mu.RUnlock()
	if failure != "" {
		return errs.Newf(errs.CodeUnavailable, "sink %q cannot run: %s", e.cfg.ID, failure)
	}
	if note != "" {
		return nil
	}
	now := e.m.now().UTC()
	tenant := string(e.pipeline.tenant)
	logRec := connector.LogRecord{Timestamp: now, Level: "info", Service: e.m.service, Event: "observability.test", TenantID: tenant, Message: "test record"}
	point := connector.MetricPoint{Name: "vidyom_observability_test", Kind: "gauge", Value: 1, Labels: map[string]string{"tenant_id": e.tenantLabel()}, At: now}
	traceID, spanID := newTraceID()
	span := connector.SpanRecord{TraceID: traceID, SpanID: spanID, Name: "observability.test", Start: now, End: now, Attributes: map[string]any{"tenant_id": tenant}, Status: "Ok"}
	switch e.kind {
	case kindLog:
		if running && lp != nil {
			return lp.direct(ctx, []connector.LogRecord{logRec})
		}
		factory, err := e.m.reg.LogSink(e.cfg.Driver)
		if err != nil {
			return err
		}
		sink, err := factory(ctx, e.cfg, e.secrets)
		if err != nil {
			return err
		}
		defer sink.Close(ctx)
		if err := sink.Write(ctx, []connector.LogRecord{logRec}); err != nil {
			return err
		}
		return sink.Flush(ctx)
	case kindMetrics:
		exp := e.currentExporter()
		if running && mp != nil && exp != nil {
			if err := mp.direct(ctx, []connector.MetricPoint{point}); err != nil {
				return err
			}
			if t, ok := exp.(exporterTester); ok {
				return t.TestExport(ctx)
			}
			return nil
		}
		factory, err := e.m.reg.MetricsExporter(e.cfg.Driver)
		if err != nil {
			return err
		}
		temp, err := factory(ctx, e.cfg, e.secrets)
		if err != nil {
			return err
		}
		defer temp.Close(ctx)
		if err := temp.Export(ctx, []connector.MetricPoint{point}); err != nil {
			return err
		}
		if t, ok := temp.(exporterTester); ok {
			return t.TestExport(ctx)
		}
		return nil
	case kindTrace:
		if running && sp != nil {
			return sp.direct(ctx, []connector.SpanRecord{span})
		}
		factory, err := e.m.reg.TraceExporter(e.cfg.Driver)
		if err != nil {
			return err
		}
		temp, err := factory(ctx, e.cfg, e.secrets)
		if err != nil {
			return err
		}
		defer temp.Close(ctx)
		return temp.ExportSpans(ctx, []connector.SpanRecord{span})
	}
	return nil
}

func (e *sinkEntry) tenantLabel() string {
	if e.pipeline.tenant == "" {
		return telemetry.SystemTenant
	}
	return string(e.pipeline.tenant)
}
