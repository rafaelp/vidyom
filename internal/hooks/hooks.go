package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// DefaultMaxTimeoutSeconds caps every hook when the tenant sets no ceiling (I149).
const DefaultMaxTimeoutSeconds = 300

// Options configures a Dispatcher. Zero values take the defaults.
type Options struct {
	MaxTimeoutSeconds int
	RetryAttempts     int           // onFailure retry; default 3
	RetryBackoff      time.Duration // default 1 s, doubling
	Logger            *slog.Logger
}

// Dispatcher implements hook.Dispatcher.
type Dispatcher struct {
	runner hook.ActionRunner
	opts   Options
}

var _ hook.Dispatcher = (*Dispatcher)(nil)

// New returns a dispatcher. With a nil runner every declared hook fails, which each
// hook's policy then handles: a `before` hook with nothing to run it blocks, rather
// than letting a security check silently pass.
func New(runner hook.ActionRunner, opts Options) *Dispatcher {
	if opts.MaxTimeoutSeconds <= 0 {
		opts.MaxTimeoutSeconds = DefaultMaxTimeoutSeconds
	}
	if opts.RetryAttempts <= 0 {
		opts.RetryAttempts = 3
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Dispatcher{runner: runner, opts: opts}
}

// IsBefore reports a point that can validate and block.
func IsBefore(p hook.Point) bool { return p == hook.WorkflowBefore || p == hook.NodeBefore }

// CanBlock reports a point where a failing hook under abort stops the job. error,
// finish and cancel report what already happened, so they never block (I149).
func CanBlock(p hook.Point) bool {
	switch p {
	case hook.WorkflowBefore, hook.WorkflowStart, hook.WorkflowAfter, hook.NodeBefore, hook.NodeAfter:
		return true
	default:
		return false
	}
}

// Order returns the hooks of a chain in execution order: global, workflow, node for
// before and start, reversed for the others (§8.1).
func Order(p hook.Point, c hook.Chain) []wfdef.HookDecl {
	out := make([]wfdef.HookDecl, 0, len(c.Global)+len(c.Workflow)+len(c.Node))
	switch p {
	case hook.WorkflowBefore, hook.WorkflowStart, hook.NodeBefore:
		out = append(append(append(out, c.Global...), c.Workflow...), c.Node...)
	default:
		out = append(append(append(out, c.Node...), c.Workflow...), c.Global...)
	}
	return out
}

func defaultPolicy(p hook.Point) wfdef.HookFailurePolicy {
	if IsBefore(p) {
		return wfdef.HookAbort
	}
	return wfdef.HookContinue
}

// Blocked is what Dispatch returns when a `before` hook said no or a hook under
// abort failed. Its Reason is safe to show.
type Blocked struct {
	Point  hook.Point
	Hook   string
	Plugin string
	Reason string
}

func (b *Blocked) Error() string {
	return fmt.Sprintf("hook %s of plugin %s blocked %s: %s", b.Hook, b.Plugin, b.Point, b.Reason)
}

// AsBlocked unwraps a Blocked error.
func AsBlocked(err error) (*Blocked, bool) {
	var b *Blocked
	if errors.As(err, &b) {
		return b, true
	}
	return nil, false
}

// Dispatch implements hook.Dispatcher. Only a `before` point can modify parameters;
// the next hook in the chain sees the modification, and the engine validates the
// final set again (§8.1). base.Data, when it is a JSON object with parameters,
// carries them.
func (d *Dispatcher) Dispatch(ctx context.Context, point hook.Point, chain hook.Chain, base hook.Invocation) (hook.Result, error) {
	final := hook.Result{Allow: true}
	data := base.Data
	for _, decl := range Order(point, chain) {
		inv := base
		inv.Point = point
		inv.Hook = decl
		inv.Data = data
		res, err := d.runWithPolicy(ctx, point, decl, inv)
		if err != nil {
			return hook.Result{Allow: false, Reason: err.Error()}, err
		}
		if point == hook.NodeBefore && len(res.Parameters) > 0 {
			final.Parameters = res.Parameters
			data = withParameters(data, res.Parameters)
		}
	}
	return final, nil
}

// Probe runs one hook once, with no policy, and returns what it said: `vidyom hook test`
// shows this, including parameters that Dispatch only passes on at node.before.
func (d *Dispatcher) Probe(ctx context.Context, inv hook.Invocation) (hook.Result, error) {
	return d.runOnce(ctx, inv.Hook, inv)
}

func withParameters(data, params json.RawMessage) json.RawMessage {
	doc := map[string]json.RawMessage{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			doc = map[string]json.RawMessage{}
		}
	}
	doc["parameters"] = params
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}

