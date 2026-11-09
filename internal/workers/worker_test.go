package workers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/connectors/storage/local"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/workers"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
)

// testNode is a scriptable node.
type testNode struct {
	desc node.Descriptor
	run  func(ctx context.Context, env node.Env, in node.Input) (node.Result, error)
}

func (n testNode) Descriptor() node.Descriptor { return n.desc }
func (n testNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	return n.run(ctx, env, in)
}

func script(nodeType string, supportsAbort bool, run func(ctx context.Context, env node.Env, in node.Input) (node.Result, error)) testNode {
	return testNode{
		desc: node.Descriptor{Type: nodeType, TypeVersion: 1, Pool: node.PoolMedia, SideEffect: node.EffectIdempotent, SupportsAbort: supportsAbort},
		run:  run,
	}
}

func ok(_ context.Context, _ node.Env, _ node.Input) (node.Result, error) {
	return node.JSONResult(map[string]any{"done": true})
}

// rig runs a worker against the fake daemon and queue.
type rig struct {
	t        *testing.T
	daemon   *fakeDaemon
	queue    *fakeQueue
	registry node.Registry
	cfg      workers.Config
	tenant   domain.TenantID
	job      id.ULID
	stop     context.CancelFunc
	done     chan error
	logs     *syncBuffer

	accessLog string
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	daemon := newFakeDaemon(t)
	connectors := connector.NewRegistry()
	local.Register(connectors)
	buf := &syncBuffer{}
	r := &rig{
		t:        t,
		daemon:   daemon,
		queue:    newFakeQueue(),
		registry: node.NewRegistry(),
		tenant:   "acme",
		job:      newJob(),
		logs:     buf,
		cfg: workers.Config{
			WorkerID:          "worker-1",
			Pools:             []node.Pool{node.PoolMedia},
			Connectors:        connectors,
			ScratchDir:        filepath.Join(root, "scratch"),
			CacheDir:          filepath.Join(root, "cache"),
			SpoolDir:          filepath.Join(root, "spool"),
			Resources:         domain.WorkerResources{CPUCores: 4, MemoryMB: 1024, DiskMB: 1024, MaxConcurrency: 2},
			ShutdownGrace:     200 * time.Millisecond,
			HeartbeatInterval: 20 * time.Millisecond,
			ReadBlock:         20 * time.Millisecond,
			ClaimInterval:     20 * time.Millisecond,
			ClaimMinIdle:      time.Hour,
			RetryMax:          50 * time.Millisecond,
			Housekeeping:      time.Hour,
			Logger:            slog.New(slog.NewJSONHandler(buf, nil)),
		},
	}
	r.accessLog = filepath.Join(root, "access.jsonl")
	params, err := json.Marshal(map[string]any{"accessLog": r.accessLog})
	if err != nil {
		t.Fatal(err)
	}
	usage := map[domain.StorageRole]config.StorageUsage{
		domain.RoleArchive: config.UsageBackupOnly,
		domain.RoleMaster:  config.UsageOperational,
		domain.RoleProxy:   config.UsageOperational,
	}
	for _, role := range domain.StorageRoles {
		daemon.roles[role] = config.StorageInstance{
			Driver: local.Driver, Root: filepath.Join(root, "roles", string(role)), Usage: usage[role], Parameters: params,
		}
	}
	return r
}

func (r *rig) start() {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	r.done = make(chan error, 1)
	cfg := r.cfg
	cfg.Coordinator = r.daemon.coordinator(r.t)
	cfg.Queue = r.queue
	go func() { r.done <- workers.Run(ctx, cfg, r.registry) }()
	r.t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(30 * time.Second):
			r.t.Error("the worker did not stop")
		}
	})
}

// shutdown stops the worker and waits for it.
func (r *rig) shutdown() error {
	r.t.Helper()
	r.stop()
	select {
	case err := <-r.done:
		r.done <- err
		return err
	case <-time.After(30 * time.Second):
		r.t.Fatal("the worker did not stop")
		return nil
	}
}

// submit adds a node task to the daemon and its pointer to the queue.
func (r *rig) submit(nodeID, nodeType string, params any, mods ...func(*workerproto.Lease)) (string, *task) {
	r.t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		r.t.Fatal(err)
	}
	taskID := jobqueue.TaskID(r.job, nodeID, 1)
	lease := workerproto.Lease{
		TaskID:         taskID,
		LeaseID:        "lease-" + nodeID,
		ExpiresAt:      time.Now().Add(time.Minute),
		TenantID:       r.tenant,
		JobID:          "JOB-001",
		JobULID:        r.job,
		NodeID:         nodeID,
		NodeType:       nodeType,
		TypeVersion:    1,
		Attempt:        1,
		Parameters:     data,
		IdempotencyKey: node.IdempotencyKey(r.tenant, r.job, nodeID),
	}
	for _, mod := range mods {
		mod(&lease)
	}
	tk := r.daemon.addTask(lease)
	if err := r.queue.Enqueue(context.Background(), jobqueue.Pointer{
		TaskID: taskID, TenantID: r.tenant, Pool: node.PoolMedia, NodeType: nodeType, EnqueuedAt: time.Now(),
	}); err != nil {
		r.t.Fatal(err)
	}
	return taskID, tk
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRegisterDeclaresResources(t *testing.T) {
	r := newRig(t)
	r.start()
	eventually(t, "registration", func() bool {
		var n int
		r.daemon.with(func() { n = len(r.daemon.registers) })
		return n == 1
	})
	r.daemon.with(func() {
		reg := r.daemon.registers[0]
		if reg.WorkerID != "worker-1" || len(reg.Pools) != 1 || reg.Pools[0] != node.PoolMedia {
			t.Errorf("%+v", reg)
		}
		if reg.Resources.CPUCores != 4 || reg.Resources.MaxConcurrency != 2 {
			t.Errorf("%+v", reg.Resources)
		}
	})
	eventually(t, "a worker heartbeat", func() bool {
		var n int
		r.daemon.with(func() { n = len(r.daemon.heartbeats) })
		return n > 0
	})
}

