package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/events"
	"github.com/rafaelp/vidyom/internal/hooks"
	"github.com/rafaelp/vidyom/internal/scheduler"
	"github.com/rafaelp/vidyom/internal/workflow"
)

// Timings. All can be set through Deps (I152).
const (
	DefaultLeaseTTL          = time.Duration(workerproto.DefaultLeaseSeconds) * time.Second
	DefaultHeartbeatSeconds  = workerproto.DefaultHeartbeatSeconds
	DefaultSweepInterval     = time.Second
	DefaultReconcileInterval = 30 * time.Second
	DefaultIdleCheck         = 30 * time.Second
	DefaultRetries           = 3
	DefaultBackoff           = 5 * time.Second
	MaxBackoff               = 5 * time.Minute
	MaxSubworkflowDepth      = 8
	defaultDaemonConcurrency = 8
	defaultKickWorkers       = 8
	daemonConsumer           = "vidyomd"
)

// Connector driver names of the AI component (D9).
const (
	DefaultLLMDriver      = "openai-compatible"
	DefaultEmbedderDriver = "bge-m3"
)

// Error codes the engine writes on nodes and jobs.
const (
	CodeNeedsAttention = "needs_attention"
	CodeHookBlocked    = "hook_blocked"
	CodeInterrupted    = "interrupted"
	CodeCancelled      = "cancelled"
	CodeTimeout        = "timeout"
	CodeReference      = "invalid_reference"
)

// Prefixes of Job.StatusReason (I146).
const (
	reasonCancelRequested = "cancel_requested"
	reasonTimeout         = "cancel_requested: timeout"
	ReasonBudgetExceeded  = "budget_exceeded" // I33
)

// Deps is what the engine needs. Nil optional fields disable what depends on them;
// a nil Hooks makes every declared hook fail under its own policy.
type Deps struct {
	Tenants   repo.TenantStores
	Queue     jobqueue.Queue
	Nodes     node.Registry
	Workflows *workflow.Registry
	Validator *workflow.Validator
	Bus       *events.Bus
	Scheduler *scheduler.Scheduler
	Hooks     hook.Dispatcher

	Config     config.TenantSource
	Daemon     config.Daemon
	Storage    connector.StorageResolver
	Publishing connector.PublishingResolver
	Connectors *connector.Registry
	// SecretsFor returns the tenant's secret resolver (core's vault).
	SecretsFor func(domain.TenantID) connector.SecretResolver
	Keys       security.DerivedKeys
	Budgets    service.Budgets
	// Plugins lists installed plugins, for --dry-run.
	Plugins repo.PluginRepo
	Metrics telemetry.Metrics
	Logger  *slog.Logger
	Clock   func() time.Time

	// Drivers of the AI connectors; empty means the defaults above.
	LLMDriver      string
	EmbedderDriver string

	LeaseTTL          time.Duration
	HeartbeatSeconds  int
	SweepInterval     time.Duration
	ReconcileInterval time.Duration
	DaemonConcurrency int
	DefaultBackoff    time.Duration
	// StartConsumers is false only in tests that drive the daemon pool themselves.
	DisableDaemonPool bool
}

// Engine is the workflow engine.
type Engine struct {
	d       Deps
	locks   keyedLocks
	tenants sync.Map // id.ULID -> domain.TenantID
	checks  sync.Map // id.ULID -> time.Time of the next look

	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	kickMu  sync.Mutex
	kicked  map[id.ULID]bool
	kickCh  chan kickItem
	busyMu  sync.Mutex
	busy    map[string]bool
	running sync.Map // task ID -> context.CancelFunc, daemon pool

	streamMu sync.Mutex
	streams  map[id.ULID]map[chan struct{}]bool

	runtimeMu sync.Mutex
	runtimes  map[string]*runtimeDef

	ready atomic.Bool
}

type kickItem struct {
	tenant domain.TenantID
	job    id.ULID
}

