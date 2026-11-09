package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

func runRequest(raw json.RawMessage) api.RunWorkflowRequest {
	return api.RunWorkflowRequest{Inputs: raw}
}

const flowTrue = `"type":"flow.if","typeVersion":1,"parameters":{"condition":"true"}`

func TestLinearJobPassesDataAndCheckpoints(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"linear","version":"1.0.0",
		"inputs":{"file":{"type":"file"},"title":{"type":"string","required":true},"note":{"type":"string"}},
		"nodes":[
			{"id":"probe","type":"video.probe","typeVersion":1,"parameters":{"source":"storage://master/a.mp4"}},
			{"id":"put","type":"storage.put","typeVersion":1,"parameters":{"storage":"master","source":"storage://proxy/${inputs.title}.${nodes.probe.output.ext}","key":"x/${job.id}.mp4","contentType":"${inputs.note}"}},
			{"id":"pure","type":"test.pure","typeVersion":1,"parameters":{"got":"${nodes.put.output}"}}
		],
		"connections":{"probe":["put"],"put":["pure"]},
		"outputs":{"key":"${nodes.put.output.key}","title":"${inputs.title}","none":"${inputs.note}"}}`)
	h.output("video.probe", map[string]any{"ext": "mov", "duration_seconds": 10})
	h.output("storage.put", map[string]any{"uri": "storage://master/k", "storage": "master", "key": "k", "sha256": "abc"})
	j := h.run("linear", map[string]any{"title": "class"})
	done := h.waitStatus(j, domain.JobCompleted)

	var outs map[string]any
	if err := json.Unmarshal(done.Outputs, &outs); err != nil {
		t.Fatal(err)
	}
	if outs["key"] != "k" || outs["title"] != "class" {
		t.Fatalf("outputs %v", outs)
	}
	if _, ok := outs["none"]; ok {
		t.Fatal("an absent output must be left out")
	}
	put := h.callsOf("put")
	if len(put) != 1 {
		t.Fatalf("put ran %d times", len(put))
	}
	var params map[string]any
	if err := json.Unmarshal(put[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	if params["source"] != "storage://proxy/class.mov" || params["key"] != "x/"+string(j.ID)+".mp4" {
		t.Fatalf("parameters %v", params)
	}
	if _, ok := params["contentType"]; ok {
		t.Fatal("an absent optional parameter must be removed")
	}
	if put[0].Key != node.IdempotencyKey("acme", j.ULID, "put") {
		t.Fatalf("idempotency key %s", put[0].Key)
	}
	nodes := h.nodes(done)
	for _, id := range []string{"probe", "put", "pure"} {
		if nodes[id].Status != domain.NodeCompleted || nodes[id].FinishedAt == nil {
			t.Fatalf("node %s: %+v", id, nodes[id])
		}
	}
	if done.FinishedAt == nil || done.ProgressPercent != 100 || done.Snapshot == nil || done.WorkflowSHA256 == "" {
		t.Fatalf("job %+v", done)
	}
	// workflow.completed committed with the terminal state.
	if n := countEvents(t, h.store, event.WorkflowCompleted); n != 1 {
		t.Fatalf("workflow.completed: %d", n)
	}
}

func countEvents(t *testing.T, store repo.TenantStore, typ event.Type) int {
	t.Helper()
	items, _, err := store.Outbox().ListEvents(context.Background(), repo.OutboxFilter{Type: typ}, repo.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return len(items)
}

func TestBranchingAndMerge(t *testing.T) {
	h := newHarness(t)
	doc := `{"id":"branches","version":"1.0.0","inputs":{"go":{"type":"boolean","required":true}},
		"nodes":[
			{"id":"check","type":"flow.if","typeVersion":1,"parameters":{"condition":"inputs.go"}},
			{"id":"yes","type":"test.pure","typeVersion":1,"parameters":{}},
			{"id":"yes_more","type":"test.pure","typeVersion":1,"parameters":{}},
			{"id":"otherwise","type":"flow.if","typeVersion":1,"parameters":{"condition":"!inputs.go"}},
			{"id":"no","type":"test.pure","typeVersion":1,"parameters":{}},
			{"id":"all","type":"flow.merge","typeVersion":1,"parameters":{"mode":"all"}},
			{"id":"after","type":"test.pure","typeVersion":1,"parameters":{"which":"${nodes.all.output.completed}"}},
			{"id":"any","type":"flow.merge","typeVersion":1,"parameters":{"mode":"any"}},
			{"id":"after_any","type":"test.pure","typeVersion":1,"parameters":{}}
		],
		"connections":{"check":["yes"],"yes":["yes_more","all","any"],"otherwise":["no"],"no":["all","any"],"all":["after"],"any":["after_any"]}}`
	h.add(doc)

	j := h.waitStatus(h.run("branches", map[string]any{"go": true}), domain.JobCompleted)
	nodes := h.nodes(j)
	for id, want := range map[string]domain.NodeStatus{
		"check": domain.NodeCompleted, "yes": domain.NodeCompleted, "yes_more": domain.NodeCompleted,
		"otherwise": domain.NodeCompleted, "no": domain.NodeSkipped,
		"all": domain.NodeCompleted, "after": domain.NodeCompleted, "any": domain.NodeCompleted, "after_any": domain.NodeCompleted,
	} {
		if nodes[id].Status != want {
			t.Errorf("%s is %s, want %s", id, nodes[id].Status, want)
		}
	}
	if got := outputMap(t, nodes["all"])["completed"]; len(got.([]any)) != 1 || got.([]any)[0] != "yes" {
		t.Fatalf("merge output %v", got)
	}

	// The other way round: the skip propagates through yes and yes_more, stops at merge.
	j = h.waitStatus(h.run("branches", map[string]any{"go": false}), domain.JobCompleted)
	nodes = h.nodes(j)
	for id, want := range map[string]domain.NodeStatus{
		"yes": domain.NodeSkipped, "yes_more": domain.NodeSkipped, "no": domain.NodeCompleted,
		"all": domain.NodeCompleted, "after": domain.NodeCompleted,
	} {
		if nodes[id].Status != want {
			t.Errorf("%s is %s, want %s", id, nodes[id].Status, want)
		}
	}
}

func TestMergeOfAllSkippedIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"none","version":"1.0.0","nodes":[
		{"id":"a","type":"flow.if","typeVersion":1,"parameters":{"condition":"false"}},
		{"id":"b","type":"flow.if","typeVersion":1,"parameters":{"condition":"false"}},
		{"id":"join","type":"flow.merge","typeVersion":1,"parameters":{"mode":"all"}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"a":["join"],"b":["join"],"join":["tail"]}}`)
	j := h.waitStatus(h.run("none", nil), domain.JobCompleted)
	if n := h.nodes(j); n["join"].Status != domain.NodeSkipped || n["tail"].Status != domain.NodeSkipped {
		t.Fatalf("nodes %+v", n)
	}
}

