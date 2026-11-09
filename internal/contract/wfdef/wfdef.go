// Package wfdef is the JSON model of a workflow (§6.2, D44). The engine owns
// validation and execution; this package only defines the shape, which
// schemas/workflow.v1.json describes for the validator.
//
// Data references use ${...} and conditions use CEL (D2). Neither is evaluated
// here.
package wfdef

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// InputType is a declared input's type. File inputs are URIs (R20).
type InputType string

const (
	InputString InputType = "string"
	InputNumber InputType = "number"
	InputBool   InputType = "boolean"
	InputFile   InputType = "file"
	InputArray  InputType = "array"
	InputObject InputType = "object"
)

// InputSpec declares one workflow input.
type InputSpec struct {
	Type        InputType       `json:"type"`
	Required    bool            `json:"required,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Description string          `json:"description,omitempty"`
}

// TriggerType says what starts a workflow (§6.2, I22). Webhook validates and is
// refused until implemented.
type TriggerType string

const (
	TriggerCLI      TriggerType = "cli"
	TriggerEvent    TriggerType = "event"
	TriggerSchedule TriggerType = "schedule"
	TriggerWebhook  TriggerType = "webhook"
)

// Trigger is a workflow's single trigger.
type Trigger struct {
	Type   TriggerType `json:"type"`
	Event  string      `json:"event,omitempty"`  // TriggerEvent: event type
	Filter string      `json:"filter,omitempty"` // TriggerEvent: CEL (D2)
	Every  string      `json:"every,omitempty"`  // TriggerSchedule: Go duration, "24h"
	Cron   string      `json:"cron,omitempty"`   // reserved
}

// HookFailurePolicy is what a failing hook does (§8.1).
type HookFailurePolicy string

const (
	HookAbort    HookFailurePolicy = "abort"
	HookRetry    HookFailurePolicy = "retry"
	HookContinue HookFailurePolicy = "continue"
)

// HookDecl is one hook declaration.
type HookDecl struct {
	ID             string            `json:"id"`
	Plugin         string            `json:"plugin"`
	Action         string            `json:"action"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"` // default 30 s (D40)
	OnFailure      HookFailurePolicy `json:"onFailure,omitempty"`
}

// HookSet is the `hooks` object at workflow or node level. The scope comes from
// its position; `start` and `cancel` do not apply to nodes (R34).
type HookSet struct {
	Before []HookDecl `json:"before,omitempty"`
	Start  []HookDecl `json:"start,omitempty"`
	After  []HookDecl `json:"after,omitempty"`
	Finish []HookDecl `json:"finish,omitempty"`
	Error  []HookDecl `json:"error,omitempty"`
	Cancel []HookDecl `json:"cancel,omitempty"`
}

// NodeSpec is one node of the graph. ID is unique per workflow and matches
// ^[A-Za-z0-9_-]+$.
type NodeSpec struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	TypeVersion int             `json:"typeVersion"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Hooks       *HookSet        `json:"hooks,omitempty"`
	Retry       *int            `json:"retry,omitempty"` // overrides settings.retry
	// ContinueOnFailure lets downstream nodes run after this node fails, for
	// steps that must not block, such as external catalog sync (R11).
	ContinueOnFailure bool `json:"continueOnFailure,omitempty"`
}

// Settings are per-workflow execution settings.
type Settings struct {
	Retry                 *int `json:"retry,omitempty"`
	RetryBackoffSeconds   *int `json:"retryBackoffSeconds,omitempty"`
	ResumeOnFailure       bool `json:"resumeOnFailure,omitempty"`
	SaveExecutionProgress bool `json:"saveExecutionProgress,omitempty"`
	TimeoutSeconds        *int `json:"timeoutSeconds,omitempty"`
	// AssetStatusOnFailure is written with a failed or cancelled job, in the same
	// transaction; job retry moves the asset back to ingesting (§2.5, I145).
	AssetStatusOnFailure string `json:"assetStatusOnFailure,omitempty"`
}

// Definition is a workflow file. Connections maps a node ID to the IDs that
// depend on it, as in §6.3.
type Definition struct {
	ID          string               `json:"id"`
	Name        string               `json:"name,omitempty"`
	Version     string               `json:"version"`
	Description string               `json:"description,omitempty"`
	Inputs      map[string]InputSpec `json:"inputs,omitempty"`
	Trigger     *Trigger             `json:"trigger,omitempty"`
	Nodes       []NodeSpec           `json:"nodes"`
	Connections map[string][]string  `json:"connections,omitempty"`
	Hooks       *HookSet             `json:"hooks,omitempty"`
	Settings    *Settings            `json:"settings,omitempty"`
	Outputs     map[string]string    `json:"outputs,omitempty"` // output name to ${...}
	// Subworkflows is written by the engine in job snapshots only: the definitions
	// of the whole job tree, keyed id or id@version, so a new version never changes
	// a job in progress (§6.5, I144). A submitted definition carrying it is refused.
	Subworkflows map[string]json.RawMessage `json:"subworkflows,omitempty"`
}

// Parse decodes a workflow file. It does not validate it; use the engine.
func Parse(data []byte) (*Definition, error) {
	var def Definition
	if err := json.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	return &def, nil
}

// SHA256 is the hash stored with job snapshots and provenance (§6.5, R21). It
// covers the exact bytes saved as the snapshot.
func SHA256(snapshot []byte) string {
	sum := sha256.Sum256(snapshot)
	return hex.EncodeToString(sum[:])
}

// ControlNodeTypes are executed by the engine itself (pool engine, I7).
const (
	TypeFlowIf          = "flow.if"
	TypeFlowForeach     = "flow.foreach"
	TypeFlowWait        = "flow.wait"
	TypeFlowMerge       = "flow.merge"
	TypeWorkflowExecute = "workflow.execute"
	TypeApprovalRequest = "approval.request"
	TypeEventEmit       = "event.emit"
)
