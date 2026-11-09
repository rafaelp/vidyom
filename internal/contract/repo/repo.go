// Package repo defines the repository layer over SQLite (D5): one database per
// tenant plus a global system.db. Only the daemon opens these files; workers go
// through the daemon's API. The interfaces keep PostgreSQL possible later (§5.1).
//
// Component core implements them. Methods take a context, return errs-coded
// errors (errs.CodeNotFound when a reference does not resolve in this tenant)
// and resolve an id.Ref as either friendly ID or ULID.
package repo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
)

// Page requests a page of a list. A zero Limit means the implementation default.
type Page struct {
	Limit  int
	Cursor string
}

// ---- System database ----

// SystemRepos are the repositories of system.db.
type SystemRepos interface {
	Tenants() TenantRepo
	Users() UserRepo
	Devices() DeviceRepo
	Pairing() PairingRepo
	Plugins() PluginRepo
	WrappedKeys() WrappedKeyRepo
	Workers() WorkerRepo
}

// SystemStore is system.db. InTx commits everything fn does atomically.
type SystemStore interface {
	SystemRepos
	InTx(ctx context.Context, fn func(SystemRepos) error) error
	Close() error
}

// TenantRepo manages tenants.
type TenantRepo interface {
	Create(ctx context.Context, t domain.Tenant) error
	Get(ctx context.Context, tenant domain.TenantID) (domain.Tenant, error)
	List(ctx context.Context) ([]domain.Tenant, error)
}

// UserRepo manages users and their per-tenant roles and grants (D6, D48).
type UserRepo interface {
	Create(ctx context.Context, u domain.User) error
	GetByEmail(ctx context.Context, email string) (domain.User, error)
	Get(ctx context.Context, user id.ULID) (domain.User, error)
	ListMembers(ctx context.Context, tenant domain.TenantID) ([]domain.Membership, error)
	SetMembership(ctx context.Context, m domain.Membership) error
	RemoveMembership(ctx context.Context, user id.ULID, tenant domain.TenantID) error
	Membership(ctx context.Context, user id.ULID, tenant domain.TenantID) (domain.Membership, error)
	Memberships(ctx context.Context, user id.ULID) ([]domain.Membership, error)
}

// DeviceRepo manages paired clients (D12). Only credential hashes are stored.
type DeviceRepo interface {
	// NextID allocates a CLIENT-nnn from the system-wide sequence (I14).
	NextID(ctx context.Context) (id.Friendly, error)
	Create(ctx context.Context, d domain.Device) error
	Get(ctx context.Context, ref id.Ref) (domain.Device, error)
	GetByCredentialHash(ctx context.Context, hash string) (domain.Device, error)
	List(ctx context.Context, user id.ULID) ([]domain.Device, error) // empty user lists all
	Update(ctx context.Context, d domain.Device) error
}

