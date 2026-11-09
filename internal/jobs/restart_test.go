package jobs

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/queue/redistest"
)

const twoStepDoc = `{"id":"steps","version":"1.0.0","nodes":[
	{"id":"probe","type":"video.probe","typeVersion":1,"parameters":{"source":"storage://master/a"}},
	{"id":"cut","type":"video.thumbnail","typeVersion":1,"parameters":{"source":"storage://master/a"}}
],"connections":{"probe":["cut"]}}`

// TestDaemonRestartDuringWorkerTask is §1.1 step 13: the daemon dies during a
// transcode, the worker keeps going, and its result lands on the new daemon.
func TestDaemonRestartDuringWorkerTask(t *testing.T) {
	h := newHarness(t, harnessOpts{lease: 300 * time.Millisecond})
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.on("video.probe", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return node.Result{}, ctx.Err()
		}
		return node.JSONResult(map[string]any{"ext": "mp4"})
	})
	h.add(twoStepDoc)
	j := h.run("steps", nil)
	<-entered
	h.stopEngine()
	// Longer than the lease: nothing renewed it while the daemon was down.
	time.Sleep(400 * time.Millisecond)
	h.startEngine()
	close(release)
	h.waitStatus(j, domain.JobCompleted)
	if len(h.callsOf("probe")) != 1 {
		t.Fatalf("a task that kept running was started again: %d", len(h.callsOf("probe")))
	}
}

// TestWorkerDiesDuringTask: the lease expires and an idempotent node gets a new
// attempt with the same key.
func TestWorkerDiesDuringTask(t *testing.T) {
	h := newHarness(t, harnessOpts{lease: 50 * time.Millisecond})
	h.stopWorker()
	h.add(twoStepDoc)
	j := h.run("steps", nil)
	h.nodeStatus(j, "probe", domain.NodeQueued)
	ctx := context.Background()
	coord := h.engine.Coordinator()
	if _, err := coord.Register(ctx, workerproto.RegisterRequest{WorkerID: "doomed", Pools: []node.Pool{node.PoolMedia}}); err != nil {
		t.Fatal(err)
	}
	n := h.nodes(j)["probe"]
	first, err := coord.Lease(ctx, n.TaskID, workerproto.LeaseRequest{WorkerID: "doomed"})
	if err != nil {
		t.Fatal(err)
	}
	// It never heartbeats. A new attempt appears.
	h.waitFor("a new attempt", func() bool { return h.nodes(j)["probe"].Attempt == 2 })
	h.startWorker()
	done := h.waitStatus(j, domain.JobCompleted)
	if calls := h.callsOf("probe"); len(calls) != 1 || calls[0].Key != first.IdempotencyKey {
		t.Fatalf("calls %+v, first key %s", calls, first.IdempotencyKey)
	}
	// The dead worker comes back to life: its result is refused.
	if err := coord.Complete(ctx, first.TaskID, workerproto.CompleteRequest{LeaseID: first.LeaseID, Output: json.RawMessage(`{"ext":"zzz"}`)}); !errs.IsCode(err, errs.CodeGone) {
		t.Fatalf("stale complete: %v", err)
	}
	if out := outputMap(t, h.nodes(done)["probe"]); out["ok"] != true {
		t.Fatalf("output %v", out)
	}
}

// TestWorkerShutdownCancelledTaskIsRetried: a worker going away is not a cancel.
func TestWorkerStopReportedAsCancelledIsRetried(t *testing.T) {
	h := newHarness(t)
	var calls atomic.Int32
	h.on("video.probe", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		if calls.Add(1) == 1 {
			return node.Result{}, context.Canceled
		}
		return node.JSONResult(map[string]any{"ext": "mp4"})
	})
	h.add(twoStepDoc)
	j := h.waitStatus(h.run("steps", nil), domain.JobCompleted)
	if h.nodes(j)["probe"].Attempt != 2 {
		t.Fatalf("attempt %d", h.nodes(j)["probe"].Attempt)
	}
}

func TestInterruptedUnsafeAndUnreconcilableGoToAttention(t *testing.T) {
	h := newHarness(t, harnessOpts{lease: 50 * time.Millisecond})
	h.stopWorker()
	h.add(`{"id":"u","version":"1.0.0","nodes":[{"id":"n","type":"test.unsafe_worker","typeVersion":1}]}`)
	h.add(`{"id":"r","version":"1.0.0","nodes":[{"id":"n","type":"test.reconcilable_worker","typeVersion":1}]}`)
	ctx := context.Background()
	coord := h.engine.Coordinator()
	if _, err := coord.Register(ctx, workerproto.RegisterRequest{WorkerID: "doomed", Pools: []node.Pool{node.PoolMedia}}); err != nil {
		t.Fatal(err)
	}
	for _, wf := range []string{"u", "r"} {
		j := h.run(wf, nil)
		h.nodeStatus(j, "n", domain.NodeQueued)
		if _, err := coord.Lease(ctx, h.nodes(j)["n"].TaskID, workerproto.LeaseRequest{WorkerID: "doomed"}); err != nil {
			t.Fatal(err)
		}
		got := h.waitStatus(j, domain.JobNeedsAttention)
		if h.nodes(got)["n"].Attempt != 1 {
			t.Fatalf("%s retried: %+v", wf, h.nodes(got)["n"])
		}
		if !got.Attention.OutcomeUnknown {
			t.Fatalf("%s attention %+v", wf, got.Attention)
		}
	}
}