func TestMergeAnyContinuesOnFirstCompleted(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.on("test.daemon", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return node.Result{}, ctx.Err()
		}
		return node.JSONResult(map[string]any{})
	})
	h.add(`{"id":"race","version":"1.0.0","nodes":[
		{"id":"fast","type":"test.pure","typeVersion":1,"parameters":{}},
		{"id":"slow","type":"test.daemon","typeVersion":1,"parameters":{}},
		{"id":"first","type":"flow.merge","typeVersion":1,"parameters":{"mode":"any"}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"fast":["first"],"slow":["first"],"first":["tail"]}}`)
	j := h.run("race", nil)
	h.nodeStatus(j, "tail", domain.NodeCompleted)
	if st := h.nodes(j)["slow"].Status; st == domain.NodeCompleted {
		t.Fatal("slow must still be running")
	}
	close(release)
	h.waitStatus(j, domain.JobCompleted)
}

func TestContinueOnFailureLetsDownstreamRun(t *testing.T) {
	h := newHarness(t)
	h.on("mediacms.register", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, node.Permanent(errs.New(errs.CodeUnavailable, "catalog down"))
	})
	h.add(`{"id":"optional","version":"1.0.0","nodes":[
		{"id":"sync","type":"mediacms.register","typeVersion":1,"continueOnFailure":true,"parameters":{"assetId":"ORI-001"}},
		{"id":"after","type":"test.pure","typeVersion":1,"parameters":{"got":"${nodes.sync.output.external_ref}"}}
	],"connections":{"sync":["after"]}}`)
	j := h.waitStatus(h.run("optional", nil), domain.JobCompleted)
	if n := h.nodes(j); n["sync"].Status != domain.NodeFailed || n["after"].Status != domain.NodeCompleted {
		t.Fatalf("nodes %+v", n)
	}
}

