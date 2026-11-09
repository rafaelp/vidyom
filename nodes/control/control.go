package controlnodes

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/internal/events"
	"github.com/rafaelp/vidyom/schemas"
)

// Limits of flow.foreach, beyond what a workflow declares.
const (
	DefaultConcurrency = 4
	MaxConcurrency     = 16
	DefaultMaxItems    = 100
	MaxItems           = 1000
)

// Suspension reasons, persisted by the engine.
const (
	ReasonWait        = "wait"
	ReasonSubworkflow = "subworkflow"
)

// ChildRequest asks for a child job (§6.2). Key tells apart the children of one node.
// Starting a key that exists returns the existing child, which is what makes these
// nodes safe to run again after a crash.
type ChildRequest struct {
	Key      string
	Workflow string
	Version  string
	Inputs   map[string]any
}

// ChildState is what a parent sees of a child: its status and outputs, nothing else
// (output isolation, §6.2).
type ChildState struct {
	Key     string
	JobID   id.Friendly
	Status  domain.JobStatus
	Outputs json.RawMessage
	Error   *domain.JobError
}

// Env is what pool engine nodes get, on top of node.DaemonEnv.
type Env interface {
	node.DaemonEnv
	// EvalCondition evaluates CEL against the job's data (D2, I23).
	EvalCondition(ctx context.Context, expr string) (bool, error)
	// MergeInputs lists the predecessors of this node that completed.
	MergeInputs() []string
	// StartChild starts a child job for this node, or finds it.
	StartChild(ctx context.Context, req ChildRequest) (ChildState, error)
	// Children lists this node's child jobs.
	Children(ctx context.Context) ([]ChildState, error)
}

func require(env node.Env) (Env, error) {
	e, ok := env.(Env)
	if !ok {
		return nil, errs.New(errs.CodeInternal, "this node must run inline in the engine")
	}
	return e, nil
}

type base struct{ desc node.Descriptor }

func (b base) Descriptor() node.Descriptor { return b.desc }

func descriptor(nodeType string) node.Descriptor {
	d, err := schemas.DescriptorFor(nodeType, 1)
	if err != nil {
		panic(err)
	}
	return d
}

// Register adds the control nodes.
func Register(r node.Registrar) {
	r.RegisterNode(flowIf{base{descriptor(wfdef.TypeFlowIf)}})
	r.RegisterNode(flowMerge{base{descriptor(wfdef.TypeFlowMerge)}})
	r.RegisterNode(flowWait{base{descriptor(wfdef.TypeFlowWait)}})
	r.RegisterNode(flowForeach{base{descriptor(wfdef.TypeFlowForeach)}})
	r.RegisterNode(workflowExecute{base{descriptor(wfdef.TypeWorkflowExecute)}})
	r.RegisterNode(eventEmit{base{descriptor(wfdef.TypeEventEmit)}})
}

// ---- flow.if ----

type flowIf struct{ base }

func (n flowIf) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	e, err := require(env)
	if err != nil {
		return node.Result{}, err
	}
	var p struct {
		Condition string `json:"condition"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	ok, err := e.EvalCondition(ctx, p.Condition)
	if err != nil {
		return node.Result{}, node.Permanent(errs.Wrap(errs.CodeInvalidArgument, err, "condition failed: "+err.Error()))
	}
	return node.JSONResult(map[string]bool{"result": ok})
}

// ---- flow.merge ----

type flowMerge struct{ base }

func (n flowMerge) Execute(_ context.Context, env node.Env, _ node.Input) (node.Result, error) {
	e, err := require(env)
	if err != nil {
		return node.Result{}, err
	}
	completed := e.MergeInputs()
	if completed == nil {
		completed = []string{}
	}
	return node.JSONResult(map[string]any{"completed": completed})
}

// ---- flow.wait ----

type flowWait struct{ base }

func (n flowWait) Execute(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
	var p struct {
		Seconds *float64 `json:"seconds"`
		Until   string   `json:"until"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var at time.Time
	switch {
	case p.Seconds != nil && *p.Seconds >= 0:
		at = time.Now().Add(time.Duration(*p.Seconds * float64(time.Second)))
	case p.Until != "":
		t, err := time.Parse(time.RFC3339, p.Until)
		if err != nil {
			return node.Result{}, node.Permanent(errs.Newf(errs.CodeInvalidArgument, "until must be RFC 3339: %s", p.Until))
		}
		at = t
	default:
		return node.Result{}, node.Permanent(errs.New(errs.CodeInvalidArgument, "flow.wait needs seconds or until"))
	}
	resume := int64(math.Ceil(float64(at.UnixNano()) / float64(time.Second)))
	return node.Result{Suspend: &node.Suspension{Reason: ReasonWait, ResumeAfter: resume}}, nil
}