// TestDaemonPoolNodeFoundRunningAtStartup: nothing of the old process can still run
// it, so it is interrupted at once; with class reconcilable, Reconcile comes first.
func TestDaemonPoolNodeFoundRunningAtStartup(t *testing.T) {
	h := newHarness(t, harnessOpts{noEngine: true, lease: time.Hour})
	h.stopWorker()
	h.add(`{"id":"pub","version":"1.0.0","nodes":[{"id":"up","type":"publish.upload","typeVersion":1,"parameters":{"publicationId":"PUB-001"}}]}`)
	ctx := context.Background()
	impl, _ := h.reg.Lookup("publish.upload", 1)
	impl.(*reconcilerNode).reconcile = func(context.Context, node.Input) (node.ReconcileOutcome, node.Result, error) {
		res, err := node.JSONResult(map[string]any{"publication_id": "PUB-001", "remote_id": "found", "status": "processing"})
		return node.ReconcileDone, res, err
	}
	var uploads atomic.Int32
	h.on("publish.upload", func(context.Context, node.Env, node.Input) (node.Result, error) {
		uploads.Add(1)
		return node.JSONResult(map[string]any{})
	})
	j := h.run("pub", nil)
	if err := h.engine.advance(ctx, "acme", j.ULID); err != nil {
		t.Fatal(err)
	}
	n := h.nodes(j)["up"]
	if n.Status != domain.NodeQueued {
		t.Fatalf("node %s", n.Status)
	}
	// The old process leased it, began the upload and died with the lease still valid.
	if _, err := h.engine.lease(ctx, n.TaskID, daemonConsumer, false); err != nil {
		t.Fatal(err)
	}
	h.startEngine()
	done := h.waitStatus(j, domain.JobCompleted)
	if uploads.Load() != 0 {
		t.Fatalf("the upload ran again: %d", uploads.Load())
	}
	if outputMap(t, h.nodes(done)["up"])["remote_id"] != "found" {
		t.Fatalf("output %s", h.nodes(done)["up"].Output)
	}
}

func TestReconciliationReenqueuesAfterFlush(t *testing.T) {
	h := newHarness(t)
	h.stopWorker()
	h.add(twoStepDoc)
	j := h.run("steps", nil)
	h.nodeStatus(j, "probe", domain.NodeQueued)
	h.waitFor("the pointer", func() bool {
		ok, _ := h.q.Inflight(context.Background(), h.nodes(j)["probe"].TaskID)
		return ok
	})
	h.mr.FlushAll()
	h.startWorker()
	h.waitStatus(j, domain.JobCompleted)
	if len(h.callsOf("probe")) != 1 || len(h.callsOf("cut")) != 1 {
		t.Fatalf("probe %d cut %d", len(h.callsOf("probe")), len(h.callsOf("cut")))
	}
}

func TestDuplicatePointerRunsOnce(t *testing.T) {
	h := newHarness(t)
	h.stopWorker()
	h.add(twoStepDoc)
	j := h.run("steps", nil)
	h.nodeStatus(j, "probe", domain.NodeQueued)
	n := h.nodes(j)["probe"]
	ctx := context.Background()
	// A duplicate entry, as a lost inflight record could produce.
	if err := h.client.HDel(ctx, jobqueue.InflightKey, n.TaskID).Err(); err != nil {
		t.Fatal(err)
	}
	if err := h.q.Enqueue(ctx, jobqueue.Pointer{TaskID: n.TaskID, TenantID: "acme", Pool: node.PoolMedia, NodeType: n.NodeType}); err != nil {
		t.Fatal(err)
	}
	h.startWorker()
	h.waitStatus(j, domain.JobCompleted)
	if len(h.callsOf("probe")) != 1 {
		t.Fatalf("a duplicated pointer ran twice: %d", len(h.callsOf("probe")))
	}
}

// ---- real Redis (§15.5, D4) ----

// realHarness runs the engine and a worker over a real redis-server.
func realHarness(t *testing.T) (*harness, *redistest.Server) {
	t.Helper()
	server := redistest.Start(t)
	client := server.Client()
	t.Cleanup(func() { _ = client.Close() })
	// A long lease: these tests are about the queue, and a worker that stopped reading
	// also stops its heartbeats while its task keeps running.
	h := newHarness(t, harnessOpts{redis: client, lease: time.Minute})
	return h, server
}

