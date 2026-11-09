// Package workerproto is the worker-to-daemon protocol (I4): JSON over HTTP on
// the daemon's listener under /api/v1/worker, authenticated with a worker token.
// Routes are in package api. Workers never open database files (D5).
package workerproto

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
)

// Default timings. The daemon may return others.
const (
	DefaultLeaseSeconds     = 60
	DefaultHeartbeatSeconds = 15
)

// RegisterRequest declares a worker and its resources (D40).
type RegisterRequest struct {
	WorkerID  string                 `json:"worker_id"`
	Pools     []node.Pool            `json:"pools"`
	Resources domain.WorkerResources `json:"resources"`
	Version   string                 `json:"version,omitempty"`
}

// RegisterResponse tells the worker its timings and where shared volumes are.
type RegisterResponse struct {
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
	ProtocolVersion  int    `json:"protocol_version"`
	DaemonVersion    string `json:"daemon_version,omitempty"`
}

// ProtocolVersion is this contract's version.
const ProtocolVersion = 1

// HeartbeatRequest keeps a worker registered and reports its load.
type HeartbeatRequest struct {
	WorkerID string `json:"worker_id"`
	Running  int    `json:"running"`
	DiskMB   int    `json:"disk_free_mb,omitempty"`
}

// LeaseRequest asks to run a task read from Redis.
type LeaseRequest struct {
	WorkerID string `json:"worker_id"`
}

// Lease grants a task. The daemon answers 410 when the task is obsolete (ack and
// drop it) and 409 when it is incompatible with this worker (leave it unacked).
type Lease struct {
	TaskID         string          `json:"task_id"`
	LeaseID        string          `json:"lease_id"`
	ExpiresAt      time.Time       `json:"expires_at"`
	TenantID       domain.TenantID `json:"tenant_id"`
	JobID          id.Friendly     `json:"job_id"`
	JobULID        id.ULID         `json:"job_ulid"`
	NodeID         string          `json:"node_id"`
	NodeType       string          `json:"node_type"`
	TypeVersion    int             `json:"type_version"`
	Attempt        int             `json:"attempt"`
	Parameters     json.RawMessage `json:"parameters"`
	IdempotencyKey string          `json:"idempotency_key"`
	TraceID        string          `json:"trace_id,omitempty"`
	// Permissions were granted to the job at submission (node.Env.HasPermission).
	Permissions []domain.Permission `json:"permissions,omitempty"`
	// ScratchJobs lists ancestor jobs whose scratch this task may read, for files a
	// parent passes to a workflow.execute child (I182). The engine fills it from
	// JobTrigger.ParentJobID; empty means only the task's own job.
	ScratchJobs []id.ULID `json:"scratch_jobs,omitempty"`
	// AI carries the tenant's AI settings for pool ai tasks (I197). No secrets. When
	// nil, connectors apply their defaults.
	AI *config.AI `json:"ai,omitempty"`
}

// TaskHeartbeatRequest extends a lease and reports progress.
type TaskHeartbeatRequest struct {
	LeaseID string  `json:"lease_id"`
	Percent float64 `json:"percent,omitempty"`
	Message string  `json:"message,omitempty"`
}

// TaskHeartbeatResponse carries the cooperative cancel flag (D11).
type TaskHeartbeatResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
	Cancel    bool      `json:"cancel"`
}

// LogLine is a line a worker reports for a task. It is stored on the job's stream
// and goes to the log sinks.
type LogLine struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// TaskLogsRequest posts log lines.
type TaskLogsRequest struct {
	LeaseID string    `json:"lease_id"`
	Lines   []LogLine `json:"lines"`
}

// CompleteRequest finishes a task. Re-delivery with the same lease is accepted, so
// a worker retries until acknowledged; only then does it XACK (I4).
type CompleteRequest struct {
	LeaseID string               `json:"lease_id"`
	Output  json.RawMessage      `json:"output"`
	Usage   []domain.UsageRecord `json:"usage,omitempty"`
	Tools   map[string]string    `json:"tools,omitempty"`
}

// FailRequest reports a failure with its classification (node.Transient and so
// on). Hooks and retry policy are the engine's.
type FailRequest struct {
	LeaseID        string `json:"lease_id"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	Retryable      bool   `json:"retryable"`
	OutcomeUnknown bool   `json:"outcome_unknown"`
}

// ConnectionRequest asks for what a leased task needs to open a connector.
type ConnectionRequest struct {
	LeaseID string             `json:"lease_id"`
	Role    domain.StorageRole `json:"role"`
}

// ConnectionResponse gives the role's instance and credential value. It is held
// in memory only and never logged (I39).
type ConnectionResponse struct {
	Instance    config.StorageInstance `json:"instance"`
	Credentials []byte                 `json:"credentials,omitempty"`
}

// CacheKeyRequest asks for the leased tenant's cache key (D22, I39).
type CacheKeyRequest struct {
	LeaseID string `json:"lease_id"`
}

// CacheKeyResponse carries a key valid for the tenant's cache only.
type CacheKeyResponse struct {
	Key []byte `json:"key"`
}

// Coordinator is what the API's worker handlers call. Component engine
// implements it. Lease errors use errs.CodeGone (410) and errs.CodeConflict (409).
type Coordinator interface {
	Register(ctx context.Context, req RegisterRequest) (RegisterResponse, error)
	Heartbeat(ctx context.Context, req HeartbeatRequest) error
	Lease(ctx context.Context, taskID string, req LeaseRequest) (Lease, error)
	TaskHeartbeat(ctx context.Context, taskID string, req TaskHeartbeatRequest) (TaskHeartbeatResponse, error)
	TaskLogs(ctx context.Context, taskID string, req TaskLogsRequest) error
	Complete(ctx context.Context, taskID string, req CompleteRequest) error
	Fail(ctx context.Context, taskID string, req FailRequest) error
	Connection(ctx context.Context, taskID string, req ConnectionRequest) (ConnectionResponse, error)
	CacheKey(ctx context.Context, taskID string, req CacheKeyRequest) (CacheKeyResponse, error)
}