// ---- retry by class (D11) ----

func TestIdempotentRetriesWithTheSameKey(t *testing.T) {
	h := newHarness(t)
	var calls atomic.Int32
	h.on("test.daemon", func(context.Context, node.Env, node.Input) (node.Result, error) {
		if calls.Add(1) < 3 {
			return node.Result{}, node.Transient(errs.New(errs.CodeUnavailable, "try again"))
		}
		return node.JSONResult(map[string]any{"done": true})
	})
	h.add(`{"id":"flaky","version":"1.0.0","nodes":[{"id":"n","type":"test.daemon","typeVersion":1,"parameters":{}}],"settings":{"retry":3,"retryBackoffSeconds":0}}`)
	j := h.waitStatus(h.run("flaky", nil), domain.JobCompleted)
	got := h.callsOf("n")
	if len(got) != 3 {
		t.Fatalf("attempts %d", len(got))
	}
	for i, c := range got {
		if c.Attempt != i+1 || c.Key != got[0].Key {
			t.Fatalf("call %d: %+v", i, c)
		}
	}
	if h.nodes(j)["n"].Attempt != 3 {
		t.Fatalf("attempt %d", h.nodes(j)["n"].Attempt)
	}
}

func TestRetryLimitAndPermanentFailure(t *testing.T) {
	h := newHarness(t)
	h.on("test.daemon", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, node.Transient(errs.New(errs.CodeUnavailable, "still down"))
	})
	h.add(`{"id":"limited","version":"1.0.0","nodes":[{"id":"n","type":"test.daemon","typeVersion":1,"retry":2,"parameters":{}}],"settings":{"retry":9,"retryBackoffSeconds":0}}`)
	j := h.waitStatus(h.run("limited", nil), domain.JobFailed)
	if len(h.callsOf("n")) != 3 {
		t.Fatalf("node retry overrides settings: %d", len(h.callsOf("n")))
	}
	if j.Error == nil || j.Error.Code != string(errs.CodeUnavailable) || j.Error.NodeID != "n" {
		t.Fatalf("error %+v", j.Error)
	}
	if countEvents(t, h.store, event.WorkflowFailed) != 1 {
		t.Fatal("workflow.failed must fire once")
	}

	h.on("test.pure", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, errors.New("unclassified counts as permanent")
	})
	h.add(`{"id":"permanent","version":"1.0.0","nodes":[{"id":"p","type":"test.pure","typeVersion":1,"parameters":{}}]}`)
	j = h.waitStatus(h.run("permanent", nil), domain.JobFailed)
	if len(h.callsOf("p")) != 1 {
		t.Fatalf("permanent must not retry: %d", len(h.callsOf("p")))
	}
	if strings.Contains(j.Error.Message, "unclassified") {
		t.Fatalf("an internal error must not leak: %+v", j.Error)
	}
}