// I154: a daemon that lost this worker answers not_found, and refuses leases until the
// worker registers again.
func TestRegistersAgainWhenTheDaemonForgets(t *testing.T) {
	r := newRig(t)
	r.start()
	eventually(t, "registration", func() bool {
		var n int
		r.daemon.with(func() { n = len(r.daemon.registers) })
		return n == 1
	})
	r.daemon.with(func() { r.daemon.heartbeatErr = errs.NotFound("worker") })
	eventually(t, "a second registration", func() bool {
		var n int
		r.daemon.with(func() { n = len(r.daemon.registers) })
		return n >= 2
	})
}

// I190: the job's stream, which clients read, gets no internal cause and no stack.
func TestInternalCausesStayOutOfTheJobStream(t *testing.T) {
	r := newRig(t)
	const secret = "/data/acme/private-title-of-a-recording.mp4: Invalid data"
	r.registry.RegisterNode(script("test.fail", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, errs.Wrap(errs.CodeFailedPrecondition, errors.New(secret), "the media tool could not process this file")
	}))
	r.registry.RegisterNode(script("test.panic", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
		panic(secret)
	}))
	failing, _ := r.submit("n1", "test.fail", nil)
	panicking, _ := r.submit("n2", "test.panic", nil)
	r.start()
	r.daemon.waitDone(t, failing)
	r.daemon.waitDone(t, panicking)
	r.daemon.with(func() {
		for _, taskID := range []string{failing, panicking} {
			for _, line := range r.daemon.logs[taskID] {
				if strings.Contains(line.Message, "private-title") || strings.Contains(line.Message, "goroutine") {
					t.Errorf("the job's stream got an internal cause: %s", line.Message)
				}
			}
			if strings.Contains(r.daemon.failed[taskID].Message, "private-title") {
				t.Errorf("the failure record got an internal cause: %+v", r.daemon.failed[taskID])
			}
		}
		if len(r.daemon.logs[failing]) == 0 || !strings.Contains(r.daemon.logs[failing][0].Message, "could not process") {
			t.Errorf("the job's stream lacks the public reason: %+v", r.daemon.logs[failing])
		}
	})
	if !strings.Contains(r.logs.String(), "private-title") {
		t.Error("the worker's own log lost the cause")
	}
}

func TestCompleteThenAck(t *testing.T) {
	r := newRig(t)
	var executed atomic.Int64
	r.registry.RegisterNode(script("test.ok", false, func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		executed.Add(1)
		var p map[string]any
		if err := in.Bind(&p); err != nil {
			return node.Result{}, err
		}
		if env.Tenant() != "acme" || env.JobID() != "JOB-001" || env.NodeID() != "n1" || in.Attempt != 1 || in.LeaseID != "lease-n1" {
			t.Errorf("environment: %s %s %s %+v", env.Tenant(), env.JobID(), env.NodeID(), in)
		}
		return node.Result{
			Output: json.RawMessage(`{"echo":true}`),
			Tools:  map[string]string{"ffmpeg": "9"},
			Usage:  []domain.UsageRecord{{Operation: "x", Quantity: 1}},
		}, nil
	}))
	taskID, _ := r.submit("n1", "test.ok", map[string]any{"a": 1})
	r.start()
	r.daemon.waitDone(t, taskID)
	out := r.daemon.completion(t, taskID)
	if out["echo"] != true || executed.Load() != 1 {
		t.Fatalf("%+v, %d runs", out, executed.Load())
	}
	r.daemon.with(func() {
		req := r.daemon.completed[taskID]
		if req.LeaseID != "lease-n1" || req.Tools["ffmpeg"] != "9" || len(req.Usage) != 1 {
			t.Errorf("%+v", req)
		}
	})
	eventually(t, "the ack", func() bool { return r.queue.isAcked(taskID) })
}

// I4: the daemon commits first, and only then does the entry leave the stream.
func TestCompleteIsRedeliveredUntilAcknowledged(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(script("test.ok", false, ok))
	taskID, tk := r.submit("n1", "test.ok", nil)
	r.daemon.with(func() {
		tk.completeErrors = []error{errs.New(errs.CodeUnavailable, "restarting"), errs.New(errs.CodeUnavailable, "restarting"), nil}
	})
	r.start()
	eventually(t, "two refused deliveries", func() bool {
		var n int
		r.daemon.with(func() { n = r.daemon.calls["worker.tasks.complete"] })
		return n >= 2
	})
	r.daemon.waitDone(t, taskID)
	eventually(t, "the ack", func() bool { return r.queue.isAcked(taskID) })
	r.daemon.with(func() {
		if r.daemon.calls["worker.tasks.complete"] != 3 {
			t.Errorf("%d deliveries", r.daemon.calls["worker.tasks.complete"])
		}
	})
}

