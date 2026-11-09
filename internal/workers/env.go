package workers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/connectors/storage/resolver"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/workers/cache"
	"github.com/rafaelp/vidyom/internal/workers/provenance"
)

// LeaseServices is the part of the coordinator a running task needs. Both calls are
// refused by the daemon without a live lease (I39).
type LeaseServices interface {
	Connection(ctx context.Context, taskID string, req workerproto.ConnectionRequest) (workerproto.ConnectionResponse, error)
	CacheKey(ctx context.Context, taskID string, req workerproto.CacheKeyRequest) (workerproto.CacheKeyResponse, error)
}

// TaskDeps is what every task environment shares.
type TaskDeps struct {
	Services    LeaseServices
	Connectors  *connector.Registry
	Cache       *cache.Cache
	ScratchDir  string
	SpoolDir    string
	IngestRoots []string
	AIWorker    config.AIWorker
	Transcriber string // driver name for pool ai
	Embedder    string // driver name for pool ai
	Metrics     telemetry.Metrics
	Now         func() time.Time
}

// TaskEnv is the node.Env of one task, built only from its lease: the tenant cannot
// come from anywhere else.
type TaskEnv struct {
	deps    TaskDeps
	lease   workerproto.Lease
	logger  *slog.Logger
	scratch *scratchArea
	meter   *meter

	mu       sync.Mutex
	storages map[domain.StorageRole]*meteredStorage
	closers  []io.Closer
	inputs   map[string]node.LocalInput
	tenant   *cache.Tenant
	inputDir string

	progressMu sync.Mutex
	progress   progressState
}

type progressState struct {
	percent float64
	message string
}

var _ node.Env = (*TaskEnv)(nil)

// NewTaskEnv builds the environment of a leased task and marks its job's scratch as
// in use.
func NewTaskEnv(deps TaskDeps, lease workerproto.Lease, logger *slog.Logger) (*TaskEnv, error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Metrics == nil {
		deps.Metrics = telemetry.Noop{}
	}
	if _, err := touchJob(deps.ScratchDir, lease.TenantID, lease.JobULID, deps.Now()); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &TaskEnv{
		deps:   deps,
		lease:  lease,
		logger: logger,
		scratch: &scratchArea{
			tenantDir: filepath.Join(deps.ScratchDir, string(lease.TenantID)),
			job:       lease.JobULID,
			nodeID:    lease.NodeID,
			ancestors: lease.ScratchJobs,
		},
		meter:    newMeter(deps.Metrics, lease.TenantID),
		storages: map[domain.StorageRole]*meteredStorage{},
		inputs:   map[string]node.LocalInput{},
	}, nil
}

// Tenant returns the leased tenant.
func (e *TaskEnv) Tenant() domain.TenantID { return e.lease.TenantID }

// JobID returns the job's friendly ID.
func (e *TaskEnv) JobID() id.Friendly { return e.lease.JobID }

// JobULID returns the job's ULID.
func (e *TaskEnv) JobULID() id.ULID { return e.lease.JobULID }

// NodeID returns the node's ID in its workflow.
func (e *TaskEnv) NodeID() string { return e.lease.NodeID }

// TraceID returns the job's trace ID.
func (e *TaskEnv) TraceID() string { return e.lease.TraceID }

// Logger returns the task's logger. Its lines also go to the job's stream.
func (e *TaskEnv) Logger() *slog.Logger { return e.logger }

// Progress records progress. The next heartbeat carries it.
func (e *TaskEnv) Progress(percent float64, message string) {
	e.progressMu.Lock()
	defer e.progressMu.Unlock()
	e.progress = progressState{percent, message}
}

func (e *TaskEnv) lastProgress() progressState {
	e.progressMu.Lock()
	defer e.progressMu.Unlock()
	return e.progress
}

// Scratch returns the job's working area.
func (e *TaskEnv) Scratch() node.Scratch { return e.scratch }