func (d *Dispatcher) timeout(decl wfdef.HookDecl) time.Duration {
	seconds := decl.TimeoutSeconds
	if seconds <= 0 {
		seconds = hook.DefaultTimeoutSeconds
	}
	if seconds > d.opts.MaxTimeoutSeconds {
		seconds = d.opts.MaxTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (d *Dispatcher) runOnce(ctx context.Context, decl wfdef.HookDecl, inv hook.Invocation) (hook.Result, error) {
	if d.runner == nil {
		return hook.Result{}, errs.New(errs.CodeUnavailable, "no plugin runtime is wired to run hooks")
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout(decl))
	defer cancel()
	res, err := d.runner.RunHookAction(ctx, inv)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return res, err
}

func (d *Dispatcher) runWithPolicy(ctx context.Context, point hook.Point, decl wfdef.HookDecl, inv hook.Invocation) (hook.Result, error) {
	policy := decl.OnFailure
	if policy == "" {
		policy = defaultPolicy(point)
	}
	attempts := 1
	if policy == wfdef.HookRetry {
		attempts = d.opts.RetryAttempts
	}
	backoff := d.opts.RetryBackoff
	var res hook.Result
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return hook.Result{}, ctx.Err()
			}
			backoff *= 2
		}
		res, err = d.runOnce(ctx, decl, inv)
		if err == nil {
			break
		}
	}
	log := d.opts.Logger.With("tenant_id", inv.TenantID, "job_id", inv.JobID, "node_id", inv.NodeID,
		"hook", decl.ID, "plugin", decl.Plugin, "point", string(point))
	if err == nil {
		// An explicit "no" at a before point blocks whatever the policy says: the
		// policy is about hooks that fail, not about hooks that decide.
		if IsBefore(point) && !res.Allow {
			reason := res.Reason
			if reason == "" {
				reason = "refused"
			}
			log.Info("hook refused", "reason", reason)
			return res, &Blocked{Point: point, Hook: decl.ID, Plugin: decl.Plugin, Reason: reason}
		}
		log.Debug("hook ran")
		return res, nil
	}
	reason := errs.Public(err).Message
	if errors.Is(err, context.DeadlineExceeded) {
		reason = fmt.Sprintf("timed out after %s", d.timeout(decl))
	}
	if ctx.Err() != nil {
		return hook.Result{}, ctx.Err()
	}
	if policy == wfdef.HookContinue || !CanBlock(point) {
		log.Warn("hook failed; continuing", "reason", reason)
		return hook.Result{Allow: true}, nil
	}
	log.Warn("hook failed; blocking", "reason", reason)
	return hook.Result{}, &Blocked{Point: point, Hook: decl.ID, Plugin: decl.Plugin, Reason: reason}
}

// Select returns the declarations of a point's short name in a hook set (R34).
func Select(set *wfdef.HookSet, p hook.Point) []wfdef.HookDecl {
	if set == nil {
		return nil
	}
	switch p {
	case hook.WorkflowBefore, hook.NodeBefore:
		return set.Before
	case hook.WorkflowStart:
		return set.Start
	case hook.WorkflowAfter, hook.NodeAfter:
		return set.After
	case hook.WorkflowFinish, hook.NodeFinish:
		return set.Finish
	case hook.WorkflowError, hook.NodeError:
		return set.Error
	case hook.WorkflowCancel:
		return set.Cancel
	}
	return nil
}

// ChainFor assembles the chain of a point. hooks.json holds the global level, and
// workflows cannot remove what it declares (§8.1). The scope comes from the position
// (R34): a workflow's top-level hooks are workflow points, a node's are node points.
func ChainFor(p hook.Point, global config.Hooks, workflow, nodeSet *wfdef.HookSet) hook.Chain {
	switch p {
	case hook.NodeBefore, hook.NodeAfter, hook.NodeError, hook.NodeFinish:
		return hook.Chain{Global: Select(&global.Node, p), Node: Select(nodeSet, p)}
	default:
		return hook.Chain{Global: Select(&global.Workflow, p), Workflow: Select(workflow, p)}
	}
}

// CapTimeouts applies a tenant's ceiling to every hook of a chain (§8.1, D40): a
// notification hook cannot hold a workflow longer than the tenant allows. A ceiling
// of 0 leaves the chain as it is, under the dispatcher's own limit.
func CapTimeouts(c hook.Chain, maxSeconds int) hook.Chain {
	if maxSeconds <= 0 {
		return c
	}
	apply := func(list []wfdef.HookDecl) []wfdef.HookDecl {
		if len(list) == 0 {
			return list
		}
		out := make([]wfdef.HookDecl, len(list))
		for i, d := range list {
			if d.TimeoutSeconds <= 0 {
				d.TimeoutSeconds = hook.DefaultTimeoutSeconds
			}
			if d.TimeoutSeconds > maxSeconds {
				d.TimeoutSeconds = maxSeconds
			}
			out[i] = d
		}
		return out
	}
	return hook.Chain{Global: apply(c.Global), Workflow: apply(c.Workflow), Node: apply(c.Node)}
}

// Empty reports a chain with nothing to run.
func Empty(c hook.Chain) bool { return len(c.Global)+len(c.Workflow)+len(c.Node) == 0 }