// PairingCode is a stored one-time code; only its hash is kept.
type PairingCode struct {
	Hash      string
	UserULID  id.ULID
	CreatedBy string
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// PairingRepo stores one-time pairing codes.
type PairingRepo interface {
	Create(ctx context.Context, c PairingCode) error
	// Consume atomically marks a valid unused code as used and returns it. A
	// second call with the same hash fails with errs.CodeNotFound.
	Consume(ctx context.Context, hash string, at time.Time) (PairingCode, error)
}

// PluginRepo is the plugin registry (§5.1).
type PluginRepo interface {
	Upsert(ctx context.Context, p domain.PluginRecord) error
	Get(ctx context.Context, pluginID string) (domain.PluginRecord, error)
	List(ctx context.Context) ([]domain.PluginRecord, error)
	Delete(ctx context.Context, pluginID string) error
}

// WrappedKey is a tenant data key wrapped by the master key (D13).
type WrappedKey struct {
	TenantID  domain.TenantID
	Version   int
	Wrapped   []byte
	Active    bool
	CreatedAt time.Time
}

// WrappedKeyRepo stores wrapped tenant data keys.
type WrappedKeyRepo interface {
	Put(ctx context.Context, k WrappedKey) error
	Active(ctx context.Context, tenant domain.TenantID) (WrappedKey, error)
	Get(ctx context.Context, tenant domain.TenantID, version int) (WrappedKey, error)
	List(ctx context.Context, tenant domain.TenantID) ([]WrappedKey, error)
}

// WorkerRepo stores worker tokens (as hashes) and registrations (I4).
type WorkerRepo interface {
	PutTokenHash(ctx context.Context, name, hash string) error
	HasTokenHash(ctx context.Context, hash string) (bool, error)
	Upsert(ctx context.Context, w domain.Worker) error
	List(ctx context.Context) ([]domain.Worker, error)
	Delete(ctx context.Context, workerID string) error
}

// ---- Tenant databases ----

// TenantStores opens tenant databases. Implementations cache open handles.
type TenantStores interface {
	// Create initializes a new tenant database, applying all migrations.
	Create(ctx context.Context, tenant domain.TenantID) (TenantStore, error)
	// ForTenant opens an existing tenant database. An unknown tenant fails with
	// errs.CodeNotFound, so no request can create one by accident.
	ForTenant(ctx context.Context, tenant domain.TenantID) (TenantStore, error)
	// Tenants lists tenants with a database, for startup reconciliation (D4).
	Tenants(ctx context.Context) ([]domain.TenantID, error)
	Close() error
}

// TenantRepos are the repositories of one tenant.db.
type TenantRepos interface {
	IDs() IDAllocator
	Assets() AssetRepo
	Versions() VersionRepo
	Collections() CollectionRepo
	Transcripts() TranscriptRepo
	Search() SearchRepo
	Jobs() JobRepo
	Approvals() ApprovalRepo
	Publications() PublicationRepo
	Uploads() UploadRepo
	Audit() AuditRepo
	Outbox() OutboxRepo
	Secrets() SecretRepo
	Budgets() BudgetRepo
	Idempotency() IdempotencyRepo
}

// TenantStore is one tenant.db. InTx is how a state change and its outbox event
// commit together (§8.2, D5).
type TenantStore interface {
	TenantRepos
	Tenant() domain.TenantID
	InTx(ctx context.Context, fn func(TenantRepos) error) error
}

// IDAllocator allocates friendly IDs transactionally; two clips created at the
// same time never get the same ID, and an ID is never reused (R5, D46).
type IDAllocator interface {
	Next(ctx context.Context, prefix id.Prefix) (id.Friendly, error)
}

// AssetFilter narrows AssetRepo.List.
type AssetFilter struct {
	Kind           domain.AssetKind
	Status         domain.AssetStatus
	SourceID       id.Ref // derivatives of this asset
	Collection     string
	IncludeDeleted bool
}

// AssetRepo is the canonical catalog (D24). Files, transcript info and
// publications on Asset are denormalized from their own repositories on read.
type AssetRepo interface {
	Create(ctx context.Context, a domain.Asset) error
	Get(ctx context.Context, ref id.Ref) (domain.Asset, error)
	List(ctx context.Context, f AssetFilter, p Page) ([]domain.Asset, string, error)
	// Update writes the mutable fields. It rejects an invalid status transition
	// with errs.CodeFailedPrecondition.
	Update(ctx context.Context, a domain.Asset) error
	AppendHistory(ctx context.Context, h domain.HistoryEntry) error
	History(ctx context.Context, ref id.Ref, p Page) ([]domain.HistoryEntry, string, error)
	// SoftDelete sets deleted_at; the friendly ID stays burned (D17).
	SoftDelete(ctx context.Context, ref id.Ref, at time.Time) error
	// ClipsOverlapping lists derivatives of a source whose lineage overlaps the
	// segments, for the D47 warning.
	ClipsOverlapping(ctx context.Context, source id.Ref, segments []domain.Segment) ([]domain.Asset, error)
}

// VersionRepo manages media versions (D35) and permanent provenance (R21).
type VersionRepo interface {
	Create(ctx context.Context, v domain.AssetVersion) error
	Get(ctx context.Context, asset id.Ref, version string) (domain.AssetVersion, error)
	List(ctx context.Context, asset id.Ref) ([]domain.AssetVersion, error)
	// PutFile records a file, replacing the same purpose and name. It fails with
	// errs.CodeFailedPrecondition on an immutable version.
	PutFile(ctx context.Context, asset id.Ref, version string, f domain.VersionFile) error
	MarkImmutable(ctx context.Context, asset id.Ref, version string) error
	SetProvenance(ctx context.Context, asset id.Ref, version string, p domain.Provenance) error
}

// CollectionRepo manages collections.
type CollectionRepo interface {
	Get(ctx context.Context, name string) (domain.Collection, error)
	List(ctx context.Context) ([]domain.Collection, error)
	Upsert(ctx context.Context, c domain.Collection) error
}

// TranscriptRepo stores transcripts and analysis (private by default, D30).
type TranscriptRepo interface {
	Save(ctx context.Context, t domain.Transcript) error
	Get(ctx context.Context, asset id.Ref, version string) (domain.Transcript, error)
	SaveAnalysis(ctx context.Context, a domain.Analysis) error
	Analysis(ctx context.Context, asset id.Ref, version string) (domain.Analysis, error)
	MarkSensitive(ctx context.Context, s domain.SensitiveSegment) error
	UnmarkSensitive(ctx context.Context, asset id.Ref, version string, start, end float64) error
	Sensitive(ctx context.Context, asset id.Ref, version string) ([]domain.SensitiveSegment, error)
}

// LexicalQuery is an FTS5 query.
type LexicalQuery struct {
	Text    string
	AssetID id.Ref // empty searches the tenant
	Limit   int
}

// VectorQuery is a sqlite-vec KNN query (D5). Only vectors of Model are searched
// (R18). Asset-scoped is the main path; tenant-wide has a scan cap (I43).
type VectorQuery struct {
	Vector  []float32
	Model   string
	AssetID id.Ref
	Limit   int
}

// EmbeddingBatch writes vectors for one asset version.
type EmbeddingBatch struct {
	AssetID  id.Ref
	Version  string
	Model    string // "<name>@<version>"
	Chunking string // segmentation strategy (R18)
	Chunks   []domain.EmbeddedChunk
}

// SearchRepo is FTS5 and sqlite-vec (D5). Side-by-side reindexing: write the new
// model's vectors, then SetActiveModel swaps searches over only when complete
// (D15).
type SearchRepo interface {
	IndexLexical(ctx context.Context, asset id.Ref, version string, chunks []domain.SearchChunk) error
	IndexVectors(ctx context.Context, batch EmbeddingBatch) error
	SearchLexical(ctx context.Context, q LexicalQuery) ([]domain.SearchHit, error)
	SearchVector(ctx context.Context, q VectorQuery) ([]domain.SearchHit, error)
	DeleteAsset(ctx context.Context, asset id.Ref) error
	DeleteModel(ctx context.Context, model string) error
	ActiveModel(ctx context.Context) (string, error)
	SetActiveModel(ctx context.Context, model string) error
	HasIndex(ctx context.Context, asset id.Ref, version, model string) (lexical, vector bool, err error)
}

// JobFilter narrows JobRepo.List.
type JobFilter struct {
	Status     domain.JobStatus
	WorkflowID string
	AssetID    id.Ref
	ParentJob  id.Ref
}

// JobRepo holds job state, the source of truth (D4), and checkpoints (D11).
type JobRepo interface {
	Create(ctx context.Context, j domain.Job) error
	Get(ctx context.Context, ref id.Ref) (domain.Job, error)
	GetByULID(ctx context.Context, job id.ULID) (domain.Job, error)
	FindByIdempotencyKey(ctx context.Context, key string) (domain.Job, bool, error)
	List(ctx context.Context, f JobFilter, p Page) ([]domain.Job, string, error)
	// Update writes the job. It rejects an invalid status transition with
	// errs.CodeFailedPrecondition.
	Update(ctx context.Context, j domain.Job) error
	// ListActive returns non-terminal jobs, for startup reconciliation (D4).
	ListActive(ctx context.Context) ([]domain.Job, error)

	PutNode(ctx context.Context, n domain.NodeExecution) error
	GetNode(ctx context.Context, job id.ULID, nodeID string) (domain.NodeExecution, error)
	GetNodeByTask(ctx context.Context, taskID string) (domain.NodeExecution, error)
	ListNodes(ctx context.Context, job id.ULID) ([]domain.NodeExecution, error)

	// AppendEvent assigns the next Seq within the job.
	AppendEvent(ctx context.Context, e domain.JobEvent) (int64, error)
	ListEvents(ctx context.Context, job id.Ref, afterSeq int64, limit int) ([]domain.JobEvent, error)

	// DeleteFinishedBefore removes job records past retention (§10.4). Provenance
	// lives on the asset version and is untouched (R21).
	DeleteFinishedBefore(ctx context.Context, t time.Time) (int64, error)
}

// ApprovalFilter narrows ApprovalRepo.List.
type ApprovalFilter struct {
	Status  domain.ApprovalStatus
	Kind    domain.ApprovalKind
	AssetID id.Ref
}

// ApprovalDecision is one decision on a pending approval.
type ApprovalDecision struct {
	Status    domain.ApprovalStatus
	DecidedBy string
	DecidedAt time.Time
	Reason    string
	Segments  []domain.Segment
}

// ApprovalRepo stores approvals (R25).
type ApprovalRepo interface {
	Create(ctx context.Context, a domain.Approval) error
	Get(ctx context.Context, ref id.Ref) (domain.Approval, error)
	GetForNode(ctx context.Context, job id.ULID, nodeID string) (domain.Approval, bool, error)
	List(ctx context.Context, f ApprovalFilter, p Page) ([]domain.Approval, string, error)
	// Decide applies a decision to a pending record; otherwise it fails with
	// errs.CodeFailedPrecondition, so one approval is decided once.
	Decide(ctx context.Context, ref id.Ref, d ApprovalDecision) (domain.Approval, error)
	ExpiringBefore(ctx context.Context, t time.Time) ([]domain.Approval, error)
}

// PublicationFilter narrows PublicationRepo.List.
type PublicationFilter struct {
	AssetID     id.Ref
	Destination string
	Status      domain.PublicationStatus
}

// PublicationRepo stores publications (§2.1).
type PublicationRepo interface {
	Create(ctx context.Context, p domain.Publication) error
	Get(ctx context.Context, ref id.Ref) (domain.Publication, error)
	FindByIdempotencyKey(ctx context.Context, key string) (domain.Publication, bool, error)
	List(ctx context.Context, f PublicationFilter, pg Page) ([]domain.Publication, string, error)
	// Update rejects a record that fails Publication.Validate (R43).
	Update(ctx context.Context, p domain.Publication) error
	// ListPublic returns publications that are published and public: the only
	// source of public.db (D18).
	ListPublic(ctx context.Context) ([]domain.Publication, error)
}

// UploadRepo tracks resumable uploads (§5.4).
type UploadRepo interface {
	Create(ctx context.Context, u domain.Upload) error
	Get(ctx context.Context, ref id.Ref) (domain.Upload, error)
	Update(ctx context.Context, u domain.Upload) error
	ListExpired(ctx context.Context, t time.Time) ([]domain.Upload, error)
	Delete(ctx context.Context, ref id.Ref) error
}

// AuditFilter narrows AuditRepo.List.
type AuditFilter struct {
	Since    *time.Time
	Actor    string
	Action   string
	Resource string
}

// AuditRepo is the append-only hash-chained table (D23). There is no update or
// delete method except the retention prune of whole, exported chain prefixes.
type AuditRepo interface {
	// Append assigns Seq, PrevHash and Hash in the same transaction.
	Append(ctx context.Context, r domain.AuditRecord) (domain.AuditRecord, error)
	List(ctx context.Context, f AuditFilter, p Page) ([]domain.AuditRecord, string, error)
	// Scan reads records with Seq greater than afterSeq, in order, for verify and export.
	Scan(ctx context.Context, afterSeq int64, limit int) ([]domain.AuditRecord, error)
}

// OutboxFilter narrows event listings.
type OutboxFilter struct {
	Since *time.Time
	Type  event.Type
}

// DeliveryFilter narrows delivery listings.
type DeliveryFilter struct {
	Status     event.DeliveryStatus
	Subscriber string
}

// OutboxRepo is the transactional outbox in the tenant database (§8.2, D5).
type OutboxRepo interface {
	// Append is called inside the transaction that made the state change.
	Append(ctx context.Context, e event.Envelope) (int64, error)
	// Pending returns undispatched events after a sequence, oldest first.
	Pending(ctx context.Context, limit int) ([]OutboxEntry, error)
	MarkDispatched(ctx context.Context, seq int64) error
	// PutDelivery upserts one subscriber's delivery record.
	PutDelivery(ctx context.Context, d event.Delivery) error
	DueDeliveries(ctx context.Context, now time.Time, limit int) ([]event.Delivery, error)
	GetEvent(ctx context.Context, eventID id.ULID) (event.Envelope, error)
	ListEvents(ctx context.Context, f OutboxFilter, p Page) ([]event.Envelope, string, error)
	ListDeliveries(ctx context.Context, f DeliveryFilter, p Page) ([]event.Delivery, string, error)
	// Stream reads events after a sequence, for GET /events/stream resume.
	Stream(ctx context.Context, afterSeq int64, limit int) ([]OutboxEntry, error)
}

// OutboxEntry is an event with its tenant-local sequence.
type OutboxEntry struct {
	Seq   int64
	Event event.Envelope
}

// SecretRecord is one vault row: ciphertext only (D13).
type SecretRecord struct {
	Meta       domain.SecretMeta
	Ciphertext []byte
	KeyVersion int
}

// SecretRepo stores encrypted secrets. Only the vault uses it.
type SecretRepo interface {
	Put(ctx context.Context, r SecretRecord) error
	Get(ctx context.Context, name string) (SecretRecord, error)
	List(ctx context.Context) ([]domain.SecretMeta, error)
	Delete(ctx context.Context, name string) error
	TouchUsed(ctx context.Context, name string, at time.Time) error
}

// BudgetRepo stores budgets and usage (D19).
type BudgetRepo interface {
	Get(ctx context.Context) (domain.Budget, bool, error)
	Set(ctx context.Context, b domain.Budget) error
	RecordUsage(ctx context.Context, u domain.UsageRecord) error
	// SpentMinor sums estimates in a period "2026-11".
	SpentMinor(ctx context.Context, period string) (int64, error)
	ListUsage(ctx context.Context, period string, p Page) ([]domain.UsageRecord, string, error)
	SetOverride(ctx context.Context, until time.Time, by, reason string) error
	Override(ctx context.Context) (*time.Time, error)
}

// IdempotentResponse is a remembered mutation result (§12.1).
type IdempotentResponse struct {
	Key         string
	RequestHash string
	Status      int
	Body        json.RawMessage
	CreatedAt   time.Time
}

// IdempotencyRepo remembers mutating API requests by key. The same key with a
// different request is errs.CodeIdempotencyMismatch at the API.
type IdempotencyRepo interface {
	Get(ctx context.Context, key string) (IdempotentResponse, bool, error)
	Put(ctx context.Context, r IdempotentResponse) error
	DeleteBefore(ctx context.Context, t time.Time) (int64, error)
}

// ---- Optional capabilities ----
//
// Implementations may offer these beside the main interfaces. Callers discover
// them by type assertion (docs/decisions/core.md, I101).

// SystemAuditProvider is implemented by a SystemRepos that keeps a hash-chained
// audit for actions with no tenant: pairing, device changes, tenant creation, the
// boot record (I108).
type SystemAuditProvider interface {
	SystemAudit() AuditRepo
}

// WorkerTokenLookup is implemented by a WorkerRepo that can name a token's owner.
type WorkerTokenLookup interface {
	TokenName(ctx context.Context, hash string) (name string, ok bool, err error)
}

// AuditExport is one completed export of a chain segment (D23).
type AuditExport struct {
	FromSeq  int64
	ToSeq    int64
	HeadHash string // Hash of the record at ToSeq
	Key      string // role-relative key in archive
	SHA256   string // of the exported file
	At       time.Time
}

// AuditAnchor is where the verifiable chain starts after a prune: the last pruned
// record's sequence and hash. A zero value means nothing has been pruned.
type AuditAnchor struct {
	Seq  int64
	Hash string
}

// AuditMaintainer is implemented by an AuditRepo that supports the export
// watermark and the retention prune of whole, exported chain prefixes (D23).
type AuditMaintainer interface {
	LastExport(ctx context.Context) (AuditExport, bool, error)
	RecordExport(ctx context.Context, e AuditExport) error
	ListExports(ctx context.Context) ([]AuditExport, error)
	Anchor(ctx context.Context) (AuditAnchor, error)
	// PruneExportedBefore removes records older than t that are at or below the
	// last export, keeping an anchor. It returns how many it removed.
	PruneExportedBefore(ctx context.Context, t time.Time) (int64, error)
}

// AIPolicySource says who decided an asset's effective ai_processing (I104).
type AIPolicySource string

const (
	AIPolicyDefault    AIPolicySource = "default"
	AIPolicyAsset      AIPolicySource = "asset"
	AIPolicyCollection AIPolicySource = "collection"
)

// AssetAIPolicyWriter is implemented by an AssetRepo. AssetRepo.Update never
// writes ai_processing or ai_consent, so only these methods change the policy
// (D8, I104). external_allowed requires a consent record.
type AssetAIPolicyWriter interface {
	SetAIPolicy(ctx context.Context, ref id.Ref, processing domain.AIProcessing, consent *domain.AIConsent, source AIPolicySource) error
	// AIPolicySource reports who decided an asset's policy.
	AIPolicySource(ctx context.Context, ref id.Ref) (AIPolicySource, error)
	// ApplyCollectionPolicy rewrites every member of a collection not sourced from
	// an asset-level decision, returning how many changed.
	ApplyCollectionPolicy(ctx context.Context, collection string, processing domain.AIProcessing, consent *domain.AIConsent) (int64, error)
}

// OutboxMaintainer is implemented by an OutboxRepo that can drop dispatched events
// past retention, with their delivery records (I118).
type OutboxMaintainer interface {
	DeleteDispatchedBefore(ctx context.Context, t time.Time) (int64, error)
}

// EmbeddingLister is implemented by a SearchRepo that can read stored vectors
// back. The public.db export uses it to carry vectors without recomputing them
// (D18, I232); without it the export embeds the released text again, or goes
// lexical-only. It returns the chunks of one asset version for one model, ordered
// by start time, and an empty list when there are none.
type EmbeddingLister interface {
	ListEmbeddings(ctx context.Context, asset id.Ref, version, model string) ([]domain.EmbeddedChunk, error)
}
