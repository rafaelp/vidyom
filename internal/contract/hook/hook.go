// Package hook defines lifecycle hooks (§8.1). A hook is synchronous and part
// of execution: it can block a `before` point and it has a timeout. Events
// (package event) are the asynchronous counterpart.
package hook

import (
	"context"
	"encoding/json"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// Point is a lifecycle interception point. Workflow and node scopes come from
// where the hook is declared (R34).
type Point string

const (
	WorkflowBefore Point = "workflow.before"
	WorkflowStart  Point = "workflow.start"
	WorkflowAfter  Point = "workflow.after"
	WorkflowFinish Point = "workflow.finish"
	WorkflowError  Point = "workflow.error"
	WorkflowCancel Point = "workflow.cancel"
	NodeBefore     Point = "node.before"
	NodeAfter      Point = "node.after"
	NodeError      Point = "node.error"
	NodeFinish     Point = "node.finish"
)

// DefaultTimeoutSeconds applies when a declaration omits timeoutSeconds (D40).
const DefaultTimeoutSeconds = 30

// Invocation is one hook call. Data holds the point's context; never secrets.
type Invocation struct {
	Point      Point           `json:"point"`
	Hook       wfdef.HookDecl  `json:"hook"`
	TenantID   domain.TenantID `json:"tenant_id"`
	JobID      string          `json:"job_id,omitempty"`
	WorkflowID string          `json:"workflow_id,omitempty"`
	NodeID     string          `json:"node_id,omitempty"`
	NodeType   string          `json:"node_type,omitempty"`
	AssetID    string          `json:"asset_id,omitempty"`
	TraceID    string          `json:"trace_id,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// Result is a hook's answer. A hook that returns modified parameters goes
// through validation and authorization again (§8.1).
type Result struct {
	Allow      bool            `json:"allow"`
	Reason     string          `json:"reason,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// ActionRunner runs one hook action, usually in a plugin process over JSON-RPC
// (D3). Implemented by component extensibility.
type ActionRunner interface {
	RunHookAction(ctx context.Context, inv Invocation) (Result, error)
}

// Chain is the ordered set of hooks for one point: global, then workflow, then
// node for before points, and the reverse for after and finish (§8.1).
type Chain struct {
	Global   []wfdef.HookDecl
	Workflow []wfdef.HookDecl
	Node     []wfdef.HookDecl
}

// Dispatcher applies order, timeouts and failure policies, and is called by the
// engine at each point. A returned error means the point is blocked. Implemented
// by component engine. Hooks cannot retry an unsafe node (D11).
type Dispatcher interface {
	Dispatch(ctx context.Context, point Point, chain Chain, base Invocation) (Result, error)
}
