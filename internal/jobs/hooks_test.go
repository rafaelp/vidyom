package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

func decl(id string) wfdef.HookDecl { return wfdef.HookDecl{ID: id, Plugin: "p", Action: "a"} }

const hookedDoc = `{"id":"hooked","version":"1.0.0",
	"hooks":{"before":[{"id":"w-before","plugin":"p","action":"a"}],"start":[{"id":"w-start","plugin":"p","action":"a"}],
		"after":[{"id":"w-after","plugin":"p","action":"a"}],"finish":[{"id":"w-finish","plugin":"p","action":"a"}],
		"error":[{"id":"w-error","plugin":"p","action":"a"}],"cancel":[{"id":"w-cancel","plugin":"p","action":"a"}]},
	"nodes":[{"id":"n","type":"test.daemon","typeVersion":1,"parameters":{},
		"hooks":{"before":[{"id":"n-before","plugin":"p","action":"a"}],"after":[{"id":"n-after","plugin":"p","action":"a"}],
			"finish":[{"id":"n-finish","plugin":"p","action":"a"}],"error":[{"id":"n-error","plugin":"p","action":"a"}]}}]}`

func (h *harness) hooksCalled() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.hookCalls...)
}

func TestHookOrderOnSuccess(t *testing.T) {
	h := newHarness(t)
	h.tenantCfg = config.Tenant{Hooks: config.Hooks{
		Workflow: wfdef.HookSet{Before: []wfdef.HookDecl{decl("g-before")}, Finish: []wfdef.HookDecl{decl("g-finish")}},
		Node:     wfdef.HookSet{Before: []wfdef.HookDecl{decl("gn-before")}, After: []wfdef.HookDecl{decl("gn-after")}},
	}}
	h.add(hookedDoc)
	h.waitStatus(h.run("hooked", nil), domain.JobCompleted)
	got := strings.Join(h.hooksCalled(), " ")
	want := "workflow.before:g-before workflow.before:w-before workflow.start:w-start " +
		"node.before:gn-before node.before:n-before node.after:n-after node.after:gn-after node.finish:n-finish " +
		"workflow.after:w-after workflow.finish:w-finish workflow.finish:g-finish"
	if got != want {
		t.Fatalf("order:\n got %s\nwant %s", got, want)
	}
}

func TestFinishRunsOnErrorAndCancel(t *testing.T) {
	h := newHarness(t)
	h.on("test.daemon", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition, "nope"))
	})
	h.add(hookedDoc)
	h.waitStatus(h.run("hooked", nil), domain.JobFailed)
	got := strings.Join(h.hooksCalled(), " ")
	for _, want := range []string{"node.error:n-error", "node.finish:n-finish", "workflow.error:w-error", "workflow.finish:w-finish"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%s missing in %s", want, got)
		}
	}
	if strings.Contains(got, "workflow.after") {
		t.Fatalf("after is for success: %s", got)
	}

	h = newHarness(t)
	h.on("test.daemon", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		time.Sleep(50 * time.Millisecond)
		return node.JSONResult(map[string]any{})
	})
	h.add(hookedDoc)
	j := h.run("hooked", nil)
	h.nodeStatus(j, "n", domain.NodeRunning)
	if _, err := h.engine.Jobs().Cancel(context.Background(), scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCancelled)
	got = strings.Join(h.hooksCalled(), " ")
	if !strings.Contains(got, "workflow.cancel:w-cancel") || !strings.Contains(got, "workflow.finish:w-finish") {
		t.Fatalf("calls %s", got)
	}
}

