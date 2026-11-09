package events

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/internal/workflow"
)

// Limits of the bus (I156).
const (
	DefaultFanOutLimit  = 32
	DefaultMaxAttempts  = 8
	DefaultBackoffBase  = 2 * time.Second
	DefaultBackoffMax   = 5 * time.Minute
	DefaultPollInterval = 250 * time.Millisecond
	defaultBatch        = 100
	workflowSubscriber  = "workflow:"
	pluginSubscriber    = "plugin:"
	reasonDepth         = "dropped: event chain deeper than the limit"
	reasonFilter        = "dropped: trigger filter did not match"
	reasonFanOut        = "dropped: more subscribers than the fan-out limit"
)

// TriggerSource lists the workflows with an event trigger. *workflow.Registry does.
type TriggerSource interface {
	Triggered(t wfdef.TriggerType) []*workflow.Entry
}

// StartRequest asks the engine to start an event-triggered job.
type StartRequest struct {
	Tenant         domain.TenantID
	WorkflowID     string
	Version        string
	Event          event.Envelope
	IdempotencyKey string
}

// JobStarter starts event-triggered jobs. Component jobs implements it. Starting
// with a known idempotency key starts nothing and succeeds.
type JobStarter interface {
	StartEventJob(ctx context.Context, req StartRequest) error
}

// Options configures a Bus.
type Options struct {
	Tenants  repo.TenantStores
	Triggers TriggerSource
	Starter  JobStarter
	// Plugins delivers to plugin subscribers. Nil means no plugin subscribes.
	Plugins plugin.EventSubscriber
	// MaxDepth overrides event.MaxCausationDepth.
	MaxDepth     int
	FanOutLimit  int
	MaxAttempts  int
	BackoffBase  time.Duration
	BackoffMax   time.Duration
	PollInterval time.Duration
	Logger       *slog.Logger
	Clock        func() time.Time
}

// Bus implements service.Events and the dispatcher.
type Bus struct {
	opts Options
	mu   sync.Mutex
	subs map[chan struct{}]domain.TenantID
	wake chan struct{}
}

var _ service.Events = (*Bus)(nil)

// New returns a bus. Run starts its dispatcher.
func New(opts Options) *Bus {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = event.MaxCausationDepth
	}
	if opts.FanOutLimit <= 0 {
		opts.FanOutLimit = DefaultFanOutLimit
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = DefaultBackoffBase
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = DefaultBackoffMax
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &Bus{opts: opts, subs: map[chan struct{}]domain.TenantID{}, wake: make(chan struct{}, 1)}
}

// SetStarter binds the engine after both exist.
func (b *Bus) SetStarter(s JobStarter) { b.opts.Starter = s }

// Source is what an event knows about the job that produced it.
type Source struct {
	JobID       string
	WorkflowID  string
	TraceID     string
	CausationID string
	Depth       int
}

// JobSource builds a Source from a job. Its events continue the chain that started
// it, so a loop through several workflows reaches the depth limit (§8.2).
func JobSource(j domain.Job) Source {
	cause := j.Trigger.EventID
	if cause == "" {
		cause = string(j.ID)
	}
	return Source{JobID: string(j.ID), WorkflowID: j.WorkflowID, TraceID: j.TraceID, CausationID: cause, Depth: j.Trigger.Depth}
}

// DeterministicID derives an event ID from a stable key (I156). The time part is
// that of the given ULID, so ordering stays close to the real one.
func DeterministicID(at id.ULID, key string) id.ULID {
	sum := sha256.Sum256([]byte(key))
	var u ulid.ULID
	parsed, err := ulid.ParseStrict(string(at))
	if err == nil {
		copy(u[:6], parsed[:6])
	}
	copy(u[6:], sum[:10])
	return id.ULID(u.String())
}