func assertEachRanOnce(t *testing.T, h *harness, nodes ...string) {
	t.Helper()
	for _, n := range nodes {
		if got := len(h.callsOf(n)); got != 1 {
			t.Errorf("node %s ran %d times", n, got)
		}
	}
}

func TestRealRedisFlushAll(t *testing.T) {
	h, server := realHarness(t)
	h.stopWorker()
	h.add(twoStepDoc)
	ctx := context.Background()
	j := h.run("steps", nil)
	h.nodeStatus(j, "probe", domain.NodeQueued)
	h.waitFor("the pointer", func() bool {
		ok, _ := h.q.Inflight(ctx, h.nodes(j)["probe"].TaskID)
		return ok
	})
	c := server.Client()
	defer c.Close()
	if err := c.FlushAll(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Exists(ctx, jobqueue.StreamKey(node.PoolMedia)).Result(); err != nil || n != 0 {
		t.Fatalf("the stream must be gone: %d %v", n, err)
	}
	h.startWorker()
	h.waitStatus(j, domain.JobCompleted)
	assertEachRanOnce(t, h, "probe", "cut")
}

// TestRealRedisLosesItsDataUnderARunningJob kills redis-server, wipes its files and
// starts it empty, while a job is in progress and other jobs are queued.
func TestRealRedisLosesItsDataUnderARunningJob(t *testing.T) {
	h, server := realHarness(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.on("video.probe", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		// It keeps running while everything around it restarts.
		<-release
		return node.JSONResult(map[string]any{"ext": "mp4"})
	})
	h.add(twoStepDoc)
	h.add(`{"id":"waiting","version":"1.0.0","nodes":[{"id":"later","type":"video.thumbnail","typeVersion":1,"parameters":{"source":"storage://master/b"}}]}`)
	running := h.run("steps", nil)
	<-entered
	h.stopWorkerKeepTask()
	queued := h.run("waiting", nil)
	h.nodeStatus(queued, "later", domain.NodeQueued)

	server.Kill()
	server.WipeData()
	server.Restart()

	h.startWorker()
	close(release)
	h.waitStatus(queued, domain.JobCompleted)
	h.waitStatus(running, domain.JobCompleted)
	assertEachRanOnce(t, h, "probe", "later", "cut")
}

// stopWorkerKeepTask stops reading new work while running tasks continue.
func (h *harness) stopWorkerKeepTask() {
	h.workerCancel()
	h.workerCancel = nil
}

// TestRealRedisAndDaemonDieTogether is the worst of §15.5: the daemon and Redis both
// lose everything in memory, jobs are in every state, and each node still runs once.
func TestRealRedisAndDaemonDieTogether(t *testing.T) {
	h, server := realHarness(t)
	ctx := context.Background()
	h.add(twoStepDoc)
	asset := h.newAsset(domain.KindOriginal)
	h.add(`{"id":"gate","version":"1.0.0","inputs":{"asset":{"type":"string"}},"nodes":[
		{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"metadata_change","assetId":"${inputs.asset}"}},
		{"id":"after_gate","type":"video.probe","typeVersion":1,"parameters":{"source":"storage://master/c"}}
	],"connections":{"ask":["after_gate"]}}`)
	done := h.waitStatus(h.run("steps", nil), domain.JobCompleted)
	h.stopWorker()
	queued := h.run("steps", nil)
	h.nodeStatus(queued, "probe", domain.NodeQueued)
	waiting := h.run("gate", map[string]any{"asset": asset.ID})
	a := h.pendingApproval(waiting)

	h.stopEngine()
	server.Kill()
	server.WipeData()
	server.Restart()
	h.startEngine()
	h.startWorker()

	h.waitStatus(queued, domain.JobCompleted)
	if _, err := h.engine.Approvals().Approve(ctx, scopeOf(publisher), id.Ref(a.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(waiting, domain.JobCompleted)
	if h.job(done.ID).Status != domain.JobCompleted {
		t.Fatal("a finished job must stay finished")
	}
	active, err := h.store.Jobs().ListActive(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("active %+v %v", active, err)
	}
	if got := len(h.callsOf("after_gate")); got != 1 {
		t.Fatalf("after_gate ran %d times", got)
	}
	// Two runs of the workflow, one node run each.
	if got := len(h.callsOf("probe")); got != 2 {
		t.Fatalf("probe ran %d times", got)
	}
	if got := len(h.callsOf("cut")); got != 2 {
		t.Fatalf("cut ran %d times", got)
	}
}

func TestStartupWithRedisDownThenHeals(t *testing.T) {
	server := redistest.Start(t)
	client := server.Client()
	t.Cleanup(func() { _ = client.Close() })
	h := newHarness(t, harnessOpts{redis: client})
	h.add(twoStepDoc)
	server.Kill()
	j := h.run("steps", nil)
	// The node is durably queued, though no pointer could be written.
	h.nodeStatus(j, "probe", domain.NodeQueued)
	server.Restart()
	h.waitStatus(j, domain.JobCompleted)
	assertEachRanOnce(t, h, "probe", "cut")
}