func TestJobRetryResumesFromCheckpoints(t *testing.T) {
	h := newHarness(t)
	var fail atomic.Bool
	fail.Store(true)
	h.on("test.daemon", func(context.Context, node.Env, node.Input) (node.Result, error) {
		if fail.Load() {
			return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition, "not yet"))
		}
		return node.JSONResult(map[string]any{})
	})
	h.add(`{"id":"resume","version":"1.0.0","nodes":[
		{"id":"first","type":"test.pure","typeVersion":1,"parameters":{}},
		{"id":"second","type":"test.daemon","typeVersion":1,"parameters":{}},
		{"id":"third","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"first":["second"],"second":["third"]}}`)
	j := h.waitStatus(h.run("resume", nil), domain.JobFailed)
	ctx := context.Background()
	if _, err := h.engine.Jobs().Retry(ctx, scopeOf(viewer), id.Ref("JOB-999"), api.JobActionRequest{}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown job: %v", err)
	}
	fail.Store(false)
	if _, err := h.engine.Jobs().Retry(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	done := h.waitStatus(j, domain.JobCompleted)
	if len(h.callsOf("first")) != 1 {
		t.Fatalf("the checkpointed node ran again: %d", len(h.callsOf("first")))
	}
	if len(h.callsOf("second")) != 2 || len(h.callsOf("third")) != 1 {
		t.Fatalf("second %d third %d", len(h.callsOf("second")), len(h.callsOf("third")))
	}
	if done.Error != nil || done.FinishedAt == nil {
		t.Fatalf("job %+v", done)
	}
	if _, err := h.engine.Jobs().Retry(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("a completed job cannot be retried: %v", err)
	}
}

// ---- unsafe and reconcilable (D11, §15.5) ----

