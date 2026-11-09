package domain

import (
	"errors"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/id"
)

// PublicationStatus is the state of one publication (§12.4, D52).
type PublicationStatus string

const (
	PubPendingApproval       PublicationStatus = "pending_approval"
	PubUploading             PublicationStatus = "uploading"
	PubProcessing            PublicationStatus = "processing"
	PubScheduled             PublicationStatus = "scheduled"
	PubPublished             PublicationStatus = "published"
	PubFailed                PublicationStatus = "failed"
	PubRejectedByDestination PublicationStatus = "rejected_by_destination"
	PubWithdrawn             PublicationStatus = "withdrawn"
)

// PublicationStatuses lists every publication state.
var PublicationStatuses = []PublicationStatus{
	PubPendingApproval, PubUploading, PubProcessing, PubScheduled, PubPublished,
	PubFailed, PubRejectedByDestination, PubWithdrawn,
}

// Publication is an independent record of an asset at a destination (§2.1).
type Publication struct {
	ID                  id.Friendly       `json:"id"`
	ULID                id.ULID           `json:"ulid"`
	TenantID            TenantID          `json:"tenant_id"`
	AssetID             id.Friendly       `json:"asset_id"`
	Destination         string            `json:"destination"`
	AssetVersion        string            `json:"asset_version"`
	PublishedFileSHA256 string            `json:"published_file_sha256,omitempty"`
	PublishedFile       *MediaFile        `json:"published_file,omitempty"` // nil when identical to the master (R4)
	RemoteID            string            `json:"remote_id,omitempty"`
	URL                 string            `json:"url,omitempty"`
	Status              PublicationStatus `json:"status"`
	Visibility          Visibility        `json:"visibility"`
	ApprovalID          id.Friendly       `json:"approval_id,omitempty"`
	Title               string            `json:"title,omitempty"`
	Description         string            `json:"description,omitempty"`
	IdempotencyKey      string            `json:"idempotency_key,omitempty"`
	StatusReason        string            `json:"status_reason,omitempty"`
	CreatedAt           time.Time         `json:"created_at,omitzero"`
	UpdatedAt           time.Time         `json:"updated_at,omitzero"`
	PublishedAt         *time.Time        `json:"published_at,omitempty"`
	WithdrawnAt         *time.Time        `json:"withdrawn_at,omitempty"`
}

// Validate enforces R43: published is only written after the destination
// returned an ID and a URL. Each publication also records its version.
func (p Publication) Validate() error {
	if p.Status == PubPublished && (p.RemoteID == "" || p.URL == "") {
		return errors.New("a published publication requires a remote ID and a URL from the destination")
	}
	if p.AssetVersion == "" {
		return errors.New("a publication must record the asset version it sent")
	}
	return nil
}

// IsPublic reports whether the publication may appear in the public library.
func (p Publication) IsPublic() bool {
	return p.Status == PubPublished && p.Visibility == VisibilityPublic
}
