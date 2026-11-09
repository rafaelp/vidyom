package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/jobs/memstore"
	"github.com/rafaelp/vidyom/internal/workflow"
)

type fakeStarter struct {
	mu   sync.Mutex
	keys map[string]int
	reqs []StartRequest
	err  error
}

func (f *fakeStarter) StartEventJob(_ context.Context, r StartRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.keys == nil {
		f.keys = map[string]int{}
	}
	f.keys[r.IdempotencyKey]++
	f.reqs = append(f.reqs, r)
	return nil
}

type fakePlugins struct {
	mu        sync.Mutex
	delivered map[string]int
	fail      int
	seen      []domain.TenantID
}

func (f *fakePlugins) EventSubscribers(_ context.Context, tenant domain.TenantID, t event.Type) ([]string, error) {
	f.seen = append(f.seen, tenant)
	if tenant == "acme" && t == event.PublicationCompleted {
		return []string{"notifier"}, nil
	}
	return nil, nil
}

func (f *fakePlugins) DeliverEvent(_ context.Context, pluginID string, e event.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return errors.New("plugin down")
	}
	if f.delivered == nil {
		f.delivered = map[string]int{}
	}
	f.delivered[pluginID+"/"+string(e.EventID)]++
	return nil
}

func triggers(t *testing.T) *workflow.Registry {
	t.Helper()
	r := workflow.NewRegistry(workflow.NewValidator(nil))
	for _, doc := range []string{
		`{"id":"on-public","version":"1.0.0","trigger":{"type":"event","event":"publication.completed","filter":"event.payload.visibility == 'public'"},"nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`,
		`{"id":"on-any","version":"1.0.0","trigger":{"type":"event","event":"publication.completed"},"nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`,
		`{"id":"other","version":"1.0.0","trigger":{"type":"event","event":"asset.created"},"nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`,
	} {
		if err := r.Add([]byte(doc), "doc"); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func newBus(t *testing.T, opts Options) (*Bus, *memstore.Stores, repo.TenantStore) {
	t.Helper()
	stores := memstore.New("acme", "globex")
	opts.Tenants = stores
	if opts.Triggers == nil {
		opts.Triggers = triggers(t)
	}
	if opts.BackoffBase == 0 {
		opts.BackoffBase = time.Nanosecond
	}
	store, err := stores.ForTenant(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	return New(opts), stores, store
}

func emit(t *testing.T, b *Bus, store repo.TenantStore, src Source, d event.Draft) id.ULID {
	t.Helper()
	var out id.ULID
	err := store.InTx(context.Background(), func(tx repo.TenantRepos) error {
		var err error
		out, err = b.Emit(context.Background(), tx, store.Tenant(), src, d)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func deliveries(t *testing.T, store repo.TenantStore) map[string]event.Delivery {
	t.Helper()
	items, _, err := store.Outbox().ListDeliveries(context.Background(), repo.DeliveryFilter{}, repo.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]event.Delivery{}
	for _, d := range items {
		out[d.Subscriber] = d
	}
	return out
}

func TestTriggerFilterAndDeliveryTracking(t *testing.T) {
	starter := &fakeStarter{}
	b, _, store := newBus(t, Options{Starter: starter})
	ctx := context.Background()
	emit(t, b, store, Source{JobID: "JOB-001", WorkflowID: "publish-video", Depth: 1, CausationID: "evt"},
		event.Draft{Type: event.PublicationCompleted, AssetID: "CLIP-001", Payload: map[string]any{"visibility": "public"}})
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := deliveries(t, store)
	if got["workflow:on-public"].Status != event.DeliveryDelivered || got["workflow:on-any"].Status != event.DeliveryDelivered {
		t.Fatalf("deliveries %+v", got)
	}
	if _, ok := got["workflow:other"]; ok {
		t.Fatal("a workflow on another event must not subscribe")
	}
	if len(starter.reqs) != 2 {
		t.Fatalf("started %+v", starter.reqs)
	}
	if starter.reqs[0].Event.AssetID != "CLIP-001" || starter.reqs[0].Event.CausationDepth != 1 {
		t.Fatalf("start request %+v", starter.reqs[0])
	}
	// A filter miss is recorded, and starts nothing.
	starter.reqs = nil
	emit(t, b, store, Source{}, event.Draft{Type: event.PublicationCompleted, Payload: map[string]any{"visibility": "private"}})
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 1 || starter.reqs[0].WorkflowID != "on-any" {
		t.Fatalf("started %+v", starter.reqs)
	}
	dropped := 0
	items, _, _ := store.Outbox().ListDeliveries(ctx, repo.DeliveryFilter{Status: event.DeliveryDropped}, repo.Page{})
	for _, d := range items {
		if d.Subscriber == "workflow:on-public" && strings.Contains(d.LastError, "filter") {
			dropped++
		}
	}
	if dropped != 1 {
		t.Fatalf("dropped: %+v", items)
	}
	// Nothing is pending any more.
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 1 {
		t.Fatalf("a delivered event must not start again: %+v", starter.reqs)
	}
}

func TestDepthLimitDropsAndRecords(t *testing.T) {
	starter := &fakeStarter{}
	b, _, store := newBus(t, Options{Starter: starter, MaxDepth: 2})
	ctx := context.Background()
	emit(t, b, store, Source{Depth: 2}, event.Draft{Type: event.PublicationCompleted, Payload: map[string]any{"visibility": "public"}})
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 2 {
		t.Fatalf("at the limit it still runs: %+v", starter.reqs)
	}
	starter.reqs = nil
	emit(t, b, store, Source{Depth: 3}, event.Draft{Type: event.PublicationCompleted, Payload: map[string]any{"visibility": "public"}})
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 0 {
		t.Fatalf("past the limit it must not run: %+v", starter.reqs)
	}
	items, _, _ := store.Outbox().ListDeliveries(ctx, repo.DeliveryFilter{Status: event.DeliveryDropped}, repo.Page{})
	if len(items) != 2 || !strings.Contains(items[0].LastError, "deeper") {
		t.Fatalf("dropped %+v", items)
	}
}

func TestFanOutLimit(t *testing.T) {
	starter := &fakeStarter{}
	b, _, store := newBus(t, Options{Starter: starter, FanOutLimit: 1})
	ctx := context.Background()
	emit(t, b, store, Source{}, event.Draft{Type: event.PublicationCompleted, Payload: map[string]any{"visibility": "public"}})
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 1 {
		t.Fatalf("started %+v", starter.reqs)
	}
	items, _, _ := store.Outbox().ListDeliveries(ctx, repo.DeliveryFilter{Status: event.DeliveryDropped}, repo.Page{})
	if len(items) != 1 || !strings.Contains(items[0].LastError, "fan-out") {
		t.Fatalf("dropped %+v", items)
	}
}

func TestRetryThenFail(t *testing.T) {
	starter := &fakeStarter{err: errors.New("engine busy")}
	b, _, store := newBus(t, Options{Starter: starter, MaxAttempts: 2})
	ctx := context.Background()
	emit(t, b, store, Source{}, event.Draft{Type: event.AssetCreated})
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if d := deliveries(t, store)["workflow:other"]; d.Status != event.DeliveryPending || d.Attempts != 1 || d.NextAttempt == nil {
		t.Fatalf("after one failure %+v", d)
	}
	time.Sleep(time.Millisecond)
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if d := deliveries(t, store)["workflow:other"]; d.Status != event.DeliveryFailed || d.NextAttempt != nil {
		t.Fatalf("after two failures %+v", d)
	}
	// A failed delivery is final.
	starter.err = nil
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 0 {
		t.Fatal("a failed delivery must stay failed")
	}
}

func TestPluginDeliveryAtLeastOnceAndTenantScope(t *testing.T) {
	plugins := &fakePlugins{fail: 1}
	b, stores, store := newBus(t, Options{Starter: &fakeStarter{}, Plugins: plugins})
	ctx := context.Background()
	eid := emit(t, b, store, Source{}, event.Draft{Type: event.PublicationCompleted, Payload: map[string]any{"visibility": "public"}})
	other, _ := stores.ForTenant(ctx, "globex")
	emit(t, b, other, Source{}, event.Draft{Type: event.PublicationCompleted, Payload: map[string]any{"visibility": "public"}})
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if d := deliveries(t, store)["plugin:notifier"]; d.Status != event.DeliveryPending || d.LastError == "" {
		t.Fatalf("first attempt %+v", d)
	}
	time.Sleep(time.Millisecond)
	if err := b.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if d := deliveries(t, store)["plugin:notifier"]; d.Status != event.DeliveryDelivered || d.Attempts != 2 || d.DeliveredAt == nil {
		t.Fatalf("second attempt %+v", d)
	}
	if plugins.delivered["notifier/"+string(eid)] != 1 {
		t.Fatalf("delivered %+v", plugins.delivered)
	}
	if _, ok := deliveries(t, other)["plugin:notifier"]; ok {
		t.Fatal("a tenant's subscription must not see another tenant's events")
	}
}

func TestStateAndEventCommitTogether(t *testing.T) {
	b, _, store := newBus(t, Options{})
	ctx := context.Background()
	failure := errors.New("state change failed")
	err := store.InTx(ctx, func(tx repo.TenantRepos) error {
		if _, err := b.Emit(ctx, tx, "acme", Source{}, event.Draft{Type: event.AssetCreated}); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	pending, err := store.Outbox().Pending(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("a rolled back event must not exist: %+v %v", pending, err)
	}
}

func TestDeterministicIDEmitsOnce(t *testing.T) {
	b, _, store := newBus(t, Options{})
	ctx := context.Background()
	at := id.New()
	eid := DeterministicID(at, "job/node")
	if eid != DeterministicID(at, "job/node") || eid == DeterministicID(at, "job/other") {
		t.Fatal("derivation")
	}
	if _, err := id.ParseULID(string(eid)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		emit(t, b, store, Source{}, event.Draft{Type: event.AssetCreated, ID: eid})
	}
	pending, err := store.Outbox().Pending(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending %+v %v", pending, err)
	}
}

func TestJobSourceChain(t *testing.T) {
	j := domain.Job{ID: "JOB-002", WorkflowID: "w", TraceID: "t", Trigger: domain.JobTrigger{Type: "event", EventID: "E", Depth: 3}}
	src := JobSource(j)
	if src.CausationID != "E" || src.Depth != 3 || src.JobID != "JOB-002" {
		t.Fatalf("source %+v", src)
	}
	if got := JobSource(domain.Job{ID: "JOB-003"}); got.CausationID != "JOB-003" {
		t.Fatalf("a job with no trigger event is its own cause: %+v", got)
	}
}

func TestServiceAndStream(t *testing.T) {
	b, stores, store := newBus(t, Options{PollInterval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope := service.Scope{Tenant: "acme"}
	emit(t, b, store, Source{}, event.Draft{Type: event.AssetCreated, Payload: map[string]any{"n": 1}})
	emit(t, b, store, Source{}, event.Draft{Type: event.ClipCreated})

	list, err := b.List(ctx, scope, api.EventQuery{Type: event.ClipCreated})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := b.List(ctx, scope, api.EventQuery{Since: "yesterday"}); err == nil {
		t.Fatal("a bad since must fail")
	}
	if _, err := b.List(ctx, service.Scope{}, api.EventQuery{}); err == nil {
		t.Fatal("a tenant is required")
	}

	stream, err := b.Subscribe(ctx, scope, api.EventQuery{Type: event.ClipCreated}, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := <-stream
	if first.Event.Type != event.ClipCreated || first.Seq != 2 {
		t.Fatalf("first %+v", first)
	}
	emit(t, b, store, Source{}, event.Draft{Type: event.AssetCreated})
	emit(t, b, store, Source{}, event.Draft{Type: event.ClipCreated})
	b.Notify()
	select {
	case second := <-stream:
		if second.Seq != 4 {
			t.Fatalf("second %+v", second)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event on the stream")
	}
	// Resume after a sequence, and isolation between tenants.
	resumed, err := b.Subscribe(ctx, scope, api.EventQuery{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if e := <-resumed; e.Seq != 4 {
		t.Fatalf("resumed %+v", e)
	}
	other, _ := stores.ForTenant(ctx, "globex")
	foreign, err := b.Subscribe(ctx, service.Scope{Tenant: "globex"}, api.EventQuery{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	emit(t, b, other, Source{}, event.Draft{Type: event.AssetDeleted})
	if e := <-foreign; e.Event.Type != event.AssetDeleted {
		t.Fatalf("foreign %+v", e)
	}
	cancel()
	for range stream {
	}
	data, _ := json.Marshal(first.Event)
	if strings.Contains(string(data), "secret") {
		t.Fatal(string(data))
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	starter := &fakeStarter{}
	b, _, store := newBus(t, Options{Starter: starter, PollInterval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	emit(t, b, store, Source{}, event.Draft{Type: event.AssetCreated})
	b.Notify()
	deadline := time.Now().Add(2 * time.Second)
	for {
		starter.mu.Lock()
		n := len(starter.reqs)
		starter.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dispatcher did not run")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}