func TestUnsafeGoesToNeedsAttentionAndResolve(t *testing.T) {
	h := newHarness(t)
	var fail atomic.Bool
	fail.Store(true)
	h.on("test.unsafe", func(context.Context, node.Env, node.Input) (node.Result, error) {
		if fail.Load() {
			return node.Result{}, node.Transient(errs.New(errs.CodeUnavailable, "lost mid-way"))
		}
		return node.JSONResult(map[string]any{"ok": true})
	})
	h.add(`{"id":"risky","version":"1.0.0","nodes":[
		{"id":"effect","type":"test.unsafe","typeVersion":1,"parameters":{}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"effect":["tail"]},"settings":{"retry":5}}`)
	ctx := context.Background()

	// Never retried automatically; a person decides.
	j := h.waitStatus(h.run("risky", nil), domain.JobNeedsAttention)
	if len(h.callsOf("effect")) != 1 {
		t.Fatalf("an unsafe node was retried: %d", len(h.callsOf("effect")))
	}
	if j.Attention == nil || j.Attention.NodeID != "effect" || j.Attention.NodeType != "test.unsafe" {
		t.Fatalf("attention %+v", j.Attention)
	}
	if countEvents(t, h.store, event.JobNeedsAttention) != 1 {
		t.Fatal("job.needs_attention must fire")
	}
	if j.Status.Terminal() {
		t.Fatal("needs_attention is not terminal")
	}

	// It is the only way out.
	if _, err := h.engine.Jobs().Cancel(ctx, scopeOf(admin), id.Ref(j.ID), api.JobActionRequest{}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := h.engine.Jobs().Pause(ctx, scopeOf(admin), id.Ref(j.ID), api.JobActionRequest{}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("pause: %v", err)
	}
	// Permissions and arguments.
	if _, err := h.engine.Jobs().Resolve(ctx, scopeOf(viewer), id.Ref(j.ID), api.ResolveJobRequest{Action: domain.ResolveRetryNode, NodeID: "effect"}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("a viewer cannot resolve: %v", err)
	}
	if _, err := h.engine.Jobs().Resolve(ctx, scopeOf(editor), id.Ref(j.ID), api.ResolveJobRequest{Action: domain.ResolveRetryNode, NodeID: "tail"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("wrong node: %v", err)
	}
	if _, err := h.engine.Jobs().Resolve(ctx, scopeOf(editor), id.Ref(j.ID), api.ResolveJobRequest{Action: "nope", NodeID: "effect"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("wrong action: %v", err)
	}

	// retry_node runs it again with the same key.
	fail.Store(false)
	if _, err := h.engine.Jobs().Resolve(ctx, scopeOf(editor), id.Ref(j.ID), api.ResolveJobRequest{Action: domain.ResolveRetryNode, NodeID: "effect", Note: "checked the remote state"}); err != nil {
		t.Fatal(err)
	}
	done := h.waitStatus(j, domain.JobCompleted)
	calls := h.callsOf("effect")
	if len(calls) != 2 || calls[0].Key != calls[1].Key {
		t.Fatalf("calls %+v", calls)
	}
	if done.Attention != nil {
		t.Fatalf("attention stayed: %+v", done.Attention)
	}
	audit, _, _ := h.store.Audit().List(ctx, repo.AuditFilter{Action: "job.resolve"}, repo.Page{})
	if len(audit) != 1 || audit[0].Actor != editor.Email || audit[0].Permission != domain.PermJobResolve {
		t.Fatalf("audit %+v", audit)
	}
	if _, err := h.engine.Jobs().Resolve(ctx, scopeOf(editor), id.Ref(j.ID), api.ResolveJobRequest{Action: domain.ResolveFail}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("resolve on a finished job: %v", err)
	}
}

func TestResolveSkipAndFail(t *testing.T) {
	h := newHarness(t)
	h.on("test.unsafe", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, node.Permanent(errs.New(errs.CodeInternal, "boom"))
	})
	h.add(`{"id":"risky","version":"1.0.0","nodes":[
		{"id":"effect","type":"test.unsafe","typeVersion":1,"parameters":{}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"effect":["tail"]}}`)
	ctx := context.Background()
	j := h.waitStatus(h.run("risky", nil), domain.JobNeedsAttention)
	if _, err := h.engine.Jobs().Resolve(ctx, scopeOf(editor), id.Ref(j.ID), api.ResolveJobRequest{Action: domain.ResolveSkipNode, NodeID: "effect"}); err != nil {
		t.Fatal(err)
	}
	done := h.waitStatus(j, domain.JobCompleted)
	if n := h.nodes(done); n["effect"].Status != domain.NodeSkipped || n["tail"].Status != domain.NodeSkipped {
		t.Fatalf("skip must propagate (D33): %+v", n)
	}

	j = h.waitStatus(h.run("risky", nil), domain.JobNeedsAttention)
	if _, err := h.engine.Jobs().Resolve(ctx, scopeOf(editor), id.Ref(j.ID), api.ResolveJobRequest{Action: domain.ResolveFail, Note: "the destination refused it"}); err != nil {
		t.Fatal(err)
	}
	failed := h.waitStatus(j, domain.JobFailed)
	if failed.Error == nil || failed.Error.Code != "resolved_as_failed" || failed.Attention != nil {
		t.Fatalf("job %+v", failed)
	}
}

func TestResolvePublicationNodeNeedsPublisher(t *testing.T) {
	h := newHarness(t)
	h.on("publish.upload", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, node.Unknown(errs.New(errs.CodeUnavailable, "connection lost at the last byte"))
	})
	// Make the reconcile answer "cannot tell".
	impl, _ := h.reg.Lookup("publish.upload", 1)
	impl.(*reconcilerNode).reconcile = func(context.Context, node.Input) (node.ReconcileOutcome, node.Result, error) {
		return node.ReconcileUnknown, node.Result{}, nil
	}
	h.add(`{"id":"pub","version":"1.0.0","nodes":[{"id":"up","type":"publish.upload","typeVersion":1,"parameters":{"publicationId":"PUB-001"}}]}`)
	j := h.waitStatus(h.run("pub", nil), domain.JobNeedsAttention)
	ctx := context.Background()
	if _, err := h.engine.Jobs().Resolve(ctx, scopeOf(editor), id.Ref(j.ID), api.ResolveJobRequest{Action: domain.ResolveFail}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("an editor cannot resolve a publication node: %v", err)
	}
	if _, err := h.engine.Jobs().Resolve(ctx, scopeOf(publisher), id.Ref(j.ID), api.ResolveJobRequest{Action: domain.ResolveFail}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobFailed)
}

func TestReconcilableAsksBeforeAnyRetry(t *testing.T) {
	h := newHarness(t)
	impl, _ := h.reg.Lookup("publish.upload", 1)
	rec := impl.(*reconcilerNode)
	var uploads atomic.Int32
	h.on("publish.upload", func(context.Context, node.Env, node.Input) (node.Result, error) {
		uploads.Add(1)
		return node.Result{}, node.Unknown(errs.New(errs.CodeUnavailable, "lost the answer"))
	})
	rec.reconcile = func(ctx context.Context, in node.Input) (node.ReconcileOutcome, node.Result, error) {
		// The earlier upload did reach the destination.
		res, err := node.JSONResult(map[string]any{"publication_id": "PUB-001", "remote_id": "r1", "status": "processing"})
		return node.ReconcileDone, res, err
	}
	h.add(`{"id":"pub","version":"1.0.0","nodes":[{"id":"up","type":"publish.upload","typeVersion":1,"parameters":{"publicationId":"PUB-001"}}],"settings":{"retry":3,"retryBackoffSeconds":0}}`)
	j := h.waitStatus(h.run("pub", nil), domain.JobCompleted)
	if uploads.Load() != 1 {
		t.Fatalf("a duplicate upload happened: %d", uploads.Load())
	}
	if h.reconciles != 1 {
		t.Fatalf("reconciles %d", h.reconciles)
	}
	if outputMap(t, h.nodes(j)["up"])["remote_id"] != "r1" {
		t.Fatalf("output %s", h.nodes(j)["up"].Output)
	}

	// not_started retries; an error reconciling means a person looks.
	uploads.Store(0)
	h.reconciles = 0
	var n atomic.Int32
	h.on("publish.upload", func(context.Context, node.Env, node.Input) (node.Result, error) {
		if n.Add(1) == 1 {
			return node.Result{}, node.Transient(errs.New(errs.CodeUnavailable, "not sent"))
		}
		return node.JSONResult(map[string]any{"publication_id": "PUB-001", "remote_id": "r2", "status": "processing"})
	})
	rec.reconcile = func(context.Context, node.Input) (node.ReconcileOutcome, node.Result, error) {
		return node.ReconcileNotStarted, node.Result{}, nil
	}
	j = h.waitStatus(h.run("pub", nil), domain.JobCompleted)
	if h.nodes(j)["up"].Attempt != 2 || h.reconciles != 1 {
		t.Fatalf("attempt %d reconciles %d", h.nodes(j)["up"].Attempt, h.reconciles)
	}

	h.on("publish.upload", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, node.Unknown(errs.New(errs.CodeUnavailable, "lost"))
	})
	rec.reconcile = func(context.Context, node.Input) (node.ReconcileOutcome, node.Result, error) {
		return node.ReconcileUnknown, node.Result{}, errs.New(errs.CodeUnavailable, "destination down")
	}
	j = h.waitStatus(h.run("pub", nil), domain.JobNeedsAttention)
	if !j.Attention.OutcomeUnknown {
		t.Fatalf("attention %+v", j.Attention)
	}
}

