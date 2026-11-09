package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

type fakeRunner struct {
	mu     sync.Mutex
	calls  []string
	fn     func(inv hook.Invocation) (hook.Result, error)
	counts map[string]int
}

func (f *fakeRunner) RunHookAction(ctx context.Context, inv hook.Invocation) (hook.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, inv.Hook.ID)
	if f.counts == nil {
		f.counts = map[string]int{}
	}
	f.counts[inv.Hook.ID]++
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(inv)
	}
	return hook.Result{Allow: true}, nil
}

func decl(id string, p wfdef.HookFailurePolicy) wfdef.HookDecl {
	return wfdef.HookDecl{ID: id, Plugin: "p", Action: "a", OnFailure: p}
}

func TestOrder(t *testing.T) {
	chain := hook.Chain{
		Global:   []wfdef.HookDecl{decl("g", "")},
		Workflow: []wfdef.HookDecl{decl("w", "")},
		Node:     []wfdef.HookDecl{decl("n", "")},
	}
	for _, c := range []struct {
		point hook.Point
		want  string
	}{
		{hook.WorkflowBefore, "g,w,n"},
		{hook.WorkflowStart, "g,w,n"},
		{hook.NodeBefore, "g,w,n"},
		{hook.WorkflowAfter, "n,w,g"},
		{hook.NodeAfter, "n,w,g"},
		{hook.WorkflowFinish, "n,w,g"},
		{hook.NodeFinish, "n,w,g"},
		{hook.WorkflowError, "n,w,g"},
		{hook.WorkflowCancel, "n,w,g"},
	} {
		r := &fakeRunner{}
		d := New(r, Options{})
		if _, err := d.Dispatch(context.Background(), c.point, chain, hook.Invocation{}); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(r.calls, ","); got != c.want {
			t.Errorf("%s: %s, want %s", c.point, got, c.want)
		}
	}
}

func TestBeforeRefusalBlocksWhateverThePolicy(t *testing.T) {
	r := &fakeRunner{fn: func(hook.Invocation) (hook.Result, error) {
		return hook.Result{Allow: false, Reason: "source is not ready"}, nil
	}}
	d := New(r, Options{})
	_, err := d.Dispatch(context.Background(), hook.WorkflowBefore, hook.Chain{Global: []wfdef.HookDecl{decl("v", wfdef.HookContinue), decl("next", "")}}, hook.Invocation{})
	b, ok := AsBlocked(err)
	if !ok || b.Reason != "source is not ready" || b.Hook != "v" {
		t.Fatalf("blocked: %v", err)
	}
	if strings.Join(r.calls, ",") != "v" {
		t.Fatalf("later hooks must not run: %v", r.calls)
	}
	// Allow false after the fact means nothing: only before points decide.
	if _, err := d.Dispatch(context.Background(), hook.NodeAfter, hook.Chain{Node: []wfdef.HookDecl{decl("v", "")}}, hook.Invocation{}); err != nil {
		t.Fatal(err)
	}
}