// Emit writes an event to the outbox through tx, which must be the transaction that
// makes the state change when there is one (§8.2). A draft with an ID that exists
// already writes nothing, which makes re-runs of idempotent producers quiet.
func (b *Bus) Emit(ctx context.Context, tx repo.TenantRepos, tenant domain.TenantID, src Source, d event.Draft) (id.ULID, error) {
	return Append(ctx, tx, tenant, src, d, b.opts.Clock())
}

// Append is Emit without a Bus, for tests and for code that has only a repository.
func Append(ctx context.Context, tx repo.TenantRepos, tenant domain.TenantID, src Source, d event.Draft, now time.Time) (id.ULID, error) {
	if d.Type == "" {
		return "", errs.New(errs.CodeInvalidArgument, "an event needs a type")
	}
	if d.ID != "" {
		if _, err := tx.Outbox().GetEvent(ctx, d.ID); err == nil {
			return d.ID, nil
		} else if !errs.IsCode(err, errs.CodeNotFound) {
			return "", err
		}
	}
	var payload json.RawMessage
	if d.Payload != nil {
		data, err := json.Marshal(d.Payload)
		if err != nil {
			return "", errs.Wrap(errs.CodeInvalidArgument, err, "event payload does not encode")
		}
		payload = data
	}
	eventID := d.ID
	if eventID == "" {
		eventID = id.New()
	}
	env := event.Envelope{
		EventID:        eventID,
		Type:           d.Type,
		SchemaVersion:  event.SchemaVersion,
		OccurredAt:     now,
		TenantID:       tenant,
		JobID:          src.JobID,
		WorkflowID:     src.WorkflowID,
		AssetID:        d.AssetID,
		TraceID:        src.TraceID,
		CausationID:    src.CausationID,
		CausationDepth: src.Depth,
		Payload:        payload,
	}
	if _, err := tx.Outbox().Append(ctx, env); err != nil {
		return "", err
	}
	return eventID, nil
}

// Notify wakes the dispatcher and stream readers after a commit. It is an
// optimization only: both also poll.
func (b *Bus) Notify() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Run dispatches until ctx ends.
func (b *Bus) Run(ctx context.Context) error {
	ticker := time.NewTicker(b.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-b.wake:
		}
		if err := b.Tick(ctx); err != nil && ctx.Err() == nil {
			b.opts.Logger.Warn("event dispatch failed", "error", err)
		}
	}
}