// New returns an engine. Run starts its background work.
func New(d Deps) *Engine {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = func() time.Time { return time.Now().UTC() }
	}
	if d.Metrics == nil {
		d.Metrics = telemetry.Noop{}
	}
	if d.Hooks == nil {
		d.Hooks = hooks.New(nil, hooks.Options{Logger: d.Logger})
	}
	if d.LeaseTTL <= 0 {
		d.LeaseTTL = DefaultLeaseTTL
	}
	if d.HeartbeatSeconds <= 0 {
		d.HeartbeatSeconds = DefaultHeartbeatSeconds
	}
	if d.SweepInterval <= 0 {
		d.SweepInterval = DefaultSweepInterval
	}
	if d.ReconcileInterval <= 0 {
		d.ReconcileInterval = DefaultReconcileInterval
	}
	if d.DaemonConcurrency <= 0 {
		d.DaemonConcurrency = defaultDaemonConcurrency
	}
	if d.DefaultBackoff <= 0 {
		d.DefaultBackoff = DefaultBackoff
	}
	if d.LLMDriver == "" {
		d.LLMDriver = DefaultLLMDriver
	}
	if d.EmbedderDriver == "" {
		d.EmbedderDriver = DefaultEmbedderDriver
	}
	if d.Scheduler == nil {
		d.Scheduler = scheduler.New(scheduler.Options{Tenants: d.Tenants, Triggers: d.Workflows, Clock: d.Clock, Logger: d.Logger})
	}
	if d.Bus == nil {
		d.Bus = events.New(events.Options{Tenants: d.Tenants, Triggers: d.Workflows, Logger: d.Logger, Clock: d.Clock})
	}
	e := &Engine{
		d:        d,
		kicked:   map[id.ULID]bool{},
		kickCh:   make(chan kickItem, 4096),
		busy:     map[string]bool{},
		streams:  map[id.ULID]map[chan struct{}]bool{},
		runtimes: map[string]*runtimeDef{},
	}
	d.Bus.SetStarter(e)
	d.Scheduler.SetStarter(e)
	return e
}

// Scheduler returns the scheduler the engine uses.
func (e *Engine) Scheduler() *scheduler.Scheduler { return e.d.Scheduler }

// Bus returns the Event Bus, which implements service.Events.
func (e *Engine) Bus() *events.Bus { return e.d.Bus }

func (e *Engine) now() time.Time { return e.d.Clock() }

// Run reconciles with the queue (D4), then runs until ctx ends: the daemon pool,
// timers, the Event Bus and the scheduler. It returns ctx's error after everything
// stopped.
func (e *Engine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.runCtx, e.cancel = ctx, cancel

	if err := e.d.Scheduler.Matcher.Load(ctx); err != nil {
		e.d.Logger.Warn("cannot load worker registrations", "error", err)
	}
	if err := e.Reconcile(ctx, true); err != nil {
		// Redis may still be starting. The periodic pass heals what this one could not.
		e.d.Logger.Warn("startup reconciliation incomplete", "error", err)
	}
	for i := 0; i < defaultKickWorkers; i++ {
		e.background(func() { e.kickWorker(ctx) })
	}
	e.background(func() { e.sweepLoop(ctx) })
	e.background(func() { e.reconcileLoop(ctx) })
	if !e.d.DisableDaemonPool {
		e.background(func() { e.daemonPool(ctx) })
	}
	e.background(func() { logStop(e.d.Logger, "event bus", e.d.Bus.Run(ctx)) })
	e.background(func() { logStop(e.d.Logger, "scheduler", e.d.Scheduler.Run(ctx)) })
	e.ready.Store(true)
	<-ctx.Done()
	e.wg.Wait()
	return ctx.Err()
}

func logStop(log *slog.Logger, what string, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Warn(what+" stopped", "error", err)
	}
}

func (e *Engine) background(fn func()) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		fn()
	}()
}

// Kick asks for a job to be looked at. It never blocks. If the queue is full, the
// sweeper finds the job anyway.
func (e *Engine) Kick(tenant domain.TenantID, job id.ULID) {
	e.kickMu.Lock()
	if e.kicked[job] {
		e.kickMu.Unlock()
		return
	}
	e.kicked[job] = true
	e.kickMu.Unlock()
	select {
	case e.kickCh <- kickItem{tenant, job}:
	default:
		e.kickMu.Lock()
		delete(e.kicked, job)
		e.kickMu.Unlock()
	}
}

