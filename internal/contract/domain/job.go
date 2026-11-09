package domain

import (
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/id"
)

// JobStatus is the execution state of §5.5, including needs_attention (D11).
type JobStatus string

const (
	JobQueued          JobStatus = "queued"
	JobRunning         JobStatus = "running"
	JobWaitingApproval JobStatus = "waiting_approval"
	JobRetrying        JobStatus = "retrying"
	JobPaused          JobStatus = "paused"
	JobCompleted       JobStatus = "completed"
	JobFailed          JobStatus = "failed"
	JobCancelled       JobStatus = "cancelled"
	JobNeedsAttention  JobStatus = "needs_attention"
)

// JobStatuses lists every job state.
var JobStatuses = []JobStatus{
	JobQueued, JobRunning, JobWaitingApproval, JobRetrying, JobPaused,
	JobCompleted, JobFailed, JobCancelled, JobNeedsAttention,
}

var jobTransitions = map[JobStatus][]JobStatus{
	JobQueued:          {JobRunning, JobPaused, JobCancelled, JobFailed},
	JobRunning:         {JobWaitingApproval, JobRetrying, JobPaused, JobCompleted, JobFailed, JobCancelled, JobNeedsAttention},
	JobWaitingApproval: {JobRunning, JobPaused, JobFailed, JobCancelled, JobCompleted},
	JobRetrying:        {JobRunning, JobPaused, JobFailed, JobCancelled, JobNeedsAttention},
	JobPaused:          {JobRunning, JobQueued, JobCancelled, JobFailed},
	JobNeedsAttention:  {JobRunning, JobFailed, JobCancelled}, // only through job resolve (D11)
	JobFailed:          {JobQueued},                           // job retry resumes from checkpoints
	JobCompleted:       {},
	JobCancelled:       {},
}

// CanTransition reports whether the job state machine allows moving to next.
func (s JobStatus) CanTransition(next JobStatus) bool {
	for _, allowed := range jobTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Terminal reports whether no further automatic progress can happen. The
// workflow.completed, workflow.failed and workflow.cancelled events fire when a
// terminal state is written. needs_attention is not terminal (§8.2).
func (s JobStatus) Terminal() bool {
	return s == JobCompleted || s == JobFailed || s == JobCancelled
}

// NodeStatus is the state of one node execution inside a job.
type NodeStatus string

const (
	NodePending   NodeStatus = "pending"
	NodeReady     NodeStatus = "ready"
	NodeQueued    NodeStatus = "queued"  // task pointer in Redis
	NodeRunning   NodeStatus = "running" // leased
	NodeWaiting   NodeStatus = "waiting" // suspended: approval, flow.wait, child job
	NodeCompleted NodeStatus = "completed"
	NodeFailed    NodeStatus = "failed"
	NodeSkipped   NodeStatus = "skipped" // branch not taken (D33)
	NodeCancelled NodeStatus = "cancelled"
)

// JobError is the terminal or latest error of a job or node. It holds no secrets.
type JobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	NodeID  string `json:"node_id,omitempty"`
}

// AttentionInfo explains why a job is in needs_attention (D11).
type AttentionInfo struct {
	NodeID         string    `json:"node_id"`
	NodeType       string    `json:"node_type"`
	Reason         string    `json:"reason"`
	OutcomeUnknown bool      `json:"outcome_unknown"`
	Since          time.Time `json:"since"`
}

// ResolveAction is how a person leaves needs_attention (§11.3).
type ResolveAction string

const (
	ResolveRetryNode ResolveAction = "retry_node"
	ResolveSkipNode  ResolveAction = "skip_node"
	ResolveFail      ResolveAction = "fail"
)

// JobTrigger says what started a job.
type JobTrigger struct {
	Type        string `json:"type"` // cli, event, schedule, subworkflow
	EventID     string `json:"event_id,omitempty"`
	CausationID string `json:"causation_id,omitempty"`
	Depth       int    `json:"depth,omitempty"` // event chain depth (§8.2)
	ParentJobID string `json:"parent_job_id,omitempty"`
	ParentNode  string `json:"parent_node,omitempty"`
}

