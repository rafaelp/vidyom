package domain

import (
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/id"
)

// AssetKind is the asset type (§2.1, R1). It is distinct from the storage
// prefix and from the content category.
type AssetKind string

const (
	KindOriginal AssetKind = "original"
	KindClip     AssetKind = "clip"
	KindEdited   AssetKind = "edited"
)

// IDPrefix returns the friendly ID prefix for the kind.
func (k AssetKind) IDPrefix() id.Prefix {
	switch k {
	case KindClip:
		return id.PrefixClip
	case KindEdited:
		return id.PrefixEdited
	default:
		return id.PrefixOriginal
	}
}

// StoragePrefix returns the role-relative key prefix (§14.1).
func (k AssetKind) StoragePrefix() string {
	switch k {
	case KindClip:
		return "clips"
	case KindEdited:
		return "edited"
	default:
		return "originals"
	}
}

// AssetStatus is the lifecycle state of §2.5. Asset state, job state and
// visibility are independent axes.
type AssetStatus string

const (
	AssetRegistered       AssetStatus = "registered"
	AssetIngesting        AssetStatus = "ingesting"
	AssetReadyForClipping AssetStatus = "ready_for_clipping"
	AssetIngestFailed     AssetStatus = "ingest_failed"
	AssetEditorialReview  AssetStatus = "editorial_review"
	AssetApproved         AssetStatus = "approved"
	AssetRejected         AssetStatus = "rejected"
	AssetPublished        AssetStatus = "published"
	AssetWithdrawn        AssetStatus = "withdrawn"
	AssetArchivedOnly     AssetStatus = "archived_only"
)

// AssetStatuses lists every asset state.
var AssetStatuses = []AssetStatus{
	AssetRegistered, AssetIngesting, AssetReadyForClipping, AssetIngestFailed,
	AssetEditorialReview, AssetApproved, AssetRejected, AssetPublished,
	AssetWithdrawn, AssetArchivedOnly,
}

var assetTransitions = map[AssetStatus][]AssetStatus{
	AssetRegistered:       {AssetIngesting, AssetIngestFailed},
	AssetIngesting:        {AssetReadyForClipping, AssetIngestFailed},
	AssetIngestFailed:     {AssetIngesting},
	AssetReadyForClipping: {AssetEditorialReview, AssetArchivedOnly},
	AssetEditorialReview:  {AssetApproved, AssetRejected, AssetArchivedOnly},
	AssetApproved:         {AssetPublished, AssetRejected, AssetEditorialReview, AssetArchivedOnly},
	AssetRejected:         {AssetEditorialReview, AssetArchivedOnly},
	AssetPublished:        {AssetWithdrawn, AssetEditorialReview},
	AssetWithdrawn:        {AssetEditorialReview, AssetPublished, AssetArchivedOnly},
	AssetArchivedOnly:     {AssetReadyForClipping},
}

