package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/internal/events"
	"github.com/rafaelp/vidyom/internal/hooks"
	"github.com/rafaelp/vidyom/internal/workflow"
	controlnodes "github.com/rafaelp/vidyom/nodes/control"
)

// runtimeDef is a job's parsed snapshot. It is immutable, cached by hash.
type runtimeDef struct {
	def   *wfdef.Definition
	graph *workflow.Graph
	specs map[string]*wfdef.NodeSpec
}

func (e *Engine) runtime(job domain.Job) (*runtimeDef, error) {
	e.runtimeMu.Lock()
	if rt, ok := e.runtimes[job.WorkflowSHA256]; ok {
		e.runtimeMu.Unlock()
		return rt, nil
	}
	e.runtimeMu.Unlock()
	def, err := wfdef.Parse(job.Snapshot)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot parse the job's snapshot")
	}
	g, problems := workflow.BuildGraph(def)
	if len(problems) > 0 {
		return nil, errs.Newf(errs.CodeInternal, "the job's snapshot is invalid: %v", problems[0])
	}
	rt := &runtimeDef{def: def, graph: g, specs: make(map[string]*wfdef.NodeSpec, len(def.Nodes))}
	for i := range def.Nodes {
		rt.specs[def.Nodes[i].ID] = &def.Nodes[i]
	}
	e.runtimeMu.Lock()
	defer e.runtimeMu.Unlock()
	if len(e.runtimes) > 256 {
		e.runtimes = map[string]*runtimeDef{}
	}
	e.runtimes[job.WorkflowSHA256] = rt
	return rt, nil
}

// suspendState is what a waiting node keeps in Output (I146).
type suspendState struct {
	Reason     string      `json:"reason"`
	ApprovalID id.Friendly `json:"approval_id,omitempty"`
}

// failure is one failed attempt.
type failure struct {
	Code      string
	Message   string
	Retryable bool
	Unknown   bool
}

func failureOf(err error) failure {
	coded := errs.Public(err)
	return failure{
		Code:      string(coded.Code),
		Message:   coded.Message,
		Retryable: node.IsTransient(err),
		Unknown:   node.IsUnknownOutcome(err),
	}
}

// jobRun is one look at one job, under its lock.
type jobRun struct {
	e        *Engine
	ctx      context.Context
	store    repo.TenantStore
	job      domain.Job
	rt       *runtimeDef
	nodes    map[string]domain.NodeExecution
	decoded  map[string]any
	inputs   map[string]any
	event    map[string]any
	loaded   bool
	progress bool
}

// advance is the only function that moves a job.
func (e *Engine) advance(ctx context.Context, tenant domain.TenantID, ulid id.ULID) error {
	unlock := e.locks.lock(ulid)
	defer unlock()
	store, err := e.d.Tenants.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	job, err := store.Jobs().GetByULID(ctx, ulid)
	if err != nil {
		return err
	}
	if job.Status.Terminal() {
		e.checks.Delete(ulid)
		return nil
	}
	rt, err := e.runtime(job)
	if err != nil {
		return err
	}
	r := &jobRun{e: e, ctx: ctx, store: store, job: job, rt: rt, decoded: map[string]any{}}
	if err := r.loadNodes(); err != nil {
		return err
	}
	return r.step()
}

func (r *jobRun) loadNodes() error {
	list, err := r.store.Jobs().ListNodes(r.ctx, r.job.ULID)
	if err != nil {
		return err
	}
	r.nodes = make(map[string]domain.NodeExecution, len(list))
	for _, n := range list {
		r.nodes[n.NodeID] = n
	}
	return nil
}

func (r *jobRun) cancelRequested() bool {
	return strings.HasPrefix(r.job.StatusReason, reasonCancelRequested)
}

func (r *jobRun) step() error {
	if r.job.Status == domain.JobQueued && !r.cancelRequested() {
		return r.startJob()
	}
	if r.job.StartedAt == nil && r.job.Status == domain.JobRunning {
		now := r.e.now()
		r.job.StartedAt = &now
	}
	r.checkJobTimeout()
	for i := 0; i < len(r.rt.def.Nodes)*4+8; i++ {
		r.progress = false
		if err := r.pass(); err != nil {
			return err
		}
		if !r.progress {
			break
		}
	}
	return r.settle()
}

// startJob runs workflow.before and workflow.start, then moves to running. Both run
// while the job is still queued, so a crash re-runs them (I149).
func (r *jobRun) startJob() error {
	e := r.e
	before := r.chain(hook.WorkflowBefore, nil)
	start := r.chain(hook.WorkflowStart, nil)
	if hooks.Empty(before) && hooks.Empty(start) {
		return r.moveToRunning()
	}
	key := finishKey(r.job.ULID)
	if !e.claim(key) {
		return nil
	}
	tenant, job := r.store.Tenant(), r.job
	base := r.invocation(nil)
	// A job-level hook judges what the job was given, as a node's judges its parameters.
	base.Data, _ = json.Marshal(map[string]json.RawMessage{"inputs": r.job.Inputs})
	onError, onFinish := r.chain(hook.WorkflowError, nil), r.chain(hook.WorkflowFinish, nil)
	e.spawn(func(ctx context.Context) {
		defer e.Kick(tenant, job.ULID)
		defer e.release(key)
		res := error(nil)
		if !hooks.Empty(before) {
			_, res = e.d.Hooks.Dispatch(ctx, hook.WorkflowBefore, before, base)
		}
		if res == nil && !hooks.Empty(start) {
			_, res = e.d.Hooks.Dispatch(ctx, hook.WorkflowStart, start, base)
		}
		if res != nil && ctx.Err() == nil {
			// The job ends here, and that is still a terminal state a finish hook is owed
			// (§8.1, §15.7). Like every terminal hook, these run before the state is
			// written, and neither can change the outcome (I149).
			ended := base
			ended.Data, _ = json.Marshal(map[string]any{"outcome": domain.JobFailed})
			if !hooks.Empty(onError) {
				_, _ = e.d.Hooks.Dispatch(ctx, hook.WorkflowError, onError, ended)
			}
			if !hooks.Empty(onFinish) {
				_, _ = e.d.Hooks.Dispatch(ctx, hook.WorkflowFinish, onFinish, ended)
			}
		}
		if err := e.afterStartHooks(ctx, tenant, job.ULID, res); err != nil {
			e.d.Logger.Warn("job start failed", "job_id", job.ID, "error", err)
		}
	})
	return nil
}

func (e *Engine) afterStartHooks(ctx context.Context, tenant domain.TenantID, ulid id.ULID, hookErr error) error {
	unlock := e.locks.lock(ulid)
	defer unlock()
	store, err := e.d.Tenants.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	job, err := store.Jobs().GetByULID(ctx, ulid)
	if err != nil {
		return err
	}
	if job.Status != domain.JobQueued {
		return nil
	}
	rt, err := e.runtime(job)
	if err != nil {
		return err
	}
	r := &jobRun{e: e, ctx: ctx, store: store, job: job, rt: rt, decoded: map[string]any{}}
	if err := r.loadNodes(); err != nil {
		return err
	}
	if hookErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		je := hookError(hookErr, "")
		return r.writeTerminal(domain.JobFailed, &je, hookErr)
	}
	return r.moveToRunning()
}

func hookError(err error, nodeID string) domain.JobError {
	if b, ok := hooks.AsBlocked(err); ok {
		return domain.JobError{Code: CodeHookBlocked, Message: b.Reason, NodeID: nodeID}
	}
	return domain.JobError{Code: CodeHookBlocked, Message: errs.Public(err).Message, NodeID: nodeID}
}

