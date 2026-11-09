package scheduler

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/workflow"
)

// Matcher keeps worker registrations, backed by system.db (I4).
type Matcher struct {
	workers repo.WorkerRepo
	clock   func() time.Time
	mu      sync.RWMutex
	known   map[string]domain.Worker
}

// NewMatcher returns a matcher. With a nil repository it keeps registrations in
// memory only, for tests.
func NewMatcher(workers repo.WorkerRepo, clock func() time.Time) *Matcher {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Matcher{workers: workers, clock: clock, known: map[string]domain.Worker{}}
}

// Load reads registrations after a restart, so workers that are still running do not
// have to register again.
func (m *Matcher) Load(ctx context.Context) error {
	if m.workers == nil {
		return nil
	}
	list, err := m.workers.List(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range list {
		m.known[w.ID] = w
	}
	return nil
}

// Register stores a worker's declared resources (D40).
func (m *Matcher) Register(ctx context.Context, w domain.Worker) error {
	if w.ID == "" {
		return errs.New(errs.CodeInvalidArgument, "a worker needs an ID")
	}
	w.LastSeenAt = m.clock()
	if m.workers != nil {
		if err := m.workers.Upsert(ctx, w); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.known[w.ID] = w
	return nil
}

// Heartbeat records that a worker is alive and how loaded it is. An unknown worker
// gets not_found and registers again.
func (m *Matcher) Heartbeat(ctx context.Context, workerID string, running, diskMB int) error {
	m.mu.Lock()
	w, ok := m.known[workerID]
	if ok {
		w.Running = running
		if diskMB > 0 {
			w.Resources.DiskMB = diskMB
		}
		w.LastSeenAt = m.clock()
		m.known[workerID] = w
	}
	m.mu.Unlock()
	if !ok {
		return errs.NotFound("worker")
	}
	if m.workers != nil {
		return m.workers.Upsert(ctx, w)
	}
	return nil
}

// Compatible decides a lease: nil, or errs.CodeConflict, which the protocol answers
// with 409 and leaves the pointer for another worker (I4, I154).
func (m *Matcher) Compatible(workerID string, pool node.Pool, q node.Requirements) error {
	m.mu.RLock()
	w, ok := m.known[workerID]
	m.mu.RUnlock()
	if !ok {
		return errs.Newf(errs.CodeConflict, "worker %s is not registered", workerID)
	}
	inPool := false
	for _, p := range w.Pools {
		if p == string(pool) {
			inPool = true
			break
		}
	}
	if !inPool {
		return errs.Newf(errs.CodeConflict, "worker %s does not serve pool %s", workerID, pool)
	}
	if !workflow.Satisfies(w.Resources, q) {
		return errs.Newf(errs.CodeConflict, "worker %s lacks the resources this node declares (D40)", workerID)
	}
	return nil
}

// List returns known workers, sorted by ID.
func (m *Matcher) List() []domain.Worker {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Worker, 0, len(m.known))
	for _, w := range m.known {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Active returns workers seen within the given period.
func (m *Matcher) Active(within time.Duration) []domain.Worker {
	cutoff := m.clock().Add(-within)
	var out []domain.Worker
	for _, w := range m.List() {
		if w.LastSeenAt.After(cutoff) {
			out = append(out, w)
		}
	}
	return out
}
