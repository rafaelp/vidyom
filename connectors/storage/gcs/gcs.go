package gcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/rafaelp/vidyom/connectors/storage/internal/storageutil"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is this connector's name in storages.json.
const Driver = "gcs"

const (
	driverVersion    = "1"
	metaSHA256       = "vidyom-sha256"
	metaCRC32C       = "vidyom-crc32c"
	defaultChunkSize = 16 << 20
	minChunkSize     = 256 << 10
	copyBuffer       = 1 << 20
	maxObjectBytes   = 5 << 40 // GCS's documented object limit
	// defaultAttempts bounds retries, so an outage reaches the node as an error it can
	// classify, not as a call that hangs while its heartbeat stays alive.
	defaultAttempts = 4
	// metadataTimeout bounds one call that moves no media.
	metadataTimeout = 2 * time.Minute
	// cleanupTimeout bounds removing a failed write after its context ended.
	cleanupTimeout = 30 * time.Second
	// defaultStallTimeout is how long a transfer may make no progress. Media transfers
	// can last hours, so they get no overall deadline, and the client's retries do not
	// bound starting an upload on a dead endpoint.
	defaultStallTimeout = 5 * time.Minute
)

type parameters struct {
	Endpoint       string `json:"endpoint"`
	Versioning     bool   `json:"versioning"`
	ChunkSizeBytes int    `json:"chunkSizeBytes"`
	RetryAttempts  int    `json:"retryAttempts"`
	StallSeconds   int    `json:"stallTimeoutSeconds"`
}

// Info describes the driver for the registry and GET /connectors.
func Info() connector.Info {
	return connector.Info{
		Family:       connector.FamilyStorage,
		Driver:       Driver,
		Version:      driverVersion,
		Available:    true,
		Reason:       "tested against fake-gcs-server only; real GCS is unvalidated (I12, I199)",
		Capabilities: storageutil.CapabilityMap(capabilities(false, false)),
		Limits:       map[string]any{"max_object_bytes": int64(maxObjectBytes)},
	}
}

// Register adds the driver to a registry.
func Register(r connector.Registrar) { r.RegisterStorage(Info(), Open) }

func capabilities(versioning, direct bool) connector.StorageCapabilities {
	return connector.StorageCapabilities{
		Versioning:           versioning,
		RangeReads:           true,
		ServerSideCopy:       true,
		ResumableUpload:      true,
		ChecksumVerification: true,
		StorageClasses:       true,
		// ARCHIVE objects read immediately: there is no restore step (I187).
		RestoreSemantics: false,
		DirectAccess:     direct,
	}
}

// Storage is one role's instance for one tenant.
type Storage struct {
	client     *storage.Client
	bucketName string
	bucket     *storage.BucketHandle
	role       domain.StorageRole
	tenant     domain.TenantID
	base       string
	class      string
	chunkSize  int
	attempts   int
	stall      time.Duration
	caps       connector.StorageCapabilities
	signer     *signer
}

var (
	_ connector.Storage      = (*Storage)(nil)
	_ connector.LinkProvider = (*Storage)(nil)
	_ connector.RoleCopier   = (*Storage)(nil)
)

// signer holds what a V4 signed URL needs.
type signer struct {
	accessID   string
	privateKey []byte
}