func TestNotAckedBeforeTheDaemonCommits(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(script("test.ok", false, ok))
	taskID, tk := r.submit("n1", "test.ok", nil)
	r.daemon.with(func() {
		tk.completeErrors = make([]error, 1000)
		for i := range tk.completeErrors {
			tk.completeErrors[i] = errs.New(errs.CodeUnavailable, "down")
		}
	})
	r.start()
	eventually(t, "some deliveries", func() bool {
		var n int
		r.daemon.with(func() { n = r.daemon.calls["worker.tasks.complete"] })
		return n >= 3
	})
	if r.queue.isAcked(taskID) {
		t.Fatal("acked before the daemon committed")
	}
	if err := r.shutdown(); err != nil {
		t.Fatal(err)
	}
	if r.queue.isAcked(taskID) {
		t.Fatal("acked at shutdown without a commit")
	}
}

func TestCompleteGoneAcksAndDrops(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(script("test.ok", false, ok))
	taskID, tk := r.submit("n1", "test.ok", nil)
	r.daemon.with(func() {
		tk.completeErrors = make([]error, 1000)
		tk.completeErrors[0] = errs.New(errs.CodeGone, "someone else")
	})
	r.start()
	eventually(t, "the ack", func() bool { return r.queue.isAcked(taskID) })
}

func TestLeaseOutcomes(t *testing.T) {
	r := newRig(t)
	var executed atomic.Int64
	r.registry.RegisterNode(script("test.ok", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
		executed.Add(1)
		return node.JSONResult(map[string]any{})
	}))
	gone, tkGone := r.submit("gone", "test.ok", nil)
	conflict, tkConflict := r.submit("conflict", "test.ok", nil)
	unavailable, tkDown := r.submit("down", "test.ok", nil)
	r.daemon.with(func() {
		tkGone.leaseErr = errs.New(errs.CodeGone, "obsolete")
		tkConflict.leaseErr = errs.New(errs.CodeConflict, "incompatible")
		tkDown.leaseErr = errs.New(errs.CodeUnavailable, "restarting")
	})
	r.start()
	eventually(t, "all three leases", func() bool {
		var n int
		r.daemon.with(func() { n = len(r.daemon.leases) })
		return n >= 3
	})
	eventually(t, "410 acks", func() bool { return r.queue.isAcked(gone) })
	time.Sleep(50 * time.Millisecond)
	if r.queue.isAcked(conflict) {
		t.Error("409 must leave the entry for another worker")
	}
	if r.queue.isAcked(unavailable) {
		t.Error("an unreachable daemon must leave the entry pending")
	}
	if executed.Load() != 0 {
		t.Errorf("%d nodes ran with no lease", executed.Load())
	}
}

func TestFailureClassification(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		code          string
		retryable     bool
		unknown       bool
		hiddenMessage string
	}{
		{"transient", node.Transient(errs.New(errs.CodeUnavailable, "storage down")), "unavailable", true, false, ""},
		{"unknown outcome", node.Unknown(errs.New(errs.CodeUnavailable, "connection lost mid-upload")), "unavailable", false, true, ""},
		{"permanent", node.Permanent(errs.New(errs.CodeInvalidArgument, "bad parameter")), "invalid_argument", false, false, ""},
		{"unclassified coded", errs.New(errs.CodeForbiddenStorageRole, "refused"), "forbidden_storage_role", false, false, ""},
		{"uncoded", errors.New("secret detail: /data/acme/private-title.mp4"), "internal", false, false, "private-title"},
		{"wrapped cause", errs.Wrap(errs.CodeInternal, errors.New("cause with secret-token"), "failed"), "internal", false, false, "secret-token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.registry.RegisterNode(script("test.fail", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
				return node.Result{}, c.err
			}))
			taskID, _ := r.submit("n1", "test.fail", nil)
			r.start()
			r.daemon.waitDone(t, taskID)
			got := r.daemon.failure(t, taskID)
			if got.Code != c.code || got.Retryable != c.retryable || got.OutcomeUnknown != c.unknown {
				t.Errorf("%+v", got)
			}
			if c.hiddenMessage != "" && strings.Contains(got.Message, c.hiddenMessage) {
				t.Errorf("an internal cause reached the daemon's record: %q", got.Message)
			}
			eventually(t, "the ack", func() bool { return r.queue.isAcked(taskID) })
		})
	}
}

func TestPanicBecomesAFailureAndTheWorkerLives(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(script("test.panic", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
		panic("node bug")
	}))
	r.registry.RegisterNode(script("test.ok", false, ok))
	panicking, _ := r.submit("n1", "test.panic", nil)
	r.start()
	r.daemon.waitDone(t, panicking)
	if got := r.daemon.failure(t, panicking); got.Code != "internal" || got.Retryable {
		t.Errorf("%+v", got)
	}
	after, _ := r.submit("n2", "test.ok", nil)
	r.daemon.waitDone(t, after)
	r.daemon.completion(t, after)
}

