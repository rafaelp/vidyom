package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

const childV1 = `{"id":"child","version":"1.0.0","inputs":{"x":{"type":"string","required":true},"y":{"type":"string"}},
	"nodes":[{"id":"work","type":"test.pure","typeVersion":1,"parameters":{"got":"${inputs.x}"}}],
	"outputs":{"echo":"${inputs.x}","result":"${nodes.work.output.value}"}}`

const parentDoc = `{"id":"parent","version":"1.0.0","inputs":{"a":{"type":"string","required":true},"secretish":{"type":"string"}},"nodes":[
	{"id":"before","type":"test.daemon","typeVersion":1,"parameters":{}},
	{"id":"call","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"child","x":"${inputs.a}"}},
	{"id":"after","type":"test.daemon","typeVersion":1,"parameters":{"from_child":"${nodes.call.output.echo}"}}
],"connections":{"before":["call"],"call":["after"]},"outputs":{"child_result":"${nodes.call.output.result}"}}`

func childOf(t *testing.T, h *harness, parent domain.Job, nodeID string) domain.Job {
	t.Helper()
	var out domain.Job
	h.waitFor("a child job of "+nodeID, func() bool {
		page, _, err := h.store.Jobs().List(context.Background(), repo.JobFilter{ParentJob: id.Ref(parent.ULID)}, repo.Page{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page {
			if c.Trigger.ParentNode == nodeID {
				out = c
				return true
			}
		}
		return false
	})
	return out
}

func TestSubworkflowIsolation(t *testing.T) {
	h := newHarness(t)
	h.output("test.pure", map[string]any{"value": "computed"})
	h.add(childV1)
	h.add(parentDoc)
	parent := h.waitStatus(h.run("parent", map[string]any{"a": "hello", "secretish": "not for the child"}), domain.JobCompleted)
	child := childOf(t, h, parent, "call")
	if child.Status != domain.JobCompleted || child.Trigger.Type != "subworkflow" || child.Trigger.ParentJobID != string(parent.ULID) {
		t.Fatalf("child %+v", child)
	}
	if child.WorkflowID != "child" || child.TraceID != parent.TraceID || child.SubmittedBy != parent.SubmittedBy {
		t.Fatalf("child %+v", child)
	}
	// Input isolation: only what the node passes.
	if strings.Contains(string(child.Inputs), "not for the child") {
		t.Fatalf("inputs leaked: %s", child.Inputs)
	}
	// Output isolation: the parent sees the child's outputs, not its nodes.
	var params map[string]any
	if err := json.Unmarshal(h.callsOf("after")[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	if params["from_child"] != "hello" {
		t.Fatalf("params %v", params)
	}
	call := outputMap(t, h.nodes(parent)["call"])
	if call["echo"] != "hello" || call["result"] != "computed" || len(call) != 2 {
		t.Fatalf("call output %v", call)
	}
	var outs map[string]any
	_ = json.Unmarshal(parent.Outputs, &outs)
	if outs["child_result"] != "computed" {
		t.Fatalf("outputs %v", outs)
	}
	// A child gets its own idempotency keys (I36).
	if h.callsOf("work")[0].Key != node.IdempotencyKey("acme", child.ULID, "work") {
		t.Fatalf("key %s", h.callsOf("work")[0].Key)
	}
}

func TestSnapshotProtectsAJobInProgress(t *testing.T) {
	h := newHarness(t)
	h.output("test.pure", map[string]any{"value": "old"})
	h.add(childV1)
	h.add(`{"id":"parent","version":"1.0.0","inputs":{"asset":{"type":"string"}},"nodes":[
		{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"metadata_change","assetId":"${inputs.asset}"}},
		{"id":"call","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"child","x":"hello"}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"ask":["call"],"call":["tail"]}}`)
	asset := h.newAsset(domain.KindClip)
	j := h.run("parent", map[string]any{"asset": asset.ID})
	a := h.pendingApproval(j)
	stored := h.job(j.ID)
	if stored.WorkflowSHA256 != wfdef.SHA256(stored.Snapshot) {
		t.Fatal("the hash must cover the exact snapshot bytes")
	}
	if !strings.Contains(string(stored.Snapshot), `"subworkflows"`) {
		t.Fatalf("snapshot %s", stored.Snapshot)
	}

	// Both the workflow and its child get new versions while the job waits.
	h.add(`{"id":"child","version":"2.0.0","inputs":{"x":{"type":"string","required":true}},"nodes":[{"id":"changed","type":"test.pure","typeVersion":1,"parameters":{}}]}`)
	if err := h.wfs.Add([]byte(`{"id":"parent","version":"2.0.0","nodes":[{"id":"other","type":"test.pure","typeVersion":1,"parameters":{}}]}`), "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Approvals().Approve(context.Background(), scopeOf(publisher), id.Ref(a.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	done := h.waitStatus(j, domain.JobCompleted)
	child := childOf(t, h, done, "call")
	if child.WorkflowVersion != "1.0.0" {
		t.Fatalf("the child ran %s; the job's snapshot says 1.0.0", child.WorkflowVersion)
	}
	if len(h.callsOf("work")) != 1 || len(h.callsOf("changed")) != 0 {
		t.Fatalf("work %d changed %d", len(h.callsOf("work")), len(h.callsOf("changed")))
	}
	// New runs use the new version.
	again := h.waitStatus(h.run("child", map[string]any{"x": "1"}), domain.JobCompleted)
	if again.WorkflowVersion != "2.0.0" {
		t.Fatalf("version %s", again.WorkflowVersion)
	}
}

func TestChildFailureAndRetryResumesTheChild(t *testing.T) {
	h := newHarness(t)
	var fail atomic.Bool
	fail.Store(true)
	h.on("test.pure", func(context.Context, node.Env, node.Input) (node.Result, error) {
		if fail.Load() {
			return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition, "not yet"))
		}
		return node.JSONResult(map[string]any{"value": "ok"})
	})
	h.add(`{"id":"child","version":"1.0.0","inputs":{"x":{"type":"string","required":true}},"nodes":[
		{"id":"first","type":"test.daemon","typeVersion":1,"parameters":{}},
		{"id":"work","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"first":["work"]},"outputs":{"result":"${nodes.work.output.value}"}}`)
	h.add(`{"id":"parent","version":"1.0.0","nodes":[{"id":"call","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"child","x":"a"}}]}`)
	j := h.waitStatus(h.run("parent", nil), domain.JobFailed)
	child := childOf(t, h, j, "call")
	if child.Status != domain.JobFailed {
		t.Fatalf("child %s", child.Status)
	}
	if j.Error.NodeID != "call" || !strings.Contains(j.Error.Message, string(child.ID)) {
		t.Fatalf("error %+v", j.Error)
	}
	fail.Store(false)
	if _, err := h.engine.Jobs().Retry(context.Background(), scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	done := h.waitStatus(j, domain.JobCompleted)
	if got := childOf(t, h, done, "call"); got.ULID != child.ULID {
		t.Fatalf("a new child replaced the failed one: %s, %s", got.ID, child.ID)
	}
	if len(h.callsOf("first")) != 1 {
		t.Fatalf("the child's checkpoint was not used: %d", len(h.callsOf("first")))
	}
}

func TestCancelAndPauseCascade(t *testing.T) {
	h := newHarness(t)
	h.on("test.slow", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		<-ctx.Done()
		return node.Result{}, ctx.Err()
	})
	h.add(`{"id":"child","version":"1.0.0","nodes":[{"id":"s","type":"test.slow","typeVersion":1,"parameters":{}}]}`)
	h.add(`{"id":"parent","version":"1.0.0","nodes":[{"id":"call","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"child"}}]}`)
	ctx := context.Background()
	j := h.run("parent", nil)
	child := childOf(t, h, j, "call")
	h.nodeStatus(child, "s", domain.NodeRunning)

	if _, err := h.engine.Jobs().Pause(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(child, domain.JobPaused)
	if _, err := h.engine.Jobs().Resume(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(child, domain.JobRunning)

	if _, err := h.engine.Jobs().Cancel(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCancelled)
	if got := h.job(child.ID); got.Status != domain.JobCancelled {
		t.Fatalf("child %s", got.Status)
	}
}

func TestChildWaitingApprovalShowsOnTheParent(t *testing.T) {
	h := newHarness(t)
	asset := h.newAsset(domain.KindClip)
	h.add(`{"id":"child","version":"1.0.0","inputs":{"asset":{"type":"string"}},"nodes":[
		{"id":"ask","type":"approval.request","typeVersion":1,"parameters":{"kind":"publication","assetId":"${inputs.asset}"}}]}`)
	h.add(`{"id":"parent","version":"1.0.0","inputs":{"asset":{"type":"string"}},"nodes":[
		{"id":"call","type":"workflow.execute","typeVersion":1,"parameters":{"workflow":"child","asset":"${inputs.asset}"}}]}`)
	j := h.run("parent", map[string]any{"asset": asset.ID})
	h.waitStatus(j, domain.JobWaitingApproval)
	child := childOf(t, h, j, "call")
	a := h.pendingApproval(child)
	if _, err := h.engine.Approvals().Approve(context.Background(), scopeOf(publisher), id.Ref(a.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)
}

func TestForeach(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	running, peak := 0, 0
	h.on("test.daemon", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		var p struct {
			Item string `json:"item"`
		}
		_ = in.Bind(&p)
		if p.Item == "bad" {
			return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition, "bad item"))
		}
		return node.JSONResult(map[string]any{"seen": p.Item})
	})
	h.add(`{"id":"one","version":"1.0.0","inputs":{"item":{"type":"string","required":true},"fixed":{"type":"string"}},"nodes":[
		{"id":"work","type":"test.daemon","typeVersion":1,"parameters":{"item":"${inputs.item}"}}],
		"outputs":{"seen":"${nodes.work.output.seen}","fixed":"${inputs.fixed}"}}`)
	h.add(`{"id":"many","version":"1.0.0","inputs":{"items":{"type":"array","required":true}},"nodes":[
		{"id":"each","type":"flow.foreach","typeVersion":1,"parameters":{"items":"${inputs.items}","workflow":"one","inputs":{"fixed":"F"},"concurrency":2,"maxItems":6}}
	],"outputs":{"results":"${nodes.each.output.results}"}}`)

	j := h.waitStatus(h.run("many", map[string]any{"items": []string{"a", "b", "c", "d", "e"}}), domain.JobCompleted)
	var outs struct {
		Results []struct {
			Index   int            `json:"index"`
			JobID   string         `json:"job_id"`
			Status  string         `json:"status"`
			Outputs map[string]any `json:"outputs"`
		} `json:"results"`
	}
	if err := json.Unmarshal(j.Outputs, &outs); err != nil {
		t.Fatal(err)
	}
	if len(outs.Results) != 5 {
		t.Fatalf("results %+v", outs.Results)
	}
	for i, r := range outs.Results {
		if r.Index != i || r.Status != "completed" || r.Outputs["fixed"] != "F" || r.Outputs["seen"] != string(rune('a'+i)) {
			t.Fatalf("result %d: %+v", i, r)
		}
	}
	if peak > 2 {
		t.Fatalf("concurrency limit passed: %d", peak)
	}
	// Each item is its own child job.
	page, _, _ := h.store.Jobs().List(context.Background(), repo.JobFilter{ParentJob: id.Ref(j.ULID)}, repo.Page{Limit: 100})
	if len(page) != 5 {
		t.Fatalf("children %d", len(page))
	}

	// Item limit.
	failed := h.waitStatus(h.run("many", map[string]any{"items": []string{"a", "b", "c", "d", "e", "f", "g"}}), domain.JobFailed)
	if !strings.Contains(failed.Error.Message, "limit") {
		t.Fatalf("error %+v", failed.Error)
	}

	// Fail fast: nothing new starts, and the running ones end.
	failed = h.waitStatus(h.run("many", map[string]any{"items": []string{"bad", "x", "y", "z"}}), domain.JobFailed)
	if !strings.Contains(failed.Error.Message, "item 0") {
		t.Fatalf("error %+v", failed.Error)
	}
	page, _, _ = h.store.Jobs().List(context.Background(), repo.JobFilter{ParentJob: id.Ref(failed.ULID)}, repo.Page{Limit: 100})
	if len(page) > 3 {
		t.Fatalf("fail fast started %d children", len(page))
	}

	// No items, no children.
	empty := h.waitStatus(h.run("many", map[string]any{"items": []string{}}), domain.JobCompleted)
	if !strings.Contains(string(empty.Outputs), `"results":[]`) {
		t.Fatalf("outputs %s", empty.Outputs)
	}
}
