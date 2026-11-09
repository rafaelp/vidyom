package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/events"
	"github.com/rafaelp/vidyom/internal/hooks"
	"github.com/rafaelp/vidyom/internal/jobs/memstore"
	"github.com/rafaelp/vidyom/internal/queue"
	"github.com/rafaelp/vidyom/internal/scheduler"
	"github.com/rafaelp/vidyom/internal/workflow"
	approvalnodes "github.com/rafaelp/vidyom/nodes/approval"
	catalognodes "github.com/rafaelp/vidyom/nodes/catalog"
	controlnodes "github.com/rafaelp/vidyom/nodes/control"
	"github.com/rafaelp/vidyom/schemas"
)

// ---- fake daemon-pool nodes ----

// fakeNode is a daemon-pool node of another component, or a test type.
type fakeNode struct {
	desc node.Descriptor
	h    *harness
}

func (f *fakeNode) Descriptor() node.Descriptor { return f.desc }

func (f *fakeNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	return f.h.runBehavior(ctx, f.desc.Type, env, in)
}

// reconcilerNode adds Reconcile.
type reconcilerNode struct {
	fakeNode
	reconcile func(ctx context.Context, in node.Input) (node.ReconcileOutcome, node.Result, error)
}

func (f *reconcilerNode) Reconcile(ctx context.Context, env node.Env, in node.Input) (node.ReconcileOutcome, node.Result, error) {
	f.h.mu.Lock()
	f.h.reconciles++
	f.h.mu.Unlock()
	return f.reconcile(ctx, in)
}

// ---- harness ----

type taskCall struct {
	NodeType string
	NodeID   string
	Attempt  int
	Key      string
	Params   json.RawMessage
	Worker   bool
}

type behavior func(ctx context.Context, env node.Env, in node.Input) (node.Result, error)

type harness struct {
	t      *testing.T
	stores repo.TenantStores
	store  repo.TenantStore
	mr     *miniredis.Miniredis
	client *redis.Client
	q      jobqueue.Queue
	reg    node.Registry
	wfs    *workflow.Registry
	val    *workflow.Validator
	engine *Engine
	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	behaviors  map[string]behavior
	calls      []taskCall
	reconciles int

	hookCalls []string
	hookFn    func(inv hook.Invocation) (hook.Result, error)
	budgets   *fakeBudgets
	tenantCfg config.Tenant

	workerCancel context.CancelFunc
	workerDone   chan struct{}
	deps         Deps
}

type fakeBudgets struct {
	service.Budgets
	mu       sync.Mutex
	blocked  bool
	recorded []domain.UsageRecord
}

func (b *fakeBudgets) Check(context.Context, domain.TenantID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blocked {
		return errs.New(errs.CodeBudgetExceeded, "monthly budget exceeded")
	}
	return nil
}

func (b *fakeBudgets) Record(_ context.Context, _ domain.TenantID, usage []domain.UsageRecord) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recorded = append(b.recorded, usage...)
	return nil
}

type tenantSource struct{ h *harness }

func (s tenantSource) TenantConfig(context.Context, domain.TenantID) (config.Tenant, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.h.tenantCfg, nil
}

type hookRunner struct{ h *harness }

func (r hookRunner) RunHookAction(_ context.Context, inv hook.Invocation) (hook.Result, error) {
	r.h.mu.Lock()
	r.h.hookCalls = append(r.h.hookCalls, string(inv.Point)+":"+inv.Hook.ID)
	fn := r.h.hookFn
	r.h.mu.Unlock()
	if fn != nil {
		return fn(inv)
	}
	return hook.Result{Allow: true}, nil
}

// defaultStores is what a harness runs on, unless a test names others.
var defaultStores = func(*testing.T) repo.TenantStores { return memstore.New("acme", "globex") }

type harnessOpts struct {
	stores   repo.TenantStores
	lease    time.Duration
	redis    redis.UniversalClient
	noEngine bool
	maxDepth int
	// register adds real nodes before fakes fill the rest of the catalog.
	register func(node.Registry)
}