func TestUnknownNodeFails(t *testing.T) {
	r := newRig(t)
	taskID, _ := r.submit("n1", "test.absent", nil)
	r.start()
	r.daemon.waitDone(t, taskID)
	if got := r.daemon.failure(t, taskID); got.Code != "not_implemented" || got.Retryable {
		t.Errorf("%+v", got)
	}
}

func TestNodeOfAnotherPoolIsRefused(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(testNode{
		desc: node.Descriptor{Type: "test.ai", TypeVersion: 1, Pool: node.PoolAI},
		run:  ok,
	})
	taskID, _ := r.submit("n1", "test.ai", nil)
	r.start()
	r.daemon.waitDone(t, taskID)
	if got := r.daemon.failure(t, taskID); got.Code != "internal" {
		t.Errorf("%+v", got)
	}
}

func TestOutputCapAndSuspend(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(script("test.huge", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
		huge := make([]byte, domain.MaxNodeOutputBytes+1)
		return node.Result{Output: huge}, nil
	}))
	r.registry.RegisterNode(script("test.suspend", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{Suspend: &node.Suspension{Reason: "approval"}}, nil
	}))
	huge, _ := r.submit("n1", "test.huge", nil)
	suspend, _ := r.submit("n2", "test.suspend", nil)
	r.start()
	r.daemon.waitDone(t, huge)
	if got := r.daemon.failure(t, huge); got.Code != "output_too_large" {
		t.Errorf("%+v", got)
	}
	r.daemon.waitDone(t, suspend)
	if got := r.daemon.failure(t, suspend); got.Code != "internal" {
		t.Errorf("%+v", got)
	}
}

func TestHeartbeatCarriesProgressAndLogsReachTheJob(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	r.registry.RegisterNode(script("test.slow", false, func(ctx context.Context, env node.Env, _ node.Input) (node.Result, error) {
		env.Progress(42, "halfway")
		env.Logger().Info("encoding started", "profile", "master-1080p@1")
		<-release
		return node.JSONResult(map[string]any{})
	}))
	taskID, _ := r.submit("n1", "test.slow", nil)
	r.start()
	eventually(t, "a heartbeat with progress", func() bool {
		var got workerproto.TaskHeartbeatRequest
		r.daemon.with(func() { got = r.daemon.lastBeat[taskID] })
		return got.Percent == 42 && got.Message == "halfway" && got.LeaseID == "lease-n1"
	})
	close(release)
	r.daemon.waitDone(t, taskID)
	r.daemon.with(func() {
		lines := r.daemon.logs[taskID]
		if len(lines) == 0 || !strings.Contains(lines[0].Message, "encoding started") || !strings.Contains(lines[0].Message, "master-1080p@1") {
			t.Errorf("log lines %+v", lines)
		}
	})
	if !strings.Contains(r.logs.String(), `"job_id":"JOB-001"`) {
		t.Errorf("the worker's log lacks the job: %s", r.logs.String())
	}
}

// D11: the cancel flag aborts a node that supports it.
func TestCancelFlagAbortsANodeThatCan(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(script("test.abortable", true, func(ctx context.Context, _ node.Env, _ node.Input) (node.Result, error) {
		<-ctx.Done()
		return node.Result{}, ctx.Err()
	}))
	taskID, tk := r.submit("n1", "test.abortable", nil)
	r.daemon.with(func() { tk.cancel = true })
	r.start()
	r.daemon.waitDone(t, taskID)
	if got := r.daemon.failure(t, taskID); got.Code != "cancelled" || got.Retryable {
		t.Errorf("%+v", got)
	}
	eventually(t, "the ack", func() bool { return r.queue.isAcked(taskID) })
}

// D11: a node that cannot abort finishes, and the engine stops at the next checkpoint.
func TestCancelFlagLetsANonAbortableNodeFinish(t *testing.T) {
	r := newRig(t)
	var cancelledEarly atomic.Bool
	r.registry.RegisterNode(script("test.solid", false, func(ctx context.Context, _ node.Env, _ node.Input) (node.Result, error) {
		select {
		case <-ctx.Done():
			cancelledEarly.Store(true)
		case <-time.After(150 * time.Millisecond):
		}
		return node.JSONResult(map[string]any{"finished": true})
	}))
	taskID, tk := r.submit("n1", "test.solid", nil)
	r.daemon.with(func() { tk.cancel = true })
	r.start()
	r.daemon.waitDone(t, taskID)
	if cancelledEarly.Load() {
		t.Fatal("a node without abort support was cancelled")
	}
	if out := r.daemon.completion(t, taskID); out["finished"] != true {
		t.Fatalf("%+v", out)
	}
}

func TestHeartbeatGoneAbortsAndDropsTheResult(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(script("test.long", false, func(ctx context.Context, _ node.Env, _ node.Input) (node.Result, error) {
		<-ctx.Done()
		return node.JSONResult(map[string]any{"late": true})
	}))
	taskID, tk := r.submit("n1", "test.long", nil)
	r.daemon.with(func() { tk.heartbeatErr = errs.New(errs.CodeGone, "lease expired") })
	r.start()
	eventually(t, "the ack", func() bool { return r.queue.isAcked(taskID) })
	r.daemon.with(func() {
		if _, reported := r.daemon.completed[taskID]; reported {
			t.Error("a result was reported for a lost lease")
		}
		if _, reported := r.daemon.failed[taskID]; reported {
			t.Error("a failure was reported for a lost lease")
		}
	})
}