// HasPermission reports a permission granted to the job at submission.
func (e *TaskEnv) HasPermission(p domain.Permission) bool {
	return slices.Contains(e.lease.Permissions, p)
}

// Storage opens a role for this tenant with what the daemon returns for this lease.
// The credential stays in this task's memory (I39).
func (e *TaskEnv) Storage(ctx context.Context, role domain.StorageRole) (connector.Storage, error) {
	if !role.Valid() {
		return nil, errs.Newf(errs.CodeInvalidArgument, "unknown storage role %q", role)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.storages[role]; ok {
		return s, nil
	}
	if e.deps.Connectors == nil {
		return nil, errs.New(errs.CodeFailedPrecondition, "this worker has no storage drivers")
	}
	conn, err := e.deps.Services.Connection(ctx, e.lease.TaskID, workerproto.ConnectionRequest{LeaseID: e.lease.LeaseID, Role: role})
	if err != nil {
		return nil, err
	}
	opened, err := resolver.Open(ctx, e.deps.Connectors, connector.StorageParams{
		Tenant:   e.lease.TenantID,
		Role:     role,
		Instance: conn.Instance,
		Secrets:  leasedSecret{ref: conn.Instance.CredentialsRef, value: conn.Credentials},
	})
	if err != nil {
		return nil, err
	}
	if closer, ok := opened.(io.Closer); ok {
		e.closers = append(e.closers, closer)
	}
	metered := meterStorage(opened, e.meter)
	e.storages[role] = metered
	return metered, nil
}

// leasedSecret answers the one credential the daemon handed over for this role.
type leasedSecret struct {
	ref   string
	value []byte
}

func (s leasedSecret) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	if ref == "" || ref != s.ref {
		return nil, errs.New(errs.CodeForbidden, "this credential was not leased to this task")
	}
	if len(s.value) == 0 {
		return nil, errs.New(errs.CodeFailedPrecondition, "the daemon returned no credential for this role")
	}
	return append([]byte(nil), s.value...), nil
}

// tempInput returns a path for a plaintext copy of an input. It lives in a hidden
// per-task directory, removed when the task ends (I181).
func (e *TaskEnv) tempInput(ref string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inputDir == "" {
		dir, err := os.MkdirTemp(e.nodeScratchDir(), ".input-")
		if err != nil {
			return "", errs.Wrap(errs.CodeUnavailable, err, "cannot create a temporary input")
		}
		e.inputDir = dir
	}
	sum := sha256.Sum256([]byte(ref))
	return filepath.Join(e.inputDir, hex.EncodeToString(sum[:])), nil
}