func newHarness(t *testing.T, opts ...harnessOpts) *harness {
	t.Helper()
	var o harnessOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	h := &harness{
		t:         t,
		stores:    o.stores,
		behaviors: map[string]behavior{},
		budgets:   &fakeBudgets{},
	}
	if h.stores == nil {
		h.stores = defaultStores(t)
	}
	var err error
	h.store, err = h.stores.ForTenant(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if o.redis == nil {
		h.mr = miniredis.RunT(t)
		h.client = redis.NewClient(&redis.Options{Addr: h.mr.Addr()})
		t.Cleanup(func() { _ = h.client.Close() })
		o.redis = h.client
	}
	h.q = queue.NewRedis(o.redis)
	h.reg = node.NewRegistry()
	controlnodes.Register(h.reg)
	approvalnodes.Register(h.reg)
	catalognodes.Register(h.reg)
	if o.register != nil {
		o.register(h.reg)
	}
	cat, err := schemas.NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cat.Nodes {
		if c.Pool != node.PoolDaemon {
			continue
		}
		if _, ok := h.reg.Lookup(c.Type, c.TypeVersion); ok {
			continue
		}
		d := c.Descriptor()
		if d.SideEffect == node.EffectReconcilable {
			h.reg.RegisterNode(&reconcilerNode{fakeNode: fakeNode{desc: d, h: h}, reconcile: func(ctx context.Context, in node.Input) (node.ReconcileOutcome, node.Result, error) {
				return node.ReconcileNotStarted, node.Result{}, nil
			}})
			continue
		}
		h.reg.RegisterNode(&fakeNode{desc: d, h: h})
	}
	for _, d := range []node.Descriptor{
		{Type: "test.unsafe", TypeVersion: 1, Pool: node.PoolDaemon, SideEffect: node.EffectUnsafe},
		{Type: "test.slow", TypeVersion: 1, Pool: node.PoolDaemon, SideEffect: node.EffectIdempotent, SupportsAbort: true},
		{Type: "test.stubborn", TypeVersion: 1, Pool: node.PoolDaemon, SideEffect: node.EffectIdempotent},
		{Type: "test.daemon", TypeVersion: 1, Pool: node.PoolDaemon, SideEffect: node.EffectIdempotent},
		{Type: "test.paid", TypeVersion: 1, Pool: node.PoolDaemon, SideEffect: node.EffectIdempotent, Paid: true},
		{Type: "test.pure", TypeVersion: 1, Pool: node.PoolDaemon, SideEffect: node.EffectNone},
		{Type: "test.suspends", TypeVersion: 1, Pool: node.PoolDaemon, SideEffect: node.EffectNone},
		{Type: "test.gpu", TypeVersion: 1, Pool: node.PoolMedia, SideEffect: node.EffectIdempotent, Requirements: node.Requirements{GPU: true}},
		{Type: "test.unsafe_worker", TypeVersion: 1, Pool: node.PoolMedia, SideEffect: node.EffectUnsafe},
		{Type: "test.reconcilable_worker", TypeVersion: 1, Pool: node.PoolMedia, SideEffect: node.EffectReconcilable},
	} {
		h.reg.RegisterNode(&fakeNode{desc: d, h: h})
	}
	h.val = workflow.NewValidator(h.reg)
	h.wfs = workflow.NewRegistry(h.val)
	h.deps = Deps{
		Tenants:           h.stores,
		Queue:             h.q,
		Nodes:             h.reg,
		Workflows:         h.wfs,
		Validator:         h.val,
		Hooks:             hooks.New(hookRunner{h}, hooks.Options{RetryBackoff: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}),
		Config:            tenantSource{h},
		Daemon:            config.Daemon{ScratchDir: t.TempDir(), SpoolDir: t.TempDir(), IngestRoots: []string{t.TempDir()}},
		Budgets:           h.budgets,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		LeaseTTL:          o.lease,
		SweepInterval:     5 * time.Millisecond,
		ReconcileInterval: 20 * time.Millisecond,
		DefaultBackoff:    time.Millisecond,
		DaemonConcurrency: 8,
	}
	if h.deps.LeaseTTL == 0 {
		h.deps.LeaseTTL = time.Second
	}
	if o.maxDepth > 0 {
		h.deps.Bus = events.New(events.Options{Tenants: h.stores, Triggers: h.wfs, MaxDepth: o.maxDepth, PollInterval: 5 * time.Millisecond, Logger: h.deps.Logger})
	} else {
		h.deps.Bus = events.New(events.Options{Tenants: h.stores, Triggers: h.wfs, PollInterval: 5 * time.Millisecond, Logger: h.deps.Logger})
	}
	h.deps.Scheduler = scheduler.New(scheduler.Options{Tenants: h.stores, Triggers: h.wfs, Tick: time.Hour, Logger: h.deps.Logger})
	if !o.noEngine {
		h.startEngine()
	} else {
		h.engine = New(h.deps)
	}
	h.startWorker()
	t.Cleanup(h.stop)
	return h
}

// startEngine starts an engine, as a daemon process would.
func (h *harness) startEngine() {
	h.t.Helper()
	h.engine = New(h.deps)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	done := make(chan struct{})
	h.done = done
	go func() {
		defer close(done)
		_ = h.engine.Run(ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for h.engine.runCtxReady() == false {
		if time.Now().After(deadline) {
			h.t.Fatal("the engine did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

// stopEngine kills the daemon: everything in memory is gone.
func (h *harness) stopEngine() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

func (h *harness) stop() {
	h.stopWorker()
	h.stopEngine()
}

// ---- a fake vidyom-worker, through the same protocol as the real one ----

func (h *harness) startWorker() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.workerCancel = cancel
	done := make(chan struct{})
	h.workerDone = done
	coord := h.engine.Coordinator()
	if _, err := coord.Register(ctx, workerproto.RegisterRequest{
		WorkerID: "w1", Pools: []node.Pool{node.PoolMedia, node.PoolAI},
		Resources: domain.WorkerResources{CPUCores: 8, MemoryMB: 16000, DiskMB: 100000, MaxConcurrency: 8},
	}); err != nil {
		h.t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, pool := range []node.Pool{node.PoolMedia, node.PoolAI} {
		pool := pool
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.workerLoop(ctx, pool, &wg)
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
}

func (h *harness) stopWorker() {
	if h.workerCancel != nil {
		h.workerCancel()
		<-h.workerDone
		h.workerCancel = nil
	}
}

func (h *harness) workerLoop(ctx context.Context, pool node.Pool, wg *sync.WaitGroup) {
	for ctx.Err() == nil {
		got, err := h.q.Read(ctx, jobqueue.ReadOptions{Pool: pool, Consumer: "w1", Count: 8, Block: 10 * time.Millisecond})
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		for _, d := range got {
			d := d
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.workerTask(ctx, pool, d)
			}()
		}
	}
}

func (h *harness) workerTask(ctx context.Context, pool node.Pool, d jobqueue.Delivery) {
	if d.Pointer.NodeType == jobqueue.NodeTypeScratchCleanup {
		_ = h.q.Ack(ctx, pool, d.StreamID, d.Pointer.TaskID)
		return
	}
	coord := h.engine.Coordinator()
	lease, err := coord.Lease(ctx, d.Pointer.TaskID, workerproto.LeaseRequest{WorkerID: "w1"})
	if err != nil {
		if errs.IsCode(err, errs.CodeGone) {
			_ = h.q.Ack(ctx, pool, d.StreamID, d.Pointer.TaskID)
		}
		return
	}
	stop := make(chan struct{})
	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(h.deps.LeaseTTL / 4)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				resp, err := coord.TaskHeartbeat(ctx, lease.TaskID, workerproto.TaskHeartbeatRequest{LeaseID: lease.LeaseID, Percent: 10})
				if err != nil {
					if errs.IsCode(err, errs.CodeGone) {
						cancel()
						return
					}
					continue
				}
				if resp.Cancel {
					cancel()
				}
			}
		}
	}()
	defer close(stop)
	in := node.Input{Parameters: lease.Parameters, Attempt: lease.Attempt, IdempotencyKey: lease.IdempotencyKey, LeaseID: lease.LeaseID}
	env := &workerEnv{lease: lease}
	res, err := h.runBehaviorAs(taskCtx, lease.NodeType, lease.NodeID, env, in, true)
	// A result is reported whatever happened to the worker's reading loop, as the real
	// worker does.
	report := context.Background()
	if err == nil {
		err2 := coord.Complete(report, lease.TaskID, workerproto.CompleteRequest{LeaseID: lease.LeaseID, Output: res.Output, Tools: res.Tools, Usage: res.Usage})
		if err2 == nil || errs.IsCode(err2, errs.CodeGone) {
			_ = h.q.Ack(report, pool, d.StreamID, d.Pointer.TaskID)
		}
		return
	}
	coded := errs.Public(err)
	code := string(coded.Code)
	if errors.Is(err, context.Canceled) {
		code = CodeCancelled
	}
	err2 := coord.Fail(report, lease.TaskID, workerproto.FailRequest{
		LeaseID: lease.LeaseID, Code: code, Message: coded.Message, Retryable: node.IsTransient(err), OutcomeUnknown: node.IsUnknownOutcome(err),
	})
	if err2 == nil || errs.IsCode(err2, errs.CodeGone) {
		_ = h.q.Ack(report, pool, d.StreamID, d.Pointer.TaskID)
	}
}

// workerEnv is a minimal env for behaviors of worker-pool nodes.
type workerEnv struct {
	node.Env
	lease workerproto.Lease
}

func (w *workerEnv) NodeID() string          { return w.lease.NodeID }
func (w *workerEnv) Tenant() domain.TenantID { return w.lease.TenantID }

func (h *harness) runBehavior(ctx context.Context, nodeType string, env node.Env, in node.Input) (node.Result, error) {
	return h.runBehaviorAs(ctx, nodeType, env.NodeID(), env, in, false)
}

func (h *harness) runBehaviorAs(ctx context.Context, nodeType, nodeID string, env node.Env, in node.Input, worker bool) (node.Result, error) {
	h.mu.Lock()
	h.calls = append(h.calls, taskCall{NodeType: nodeType, NodeID: nodeID, Attempt: in.Attempt, Key: in.IdempotencyKey, Params: in.Parameters, Worker: worker})
	b := h.behaviors[nodeType]
	h.mu.Unlock()
	if b == nil {
		return node.JSONResult(map[string]any{"ok": true})
	}
	return b(ctx, env, in)
}

func (h *harness) on(nodeType string, b behavior) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.behaviors[nodeType] = b
}

func (h *harness) output(nodeType string, v any) {
	h.on(nodeType, func(context.Context, node.Env, node.Input) (node.Result, error) { return node.JSONResult(v) })
}

func (h *harness) callsOf(nodeID string) []taskCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []taskCall
	for _, c := range h.calls {
		if c.NodeID == nodeID {
			out = append(out, c)
		}
	}
	return out
}

// ---- helpers ----

var admin = domain.Principal{Email: "admin@example.com", Method: domain.AuthDevice, TenantID: "acme", Role: domain.RoleAdmin}
var editor = domain.Principal{Email: "editor@example.com", Method: domain.AuthDevice, TenantID: "acme", Role: domain.RoleEditor}
var publisher = domain.Principal{Email: "publisher@example.com", Method: domain.AuthDevice, TenantID: "acme", Role: domain.RolePublisher}
var viewer = domain.Principal{Email: "viewer@example.com", Method: domain.AuthDevice, TenantID: "acme", Role: domain.RoleViewer}

func scopeOf(p domain.Principal) service.Scope {
	return service.Scope{Tenant: "acme", Principal: p, RequestID: "req-test"}
}

func (h *harness) add(doc string) {
	h.t.Helper()
	res := h.val.Validate([]byte(doc))
	if !res.OK() {
		h.t.Fatalf("invalid test workflow: %+v", res.Issues)
	}
	if err := h.wfs.Add([]byte(doc), "test"); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) run(workflowID string, inputs any) domain.Job {
	h.t.Helper()
	return h.runAs(admin, workflowID, inputs)
}

func (h *harness) runAs(p domain.Principal, workflowID string, inputs any) domain.Job {
	h.t.Helper()
	raw, err := json.Marshal(inputs)
	if err != nil {
		h.t.Fatal(err)
	}
	if inputs == nil {
		raw = nil
	}
	resp, err := h.engine.Workflows().Run(context.Background(), scopeOf(p), workflowID, runRequest(raw))
	if err != nil {
		h.t.Fatalf("run %s: %v", workflowID, err)
	}
	job, err := h.store.Jobs().Get(context.Background(), id.Ref(resp.JobID))
	if err != nil {
		h.t.Fatal(err)
	}
	return job
}

func (h *harness) job(ref id.Friendly) domain.Job {
	h.t.Helper()
	j, err := h.store.Jobs().Get(context.Background(), id.Ref(ref))
	if err != nil {
		h.t.Fatal(err)
	}
	return j
}

func (h *harness) nodes(j domain.Job) map[string]domain.NodeExecution {
	h.t.Helper()
	list, err := h.store.Jobs().ListNodes(context.Background(), j.ULID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]domain.NodeExecution{}
	for _, n := range list {
		out[n.NodeID] = n
	}
	return out
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) waitStatus(j domain.Job, want domain.JobStatus) domain.Job {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		cur := h.job(j.ID)
		if cur.Status == want {
			return cur
		}
		if time.Now().After(deadline) || (cur.Status.Terminal() && !want.Terminal()) {
			detail := ""
			for _, n := range h.nodes(cur) {
				detail += fmt.Sprintf("\n  %s: %s attempt=%d err=%+v", n.NodeID, n.Status, n.Attempt, n.Error)
			}
			h.t.Fatalf("job %s (%s) is %s (reason %q, error %+v), want %s%s", cur.ID, cur.WorkflowID, cur.Status, cur.StatusReason, cur.Error, want, detail)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) nodeStatus(j domain.Job, nodeID string, want domain.NodeStatus) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("node %s to be %s", nodeID, want), func() bool { return h.nodes(j)[nodeID].Status == want })
}

func outputMap(t *testing.T, n domain.NodeExecution) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(n.Output, &out); err != nil {
		t.Fatalf("output of %s: %v: %s", n.NodeID, err, n.Output)
	}
	return out
}

func deliverEvents(h *harness) {
	h.t.Helper()
	if err := h.engine.Bus().Tick(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

type eventsStart = events.StartRequest
type schedulerStart = scheduler.StartRequest

func idULID(s string) id.ULID { return id.ULID(s) }

type connectorSecrets = connector.SecretResolver

func eventDraft() event.Draft { return event.Draft{Type: "custom.from_node"} }
