package publishingnodes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/connectors/catalog/mediacms"
	catalogpublic "github.com/rafaelp/vidyom/connectors/catalog/public"
	"github.com/rafaelp/vidyom/connectors/publishing/mock"
	"github.com/rafaelp/vidyom/connectors/publishing/resolver"
	"github.com/rafaelp/vidyom/connectors/publishing/vimeo"
	"github.com/rafaelp/vidyom/connectors/publishing/youtube"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/publicexport"
	"github.com/rafaelp/vidyom/internal/publicexport/fakestore"
	"github.com/rafaelp/vidyom/schemas"
)

const (
	tenantID     = domain.TenantID("acme")
	mockDest     = "mock-main"
	masterKey    = "clips/CLIP-001/v1/master.mp4"
	masterBytes  = "fictional clip master bytes"
	publicText   = "conscious breathing begins at the base of the spine"
	privateWords = "the student named Maria told us about her diagnosis"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// memStorage is one role in memory. It records every key read, so tests can prove
// what was and was not touched.
type memStorage struct {
	connector.Storage // anything else panics: publishing must not use it
	role              domain.StorageRole
	objects           map[string]string
	noRangeReads      bool
	verifyErr         error
	h                 *harness
}

func (m *memStorage) Capabilities() connector.StorageCapabilities {
	return connector.StorageCapabilities{RangeReads: !m.noRangeReads}
}

func (m *memStorage) Stat(_ context.Context, key string) (connector.ObjectInfo, error) {
	data, ok := m.objects[key]
	if !ok {
		return connector.ObjectInfo{}, errs.NotFound("object")
	}
	return connector.ObjectInfo{Key: key, SizeBytes: int64(len(data))}, nil
}

func (m *memStorage) Verify(_ context.Context, key, want string) (connector.VerifyResult, error) {
	if m.verifyErr != nil {
		return connector.VerifyResult{}, m.verifyErr
	}
	data, ok := m.objects[key]
	if !ok {
		return connector.VerifyResult{}, errs.NotFound("object")
	}
	return connector.VerifyResult{OK: sha(data) == want, SHA256: sha(data), SizeBytes: int64(len(data))}, nil
}

func (m *memStorage) Get(_ context.Context, key string) (io.ReadCloser, connector.ObjectInfo, error) {
	m.h.noteRead(m.role, key)
	data, ok := m.objects[key]
	if !ok {
		return nil, connector.ObjectInfo{}, errs.NotFound("object")
	}
	return io.NopCloser(strings.NewReader(data)), connector.ObjectInfo{Key: key}, nil
}

func (m *memStorage) DownloadRange(_ context.Context, key string, off, length int64) (io.ReadCloser, error) {
	m.h.noteRead(m.role, key)
	data, ok := m.objects[key]
	if !ok {
		return nil, errs.NotFound("object")
	}
	if off > int64(len(data)) {
		return nil, errs.New(errs.CodeInvalidArgument, "range past the end")
	}
	out := data[off:]
	if length >= 0 && length < int64(len(out)) {
		out = out[:length]
	}
	return io.NopCloser(strings.NewReader(out)), nil
}

// harness runs nodes against the fake store, the real mock destination and the real
// exporter.
type harness struct {
	t        *testing.T
	store    *fakestore.Store
	cfg      config.Tenant
	registry *connector.Registry
	resolver *resolver.Resolver
	nodes    node.Registry
	stateDir string
	dataDir  string
	library  string
	storages map[domain.StorageRole]*memStorage
	embedder connector.Embedder
	exporter Exporter
	secrets  map[string][]byte
	logBuf   *bytes.Buffer

	mu     sync.Mutex
	reads  map[domain.StorageRole][]string
	opened []domain.StorageRole
	inputs []string
	jobSeq int
}

type tenants struct{ h *harness }

func (t tenants) TenantConfig(context.Context, domain.TenantID) (config.Tenant, error) {
	return t.h.cfg, nil
}

// vault is the tenant vault's answer to a credentialsRef.
type vault struct{ h *harness }

func (v vault) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	value, ok := v.h.secrets[ref]
	if !ok {
		return nil, errs.NotFound("secret")
	}
	return value, nil
}

type keyProvider struct{}