func (r *jobRun) moveToRunning() error {
	now := r.e.now()
	r.job.Status = domain.JobRunning
	if r.job.StartedAt == nil {
		r.job.StartedAt = &now
	}
	if err := r.store.InTx(r.ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().Update(r.ctx, r.job); err != nil {
			return err
		}
		return r.appendEvent(tx, domain.JobEventState, "", "", map[string]any{"status": r.job.Status})
	}); err != nil {
		return err
	}
	r.e.notifyJob(r.job.ULID)
	return r.step()
}

func (r *jobRun) appendEvent(tx repo.TenantRepos, kind domain.JobEventKind, nodeID, message string, data any) error {
	var raw json.RawMessage
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			return err
		}
		raw = encoded
	}
	_, err := tx.Jobs().AppendEvent(r.ctx, domain.JobEvent{
		JobID: r.job.ID, At: r.e.now(), Kind: kind, NodeID: nodeID, Message: message, Data: raw,
	})
	return err
}

// putNode commits a node record with its event.
func (r *jobRun) putNode(n domain.NodeExecution, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	data["status"] = n.Status
	data["attempt"] = n.Attempt
	if n.Error != nil {
		data["error"] = n.Error
	}
	err := r.store.InTx(r.ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().PutNode(r.ctx, n); err != nil {
			return err
		}
		return r.appendEvent(tx, domain.JobEventNode, n.NodeID, "", data)
	})
	if err != nil {
		return err
	}
	r.nodes[n.NodeID] = n
	r.progress = true
	r.e.notifyJob(r.job.ULID)
	return nil
}

func (r *jobRun) failing() (domain.NodeExecution, bool) {
	for _, id := range r.rt.graph.Order {
		n := r.nodes[id]
		if blocksJob(n, r.rt.specs[id]) {
			return n, true
		}
	}
	return domain.NodeExecution{}, false
}

// blocksJob reports a node whose failure stops new work.
func blocksJob(n domain.NodeExecution, spec *wfdef.NodeSpec) bool {
	if n.Error != nil && n.Error.Code == CodeNeedsAttention {
		return false
	}
	switch n.Status {
	case domain.NodeFailed:
		return !spec.ContinueOnFailure
	case domain.NodeCompleted:
		return n.Error != nil && n.Error.Code == CodeHookBlocked
	}
	return false
}

func needsAttention(n domain.NodeExecution) bool {
	return n.Status == domain.NodeFailed && n.Error != nil && n.Error.Code == CodeNeedsAttention
}

func (r *jobRun) attentionNode() (domain.NodeExecution, bool) {
	for _, id := range r.rt.graph.Order {
		if needsAttention(r.nodes[id]) {
			return r.nodes[id], true
		}
	}
	return domain.NodeExecution{}, false
}

// pass looks at every node once, in topological order.
func (r *jobRun) pass() error {
	for _, nodeID := range r.rt.graph.Order {
		n := r.nodes[nodeID]
		if r.e.isBusy(nodeKey(r.job.ULID, nodeID)) {
			continue
		}
		hold := r.job.Status == domain.JobPaused || r.job.Status == domain.JobNeedsAttention
		_, attention := r.attentionNode()
		_, failing := r.failing()
		stopping := r.cancelRequested() || failing || attention
		switch n.Status {
		case domain.NodeRunning:
			if len(n.Output) > 0 {
				if err := r.finishNode(nodeID); err != nil {
					return err
				}
				continue
			}
			if n.LeaseExpiresAt != nil && !r.e.now().Before(*n.LeaseExpiresAt) {
				if err := r.onFailure(nodeID, failure{Code: CodeInterrupted, Message: "the lease expired before the node finished", Retryable: true, Unknown: true}); err != nil {
					return err
				}
			}
		case domain.NodeWaiting:
			switch {
			case r.cancelRequested() || failing:
				if err := r.stopWaiting(nodeID); err != nil {
					return err
				}
			case !hold:
				if err := r.checkWake(nodeID); err != nil {
					return err
				}
			}
		case domain.NodeQueued:
			if r.cancelRequested() {
				n.Status = domain.NodeCancelled
				if err := r.putNode(n, nil); err != nil {
					return err
				}
			} else if failing {
				// Back to pending, so job retry dispatches it with a new attempt. The old
				// pointer gets 410.
				n.Status = domain.NodePending
				if err := r.putNode(n, nil); err != nil {
					return err
				}
			}
		case domain.NodePending:
			if hold || stopping {
				continue
			}
			switch r.readiness(nodeID) {
			case verdictSkip:
				n.Status = domain.NodeSkipped
				now := r.e.now()
				n.FinishedAt = &now
				if err := r.putNode(n, nil); err != nil {
					return err
				}
			case verdictRun:
				if err := r.startNode(nodeID); err != nil {
					return err
				}
			}
		case domain.NodeReady:
			if hold || stopping {
				continue
			}
			if n.LeaseExpiresAt == nil || !r.e.now().Before(*n.LeaseExpiresAt) {
				if err := r.startNode(nodeID); err != nil {
					return err
				}
			}
		}
		// The budget can pause the job in the middle of a pass (I33).
		if r.job.Status == domain.JobPaused && !hold {
			return nil
		}
	}
	return nil
}

type verdict int

const (
	verdictWait verdict = iota
	verdictRun
	verdictSkip
)

// flowIfFalse reports a flow.if whose branch was not taken (D33, I140).
func (r *jobRun) flowIfFalse(nodeID string) bool {
	if r.rt.specs[nodeID].Type != wfdef.TypeFlowIf {
		return false
	}
	n := r.nodes[nodeID]
	if n.Status != domain.NodeCompleted {
		return false
	}
	var out struct {
		Result bool `json:"result"`
	}
	return json.Unmarshal(n.Output, &out) == nil && !out.Result
}

type inputState int

const (
	inputWaiting inputState = iota
	inputDone
	inputSkipped
)

func (r *jobRun) inputState(pred string) inputState {
	n := r.nodes[pred]
	switch n.Status {
	case domain.NodeCompleted:
		if n.Error != nil {
			return inputWaiting
		}
		if r.flowIfFalse(pred) {
			return inputSkipped
		}
		return inputDone
	case domain.NodeSkipped:
		return inputSkipped
	case domain.NodeFailed:
		if r.rt.specs[pred].ContinueOnFailure && !needsAttention(n) {
			return inputDone
		}
	}
	return inputWaiting
}

// readiness applies D33: a node runs when all its predecessors completed; a skip
// propagates, except through flow.merge.
func (r *jobRun) readiness(nodeID string) verdict {
	preds := r.rt.graph.Preds[nodeID]
	spec := r.rt.specs[nodeID]
	if spec.Type == wfdef.TypeFlowMerge {
		var p struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(spec.Parameters, &p)
		done, skipped := 0, 0
		for _, pred := range preds {
			switch r.inputState(pred) {
			case inputDone:
				done++
			case inputSkipped:
				skipped++
			}
		}
		switch {
		case p.Mode == "any" && done > 0:
			return verdictRun
		case done+skipped < len(preds):
			return verdictWait
		case done == 0 && len(preds) > 0:
			return verdictSkip
		default:
			return verdictRun
		}
	}
	waiting := false
	for _, pred := range preds {
		switch r.inputState(pred) {
		case inputSkipped:
			return verdictSkip
		case inputWaiting:
			waiting = true
		}
	}
	if waiting {
		return verdictWait
	}
	return verdictRun
}

// ---- data for references and conditions ----

func (r *jobRun) inputsMap() map[string]any {
	if r.inputs != nil {
		return r.inputs
	}
	r.inputs = map[string]any{}
	if len(r.job.Inputs) > 0 {
		if v, err := workflow.DecodeJSON(r.job.Inputs); err == nil {
			if m, ok := v.(map[string]any); ok {
				r.inputs = m
			}
		}
	}
	return r.inputs
}

