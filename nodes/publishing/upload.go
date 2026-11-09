package publishingnodes

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"sync"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

type uploadParams struct {
	PublicationID string `json:"publicationId"`
}

type uploadOutput struct {
	PublicationID string `json:"publication_id"`
	RemoteID      string `json:"remote_id"`
	Status        string `json:"status"`
}

// uploadNode sends the file. Class reconcilable: the engine calls Reconcile before
// any retry, and Execute reconciles too when a record shows an earlier attempt, so
// a restart can never start a second upload blindly (D11).
type uploadNode struct{ base }

var (
	_ node.Node       = (*uploadNode)(nil)
	_ node.Reconciler = (*uploadNode)(nil)
)

func (n *uploadNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var params uploadParams
	if err := in.Bind(&params); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	store := d.Store()
	record, err := getPublication(ctx, store, params.PublicationID)
	if err != nil {
		return node.Result{}, classifyStore(err)
	}

	unlock, ok := n.locks.tryLock(string(record.ULID))
	if !ok {
		return node.Result{}, node.Transient(errs.Newf(errs.CodeConflict, "another task is working on publication %s", record.ID))
	}
	defer unlock()

	// Re-read under the lock: the holder may just have finished.
	record, err = getPublication(ctx, store, params.PublicationID)
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	if record.RemoteID != "" {
		return node.JSONResult(uploadOutputOf(record))
	}
	switch record.Status {
	case domain.PubWithdrawn, domain.PubRejectedByDestination, domain.PubPublished:
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeFailedPrecondition, "publication %s is %s", record.ID, record.Status))
	}

	cfg, err := d.TenantConfig(ctx)
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	if err := checkUploadGate(ctx, store, record, cfg.Publishing); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	conn, err := d.Publishing(ctx, record.Destination)
	if err != nil {
		return node.Result{}, classify(err)
	}
	if info := conn.Info(); !info.Available {
		return node.Result{}, node.Permanent(errs.NotImplemented("destination " + record.Destination + " (" + info.Reason + ")"))
	}

	// An earlier attempt may have reached the destination (D11).
	if record.Status == domain.PubUploading || in.Attempt > 1 {
		remote, err := conn.Reconcile(ctx, record.IdempotencyKey)
		switch {
		case err != nil:
			return node.Result{}, node.Unknown(err)
		case remote != nil:
			record, err = n.recordRemote(ctx, d, record, *remote)
			if err != nil {
				return node.Result{}, classifyStore(err)
			}
			return node.JSONResult(uploadOutputOf(record))
		}
	}

	visibility, err := uploadVisibility(conn.Capabilities(), record)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	asset, err := store.Assets().Get(ctx, id.Ref(record.AssetID))
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	file, err := sourceOf(ctx, store, record)
	if err != nil {
		return node.Result{}, classifyStore(err)
	}

	now := n.opts.Now().UTC()
	if err := move(&record, domain.PubUploading, now); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if err := store.Publications().Update(ctx, record); err != nil {
		return node.Result{}, classifyStore(err)
	}

	reader, size, contentType, closeReader, err := openSource(ctx, d, file)
	if err != nil {
		return node.Result{}, n.fail(ctx, d, record, err)
	}
	defer closeReader()

	remote, err := conn.Upload(ctx, connector.UploadRequest{
		IdempotencyKey: record.IdempotencyKey,
		File:           reader,
		SizeBytes:      size,
		SHA256:         record.PublishedFileSHA256,
		ContentType:    contentType,
		Metadata: connector.VideoMetadata{
			Title:       record.Title,
			Description: record.Description,
			Tags:        asset.Tags,
			Language:    asset.Language,
			Visibility:  visibility,
		},
		Progress: func(done int64) {
			if size > 0 {
				env.Progress(float64(done)/float64(size)*100, "uploading to "+record.Destination)
			}
		},
	})
	if err != nil {
		return node.Result{}, n.fail(ctx, d, record, err)
	}

	record, err = n.recordRemote(ctx, d, record, remote)
	if err != nil {
		// The video exists and its ID is not recorded. A retry reconciles and finds it.
		return node.Result{}, node.Transient(err)
	}
	result, err := node.JSONResult(uploadOutputOf(record))
	if err != nil {
		return node.Result{}, err
	}
	result.Usage = []domain.UsageRecord{n.usage(ctx, d, conn.Info().Driver, size)}
	return result, nil
}