func (e *TaskEnv) nodeScratchDir() string {
	dir := e.scratch.nodeDir()
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// tenantCache returns the tenant's cache, fetching its key on first use. The key is
// never stored and dies with the task (I39).
func (e *TaskEnv) tenantCache(ctx context.Context) (*cache.Tenant, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tenant != nil {
		return e.tenant, nil
	}
	if e.deps.Cache == nil {
		return nil, errs.New(errs.CodeFailedPrecondition, "this worker has no cache configured")
	}
	key, err := e.deps.Services.CacheKey(ctx, e.lease.TaskID, workerproto.CacheKeyRequest{LeaseID: e.lease.LeaseID})
	if err != nil {
		return nil, err
	}
	tenant, err := e.deps.Cache.ForTenant(e.lease.TenantID, key.Key)
	if err != nil {
		return nil, err
	}
	e.tenant = tenant
	return tenant, nil
}

// OpenInput resolves a reference to a local readable path. Forbidden roles are
// refused before anything is opened, including the role a scratch file records.
func (e *TaskEnv) OpenInput(ctx context.Context, ref string, forbid ...domain.StorageRole) (node.LocalInput, error) {
	uri, err := domain.ParseInputURI(ref)
	if err != nil {
		return node.LocalInput{}, errs.Wrap(errs.CodeInvalidArgument, err, "invalid input reference")
	}
	if err := uri.ForbidRole(forbid...); err != nil {
		return node.LocalInput{}, err
	}
	canonical := uri.String()
	e.mu.Lock()
	if known, ok := e.inputs[canonical]; ok {
		e.mu.Unlock()
		if slices.Contains(forbid, known.Role) {
			return node.LocalInput{}, errs.Newf(errs.CodeForbiddenStorageRole, "reading from the %s role is forbidden here", known.Role)
		}
		return known, nil
	}
	e.mu.Unlock()

	var input node.LocalInput
	switch uri.Kind {
	case domain.URIServer:
		input, err = e.openServer(uri)
	case domain.URIUpload:
		input, err = e.openUpload(uri)
	case domain.URIStorage:
		input, err = e.openStorage(ctx, uri, canonical)
	case domain.URIScratch:
		input, err = e.openScratch(ctx, uri, forbid)
	case domain.URIImport:
		err = errs.NotImplemented("import: inputs")
	default:
		err = errs.Newf(errs.CodeInvalidArgument, "unknown input reference form")
	}
	if err != nil {
		return node.LocalInput{}, err
	}
	e.mu.Lock()
	e.inputs[canonical] = input
	e.mu.Unlock()
	return input, nil
}

func (e *TaskEnv) openServer(uri domain.InputURI) (node.LocalInput, error) {
	path, err := domain.ResolveServerPath(e.deps.IngestRoots, uri.Path)
	if err != nil {
		return node.LocalInput{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return node.LocalInput{}, errs.NotFound("server path")
	}
	if st.IsDir() {
		return node.LocalInput{}, errs.New(errs.CodeInvalidArgument, "the server path is a directory")
	}
	return node.LocalInput{Path: path, Size: st.Size()}, nil
}

// openUpload reads a complete upload from the shared spool. The spool is named by
// ULID, and a friendly reference only resolves if the daemon linked it (I183).
func (e *TaskEnv) openUpload(uri domain.InputURI) (node.LocalInput, error) {
	if e.deps.SpoolDir == "" {
		return node.LocalInput{}, errs.New(errs.CodeFailedPrecondition, "this worker has no spool directory configured")
	}
	path := filepath.Join(e.deps.SpoolDir, string(e.lease.TenantID), uri.UploadRef)
	st, err := os.Stat(path)
	switch {
	case err == nil && st.Mode().IsRegular():
		return node.LocalInput{Path: path, Size: st.Size()}, nil
	case err != nil && !os.IsNotExist(err):
		return node.LocalInput{}, errs.Wrap(errs.CodeUnavailable, err, "cannot read the upload spool")
	case id.IsFriendly(uri.UploadRef):
		return node.LocalInput{}, errs.Newf(errs.CodeFailedPrecondition, "upload %s reached the worker by its friendly ID, and the spool is named by ULID: the daemon must rewrite it on accepting the run (I183)", uri.UploadRef)
	default:
		return node.LocalInput{}, errs.NotFound("upload")
	}
}

// openStorage reads an object through the tenant's verified cache. The checksum from
// Stat is the origin's word on what the bytes must be, on a hit and on a miss.
func (e *TaskEnv) openStorage(ctx context.Context, uri domain.InputURI, canonical string) (node.LocalInput, error) {
	storage, err := e.Storage(ctx, uri.Role)
	if err != nil {
		return node.LocalInput{}, err
	}
	info, err := storage.Stat(ctx, uri.Key)
	if err != nil {
		return node.LocalInput{}, err
	}
	origin := info.SHA256
	if origin == "" {
		origin, err = storage.Checksum(ctx, uri.Key)
		if err != nil {
			return node.LocalInput{}, err
		}
	}
	tenant, err := e.tenantCache(ctx)
	if err != nil {
		return node.LocalInput{}, err
	}
	dst, err := e.tempInput(canonical)
	if err != nil {
		return node.LocalInput{}, err
	}
	res, err := tenant.Fetch(ctx, cache.Request{Role: uri.Role, Key: uri.Key, SHA256: origin}, dst, func(ctx context.Context) (io.ReadCloser, error) {
		rc, _, err := storage.Get(ctx, uri.Key)
		return rc, err
	})
	if err != nil {
		return node.LocalInput{}, err
	}
	return node.LocalInput{Path: dst, Role: uri.Role, SHA256: res.SHA256, Size: res.SizeBytes}, nil
}

// openScratch returns a file of this job. A recorded role is verified against the
// file's checksum, so a damaged cached copy cannot pass as the master (§6.4).
func (e *TaskEnv) openScratch(ctx context.Context, uri domain.InputURI, forbid []domain.StorageRole) (node.LocalInput, error) {
	path, err := e.scratch.Resolve(uri)
	if err != nil {
		return node.LocalInput{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return node.LocalInput{}, errs.NotFound("scratch file")
	}
	if st.IsDir() {
		return node.LocalInput{}, errs.New(errs.CodeInvalidArgument, "the scratch reference is a directory")
	}
	input := node.LocalInput{Path: path, Size: st.Size()}
	origin, known, err := provenance.Read(path)
	if err != nil {
		return node.LocalInput{}, errs.Wrap(errs.CodeFailedPrecondition, err, "a scratch file's origin record is unreadable")
	}
	if !known {
		return input, nil
	}
	if origin.Role != "" {
		if slices.Contains(forbid, origin.Role) {
			return node.LocalInput{}, errs.Newf(errs.CodeForbiddenStorageRole, "this scratch file comes from the %s role, which is forbidden here", origin.Role)
		}
		sum, _, err := provenance.HashFile(ctx, path)
		if err != nil {
			return node.LocalInput{}, errs.Wrap(errs.CodeUnavailable, err, "cannot verify a scratch file")
		}
		if origin.SHA256 == "" || sum != origin.SHA256 {
			return node.LocalInput{}, errs.New(errs.CodeForbiddenStorageRole, "a scratch file no longer matches the checksum of its origin")
		}
		input.Role = origin.Role
		input.SHA256 = origin.SHA256
	}
	return input, nil
}

// Close ends the task's hold on its resources: plaintext inputs go, instances close.
func (e *TaskEnv) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inputDir != "" {
		_ = os.RemoveAll(e.inputDir)
		e.inputDir = ""
	}
	for _, closer := range e.closers {
		_ = closer.Close()
	}
	e.closers = nil
	e.storages = map[domain.StorageRole]*meteredStorage{}
	e.inputs = map[string]node.LocalInput{}
	e.tenant = nil
}

// Usage returns what the task transferred, for the daemon to price (I195).
func (e *TaskEnv) Usage() []domain.UsageRecord {
	records := e.meter.records()
	for i := range records {
		records[i].TenantID = e.lease.TenantID
		records[i].JobID = string(e.lease.JobID)
		records[i].NodeID = e.lease.NodeID
		records[i].At = e.deps.Now().UTC()
	}
	return records
}

// aiEnv adds the AI connectors for pool ai. Media nodes never see them.
type aiEnv struct {
	*TaskEnv
}

var _ node.AIEnv = aiEnv{}

func (a aiEnv) params() connector.AIParams {
	p := connector.AIParams{Tenant: a.lease.TenantID, Daemon: a.deps.AIWorker}
	if a.lease.AI != nil {
		p.AI = *a.lease.AI
	}
	return p
}

// Transcriber opens the configured transcription driver (I197).
func (a aiEnv) Transcriber(ctx context.Context) (connector.Transcriber, error) {
	factory, err := a.deps.Connectors.Transcriber(a.deps.Transcriber)
	if err != nil {
		return nil, err
	}
	return factory(ctx, a.params())
}

// Embedder opens the configured embedding driver (I197).
func (a aiEnv) Embedder(ctx context.Context) (connector.Embedder, error) {
	factory, err := a.deps.Connectors.Embedder(a.deps.Embedder)
	if err != nil {
		return nil, err
	}
	return factory(ctx, a.params())
}