func (keyProvider) CacheKey(context.Context, domain.TenantID) ([]byte, error) { return nil, nil }
func (keyProvider) PublicIDKey(_ context.Context, t domain.TenantID) ([]byte, error) {
	return []byte("fictional key of " + string(t)), nil
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:        t,
		store:    fakestore.New(tenantID),
		registry: connector.NewRegistry(),
		nodes:    node.NewRegistry(),
		stateDir: t.TempDir(),
		dataDir:  t.TempDir(),
		library:  t.TempDir(),
		reads:    map[domain.StorageRole][]string{},
		logBuf:   &bytes.Buffer{},
		secrets:  map[string][]byte{},
	}
	h.storages = map[domain.StorageRole]*memStorage{
		domain.RoleMaster:  {role: domain.RoleMaster, objects: map[string]string{masterKey: masterBytes}, h: h},
		domain.RoleProxy:   {role: domain.RoleProxy, objects: map[string]string{"clips/CLIP-001/v1/thumbnail.jpg": "jpeg"}, h: h},
		domain.RoleArchive: {role: domain.RoleArchive, objects: map[string]string{masterKey: masterBytes}, h: h},
	}
	mock.Register(h.registry)
	youtube.Register(h.registry)
	vimeo.Register(h.registry)
	catalogpublic.Register(h.registry)
	mediacms.Register(h.registry)
	h.setMockParams(map[string]any{})
	h.cfg.Publishing.Defaults.RequireApproval = true
	h.setLibrary()
	h.resolver = resolver.New(h.registry, tenants{h}, func(domain.TenantID) connector.SecretResolver { return vault{h} })
	h.exporter = publicexport.New(publicexport.Options{DataDir: h.dataDir, Keys: keyProvider{}})
	Register(h.nodes, Options{
		Exporter: exporterFunc(func(ctx context.Context, r publicexport.Request) (publicexport.Result, error) {
			return h.exporter.Export(ctx, r)
		}),
		Connectors: h.registry,
		PollMin:    time.Millisecond,
		PollMax:    2 * time.Millisecond,
	})
	h.seed()
	return h
}

type exporterFunc func(context.Context, publicexport.Request) (publicexport.Result, error)

func (f exporterFunc) Export(ctx context.Context, r publicexport.Request) (publicexport.Result, error) {
	return f(ctx, r)
}

func (h *harness) setMockParams(extra map[string]any) {
	params := map[string]any{"stateDir": h.stateDir}
	for k, v := range extra {
		params[k] = v
	}
	raw, _ := json.Marshal(params)
	if h.cfg.Publishing.Destinations == nil {
		h.cfg.Publishing.Destinations = map[string]config.Destination{}
	}
	dest := h.cfg.Publishing.Destinations[mockDest]
	dest.Driver = mock.Driver
	dest.Parameters = raw
	h.cfg.Publishing.Destinations[mockDest] = dest
}

func (h *harness) setLibrary() {
	raw, _ := json.Marshal(map[string]string{"directory": h.library})
	h.cfg.Catalogs.PublicLibrary = &config.CatalogInstance{Driver: catalogpublic.Driver, Parameters: raw}
}

func (h *harness) noteRead(role domain.StorageRole, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads[role] = append(h.reads[role], key)
}

func (h *harness) ctx() context.Context { return context.Background() }

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