// Reconcile answers whether an earlier attempt reached the destination. Anything
// short of a clear answer is unknown, which moves the job to needs_attention.
func (n *uploadNode) Reconcile(ctx context.Context, env node.Env, in node.Input) (node.ReconcileOutcome, node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.ReconcileUnknown, node.Result{}, err
	}
	var params uploadParams
	if err := in.Bind(&params); err != nil {
		return node.ReconcileUnknown, node.Result{}, err
	}
	record, err := getPublication(ctx, d.Store(), params.PublicationID)
	if err != nil {
		return node.ReconcileUnknown, node.Result{}, err
	}
	if record.RemoteID != "" {
		result, err := node.JSONResult(uploadOutputOf(record))
		return node.ReconcileDone, result, err
	}
	conn, err := d.Publishing(ctx, record.Destination)
	if err != nil {
		return node.ReconcileUnknown, node.Result{}, err
	}
	remote, err := conn.Reconcile(ctx, record.IdempotencyKey)
	if err != nil {
		return node.ReconcileUnknown, node.Result{}, err
	}
	if remote == nil {
		return node.ReconcileNotStarted, node.Result{}, nil
	}
	record, err = n.recordRemote(ctx, d, record, *remote)
	if err != nil {
		return node.ReconcileUnknown, node.Result{}, err
	}
	result, err := node.JSONResult(uploadOutputOf(record))
	return node.ReconcileDone, result, err
}

func uploadOutputOf(r domain.Publication) uploadOutput {
	return uploadOutput{PublicationID: string(r.ID), RemoteID: r.RemoteID, Status: string(r.Status)}
}

// recordRemote commits the destination's answer. It survives a cancelled context:
// the effect already happened, and losing it would force a reconcile for nothing.
func (n *uploadNode) recordRemote(ctx context.Context, d node.DaemonEnv, record domain.Publication, remote connector.RemoteVideo) (domain.Publication, error) {
	ctx = context.WithoutCancel(ctx)
	now := n.opts.Now().UTC()
	record.RemoteID = remote.RemoteID
	record.URL = remote.URL
	record.StatusReason = remote.Reason
	if err := move(&record, statusFromRemote(remote), now); err != nil {
		return record, err
	}
	err := d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Publications().Update(ctx, record); err != nil {
			return err
		}
		if record.Status == domain.PubRejectedByDestination || record.Status == domain.PubFailed {
			return d.Emit(ctx, tx, event.Draft{Type: event.PublicationUpdated, AssetID: string(record.AssetID), Payload: eventPayload(record)})
		}
		return nil
	})
	return record, err
}

// fail classifies an upload error and writes what it means for the record. Unknown
// and transient outcomes stay uploading, so the next attempt reconciles (I223).
func (n *uploadNode) fail(ctx context.Context, d node.DaemonEnv, record domain.Publication, err error) error {
	classified := classify(err)
	switch {
	case connector.IsOutcomeUnknown(err), retryable(err):
		return classified
	case errors.Is(err, context.DeadlineExceeded):
		return classified
	case errors.Is(err, context.Canceled):
		n.writeFailure(ctx, d, record, "upload cancelled")
		return classified
	}
	n.writeFailure(ctx, d, record, errs.Public(err).Message)
	return classified
}

func (n *uploadNode) writeFailure(ctx context.Context, d node.DaemonEnv, record domain.Publication, reason string) {
	ctx = context.WithoutCancel(ctx)
	if err := move(&record, domain.PubFailed, n.opts.Now().UTC()); err != nil {
		return
	}
	record.StatusReason = reason
	err := d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Publications().Update(ctx, record); err != nil {
			return err
		}
		return d.Emit(ctx, tx, event.Draft{Type: event.PublicationUpdated, AssetID: string(record.AssetID), Payload: eventPayload(record)})
	})
	if err != nil {
		d.Logger().Warn("could not record a failed upload", "publication", string(record.ID), "code", string(errs.CodeOf(err)))
	}
}