// CanTransition reports whether the state machine allows moving to next.
// Repeating the current state is allowed, so retried nodes stay idempotent.
func (s AssetStatus) CanTransition(next AssetStatus) bool {
	if s == next {
		return true
	}
	for _, allowed := range assetTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Visibility describes only the public library and the destinations; assets
// are born private (§2.5).
type Visibility string

const (
	VisibilityPrivate  Visibility = "private"
	VisibilityUnlisted Visibility = "unlisted"
	VisibilityPublic   Visibility = "public"
)

// AIProcessing is the per-asset or per-collection external AI policy (D8).
type AIProcessing string

const (
	AILocalOnly       AIProcessing = "local_only"
	AIExternalAllowed AIProcessing = "external_allowed"
)

// CatalogSync is the state of synchronization with optional external catalogs.
// It never blocks readiness or publication (R11).
type CatalogSync string

const (
	CatalogSyncNone    CatalogSync = "none"
	CatalogSyncPending CatalogSync = "pending"
	CatalogSyncSynced  CatalogSync = "synced"
	CatalogSyncFailed  CatalogSync = "failed"
)

// AIConsent is the consent record for external AI processing (D8).
type AIConsent struct {
	AuthorizedBy string    `json:"authorized_by"`
	AuthorizedAt time.Time `json:"authorized_at"`
	Scope        string    `json:"scope"` // "asset" or "collection"
}

// Location is the immutable physical location written at write time (R30), so
// the audit still knows where a file was after a migration.
type Location struct {
	Driver     string `json:"driver"`
	Bucket     string `json:"bucket,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	Generation string `json:"generation,omitempty"`
}

// MediaFile is one stored object of an asset version. Assets reference the
// logical role and the key, never a bucket (§14.1).
type MediaFile struct {
	Storage    StorageRole `json:"storage"`
	Key        string      `json:"key"`
	SHA256     string      `json:"sha256,omitempty"`
	SizeBytes  int64       `json:"size_bytes,omitempty"`
	Profile    string      `json:"profile,omitempty"` // "master-1080p@1"
	Verified   bool        `json:"verified"`
	Location   *Location   `json:"location,omitempty"`
	RecordedAt time.Time   `json:"recorded_at,omitzero"`
}

// URI returns the storage:// form used in workflow parameters.
func (f MediaFile) URI() string {
	return (InputURI{Kind: URIStorage, Role: f.Storage, Key: f.Key}).String()
}

// TranscriptInfo summarizes the indexed transcript on an asset (§12.2).
type TranscriptInfo struct {
	Format         string `json:"format"`
	Indexed        bool   `json:"indexed"`
	TimestampBasis string `json:"timestamp_basis"` // "working_master" (D21)
	Engine         string `json:"engine,omitempty"`
	EmbeddingModel string `json:"embedding_model,omitempty"`
}

// Provenance is the permanent provenance summary of an asset version (R21). It
// belongs to the asset, so job retention never deletes it.
type Provenance struct {
	JobID          id.Friendly       `json:"job_id"`
	Workflow       string            `json:"workflow"` // "ingest-original@1.0.0"
	WorkflowSHA256 string            `json:"workflow_sha256"`
	NodeTypes      map[string]int    `json:"node_types,omitempty"`
	Profiles       []string          `json:"profiles,omitempty"`
	Tools          map[string]string `json:"tools,omitempty"` // ffmpeg, asr and AI model versions
	InputSHA256    map[string]string `json:"input_sha256,omitempty"`
	OutputSHA256   map[string]string `json:"output_sha256,omitempty"`
}

// LineageMethod says how a lineage was established (§2.1).
type LineageMethod string

const (
	LineageComputed LineageMethod = "computed"
	LineageDeclared LineageMethod = "declared" // Future (D50)
)

// Segment is one interval of a source. Seconds accept fractions (R42).
type Segment struct {
	SourceAssetID id.Friendly `json:"source_asset_id"`
	SourceVersion string      `json:"source_version"`
	StartSeconds  float64     `json:"start_seconds"`
	EndSeconds    float64     `json:"end_seconds"`
}

// Lineage records where a derivative comes from (§12.3, D36).
type Lineage struct {
	Method              LineageMethod `json:"method"`
	SourceAssetID       id.Friendly   `json:"source_asset_id"`
	RootAssetID         id.Friendly   `json:"root_asset_id"`
	SelectionApprovalID id.Friendly   `json:"selection_approval_id,omitempty"`
	Segments            []Segment     `json:"segments"`
	// Chain lists the ancestors from the direct parent up to the root (D36).
	Chain []id.Friendly `json:"chain,omitempty"`
}

// Asset is the canonical catalog record (§12.2, §12.3). Files are those of
// CurrentVersion, denormalized for reads; AssetVersion holds the full history.
// Field naming across kinds is unified in I26.
type Asset struct {
	ID             id.Friendly  `json:"id"`
	ULID           id.ULID      `json:"ulid"`
	TenantID       TenantID     `json:"tenant_id"`
	Kind           AssetKind    `json:"kind"`
	Category       string       `json:"category,omitempty"`
	Title          string       `json:"title"`
	Description    string       `json:"description,omitempty"`
	Tags           []string     `json:"tags,omitempty"`
	Collection     string       `json:"collection,omitempty"`
	Visibility     Visibility   `json:"visibility"`
	AIProcessing   AIProcessing `json:"ai_processing"`
	AIConsent      *AIConsent   `json:"ai_consent"`
	Status         AssetStatus  `json:"status"`
	CurrentVersion string       `json:"current_version"`

	MasterAvailable   bool        `json:"master_available"`
	BackupVerified    bool        `json:"backup_verified"`
	ProxyAvailable    bool        `json:"proxy_available"`
	TimelineValidated bool        `json:"timeline_validated"`
	CatalogSync       CatalogSync `json:"catalog_sync"`

	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Language        string  `json:"language,omitempty"`

	ParentAssetID id.Friendly `json:"parent_asset_id,omitempty"`
	Lineage       *Lineage    `json:"lineage,omitempty"`

	SourceOriginal *MediaFile      `json:"source_original,omitempty"`
	WorkingMaster  *MediaFile      `json:"working_master,omitempty"`
	Proxy          *MediaFile      `json:"proxy,omitempty"`
	Backup         *MediaFile      `json:"backup,omitempty"`
	Thumbnail      *MediaFile      `json:"thumbnail,omitempty"`
	Transcript     *TranscriptInfo `json:"transcript,omitempty"`
	Provenance     *Provenance     `json:"provenance,omitempty"`
	Publications   []Publication   `json:"publications,omitempty"`

	CreatedAt time.Time  `json:"created_at,omitzero"`
	UpdatedAt time.Time  `json:"updated_at,omitzero"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// ReadinessSatisfied is the consolidated condition of §3.1 (R11): verified
// backup, verified master, proxy, transcript with timestamps, valid text and
// vector indexes, validated timeline. External catalog sync never counts.
func (a Asset) ReadinessSatisfied() bool {
	return a.BackupVerified && a.MasterAvailable && a.ProxyAvailable &&
		a.TimelineValidated && a.Transcript != nil && a.Transcript.Indexed &&
		a.Transcript.EmbeddingModel != ""
}

// ClipEligible reports whether the asset may be a clip source (I21). Status
// ready_for_clipping qualifies, and so does a derivative further along its
// editorial life whose readiness still holds. An original that is not ready,
// archived_only, withdrawn or a missing master never qualifies.
func (a Asset) ClipEligible() bool {
	if !a.ReadinessSatisfied() || a.DeletedAt != nil {
		return false
	}
	switch a.Status {
	case AssetReadyForClipping:
		return true
	case AssetEditorialReview, AssetApproved, AssetPublished:
		return a.Kind != KindOriginal
	default:
		return false
	}
}

// FilePurpose says what a stored file is to its version.
type FilePurpose string

const (
	FileSourceOriginal     FilePurpose = "source_original"
	FileWorkingMaster      FilePurpose = "working_master"
	FileProxy              FilePurpose = "proxy"
	FileBackup             FilePurpose = "backup"
	FileThumbnail          FilePurpose = "thumbnail"
	FilePreview            FilePurpose = "preview"
	FileTranscriptionAudio FilePurpose = "transcription_audio"
	FilePublishedFile      FilePurpose = "published_file"
	FileCaptions           FilePurpose = "captions"
	FileTranscript         FilePurpose = "transcript"
)

// VersionFile is a file recorded on a version.
type VersionFile struct {
	Purpose FilePurpose `json:"purpose"`
	Name    string      `json:"name,omitempty"` // disambiguates, e.g. a destination or a language
	File    MediaFile   `json:"file"`
}

// AssetVersion is a media version (D35). It becomes immutable at its first
// confirmed publication; before that its files may be replaced in place.
type AssetVersion struct {
	AssetID    id.Friendly   `json:"asset_id"`
	Version    string        `json:"version"` // "v1"
	Immutable  bool          `json:"immutable"`
	Files      []VersionFile `json:"files"`
	Provenance *Provenance   `json:"provenance,omitempty"`
	CreatedAt  time.Time     `json:"created_at,omitzero"`
}

// HistoryEntry records a metadata edit, which never creates a media version.
type HistoryEntry struct {
	AssetID id.Friendly     `json:"asset_id"`
	At      time.Time       `json:"at"`
	Actor   string          `json:"actor"`
	Action  string          `json:"action"`
	Changes json.RawMessage `json:"changes,omitempty"`
}

// Collection groups assets and can carry the AI policy (D8).
type Collection struct {
	Name         string       `json:"name"`
	TenantID     TenantID     `json:"tenant_id"`
	AIProcessing AIProcessing `json:"ai_processing"`
	AIConsent    *AIConsent   `json:"ai_consent,omitempty"`
}