func (r *jobRun) eventMap() map[string]any {
	if r.loaded {
		return r.event
	}
	r.loaded = true
	if r.job.Trigger.EventID == "" {
		return nil
	}
	e, err := r.store.Outbox().GetEvent(r.ctx, id.ULID(r.job.Trigger.EventID))
	if err != nil {
		r.e.d.Logger.Warn("the job's triggering event is gone", "job_id", r.job.ID, "error", err)
		return nil
	}
	if m, err := events.EnvelopeMap(e); err == nil {
		r.event = m
	}
	return r.event
}

func (r *jobRun) jobMap() map[string]any {
	return map[string]any{
		"id": string(r.job.ID), "ulid": string(r.job.ULID), "workflow": r.job.WorkflowID,
		"workflow_version": r.job.WorkflowVersion, "tenant_id": string(r.job.TenantID), "trace_id": r.job.TraceID,
	}
}

// scopeFor builds the data of a set of nodes. Outputs can be megabytes (I35), so only
// what a node actually reads is decoded.
func (r *jobRun) scopeFor(nodeIDs []string) workflow.Scope {
	scope := workflow.Scope{Inputs: r.inputsMap(), Event: r.eventMap(), Job: r.jobMap(), Nodes: map[string]workflow.NodeData{}}
	for _, nodeID := range nodeIDs {
		n, ok := r.nodes[nodeID]
		if !ok {
			continue
		}
		data := workflow.NodeData{Status: string(n.Status)}
		if n.Error != nil {
			data.ErrorCode = n.Error.Code
		}
		if n.Status == domain.NodeCompleted || (n.Status == domain.NodeRunning && len(n.Output) > 0) {
			if v, seen := r.decoded[nodeID]; seen {
				data.Output = v
			} else if len(n.Output) > 0 {
				if v, err := workflow.DecodeJSON(n.Output); err == nil {
					r.decoded[nodeID] = v
					data.Output = v
				}
			}
		}
		scope.Nodes[nodeID] = data
	}
	return scope
}

func (r *jobRun) refsOf(raw json.RawMessage, skip map[string]bool) []string {
	v, err := workflow.DecodeJSON(raw)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	_ = workflow.CollectRefs(v, skip, func(ref workflow.Ref) {
		if ref.Kind == workflow.RefNodes && !seen[ref.Node] {
			seen[ref.Node] = true
			out = append(out, ref.Node)
		}
	})
	return out
}

// ---- starting a node ----

func (r *jobRun) startNode(nodeID string) error {
	n := r.nodes[nodeID]
	spec := r.rt.specs[nodeID]
	desc, ok := r.e.descriptor(spec.Type, spec.TypeVersion)
	if !ok {
		return r.onFailure(nodeID, failure{Code: string(errs.CodeInternal), Message: "unknown node type " + spec.Type})
	}
	params := spec.Parameters
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	skip := workflow.SkipKeys(spec.Type)
	resolved, err := workflow.ResolveParameters(params, r.scopeFor(r.refsOf(params, skip)), skip)
	if err != nil {
		return r.onFailure(nodeID, failure{Code: CodeReference, Message: "node " + nodeID + ": " + err.Error()})
	}
	if err := workflow.ValidateResolved(desc, resolved); err != nil {
		return r.onFailure(nodeID, failure{Code: string(errs.CodeValidationFailed), Message: errs.Public(err).Message})
	}
	if desc.Paid && r.e.d.Budgets != nil {
		if err := r.e.d.Budgets.Check(r.ctx, r.store.Tenant()); err != nil {
			if errs.IsCode(err, errs.CodeBudgetExceeded) {
				return r.pauseForBudget(nodeID)
			}
			r.e.d.Logger.Warn("budget check failed; holding the node", "job_id", r.job.ID, "node_id", nodeID, "error", err)
			return nil
		}
	}
	chain := r.chain(hook.NodeBefore, spec)
	if hooks.Empty(chain) {
		return r.dispatch(nodeID, desc, resolved)
	}
	key := nodeKey(r.job.ULID, nodeID)
	if !r.e.claim(key) {
		return nil
	}
	e, tenant, ulid := r.e, r.store.Tenant(), r.job.ULID
	inv := r.invocation(&n)
	inv.Data, _ = json.Marshal(map[string]json.RawMessage{"parameters": resolved})
	e.spawn(func(ctx context.Context) {
		defer e.Kick(tenant, ulid)
		defer e.release(key) // held until the result is written, or a look at the job would run the hooks again
		res, hookErr := e.d.Hooks.Dispatch(ctx, hook.NodeBefore, chain, inv)
		if ctx.Err() != nil {
			return
		}
		if err := e.afterNodeBefore(ctx, tenant, ulid, nodeID, desc, resolved, res, hookErr); err != nil {
			e.d.Logger.Warn("node start failed", "job", ulid, "node_id", nodeID, "error", err)
		}
	})
	return nil
}

func (e *Engine) afterNodeBefore(ctx context.Context, tenant domain.TenantID, ulid id.ULID, nodeID string, desc node.Descriptor, resolved json.RawMessage, res hook.Result, hookErr error) error {
	unlock := e.locks.lock(ulid)
	defer unlock()
	store, err := e.d.Tenants.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	job, err := store.Jobs().GetByULID(ctx, ulid)
	if err != nil {
		return err
	}
	if job.Status.Terminal() {
		return nil
	}
	rt, err := e.runtime(job)
	if err != nil {
		return err
	}
	r := &jobRun{e: e, ctx: ctx, store: store, job: job, rt: rt, decoded: map[string]any{}}
	if err := r.loadNodes(); err != nil {
		return err
	}
	if hookErr != nil {
		je := hookError(hookErr, nodeID)
		if err := r.auditHook(hook.NodeBefore, nodeID, je.Message); err != nil {
			return err
		}
		return r.failNode(nodeID, failure{Code: je.Code, Message: je.Message})
	}
	params := resolved
	if len(res.Parameters) > 0 {
		// A hook that changes parameters goes through validation and authorization
		// again (§8.1).
		if err := workflow.ValidateResolved(desc, res.Parameters); err != nil {
			return r.failNode(nodeID, failure{Code: CodeHookBlocked, Message: "a hook returned invalid parameters: " + errs.Public(err).Message})
		}
		spec := *rt.specs[nodeID]
		spec.Parameters = res.Parameters
		for _, p := range workflow.NodePermissions(&spec) {
			if p == domain.PermPublicationCreate {
				continue // authorized by the gate or at submission
			}
			if !r.hasPermission(p) {
				return r.failNode(nodeID, failure{Code: CodeHookBlocked, Message: "a hook returned parameters needing " + string(p) + ", which the job does not hold"})
			}
		}
		params = res.Parameters
	}
	return r.dispatch(nodeID, desc, params)
}

func (r *jobRun) hasPermission(p domain.Permission) bool {
	for _, have := range r.job.Permissions {
		if have == p {
			return true
		}
	}
	return false
}

func (r *jobRun) dispatch(nodeID string, desc node.Descriptor, params json.RawMessage) error {
	n := r.nodes[nodeID]
	now := r.e.now()
	n.Attempt++
	n.Parameters = params
	n.IdempotencyKey = node.IdempotencyKey(r.store.Tenant(), r.job.ULID, nodeID)
	n.Output = nil
	if n.Error == nil || n.Error.Code != CodeResolvedRetry {
		n.Error = nil
	}
	n.LeaseID, n.WorkerID = "", ""
	n.LeaseExpiresAt = nil
	n.FinishedAt = nil
	n.ProgressPercent = 0
	if desc.Pool == node.PoolEngine {
		n.Status = domain.NodeRunning
		n.StartedAt = &now
		n.TaskID = ""
		r.nodes[nodeID] = n
		return r.runInline(nodeID, desc)
	}
	n.Status = domain.NodeQueued
	n.TaskID = jobqueue.TaskID(r.job.ULID, nodeID, n.Attempt)
	if err := r.putNode(n, nil); err != nil {
		return err
	}
	if err := r.e.enqueue(r.ctx, r.store.Tenant(), r.job, n); err != nil {
		// The node is durably queued: reconciliation retries the pointer (D4).
		r.e.d.Logger.Warn("cannot enqueue; reconciliation will", "job_id", r.job.ID, "node_id", nodeID, "error", err)
	}
	return nil
}

