// Package uploads implements resumable client uploads (§5.4, I20, I130) over a
// spool directory and the tenant's upload repository. It implements
// service.Uploads, plus AppendChunk for the PATCH route.
//
// Data lands in <spool-dir>/<tenant>/<upload-ulid>, on a volume workers can read.
// The database offset is the truth: a chunk is written and fsynced before the
// offset moves, so after a crash the file can only be longer than the record, and
// the next request truncates it.
package uploads

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/rafaelp/vidyom/internal/api/keylock"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

// Defaults of I130.
const (
	DefaultTTL      = 7 * 24 * time.Hour
	MinChunkBytes   = 1 << 20
	MaxChunkBytes   = 64 << 20
	maxFilenameSize = 255

	dirMode  = 0o750
	fileMode = 0o640
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Chunk is one PATCH: the offset the client believes in, an optional raw SHA-256
// of the chunk, and its bytes.
type Chunk struct {
	Offset int64
	SHA256 []byte // nil when the client sent no checksum
	Body   io.Reader
}

// Options configures the service.
type Options struct {
	Stores   repo.TenantStores
	SpoolDir string
	TTL      time.Duration // zero means DefaultTTL
	Now      func() time.Time
	Logger   *slog.Logger
}

// Service is the upload service.
type Service struct {
	stores repo.TenantStores
	spool  string
	ttl    time.Duration
	now    func() time.Time
	log    *slog.Logger
	locks  *keylock.Locks
}

var _ service.Uploads = (*Service)(nil)

// New returns the service. It does not touch the disk until the first upload.
func New(o Options) (*Service, error) {
	if o.Stores == nil {
		return nil, errors.New("uploads: no tenant stores")
	}
	if o.SpoolDir == "" {
		return nil, errors.New("uploads: no spool directory")
	}
	s := &Service{stores: o.Stores, spool: o.SpoolDir, ttl: o.TTL, now: o.Now, log: o.Logger, locks: keylock.New()}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Path returns where a tenant's upload is spooled (I20). The tenant must already
// be validated.
func Path(spoolDir string, tenant domain.TenantID, upload id.ULID) string {
	return filepath.Join(spoolDir, string(tenant), string(upload))
}

func (s *Service) path(t domain.TenantID, u id.ULID) string { return Path(s.spool, t, u) }

func clampChunk(n int64) int64 {
	switch {
	case n <= 0:
		return api.DefaultChunkBytes
	case n < MinChunkBytes:
		return MinChunkBytes
	case n > MaxChunkBytes:
		return MaxChunkBytes
	default:
		return n
	}
}

func validFilename(name string) bool {
	if name == "" || len(name) > maxFilenameSize || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Create starts an upload: an empty spool file, then the record with its UPL-nnn.
func (s *Service) Create(ctx context.Context, sc service.Scope, req api.CreateUploadRequest) (domain.Upload, error) {
	if err := sc.Tenant.Validate(); err != nil {
		return domain.Upload{}, errs.New(errs.CodeInvalidArgument, "invalid tenant")
	}
	if !validFilename(req.Filename) {
		return domain.Upload{}, errs.New(errs.CodeInvalidArgument, "filename must be a plain file name, without a directory")
	}
	if req.SizeBytes <= 0 {
		return domain.Upload{}, errs.New(errs.CodeInvalidArgument, "size_bytes must be positive")
	}
	sum := strings.ToLower(req.SHA256)
	if !sha256Pattern.MatchString(sum) {
		return domain.Upload{}, errs.New(errs.CodeInvalidArgument, "sha256 must be 64 hexadecimal characters")
	}
	store, err := s.stores.ForTenant(ctx, sc.Tenant)
	if err != nil {
		return domain.Upload{}, err
	}

	ulid := id.New()
	path := s.path(sc.Tenant, ulid)
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool directory is not usable")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool directory is not usable")
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool directory is not usable")
	}

	now := s.now().UTC()
	u := domain.Upload{
		ULID:       ulid,
		TenantID:   sc.Tenant,
		Filename:   req.Filename,
		SizeBytes:  req.SizeBytes,
		SHA256:     sum,
		ChunkBytes: clampChunk(req.ChunkBytes),
		Status:     domain.UploadPending,
		CreatedBy:  sc.Principal.Actor(),
		CreatedAt:  now,
		ExpiresAt:  now.Add(s.ttl),
	}
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		friendly, err := tx.IDs().Next(ctx, id.PrefixUpload)
		if err != nil {
			return err
		}
		u.ID = friendly
		return tx.Uploads().Create(ctx, u)
	})
	if err != nil {
		_ = os.Remove(path)
		return domain.Upload{}, err
	}
	return u, nil
}

