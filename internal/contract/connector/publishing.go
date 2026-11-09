package connector

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// PublishingCapabilities declares what a destination supports (§7.3). States and
// metadata vary by platform, so a connector must declare them.
type PublishingCapabilities struct {
	ResumableUpload bool                `json:"resumable_upload"`
	Thumbnails      bool                `json:"thumbnails"`
	Captions        bool                `json:"captions"`
	Scheduling      bool                `json:"scheduling"`
	Update          bool                `json:"update"`
	Delete          bool                `json:"delete"`
	Reconcile       bool                `json:"reconcile"`
	Visibilities    []domain.Visibility `json:"visibilities,omitempty"`
}

// VideoMetadata is what a destination shows.
type VideoMetadata struct {
	Title       string            `json:"title"`
	Description string            `json:"description,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	Language    string            `json:"language,omitempty"`
	Visibility  domain.Visibility `json:"visibility"`
	ScheduledAt *time.Time        `json:"scheduled_at,omitempty"`
}

// UploadRequest is one upload. File reads from the master role; IdempotencyKey
// is stable across retries (I36).
type UploadRequest struct {
	IdempotencyKey string
	File           io.ReaderAt
	SizeBytes      int64
	SHA256         string
	ContentType    string
	Metadata       VideoMetadata
	Progress       func(bytesDone int64)
}

// RemoteStatus is the destination's view of a video.
type RemoteStatus string

const (
	RemoteProcessing RemoteStatus = "processing"
	RemoteReady      RemoteStatus = "ready"
	RemoteScheduled  RemoteStatus = "scheduled"
	RemoteRejected   RemoteStatus = "rejected" // refused or blocked by the platform (D52)
	RemoteFailed     RemoteStatus = "failed"
	RemoteRemoved    RemoteStatus = "removed"
)

// RemoteVideo is a destination's answer. URLs come only from confirmed API
// responses (D45).
type RemoteVideo struct {
	RemoteID   string            `json:"remote_id"`
	URL        string            `json:"url,omitempty"`
	Status     RemoteStatus      `json:"status"`
	Visibility domain.Visibility `json:"visibility"`
	Reason     string            `json:"reason,omitempty"`
}

// Publishing is the capabilities contract of §7.3.
type Publishing interface {
	Info() Info
	Capabilities() PublishingCapabilities

	Upload(ctx context.Context, req UploadRequest) (RemoteVideo, error)
	Get(ctx context.Context, remoteID string) (RemoteVideo, error)
	Update(ctx context.Context, remoteID string, meta VideoMetadata) (RemoteVideo, error)
	Delete(ctx context.Context, remoteID string) error
	SetVisibility(ctx context.Context, remoteID string, v domain.Visibility) (RemoteVideo, error)
	UploadThumbnail(ctx context.Context, remoteID string, image io.Reader, contentType string) error
	UploadCaptions(ctx context.Context, remoteID, language, format string, captions io.Reader) error
	GetStatus(ctx context.Context, remoteID string) (RemoteVideo, error)
	GetURL(ctx context.Context, remoteID string) (string, error)

	// Reconcile answers whether an earlier upload with this key reached the
	// destination, and must be called before any retry (D11, class reconcilable).
	// It returns nil, nil when the destination has no such video. A destination
	// without this ability returns errs.CodeCapabilityUnsupported, which the node
	// treats as an unknown outcome, so the job moves to needs_attention.
	Reconcile(ctx context.Context, idempotencyKey string) (*RemoteVideo, error)
}

// PublishingParams is what a factory receives for one tenant destination.
type PublishingParams struct {
	Tenant      domain.TenantID
	Destination string
	Config      config.Destination
	Secrets     SecretResolver
}

// PublishingFactory opens a publishing connector.
type PublishingFactory func(ctx context.Context, p PublishingParams) (Publishing, error)

// PublishingResolver returns a tenant's destination by logical name.
type PublishingResolver interface {
	Publishing(ctx context.Context, tenant domain.TenantID, destination string) (Publishing, error)
}

// OutcomeUnknown marks a destination call whose effect is uncertain: a request
// that could have created or changed a video and whose answer was lost. Connectors
// cannot import package node, so this is how they say "this may have happened".
// A plain coded error means the effect did not happen (D11).
func OutcomeUnknown(err error) error { return outcomeUnknown{err} }

// IsOutcomeUnknown reports whether err was marked by OutcomeUnknown.
func IsOutcomeUnknown(err error) bool {
	var u outcomeUnknown
	return errors.As(err, &u)
}

type outcomeUnknown struct{ error }

func (u outcomeUnknown) Unwrap() error { return u.error }

// PublicLinks is what a public page needs beyond a video's URL (D18). A field the
// platform does not offer stays empty.
type PublicLinks struct {
	EmbedURL           string
	ThumbnailURL       string
	SupportsTimestamps bool
}

// PublicLinker is an optional capability of a Publishing connector, discovered by
// type assertion. It derives links from a confirmed RemoteVideo and makes no
// network call, so exporting public.db stays local. A connector without it gets
// no embed and no timestamp links in the public library.
type PublicLinker interface {
	PublicLinks(remote RemoteVideo) PublicLinks
}