func (r *jobRun) runInline(nodeID string, desc node.Descriptor) error {
	impl, ok := r.e.d.Nodes.Lookup(desc.Type, desc.TypeVersion)
	if !ok {
		return r.failNode(nodeID, failure{Code: string(errs.CodeInternal), Message: "node type " + desc.Type + " is not registered"})
	}
	n := r.nodes[nodeID]
	env := r.controlEnv(nodeID)
	in := node.Input{Parameters: n.Parameters, Attempt: n.Attempt, IdempotencyKey: n.IdempotencyKey}
	res, err := safeExecute(r.ctx, impl, env, in)
	return r.applyResult(nodeID, res, err)
}

func safeExecute(ctx context.Context, impl node.Node, env node.Env, in node.Input) (res node.Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = node.Permanent(errs.Newf(errs.CodeInternal, "node panicked: %v", p))
		}
	}()
	return impl.Execute(ctx, env, in)
}

func safeResume(ctx context.Context, impl node.Resumer, env node.Env, in node.Input) (res node.Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = node.Permanent(errs.Newf(errs.CodeInternal, "node panicked: %v", p))
		}
	}()
	return impl.Resume(ctx, env, in, nil)
}

// applyResult records what a node returned: a suspension, an output or a failure.
func (r *jobRun) applyResult(nodeID string, res node.Result, err error) error {
	if err != nil {
		// The cause stays in the daemon's log; what reaches the job is the public part.
		r.e.d.Logger.Warn("node failed", "tenant_id", r.store.Tenant(), "job_id", r.job.ID, "node_id", nodeID, "attempt", r.nodes[nodeID].Attempt, "error", err)
		return r.onFailure(nodeID, failureOf(err))
	}
	if res.Suspend != nil {
		return r.suspend(nodeID, res.Suspend)
	}
	return r.succeed(nodeID, res.Output, res.Tools)
}

func (r *jobRun) suspend(nodeID string, s *node.Suspension) error {
	n := r.nodes[nodeID]
	state, err := json.Marshal(suspendState{Reason: s.Reason, ApprovalID: s.ApprovalID})
	if err != nil {
		return err
	}
	changed := n.Status != domain.NodeWaiting || string(n.Output) != string(state)
	n.Status = domain.NodeWaiting
	n.Output = state
	n.LeaseExpiresAt = nil
	if s.ResumeAfter > 0 {
		at := time.Unix(s.ResumeAfter, 0).UTC()
		n.LeaseExpiresAt = &at
	}
	if !changed {
		return nil
	}
	err = r.store.InTx(r.ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().PutNode(r.ctx, n); err != nil {
			return err
		}
		data := map[string]any{"status": n.Status, "reason": s.Reason}
		kind := domain.JobEventNode
		if s.Reason == "approval" {
			kind = domain.JobEventApproval
			data["approval_id"] = s.ApprovalID
		}
		return r.appendEvent(tx, kind, nodeID, "", data)
	})
	if err != nil {
		return err
	}
	r.nodes[nodeID] = n
	r.progress = true
	r.e.notifyJob(r.job.ULID)
	return nil
}

func (r *jobRun) succeed(nodeID string, output json.RawMessage, tools map[string]string) error {
	n := r.nodes[nodeID]
	if len(output) == 0 {
		output = json.RawMessage(`{}`)
	}
	if len(output) > domain.MaxNodeOutputBytes {
		return r.failNode(nodeID, failure{Code: string(errs.CodeOutputTooLarge), Message: fmt.Sprintf("node output is %d bytes; the limit is %d (I35)", len(output), domain.MaxNodeOutputBytes)})
	}
	if !json.Valid(output) {
		return r.failNode(nodeID, failure{Code: string(errs.CodeInternal), Message: "node output is not valid JSON"})
	}
	n.Output = output
	n.Status = domain.NodeRunning
	data := map[string]any{"stage": "output_stored"}
	if len(tools) > 0 {
		data["tools"] = tools
	}
	spec := r.rt.specs[nodeID]
	if hooks.Empty(r.chain(hook.NodeAfter, spec)) && hooks.Empty(r.chain(hook.NodeFinish, spec)) {
		return r.complete(n, tools, nil)
	}
	if err := r.putNode(n, data); err != nil {
		return err
	}
	return r.finishNode(nodeID)
}

