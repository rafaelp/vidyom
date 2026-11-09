package connector

import (
	"context"
	"io"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// StorageCapabilities declares optional behavior (§7.2). The direct-access
// policy comes from the instance's configuration, never from the driver name.
type StorageCapabilities struct {
	Versioning           bool `json:"versioning"`
	RangeReads           bool `json:"range_reads"`
	ServerSideCopy       bool `json:"server_side_copy"`
	ResumableUpload      bool `json:"resumable_upload"`
	ChecksumVerification bool `json:"checksum_verification"`
	StorageClasses       bool `json:"storage_classes"`
	RestoreSemantics     bool `json:"restore_semantics"`
	DirectAccess         bool `json:"direct_access"`
}

// ObjectInfo describes a stored object. Keys are relative to the instance's
// role and tenant prefix, as they appear in storage:// URIs.
type ObjectInfo struct {
	Key         string          `json:"key"`
	SizeBytes   int64           `json:"size_bytes"`
	SHA256      string          `json:"sha256,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	ModTime     time.Time       `json:"mod_time,omitzero"`
	Location    domain.Location `json:"location"` // immutable record of where it is (R30)
}

// PutOptions controls a write. When SHA256 is set the driver verifies it and
// fails rather than leaving a bad object.
type PutOptions struct {
	SHA256         string
	SizeBytes      int64 // -1 or 0 when unknown
	ContentType    string
	StorageClass   string
	IdempotencyKey string
	Progress       func(bytesDone int64)
}

// ListOptions pages a listing.
type ListOptions struct {
	Prefix string
	Cursor string
	Limit  int
}

// ListPage is one page of a listing.
type ListPage struct {
	Objects    []ObjectInfo
	NextCursor string
}

// VerifyResult is the outcome of a checksum check.
type VerifyResult struct {
	OK        bool   `json:"ok"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Storage is one role's instance for one tenant, with the standard operations
// of §7.2. An operation a driver lacks returns errs.CodeCapabilityUnsupported,
// or an explicit fallback that is subject to cost and permission.
type Storage interface {
	Info() Info
	Role() domain.StorageRole
	Capabilities() StorageCapabilities

	Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (ObjectInfo, error)
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	// DownloadRange reads length bytes from offset; a negative length reads to the end.
	DownloadRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Exists(ctx context.Context, key string) (bool, error)
	List(ctx context.Context, opts ListOptions) (ListPage, error)
	Delete(ctx context.Context, key string) error
	Copy(ctx context.Context, srcKey, dstKey string) (ObjectInfo, error)
	Move(ctx context.Context, srcKey, dstKey string) (ObjectInfo, error)
	Checksum(ctx context.Context, key string) (string, error)
	Verify(ctx context.Context, key, wantSHA256 string) (VerifyResult, error)
}

// LinkProvider is an optional capability, discovered by type assertion, that
// returns a time-limited link for CLI review with proxy links (D24). Links are
// never logged.
type LinkProvider interface {
	TemporaryLink(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// StorageParams is what a factory receives to open one role for one tenant.
type StorageParams struct {
	Tenant   domain.TenantID
	Role     domain.StorageRole
	Instance config.StorageInstance
	Secrets  SecretResolver
}

// StorageFactory opens a storage instance.
type StorageFactory func(ctx context.Context, p StorageParams) (Storage, error)

// StorageResolver returns the instance of a role for a tenant. Component media
// provides the implementation used by both the daemon and the workers.
type StorageResolver interface {
	Storage(ctx context.Context, tenant domain.TenantID, role domain.StorageRole) (Storage, error)
}