func TestBeforeHookBlocksAndIsAudited(t *testing.T) {
	h := newHarness(t)
	h.hookFn = func(inv hook.Invocation) (hook.Result, error) {
		if inv.Hook.ID == "w-before" {
			return hook.Result{Allow: false, Reason: "the source is not ready"}, nil
		}
		return hook.Result{Allow: true}, nil
	}
	h.add(hookedDoc)
	j := h.waitStatus(h.run("hooked", nil), domain.JobFailed)
	if j.Error == nil || j.Error.Code != CodeHookBlocked || !strings.Contains(j.Error.Message, "not ready") {
		t.Fatalf("error %+v", j.Error)
	}
	if len(h.callsOf("n")) != 0 {
		t.Fatal("nothing may run after a refusal")
	}
	audit, _, _ := h.store.Audit().List(context.Background(), repo.AuditFilter{Action: "hook.blocked"}, repo.Page{})
	if len(audit) != 1 || audit[0].Result != "denied" {
		t.Fatalf("audit %+v (§15.7)", audit)
	}

	// At node level too.
	h = newHarness(t)
	h.hookFn = func(inv hook.Invocation) (hook.Result, error) {
		return hook.Result{Allow: inv.Hook.ID != "n-before", Reason: "refused"}, nil
	}
	h.add(hookedDoc)
	j = h.waitStatus(h.run("hooked", nil), domain.JobFailed)
	if j.Error.Code != CodeHookBlocked || j.Error.NodeID != "n" || len(h.callsOf("n")) != 0 {
		t.Fatalf("error %+v calls %d", j.Error, len(h.callsOf("n")))
	}
}

func TestAbortAtAfterStopsTheJobKeepingTheCheckpoint(t *testing.T) {
	h := newHarness(t)
	h.hookFn = func(inv hook.Invocation) (hook.Result, error) {
		if inv.Hook.ID == "guard" {
			return hook.Result{}, errors.New("guard failed")
		}
		return hook.Result{Allow: true}, nil
	}
	h.add(`{"id":"guarded","version":"1.0.0","nodes":[
		{"id":"n","type":"test.daemon","typeVersion":1,"parameters":{},"hooks":{"after":[{"id":"guard","plugin":"p","action":"a","onFailure":"abort"}]}},
		{"id":"tail","type":"test.pure","typeVersion":1,"parameters":{}}
	],"connections":{"n":["tail"]}}`)
	j := h.waitStatus(h.run("guarded", nil), domain.JobFailed)
	nodes := h.nodes(j)
	if nodes["n"].Status != domain.NodeCompleted || len(nodes["n"].Output) == 0 || nodes["tail"].Status != domain.NodePending {
		t.Fatalf("nodes %+v", nodes)
	}
	// A retry continues past it, without running the node again.
	h.mu.Lock()
	h.hookFn = nil
	h.mu.Unlock()
	if _, err := h.engine.Jobs().Retry(context.Background(), scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(j, domain.JobCompleted)
	if len(h.callsOf("n")) != 1 {
		t.Fatalf("n ran %d times", len(h.callsOf("n")))
	}
}

func TestHookModifiedParametersGoThroughValidationAgain(t *testing.T) {
	h := newHarness(t)
	doc := `{"id":"mod","version":"1.0.0","nodes":[
		{"id":"p","type":"storage.get","typeVersion":1,"parameters":{"source":"storage://master/a"},"hooks":{"before":[{"id":"mod","plugin":"p","action":"a"}]}}]}`
	h.add(doc)

	// A valid change reaches the node.
	h.hookFn = func(inv hook.Invocation) (hook.Result, error) {
		var data struct {
			Parameters map[string]any `json:"parameters"`
		}
		if err := json.Unmarshal(inv.Data, &data); err != nil || data.Parameters["source"] != "storage://master/a" {
			t.Errorf("hook data %s", inv.Data)
		}
		return hook.Result{Allow: true, Parameters: json.RawMessage(`{"source":"storage://master/b"}`)}, nil
	}
	h.waitStatus(h.run("mod", nil), domain.JobCompleted)
	var params map[string]any
	_ = json.Unmarshal(h.callsOf("p")[0].Params, &params)
	if params["source"] != "storage://master/b" {
		t.Fatalf("params %v", params)
	}

	// An invalid one fails the node.
	h.mu.Lock()
	h.hookFn = func(inv hook.Invocation) (hook.Result, error) {
		return hook.Result{Allow: true, Parameters: json.RawMessage(`{"nope":1}`)}, nil
	}
	h.mu.Unlock()
	j := h.waitStatus(h.run("mod", nil), domain.JobFailed)
	if j.Error.Code != CodeHookBlocked {
		t.Fatalf("error %+v", j.Error)
	}

	// One needing a permission the job lacks fails it too: authorization runs again.
	h.mu.Lock()
	h.hookFn = func(inv hook.Invocation) (hook.Result, error) {
		return hook.Result{Allow: true, Parameters: json.RawMessage(`{"source":"storage://archive/raw","allowArchive":true}`)}, nil
	}
	h.mu.Unlock()
	j = h.waitStatus(h.run("mod", nil), domain.JobFailed)
	if !strings.Contains(j.Error.Message, string(domain.PermStorageArchiveRestore)) {
		t.Fatalf("error %+v", j.Error)
	}
}

func TestHookCannotRetryUnsafe(t *testing.T) {
	h := newHarness(t)
	h.on("test.unsafe", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, node.Unknown(errs.New(errs.CodeUnavailable, "lost"))
	})
	h.hookFn = func(inv hook.Invocation) (hook.Result, error) {
		// Whatever a node.error hook answers.
		return hook.Result{Allow: true, Reason: "retry please"}, nil
	}
	h.add(`{"id":"unsafe","version":"1.0.0","nodes":[{"id":"u","type":"test.unsafe","typeVersion":1,"parameters":{},
		"hooks":{"error":[{"id":"e","plugin":"p","action":"a","onFailure":"retry"}]}}]}`)
	h.waitStatus(h.run("unsafe", nil), domain.JobNeedsAttention)
	h.waitFor("the error hook", func() bool { return strings.Contains(strings.Join(h.hooksCalled(), " "), "node.error:e") })
	if len(h.callsOf("u")) != 1 {
		t.Fatalf("D11: %d runs", len(h.callsOf("u")))
	}
}

func TestFinishHookSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	h.hookFn = func(inv hook.Invocation) (hook.Result, error) {
		if inv.Hook.ID == "f" {
			entered <- struct{}{}
			<-release
		}
		return hook.Result{Allow: true}, nil
	}
	h.add(`{"id":"finishing","version":"1.0.0","hooks":{"finish":[{"id":"f","plugin":"p","action":"a"}]},"nodes":[{"id":"n","type":"test.pure","typeVersion":1,"parameters":{}}]}`)
	j := h.run("finishing", nil)
	<-entered
	if st := h.job(j.ID).Status; st.Terminal() {
		t.Fatalf("the terminal state is written after finish hooks: %s", st)
	}
	// The daemon dies mid-hook. The hook runs again after it comes back (§8.1).
	stopped := make(chan struct{})
	go func() {
		h.stopWorker()
		h.stopEngine()
		close(stopped)
	}()
	close(release)
	<-stopped
	calls := len(h.callsOf("n"))
	h.mu.Lock()
	h.hookFn = nil
	h.mu.Unlock()
	if h.job(j.ID).Status.Terminal() {
		// The hook finished before the stop reached it, which is also a valid order.
		return
	}
	h.startEngine()
	h.startWorker()
	h.waitStatus(j, domain.JobCompleted)
	if len(h.callsOf("n")) != calls {
		t.Fatal("the node must not run again")
	}
	count := 0
	for _, c := range h.hooksCalled() {
		if c == "workflow.finish:f" {
			count++
		}
	}
	if count < 2 {
		t.Fatalf("finish ran %d times", count)
	}
}

func TestDeclaredHookWithNoRunnerFailsClosed(t *testing.T) {
	h := newHarness(t, harnessOpts{noEngine: true})
	h.deps.Hooks = nil
	h.stopWorker()
	h.startEngine()
	h.startWorker()
	h.add(`{"id":"sec","version":"1.0.0","hooks":{"before":[{"id":"sec","plugin":"p","action":"a"}]},"nodes":[{"id":"n","type":"test.pure","typeVersion":1,"parameters":{}}]}`)
	j := h.waitStatus(h.run("sec", nil), domain.JobFailed)
	if j.Error.Code != CodeHookBlocked || len(h.callsOf("n")) != 0 {
		t.Fatalf("error %+v", j.Error)
	}
}