// seed creates an original and an approved clip of it, ready to publish.
func (h *harness) seed() {
	ctx := h.ctx()
	h.must(h.store.Assets().Create(ctx, domain.Asset{
		ID: "ORI-001", ULID: id.New(), TenantID: tenantID, Kind: domain.KindOriginal, Title: "Private recording",
		Status: domain.AssetReadyForClipping, Visibility: domain.VisibilityPrivate, CurrentVersion: "v1", MasterAvailable: true,
	}))
	h.must(h.store.Versions().Create(ctx, domain.AssetVersion{AssetID: "ORI-001", Version: "v1"}))
	h.must(h.store.Assets().Create(ctx, domain.Asset{
		ID: "CLIP-001", ULID: id.New(), TenantID: tenantID, Kind: domain.KindClip, Title: "Conscious breathing",
		Description: "A clip about breath.", Tags: []string{"breathing"}, Language: "en", DurationSeconds: 30,
		Status: domain.AssetApproved, Visibility: domain.VisibilityPrivate, CurrentVersion: "v1", MasterAvailable: true,
		ParentAssetID: "ORI-001",
		Lineage: &domain.Lineage{
			Method: domain.LineageComputed, SourceAssetID: "ORI-001", RootAssetID: "ORI-001",
			Segments: []domain.Segment{{SourceAssetID: "ORI-001", SourceVersion: "v1", StartSeconds: 100, EndSeconds: 130}},
		},
	}))
	h.must(h.store.Versions().Create(ctx, domain.AssetVersion{
		AssetID: "CLIP-001", Version: "v1",
		Files: []domain.VersionFile{{Purpose: domain.FileWorkingMaster, File: domain.MediaFile{
			Storage: domain.RoleMaster, Key: masterKey, SHA256: sha(masterBytes), SizeBytes: int64(len(masterBytes)), Verified: true,
		}}},
	}))
	h.must(h.store.Transcripts().Save(ctx, domain.Transcript{
		AssetID: "CLIP-001", Version: "v1", Language: "en", DurationSeconds: 30,
		Segments: []domain.TranscriptSegment{
			{Start: 0, End: 10, Text: publicText},
			{Start: 10, End: 20, Text: privateWords},
			{Start: 20, End: 30, Text: "and the exhale follows without effort"},
		},
	}))
	h.must(h.store.Transcripts().MarkSensitive(ctx, domain.SensitiveSegment{
		AssetID: "CLIP-001", Version: "v1", StartSeconds: 11, EndSeconds: 19, Reason: "health",
	}))
	h.addApproval("APR-001", domain.ApprovalPublication, domain.ApprovalApproved, "CLIP-001", "v1")
}

func (h *harness) addApproval(friendly id.Friendly, kind domain.ApprovalKind, status domain.ApprovalStatus, asset id.Friendly, version string) {
	h.must(h.store.Approvals().Create(h.ctx(), domain.Approval{
		ID: friendly, ULID: id.New(), TenantID: tenantID, Kind: kind, Status: status, AssetID: asset, AssetVersion: version,
	}))
}

// run executes a node, checking its parameters and output against the catalog.
type runOpts struct {
	attempt int
	key     string
	ctx     context.Context
	node    string
}

func (h *harness) node(nodeType string) node.Node {
	n, ok := h.nodes.Lookup(nodeType, 1)
	if !ok {
		h.t.Fatalf("node %s is not registered", nodeType)
	}
	return n
}

func (h *harness) input(nodeType string, params any, o runOpts) (context.Context, *env, node.Input) {
	h.t.Helper()
	raw, err := json.Marshal(params)
	h.must(err)
	cat, err := schemas.NodeCatalog()
	h.must(err)
	entry, ok := cat.Lookup(nodeType, 1)
	if !ok {
		h.t.Fatalf("%s is not in the catalog", nodeType)
	}
	if err := schemas.Validate(entry.ParametersRef(), raw); err != nil {
		h.t.Fatalf("%s parameters %s fail the catalog: %v", nodeType, raw, err)
	}
	ctx := o.ctx
	if ctx == nil {
		ctx = h.ctx()
	}
	if o.attempt == 0 {
		o.attempt = 1
	}
	if o.key == "" {
		o.key = "key-of-" + nodeType
		if nodeType == "publish.prepare" {
			o.key = "prepare-key-" + mockDest
		}
	}
	return ctx, &env{h: h, nodeID: firstOf(o.node, nodeType)}, node.Input{Parameters: raw, Attempt: o.attempt, IdempotencyKey: o.key}
}

func (h *harness) checkOutput(nodeType string, r node.Result) {
	h.t.Helper()
	cat, err := schemas.NodeCatalog()
	h.must(err)
	entry, _ := cat.Lookup(nodeType, 1)
	if len(r.Output) == 0 {
		h.t.Fatalf("%s returned no output", nodeType)
	}
	if err := schemas.Validate(entry.OutputRef(), r.Output); err != nil {
		h.t.Fatalf("%s output %s fails the catalog: %v", nodeType, r.Output, err)
	}
}

func (h *harness) runErr(nodeType string, params any, opts ...runOpts) (node.Result, error) {
	h.t.Helper()
	var o runOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	ctx, e, in := h.input(nodeType, params, o)
	res, err := h.node(nodeType).Execute(ctx, e, in)
	if err == nil {
		h.checkOutput(nodeType, res)
	}
	return res, err
}

// run fails the test on an error and decodes the output.
func run[T any](h *harness, nodeType string, params any, opts ...runOpts) T {
	h.t.Helper()
	res, err := h.runErr(nodeType, params, opts...)
	if err != nil {
		h.t.Fatalf("%s: %v", nodeType, err)
	}
	var out T
	h.must(json.Unmarshal(res.Output, &out))
	return out
}

