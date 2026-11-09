package api

import (
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// List is every list response (I15). Pass limit and cursor as query parameters.
type List[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// PageQuery is the common query of list routes.
type PageQuery struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// ---- Health ----

// ComponentHealth is one dependency's state.
type ComponentHealth struct {
	Status string `json:"status"` // "ok", "degraded", "down"
	Detail string `json:"detail,omitempty"`
}

// HealthResponse answers GET /health. It reports how the master key is sealed
// (I10) but nothing that helps an attacker.
type HealthResponse struct {
	Status     string                     `json:"status"`
	Version    string                     `json:"version"`
	Sealing    string                     `json:"sealing,omitempty"`
	Components map[string]ComponentHealth `json:"components,omitempty"`
}

// VersionResponse lets old clients detect behavior changes (website API notes).
type VersionResponse struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
}

// DoctorCheck is one line of `vidyom doctor`.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ok", "warn", "fail"
	Detail string `json:"detail,omitempty"`
}

// DoctorResponse answers GET /doctor?check=security|network|storage.
type DoctorResponse struct {
	Checks []DoctorCheck `json:"checks"`
}

// ---- Workflows ----

// WorkflowSummary is an entry of GET /workflows.
type WorkflowSummary struct {
	ID          string                     `json:"id"`
	Name        string                     `json:"name,omitempty"`
	Version     string                     `json:"version"`
	Description string                     `json:"description,omitempty"`
	Inputs      map[string]wfdef.InputSpec `json:"inputs,omitempty"`
	Trigger     *wfdef.Trigger             `json:"trigger,omitempty"`
	SHA256      string                     `json:"sha256"`
}

// WorkflowDetail answers GET /workflows/{id}.
type WorkflowDetail struct {
	WorkflowSummary
	Definition json.RawMessage `json:"definition"`
}

// ValidateWorkflowRequest validates an inline definition.
type ValidateWorkflowRequest struct {
	Definition json.RawMessage `json:"definition"`
}

// ValidationIssue is one finding of validation or a dry run.
type ValidationIssue struct {
	Severity string `json:"severity"` // "error" or "warning"
	Code     string `json:"code"`
	Message  string `json:"message"`
	NodeID   string `json:"node_id,omitempty"`
	Path     string `json:"path,omitempty"`
}

// ValidationResult is the outcome of validation or `--dry-run` (§6.2), including
// the client-credential warning of R37.
type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Issues []ValidationIssue `json:"issues,omitempty"`
}

// RunWorkflowRequest submits a job (§12.1). POST /workflows/{id}/runs names the
// workflow in the path; POST /workflows/runs carries Definition inline.
type RunWorkflowRequest struct {
	WorkflowVersion string          `json:"workflow_version,omitempty"`
	Definition      json.RawMessage `json:"definition,omitempty"`
	Inputs          json.RawMessage `json:"inputs,omitempty"`
	DryRun          bool            `json:"dry_run,omitempty"`
	IdempotencyKey  string          `json:"idempotency_key,omitempty"` // or the header (I15)
}

// RunWorkflowResponse returns immediately with the job (§5.5). Clients attach
// later. DryRun runs return Validation and no job.
type RunWorkflowResponse struct {
	JobID           id.Friendly       `json:"job_id,omitempty"`
	JobULID         id.ULID           `json:"job_ulid,omitempty"`
	Status          domain.JobStatus  `json:"status,omitempty"`
	Workflow        string            `json:"workflow"`
	WorkflowVersion string            `json:"workflow_version"`
	Validation      *ValidationResult `json:"validation,omitempty"`
}

// ---- Jobs ----

// JobQuery filters GET /jobs.
type JobQuery struct {
	PageQuery
	Status   domain.JobStatus `json:"status,omitempty"`
	Workflow string           `json:"workflow,omitempty"`
	AssetID  string           `json:"asset_id,omitempty"`
}

// JobDetail answers GET /jobs/{id}. Status includes needs_attention with its node
// and reason (D11).
type JobDetail struct {
	domain.Job
	Nodes []domain.NodeExecution `json:"nodes,omitempty"`
}

// JobActionRequest is the body of cancel, retry, pause and resume.
type JobActionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// ResolveJobRequest leaves needs_attention (§11.3). retry_node and skip_node name
// the node; publication nodes require publisher.
type ResolveJobRequest struct {
	Action domain.ResolveAction `json:"action"`
	NodeID string               `json:"node_id,omitempty"`
	Note   string               `json:"note,omitempty"`
}