// Job is one workflow execution. Snapshot is the immutable workflow JSON saved
// at submission (§6.5), so later versions never affect it.
type Job struct {
	ID              id.Friendly     `json:"id"`
	ULID            id.ULID         `json:"ulid"`
	TenantID        TenantID        `json:"tenant_id"`
	WorkflowID      string          `json:"workflow"`
	WorkflowVersion string          `json:"workflow_version"`
	WorkflowSHA256  string          `json:"workflow_sha256"`
	Snapshot        json.RawMessage `json:"snapshot,omitempty"`
	Status          JobStatus       `json:"status"`
	StatusReason    string          `json:"status_reason,omitempty"`
	Inputs          json.RawMessage `json:"inputs,omitempty"`
	Outputs         json.RawMessage `json:"outputs,omitempty"`
	Trigger         JobTrigger      `json:"trigger"`
	AssetID         id.Friendly     `json:"asset_id,omitempty"`
	SubmittedBy     string          `json:"submitted_by,omitempty"`
	Permissions     []Permission    `json:"permissions,omitempty"` // granted to the job at submission
	IdempotencyKey  string          `json:"idempotency_key,omitempty"`
	TraceID         string          `json:"trace_id,omitempty"`
	ProgressPercent float64         `json:"progress_percent"`
	Attention       *AttentionInfo  `json:"attention,omitempty"`
	Error           *JobError       `json:"error,omitempty"`
	CreatedAt       time.Time       `json:"created_at,omitzero"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
}

// NodeExecution is the durable record of one node in a job; a completed record
// is the checkpoint (D11). Output is JSON capped at MaxNodeOutputBytes (I35).
type NodeExecution struct {
	JobULID         id.ULID         `json:"job_ulid"`
	NodeID          string          `json:"node_id"`
	NodeType        string          `json:"node_type"`
	TypeVersion     int             `json:"type_version"`
	Status          NodeStatus      `json:"status"`
	Attempt         int             `json:"attempt"`
	Parameters      json.RawMessage `json:"parameters,omitempty"` // resolved, never secrets
	Output          json.RawMessage `json:"output,omitempty"`
	Error           *JobError       `json:"error,omitempty"`
	IdempotencyKey  string          `json:"idempotency_key,omitempty"`
	TaskID          string          `json:"task_id,omitempty"`
	WorkerID        string          `json:"worker_id,omitempty"`
	LeaseID         string          `json:"lease_id,omitempty"`
	LeaseExpiresAt  *time.Time      `json:"lease_expires_at,omitempty"`
	ProgressPercent float64         `json:"progress_percent"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
}

// MaxNodeOutputBytes caps a node's JSON output (I35).
const MaxNodeOutputBytes = 32 << 20

// JobEventKind classifies entries of a job's stream.
type JobEventKind string

const (
	JobEventState    JobEventKind = "job.state"
	JobEventNode     JobEventKind = "node.state"
	JobEventProgress JobEventKind = "node.progress"
	JobEventLog      JobEventKind = "log"
	JobEventApproval JobEventKind = "approval"
)

// JobEvent is one entry of a job's durable stream, served by
// GET /jobs/{id}/events. Seq is monotonic within the job and is the SSE id.
type JobEvent struct {
	Seq     int64           `json:"seq"`
	JobID   id.Friendly     `json:"job_id"`
	At      time.Time       `json:"at"`
	Kind    JobEventKind    `json:"kind"`
	NodeID  string          `json:"node_id,omitempty"`
	Level   string          `json:"level,omitempty"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// WorkerResources is what a worker declares (D40).
type WorkerResources struct {
	CPUCores       int  `json:"cpu_cores"`
	MemoryMB       int  `json:"memory_mb"`
	DiskMB         int  `json:"disk_mb"`
	GPU            bool `json:"gpu"`
	MaxConcurrency int  `json:"max_concurrency"`
}

// Worker is a registered worker process.
type Worker struct {
	ID         string          `json:"id"`
	Pools      []string        `json:"pools"`
	Resources  WorkerResources `json:"resources"`
	Version    string          `json:"version,omitempty"`
	Running    int             `json:"running"`
	LastSeenAt time.Time       `json:"last_seen_at"`
}