// Open opens an instance. A missing credential fails unless parameters.endpoint
// names an emulator (I199).
func Open(ctx context.Context, p connector.StorageParams) (connector.Storage, error) {
	inst := p.Instance
	if inst.Bucket == "" {
		return nil, errs.Newf(errs.CodeInvalidArgument, "gcs storage for role %s has no bucket", p.Role)
	}
	base, err := storageutil.Base(inst.Prefix, p.Tenant)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid gcs namespace")
	}
	var params parameters
	if len(inst.Parameters) > 0 {
		dec := json.NewDecoder(bytes.NewReader(inst.Parameters))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&params); err != nil {
			return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid gcs parameters")
		}
	}
	chunk := params.ChunkSizeBytes
	if chunk == 0 {
		chunk = defaultChunkSize
	}
	if chunk < minChunkSize {
		return nil, errs.Newf(errs.CodeInvalidArgument, "gcs chunkSizeBytes must be at least %d", minChunkSize)
	}

	attempts := params.RetryAttempts
	if attempts == 0 {
		attempts = defaultAttempts
	}
	if attempts < 1 {
		return nil, errs.New(errs.CodeInvalidArgument, "gcs retryAttempts must be positive")
	}

	stall := defaultStallTimeout
	switch {
	case params.StallSeconds < 0:
		return nil, errs.New(errs.CodeInvalidArgument, "gcs stallTimeoutSeconds must be positive")
	case params.StallSeconds > 0:
		stall = time.Duration(params.StallSeconds) * time.Second
	}

	var credentials []byte
	if inst.CredentialsRef != "" {
		if p.Secrets == nil {
			return nil, errs.Newf(errs.CodeFailedPrecondition, "gcs storage for role %s has a credentialsRef and no way to resolve it", p.Role)
		}
		credentials, err = p.Secrets.ResolveSecret(ctx, inst.CredentialsRef)
		if err != nil {
			return nil, errs.Wrap(errs.CodeFailedPrecondition, err, "cannot resolve the gcs credential")
		}
	}

	var opts []option.ClientOption
	switch {
	case params.Endpoint != "":
		if err := checkEndpoint(params.Endpoint); err != nil {
			return nil, err
		}
		opts = append(opts, option.WithEndpoint(params.Endpoint), option.WithoutAuthentication(), storage.WithJSONReads())
	case len(credentials) > 0:
		opts = append(opts, option.WithAuthCredentialsJSON(option.ServiceAccount, credentials))
	default:
		return nil, errs.Newf(errs.CodeFailedPrecondition, "gcs storage for role %s needs a credentialsRef: ambient credentials are not used (I199)", p.Role)
	}
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, errs.Wrap(errs.CodeFailedPrecondition, err, "cannot create the gcs client")
	}
	return &Storage{
		client:     client,
		bucketName: inst.Bucket,
		bucket:     client.Bucket(inst.Bucket).Retryer(storage.WithMaxAttempts(attempts)),
		role:       p.Role,
		tenant:     p.Tenant,
		base:       base,
		class:      inst.StorageClass,
		chunkSize:  chunk,
		attempts:   attempts,
		stall:      stall,
		caps:       capabilities(params.Versioning, storageutil.DirectAccess(inst.Usage)),
		signer:     parseSigner(credentials),
	}, nil
}

func checkEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return errs.Newf(errs.CodeInvalidArgument, "invalid gcs endpoint")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme == "http" && (host == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return errs.Newf(errs.CodeInvalidArgument, "a gcs endpoint must use https, or http on loopback")
}

func parseSigner(credentials []byte) *signer {
	if len(credentials) == 0 {
		return nil
	}
	var sa struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if json.Unmarshal(credentials, &sa) != nil || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil
	}
	return &signer{accessID: sa.ClientEmail, privateKey: []byte(sa.PrivateKey)}
}

// Close releases the client.
func (s *Storage) Close() error { return s.client.Close() }

// Info describes this instance.
func (s *Storage) Info() connector.Info {
	info := Info()
	info.Capabilities = storageutil.CapabilityMap(s.caps)
	return info
}

// Role returns the instance's role.
func (s *Storage) Role() domain.StorageRole { return s.role }

// Capabilities declares what this instance offers.
func (s *Storage) Capabilities() connector.StorageCapabilities { return s.caps }

func validKey(key string) error {
	if err := domain.ValidateStorageKey(key); err != nil {
		return errs.Wrap(errs.CodeInvalidArgument, err, "invalid storage key")
	}
	return nil
}

func (s *Storage) object(key string) *storage.ObjectHandle {
	return s.bucket.Object(s.base+key).Retryer(storage.WithPolicy(storage.RetryAlways), storage.WithMaxAttempts(s.attempts))
}