// finishNode runs node.after and node.finish on a node whose output is stored; then
// it becomes completed (I149).
func (r *jobRun) finishNode(nodeID string) error {
	n := r.nodes[nodeID]
	spec := r.rt.specs[nodeID]
	after := r.chain(hook.NodeAfter, spec)
	finish := r.chain(hook.NodeFinish, spec)
	if hooks.Empty(after) && hooks.Empty(finish) {
		return r.complete(n, nil, nil)
	}
	key := nodeKey(r.job.ULID, nodeID)
	e := r.e
	if !e.claim(key) {
		return nil
	}
	tenant, ulid := r.store.Tenant(), r.job.ULID
	base := r.invocation(&n)
	e.spawn(func(ctx context.Context) {
		defer e.Kick(tenant, ulid)
		defer e.release(key) // held until the checkpoint is written
		var blocked error
		if !hooks.Empty(after) {
			_, blocked = e.d.Hooks.Dispatch(ctx, hook.NodeAfter, after, base)
		}
		if !hooks.Empty(finish) {
			if _, err := e.d.Hooks.Dispatch(ctx, hook.NodeFinish, finish, base); err != nil {
				e.d.Logger.Warn("node.finish hook failed", "job", ulid, "node_id", nodeID, "error", err)
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err := e.afterNodeHooks(ctx, tenant, ulid, nodeID, blocked); err != nil {
			e.d.Logger.Warn("cannot finish node", "job", ulid, "node_id", nodeID, "error", err)
		}
	})
	return nil
}

func (e *Engine) afterNodeHooks(ctx context.Context, tenant domain.TenantID, ulid id.ULID, nodeID string, blocked error) error {
	unlock := e.locks.lock(ulid)
	defer unlock()
	store, err := e.d.Tenants.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	job, err := store.Jobs().GetByULID(ctx, ulid)
	if err != nil {
		return err
	}
	rt, err := e.runtime(job)
	if err != nil {
		return err
	}
	r := &jobRun{e: e, ctx: ctx, store: store, job: job, rt: rt, decoded: map[string]any{}}
	if err := r.loadNodes(); err != nil {
		return err
	}
	n := r.nodes[nodeID]
	if n.Status != domain.NodeRunning || len(n.Output) == 0 {
		return nil
	}
	var nodeErr *domain.JobError
	if blocked != nil {
		je := hookError(blocked, nodeID)
		nodeErr = &je
		if err := r.auditHook(hook.NodeAfter, nodeID, je.Message); err != nil {
			return err
		}
	}
	return r.complete(n, nil, nodeErr)
}

// complete writes the checkpoint (D11).
func (r *jobRun) complete(n domain.NodeExecution, tools map[string]string, nodeErr *domain.JobError) error {
	now := r.e.now()
	n.Status = domain.NodeCompleted
	n.FinishedAt = &now
	n.ProgressPercent = 100
	n.LeaseExpiresAt = nil
	n.Error = nodeErr
	data := map[string]any{"status": n.Status, "attempt": n.Attempt}
	if len(tools) > 0 {
		data["tools"] = tools
	}
	if nodeErr != nil {
		data["error"] = nodeErr
	}
	setAsset := n.NodeType == "catalog.register" && r.job.AssetID == ""
	if setAsset {
		var out struct {
			ID id.Friendly `json:"id"`
		}
		if json.Unmarshal(n.Output, &out) == nil && out.ID != "" {
			r.job.AssetID = out.ID
		} else {
			setAsset = false
		}
	}
	err := r.store.InTx(r.ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().PutNode(r.ctx, n); err != nil {
			return err
		}
		if setAsset {
			if err := tx.Jobs().Update(r.ctx, r.job); err != nil {
				return err
			}
		}
		return r.appendEvent(tx, domain.JobEventNode, n.NodeID, "", data)
	})
	if err != nil {
		return err
	}
	r.nodes[n.NodeID] = n
	delete(r.decoded, n.NodeID)
	r.progress = true
	r.e.notifyJob(r.job.ULID)
	if n.StartedAt != nil {
		r.e.d.Metrics.Observe(telemetry.NodeDuration, now.Sub(*n.StartedAt).Seconds(), telemetry.Labels{"node_type": n.NodeType, "tenant_id": string(r.store.Tenant())})
	}
	return nil
}

// ---- failures ----

func (r *jobRun) retryLimit(spec *wfdef.NodeSpec) int {
	if spec.Retry != nil {
		return *spec.Retry
	}
	if s := r.rt.def.Settings; s != nil && s.Retry != nil {
		return *s.Retry
	}
	return DefaultRetries
}

func (r *jobRun) backoff(attempt int) time.Duration {
	base := r.e.d.DefaultBackoff
	if s := r.rt.def.Settings; s != nil && s.RetryBackoffSeconds != nil {
		base = time.Duration(*s.RetryBackoffSeconds) * time.Second
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= MaxBackoff {
			return MaxBackoff
		}
	}
	if d > MaxBackoff {
		return MaxBackoff
	}
	return d
}

// onFailure applies the side-effect class (D11, I148). Hooks can observe a failure
// but never change this decision (§8.1).
func (r *jobRun) onFailure(nodeID string, f failure) error {
	spec := r.rt.specs[nodeID]
	desc, ok := r.e.descriptor(spec.Type, spec.TypeVersion)
	if !ok {
		return r.failNode(nodeID, f)
	}
	if f.Code == CodeCancelled {
		if r.cancelRequested() {
			n := r.nodes[nodeID]
			n.Status = domain.NodeCancelled
			n.Error = &domain.JobError{Code: f.Code, Message: f.Message, NodeID: nodeID}
			now := r.e.now()
			n.FinishedAt = &now
			n.LeaseExpiresAt = nil
			return r.putNode(n, nil)
		}
		// Nobody asked for it, so a worker stopped it for its own reasons: an
		// interruption like any other.
		f.Retryable = true
	}
	retryable := f.Retryable || f.Unknown
	switch desc.SideEffect {
	case node.EffectUnsafe:
		return r.toAttention(nodeID, f)
	case node.EffectReconcilable:
		if !retryable {
			return r.failNode(nodeID, f)
		}
		if desc.Pool != node.PoolDaemon {
			// The worker protocol has no reconcile call, so nothing can check (I148).
			return r.toAttention(nodeID, f)
		}
	default:
		if !retryable {
			return r.failNode(nodeID, f)
		}
	}
	n := r.nodes[nodeID]
	if n.Attempt > r.retryLimit(spec) {
		if desc.SideEffect == node.EffectReconcilable && f.Unknown {
			return r.toAttention(nodeID, f)
		}
		return r.failNode(nodeID, f)
	}
	next := r.e.now().Add(r.backoff(n.Attempt))
	n.Status = domain.NodeReady
	n.Error = &domain.JobError{Code: f.Code, Message: f.Message, NodeID: nodeID}
	n.LeaseExpiresAt = &next
	n.LeaseID, n.WorkerID = "", ""
	r.fireNodeHooks(nodeID, hook.NodeError)
	r.e.d.Metrics.Add(telemetry.NodeRetries, 1, telemetry.Labels{"node_type": n.NodeType, "tenant_id": string(r.store.Tenant())})
	return r.putNode(n, map[string]any{"next_attempt": next})
}

func (r *jobRun) toAttention(nodeID string, f failure) error {
	n := r.nodes[nodeID]
	now := r.e.now()
	n.Status = domain.NodeFailed
	n.Error = &domain.JobError{Code: CodeNeedsAttention, Message: f.Message, NodeID: nodeID}
	n.FinishedAt = &now
	n.LeaseExpiresAt = nil
	r.fireNodeHooks(nodeID, hook.NodeError, hook.NodeFinish)
	return r.putNode(n, map[string]any{"cause": f.Code, "outcome_unknown": f.Unknown})
}

func (r *jobRun) failNode(nodeID string, f failure) error {
	n := r.nodes[nodeID]
	now := r.e.now()
	n.Status = domain.NodeFailed
	n.Error = &domain.JobError{Code: f.Code, Message: f.Message, NodeID: nodeID}
	n.FinishedAt = &now
	n.LeaseExpiresAt = nil
	r.fireNodeHooks(nodeID, hook.NodeError, hook.NodeFinish)
	r.e.d.Metrics.Add(telemetry.NodeFailures, 1, telemetry.Labels{"node_type": n.NodeType, "tenant_id": string(r.store.Tenant())})
	return r.putNode(n, nil)
}

// fireNodeHooks runs reporting hooks in the background. They can never block (I149).
func (r *jobRun) fireNodeHooks(nodeID string, points ...hook.Point) {
	n := r.nodes[nodeID]
	spec := r.rt.specs[nodeID]
	type planned struct {
		point hook.Point
		chain hook.Chain
	}
	var plan []planned
	for _, p := range points {
		if c := r.chain(p, spec); !hooks.Empty(c) {
			plan = append(plan, planned{p, c})
		}
	}
	if len(plan) == 0 {
		return
	}
	base := r.invocation(&n)
	e := r.e
	e.spawn(func(ctx context.Context) {
		for _, p := range plan {
			if _, err := e.d.Hooks.Dispatch(ctx, p.point, p.chain, base); err != nil {
				e.d.Logger.Warn("reporting hook failed", "point", string(p.point), "node_id", nodeID, "error", err)
			}
		}
	})
}

// ---- waits ----

func (r *jobRun) checkWake(nodeID string) error {
	n := r.nodes[nodeID]
	var s suspendState
	_ = json.Unmarshal(n.Output, &s)
	switch s.Reason {
	case "approval":
		a, found, err := r.store.Approvals().GetForNode(r.ctx, r.job.ULID, nodeID)
		if err != nil {
			return err
		}
		if found && a.Status == domain.ApprovalPending {
			return r.checkApprovalTimeout(nodeID, a)
		}
	case controlnodes.ReasonSubworkflow:
		// Resume decides by looking at the children; it re-suspends when nothing changed.
	default:
		if n.LeaseExpiresAt != nil && r.e.now().Before(*n.LeaseExpiresAt) {
			return nil
		}
	}
	impl, ok := r.e.d.Nodes.Lookup(n.NodeType, n.TypeVersion)
	if !ok {
		return r.failNode(nodeID, failure{Code: string(errs.CodeInternal), Message: "node type " + n.NodeType + " is not registered"})
	}
	resumer, ok := impl.(node.Resumer)
	if !ok {
		return r.failNode(nodeID, failure{Code: string(errs.CodeInternal), Message: "node type " + n.NodeType + " suspended but cannot resume"})
	}
	in := node.Input{Parameters: n.Parameters, Attempt: n.Attempt, IdempotencyKey: n.IdempotencyKey}
	res, err := safeResume(r.ctx, resumer, r.controlEnv(nodeID), in)
	return r.applyResult(nodeID, res, err)
}

func (r *jobRun) checkApprovalTimeout(nodeID string, a domain.Approval) error {
	now := r.e.now()
	if a.ExpiresAt == nil || now.Before(*a.ExpiresAt) {
		return nil
	}
	switch a.OnTimeout {
	case domain.OnTimeoutRemind:
		n := r.nodes[nodeID]
		if n.LeaseExpiresAt != nil && now.Before(*n.LeaseExpiresAt) {
			return nil
		}
		period := time.Duration(a.TimeoutSeconds) * time.Second
		if period <= 0 {
			period = time.Hour
		}
		next := now.Add(period)
		n.LeaseExpiresAt = &next
		err := r.store.InTx(r.ctx, func(tx repo.TenantRepos) error {
			if err := tx.Jobs().PutNode(r.ctx, n); err != nil {
				return err
			}
			_, err := events.Append(r.ctx, tx, r.store.Tenant(), events.JobSource(r.job), event.Draft{
				Type: event.ApprovalRequested, AssetID: string(a.AssetID),
				Payload: map[string]any{"approval_id": a.ID, "kind": a.Kind, "node_id": a.NodeID, "reminder": true},
			}, now)
			return err
		})
		if err != nil {
			return err
		}
		r.nodes[nodeID] = n
		r.e.d.Bus.Notify()
		return nil
	case domain.OnTimeoutReject:
		return r.decideByTimeout(nodeID, a, domain.ApprovalRejected, "no decision before the timeout")
	default:
		return r.decideByTimeout(nodeID, a, domain.ApprovalExpired, "no decision before the timeout")
	}
}

func (r *jobRun) decideByTimeout(nodeID string, a domain.Approval, status domain.ApprovalStatus, reason string) error {
	now := r.e.now()
	err := r.store.InTx(r.ctx, func(tx repo.TenantRepos) error {
		if _, err := tx.Approvals().Decide(r.ctx, id.Ref(a.ID), repo.ApprovalDecision{Status: status, DecidedBy: "system", DecidedAt: now, Reason: reason}); err != nil {
			return err
		}
		_, err := events.Append(r.ctx, tx, r.store.Tenant(), events.JobSource(r.job), event.Draft{
			Type: event.ApprovalDecided, AssetID: string(a.AssetID),
			Payload: map[string]any{"approval_id": a.ID, "kind": a.Kind, "decision": status, "decided_by": "system"},
		}, now)
		return err
	})
	if err != nil {
		return err
	}
	r.e.d.Bus.Notify()
	r.progress = true
	return nil
}

// stopWaiting ends a suspended node when the job is cancelled or failing. Approvals
// expire, so nobody decides a gate no job waits for (I158). Child jobs are cancelled,
// and the node ends when they have.
func (r *jobRun) stopWaiting(nodeID string) error {
	n := r.nodes[nodeID]
	var s suspendState
	_ = json.Unmarshal(n.Output, &s)
	switch s.Reason {
	case "approval":
		a, found, err := r.store.Approvals().GetForNode(r.ctx, r.job.ULID, nodeID)
		if err != nil {
			return err
		}
		if found && a.Status == domain.ApprovalPending {
			if err := r.decideByTimeout(nodeID, a, domain.ApprovalExpired, "the job stopped"); err != nil {
				return err
			}
		}
	case controlnodes.ReasonSubworkflow:
		children, err := r.childJobs(nodeID)
		if err != nil {
			return err
		}
		live := false
		for _, c := range children {
			if !c.Status.Terminal() {
				live = true
				if err := r.e.requestCancel(r.ctx, r.store, c.ULID, "the parent job stopped", ""); err != nil {
					r.e.d.Logger.Warn("cannot cancel child job", "job_id", c.ID, "error", err)
				}
			}
		}
		if live {
			return nil
		}
	}
	n.Status = domain.NodeCancelled
	now := r.e.now()
	n.FinishedAt = &now
	n.LeaseExpiresAt = nil
	n.Output = nil
	return r.putNode(n, nil)
}

func (r *jobRun) checkJobTimeout() {
	s := r.rt.def.Settings
	if s == nil || s.TimeoutSeconds == nil || r.job.StartedAt == nil || r.cancelRequested() {
		return
	}
	if r.job.Status == domain.JobPaused || r.job.Status == domain.JobNeedsAttention {
		return
	}
	now := r.e.now()
	elapsed := now.Sub(*r.job.StartedAt)
	// Time a person takes to decide does not count (I151).
	for nodeID, n := range r.nodes {
		if r.rt.specs[nodeID] == nil || r.rt.specs[nodeID].Type != wfdef.TypeApprovalRequest || n.StartedAt == nil {
			continue
		}
		switch {
		case n.Status == domain.NodeWaiting:
			elapsed -= now.Sub(*n.StartedAt)
		case n.FinishedAt != nil:
			elapsed -= n.FinishedAt.Sub(*n.StartedAt)
		}
	}
	if elapsed > time.Duration(*s.TimeoutSeconds)*time.Second {
		r.job.StatusReason = reasonTimeout
		r.progress = true
	}
}

// ---- settling the job ----

// settle decides the job's state from its nodes.
func (r *jobRun) settle() error {
	var active, waiting, retrying, approvals, prepared bool
	allDone := true
	for _, nodeID := range r.rt.graph.Order {
		n := r.nodes[nodeID]
		if r.e.isBusy(nodeKey(r.job.ULID, nodeID)) {
			prepared = true
		}
		switch n.Status {
		case domain.NodeQueued, domain.NodeRunning:
			active = true
			allDone = false
		case domain.NodeWaiting:
			waiting = true
			allDone = false
			var s suspendState
			_ = json.Unmarshal(n.Output, &s)
			if s.Reason == "approval" {
				approvals = true
			} else if s.Reason == controlnodes.ReasonSubworkflow {
				if children, err := r.childJobs(nodeID); err == nil {
					for _, c := range children {
						if c.Status == domain.JobWaitingApproval {
							approvals = true
						}
					}
				}
			}
		case domain.NodeReady:
			retrying = true
			allDone = false
		case domain.NodePending:
			allDone = false
		}
	}
	active = active || prepared

	if r.cancelRequested() {
		if active || waiting {
			return r.saveJob()
		}
		if r.job.StatusReason == reasonTimeout {
			je := domain.JobError{Code: CodeTimeout, Message: "the job passed its timeoutSeconds"}
			return r.finalize(domain.JobFailed, &je)
		}
		return r.finalize(domain.JobCancelled, nil)
	}
	if n, ok := r.attentionNode(); ok {
		return r.toNeedsAttention(n)
	}
	if n, failing := r.failing(); failing {
		if active || waiting {
			return r.saveJob()
		}
		je := n.Error
		if je == nil {
			je = &domain.JobError{Code: "failed", Message: "node " + n.NodeID + " failed", NodeID: n.NodeID}
		}
		return r.finalize(domain.JobFailed, je)
	}
	if r.job.Status == domain.JobPaused || r.job.Status == domain.JobNeedsAttention {
		return r.saveJob()
	}
	if allDone && !prepared {
		return r.finalize(domain.JobCompleted, nil)
	}
	switch {
	case active:
		r.job.Status = domain.JobRunning
	case retrying:
		r.job.Status = domain.JobRetrying
	case approvals:
		r.job.Status = domain.JobWaitingApproval
	default:
		r.job.Status = domain.JobRunning
	}
	return r.saveJob()
}

// steps returns the transitions from one status to another (I146): direct when the
// state machine allows it, otherwise through running.
func steps(from, to domain.JobStatus) []domain.JobStatus {
	if from == to {
		return nil
	}
	if from.CanTransition(to) {
		return []domain.JobStatus{to}
	}
	if from.CanTransition(domain.JobRunning) && domain.JobRunning.CanTransition(to) {
		return []domain.JobStatus{domain.JobRunning, to}
	}
	if from == domain.JobPaused && domain.JobQueued.CanTransition(to) {
		return []domain.JobStatus{domain.JobQueued, to}
	}
	return []domain.JobStatus{to}
}

func (r *jobRun) progressPercent() float64 {
	total := len(r.rt.graph.Order)
	if total == 0 {
		return 100
	}
	var sum float64
	for _, nodeID := range r.rt.graph.Order {
		n := r.nodes[nodeID]
		switch n.Status {
		case domain.NodeCompleted, domain.NodeSkipped, domain.NodeFailed, domain.NodeCancelled:
			sum += 100
		default:
			sum += n.ProgressPercent
		}
	}
	return sum / float64(total)
}

// saveJob writes the job when something changed, and remembers when to look again.
func (r *jobRun) saveJob() error {
	current, err := r.store.Jobs().GetByULID(r.ctx, r.job.ULID)
	if err != nil {
		return err
	}
	progress := r.progressPercent()
	changed := current.Status != r.job.Status || current.StatusReason != r.job.StatusReason ||
		timeChanged(current.StartedAt, r.job.StartedAt) || current.AssetID != r.job.AssetID ||
		progress-current.ProgressPercent >= 1 || current.ProgressPercent-progress >= 1
	r.job.ProgressPercent = progress
	if changed {
		if err := r.writeStatus(current.Status, r.job.Status); err != nil {
			return err
		}
		if current.Status != r.job.Status {
			r.e.notifyJob(r.job.ULID)
			r.kickParent()
		}
	}
	r.scheduleNextLook()
	return nil
}

func timeChanged(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a != b
	}
	return !a.Equal(*b)
}

