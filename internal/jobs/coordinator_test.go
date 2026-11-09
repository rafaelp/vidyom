package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
)

type fixedSecrets map[string]string

func (f fixedSecrets) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	v, ok := f[ref]
	if !ok {
		return nil, errs.NotFound("secret")
	}
	return []byte(v), nil
}

type fixedKeys struct{}

func (fixedKeys) CacheKey(_ context.Context, tenant domain.TenantID) ([]byte, error) {
	return []byte("cache-key-of-" + tenant), nil
}
func (fixedKeys) PublicIDKey(context.Context, domain.TenantID) ([]byte, error) { return nil, nil }

func TestLeaseOutcomes(t *testing.T) {
	h := newHarness(t)
	h.stopWorker()
	h.add(`{"id":"gpu","version":"1.0.0","nodes":[{"id":"g","type":"test.gpu","typeVersion":1}]}`)
	ctx := context.Background()
	coord := h.engine.Coordinator()
	register := func(worker string, gpu bool, pools ...node.Pool) {
		t.Helper()
		if _, err := coord.Register(ctx, workerproto.RegisterRequest{WorkerID: worker, Pools: pools, Resources: domain.WorkerResources{CPUCores: 4, GPU: gpu}}); err != nil {
			t.Fatal(err)
		}
	}
	register("cpu-only", false, node.PoolMedia)
	register("gpu", true, node.PoolMedia)
	register("ai-only", true, node.PoolAI)

	j := h.run("gpu", nil)
	h.nodeStatus(j, "g", domain.NodeQueued)
	n := h.nodes(j)["g"]

	// D40: incompatible workers get 409, and the pointer stays for another.
	for _, w := range []string{"cpu-only", "ai-only", "ghost"} {
		if _, err := coord.Lease(ctx, n.TaskID, workerproto.LeaseRequest{WorkerID: w}); !errs.IsCode(err, errs.CodeConflict) {
			t.Fatalf("%s: %v", w, err)
		}
	}
	lease, err := coord.Lease(ctx, n.TaskID, workerproto.LeaseRequest{WorkerID: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.TenantID != "acme" || lease.JobID != j.ID || lease.NodeType != "test.gpu" || lease.Attempt != 1 || lease.IdempotencyKey == "" {
		t.Fatalf("lease %+v", lease)
	}
	// One live lease, even for the same worker: a duplicate entry must not run twice.
	if _, err := coord.Lease(ctx, n.TaskID, workerproto.LeaseRequest{WorkerID: "gpu"}); !errs.IsCode(err, errs.CodeConflict) {
		t.Fatalf("same worker: %v", err)
	}
	register("gpu2", true, node.PoolMedia)
	if _, err := coord.Lease(ctx, n.TaskID, workerproto.LeaseRequest{WorkerID: "gpu2"}); !errs.IsCode(err, errs.CodeConflict) {
		t.Fatalf("second worker: %v", err)
	}
	// Wrong lease is 410 everywhere.
	if _, err := coord.TaskHeartbeat(ctx, n.TaskID, workerproto.TaskHeartbeatRequest{LeaseID: "nope"}); !errs.IsCode(err, errs.CodeGone) {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := coord.Complete(ctx, n.TaskID, workerproto.CompleteRequest{LeaseID: "nope"}); !errs.IsCode(err, errs.CodeGone) {
		t.Fatalf("complete: %v", err)
	}
	if err := coord.Fail(ctx, n.TaskID, workerproto.FailRequest{LeaseID: "nope"}); !errs.IsCode(err, errs.CodeGone) {
		t.Fatalf("fail: %v", err)
	}
	// A heartbeat renews and reports progress.
	before := h.nodes(j)["g"].LeaseExpiresAt
	time.Sleep(5 * time.Millisecond)
	beat, err := coord.TaskHeartbeat(ctx, n.TaskID, workerproto.TaskHeartbeatRequest{LeaseID: lease.LeaseID, Percent: 40, Message: "encoding"})
	if err != nil || beat.Cancel || !beat.ExpiresAt.After(*before) {
		t.Fatalf("beat %+v %v", beat, err)
	}
	if got := h.nodes(j)["g"].ProgressPercent; got != 40 {
		t.Fatalf("percent %v", got)
	}
	// Logs reach the job's stream.
	if err := coord.TaskLogs(ctx, n.TaskID, workerproto.TaskLogsRequest{LeaseID: lease.LeaseID, Lines: []workerproto.LogLine{{Level: "info", Message: "frame 10"}}}); err != nil {
		t.Fatal(err)
	}
	// Complete is idempotent on task and lease (I4).
	output := json.RawMessage(`{"done":true}`)
	usage := []domain.UsageRecord{{Operation: "storage.egress", Quantity: 1}}
	for i := 0; i < 3; i++ {
		if err := coord.Complete(ctx, n.TaskID, workerproto.CompleteRequest{LeaseID: lease.LeaseID, Output: output, Usage: usage}); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	h.waitStatus(j, domain.JobCompleted)
	h.budgets.mu.Lock()
	recorded := len(h.budgets.recorded)
	h.budgets.mu.Unlock()
	if recorded != 1 {
		t.Fatalf("usage recorded %d times", recorded)
	}
	// Everything after the end is 410.
	if _, err := coord.Lease(ctx, n.TaskID, workerproto.LeaseRequest{WorkerID: "gpu"}); !errs.IsCode(err, errs.CodeGone) {
		t.Fatalf("after the end: %v", err)
	}
	if _, err := coord.Lease(ctx, jobqueue.TaskID(id.New(), "x", 1), workerproto.LeaseRequest{WorkerID: "gpu"}); !errs.IsCode(err, errs.CodeGone) {
		t.Fatalf("unknown job: %v", err)
	}
	if _, err := coord.Lease(ctx, "garbage", workerproto.LeaseRequest{WorkerID: "gpu"}); !errs.IsCode(err, errs.CodeGone) {
		t.Fatalf("garbage: %v", err)
	}
	if err := coord.Heartbeat(ctx, workerproto.HeartbeatRequest{WorkerID: "gpu", Running: 1, DiskMB: 500}); err != nil {
		t.Fatal(err)
	}
	if err := coord.Heartbeat(ctx, workerproto.HeartbeatRequest{WorkerID: "ghost"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("ghost: %v", err)
	}
}

func TestOversizeOutputFailsTheTask(t *testing.T) {
	h := newHarness(t)
	h.stopWorker()
	h.add(`{"id":"big","version":"1.0.0","nodes":[{"id":"p","type":"video.probe","typeVersion":1,"parameters":{"source":"storage://master/a"}}]}`)
	ctx := context.Background()
	coord := h.engine.Coordinator()
	if _, err := coord.Register(ctx, workerproto.RegisterRequest{WorkerID: "w", Pools: []node.Pool{node.PoolMedia}}); err != nil {
		t.Fatal(err)
	}
	j := h.run("big", nil)
	h.nodeStatus(j, "p", domain.NodeQueued)
	lease, err := coord.Lease(ctx, h.nodes(j)["p"].TaskID, workerproto.LeaseRequest{WorkerID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, domain.MaxNodeOutputBytes+1)
	if err := coord.Complete(ctx, lease.TaskID, workerproto.CompleteRequest{LeaseID: lease.LeaseID, Output: big}); err != nil {
		t.Fatal(err)
	}
	failed := h.waitStatus(j, domain.JobFailed)
	if failed.Error.Code != string(errs.CodeOutputTooLarge) {
		t.Fatalf("error %+v", failed.Error)
	}
}

func TestConnectionAndCacheKeyNeedALiveLease(t *testing.T) {
	h := newHarness(t, harnessOpts{noEngine: true})
	h.stopWorker()
	h.deps.SecretsFor = func(domain.TenantID) connectorSecrets { return fixedSecrets{"gcs-main": "s3cret"} }
	h.deps.Keys = fixedKeys{}
	h.tenantCfg = config.Tenant{
		Storages: config.Storages{Roles: map[domain.StorageRole]config.StorageInstance{
			domain.RoleMaster: {Driver: "local", Root: "/m", CredentialsRef: "gcs-main"},
			domain.RoleProxy:  {Driver: "local", Root: "/p"},
		}},
		AI: config.AI{EmbeddingModel: "bge-m3"},
	}
	h.startEngine()
	h.add(twoStepDoc)
	h.add(`{"id":"ai","version":"1.0.0","nodes":[{"id":"x","type":"ai.embed","typeVersion":1,"parameters":{"transcript":{"language":"en"}}}]}`)
	ctx := context.Background()
	coord := h.engine.Coordinator()
	if _, err := coord.Register(ctx, workerproto.RegisterRequest{WorkerID: "w", Pools: []node.Pool{node.PoolMedia, node.PoolAI},
		Resources: domain.WorkerResources{CPUCores: 32, MemoryMB: 1 << 20, DiskMB: 1 << 30, GPU: true}}); err != nil {
		t.Fatal(err)
	}
	j := h.run("steps", nil)
	h.nodeStatus(j, "probe", domain.NodeQueued)
	task := h.nodes(j)["probe"].TaskID
	if _, err := coord.Connection(ctx, task, workerproto.ConnectionRequest{LeaseID: "x", Role: domain.RoleMaster}); !errs.IsCode(err, errs.CodeGone) {
		t.Fatalf("no lease: %v", err)
	}
	if _, err := coord.CacheKey(ctx, task, workerproto.CacheKeyRequest{LeaseID: "x"}); !errs.IsCode(err, errs.CodeGone) {
		t.Fatalf("no lease: %v", err)
	}
	lease, err := coord.Lease(ctx, task, workerproto.LeaseRequest{WorkerID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := coord.Connection(ctx, task, workerproto.ConnectionRequest{LeaseID: lease.LeaseID, Role: domain.RoleMaster})
	if err != nil || string(conn.Credentials) != "s3cret" || conn.Instance.Driver != "local" {
		t.Fatalf("connection %+v %v", conn, err)
	}
	if conn, err := coord.Connection(ctx, task, workerproto.ConnectionRequest{LeaseID: lease.LeaseID, Role: domain.RoleProxy}); err != nil || conn.Credentials != nil {
		t.Fatalf("proxy %+v %v", conn, err)
	}
	if _, err := coord.Connection(ctx, task, workerproto.ConnectionRequest{LeaseID: lease.LeaseID, Role: domain.RoleArchive}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("archive: %v", err)
	}
	key, err := coord.CacheKey(ctx, task, workerproto.CacheKeyRequest{LeaseID: lease.LeaseID})
	if err != nil || string(key.Key) != "cache-key-of-acme" {
		t.Fatalf("key %s %v", key.Key, err)
	}
	// Secrets never reach the job's records.
	for _, ev := range mustEvents(t, h, j) {
		if strings.Contains(string(ev.Data)+ev.Message, "s3cret") {
			t.Fatalf("secret in an event: %+v", ev)
		}
	}
	// Pool ai tasks get the tenant's model names (I197).
	aiJob := h.run("ai", nil)
	h.nodeStatus(aiJob, "x", domain.NodeQueued)
	aiLease, err := coord.Lease(ctx, h.nodes(aiJob)["x"].TaskID, workerproto.LeaseRequest{WorkerID: "w"})
	if err != nil || aiLease.AI == nil || aiLease.AI.EmbeddingModel != "bge-m3" {
		t.Fatalf("ai lease %+v %v", aiLease, err)
	}
	if lease.AI != nil {
		t.Fatalf("a media task needs no AI settings: %+v", lease.AI)
	}
}

func mustEvents(t *testing.T, h *harness, j domain.Job) []domain.JobEvent {
	t.Helper()
	out, err := h.store.Jobs().ListEvents(context.Background(), id.Ref(j.ID), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChildLeaseListsScratchAncestors(t *testing.T) {
	h := newHarness(t)
	h.stopWorker()
	h.add(`{"id":"leaf","version":"1.0.0","nodes":[{"id":"p","type":"video.probe","typeVersion":1,"parameters":{"source":"storage://master/a"}}]}`)
	h.add(`{"id":"mid","version":"1.0.0","nodes":[{"id":"call","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"leaf"}}]}`)
	h.add(`{"id":"top","version":"1.0.0","nodes":[{"id":"call","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"mid"}}]}`)
	ctx := context.Background()
	coord := h.engine.Coordinator()
	if _, err := coord.Register(ctx, workerproto.RegisterRequest{WorkerID: "w", Pools: []node.Pool{node.PoolMedia}}); err != nil {
		t.Fatal(err)
	}
	top := h.runAs(editor, "top", nil)
	mid := childOf(t, h, top, "call")
	leaf := childOf(t, h, mid, "call")
	h.nodeStatus(leaf, "p", domain.NodeQueued)
	lease, err := coord.Lease(ctx, h.nodes(leaf)["p"].TaskID, workerproto.LeaseRequest{WorkerID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	if len(lease.ScratchJobs) != 2 || lease.ScratchJobs[0] != mid.ULID || lease.ScratchJobs[1] != top.ULID {
		t.Fatalf("scratch jobs %v", lease.ScratchJobs)
	}
	if len(lease.Permissions) != 0 {
		t.Fatalf("permissions %v", lease.Permissions)
	}
}

func TestScratchCleanupPointer(t *testing.T) {
	h := newHarness(t)
	h.stopWorker()
	h.add(`{"id":"quick","version":"1.0.0","nodes":[{"id":"n","type":"test.pure","typeVersion":1,"parameters":{}}]}`)
	h.add(`{"id":"bad","version":"1.0.0","nodes":[{"id":"b","type":"test.daemon","typeVersion":1,"parameters":{}}]}`)
	h.on("test.daemon", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition, "no"))
	})
	ctx := context.Background()
	j := h.waitStatus(h.run("quick", nil), domain.JobCompleted)
	failed := h.waitStatus(h.run("bad", nil), domain.JobFailed)
	got, err := h.q.Read(ctx, jobqueue.ReadOptions{Pool: node.PoolMedia, Consumer: "cleaner", Count: 10})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[id.ULID]bool{}
	for _, d := range got {
		if d.Pointer.NodeType == jobqueue.NodeTypeScratchCleanup {
			u, _, _, _ := jobqueue.ParseTaskID(d.Pointer.TaskID)
			seen[u] = true
		}
	}
	if !seen[j.ULID] {
		t.Fatalf("a completed job's scratch must be cleaned: %+v", got)
	}
	if seen[failed.ULID] {
		t.Fatal("a failed job keeps its scratch, because job retry needs it (I153)")
	}
}

func TestSuspensionFromDaemonPoolNode(t *testing.T) {
	h := newHarness(t)
	resume := time.Now().Add(time.Second).Unix()
	h.on("test.suspends", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{Suspend: &node.Suspension{Reason: "external", ResumeAfter: resume}}, nil
	})
	h.add(`{"id":"sus","version":"1.0.0","nodes":[{"id":"s","type":"test.suspends","typeVersion":1,"parameters":{}}]}`)
	j := h.run("sus", nil)
	h.nodeStatus(j, "s", domain.NodeWaiting)
	// A node that suspends but cannot resume is a defect, reported as a failure.
	failed := h.waitStatus(j, domain.JobFailed)
	if !strings.Contains(failed.Error.Message, "cannot resume") {
		t.Fatalf("error %+v", failed.Error)
	}
}