// mapError codes an error. The context decides what a cancellation means: the task's
// own context ending passes through, so the runner sees it, while a call's timeout is
// an outage worth a retry.
func mapError(ctx context.Context, err error, what string) error {
	var coded *errs.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &coded):
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errs.Wrap(errs.CodeUnavailable, err, what+" timed out on gcs")
	case errors.Is(err, storage.ErrObjectNotExist), errors.Is(err, storage.ErrBucketNotExist):
		return errs.NotFound("object")
	}
	var api *googleapi.Error
	if errors.As(err, &api) {
		switch {
		case api.Code == http.StatusNotFound:
			return errs.NotFound("object")
		case api.Code == http.StatusUnauthorized || api.Code == http.StatusForbidden:
			return errs.Wrap(errs.CodeForbidden, err, "gcs refused "+what)
		case api.Code == http.StatusRequestedRangeNotSatisfiable:
			return errs.Wrap(errs.CodeInvalidArgument, err, "range is outside the object")
		case api.Code == http.StatusTooManyRequests:
			return errs.Wrap(errs.CodeRateLimited, err, "gcs rate limited "+what)
		case api.Code >= 400 && api.Code < 500:
			return errs.Wrap(errs.CodeInvalidArgument, err, "gcs rejected "+what)
		}
	}
	return errs.Wrap(errs.CodeUnavailable, err, what+" failed on gcs")
}

func (s *Storage) info(key string, attrs *storage.ObjectAttrs) connector.ObjectInfo {
	return connector.ObjectInfo{
		Key:         key,
		SizeBytes:   attrs.Size,
		SHA256:      attrs.Metadata[metaSHA256],
		ContentType: attrs.ContentType,
		ModTime:     attrs.Updated,
		Location: domain.Location{
			Driver:     Driver,
			Bucket:     s.bucketName,
			Prefix:     s.base,
			Generation: strconv.FormatInt(attrs.Generation, 10),
		},
	}
}

// attrs fetches an object's attributes under a deadline.
func (s *Storage) attrs(ctx context.Context, key string) (*storage.ObjectAttrs, error) {
	callCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	return s.object(key).Attrs(callCtx)
}

// Stat describes an object without reading it.
func (s *Storage) Stat(ctx context.Context, key string) (connector.ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return connector.ObjectInfo{}, err
	}
	attrs, err := s.attrs(ctx, key)
	if err != nil {
		return connector.ObjectInfo{}, mapError(ctx, err, "stat")
	}
	return s.info(key, attrs), nil
}

// Exists reports whether an object exists.
func (s *Storage) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Stat(ctx, key)
	if errs.IsCode(err, errs.CodeNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Get opens an object for reading, pinned to the generation it described.
func (s *Storage) Get(ctx context.Context, key string) (io.ReadCloser, connector.ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return nil, connector.ObjectInfo{}, err
	}
	attrs, err := s.attrs(ctx, key)
	if err != nil {
		return nil, connector.ObjectInfo{}, mapError(ctx, err, "get")
	}
	readCtx, cancel := context.WithCancel(ctx)
	r, err := s.object(key).Generation(attrs.Generation).NewReader(readCtx)
	if err != nil {
		cancel()
		return nil, connector.ObjectInfo{}, mapError(ctx, err, "get")
	}
	return &guardedReader{ctx: ctx, r: r, cancel: cancel, stall: s.stall}, s.info(key, attrs), nil
}

// DownloadRange reads length bytes from offset; a negative length reads to the end.
func (s *Storage) DownloadRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	attrs, err := s.attrs(ctx, key)
	if err != nil {
		return nil, mapError(ctx, err, "range read")
	}
	if offset < 0 || offset > attrs.Size {
		return nil, errs.Newf(errs.CodeInvalidArgument, "range offset %d is outside the object", offset)
	}
	if offset == attrs.Size {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	readCtx, cancel := context.WithCancel(ctx)
	r, err := s.object(key).Generation(attrs.Generation).NewRangeReader(readCtx, offset, length)
	if err != nil {
		cancel()
		return nil, mapError(ctx, err, "range read")
	}
	return &guardedReader{ctx: ctx, r: r, cancel: cancel, stall: s.stall}, nil
}

