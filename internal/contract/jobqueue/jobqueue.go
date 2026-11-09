// Package jobqueue is the dispatch queue contract: Redis Streams with consumer
// groups (D4). The tenant database is the source of truth; Redis only points at
// work. A stream entry carries no parameters, content or secrets (I4).
package jobqueue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
)

// Stream and key names, shared by the daemon and every worker.
const (
	StreamPrefix = "vidyom:tasks:"
	GroupPrefix  = "vidyom-"
	// InflightKey is a hash of task ID to stream ID, used by startup reconciliation
	// to find tasks missing from Redis (D4).
	InflightKey = "vidyom:tasks:inflight"
)

// StreamKey returns the stream of a pool.
func StreamKey(p node.Pool) string { return StreamPrefix + string(p) }

// GroupName returns the consumer group of a pool.
func GroupName(p node.Pool) string { return GroupPrefix + string(p) }

// TaskID identifies one attempt of one node. It is URL-path safe.
func TaskID(job id.ULID, nodeID string, attempt int) string {
	return fmt.Sprintf("%s.%s.%d", job, nodeID, attempt)
}

// ParseTaskID splits a task ID.
func ParseTaskID(taskID string) (job id.ULID, nodeID string, attempt int, err error) {
	first := strings.IndexByte(taskID, '.')
	last := strings.LastIndexByte(taskID, '.')
	if first <= 0 || last <= first {
		return "", "", 0, fmt.Errorf("invalid task ID %q", taskID)
	}
	if _, err := fmt.Sscanf(taskID[last+1:], "%d", &attempt); err != nil {
		return "", "", 0, fmt.Errorf("invalid task ID %q", taskID)
	}
	job, err = id.ParseULID(taskID[:first])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid task ID %q", taskID)
	}
	return job, taskID[first+1 : last], attempt, nil
}

// Pointer is the whole content of a stream entry.
type Pointer struct {
	TaskID     string          `json:"task_id"`
	TenantID   domain.TenantID `json:"tenant_id"`
	Pool       node.Pool       `json:"pool"`
	NodeType   string          `json:"node_type"`
	TraceID    string          `json:"trace_id,omitempty"`
	EnqueuedAt time.Time       `json:"enqueued_at"`
}

// NodeTypeScratchCleanup marks a pointer that asks a worker to delete a finished
// job's scratch directory (I5, I45). The engine enqueues it when a job reaches a
// terminal state, in a pool with access to the shared scratch volume. It takes no
// lease and has no result: the worker removes the directory and acks. No node of
// the catalog has this type.
const NodeTypeScratchCleanup = "internal.scratch_cleanup"

// ScratchCleanupNodeID is the node part of a cleanup pointer's task ID.
const ScratchCleanupNodeID = "_scratch_cleanup"

// ScratchCleanupPointer builds the pointer for a job that reached a terminal state.
func ScratchCleanupPointer(tenant domain.TenantID, job id.ULID, at time.Time) Pointer {
	return Pointer{
		TaskID:     TaskID(job, ScratchCleanupNodeID, 1),
		TenantID:   tenant,
		Pool:       node.PoolMedia,
		NodeType:   NodeTypeScratchCleanup,
		EnqueuedAt: at,
	}
}

// Delivery is a read pointer with its stream position.
type Delivery struct {
	Pointer  Pointer
	StreamID string
}

// ReadOptions controls a blocking read.
type ReadOptions struct {
	Pool     node.Pool
	Consumer string
	Count    int
	Block    time.Duration
}

// Depth reports a pool's backlog (§10.3).
type Depth struct {
	Length  int64 `json:"length"`
	Pending int64 `json:"pending"`
}

// Queue is the job queue. Component engine implements it with go-redis (I2).
type Queue interface {
	// Enqueue adds a pointer, atomically with its inflight entry. It is a no-op
	// when the task is already inflight, so reconciliation can call it freely.
	Enqueue(ctx context.Context, p Pointer) error
	// Inflight reports whether a task is known to Redis (D4).
	Inflight(ctx context.Context, taskID string) (bool, error)
	// Read returns new entries for a consumer, creating the group if needed.
	Read(ctx context.Context, opts ReadOptions) ([]Delivery, error)
	// ClaimStale takes over entries idle longer than minIdle from dead consumers.
	ClaimStale(ctx context.Context, pool node.Pool, consumer string, minIdle time.Duration, count int) ([]Delivery, error)
	// Ack acknowledges an entry and removes its inflight record. Workers call it
	// only after the daemon has committed the result, or answered 410 (I4).
	Ack(ctx context.Context, pool node.Pool, streamID, taskID string) error
	// Depth reports a pool's backlog.
	Depth(ctx context.Context, pool node.Pool) (Depth, error)
	Ping(ctx context.Context) error
	Close() error
}
