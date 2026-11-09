package observability

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

const (
	loadWait       = 2 * time.Second
	freshnessWait  = 500 * time.Millisecond
	metricsOpening = 2 * time.Second
)

func requireTenant(s service.Scope) error {
	if s.Tenant == "" {
		return errs.New(errs.CodeTenantRequired, "this operation needs a tenant")
	}
	return nil
}

// parseSince accepts RFC 3339 or a duration such as 24h, as `--since 24h` sends.
func (m *Manager) parseSince(text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(text); err == nil && d >= 0 {
		return m.now().Add(-d), nil
	}
	return time.Time{}, errs.New(errs.CodeInvalidArgument, "since must be RFC 3339 or a duration such as 24h")
}

func (m *Manager) buildQuery(s service.Scope, q api.LogQuery) (connector.LogQuery, int, error) {
	since, err := m.parseSince(q.Since)
	if err != nil {
		return connector.LogQuery{}, 0, err
	}
	minRank := 0
	if q.Level != "" {
		if _, err := parseLevel(q.Level); err != nil {
			return connector.LogQuery{}, 0, errs.New(errs.CodeInvalidArgument, "level must be debug, info, warn or error")
		}
		minRank = levelRank(q.Level)
	}
	return connector.LogQuery{
		TenantID: string(s.Tenant),
		Since:    since,
		MinLevel: q.Level,
		JobID:    q.JobID,
		Limit:    q.Limit,
		Cursor:   q.Cursor,
	}, minRank, nil
}

// querier finds the sink that keeps this tenant's records: its own, else the daemon's.
func (m *Manager) querier(ctx context.Context, tenant domain.TenantID) (connector.LogQuerier, *sinkEntry) {
	var candidates []*pipeline
	if p := m.tenantPipe(tenant); p != nil {
		waitCtx, cancel := context.WithTimeout(ctx, loadWait)
		_ = p.waitLoaded(waitCtx)
		cancel()
		candidates = append(candidates, p)
	}
	candidates = append(candidates, m.daemon.Load())
	for _, p := range candidates {
		p.mu.RLock()
		sinks := p.logs
		p.mu.RUnlock()
		for _, e := range sinks {
			if q, ok := e.currentLogSink().(connector.LogQuerier); ok {
				return q, e
			}
		}
	}
	return nil, nil
}

// Logs implements service.Observability: a tenant sees only its own records (D6), from
// the local file when there is one and from the in-memory ring otherwise (§10.1).
func (m *Manager) Logs(ctx context.Context, s service.Scope, q api.LogQuery) (api.List[connector.LogRecord], error) {
	if err := requireTenant(s); err != nil {
		return api.List[connector.LogRecord]{}, err
	}
	query, minRank, err := m.buildQuery(s, q)
	if err != nil {
		return api.List[connector.LogRecord]{}, err
	}
	querier, entry := m.querier(ctx, s.Tenant)
	if querier != nil {
		flushCtx, cancel := context.WithTimeout(ctx, freshnessWait)
		_ = entry.flush(flushCtx) // reading one's own writes is nice, not required
		cancel()
		page, err := querier.QueryLogs(ctx, query)
		if err == nil {
			return api.List[connector.LogRecord]{Items: orEmpty(page.Records), NextCursor: page.NextCursor}, nil
		}
		if errs.IsCode(err, errs.CodeInvalidArgument) {
			return api.List[connector.LogRecord]{}, err
		}
		m.failureRecord(entry, "observability.query_failed", "warn", m.redactor.Error(err))
	}
	page := m.ring.query(query, minRank)
	return api.List[connector.LogRecord]{Items: orEmpty(page.Records), NextCursor: page.NextCursor}, nil
}

func orEmpty(records []connector.LogRecord) []connector.LogRecord {
	if records == nil {
		return []connector.LogRecord{}
	}
	return records
}

// subscriber is one live GET /logs/stream.
type subscriber struct {
	ch      chan connector.LogRecord
	query   connector.LogQuery
	minRank int
	once    sync.Once
}

func (sub *subscriber) close() { sub.once.Do(func() { close(sub.ch) }) }

// SubscribeLogs implements service.Observability. A slow reader loses records rather
// than slowing anything down. The channel closes when ctx ends or the manager closes.
func (m *Manager) SubscribeLogs(ctx context.Context, s service.Scope, q api.LogQuery) (<-chan connector.LogRecord, error) {
	if err := requireTenant(s); err != nil {
		return nil, err
	}
	query, minRank, err := m.buildQuery(s, q)
	if err != nil {
		return nil, err
	}
	sub := &subscriber{ch: make(chan connector.LogRecord, subscriberBuffer), query: query, minRank: minRank}
	if !query.Since.IsZero() {
		for _, rec := range m.ring.since(query, minRank, query.Since) {
			select {
			case sub.ch <- rec:
			default:
			}
		}
	}
	m.subsMu.Lock()
	if m.closed.Load() {
		m.subsMu.Unlock()
		sub.close()
		return sub.ch, nil
	}
	m.subs[sub] = struct{}{}
	m.subsMu.Unlock()
	context.AfterFunc(ctx, func() {
		m.subsMu.Lock()
		delete(m.subs, sub)
		sub.close()
		m.subsMu.Unlock()
	})
	return sub.ch, nil
}

func (m *Manager) broadcast(rec connector.LogRecord) {
	m.subsMu.RLock()
	defer m.subsMu.RUnlock()
	for sub := range m.subs {
		if !matchesQuery(&rec, &sub.query, sub.minRank) {
			continue
		}
		select {
		case sub.ch <- rec:
		default:
		}
	}
}