// guardedReader fails a read that makes no progress for the stall timeout, and maps
// what the client returns, so an outage mid-transfer reads as unavailable.
type guardedReader struct {
	ctx    context.Context
	r      io.ReadCloser
	cancel context.CancelFunc
	stall  time.Duration
}

func (g *guardedReader) Read(p []byte) (int, error) {
	watchdog := time.AfterFunc(g.stall, g.cancel)
	n, err := g.r.Read(p)
	watchdog.Stop()
	if err != nil && !errors.Is(err, io.EOF) {
		return n, mapError(g.ctx, err, "read")
	}
	return n, err
}

func (g *guardedReader) Close() error {
	defer g.cancel()
	return g.r.Close()
}

// guardedWriter aborts an upload that makes no progress for the stall timeout, which
// also bounds starting a session on a dead endpoint and the final flush.
type guardedWriter struct {
	w        io.Writer
	watchdog *time.Timer
	stall    time.Duration
}

func (g *guardedWriter) Write(p []byte) (int, error) {
	n, err := g.w.Write(p)
	g.watchdog.Reset(g.stall)
	return n, err
}

// Put writes an object. The upload commits only at Close, so a checksum or size
// that does not match cancels it and no object ever appears. After commit the
// server's CRC32C must match what was streamed (I186).
func (s *Storage) Put(ctx context.Context, key string, r io.Reader, opts connector.PutOptions) (connector.ObjectInfo, error) {
	if err := validKey(key); err != nil {
		return connector.ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return connector.ObjectInfo{}, err
	}
	writeCtx, abort := context.WithCancel(ctx)
	defer abort()
	obj := s.object(key)
	w := obj.NewWriter(writeCtx)
	w.ChunkSize = s.chunkSize
	w.ContentType = opts.ContentType
	if w.ContentType == "" {
		w.ContentType = "application/octet-stream"
	}
	w.StorageClass = opts.StorageClass
	if w.StorageClass == "" {
		w.StorageClass = s.class
	}
	shaHash := sha256.New()
	crcHash := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	source := &storageutil.ProgressReader{R: storageutil.ContextReader{Ctx: ctx, R: r}, Progress: opts.Progress}
	watchdog := time.AfterFunc(s.stall, abort)
	defer watchdog.Stop()
	guarded := &guardedWriter{w: w, watchdog: watchdog, stall: s.stall}
	written, err := io.CopyBuffer(io.MultiWriter(guarded, shaHash, crcHash), source, make([]byte, copyBuffer))
	if err != nil {
		abort()
		_ = w.Close()
		return connector.ObjectInfo{}, mapError(ctx, err, "put")
	}
	sum := hex.EncodeToString(shaHash.Sum(nil))
	if opts.SizeBytes > 0 && opts.SizeBytes != written {
		abort()
		_ = w.Close()
		return connector.ObjectInfo{}, errs.Newf(errs.CodeValidationFailed, "size mismatch writing an object: got %d bytes, expected %d", written, opts.SizeBytes)
	}
	if opts.SHA256 != "" && !strings.EqualFold(opts.SHA256, sum) {
		abort()
		_ = w.Close()
		return connector.ObjectInfo{}, errs.New(errs.CodeValidationFailed, "checksum mismatch writing an object")
	}
	watchdog.Reset(s.stall)
	if err := w.Close(); err != nil {
		return connector.ObjectInfo{}, mapError(ctx, err, "put")
	}
	watchdog.Stop()
	attrs := w.Attrs()
	pinned := obj.Generation(attrs.Generation)
	if attrs.CRC32C != crcHash.Sum32() {
		s.removeFailedWrite(ctx, pinned)
		return connector.ObjectInfo{}, errs.New(errs.CodeValidationFailed, "gcs stored different bytes than were sent")
	}
	callCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	updated, err := pinned.Update(callCtx, storage.ObjectAttrsToUpdate{Metadata: map[string]string{
		metaSHA256: sum,
		metaCRC32C: strconv.FormatUint(uint64(crcHash.Sum32()), 10),
	}})
	if err != nil {
		// An object with no record could not be verified, so a failed write leaves none.
		s.removeFailedWrite(ctx, pinned)
		return connector.ObjectInfo{}, mapError(ctx, err, "put")
	}
	return s.info(key, updated), nil
}