func (e *Engine) kickWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-e.kickCh:
			e.kickMu.Lock()
			delete(e.kicked, item.job)
			e.kickMu.Unlock()
			if err := e.advance(ctx, item.tenant, item.job); err != nil && ctx.Err() == nil {
				e.d.Logger.Warn("job advance failed", "tenant_id", item.tenant, "job", item.job, "error", err)
				// Look again soon: the database is the truth and may just be busy.
				e.checks.Store(item.job, e.now().Add(e.d.SweepInterval))
			}
		}
	}
}

func (e *Engine) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(e.d.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := e.sweep(ctx); err != nil && ctx.Err() == nil {
			e.d.Logger.Warn("job sweep failed", "error", err)
		}
	}
}

// sweep wakes every active job whose next deadline has come (I152).
func (e *Engine) sweep(ctx context.Context) error {
	tenants, err := e.d.Tenants.Tenants(ctx)
	if err != nil {
		return err
	}
	now := e.now()
	for _, t := range tenants {
		store, err := e.d.Tenants.ForTenant(ctx, t)
		if err != nil {
			return err
		}
		active, err := store.Jobs().ListActive(ctx)
		if err != nil {
			return err
		}
		counts := map[domain.JobStatus]int{}
		for _, j := range active {
			counts[j.Status]++
			e.tenants.Store(j.ULID, t)
			if at, ok := e.checks.Load(j.ULID); ok && now.Before(at.(time.Time)) {
				continue
			}
			e.Kick(t, j.ULID)
		}
		for _, s := range domain.JobStatuses {
			if !s.Terminal() {
				e.d.Metrics.Set(telemetry.JobsByState, float64(counts[s]), telemetry.Labels{"state": string(s), "tenant_id": string(t)})
			}
		}
	}
	return nil
}

func (e *Engine) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(e.d.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := e.Reconcile(ctx, false); err != nil && ctx.Err() == nil {
			e.d.Logger.Warn("reconciliation incomplete", "error", err)
		}
		for _, p := range node.Pools {
			if depth, err := e.d.Queue.Depth(ctx, p); err == nil {
				e.d.Metrics.Set(telemetry.QueueDepth, float64(depth.Length), telemetry.Labels{"pool": string(p)})
			}
		}
	}
}