// Resume implements node.Resumer: the wait ended.
func (n flowWait) Resume(context.Context, node.Env, node.Input, json.RawMessage) (node.Result, error) {
	return node.JSONResult(map[string]any{})
}

// ---- workflow.execute ----

type workflowExecute struct{ base }

func (n workflowExecute) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	e, err := require(env)
	if err != nil {
		return node.Result{}, err
	}
	var params map[string]any
	if err := json.Unmarshal(in.Parameters, &params); err != nil {
		return node.Result{}, node.Permanent(errs.Wrap(errs.CodeInvalidArgument, err, "invalid parameters"))
	}
	name, _ := params["workflow"].(string)
	version, _ := params["version"].(string)
	delete(params, "workflow")
	delete(params, "version")
	if _, err := e.StartChild(ctx, ChildRequest{Workflow: name, Version: version, Inputs: params}); err != nil {
		return node.Result{}, err
	}
	return node.Result{Suspend: &node.Suspension{Reason: ReasonSubworkflow}}, nil
}

// Resume implements node.Resumer: it finishes when the child does, with the child's
// outputs as its own, and nothing else of the child's (§6.2).
func (n workflowExecute) Resume(ctx context.Context, env node.Env, in node.Input, _ json.RawMessage) (node.Result, error) {
	e, err := require(env)
	if err != nil {
		return node.Result{}, err
	}
	children, err := e.Children(ctx)
	if err != nil {
		return node.Result{}, err
	}
	if len(children) == 0 {
		return n.Execute(ctx, env, in)
	}
	child := children[0]
	if !child.Status.Terminal() {
		return node.Result{Suspend: &node.Suspension{Reason: ReasonSubworkflow}}, nil
	}
	if child.Status != domain.JobCompleted {
		return node.Result{}, node.Permanent(childError(child))
	}
	output := child.Outputs
	if len(output) == 0 {
		output = json.RawMessage(`{}`)
	}
	return node.Result{Output: output}, nil
}

func childError(c ChildState) error {
	if c.Error != nil {
		return errs.Newf(errs.CodeFailedPrecondition, "child job %s %s: %s", c.JobID, c.Status, c.Error.Message)
	}
	return errs.Newf(errs.CodeFailedPrecondition, "child job %s %s", c.JobID, c.Status)
}

// ---- flow.foreach ----

type flowForeach struct{ base }

type foreachParams struct {
	Items       []any          `json:"items"`
	Workflow    string         `json:"workflow"`
	Version     string         `json:"version"`
	ItemInput   string         `json:"itemInput"`
	Inputs      map[string]any `json:"inputs"`
	Concurrency int            `json:"concurrency"`
	MaxItems    int            `json:"maxItems"`
}

func (p *foreachParams) normalize() error {
	if p.ItemInput == "" {
		p.ItemInput = "item"
	}
	if p.Concurrency <= 0 {
		p.Concurrency = DefaultConcurrency
	}
	if p.Concurrency > MaxConcurrency {
		p.Concurrency = MaxConcurrency
	}
	if p.MaxItems <= 0 {
		p.MaxItems = DefaultMaxItems
	}
	if p.MaxItems > MaxItems {
		p.MaxItems = MaxItems
	}
	if len(p.Items) > p.MaxItems {
		return errs.Newf(errs.CodeInvalidArgument, "flow.foreach got %d items; the limit is %d", len(p.Items), p.MaxItems)
	}
	return nil
}