// ---- pause, resume, cancel ----

func TestPauseFinishesWhatStartedAndResumes(t *testing.T) {
	h := newHarness(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.on("video.probe", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		entered <- struct{}{}
		<-release
		return node.JSONResult(map[string]any{"ext": "mp4"})
	})
	h.add(`{"id":"pausable","version":"1.0.0","nodes":[
		{"id":"probe","type":"video.probe","typeVersion":1,"parameters":{"source":"storage://master/a"}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"probe":["tail"]}}`)
	ctx := context.Background()
	j := h.run("pausable", nil)
	<-entered
	paused, err := h.engine.Jobs().Pause(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{Reason: "for a moment"})
	if err != nil || paused.Status != domain.JobPaused {
		t.Fatalf("pause: %+v %v", paused, err)
	}
	// The external effect that started finishes, and the job pauses at the checkpoint (R22).
	close(release)
	h.nodeStatus(j, "probe", domain.NodeCompleted)
	time.Sleep(30 * time.Millisecond)
	cur := h.job(j.ID)
	if cur.Status != domain.JobPaused || h.nodes(cur)["tail"].Status != domain.NodePending {
		t.Fatalf("job %s, tail %s", cur.Status, h.nodes(cur)["tail"].Status)
	}
	if _, err := h.engine.Jobs().Resume(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)
	if _, err := h.engine.Jobs().Pause(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("a finished job cannot pause: %v", err)
	}
}