func (r *jobRun) writeStatus(from, to domain.JobStatus) error {
	path := steps(from, to)
	return r.store.InTx(r.ctx, func(tx repo.TenantRepos) error {
		final := r.job.Status
		for _, s := range path {
			r.job.Status = s
			if err := tx.Jobs().Update(r.ctx, r.job); err != nil {
				r.job.Status = final
				return err
			}
		}
		r.job.Status = final
		if len(path) == 0 {
			if err := tx.Jobs().Update(r.ctx, r.job); err != nil {
				return err
			}
			return nil
		}
		return r.appendEvent(tx, domain.JobEventState, "", r.job.StatusReason, map[string]any{"status": final})
	})
}

func (r *jobRun) kickParent() {
	if p := r.job.Trigger.ParentJobID; p != "" {
		r.e.Kick(r.store.Tenant(), id.ULID(p))
	}
}

// scheduleNextLook tells the sweeper when this job has its next deadline (I152).
func (r *jobRun) scheduleNextLook() {
	now := r.e.now()
	next := now.Add(DefaultIdleCheck)
	for _, n := range r.nodes {
		if n.LeaseExpiresAt != nil && n.LeaseExpiresAt.Before(next) {
			next = *n.LeaseExpiresAt
		}
	}
	if r.job.Status == domain.JobWaitingApproval || r.job.Status == domain.JobRunning {
		for nodeID, n := range r.nodes {
			if n.Status != domain.NodeWaiting || r.rt.specs[nodeID] == nil || r.rt.specs[nodeID].Type != wfdef.TypeApprovalRequest {
				continue
			}
			if a, found, err := r.store.Approvals().GetForNode(r.ctx, r.job.ULID, nodeID); err == nil && found && a.ExpiresAt != nil && a.ExpiresAt.Before(next) {
				next = *a.ExpiresAt
			}
		}
	}
	if s := r.rt.def.Settings; s != nil && s.TimeoutSeconds != nil && r.job.StartedAt != nil {
		if at := r.job.StartedAt.Add(time.Duration(*s.TimeoutSeconds) * time.Second); at.After(now) && at.Before(next) {
			next = at
		}
	}
	r.e.checks.Store(r.job.ULID, next)
}

