// Package event defines the event envelope and types of the persistent Event
// Bus (§8.2). An event is asynchronous, goes through the transactional outbox
// in the tenant database and never blocks a workflow. A hook, by contrast, is
// synchronous; see package hook.
package event

import (
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
)

// Type is an event type name.
type Type string

// Event types and when they fire (R35, D11, D19).
const (
	AssetCreated          Type = "asset.created"    // ID allocated, any kind
	ClipCreated           Type = "clip.created"     // specialization for kind clip; both fire
	AssetIndexed          Type = "asset.indexed"    // valid canonical index, any kind
	OriginalIndexed       Type = "original.indexed" // specialization for originals; both fire
	AssetReadyForClipping Type = "asset.ready_for_clipping"
	AssetDeleted          Type = "asset.deleted" // audited, without content (D17)

	StorageBackupVerified Type = "storage.backup.verified"

	ApprovalRequested Type = "approval.requested"
	ApprovalDecided   Type = "approval.decided"

	PublicationCreated   Type = "publication.created"   // before or during upload
	PublicationCompleted Type = "publication.completed" // payload carries visibility
	PublicationUpdated   Type = "publication.updated"
	PublicationWithdrawn Type = "publication.withdrawn"

	WorkflowCompleted Type = "workflow.completed" // terminal state already written
	WorkflowFailed    Type = "workflow.failed"
	WorkflowCancelled Type = "workflow.cancelled"
	JobNeedsAttention Type = "job.needs_attention"

	BudgetThresholdReached Type = "budget.threshold_reached" // 80%
	BudgetExceeded         Type = "budget.exceeded"          // 100%

	// PublicLibrarySynced is additive to R35: fires after public.db is shipped.
	PublicLibrarySynced Type = "public_library.synced"
)

// SchemaVersion is the current envelope schema version.
const SchemaVersion = 1

// MaxCausationDepth is the default limit on event chains (§8.2); deeper chains
// are dropped and recorded, preventing loops.
const MaxCausationDepth = 5

// Envelope is the minimum event envelope (§8.2). EventID doubles as the
// idempotency key for at-least-once delivery. Events are tenant-scoped (D6).
// Payloads never carry secrets or transcript content.
type Envelope struct {
	EventID       id.ULID         `json:"event_id"`
	Type          Type            `json:"type"`
	SchemaVersion int             `json:"schema_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
	TenantID      domain.TenantID `json:"tenant_id"`
	JobID         string          `json:"job_id,omitempty"`
	WorkflowID    string          `json:"workflow_id,omitempty"`
	AssetID       string          `json:"asset_id,omitempty"`
	TraceID       string          `json:"trace_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	// CausationDepth is how many events precede this one in its chain.
	CausationDepth int             `json:"causation_depth,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// Draft is what a producer supplies. The outbox fills in the ID, time, version
// and tenant, and the job context when one is known.
type Draft struct {
	Type    Type
	AssetID string
	Payload any
	// ID is optional: a deterministic event ID, so a re-run of an idempotent
	// producer emits nothing new (I156). Empty means a fresh ULID.
	ID id.ULID
}

// DeliveryStatus tracks one subscriber's delivery of an event.
type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliveryDelivered DeliveryStatus = "delivered"
	DeliveryFailed    DeliveryStatus = "failed"
	DeliveryDropped   DeliveryStatus = "dropped" // loop limit or filter
)

// Delivery is one subscriber's delivery record (§8.2, `event deliveries`).
type Delivery struct {
	EventID     id.ULID        `json:"event_id"`
	Subscriber  string         `json:"subscriber"` // workflow:<id> or plugin:<id>
	Status      DeliveryStatus `json:"status"`
	Attempts    int            `json:"attempts"`
	LastError   string         `json:"last_error,omitempty"`
	NextAttempt *time.Time     `json:"next_attempt,omitempty"`
	DeliveredAt *time.Time     `json:"delivered_at,omitempty"`
}