func TestPointerReadWhilePausedIsEnqueuedAgainOnResume(t *testing.T) {
	h := newHarness(t)
	h.stopWorker()
	h.add(`{"id":"queued","version":"1.0.0","nodes":[{"id":"probe","type":"video.probe","typeVersion":1,"parameters":{"source":"storage://master/a"}}]}`)
	ctx := context.Background()
	j := h.run("queued", nil)
	h.nodeStatus(j, "probe", domain.NodeQueued)
	if _, err := h.engine.Jobs().Pause(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	// A worker reading the pointer now gets 410 and acks it.
	h.startWorker()
	h.waitFor("the worker to drop the pointer", func() bool {
		ok, err := h.q.Inflight(ctx, h.nodes(j)["probe"].TaskID)
		return err == nil && !ok
	})
	if len(h.callsOf("probe")) != 0 {
		t.Fatal("a paused job's task must not start")
	}
	if _, err := h.engine.Jobs().Resume(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)
}

func TestCooperativeCancel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// A node that supports abort aborts.
	h.on("test.slow", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		<-ctx.Done()
		return node.Result{}, ctx.Err()
	})
	h.add(`{"id":"abort","version":"1.0.0","nodes":[
		{"id":"slow","type":"test.slow","typeVersion":1,"parameters":{}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"slow":["tail"]}}`)
	j := h.run("abort", nil)
	h.nodeStatus(j, "slow", domain.NodeRunning)
	if _, err := h.engine.Jobs().Cancel(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{Reason: "changed my mind"}); err != nil {
		t.Fatal(err)
	}
	done := h.waitStatus(j, domain.JobCancelled)
	if n := h.nodes(done); n["slow"].Status != domain.NodeCancelled || n["tail"].Status != domain.NodeCancelled {
		t.Fatalf("nodes %+v", n)
	}
	if countEvents(t, h.store, event.WorkflowCancelled) != 1 {
		t.Fatal("workflow.cancelled must fire")
	}
	// It is idempotent.
	if again, err := h.engine.Jobs().Cancel(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil || again.Status != domain.JobCancelled {
		t.Fatalf("again: %+v %v", again, err)
	}

	// A node that cannot abort finishes, and its checkpoint stands.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h.on("test.stubborn", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		entered <- struct{}{}
		<-release
		return node.JSONResult(map[string]any{"finished": true})
	})
	h.add(`{"id":"stubborn","version":"1.0.0","nodes":[
		{"id":"st","type":"test.stubborn","typeVersion":1,"parameters":{}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"st":["tail"]}}`)
	j = h.run("stubborn", nil)
	<-entered
	if _, err := h.engine.Jobs().Cancel(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if st := h.job(j.ID).Status; st.Terminal() {
		t.Fatalf("the job must wait for the running node: %s", st)
	}
	close(release)
	done = h.waitStatus(j, domain.JobCancelled)
	if n := h.nodes(done); n["st"].Status != domain.NodeCompleted || n["tail"].Status != domain.NodeCancelled {
		t.Fatalf("nodes %+v", n)
	}
	if len(h.callsOf("tail")) != 0 {
		t.Fatal("nothing new may start")
	}
}

func TestCancelWorkerTaskThroughHeartbeatFlag(t *testing.T) {
	h := newHarness(t, harnessOpts{lease: 200 * time.Millisecond})
	h.on("video.transcode", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		<-ctx.Done()
		return node.Result{}, ctx.Err()
	})
	h.add(`{"id":"long","version":"1.0.0","nodes":[{"id":"t","type":"video.transcode","typeVersion":1,"parameters":{"source":"storage://master/a","profile":"proxy-720p"}}]}`)
	j := h.run("long", nil)
	h.nodeStatus(j, "t", domain.NodeRunning)
	if _, err := h.engine.Jobs().Cancel(context.Background(), scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCancelled)
}