// §1.1 step 13: a daemon restart during a transcode loses nothing.
func TestDaemonOutageDoesNotStopANode(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	var cancelled atomic.Bool
	r.registry.RegisterNode(script("test.long", true, func(ctx context.Context, _ node.Env, _ node.Input) (node.Result, error) {
		select {
		case <-ctx.Done():
			cancelled.Store(true)
			return node.Result{}, ctx.Err()
		case <-release:
		}
		return node.JSONResult(map[string]any{"survived": true})
	}))
	taskID, _ := r.submit("n1", "test.long", nil)
	r.start()
	eventually(t, "the first heartbeat", func() bool {
		var n int
		r.daemon.with(func() { n = r.daemon.taskBeats[taskID] })
		return n > 0
	})
	r.daemon.setDown(true)
	time.Sleep(150 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("an outage cancelled the node")
	}
	close(release)
	time.Sleep(100 * time.Millisecond)
	if r.queue.isAcked(taskID) {
		t.Fatal("acked while the daemon was down")
	}
	r.daemon.setDown(false)
	r.daemon.waitDone(t, taskID)
	if out := r.daemon.completion(t, taskID); out["survived"] != true {
		t.Fatalf("%+v", out)
	}
	eventually(t, "the ack", func() bool { return r.queue.isAcked(taskID) })
}

func TestLongOutageAbortsAsRetryable(t *testing.T) {
	r := newRig(t)
	r.cfg.DaemonOutageLimit = 50 * time.Millisecond
	started := make(chan struct{})
	var once sync.Once
	r.registry.RegisterNode(script("test.long", true, func(ctx context.Context, _ node.Env, _ node.Input) (node.Result, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return node.Result{}, ctx.Err()
	}))
	taskID, _ := r.submit("n1", "test.long", nil)
	r.start()
	<-started
	r.daemon.setDown(true)
	time.Sleep(200 * time.Millisecond)
	r.daemon.setDown(false)
	r.daemon.waitDone(t, taskID)
	if got := r.daemon.failure(t, taskID); got.Code != "unavailable" || !got.Retryable {
		t.Errorf("%+v", got)
	}
}

func TestGracefulShutdownLetsANodeFinish(t *testing.T) {
	r := newRig(t)
	r.cfg.ShutdownGrace = 5 * time.Second
	started := make(chan struct{})
	r.registry.RegisterNode(script("test.brief", true, func(ctx context.Context, _ node.Env, _ node.Input) (node.Result, error) {
		close(started)
		select {
		case <-ctx.Done():
			return node.Result{}, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		return node.JSONResult(map[string]any{"finished": true})
	}))
	taskID, _ := r.submit("n1", "test.brief", nil)
	r.start()
	<-started
	if err := r.shutdown(); err != nil {
		t.Fatal(err)
	}
	if out := r.daemon.completion(t, taskID); out["finished"] != true {
		t.Fatalf("%+v", out)
	}
	if !r.queue.isAcked(taskID) {
		t.Fatal("not acked")
	}
}

func TestShutdownCancelsAfterGraceAndReportsRetryable(t *testing.T) {
	r := newRig(t)
	r.cfg.ShutdownGrace = 50 * time.Millisecond
	started := make(chan struct{})
	r.registry.RegisterNode(script("test.long", true, func(ctx context.Context, _ node.Env, _ node.Input) (node.Result, error) {
		close(started)
		<-ctx.Done()
		return node.Result{}, ctx.Err()
	}))
	taskID, _ := r.submit("n1", "test.long", nil)
	r.start()
	<-started
	if err := r.shutdown(); err != nil {
		t.Fatal(err)
	}
	if got := r.daemon.failure(t, taskID); got.Code != "unavailable" || !got.Retryable {
		t.Errorf("%+v", got)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	r := newRig(t)
	r.cfg.Resources.MaxConcurrency = 2
	var current, peak atomic.Int64
	r.registry.RegisterNode(script("test.count", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
		now := current.Add(1)
		for {
			prior := peak.Load()
			if now <= prior || peak.CompareAndSwap(prior, now) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		current.Add(-1)
		return node.JSONResult(map[string]any{})
	}))
	var ids []string
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		taskID, _ := r.submit(name, "test.count", nil)
		ids = append(ids, taskID)
	}
	r.start()
	for _, taskID := range ids {
		eventually(t, "completion", func() bool { return r.queue.isAcked(taskID) })
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d, want 2", peak.Load())
	}
}

func TestStaleEntriesAreClaimedAndRunningOnesSkipped(t *testing.T) {
	r := newRig(t)
	r.cfg.ClaimMinIdle = 10 * time.Millisecond
	var runs atomic.Int64
	release := make(chan struct{})
	r.registry.RegisterNode(script("test.long", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
		runs.Add(1)
		<-release
		return node.JSONResult(map[string]any{})
	}))
	taskID, _ := r.submit("n1", "test.long", nil)
	// Another consumer read this entry and died.
	stale, _ := r.submit("stale", "test.long", nil)
	func() {
		r.queue.mu.Lock()
		defer r.queue.mu.Unlock()
		for _, e := range r.queue.streams[node.PoolMedia] {
			if e.delivery.Pointer.TaskID == stale {
				e.delivered = time.Now().Add(-time.Hour)
				e.consumer = "dead-worker"
				e.pending = true
			}
		}
	}()
	r.start()
	eventually(t, "both tasks running", func() bool { return runs.Load() == 2 })
	time.Sleep(100 * time.Millisecond)
	if runs.Load() != 2 {
		t.Fatalf("a running task started again: %d runs", runs.Load())
	}
	close(release)
	eventually(t, "both acks", func() bool { return r.queue.isAcked(taskID) && r.queue.isAcked(stale) })
}