// Tick dispatches and delivers once, for every tenant. Tests call it directly.
func (b *Bus) Tick(ctx context.Context) error {
	tenants, err := b.opts.Tenants.Tenants(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, t := range tenants {
		store, err := b.opts.Tenants.ForTenant(ctx, t)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if err := b.dispatch(ctx, t, store); err != nil && first == nil {
			first = err
		}
		if err := b.deliver(ctx, t, store); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// dispatch turns outbox entries into deliveries, in one transaction per entry, so a
// crash either did both or neither.
func (b *Bus) dispatch(ctx context.Context, tenant domain.TenantID, store repo.TenantStore) error {
	for {
		entries, err := store.Outbox().Pending(ctx, defaultBatch)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		for _, entry := range entries {
			deliveries, err := b.subscribersOf(ctx, tenant, entry.Event)
			if err != nil {
				return err
			}
			if err := store.InTx(ctx, func(tx repo.TenantRepos) error {
				for _, d := range deliveries {
					if err := tx.Outbox().PutDelivery(ctx, d); err != nil {
						return err
					}
				}
				return tx.Outbox().MarkDispatched(ctx, entry.Seq)
			}); err != nil {
				return err
			}
		}
	}
}

func (b *Bus) subscribersOf(ctx context.Context, tenant domain.TenantID, e event.Envelope) ([]event.Delivery, error) {
	var names []string
	dropped := map[string]string{}
	if b.opts.Triggers != nil {
		for _, wf := range b.opts.Triggers.Triggered(wfdef.TriggerEvent) {
			trig := wf.Definition.Trigger
			if event.Type(trig.Event) != e.Type {
				continue
			}
			name := workflowSubscriber + wf.Definition.ID
			names = append(names, name)
			if trig.Filter == "" {
				continue
			}
			match, err := b.matches(ctx, trig.Filter, e)
			if err != nil {
				dropped[name] = "dropped: trigger filter failed: " + errs.Public(err).Message
				b.opts.Logger.Warn("event trigger filter failed", "workflow", wf.Definition.ID, "error", err)
			} else if !match {
				dropped[name] = reasonFilter
			}
		}
	}
	if b.opts.Plugins != nil {
		ids, err := b.opts.Plugins.EventSubscribers(ctx, tenant, e.Type)
		if err != nil {
			return nil, err
		}
		for _, p := range ids {
			names = append(names, pluginSubscriber+p)
		}
	}
	sort.Strings(names)
	out := make([]event.Delivery, 0, len(names))
	active := 0
	for _, name := range names {
		d := event.Delivery{EventID: e.EventID, Subscriber: name, Status: event.DeliveryPending}
		switch {
		case e.CausationDepth > b.opts.MaxDepth:
			d.Status, d.LastError = event.DeliveryDropped, reasonDepth
		case dropped[name] != "":
			d.Status, d.LastError = event.DeliveryDropped, dropped[name]
		case active >= b.opts.FanOutLimit:
			d.Status, d.LastError = event.DeliveryDropped, reasonFanOut
		default:
			active++
		}
		out = append(out, d)
	}
	return out, nil
}

func (b *Bus) matches(ctx context.Context, filter string, e event.Envelope) (bool, error) {
	c, err := workflow.CompileFilter(filter)
	if err != nil {
		return false, err
	}
	doc, err := EnvelopeMap(e)
	if err != nil {
		return false, err
	}
	return c.EvalEvent(ctx, doc)
}

// EnvelopeMap decodes an envelope for filters and ${event...} references.
func EnvelopeMap(e event.Envelope) (map[string]any, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	v, err := workflow.DecodeJSON(raw)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}

func (b *Bus) backoff(attempts int) time.Duration {
	d := b.opts.BackoffBase
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= b.opts.BackoffMax {
			return b.opts.BackoffMax
		}
	}
	return d
}

func (b *Bus) deliver(ctx context.Context, tenant domain.TenantID, store repo.TenantStore) error {
	now := b.opts.Clock()
	due, err := store.Outbox().DueDeliveries(ctx, now, defaultBatch)
	if err != nil {
		return err
	}
	for _, d := range due {
		e, err := store.Outbox().GetEvent(ctx, d.EventID)
		if err != nil {
			return err
		}
		d.Attempts++
		err = b.deliverOne(ctx, tenant, d, e)
		now = b.opts.Clock()
		if err == nil {
			d.Status, d.LastError, d.NextAttempt = event.DeliveryDelivered, "", nil
			at := now
			d.DeliveredAt = &at
		} else {
			d.LastError = errs.Public(err).Message
			if d.Attempts >= b.opts.MaxAttempts {
				d.Status, d.NextAttempt = event.DeliveryFailed, nil
			} else {
				next := now.Add(b.backoff(d.Attempts))
				d.Status, d.NextAttempt = event.DeliveryPending, &next
			}
			b.opts.Logger.Warn("event delivery failed", "tenant_id", tenant, "event_id", d.EventID,
				"subscriber", d.Subscriber, "attempts", d.Attempts, "error", err)
		}
		if err := store.Outbox().PutDelivery(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bus) deliverOne(ctx context.Context, tenant domain.TenantID, d event.Delivery, e event.Envelope) error {
	if name, ok := cutPrefix(d.Subscriber, workflowSubscriber); ok {
		if b.opts.Starter == nil {
			return errs.New(errs.CodeUnavailable, "the job engine is not ready")
		}
		var version string
		if b.opts.Triggers != nil {
			for _, wf := range b.opts.Triggers.Triggered(wfdef.TriggerEvent) {
				if wf.Definition.ID == name {
					version = wf.Definition.Version
				}
			}
		}
		return b.opts.Starter.StartEventJob(ctx, StartRequest{
			Tenant: tenant, WorkflowID: name, Version: version, Event: e,
			IdempotencyKey: fmt.Sprintf("event:%s:%s", e.EventID, name),
		})
	}
	if name, ok := cutPrefix(d.Subscriber, pluginSubscriber); ok {
		if b.opts.Plugins == nil {
			return errs.New(errs.CodeUnavailable, "the plugin runtime is not wired")
		}
		return b.opts.Plugins.DeliverEvent(ctx, name, e)
	}
	return errs.Newf(errs.CodeInternal, "unknown subscriber %s", d.Subscriber)
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

// ---- service.Events ----

func (b *Bus) store(ctx context.Context, s service.Scope) (repo.TenantStore, error) {
	if s.Tenant == "" {
		return nil, errs.New(errs.CodeTenantRequired, "this operation needs a tenant")
	}
	return b.opts.Tenants.ForTenant(ctx, s.Tenant)
}

// List implements service.Events.
func (b *Bus) List(ctx context.Context, s service.Scope, q api.EventQuery) (api.List[event.Envelope], error) {
	store, err := b.store(ctx, s)
	if err != nil {
		return api.List[event.Envelope]{}, err
	}
	f := repo.OutboxFilter{Type: q.Type}
	if q.Since != "" {
		t, err := time.Parse(time.RFC3339, q.Since)
		if err != nil {
			return api.List[event.Envelope]{}, errs.Newf(errs.CodeInvalidArgument, "since must be RFC 3339")
		}
		f.Since = &t
	}
	items, next, err := store.Outbox().ListEvents(ctx, f, repo.Page{Limit: q.Limit, Cursor: q.Cursor})
	if err != nil {
		return api.List[event.Envelope]{}, err
	}
	if items == nil {
		items = []event.Envelope{}
	}
	return api.List[event.Envelope]{Items: items, NextCursor: next}, nil
}

// Deliveries implements service.Events.
func (b *Bus) Deliveries(ctx context.Context, s service.Scope, q api.DeliveryQuery) (api.List[event.Delivery], error) {
	store, err := b.store(ctx, s)
	if err != nil {
		return api.List[event.Delivery]{}, err
	}
	items, next, err := store.Outbox().ListDeliveries(ctx, repo.DeliveryFilter{Status: q.Status}, repo.Page{Limit: q.Limit, Cursor: q.Cursor})
	if err != nil {
		return api.List[event.Delivery]{}, err
	}
	if items == nil {
		items = []event.Delivery{}
	}
	return api.List[event.Delivery]{Items: items, NextCursor: next}, nil
}

// Subscribe implements service.Events. Reading never changes state. The channel
// closes when ctx ends.
func (b *Bus) Subscribe(ctx context.Context, s service.Scope, q api.EventQuery, afterSeq int64) (<-chan service.StreamedEvent, error) {
	store, err := b.store(ctx, s)
	if err != nil {
		return nil, err
	}
	out := make(chan service.StreamedEvent, 16)
	notify := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[notify] = s.Tenant
	b.mu.Unlock()
	go func() {
		defer close(out)
		defer func() {
			b.mu.Lock()
			delete(b.subs, notify)
			b.mu.Unlock()
		}()
		ticker := time.NewTicker(b.opts.PollInterval)
		defer ticker.Stop()
		after := afterSeq
		for {
			entries, err := store.Outbox().Stream(ctx, after, defaultBatch)
			if err != nil {
				if ctx.Err() == nil {
					b.opts.Logger.Warn("event stream read failed", "error", err)
				}
				return
			}
			for _, entry := range entries {
				after = entry.Seq
				if q.Type != "" && entry.Event.Type != q.Type {
					continue
				}
				select {
				case out <- service.StreamedEvent{Seq: entry.Seq, Event: entry.Event}:
				case <-ctx.Done():
					return
				}
			}
			if len(entries) == defaultBatch {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-notify:
			}
		}
	}()
	return out, nil
}