// Get reports an upload and its offset. A record past its expiry reads as expired
// even before the sweep removes it.
func (s *Service) Get(ctx context.Context, sc service.Scope, ref id.Ref) (domain.Upload, error) {
	u, _, err := s.load(ctx, sc, ref)
	return u, err
}

func (s *Service) load(ctx context.Context, sc service.Scope, ref id.Ref) (domain.Upload, repo.TenantStore, error) {
	if err := sc.Tenant.Validate(); err != nil {
		return domain.Upload{}, nil, errs.New(errs.CodeInvalidArgument, "invalid tenant")
	}
	store, err := s.stores.ForTenant(ctx, sc.Tenant)
	if err != nil {
		return domain.Upload{}, nil, err
	}
	u, err := store.Uploads().Get(ctx, ref)
	if err != nil {
		return domain.Upload{}, nil, err
	}
	if u.Status != domain.UploadExpired && !u.ExpiresAt.IsZero() && s.now().After(u.ExpiresAt) {
		u.Status = domain.UploadExpired
	}
	return u, store, nil
}

// lockAndLoad resolves ref to a ULID, takes its lock and reads the record again
// under it.
func (s *Service) lockAndLoad(ctx context.Context, sc service.Scope, ref id.Ref) (domain.Upload, repo.TenantStore, func(), error) {
	first, _, err := s.load(ctx, sc, ref)
	if err != nil {
		return domain.Upload{}, nil, nil, err
	}
	unlock, err := s.locks.Lock(ctx, string(sc.Tenant)+"/"+string(first.ULID))
	if err != nil {
		return domain.Upload{}, nil, nil, errs.Wrap(errs.CodeUnavailable, err, "the upload is busy")
	}
	u, store, err := s.load(ctx, sc, id.Ref(first.ULID))
	if err != nil {
		unlock()
		return domain.Upload{}, nil, nil, err
	}
	return u, store, unlock, nil
}

func requirePending(u domain.Upload) error {
	switch u.Status {
	case domain.UploadPending:
		return nil
	case domain.UploadExpired:
		return errs.Newf(errs.CodeGone, "upload %s has expired", u.ID)
	case domain.UploadComplete:
		return errs.Newf(errs.CodeFailedPrecondition, "upload %s is already complete", u.ID)
	default:
		return errs.Newf(errs.CodeFailedPrecondition, "upload %s failed verification; start a new one", u.ID)
	}
}