func (n flowForeach) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	return n.step(ctx, env, in)
}

// Resume implements node.Resumer: it starts more items as others end.
func (n flowForeach) Resume(ctx context.Context, env node.Env, in node.Input, _ json.RawMessage) (node.Result, error) {
	return n.step(ctx, env, in)
}

func (n flowForeach) step(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	e, err := require(env)
	if err != nil {
		return node.Result{}, err
	}
	var p foreachParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if err := p.normalize(); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	children, err := e.Children(ctx)
	if err != nil {
		return node.Result{}, err
	}
	state := map[int]ChildState{}
	running, failed := 0, 0
	for _, c := range children {
		idx, err := strconv.Atoi(c.Key)
		if err != nil {
			continue
		}
		state[idx] = c
		switch {
		case !c.Status.Terminal():
			running++
		case c.Status != domain.JobCompleted:
			failed++
		}
	}
	if failed > 0 {
		// Fail fast: start nothing new, let the running ones end (I159).
		if running > 0 {
			return node.Result{Suspend: &node.Suspension{Reason: ReasonSubworkflow}}, nil
		}
		for i := range p.Items {
			if c, ok := state[i]; ok && c.Status.Terminal() && c.Status != domain.JobCompleted {
				return node.Result{}, node.Permanent(errs.Newf(errs.CodeFailedPrecondition, "item %d: %s", i, errs.Public(childError(c)).Message))
			}
		}
	}
	for i := range p.Items {
		if running >= p.Concurrency {
			break
		}
		if _, started := state[i]; started {
			continue
		}
		inputs := make(map[string]any, len(p.Inputs)+1)
		for k, v := range p.Inputs {
			inputs[k] = v
		}
		inputs[p.ItemInput] = p.Items[i]
		child, err := e.StartChild(ctx, ChildRequest{Key: strconv.Itoa(i), Workflow: p.Workflow, Version: p.Version, Inputs: inputs})
		if err != nil {
			return node.Result{}, err
		}
		state[i] = child
		running++
	}
	if running > 0 {
		return node.Result{Suspend: &node.Suspension{Reason: ReasonSubworkflow}}, nil
	}
	indexes := make([]int, 0, len(state))
	for i := range state {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	results := make([]map[string]any, 0, len(indexes))
	for _, i := range indexes {
		c := state[i]
		item := map[string]any{"index": i, "job_id": c.JobID, "status": c.Status}
		if len(c.Outputs) > 0 {
			item["outputs"] = c.Outputs
		}
		results = append(results, item)
	}
	return node.JSONResult(map[string]any{"results": results})
}

// ---- event.emit ----

type eventEmit struct{ base }

// reservedEvents are written by the engine or the budget service only: a workflow
// stating that a job finished or a budget ran out would be a false record.
var reservedEvents = map[event.Type]bool{
	event.WorkflowCompleted: true, event.WorkflowFailed: true, event.WorkflowCancelled: true,
	event.JobNeedsAttention: true, event.ApprovalRequested: true, event.ApprovalDecided: true,
	event.BudgetThresholdReached: true, event.BudgetExceeded: true,
}

func (n eventEmit) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, err
	}
	var p struct {
		Type    string          `json:"type"`
		AssetID string          `json:"assetId"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if reservedEvents[event.Type(p.Type)] {
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeForbidden, "event %s is written by the engine, not by a workflow", p.Type))
	}
	asset := p.AssetID
	if asset != "" {
		a, err := d.Store().Assets().Get(ctx, id.Ref(asset))
		if err != nil {
			return node.Result{}, node.Permanent(err)
		}
		asset = string(a.ID)
	}
	draft := event.Draft{
		Type:    event.Type(p.Type),
		AssetID: asset,
		ID:      events.DeterministicID(env.JobULID(), in.IdempotencyKey),
	}
	if len(p.Payload) > 0 {
		draft.Payload = p.Payload
	}
	if err := d.Emit(ctx, nil, draft); err != nil {
		return node.Result{}, node.Transient(err)
	}
	return node.JSONResult(map[string]string{"event_id": string(draft.ID)})
}