func TestFailurePolicies(t *testing.T) {
	fail := func(hook.Invocation) (hook.Result, error) { return hook.Result{}, errors.New("internal detail") }
	cases := []struct {
		name    string
		point   hook.Point
		policy  wfdef.HookFailurePolicy
		blocked bool
		calls   int
	}{
		{"before defaults to abort", hook.WorkflowBefore, "", true, 1},
		{"after defaults to continue", hook.WorkflowAfter, "", false, 1},
		{"abort at after blocks", hook.WorkflowAfter, wfdef.HookAbort, true, 1},
		{"abort at start blocks", hook.WorkflowStart, wfdef.HookAbort, true, 1},
		{"abort at node after blocks", hook.NodeAfter, wfdef.HookAbort, true, 1},
		{"continue at before continues", hook.NodeBefore, wfdef.HookContinue, false, 1},
		{"abort at finish cannot block", hook.WorkflowFinish, wfdef.HookAbort, false, 1},
		{"abort at error cannot block", hook.NodeError, wfdef.HookAbort, false, 1},
		{"abort at cancel cannot block", hook.WorkflowCancel, wfdef.HookAbort, false, 1},
		{"retry then abort", hook.WorkflowBefore, wfdef.HookRetry, true, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &fakeRunner{fn: fail}
			d := New(r, Options{RetryBackoff: time.Millisecond})
			_, err := d.Dispatch(context.Background(), c.point, hook.Chain{Workflow: []wfdef.HookDecl{decl("h", c.policy)}}, hook.Invocation{})
			if _, blocked := AsBlocked(err); blocked != c.blocked {
				t.Fatalf("blocked=%v: %v", blocked, err)
			}
			if len(r.calls) != c.calls {
				t.Fatalf("calls %d", len(r.calls))
			}
			if err != nil && strings.Contains(err.Error(), "internal detail") {
				t.Fatalf("an internal cause must not leak: %v", err)
			}
		})
	}
	attempts := 0
	r := &fakeRunner{fn: func(hook.Invocation) (hook.Result, error) {
		attempts++
		if attempts < 2 {
			return hook.Result{}, errs.New(errs.CodeUnavailable, "try again")
		}
		return hook.Result{Allow: true}, nil
	}}
	d := New(r, Options{RetryBackoff: time.Millisecond})
	if _, err := d.Dispatch(context.Background(), hook.WorkflowBefore, hook.Chain{Global: []wfdef.HookDecl{decl("h", wfdef.HookRetry)}}, hook.Invocation{}); err != nil {
		t.Fatalf("a retry that succeeds passes: %v", err)
	}
}