// AppendChunk writes one chunk, all or nothing (I130).
func (s *Service) AppendChunk(ctx context.Context, sc service.Scope, ref id.Ref, chunk Chunk) (domain.Upload, error) {
	u, store, unlock, err := s.lockAndLoad(ctx, sc, ref)
	if err != nil {
		return domain.Upload{}, err
	}
	defer unlock()
	if err := requirePending(u); err != nil {
		return domain.Upload{}, err
	}
	if chunk.Offset != u.ReceivedBytes {
		return domain.Upload{}, errs.Newf(errs.CodeConflict, "upload %s is at offset %d, not %d", u.ID, u.ReceivedBytes, chunk.Offset).
			With("offset", u.ReceivedBytes)
	}

	path := s.path(sc.Tenant, u.ULID)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool file is not usable")
	}
	defer f.Close()

	// Reconcile with what a crash may have left. A longer file is an uncommitted
	// chunk. A shorter file lost data the record promised, so the offset goes back.
	info, err := f.Stat()
	if err != nil {
		return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool file is not usable")
	}
	if info.Size() < u.ReceivedBytes {
		u.ReceivedBytes = info.Size()
		if err := store.Uploads().Update(ctx, u); err != nil {
			return domain.Upload{}, err
		}
		return domain.Upload{}, errs.Newf(errs.CodeConflict, "upload %s is at offset %d", u.ID, u.ReceivedBytes).
			With("offset", u.ReceivedBytes)
	}
	if err := f.Truncate(u.ReceivedBytes); err != nil {
		return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool file is not usable")
	}
	if _, err := f.Seek(u.ReceivedBytes, io.SeekStart); err != nil {
		return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool file is not usable")
	}

	remaining := u.SizeBytes - u.ReceivedBytes
	limit := min(u.ChunkBytes, remaining)
	rollback := func() { _ = f.Truncate(u.ReceivedBytes) }

	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(chunk.Body, limit+1))
	if err != nil {
		rollback()
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool volume cannot take the chunk")
		}
		return domain.Upload{}, errs.Wrap(errs.CodeInvalidArgument, err, "the chunk was not received completely")
	}
	switch {
	case n == 0:
		rollback()
		return domain.Upload{}, errs.New(errs.CodeInvalidArgument, "empty chunk")
	case n > limit && limit == remaining:
		rollback()
		return domain.Upload{}, errs.Newf(errs.CodeConflict, "the chunk goes past the declared size of %d bytes", u.SizeBytes).
			With("offset", u.ReceivedBytes)
	case n > limit:
		rollback()
		return domain.Upload{}, errs.Newf(errs.CodePayloadTooLarge, "a chunk may not exceed %d bytes", u.ChunkBytes)
	}
	if chunk.SHA256 != nil && subtle.ConstantTimeCompare(chunk.SHA256, hash.Sum(nil)) != 1 {
		rollback()
		return domain.Upload{}, errs.New(errs.CodeValidationFailed, "the chunk does not match its Upload-Checksum").
			With("offset", u.ReceivedBytes)
	}
	if err := f.Sync(); err != nil {
		rollback()
		return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool volume cannot take the chunk")
	}

	committed := u
	committed.ReceivedBytes += n
	committed.ExpiresAt = s.now().UTC().Add(s.ttl)
	if err := store.Uploads().Update(ctx, committed); err != nil {
		rollback()
		return domain.Upload{}, err
	}
	return committed, nil
}

// Complete verifies size and SHA-256. On a mismatch the upload is failed and its
// file removed; repeating on a complete upload returns it unchanged.
func (s *Service) Complete(ctx context.Context, sc service.Scope, ref id.Ref) (domain.Upload, error) {
	u, store, unlock, err := s.lockAndLoad(ctx, sc, ref)
	if err != nil {
		return domain.Upload{}, err
	}
	defer unlock()
	if u.Status == domain.UploadComplete {
		return u, nil
	}
	if err := requirePending(u); err != nil {
		return domain.Upload{}, err
	}
	if u.ReceivedBytes != u.SizeBytes {
		return domain.Upload{}, errs.Newf(errs.CodeFailedPrecondition, "upload %s has %d of %d bytes", u.ID, u.ReceivedBytes, u.SizeBytes).
			With("offset", u.ReceivedBytes)
	}

	path := s.path(sc.Tenant, u.ULID)
	sum, size, err := hashFile(ctx, path, u.SizeBytes)
	if err != nil {
		if ctx.Err() != nil {
			return domain.Upload{}, errs.Wrap(errs.CodeTimeout, err, "verification was interrupted")
		}
		return domain.Upload{}, errs.Wrap(errs.CodeUnavailable, err, "the spool file is not readable")
	}
	if size != u.SizeBytes || sum != u.SHA256 {
		u.Status = domain.UploadFailed
		if err := store.Uploads().Update(ctx, u); err != nil {
			return domain.Upload{}, err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("uploads.remove_failed", "tenant_id", sc.Tenant, "upload", u.ID, "error", err)
		}
		return domain.Upload{}, errs.Newf(errs.CodeValidationFailed, "upload %s does not match its declared size and SHA-256", u.ID)
	}

	u.Status = domain.UploadComplete
	u.ExpiresAt = s.now().UTC().Add(s.ttl)
	if err := store.Uploads().Update(ctx, u); err != nil {
		return domain.Upload{}, err
	}
	return u, nil
}