func (h *harness) publication(ref string) domain.Publication {
	h.t.Helper()
	p, err := h.store.Publications().Get(h.ctx(), id.Ref(ref))
	h.must(err)
	return p
}

func (h *harness) asset(ref string) domain.Asset {
	h.t.Helper()
	a, err := h.store.Assets().Get(h.ctx(), id.Ref(ref))
	h.must(err)
	return a
}

func (h *harness) mockState() mock.State {
	h.t.Helper()
	s, err := mock.ReadState(h.stateDir, tenantID, mockDest)
	h.must(err)
	return s
}

func (h *harness) publicDB() string {
	return filepath.Join(h.library, string(tenantID), "public.db")
}

// freshResolver forgets open connectors, as a restarted daemon has.
func freshResolver(h *harness) *resolver.Resolver {
	return resolver.New(h.registry, tenants{h}, func(domain.TenantID) connector.SecretResolver { return vault{h} })
}

// prepared runs publish.prepare with the seeded approval.
func (h *harness) prepared() prepareOutput {
	h.t.Helper()
	return run[prepareOutput](h, "publish.prepare", map[string]any{
		"assetId": "CLIP-001", "destination": mockDest, "visibility": "public", "approvalId": "APR-001",
	})
}

// published runs the whole flow to published and returns the publication ID.
func (h *harness) published() string {
	h.t.Helper()
	pub := h.prepared().PublicationID
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": pub})
	run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
	return pub
}

// env is node.DaemonEnv for tests.
type env struct {
	h      *harness
	nodeID string
}

var _ node.DaemonEnv = (*env)(nil)

func (e *env) Tenant() domain.TenantID              { return tenantID }
func (e *env) JobID() id.Friendly                   { return "JOB-001" }
func (e *env) JobULID() id.ULID                     { return "01J00000000000000000000000" }
func (e *env) NodeID() string                       { return e.nodeID }
func (e *env) TraceID() string                      { return "trace" }
func (e *env) Logger() *slog.Logger                 { return slog.New(slog.NewTextHandler(e.h.logBuf, nil)) }
func (e *env) Progress(float64, string)             {}
func (e *env) Scratch() node.Scratch                { return nil }
func (e *env) HasPermission(domain.Permission) bool { return false }

func (e *env) Storage(_ context.Context, role domain.StorageRole) (connector.Storage, error) {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	e.h.opened = append(e.h.opened, role)
	return e.h.storages[role], nil
}

func (e *env) OpenInput(_ context.Context, ref string, forbid ...domain.StorageRole) (node.LocalInput, error) {
	e.h.mu.Lock()
	e.h.inputs = append(e.h.inputs, ref)
	e.h.mu.Unlock()
	u, err := domain.ParseInputURI(ref)
	if err != nil {
		return node.LocalInput{}, err
	}
	if err := u.ForbidRole(forbid...); err != nil {
		return node.LocalInput{}, err
	}
	if u.Kind != domain.URIStorage {
		return node.LocalInput{}, errs.New(errs.CodeNotImplemented, "only storage references in this fake")
	}
	data, ok := e.h.storages[u.Role].objects[u.Key]
	if !ok {
		return node.LocalInput{}, errs.NotFound("object")
	}
	path := filepath.Join(e.h.t.TempDir(), filepath.Base(u.Key))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		return node.LocalInput{}, err
	}
	return node.LocalInput{Path: path, Role: u.Role, Size: int64(len(data))}, nil
}

func (e *env) Store() repo.TenantStore { return e.h.store }

func (e *env) Emit(ctx context.Context, tx repo.TenantRepos, d event.Draft) error {
	payload, err := json.Marshal(d.Payload)
	if err != nil {
		return err
	}
	target := tx
	if target == nil {
		target = e.h.store
	}
	_, err = target.Outbox().Append(ctx, event.Envelope{
		EventID: id.New(), Type: d.Type, SchemaVersion: event.SchemaVersion, OccurredAt: time.Now().UTC(),
		TenantID: tenantID, AssetID: d.AssetID, Payload: payload,
	})
	return err
}

func (e *env) Publishing(ctx context.Context, destination string) (connector.Publishing, error) {
	return e.h.resolver.Publishing(ctx, tenantID, destination)
}