// removeFailedWrite deletes the generation a failed Put created. It runs after the
// task's context may have ended, so it has its own deadline.
func (s *Storage) removeFailedWrite(ctx context.Context, pinned *storage.ObjectHandle) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_ = pinned.Delete(cleanupCtx)
}

// List pages objects in key order under a key prefix. The cursor is the last key of
// the previous page, so paging needs no server state.
func (s *Storage) List(ctx context.Context, opts connector.ListOptions) (connector.ListPage, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 1000
	}
	query := &storage.Query{Prefix: s.base + opts.Prefix}
	if opts.Cursor != "" {
		// A NUL sorts just after the cursor's own name.
		query.StartOffset = s.base + opts.Cursor + "\x00"
	}
	callCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	it := s.bucket.Objects(callCtx, query)
	it.PageInfo().MaxSize = limit + 1
	var page connector.ListPage
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return page, nil
		}
		if err != nil {
			return connector.ListPage{}, mapError(ctx, err, "list")
		}
		if attrs.Name == "" {
			continue
		}
		if len(page.Objects) == limit {
			page.NextCursor = page.Objects[limit-1].Key
			return page, nil
		}
		page.Objects = append(page.Objects, s.info(strings.TrimPrefix(attrs.Name, s.base), attrs))
	}
}

// Delete removes an object.
func (s *Storage) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	return mapError(ctx, s.object(key).Delete(callCtx), "delete")
}

// Copy duplicates an object inside this instance, server side.
func (s *Storage) Copy(ctx context.Context, srcKey, dstKey string) (connector.ObjectInfo, error) {
	if err := validKey(srcKey); err != nil {
		return connector.ObjectInfo{}, err
	}
	if err := validKey(dstKey); err != nil {
		return connector.ObjectInfo{}, err
	}
	return s.rewrite(ctx, s.object(srcKey), dstKey)
}

// rewrite copies server side. It names the source's metadata and content type itself:
// a rewrite that also sets a storage class has a non-empty body, and the service may
// then not carry them over, which would lose the object's recorded checksum.
// fake-gcs-server merges either way, so a test can only check what is sent. The source
// is pinned to the generation those attributes describe.
func (s *Storage) rewrite(ctx context.Context, src *storage.ObjectHandle, dstKey string) (connector.ObjectInfo, error) {
	callCtx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	attrs, err := src.Attrs(callCtx)
	if err != nil {
		return connector.ObjectInfo{}, mapError(ctx, err, "copy")
	}
	copier := s.object(dstKey).CopierFrom(src.Generation(attrs.Generation))
	copier.ContentType = attrs.ContentType
	copier.Metadata = attrs.Metadata
	if s.class != "" {
		copier.StorageClass = s.class
	}
	copied, err := copier.Run(ctx)
	if err != nil {
		return connector.ObjectInfo{}, mapError(ctx, err, "copy")
	}
	return s.info(dstKey, copied), nil
}

// CopyFrom rewrites an object from another gcs instance, so no bytes pass through
// the worker. Metadata travels with it. Any other source answers
// capability_unsupported, and the caller streams.
func (s *Storage) CopyFrom(ctx context.Context, src connector.Storage, srcKey, dstKey string) (connector.ObjectInfo, error) {
	other, ok := src.(*Storage)
	if !ok {
		return connector.ObjectInfo{}, errs.New(errs.CodeCapabilityUnsupported, "gcs can only copy server-side from another gcs instance")
	}
	if err := validKey(srcKey); err != nil {
		return connector.ObjectInfo{}, err
	}
	if err := validKey(dstKey); err != nil {
		return connector.ObjectInfo{}, err
	}
	info, err := s.rewrite(ctx, s.client.Bucket(other.bucketName).Object(other.base+srcKey), dstKey)
	if errs.IsCode(err, errs.CodeForbidden) {
		// This instance's credential cannot read the other bucket.
		return connector.ObjectInfo{}, errs.Wrap(errs.CodeCapabilityUnsupported, err, "gcs server-side copy is not permitted between these buckets")
	}
	return info, err
}

