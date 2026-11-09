package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/jobs/memstore"
	"github.com/rafaelp/vidyom/internal/workflow"
)

type fakeWorkers struct {
	mu   sync.Mutex
	data map[string]domain.Worker
}

func (f *fakeWorkers) PutTokenHash(context.Context, string, string) error { return nil }
func (f *fakeWorkers) HasTokenHash(context.Context, string) (bool, error) { return false, nil }
func (f *fakeWorkers) Upsert(_ context.Context, w domain.Worker) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data == nil {
		f.data = map[string]domain.Worker{}
	}
	f.data[w.ID] = w
	return nil
}
func (f *fakeWorkers) List(context.Context) ([]domain.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Worker
	for _, w := range f.data {
		out = append(out, w)
	}
	return out, nil
}
func (f *fakeWorkers) Delete(context.Context, string) error { return nil }

func TestMatcher(t *testing.T) {
	ctx := context.Background()
	store := &fakeWorkers{}
	m := NewMatcher(store, nil)
	gpu := domain.Worker{ID: "gpu", Pools: []string{"ai"}, Resources: domain.WorkerResources{CPUCores: 8, MemoryMB: 32000, DiskMB: 100000, GPU: true}}
	small := domain.Worker{ID: "small", Pools: []string{"media"}, Resources: domain.WorkerResources{CPUCores: 2, MemoryMB: 2000, DiskMB: 1000}}
	for _, w := range []domain.Worker{gpu, small} {
		if err := m.Register(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct {
		worker string
		pool   node.Pool
		req    node.Requirements
		ok     bool
	}{
		"fits":            {"small", node.PoolMedia, node.Requirements{CPUCores: 2, MemoryMB: 1000}, true},
		"too many cores":  {"small", node.PoolMedia, node.Requirements{CPUCores: 4}, false},
		"too little disk": {"small", node.PoolMedia, node.Requirements{DiskMB: 5000}, false},
		"no gpu":          {"small", node.PoolMedia, node.Requirements{GPU: true}, false},
		"gpu":             {"gpu", node.PoolAI, node.Requirements{GPU: true}, true},
		"wrong pool":      {"gpu", node.PoolMedia, node.Requirements{}, false},
		"unknown":         {"ghost", node.PoolMedia, node.Requirements{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := m.Compatible(c.worker, c.pool, c.req)
			if c.ok && err != nil {
				t.Fatal(err)
			}
			if !c.ok && !errs.IsCode(err, errs.CodeConflict) {
				t.Fatalf("expected a conflict, got %v", err)
			}
		})
	}
	if err := m.Heartbeat(ctx, "ghost", 0, 0); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown heartbeat: %v", err)
	}
	if err := m.Heartbeat(ctx, "small", 3, 500); err != nil {
		t.Fatal(err)
	}
	// Free disk shrinks what a worker can take.
	if err := m.Compatible("small", node.PoolMedia, node.Requirements{DiskMB: 800}); err == nil {
		t.Fatal("the heartbeat's free disk must count")
	}
	// A restart loads what was stored.
	again := NewMatcher(store, nil)
	if err := again.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := again.Compatible("gpu", node.PoolAI, node.Requirements{}); err != nil {
		t.Fatal(err)
	}
	if len(again.List()) != 2 || len(again.Active(time.Minute)) != 2 || len(again.Active(-time.Hour)) != 0 {
		t.Fatalf("list %+v", again.List())
	}
	if err := m.Register(ctx, domain.Worker{}); err == nil {
		t.Fatal("a worker needs an ID")
	}
}

type fakeStarter struct {
	mu   sync.Mutex
	reqs []StartRequest
	err  error
}

func (f *fakeStarter) StartScheduledJob(_ context.Context, r StartRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.reqs = append(f.reqs, r)
	return nil
}

func scheduledRegistry(t *testing.T) *workflow.Registry {
	t.Helper()
	r := workflow.NewRegistry(workflow.NewValidator(nil))
	if err := r.Add([]byte(`{"id":"sweep","version":"1.0.0","trigger":{"type":"schedule","every":"1h"},"nodes":[{"id":"n","type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}}]}`), "x"); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestScheduleSlots(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 9, 10, 30, 0, 0, time.UTC)
	starter := &fakeStarter{}
	stores := memstore.New("acme", "globex")
	s := New(Options{Tenants: stores, Triggers: scheduledRegistry(t), Starter: starter, Clock: func() time.Time { return now }})

	// A fresh install does no maintenance in its first seconds.
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 0 {
		t.Fatalf("first boot: %+v", starter.reqs)
	}
	// Within the slot, nothing.
	now = now.Add(10 * time.Minute)
	_ = s.Tick(ctx)
	if len(starter.reqs) != 0 {
		t.Fatalf("same slot: %+v", starter.reqs)
	}
	// The next slot starts it once per tenant.
	now = now.Add(30 * time.Minute)
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 2 {
		t.Fatalf("next slot: %+v", starter.reqs)
	}
	if starter.reqs[0].IdempotencyKey != starter.reqs[1].IdempotencyKey {
		t.Fatalf("the slot names the key: %+v", starter.reqs)
	}
	_ = s.Tick(ctx)
	if len(starter.reqs) != 2 {
		t.Fatalf("a slot runs once: %+v", starter.reqs)
	}
	// A failure is tried again at the next tick.
	starter.reqs, starter.err = nil, errors.New("busy")
	now = now.Add(time.Hour)
	if err := s.Tick(ctx); err == nil {
		t.Fatal("the failure must surface")
	}
	starter.err = nil
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 2 {
		t.Fatalf("after recovery: %+v", starter.reqs)
	}
}

func TestScheduleCatchesUpAfterDowntime(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 9, 10, 30, 0, 0, time.UTC)
	starter := &fakeStarter{}
	stores := memstore.New("acme")
	store, _ := stores.ForTenant(ctx, "acme")
	job := domain.Job{ID: "JOB-001", ULID: id.New(), WorkflowID: "sweep", Status: domain.JobCompleted, CreatedAt: now.Add(-3 * time.Hour)}
	if err := store.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Tenants: stores, Triggers: scheduledRegistry(t), Starter: starter, Clock: func() time.Time { return now }})
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(starter.reqs) != 1 {
		t.Fatalf("a missed slot runs on boot: %+v", starter.reqs)
	}
}

func TestRunStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := New(Options{Tenants: memstore.New(), Tick: time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