// usage prices the upload from the tenant's cost table when listed (D19, I239).
func (n *uploadNode) usage(ctx context.Context, d node.DaemonEnv, driver string, size int64) domain.UsageRecord {
	gib := float64(size) / (1 << 30)
	rec := domain.UsageRecord{
		TenantID: d.Tenant(), Operation: "publish.upload", Provider: driver, Quantity: gib, Unit: "GiB",
		JobID: string(d.JobID()), NodeID: d.NodeID(), At: n.opts.Now().UTC(),
	}
	if cfg, err := d.TenantConfig(context.WithoutCancel(ctx)); err == nil {
		if price, ok := cfg.Budget.PerUnitMinor[rec.Operation]; ok {
			rec.EstimatedMinor = int64(math.Round(price * gib))
		}
	}
	return rec
}

// uploadVisibility is private unless the destination cannot do private (I224).
func uploadVisibility(caps connector.PublishingCapabilities, record domain.Publication) (domain.Visibility, error) {
	if len(caps.Visibilities) == 0 || slices.Contains(caps.Visibilities, domain.VisibilityPrivate) {
		return domain.VisibilityPrivate, nil
	}
	if record.ApprovalID == "" {
		return "", errs.Newf(errs.CodeFailedPrecondition, "destination %s has no private state, so an upload needs an approval first (D37)", record.Destination)
	}
	return record.Visibility, nil
}

// openSource verifies the stored file and returns a reader for it. Publishing reads
// the master role directly; only without range reads does it fall back to the
// environment's verified local copy. It never touches archive.
func openSource(ctx context.Context, d node.DaemonEnv, file domain.MediaFile) (io.ReaderAt, int64, string, func(), error) {
	if file.Storage != domain.RoleMaster {
		return nil, 0, "", nil, errs.Newf(errs.CodeForbiddenStorageRole, "publishing only reads from the master role, not %s", file.Storage)
	}
	st, err := d.Storage(ctx, file.Storage)
	if err != nil {
		return nil, 0, "", nil, err
	}
	info, err := st.Stat(ctx, file.Key)
	if err != nil {
		return nil, 0, "", nil, err
	}
	if file.SHA256 != "" {
		res, err := st.Verify(ctx, file.Key, file.SHA256)
		switch {
		case errs.IsCode(err, errs.CodeCapabilityUnsupported):
			d.Logger().Warn("the master role cannot verify checksums before an upload", "key_sha256", file.SHA256)
		case err != nil:
			return nil, 0, "", nil, err
		case !res.OK:
			return nil, 0, "", nil, errs.New(errs.CodeFailedPrecondition, "the stored file does not match its recorded checksum")
		}
	}
	contentType := info.ContentType
	if contentType == "" {
		contentType = "video/mp4"
	}
	if st.Capabilities().RangeReads {
		r := &rangeReader{ctx: ctx, storage: st, key: file.Key, size: info.SizeBytes}
		return r, info.SizeBytes, contentType, r.close, nil
	}
	local, err := d.OpenInput(ctx, file.URI(), domain.RoleArchive)
	if err != nil {
		return nil, 0, "", nil, err
	}
	f, err := os.Open(local.Path)
	if err != nil {
		return nil, 0, "", nil, errs.Wrap(errs.CodeUnavailable, err, "cannot open the local copy of the file")
	}
	return f, info.SizeBytes, contentType, func() { f.Close() }, nil
}

// rangeReader adapts Storage.DownloadRange to io.ReaderAt. It keeps one stream open
// and reuses it while reads are sequential, which is how uploads read.
type rangeReader struct {
	ctx     context.Context
	storage connector.Storage
	key     string
	size    int64

	mu     sync.Mutex
	stream io.ReadCloser
	pos    int64
}

func (r *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if off >= r.size {
		return 0, io.EOF
	}
	if r.stream == nil || off != r.pos {
		r.closeStream()
		stream, err := r.storage.DownloadRange(r.ctx, r.key, off, -1)
		if err != nil {
			return 0, err
		}
		r.stream, r.pos = stream, off
	}
	n, err := io.ReadFull(r.stream, p)
	r.pos += int64(n)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		r.closeStream()
		return n, io.EOF
	case err != nil:
		r.closeStream()
		return n, err
	case r.pos == r.size:
		return n, io.EOF
	}
	return n, nil
}

func (r *rangeReader) closeStream() {
	if r.stream != nil {
		r.stream.Close()
		r.stream = nil
	}
}

func (r *rangeReader) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeStream()
}