func (m *Manager) closeSubscribers() {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for sub := range m.subs {
		sub.close()
		delete(m.subs, sub)
	}
}

// visible returns the entries this scope may see: the tenant's own, plus the daemon's
// for a system admin (I272).
func (m *Manager) visible(ctx context.Context, s service.Scope) ([]*sinkEntry, error) {
	var out []*sinkEntry
	if s.Tenant != "" {
		if p := m.tenantPipe(s.Tenant); p != nil {
			waitCtx, cancel := context.WithTimeout(ctx, loadWait)
			err := p.waitLoaded(waitCtx)
			cancel()
			if err != nil {
				return nil, errs.Wrap(errs.CodeUnavailable, err, "tenant observability configuration is not available")
			}
			out = append(out, p.entries()...)
		}
	}
	if s.Principal.SystemAdmin {
		out = append(out, m.daemon.Load().entries()...)
	}
	return out, nil
}

// Sinks implements service.Observability.
func (m *Manager) Sinks(ctx context.Context, s service.Scope) ([]api.ObservabilitySink, error) {
	entries, err := m.visible(ctx, s)
	if err != nil {
		return nil, err
	}
	out := make([]api.ObservabilitySink, 0, len(entries))
	for _, e := range entries {
		status, st := e.status()
		out = append(out, api.ObservabilitySink{
			ID:        e.cfg.ID,
			Kind:      e.kind,
			Driver:    e.cfg.Driver,
			Enabled:   e.isEnabled(),
			Status:    status,
			Backlog:   st.Backlog,
			Dropped:   st.Dropped,
			LastError: st.LastError,
		})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		return out[a].ID < out[b].ID
	})
	return out, nil
}

func (m *Manager) matching(ctx context.Context, s service.Scope, sinkID string) ([]*sinkEntry, error) {
	entries, err := m.visible(ctx, s)
	if err != nil {
		return nil, err
	}
	var out []*sinkEntry
	for _, e := range entries {
		if e.cfg.ID == sinkID {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, errs.NotFound("observability sink")
	}
	return out, nil
}

// TestSink implements service.Observability. It writes directly and reports the sink's
// answer, with secrets removed.
func (m *Manager) TestSink(ctx context.Context, s service.Scope, sinkID string) error {
	entries, err := m.matching(ctx, s, sinkID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		testCtx, cancel := context.WithTimeout(ctx, m.tuning.WriteTimeout)
		err := e.test(testCtx)
		cancel()
		if err != nil {
			return errs.Wrap(errs.CodeUnavailable, err, fmt.Sprintf("sink %q test failed: %s", sinkID, m.redactor.Error(err)))
		}
	}
	return nil
}

// SetSinkEnabled implements service.Observability. It is audited first, so a mutation
// that cannot be audited does not happen (§12.1), and it survives a restart when a
// state file is configured (I279).
func (m *Manager) SetSinkEnabled(ctx context.Context, s service.Scope, sinkID string, enabled bool) error {
	entries, err := m.matching(ctx, s, sinkID)
	if err != nil {
		return err
	}
	if m.opts.Auditor == nil {
		return errs.New(errs.CodeFailedPrecondition, "changing a sink needs the audit trail, which is not wired")
	}
	action := "observability.disable"
	if enabled {
		action = "observability.enable"
	}
	details := map[string]any{"sink": sinkID, "persisted": m.opts.StateFile != ""}
	if err := m.opts.Auditor.Record(ctx, security.AuditEntry{
		TenantID:   s.Tenant,
		Principal:  s.Principal,
		Action:     action,
		Permission: domain.PermObservabilityManage,
		Resource:   "observability-sink:" + sinkID,
		Result:     "allowed",
		RequestID:  s.RequestID,
		Details:    details,
	}); err != nil {
		return err
	}
	m.mu.Lock()
	for _, e := range entries {
		m.overlay[overlayKey(e.pipeline.tenant, sinkID)] = enabled
	}
	m.mu.Unlock()
	if err := m.saveOverlay(); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot save the sink state")
	}
	for _, e := range entries {
		stopCtx, cancel := context.WithTimeout(ctx, m.tuning.DrainTimeout)
		err := e.setEnabled(stopCtx, enabled)
		cancel()
		if err != nil {
			return errs.Wrap(errs.CodeUnavailable, err, "the sink did not stop in time; it will not receive more records")
		}
	}
	return nil
}

// MetricsHandler implements service.Observability. It serves the daemon's pull
// exporter, which answers with one tenant's series when the request carries the
// Vidyom-Tenant header. The API layer must set it for anyone but a system admin (I272).
func (m *Manager) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline := time.NewTimer(metricsOpening)
		defer deadline.Stop()
		for {
			handler, configured := m.pullHandler()
			if handler != nil {
				handler.ServeHTTP(w, r)
				return
			}
			if !configured {
				http.Error(w, "no metrics exporter serves this endpoint", http.StatusNotFound)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-deadline.C:
				http.Error(w, "metrics exporter is not ready", http.StatusServiceUnavailable)
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
}

func (m *Manager) pullHandler() (http.Handler, bool) {
	daemon := m.daemon.Load()
	daemon.mu.RLock()
	sinks := daemon.metrics
	daemon.mu.RUnlock()
	configured := false
	for _, e := range sinks {
		if !e.isEnabled() || !m.isPullExporter(e.cfg.Driver) {
			continue
		}
		configured = true
		if h, ok := e.currentExporter().(connector.MetricsHandler); ok {
			return h.Handler(), true
		}
	}
	return nil, configured
}