// Reconcile compares jobs with the queue (D4, §15.5). At startup, daemon-pool nodes
// marked running died with the previous process, and worker leases get a new grace
// period so a live worker can reach the daemon again. Always, queued nodes missing
// from Redis are enqueued again; a lease makes a duplicate pointer harmless (I4).
func (e *Engine) Reconcile(ctx context.Context, startup bool) error {
	tenants, err := e.d.Tenants.Tenants(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, t := range tenants {
		store, err := e.d.Tenants.ForTenant(ctx, t)
		if err != nil {
			return err
		}
		active, err := store.Jobs().ListActive(ctx)
		if err != nil {
			return err
		}
		for _, j := range active {
			e.tenants.Store(j.ULID, t)
			if err := e.reconcileJob(ctx, store, j, startup); err != nil {
				if first == nil {
					first = err
				}
				e.d.Logger.Warn("job reconciliation failed", "tenant_id", t, "job_id", j.ID, "error", err)
			}
			e.Kick(t, j.ULID)
		}
	}
	return first
}

func (e *Engine) reconcileJob(ctx context.Context, store repo.TenantStore, j domain.Job, startup bool) error {
	unlock := e.locks.lock(j.ULID)
	defer unlock()
	nodes, err := store.Jobs().ListNodes(ctx, j.ULID)
	if err != nil {
		return err
	}
	var first error
	for _, n := range nodes {
		switch n.Status {
		case domain.NodeQueued:
			if j.Status == domain.JobPaused || strings.HasPrefix(j.StatusReason, reasonCancelRequested) {
				continue
			}
			known, err := e.d.Queue.Inflight(ctx, n.TaskID)
			if err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			if !known {
				if err := e.enqueue(ctx, store.Tenant(), j, n); err != nil && first == nil {
					first = err
				}
				e.d.Logger.Info("task re-enqueued by reconciliation", "tenant_id", store.Tenant(), "job_id", j.ID, "node_id", n.NodeID, "task", n.TaskID)
			}
		case domain.NodeRunning:
			if !startup || len(n.Output) > 0 {
				continue
			}
			if n.WorkerID == daemonConsumer {
				// Nothing of this process can still be running it.
				expired := e.now().Add(-time.Second)
				n.LeaseExpiresAt = &expired
			} else {
				grace := e.now().Add(e.d.LeaseTTL)
				n.LeaseExpiresAt = &grace
			}
			if err := store.Jobs().PutNode(ctx, n); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// enqueue writes a task pointer. It holds no content (I4).
func (e *Engine) enqueue(ctx context.Context, tenant domain.TenantID, j domain.Job, n domain.NodeExecution) error {
	d, ok := e.descriptor(n.NodeType, n.TypeVersion)
	if !ok {
		return errs.Newf(errs.CodeInternal, "unknown node type %s", n.NodeType)
	}
	return e.d.Queue.Enqueue(ctx, jobqueue.Pointer{
		TaskID: n.TaskID, TenantID: tenant, Pool: d.Pool, NodeType: n.NodeType, TraceID: j.TraceID, EnqueuedAt: e.now(),
	})
}

func (e *Engine) descriptor(nodeType string, version int) (node.Descriptor, bool) {
	return e.d.Validator.Descriptor(nodeType, version)
}

// tenantOf finds the tenant of a job: the coordinator's routes carry only a task ID.
func (e *Engine) tenantOf(ctx context.Context, job id.ULID) (domain.TenantID, repo.TenantStore, error) {
	if t, ok := e.tenants.Load(job); ok {
		store, err := e.d.Tenants.ForTenant(ctx, t.(domain.TenantID))
		return t.(domain.TenantID), store, err
	}
	tenants, err := e.d.Tenants.Tenants(ctx)
	if err != nil {
		return "", nil, err
	}
	for _, t := range tenants {
		store, err := e.d.Tenants.ForTenant(ctx, t)
		if err != nil {
			return "", nil, err
		}
		if _, err := store.Jobs().GetByULID(ctx, job); err == nil {
			e.tenants.Store(job, t)
			return t, store, nil
		} else if !errs.IsCode(err, errs.CodeNotFound) {
			return "", nil, err
		}
	}
	return "", nil, errs.NotFound("job")
}

// ---- small helpers ----

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return string(id.New())
	}
	return hex.EncodeToString(b[:])
}

// keyedLocks gives one mutex per job, created on demand and freed when unused.
type keyedLocks struct {
	mu sync.Mutex
	m  map[id.ULID]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocks) lock(key id.ULID) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[id.ULID]*lockEntry{}
	}
	entry := k.m[key]
	if entry == nil {
		entry = &lockEntry{}
		k.m[key] = entry
	}
	entry.refs++
	k.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// claim marks a piece of work as being done outside the job lock, such as hooks. It
// returns false when someone already is.
func (e *Engine) claim(key string) bool {
	e.busyMu.Lock()
	defer e.busyMu.Unlock()
	if e.busy[key] {
		return false
	}
	e.busy[key] = true
	return true
}

func (e *Engine) release(key string) {
	e.busyMu.Lock()
	defer e.busyMu.Unlock()
	delete(e.busy, key)
}

func (e *Engine) isBusy(key string) bool {
	e.busyMu.Lock()
	defer e.busyMu.Unlock()
	return e.busy[key]
}

func nodeKey(job id.ULID, nodeID string) string { return string(job) + "/" + nodeID }
func finishKey(job id.ULID) string              { return string(job) + "//finish" }

// spawn runs fn in the background during the engine's life. Without a running engine,
// as in direct calls from tests, it runs on context.Background.
func (e *Engine) spawn(fn func(ctx context.Context)) {
	ctx := e.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	e.background(func() { fn(ctx) })
}

// runCtxReady reports that Run got past its startup, for tests.
func (e *Engine) runCtxReady() bool { return e.ready.Load() }