func (e *env) TenantConfig(context.Context) (config.Tenant, error) { return e.h.cfg, nil }

func (e *env) Embedder(context.Context) (connector.Embedder, error) {
	if e.h.embedder == nil {
		return nil, errs.New(errs.CodeUnavailable, "no embedder")
	}
	return e.h.embedder, nil
}

func (e *env) LLM(context.Context, domain.Asset) (connector.LLM, error) {
	panic("publishing nodes never call an LLM")
}

func (e *env) Secrets() connector.SecretResolver { return vault{e.h} }

// ioReader names the reader parameter of the connector's upload methods.
type ioReader = io.Reader

// scripted wraps a real connector with overrides, registered as its own driver.
type scripted struct {
	connector.Publishing
	caps      *connector.PublishingCapabilities
	getStatus func(context.Context, string) (connector.RemoteVideo, error)
	setVis    func(context.Context, string, domain.Visibility) (connector.RemoteVideo, error)
	remove    func(context.Context, string) error
	reconcile func(context.Context, string) (*connector.RemoteVideo, error)
	upload    func(context.Context, connector.UploadRequest) (connector.RemoteVideo, error)
	thumb     func(context.Context, string, io.Reader, string) error
	captions  func(context.Context, string, string, string, io.Reader) error
}

func (s *scripted) Capabilities() connector.PublishingCapabilities {
	if s.caps != nil {
		return *s.caps
	}
	return s.Publishing.Capabilities()
}
func (s *scripted) GetStatus(ctx context.Context, id string) (connector.RemoteVideo, error) {
	if s.getStatus != nil {
		return s.getStatus(ctx, id)
	}
	return s.Publishing.GetStatus(ctx, id)
}
func (s *scripted) SetVisibility(ctx context.Context, id string, v domain.Visibility) (connector.RemoteVideo, error) {
	if s.setVis != nil {
		return s.setVis(ctx, id, v)
	}
	return s.Publishing.SetVisibility(ctx, id, v)
}
func (s *scripted) Delete(ctx context.Context, id string) error {
	if s.remove != nil {
		return s.remove(ctx, id)
	}
	return s.Publishing.Delete(ctx, id)
}
func (s *scripted) Reconcile(ctx context.Context, key string) (*connector.RemoteVideo, error) {
	if s.reconcile != nil {
		return s.reconcile(ctx, key)
	}
	return s.Publishing.Reconcile(ctx, key)
}
func (s *scripted) Upload(ctx context.Context, r connector.UploadRequest) (connector.RemoteVideo, error) {
	if s.upload != nil {
		return s.upload(ctx, r)
	}
	return s.Publishing.Upload(ctx, r)
}
func (s *scripted) UploadThumbnail(ctx context.Context, id string, r io.Reader, ct string) error {
	if s.thumb != nil {
		return s.thumb(ctx, id, r, ct)
	}
	return s.Publishing.UploadThumbnail(ctx, id, r, ct)
}
func (s *scripted) UploadCaptions(ctx context.Context, id, lang, format string, r io.Reader) error {
	if s.captions != nil {
		return s.captions(ctx, id, lang, format, r)
	}
	return s.Publishing.UploadCaptions(ctx, id, lang, format, r)
}

// addScripted registers a destination whose connector is the mock with overrides.
func (h *harness) addScripted(destination string, build func(*scripted)) *scripted {
	h.t.Helper()
	s := &scripted{}
	build(s)
	driver := "scripted-" + destination
	h.registry.RegisterPublishing(connector.Info{Driver: driver, Available: true}, func(ctx context.Context, p connector.PublishingParams) (connector.Publishing, error) {
		inner, err := mustFactory(h)(ctx, connector.PublishingParams{Tenant: p.Tenant, Destination: p.Destination, Config: p.Config})
		if err != nil {
			return nil, err
		}
		s.Publishing = inner
		return s, nil
	})
	params, _ := json.Marshal(map[string]string{"stateDir": h.stateDir})
	h.cfg.Publishing.Destinations[destination] = config.Destination{Driver: driver, Parameters: params}
	return s
}

func mustFactory(h *harness) connector.PublishingFactory {
	f, err := h.registry.Publishing(mock.Driver)
	h.must(err)
	return f
}

func hasEvent(events []event.Envelope, t event.Type) int {
	n := 0
	for _, e := range events {
		if e.Type == t {
			n++
		}
	}
	return n
}