func TestRegisterWaitsForTheDaemonThenRefusesABadToken(t *testing.T) {
	r := newRig(t)
	r.daemon.with(func() { r.daemon.registerErr = errs.New(errs.CodeUnavailable, "starting") })
	r.start()
	eventually(t, "retries", func() bool {
		var n int
		r.daemon.with(func() { n = r.daemon.calls["worker.register"] })
		return n >= 2
	})
	r.daemon.with(func() { r.daemon.registerErr = nil })
	eventually(t, "registration", func() bool {
		var n int
		r.daemon.with(func() { n = len(r.daemon.registers) })
		return n == 1
	})

	refused := newRig(t)
	refused.daemon.with(func() { refused.daemon.registerErr = errs.New(errs.CodeUnauthenticated, "bad token") })
	refused.start()
	select {
	case err := <-refused.done:
		refused.done <- err
		if !errs.IsCode(err, errs.CodeUnauthenticated) {
			t.Fatalf("%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a refused worker kept running")
	}
}

func TestProtocolMismatchFails(t *testing.T) {
	r := newRig(t)
	r.daemon.with(func() { r.daemon.registerResp.ProtocolVersion = workerproto.ProtocolVersion + 1 })
	r.start()
	select {
	case err := <-r.done:
		r.done <- err
		if err == nil || !strings.Contains(err.Error(), "protocol") {
			t.Fatalf("%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a mismatched worker kept running")
	}
}

func TestRunRefusesBadConfig(t *testing.T) {
	ctx := context.Background()
	registry := node.NewRegistry()
	d := newFakeDaemon(t)
	base := func() workers.Config {
		return workers.Config{
			Pools: []node.Pool{node.PoolAI}, Coordinator: d.coordinator(t), Queue: newFakeQueue(),
			ScratchDir: t.TempDir(), CacheDir: t.TempDir(),
		}
	}
	cases := map[string]func(*workers.Config){
		"no pools":       func(c *workers.Config) { c.Pools = nil },
		"daemon pool":    func(c *workers.Config) { c.Pools = []node.Pool{node.PoolDaemon} },
		"unknown pool":   func(c *workers.Config) { c.Pools = []node.Pool{"gpu"} },
		"no coordinator": func(c *workers.Config) { c.Coordinator = nil },
		"no queue":       func(c *workers.Config) { c.Queue = nil },
		"no scratch":     func(c *workers.Config) { c.ScratchDir = "" },
		"no cache":       func(c *workers.Config) { c.CacheDir = "" },
	}
	for name, mod := range cases {
		cfg := base()
		mod(&cfg)
		if err := workers.Run(ctx, cfg, registry); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMediaPoolNeedsFFmpegAndAIPoolDoesNot(t *testing.T) {
	r := newRig(t)
	r.cfg.FFmpeg = ffmpeg.New(filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "absent"))
	cfg := r.cfg
	cfg.Coordinator = r.daemon.coordinator(t)
	cfg.Queue = r.queue
	err := workers.Run(context.Background(), cfg, r.registry)
	if err == nil || !strings.Contains(err.Error(), "FFmpeg") {
		t.Fatalf("%v", err)
	}
	var n int
	r.daemon.with(func() { n = len(r.daemon.registers) })
	if n != 0 {
		t.Fatal("registered before checking FFmpeg")
	}

	ai := newRig(t)
	ai.cfg.Pools = []node.Pool{node.PoolAI}
	ai.cfg.FFmpeg = ffmpeg.New(filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "absent"))
	ai.start()
	eventually(t, "registration without FFmpeg", func() bool {
		var n int
		ai.daemon.with(func() { n = len(ai.daemon.registers) })
		return n == 1
	})
}

func TestCleanupPointerRemovesOnlyThatJob(t *testing.T) {
	r := newRig(t)
	finished := r.job
	other := newJob()
	for _, job := range []id.ULID{finished, other} {
		dir := filepath.Join(r.cfg.ScratchDir, "acme", string(job), "render")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "clip.mp4"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := jobqueue.ScratchCleanupPointer("acme", finished, time.Now())
	if err := r.queue.Enqueue(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	r.start()
	eventually(t, "the ack", func() bool { return r.queue.isAcked(p.TaskID) })
	if _, err := os.Stat(filepath.Join(r.cfg.ScratchDir, "acme", string(finished))); !os.IsNotExist(err) {
		t.Fatalf("the finished job's scratch survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.ScratchDir, "acme", string(other))); err != nil {
		t.Fatalf("another job's scratch was removed: %v", err)
	}
	var leases int
	r.daemon.with(func() { leases = len(r.daemon.leases) })
	if leases != 0 {
		t.Fatal("a cleanup pointer took a lease")
	}
}

func TestCleanupPointerCannotEscape(t *testing.T) {
	r := newRig(t)
	outside := filepath.Join(filepath.Dir(r.cfg.ScratchDir), "precious")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	job := newJob()
	bad := jobqueue.Pointer{
		TaskID:   jobqueue.TaskID(job, jobqueue.ScratchCleanupNodeID, 1),
		TenantID: "../..",
		Pool:     node.PoolMedia,
		NodeType: jobqueue.NodeTypeScratchCleanup,
	}
	if err := r.queue.Enqueue(context.Background(), bad); err != nil {
		t.Fatal(err)
	}
	r.start()
	eventually(t, "the ack", func() bool { return r.queue.isAcked(bad.TaskID) })
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a crafted pointer deleted outside scratch: %v", err)
	}
}

func TestIdleScratchIsReapedAndRunningJobsAreNot(t *testing.T) {
	r := newRig(t)
	r.cfg.ScratchMaxIdle = time.Hour
	r.cfg.Housekeeping = 20 * time.Millisecond
	idle := newJob()
	fresh := newJob()
	old := time.Now().Add(-2 * time.Hour)
	for _, job := range []id.ULID{idle, fresh, r.job} {
		dir := filepath.Join(r.cfg.ScratchDir, "acme", string(job), "node")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{
		filepath.Join(r.cfg.ScratchDir, "acme", string(idle), "node"),
		filepath.Join(r.cfg.ScratchDir, "acme", string(idle)),
	} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	release := make(chan struct{})
	r.registry.RegisterNode(script("test.long", false, func(context.Context, node.Env, node.Input) (node.Result, error) {
		<-release
		return node.JSONResult(map[string]any{})
	}))
	taskID, _ := r.submit("running", "test.long", nil)
	r.start()
	eventually(t, "the idle job's removal", func() bool {
		_, err := os.Stat(filepath.Join(r.cfg.ScratchDir, "acme", string(idle)))
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(filepath.Join(r.cfg.ScratchDir, "acme", string(fresh))); err != nil {
		t.Fatalf("a fresh job was reaped: %v", err)
	}
	close(release)
	r.daemon.waitDone(t, taskID)
}

// §15.6: a task reaches only its own tenant's and job's scratch.
func TestScratchIsolation(t *testing.T) {
	r := newRig(t)
	ancestor := newJob()
	stranger := newJob()
	write := func(job id.ULID) {
		dir := filepath.Join(r.cfg.ScratchDir, "acme", string(job), "render")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "clip.mp4"), []byte("clip"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(ancestor)
	write(stranger)
	otherTenant := filepath.Join(r.cfg.ScratchDir, "globex", string(stranger), "render")
	if err := os.MkdirAll(otherTenant, 0o700); err != nil {
		t.Fatal(err)
	}
	results := map[string]error{}
	r.registry.RegisterNode(script("test.scratch", false, func(ctx context.Context, env node.Env, _ node.Input) (node.Result, error) {
		for name, job := range map[string]id.ULID{"ancestor": ancestor, "stranger": stranger} {
			_, err := env.OpenInput(ctx, "scratch://"+string(job)+"/render/clip.mp4")
			results[name] = err
		}
		p, err := env.Scratch().Path("own/file.bin")
		if err != nil {
			return node.Result{}, err
		}
		if err := os.WriteFile(p, []byte("own"), 0o600); err != nil {
			return node.Result{}, err
		}
		if _, err := env.OpenInput(ctx, env.Scratch().URI("own/file.bin")); err != nil {
			return node.Result{}, err
		}
		if _, err := env.Scratch().Path("../../escape"); err == nil {
			t.Error("a scratch path escaped")
		}
		return node.JSONResult(map[string]any{})
	}))
	taskID, _ := r.submit("n1", "test.scratch", nil, func(l *workerproto.Lease) { l.ScratchJobs = []id.ULID{ancestor} })
	r.start()
	r.daemon.waitDone(t, taskID)
	r.daemon.completion(t, taskID)
	if results["ancestor"] != nil {
		t.Errorf("an ancestor's file was refused: %v", results["ancestor"])
	}
	if !errs.IsCode(results["stranger"], errs.CodeForbidden) {
		t.Errorf("another job's file: %v", results["stranger"])
	}
}

// fakeDriver records what a storage factory receives.
type fakeDriver struct {
	connector.Storage
	secret []byte
}

func (fakeDriver) Info() connector.Info { return connector.Info{Driver: "fake"} }

func TestCredentialsReachTheDriverAndNoLog(t *testing.T) {
	r := newRig(t)
	const value = "credential-value-never-logged"
	const ref = "gcs-main"
	var received atomic.Pointer[[]byte]
	r.cfg.Connectors.RegisterStorage(connector.Info{Driver: "fake"}, func(ctx context.Context, p connector.StorageParams) (connector.Storage, error) {
		secret, err := p.Secrets.ResolveSecret(ctx, p.Instance.CredentialsRef)
		if err != nil {
			return nil, err
		}
		received.Store(&secret)
		if _, err := p.Secrets.ResolveSecret(ctx, "another-reference"); !errs.IsCode(err, errs.CodeForbidden) {
			t.Errorf("another reference: %v", err)
		}
		return fakeDriver{secret: secret}, nil
	})
	r.daemon.with(func() {
		r.daemon.roles[domain.RoleMaster] = config.StorageInstance{Driver: "fake", Bucket: "b", CredentialsRef: ref}
		r.daemon.creds = []byte(value)
	})
	r.registry.RegisterNode(script("test.creds", false, func(ctx context.Context, env node.Env, _ node.Input) (node.Result, error) {
		_, err := env.Storage(ctx, domain.RoleMaster)
		return node.Result{Output: json.RawMessage(`{}`)}, err
	}))
	taskID, _ := r.submit("n1", "test.creds", nil)
	r.start()
	r.daemon.waitDone(t, taskID)
	r.daemon.completion(t, taskID)
	got := received.Load()
	if got == nil || string(*got) != value {
		t.Fatalf("the driver got %v", got)
	}
	if strings.Contains(r.logs.String(), value) {
		t.Fatal("a credential reached the worker's log")
	}
	r.daemon.with(func() {
		for _, line := range r.daemon.logs[taskID] {
			if strings.Contains(line.Message, value) {
				t.Error("a credential reached the job's log")
			}
		}
		encoded, _ := json.Marshal(r.daemon.completed[taskID])
		if strings.Contains(string(encoded), value) {
			t.Error("a credential reached the node's result")
		}
	})
}

// Fakes for the AI pool.
type fakeTranscriber struct{ params connector.AIParams }

func (fakeTranscriber) Info() connector.Info { return connector.Info{Driver: "faster-whisper"} }
func (fakeTranscriber) Transcribe(context.Context, connector.TranscribeRequest) (domain.Transcript, error) {
	return domain.Transcript{}, nil
}

func TestAIPoolGetsTheAIEnvironment(t *testing.T) {
	r := newRig(t)
	r.cfg.Pools = []node.Pool{node.PoolAI}
	r.cfg.AIWorker = config.AIWorker{Endpoint: "unix:///run/vidyom/ai.sock"}
	var got connector.AIParams
	r.cfg.Connectors.RegisterTranscriber(connector.Info{Driver: "faster-whisper"}, func(_ context.Context, p connector.AIParams) (connector.Transcriber, error) {
		got = p
		return fakeTranscriber{p}, nil
	})
	r.registry.RegisterNode(testNode{
		desc: node.Descriptor{Type: "test.ai", TypeVersion: 1, Pool: node.PoolAI},
		run: func(ctx context.Context, env node.Env, _ node.Input) (node.Result, error) {
			ai, err := node.RequireAI(env)
			if err != nil {
				return node.Result{}, err
			}
			if _, err := ai.Transcriber(ctx); err != nil {
				return node.Result{}, err
			}
			if _, err := node.RequireDaemon(env); err == nil {
				t.Error("a worker environment passed as the daemon's")
			}
			return node.JSONResult(map[string]any{})
		},
	})
	data, _ := json.Marshal(nil)
	taskID := jobqueue.TaskID(r.job, "n1", 1)
	r.daemon.addTask(workerproto.Lease{
		TaskID: taskID, LeaseID: "l", TenantID: "acme", JobID: "JOB-001", JobULID: r.job, NodeID: "n1",
		NodeType: "test.ai", TypeVersion: 1, Attempt: 1, Parameters: data,
		AI: &config.AI{TranscriptionModel: "large-v3"},
	})
	if err := r.queue.Enqueue(context.Background(), jobqueue.Pointer{TaskID: taskID, TenantID: "acme", Pool: node.PoolAI, NodeType: "test.ai"}); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.daemon.waitDone(t, taskID)
	r.daemon.completion(t, taskID)
	if got.Tenant != "acme" || got.Daemon.Endpoint != "unix:///run/vidyom/ai.sock" || got.AI.TranscriptionModel != "large-v3" {
		t.Fatalf("%+v", got)
	}
}

func TestMediaPoolIsNotTheAIEnvironment(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(script("test.env", false, func(_ context.Context, env node.Env, _ node.Input) (node.Result, error) {
		if _, err := node.RequireAI(env); err == nil {
			t.Error("a media task got the AI environment")
		}
		if _, err := node.RequireDaemon(env); err == nil {
			t.Error("a media task got the daemon's environment")
		}
		if !env.HasPermission(domain.PermStorageArchiveRestore) {
			t.Error("the lease's permission is missing")
		}
		if env.HasPermission(domain.PermAssetDelete) {
			t.Error("a permission appeared from nowhere")
		}
		return node.JSONResult(map[string]any{})
	}))
	taskID, _ := r.submit("n1", "test.env", nil, func(l *workerproto.Lease) {
		l.Permissions = []domain.Permission{domain.PermStorageArchiveRestore}
	})
	r.start()
	r.daemon.waitDone(t, taskID)
	r.daemon.completion(t, taskID)
}

func TestQueueErrorsDoNotStopTheWorker(t *testing.T) {
	r := newRig(t)
	r.registry.RegisterNode(script("test.ok", false, ok))
	r.queue.readErr = errors.New("redis down")
	r.start()
	time.Sleep(60 * time.Millisecond)
	r.queue.mu.Lock()
	r.queue.readErr = nil
	r.queue.mu.Unlock()
	taskID, _ := r.submit("n1", "test.ok", nil)
	r.daemon.waitDone(t, taskID)
	r.daemon.completion(t, taskID)
}