// Move copies then deletes. GCS has no atomic rename on flat buckets, and a retry
// after a failure in between finds the source still there or the copy already done.
func (s *Storage) Move(ctx context.Context, srcKey, dstKey string) (connector.ObjectInfo, error) {
	info, err := s.Copy(ctx, srcKey, dstKey)
	if err != nil {
		return connector.ObjectInfo{}, err
	}
	if err := s.Delete(ctx, srcKey); err != nil {
		return connector.ObjectInfo{}, err
	}
	return info, nil
}

// recordedSum returns the SHA-256 kept in metadata when the recorded CRC32C still
// matches the server's: the check of I186, with no read.
func recordedSum(attrs *storage.ObjectAttrs) (string, bool) {
	sum, hasSum := attrs.Metadata[metaSHA256]
	crc, hasCRC := attrs.Metadata[metaCRC32C]
	if !hasSum || !hasCRC {
		return "", false
	}
	if crc != strconv.FormatUint(uint64(attrs.CRC32C), 10) {
		return "", false
	}
	return sum, true
}

func (s *Storage) streamSum(ctx context.Context, key string, generation int64) (string, error) {
	readCtx, cancel := context.WithCancel(ctx)
	r, err := s.object(key).Generation(generation).NewReader(readCtx)
	if err != nil {
		cancel()
		return "", mapError(ctx, err, "checksum")
	}
	guarded := &guardedReader{ctx: ctx, r: r, cancel: cancel, stall: s.stall}
	defer func() { _ = guarded.Close() }()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, guarded, make([]byte, copyBuffer)); err != nil {
		return "", mapError(ctx, err, "checksum")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Storage) sumOf(ctx context.Context, key string) (string, *storage.ObjectAttrs, error) {
	if err := validKey(key); err != nil {
		return "", nil, err
	}
	attrs, err := s.attrs(ctx, key)
	if err != nil {
		return "", nil, mapError(ctx, err, "checksum")
	}
	if sum, ok := recordedSum(attrs); ok {
		return sum, attrs, nil
	}
	sum, err := s.streamSum(ctx, key, attrs.Generation)
	return sum, attrs, err
}

// Checksum returns the object's SHA-256: recorded when the CRC32C still holds,
// computed from the bytes otherwise.
func (s *Storage) Checksum(ctx context.Context, key string) (string, error) {
	sum, _, err := s.sumOf(ctx, key)
	return sum, err
}

// Verify compares the object with a wanted SHA-256. An object written through this
// driver is checked against the server's CRC32C with no read (I186); anything else
// is streamed and hashed.
func (s *Storage) Verify(ctx context.Context, key, wantSHA256 string) (connector.VerifyResult, error) {
	sum, attrs, err := s.sumOf(ctx, key)
	if err != nil {
		return connector.VerifyResult{}, err
	}
	return connector.VerifyResult{OK: strings.EqualFold(sum, wantSHA256), SHA256: sum, SizeBytes: attrs.Size}, nil
}

// TemporaryLink signs a V4 URL with the service account's key. A credential without
// a key answers capability_unsupported. The link is never logged here (D24).
func (s *Storage) TemporaryLink(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if _, err := s.Stat(ctx, key); err != nil {
		return "", err
	}
	if s.signer == nil {
		return "", errs.New(errs.CodeCapabilityUnsupported, "this gcs credential cannot sign links")
	}
	link, err := s.bucket.SignedURL(s.base+key, &storage.SignedURLOptions{
		GoogleAccessID: s.signer.accessID,
		PrivateKey:     s.signer.privateKey,
		Method:         http.MethodGet,
		Expires:        time.Now().Add(ttl),
		Scheme:         storage.SigningSchemeV4,
	})
	if err != nil {
		return "", errs.Wrap(errs.CodeFailedPrecondition, err, fmt.Sprintf("cannot sign a link for role %s", s.role))
	}
	return link, nil
}