func (r *jobRun) toNeedsAttention(n domain.NodeExecution) error {
	if r.job.Status == domain.JobNeedsAttention {
		return r.saveJob()
	}
	now := r.e.now()
	from := r.job.Status
	r.job.Status = domain.JobNeedsAttention
	r.job.Attention = &domain.AttentionInfo{
		NodeID: n.NodeID, NodeType: n.NodeType, Reason: n.Error.Message, OutcomeUnknown: true, Since: now,
	}
	r.job.Error = n.Error
	err := r.store.InTx(r.ctx, func(tx repo.TenantRepos) error {
		final := r.job.Status
		for _, s := range steps(from, final) {
			r.job.Status = s
			if err := tx.Jobs().Update(r.ctx, r.job); err != nil {
				r.job.Status = final
				return err
			}
		}
		r.job.Status = final
		if err := r.appendEvent(tx, domain.JobEventState, n.NodeID, n.Error.Message, map[string]any{"status": final, "attention": r.job.Attention}); err != nil {
			return err
		}
		_, err := events.Append(r.ctx, tx, r.store.Tenant(), events.JobSource(r.job), event.Draft{
			Type: event.JobNeedsAttention, AssetID: string(r.job.AssetID),
			Payload: map[string]any{"node_id": n.NodeID, "node_type": n.NodeType, "reason": n.Error.Message},
		}, now)
		return err
	})
	if err != nil {
		return err
	}
	r.e.d.Bus.Notify()
	r.e.notifyJob(r.job.ULID)
	r.kickParent()
	r.scheduleNextLook()
	return nil
}

func (r *jobRun) pauseForBudget(nodeID string) error {
	r.job.Status = domain.JobPaused
	r.job.StatusReason = ReasonBudgetExceeded
	r.e.d.Logger.Info("job paused: monthly budget exceeded", "job_id", r.job.ID, "node_id", nodeID)
	r.progress = false
	return nil
}

// finalize runs the terminal hooks, while the job is still not terminal, and then
// writes the terminal state (I149).
func (r *jobRun) finalize(outcome domain.JobStatus, jobErr *domain.JobError) error {
	var points []hook.Point
	switch outcome {
	case domain.JobCompleted:
		points = []hook.Point{hook.WorkflowAfter, hook.WorkflowFinish}
	case domain.JobFailed:
		points = []hook.Point{hook.WorkflowError, hook.WorkflowFinish}
	default:
		points = []hook.Point{hook.WorkflowFinish}
	}
	type planned struct {
		point hook.Point
		chain hook.Chain
	}
	var plan []planned
	for _, p := range points {
		if c := r.chain(p, nil); !hooks.Empty(c) {
			plan = append(plan, planned{p, c})
		}
	}
	if len(plan) == 0 {
		return r.writeTerminal(outcome, jobErr, nil)
	}
	key := finishKey(r.job.ULID)
	e := r.e
	if !e.claim(key) {
		return r.saveJob()
	}
	base := r.invocation(nil)
	base.Data, _ = json.Marshal(map[string]any{"outcome": outcome})
	tenant, ulid := r.store.Tenant(), r.job.ULID
	e.spawn(func(ctx context.Context) {
		defer e.release(key)
		defer e.Kick(tenant, ulid)
		finalOutcome, finalErr := outcome, jobErr
		var blocked error
		for _, p := range plan {
			_, err := e.d.Hooks.Dispatch(ctx, p.point, p.chain, base)
			if ctx.Err() != nil {
				return
			}
			if err != nil && p.point == hook.WorkflowAfter && hooks.CanBlock(p.point) {
				blocked = err
				je := hookError(err, "")
				finalOutcome, finalErr = domain.JobFailed, &je
			}
		}
		if err := e.finishJob(ctx, tenant, ulid, finalOutcome, finalErr, blocked); err != nil {
			e.d.Logger.Warn("cannot finish job", "job", ulid, "error", err)
		}
	})
	return r.saveJob()
}