func TestTimeouts(t *testing.T) {
	var got time.Duration
	r := &fakeRunner{}
	run := func(seconds, max int) time.Duration {
		d := New(runnerFunc(func(ctx context.Context, inv hook.Invocation) (hook.Result, error) {
			deadline, _ := ctx.Deadline()
			got = time.Until(deadline).Round(time.Second)
			return hook.Result{Allow: true}, nil
		}), Options{MaxTimeoutSeconds: max})
		h := decl("h", "")
		h.TimeoutSeconds = seconds
		if _, err := d.Dispatch(context.Background(), hook.WorkflowAfter, hook.Chain{Global: []wfdef.HookDecl{h}}, hook.Invocation{}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if d := run(0, 0); d != 30*time.Second {
		t.Fatalf("default: %s (D40)", d)
	}
	if d := run(15, 0); d != 15*time.Second {
		t.Fatalf("declared: %s", d)
	}
	if d := run(600, 60); d != 60*time.Second {
		t.Fatalf("capped: %s", d)
	}
	_ = r
	// A hook that hangs is stopped by its timeout.
	slow := New(runnerFunc(func(ctx context.Context, inv hook.Invocation) (hook.Result, error) {
		<-ctx.Done()
		return hook.Result{}, ctx.Err()
	}), Options{})
	h := decl("h", wfdef.HookAbort)
	h.TimeoutSeconds = 1
	start := time.Now()
	_, err := slow.Dispatch(context.Background(), hook.WorkflowBefore, hook.Chain{Global: []wfdef.HookDecl{h}}, hook.Invocation{})
	b, ok := AsBlocked(err)
	if !ok || !strings.Contains(b.Reason, "timed out") || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
}

type runnerFunc func(ctx context.Context, inv hook.Invocation) (hook.Result, error)

func (f runnerFunc) RunHookAction(ctx context.Context, inv hook.Invocation) (hook.Result, error) {
	return f(ctx, inv)
}

func TestModifiedParametersPassAlong(t *testing.T) {
	var seen []string
	r := runnerFunc(func(ctx context.Context, inv hook.Invocation) (hook.Result, error) {
		seen = append(seen, string(inv.Data))
		if inv.Hook.ID == "first" {
			return hook.Result{Allow: true, Parameters: json.RawMessage(`{"a":2}`)}, nil
		}
		return hook.Result{Allow: true}, nil
	})
	d := New(r, Options{})
	res, err := d.Dispatch(context.Background(), hook.NodeBefore,
		hook.Chain{Global: []wfdef.HookDecl{decl("first", "")}, Node: []wfdef.HookDecl{decl("second", "")}},
		hook.Invocation{Data: json.RawMessage(`{"parameters":{"a":1}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Parameters) != `{"a":2}` {
		t.Fatalf("result %s", res.Parameters)
	}
	if !strings.Contains(seen[1], `"a":2`) {
		t.Fatalf("second hook must see the modification: %s", seen[1])
	}
	// After a node has run, parameters are history.
	res, err = d.Dispatch(context.Background(), hook.NodeAfter, hook.Chain{Global: []wfdef.HookDecl{decl("first", "")}}, hook.Invocation{})
	if err != nil || len(res.Parameters) != 0 {
		t.Fatalf("after: %s %v", res.Parameters, err)
	}
}

func TestNoRunnerFailsClosed(t *testing.T) {
	d := New(nil, Options{})
	if _, err := d.Dispatch(context.Background(), hook.WorkflowBefore, hook.Chain{Global: []wfdef.HookDecl{decl("sec", "")}}, hook.Invocation{}); err == nil {
		t.Fatal("a security hook with nothing to run it must block")
	}
	if _, err := d.Dispatch(context.Background(), hook.WorkflowFinish, hook.Chain{Global: []wfdef.HookDecl{decl("x", "")}}, hook.Invocation{}); err != nil {
		t.Fatal(err)
	}
}

func TestChainFor(t *testing.T) {
	global := config.Hooks{
		Workflow: wfdef.HookSet{Before: []wfdef.HookDecl{decl("gw", "")}},
		Node:     wfdef.HookSet{Before: []wfdef.HookDecl{decl("gn", "")}},
	}
	wf := &wfdef.HookSet{Before: []wfdef.HookDecl{decl("w", "")}, Cancel: []wfdef.HookDecl{decl("wc", "")}}
	nd := &wfdef.HookSet{Before: []wfdef.HookDecl{decl("n", "")}}
	c := ChainFor(hook.WorkflowBefore, global, wf, nd)
	if len(c.Global) != 1 || c.Global[0].ID != "gw" || c.Workflow[0].ID != "w" || len(c.Node) != 0 {
		t.Fatalf("workflow chain %+v", c)
	}
	c = ChainFor(hook.NodeBefore, global, wf, nd)
	if c.Global[0].ID != "gn" || len(c.Workflow) != 0 || c.Node[0].ID != "n" {
		t.Fatalf("node chain %+v", c)
	}
	if Empty(c) || !Empty(ChainFor(hook.NodeFinish, global, wf, nd)) {
		t.Fatal("empty")
	}
	if got := ChainFor(hook.WorkflowCancel, global, wf, nil); got.Workflow[0].ID != "wc" {
		t.Fatalf("cancel %+v", got)
	}
}

func TestCapTimeouts(t *testing.T) {
	short := decl("short", "")
	short.TimeoutSeconds = 5
	long := decl("long", "")
	long.TimeoutSeconds = 500
	unset := decl("unset", "")
	c := CapTimeouts(hook.Chain{Global: []wfdef.HookDecl{short}, Workflow: []wfdef.HookDecl{long}, Node: []wfdef.HookDecl{unset}}, 20)
	if c.Global[0].TimeoutSeconds != 5 || c.Workflow[0].TimeoutSeconds != 20 || c.Node[0].TimeoutSeconds != 20 {
		t.Fatalf("chain %+v", c)
	}
	if long.TimeoutSeconds != 500 {
		t.Fatal("the declaration must not change")
	}
	same := CapTimeouts(hook.Chain{Workflow: []wfdef.HookDecl{long}}, 0)
	if same.Workflow[0].TimeoutSeconds != 500 {
		t.Fatalf("no ceiling %+v", same)
	}
}