func TestCancelledQueuedTaskIsNeverLeased(t *testing.T) {
	h := newHarness(t)
	h.stopWorker()
	h.add(`{"id":"queued","version":"1.0.0","nodes":[{"id":"probe","type":"video.probe","typeVersion":1,"parameters":{"source":"storage://master/a"}}]}`)
	j := h.run("queued", nil)
	h.nodeStatus(j, "probe", domain.NodeQueued)
	if _, err := h.engine.Jobs().Cancel(context.Background(), scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCancelled)
	h.startWorker()
	time.Sleep(30 * time.Millisecond)
	if len(h.callsOf("probe")) != 0 {
		t.Fatal("a cancelled task ran")
	}
}

func TestJobTimeout(t *testing.T) {
	h := newHarness(t)
	h.on("test.slow", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		<-ctx.Done()
		return node.Result{}, ctx.Err()
	})
	h.add(`{"id":"timed","version":"1.0.0","nodes":[{"id":"s","type":"test.slow","typeVersion":1,"parameters":{}}],"settings":{"timeoutSeconds":1}}`)
	j := h.waitStatus(h.run("timed", nil), domain.JobFailed)
	if j.Error == nil || j.Error.Code != CodeTimeout {
		t.Fatalf("error %+v", j.Error)
	}
}

func TestPanicInNodeFailsIt(t *testing.T) {
	h := newHarness(t)
	h.on("test.pure", func(context.Context, node.Env, node.Input) (node.Result, error) { panic("oops") })
	h.add(`{"id":"panics","version":"1.0.0","nodes":[{"id":"p","type":"test.pure","typeVersion":1,"parameters":{}}]}`)
	j := h.waitStatus(h.run("panics", nil), domain.JobFailed)
	if j.Error.Code != string(errs.CodeInternal) {
		t.Fatalf("error %+v", j.Error)
	}
}

func TestBadParametersFailTheNode(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"bad","version":"1.0.0","inputs":{"n":{"type":"number"}},"nodes":[
		{"id":"c","type":"catalog.get","typeVersion":1,"parameters":{"id":"${inputs.n}"}}]}`)
	j := h.waitStatus(h.run("bad", map[string]any{"n": 3}), domain.JobFailed)
	if j.Error.Code != string(errs.CodeValidationFailed) || !strings.Contains(j.Error.Message, "catalog.get") {
		t.Fatalf("error %+v", j.Error)
	}
	if len(h.callsOf("c")) != 0 {
		t.Fatal("the node must not start")
	}
}

// ---- budget (I33) ----

func TestBudgetPausesBeforePaidNode(t *testing.T) {
	h := newHarness(t)
	h.budgets.blocked = true
	h.output("test.paid", map[string]any{"ok": true})
	h.on("test.daemon", func(context.Context, node.Env, node.Input) (node.Result, error) {
		res, err := node.JSONResult(map[string]any{})
		res.Usage = []domain.UsageRecord{{Operation: "ai.external", Quantity: 1}}
		return res, err
	})
	h.add(`{"id":"paid","version":"1.0.0","nodes":[
		{"id":"free","type":"test.daemon","typeVersion":1,"parameters":{}},
		{"id":"costly","type":"test.paid","typeVersion":1,"parameters":{}}
	],"connections":{"free":["costly"]}}`)
	j := h.waitStatus(h.run("paid", nil), domain.JobPaused)
	if j.StatusReason != ReasonBudgetExceeded || j.Error != nil {
		t.Fatalf("job %+v", j)
	}
	if len(h.callsOf("costly")) != 0 {
		t.Fatal("a blocked paid node must not start")
	}
	// Resuming while still blocked pauses again.
	ctx := context.Background()
	if _, err := h.engine.Jobs().Resume(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobPaused)
	h.budgets.mu.Lock()
	h.budgets.blocked = false
	h.budgets.mu.Unlock()
	if _, err := h.engine.Jobs().Resume(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)
	h.budgets.mu.Lock()
	defer h.budgets.mu.Unlock()
	if len(h.budgets.recorded) != 1 || h.budgets.recorded[0].NodeID != "free" || h.budgets.recorded[0].JobID != string(j.ID) {
		t.Fatalf("usage %+v", h.budgets.recorded)
	}
}