func hashFile(ctx context.Context, path string, sizeHint int64) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	buf := make([]byte, 1<<20)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		n, err := f.Read(buf)
		if n > 0 {
			_, _ = hash.Write(buf[:n])
			total += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(hash.Sum(nil)), total, nil
		}
		if err != nil {
			return "", total, fmt.Errorf("read spool file of %d bytes: %w", sizeHint, err)
		}
	}
}

// Abort removes an upload in any state. Its friendly ID stays burned (R5).
func (s *Service) Abort(ctx context.Context, sc service.Scope, ref id.Ref) error {
	u, store, unlock, err := s.lockAndLoad(ctx, sc, ref)
	if err != nil {
		return err
	}
	defer unlock()
	return s.remove(ctx, store, u)
}

func (s *Service) remove(ctx context.Context, store repo.TenantStore, u domain.Upload) error {
	if err := os.Remove(s.path(store.Tenant(), u.ULID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errs.Wrap(errs.CodeUnavailable, err, "the spool file cannot be removed")
	}
	return store.Uploads().Delete(ctx, id.Ref(u.ULID))
}

// Sweep removes expired uploads of every tenant, and spool files whose record
// never landed. Failures are logged and retried by the next sweep; the first one
// is returned so the caller can see them.
func (s *Service) Sweep(ctx context.Context) error {
	tenants, err := s.stores.Tenants(ctx)
	if err != nil {
		return err
	}
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, tenant := range tenants {
		store, err := s.stores.ForTenant(ctx, tenant)
		if err != nil {
			keep(err)
			continue
		}
		keep(s.sweepExpired(ctx, store))
		keep(s.sweepOrphans(ctx, store))
	}
	return first
}

func (s *Service) sweepExpired(ctx context.Context, store repo.TenantStore) error {
	expired, err := store.Uploads().ListExpired(ctx, s.now())
	if err != nil {
		return err
	}
	var first error
	for _, u := range expired {
		unlock, err := s.locks.Lock(ctx, string(store.Tenant())+"/"+string(u.ULID))
		if err != nil {
			return err
		}
		err = s.remove(ctx, store, u)
		unlock()
		if err != nil {
			s.log.Warn("uploads.sweep_failed", "tenant_id", store.Tenant(), "upload", u.ID, "error", err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// sweepOrphans removes files left by a crash between creating the file and writing
// the record. Only names that are ULIDs older than the TTL are considered, so a
// create in progress is safe.
func (s *Service) sweepOrphans(ctx context.Context, store repo.TenantStore) error {
	dir := filepath.Join(s.spool, string(store.Tenant()))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := s.now().Add(-s.ttl)
	for _, entry := range entries {
		ulid, err := id.ParseULID(entry.Name())
		if err != nil || !entry.Type().IsRegular() || ulid.Time().After(cutoff) {
			continue
		}
		if _, err := store.Uploads().Get(ctx, id.Ref(ulid)); errs.IsCode(err, errs.CodeNotFound) {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				s.log.Warn("uploads.orphan_remove_failed", "tenant_id", store.Tenant(), "error", err)
			}
		}
	}
	return nil
}