// ---- Uploads ----

// CreateUploadRequest starts a resumable upload (§5.4, I20).
type CreateUploadRequest struct {
	Filename   string `json:"filename"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	ChunkBytes int64  `json:"chunk_bytes,omitempty"`
}

// DefaultChunkBytes is the default upload chunk size.
const DefaultChunkBytes = 16 << 20

// ---- Assets ----

// AssetQuery filters GET /assets.
type AssetQuery struct {
	PageQuery
	Kind       domain.AssetKind   `json:"kind,omitempty"`
	Status     domain.AssetStatus `json:"status,omitempty"`
	SourceID   string             `json:"source_id,omitempty"`
	Collection string             `json:"collection,omitempty"`
}

// UpdateAssetRequest edits metadata. It never creates a media version (D35).
// Changing public metadata of a public video needs a new approval (D7).
type UpdateAssetRequest struct {
	Title       *string   `json:"title,omitempty"`
	Description *string   `json:"description,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
	Category    *string   `json:"category,omitempty"`
	Collection  *string   `json:"collection,omitempty"`
}

// LineageResponse answers GET /assets/{id}/lineage.
type LineageResponse struct {
	AssetID  id.Friendly     `json:"asset_id"`
	Lineage  *domain.Lineage `json:"lineage,omitempty"`
	Children []id.Friendly   `json:"children,omitempty"`
}

// SetAIProcessingRequest changes the external AI policy; the server writes the
// consent record with who and when (D8).
type SetAIProcessingRequest struct {
	AIProcessing domain.AIProcessing `json:"ai_processing"`
}

// ConsentResponse answers GET /assets/{id}/consent.
type ConsentResponse struct {
	AIProcessing domain.AIProcessing `json:"ai_processing"`
	Consent      *domain.AIConsent   `json:"consent"`
}

// ProxyLinkResponse answers GET /assets/{id}/proxy-link, for CLI review (D24).
type ProxyLinkResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SensitiveSegmentRequest marks or unmarks an interval (D30).
type SensitiveSegmentRequest struct {
	Version      string  `json:"version,omitempty"`
	StartSeconds float64 `json:"start_seconds"`
	EndSeconds   float64 `json:"end_seconds"`
	Reason       string  `json:"reason,omitempty"`
}

// ---- Clips and search ----

// ClipMode selects how a story is found (§3.2).
type ClipMode string

const (
	ClipCompleteStory ClipMode = "complete_story"
	ClipShort         ClipMode = "short"
	ClipBestMoments   ClipMode = "best_moments"
)

// CreateClipRequest requests a semantic clip: a thin wrapper over running the
// create-clip workflow.
type CreateClipRequest struct {
	SourceID              string   `json:"source_id"`
	Prompt                string   `json:"prompt"`
	Mode                  ClipMode `json:"mode,omitempty"`
	TargetDurationSeconds float64  `json:"target_duration_seconds,omitempty"`
	Destinations          []string `json:"destinations,omitempty"`
	WorkflowVersion       string   `json:"workflow_version,omitempty"`
}