func (e *Engine) finishJob(ctx context.Context, tenant domain.TenantID, ulid id.ULID, outcome domain.JobStatus, jobErr *domain.JobError, blocked error) error {
	unlock := e.locks.lock(ulid)
	defer unlock()
	store, err := e.d.Tenants.ForTenant(ctx, tenant)
	if err != nil {
		return err
	}
	job, err := store.Jobs().GetByULID(ctx, ulid)
	if err != nil {
		return err
	}
	if job.Status.Terminal() {
		return nil
	}
	rt, err := e.runtime(job)
	if err != nil {
		return err
	}
	r := &jobRun{e: e, ctx: ctx, store: store, job: job, rt: rt, decoded: map[string]any{}}
	if err := r.loadNodes(); err != nil {
		return err
	}
	return r.writeTerminal(outcome, jobErr, blocked)
}

// writeTerminal commits the terminal state, its outputs and its event together
// (§8.1, §8.2).
func (r *jobRun) writeTerminal(outcome domain.JobStatus, jobErr *domain.JobError, hookErr error) error {
	now := r.e.now()
	r.job.Error = jobErr
	r.job.Attention = nil
	r.job.StatusReason = ""
	r.job.FinishedAt = &now
	if outcome == domain.JobCompleted {
		r.job.ProgressPercent = 100
		r.job.Error = nil
		r.job.Outputs = r.outputs()
	} else {
		r.job.ProgressPercent = r.progressPercent()
	}
	eventType := event.WorkflowCompleted
	switch outcome {
	case domain.JobFailed:
		eventType = event.WorkflowFailed
	case domain.JobCancelled:
		eventType = event.WorkflowCancelled
	}
	payload := map[string]any{"status": outcome}
	if jobErr != nil {
		payload["error"] = jobErr
	}
	from := r.job.Status
	err := r.store.InTx(r.ctx, func(tx repo.TenantRepos) error {
		for _, nodeID := range r.rt.graph.Order {
			n := r.nodes[nodeID]
			switch n.Status {
			case domain.NodePending, domain.NodeReady, domain.NodeQueued:
				if outcome == domain.JobCancelled {
					n.Status = domain.NodeCancelled
					if err := tx.Jobs().PutNode(r.ctx, n); err != nil {
						return err
					}
					r.nodes[nodeID] = n
				}
			}
		}
		final := outcome
		for _, s := range steps(from, final) {
			r.job.Status = s
			if err := tx.Jobs().Update(r.ctx, r.job); err != nil {
				r.job.Status = final
				return err
			}
		}
		r.job.Status = final
		if err := r.applyAssetFailure(tx, outcome); err != nil {
			return err
		}
		if hookErr != nil {
			point := hook.WorkflowBefore
			reason := errs.Public(hookErr).Message
			if b, ok := hooks.AsBlocked(hookErr); ok {
				point, reason = b.Point, b.Reason
			}
			if err := r.auditHookTx(tx, point, "", reason); err != nil {
				return err
			}
		}
		if err := r.appendEvent(tx, domain.JobEventState, "", "", map[string]any{"status": outcome, "error": jobErr}); err != nil {
			return err
		}
		_, err := events.Append(r.ctx, tx, r.store.Tenant(), events.JobSource(r.job), event.Draft{
			Type: eventType, AssetID: string(r.job.AssetID), Payload: payload,
		}, now)
		return err
	})
	if err != nil {
		return err
	}
	r.e.checks.Delete(r.job.ULID)
	r.e.d.Bus.Notify()
	r.e.notifyJob(r.job.ULID)
	r.kickParent()
	if r.job.StartedAt != nil {
		r.e.d.Metrics.Observe(telemetry.WorkflowDuration, now.Sub(*r.job.StartedAt).Seconds(), telemetry.Labels{"workflow": r.job.WorkflowID, "tenant_id": string(r.store.Tenant())})
	}
	// A failed job keeps its scratch, because job retry resumes from checkpoints that
	// may name those files (I153). The media worker's reaper is the net.
	if outcome != domain.JobFailed {
		if err := r.e.d.Queue.Enqueue(r.ctx, jobqueue.ScratchCleanupPointer(r.store.Tenant(), r.job.ULID, now)); err != nil {
			r.e.d.Logger.Warn("cannot enqueue scratch cleanup", "job_id", r.job.ID, "error", err)
		}
	}
	return nil
}

// applyAssetFailure writes settings.assetStatusOnFailure with a failed or cancelled
// job (I145).
func (r *jobRun) applyAssetFailure(tx repo.TenantRepos, outcome domain.JobStatus) error {
	s := r.rt.def.Settings
	if s == nil || s.AssetStatusOnFailure == "" || r.job.AssetID == "" || outcome == domain.JobCompleted {
		return nil
	}
	a, err := tx.Assets().Get(r.ctx, id.Ref(r.job.AssetID))
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return nil
		}
		return err
	}
	target := domain.AssetStatus(s.AssetStatusOnFailure)
	if a.Status == target || !a.Status.CanTransition(target) {
		return nil
	}
	a.Status = target
	return tx.Assets().Update(r.ctx, a)
}

// outputs resolves the workflow's outputs. An absent one is left out.
func (r *jobRun) outputs() json.RawMessage {
	if len(r.rt.def.Outputs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var read []string
	for _, tmpl := range r.rt.def.Outputs {
		parts, err := workflow.ParseTemplate(tmpl)
		if err != nil {
			continue
		}
		for _, p := range parts {
			if p.Ref != nil && p.Ref.Kind == workflow.RefNodes && !seen[p.Ref.Node] {
				seen[p.Ref.Node] = true
				read = append(read, p.Ref.Node)
			}
		}
	}
	scope := r.scopeFor(read)
	out := map[string]any{}
	for name, tmpl := range r.rt.def.Outputs {
		v, ok, err := workflow.ResolveOutput(tmpl, scope)
		if err != nil {
			r.e.d.Logger.Warn("cannot resolve a workflow output", "job_id", r.job.ID, "output", name, "error", err)
			continue
		}
		if ok {
			out[name] = v
		}
	}
	raw, err := workflow.EncodeJSON(out)
	if err != nil {
		return nil
	}
	return raw
}

// ---- hooks ----

func (r *jobRun) chain(p hook.Point, spec *wfdef.NodeSpec) hook.Chain {
	var global config.Hooks
	if r.e.d.Config != nil {
		if cfg, err := r.e.d.Config.TenantConfig(r.ctx, r.store.Tenant()); err == nil {
			global = cfg.Hooks
		}
	}
	var nodeSet *wfdef.HookSet
	if spec != nil {
		nodeSet = spec.Hooks
	}
	return hooks.CapTimeouts(hooks.ChainFor(p, global, r.rt.def.Hooks, nodeSet), global.MaxTimeoutSeconds)
}

func (r *jobRun) invocation(n *domain.NodeExecution) hook.Invocation {
	inv := hook.Invocation{
		TenantID:   r.store.Tenant(),
		JobID:      string(r.job.ID),
		WorkflowID: r.job.WorkflowID,
		AssetID:    string(r.job.AssetID),
		TraceID:    r.job.TraceID,
	}
	if n != nil {
		inv.NodeID = n.NodeID
		inv.NodeType = n.NodeType
	}
	return inv
}

func (r *jobRun) auditHook(p hook.Point, nodeID, reason string) error {
	return r.store.InTx(r.ctx, func(tx repo.TenantRepos) error { return r.auditHookTx(tx, p, nodeID, reason) })
}

// auditHookTx records that a hook stopped something (§15.7).
func (r *jobRun) auditHookTx(tx repo.TenantRepos, p hook.Point, nodeID, reason string) error {
	details, err := json.Marshal(map[string]any{"point": p, "node_id": nodeID, "reason": reason, "workflow": r.job.WorkflowID})
	if err != nil {
		return err
	}
	_, err = tx.Audit().Append(r.ctx, domain.AuditRecord{
		ULID: id.New(), TenantID: r.store.Tenant(), At: r.e.now(), Actor: "system", Method: domain.AuthSystem,
		Action: "hook.blocked", Resource: string(r.job.ID), Result: "denied", Details: details,
	})
	return err
}