// SearchQuery is GET /search. Mode is "hybrid" by default, or "lexical" or "semantic".
type SearchQuery struct {
	Q       string `json:"q"`
	AssetID string `json:"asset_id,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// SearchResponse answers GET /search (R41).
type SearchResponse struct {
	Items          []domain.SearchHit `json:"items"`
	Mode           string             `json:"mode"`
	EmbeddingModel string             `json:"embedding_model,omitempty"`
}

// ---- Approvals ----

// ApprovalQuery filters GET /approvals.
type ApprovalQuery struct {
	PageQuery
	Status  domain.ApprovalStatus `json:"status,omitempty"`
	Kind    domain.ApprovalKind   `json:"kind,omitempty"`
	AssetID string                `json:"asset_id,omitempty"`
}

// ApproveRequest decides a gate. StartSeconds and EndSeconds adjust a single
// interval; Segments adjust several. Ignored unless allow_adjust is set (§3.2).
type ApproveRequest struct {
	StartSeconds *float64         `json:"start_seconds,omitempty"`
	EndSeconds   *float64         `json:"end_seconds,omitempty"`
	Segments     []domain.Segment `json:"segments,omitempty"`
	Note         string           `json:"note,omitempty"`
}

// RejectRequest rejects a gate.
type RejectRequest struct {
	Reason string `json:"reason"`
}

// ---- Publications ----

// CreatePublicationRequest requests a publication. It runs publish-video.
type CreatePublicationRequest struct {
	AssetID      string            `json:"asset_id"`
	Destinations []string          `json:"destinations"`
	Visibility   domain.Visibility `json:"visibility,omitempty"`
}

// PublicationQuery filters GET /publications.
type PublicationQuery struct {
	PageQuery
	AssetID     string                   `json:"asset_id,omitempty"`
	Destination string                   `json:"destination,omitempty"`
	Status      domain.PublicationStatus `json:"status,omitempty"`
}

// WithdrawRequest withdraws by asset (`vidyom unpublish`, §11.4). Immediate and
// idempotent (D17).
type WithdrawRequest struct {
	AssetID      string   `json:"asset_id"`
	Destinations []string `json:"destinations,omitempty"`
	All          bool     `json:"all,omitempty"`
}

// DestinationInfo is an entry of GET /publishing/destinations.
type DestinationInfo struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Visibility domain.Visibility `json:"visibility,omitempty"`
	Available  bool              `json:"available"`
	Reason     string            `json:"reason,omitempty"`
}

// ---- Introspection ----

// RunNodeRequest is `vidyom node run`: an ad-hoc single-node job.
type RunNodeRequest struct {
	Parameters json.RawMessage `json:"parameters"`
	AssetID    string          `json:"asset_id,omitempty"`
}

// ConnectorList answers GET /connectors.
type ConnectorList = List[connector.Info]

// InstallPluginRequest installs from a directory on the server (§11.5).
type InstallPluginRequest struct {
	Path string `json:"path"`
}

// HookInfo is an entry of GET /hooks?workflow=.
type HookInfo struct {
	ID     string `json:"id"`
	Point  string `json:"point"`
	Level  string `json:"level"` // "global", "workflow" or "node"
	Plugin string `json:"plugin"`
	Action string `json:"action"`
}

// TestHookRequest runs one hook on demand.
type TestHookRequest struct {
	AssetID string `json:"asset_id,omitempty"`
}

// EventQuery filters GET /events and GET /events/stream.
type EventQuery struct {
	PageQuery
	Since string     `json:"since,omitempty"`
	Type  event.Type `json:"type,omitempty"`
}

// DeliveryQuery filters GET /events/deliveries.
type DeliveryQuery struct {
	PageQuery
	Status event.DeliveryStatus `json:"status,omitempty"`
}

// ---- Storage ----

// StorageInfo is an entry of GET /storages. Never includes credentials.
type StorageInfo struct {
	Role         domain.StorageRole            `json:"role"`
	Driver       string                        `json:"driver"`
	Usage        string                        `json:"usage,omitempty"`
	Capabilities connector.StorageCapabilities `json:"capabilities"`
}

// StorageTestResponse answers POST /storages/{role}/test.
type StorageTestResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// StorageVerifyRequest verifies an asset's objects in a role.
type StorageVerifyRequest struct {
	AssetID string `json:"asset_id"`
}

// StorageVerifyResponse is the per-object outcome.
type StorageVerifyResponse struct {
	OK      bool     `json:"ok"`
	Checked int      `json:"checked"`
	Failed  []string `json:"failed,omitempty"`
}

// StorageCopyRequest copies an asset between storage instances. Migration is a
// separate operation from a configuration change (§7.1).
type StorageCopyRequest struct {
	From    domain.StorageRole `json:"from"`
	To      domain.StorageRole `json:"to"`
	AssetID string             `json:"asset_id"`
}

// StorageRestoreRequest restores from archive. Confirm must be set after the cost
// was shown (§3.3).
type StorageRestoreRequest struct {
	AssetID string             `json:"asset_id"`
	To      domain.StorageRole `json:"to"`
	Confirm bool               `json:"confirm,omitempty"`
}

// StorageRestorePreview answers POST /storages/restore with confirm unset: what the
// restore would read, so the cost is shown before confirming (§3.3, I132). Nothing
// has started. EstimateMinor is absent when the tenant has no cost table entry.
type StorageRestorePreview struct {
	ConfirmRequired bool               `json:"confirm_required"`
	AssetID         id.Friendly        `json:"asset_id"`
	To              domain.StorageRole `json:"to"`
	ArchiveBytes    int64              `json:"archive_bytes"`
	EstimateMinor   *int64             `json:"estimate_minor,omitempty"`
	Currency        string             `json:"currency,omitempty"`
	Note            string             `json:"note,omitempty"`
}

// ---- Secrets ----

// PutSecretRequest stores a server or delegated secret. The CLI reads the value
// through a no-echo prompt or stdin, never an argument (R36). Client-location
// secrets never reach this route.
type PutSecretRequest struct {
	Name       string                `json:"name"`
	Location   domain.SecretLocation `json:"location"`
	Value      string                `json:"value"`
	TTLSeconds int                   `json:"ttl_seconds,omitempty"`
}

// RotateSecretRequest replaces a value.
type RotateSecretRequest struct {
	Value string `json:"value"`
}

// SecretTestResponse answers POST /secrets/{name}/test.
type SecretTestResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// ---- Pairing and devices ----

// CreatePairingCodeRequest generates a one-time code (D12).
type CreatePairingCodeRequest struct {
	UserEmail  string `json:"user_email,omitempty"` // defaults to the caller
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// PairingCodeResponse carries the code once, and the TLS pin when the listener
// uses a self-signed certificate (I18).
type PairingCodeResponse struct {
	Code           string    `json:"code"`
	ExpiresAt      time.Time `json:"expires_at"`
	TLSFingerprint string    `json:"tls_fingerprint,omitempty"`
}

// PairDeviceRequest exchanges a code. It is the only unauthenticated mutating call.
type PairDeviceRequest struct {
	Code       string `json:"code"`
	DeviceName string `json:"device_name,omitempty"`
}

// DeviceCredentialResponse carries a credential once, at pairing or rotation. The
// CLI stores it in the OS keyring (§9.1).
type DeviceCredentialResponse struct {
	Device domain.Device `json:"device"`
	Token  string        `json:"token"`
}

// ---- Tenants, users, budget ----

// TenantInfo is an entry of GET /tenants: a tenant with the caller's role.
type TenantInfo struct {
	domain.Tenant
	Role domain.Role `json:"role,omitempty"`
}

// CreateTenantRequest creates a tenant (system admin).
type CreateTenantRequest struct {
	ID   domain.TenantID `json:"id"`
	Name string          `json:"name,omitempty"`
}

// AddUserRequest adds a user to the header's tenant with a role.
type AddUserRequest struct {
	Email string      `json:"email"`
	Role  domain.Role `json:"role"`
}

// GrantRequest grants or revokes one permission (D48).
type GrantRequest struct {
	Permission domain.Permission `json:"permission"`
	Revoke     bool              `json:"revoke,omitempty"`
}

// SetBudgetRequest sets the monthly budget (D19).
type SetBudgetRequest struct {
	MonthlyMinor int64  `json:"monthly_minor"`
	Currency     string `json:"currency"`
}

// BudgetOverrideRequest lets paid operations run past 100%; admin and audited.
type BudgetOverrideRequest struct {
	Reason     string `json:"reason"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// AuditQuery filters GET /audit.
type AuditQuery struct {
	PageQuery
	Since    string `json:"since,omitempty"`
	Actor    string `json:"actor,omitempty"`
	Action   string `json:"action,omitempty"`
	Resource string `json:"resource,omitempty"`
}

// AuditExportResponse answers POST /audit/export.
type AuditExportResponse struct {
	Key string `json:"key"` // role-relative key in archive
}

// LogQuery filters GET /logs. Local logs stay available with no external sink (§10.1).
type LogQuery struct {
	PageQuery
	Since string `json:"since,omitempty"`
	Level string `json:"level,omitempty"`
	JobID string `json:"job_id,omitempty"`
}

// ObservabilitySink is an entry of GET /observability.
type ObservabilitySink struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // "log", "metrics" or "trace"
	Driver    string `json:"driver"`
	Enabled   bool   `json:"enabled"`
	Status    string `json:"status,omitempty"`
	Backlog   int64  `json:"backlog,omitempty"`
	Dropped   int64  `json:"dropped,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// ---- Server-Sent Events (I3) ----

// SSE event names.
const (
	SSEJobEvent    = "job"   // data: domain.JobEvent; id: its Seq
	SSEEvent       = "event" // data: event.Envelope; id: its outbox sequence
	SSELog         = "log"   // data: connector.LogRecord
	SSEEnd         = "end"   // the stream ended normally, e.g. a terminal job
	SSEKeepAlive   = 15 * time.Second
	SSEContentType = "text/event-stream"
)
