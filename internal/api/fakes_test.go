package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rafaelp/vidyom/internal/api/fakestore"
	"github.com/rafaelp/vidyom/internal/api/uploads"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
)

const (
	tenantA = domain.TenantID("acme")
	tenantB = domain.TenantID("globex")

	tokenAdmin     = "tok-admin"
	tokenPublisher = "tok-publisher"
	tokenEditor    = "tok-editor"
	tokenViewer    = "tok-viewer"
	tokenSystem    = "tok-system"
	tokenWorker    = "tok-worker"
	tokenRevoked   = "tok-revoked"
)

// world is a fake of everything behind the handler. Each call is recorded with its
// scope, so tests can assert what reached a service and what did not.
type world struct {
	t *testing.T

	mu       sync.Mutex
	calls    []recorded
	audit    []security.AuditEntry
	auditErr error
	err      error // returned by every service method when set
	tokens   map[string]domain.Principal
	members  map[string]domain.Role // email/tenant -> role

	stores     *fakestore.Stores
	logs       bytes.Buffer
	workflows  fakeWorkflows
	jobs       fakeJobs
	events     fakeEvents
	obs        fakeObservability
	assets     fakeAssets
	uploadsSvc *uploads.Service
	spool      string
	shutdown   chan struct{}
	doctor     fakeDoctor
	counts     map[string]int
}

type recorded struct {
	Name  string
	Scope service.Scope
	Args  any
}

func (w *world) called(name string, s service.Scope, args any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, recorded{name, s, args})
	return w.err
}

func (w *world) callNames() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, len(w.calls))
	for i, c := range w.calls {
		names[i] = c.Name
	}
	return names
}

func (w *world) lastCall(t *testing.T) recorded {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.calls) == 0 {
		t.Fatal("no service was called")
	}
	return w.calls[len(w.calls)-1]
}

func (w *world) resetCalls() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = nil
	w.audit = nil
}

func (w *world) auditRecords() []security.AuditEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]security.AuditEntry(nil), w.audit...)
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		t:      t,
		stores: fakestore.New(tenantA, tenantB),
		spool:  t.TempDir(),
		tokens: map[string]domain.Principal{
			tokenAdmin:     {UserULID: "01J0000000000000000000000A", Email: "admin@example.com", Method: domain.AuthDevice, DeviceID: "CLIENT-001"},
			tokenPublisher: {UserULID: "01J0000000000000000000000B", Email: "publisher@example.com", Method: domain.AuthDevice},
			tokenEditor:    {UserULID: "01J0000000000000000000000C", Email: "editor@example.com", Method: domain.AuthDevice},
			tokenViewer:    {UserULID: "01J0000000000000000000000D", Email: "viewer@example.com", Method: domain.AuthDevice},
			tokenSystem:    {UserULID: "01J0000000000000000000000E", Email: "root@example.com", Method: domain.AuthDevice, SystemAdmin: true},
		},
		members: map[string]domain.Role{
			"admin@example.com/acme":     domain.RoleAdmin,
			"publisher@example.com/acme": domain.RolePublisher,
			"editor@example.com/acme":    domain.RoleEditor,
			"viewer@example.com/acme":    domain.RoleViewer,
			"viewer@example.com/globex":  domain.RoleAdmin,
		},
		shutdown: make(chan struct{}),
		counts:   map[string]int{},
	}
	svc, err := uploads.New(uploads.Options{Stores: w.stores, SpoolDir: w.spool})
	if err != nil {
		t.Fatal(err)
	}
	w.uploadsSvc = svc
	w.workflows = fakeWorkflows{w: w}
	w.jobs = fakeJobs{w: w}
	w.events = fakeEvents{w: w}
	w.obs = fakeObservability{w: w}
	w.assets = fakeAssets{w: w}
	w.doctor = fakeDoctor{w: w}
	return w
}

func (w *world) deps() Deps {
	return Deps{
		Authenticator: authn{w},
		Authorizer:    authz{w},
		Auditor:       auditor{w},
		TenantStores:  w.stores,
		Workflows:     &w.workflows,
		Jobs:          &w.jobs,
		Approvals:     fakeApprovals{w},
		Events:        &w.events,
		Introspection: fakeIntrospection{w},
		Assets:        &w.assets,
		Search:        fakeSearch{w},
		Publications:  fakePublications{w},
		Uploads:       w.uploadsSvc,
		Storages:      fakeStorages{w},
		Secrets:       fakeSecrets{w},
		Devices:       fakeDevices{w},
		Tenants:       fakeTenants{w},
		Users:         fakeUsers{w},
		Budgets:       fakeBudgets{w},
		Audit:         fakeAudit{w},
		Plugins:       fakePlugins{w},
		Observability: &w.obs,
		Connectors:    fakeConnectors{w},
		Doctor:        &w.doctor,
		Coordinator:   fakeCoordinator{w},
		TenantConfig:  fakeTenantConfig{},
		Logger:        slog.New(slog.NewJSONHandler(lockedWriter{&w.mu, &w.logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Shutdown:      w.shutdown,
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (w *world) logText() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.logs.String()
}

// serve builds a handler, with the deps adjusted by tweak when given.
func (w *world) serve(tweak ...func(*Deps)) http.Handler {
	d := w.deps()
	for _, f := range tweak {
		f(&d)
	}
	return NewHandler(d)
}

// request is one test request.
type request struct {
	method, path string
	token        string
	tenant       domain.TenantID
	body         string
	key          string
	headers      map[string]string
}

func do(h http.Handler, req request) *httptest.ResponseRecorder {
	var body io.Reader
	if req.body != "" {
		body = bytes.NewBufferString(req.body)
	}
	r := httptest.NewRequest(req.method, req.path, body)
	if req.token != "" {
		r.Header.Set("Authorization", "Bearer "+req.token)
	}
	if req.tenant != "" {
		r.Header.Set(api.HeaderTenant, string(req.tenant))
	}
	if req.key != "" {
		r.Header.Set(api.HeaderIdempotencyKey, req.key)
	}
	for k, v := range req.headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, status, rec.Body.String())
	}
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code errs.Code) *errs.Error {
	t.Helper()
	wantStatus(t, rec, status)
	var body errs.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %s", rec.Body.String())
	}
	if body.Error == nil || body.Error.Code != code {
		t.Fatalf("error = %s, want code %s", rec.Body.String(), code)
	}
	if body.Error.RequestID == "" || body.Error.RequestID != rec.Header().Get(api.HeaderRequestID) {
		t.Fatalf("request ID missing from the error: %s", rec.Body.String())
	}
	return body.Error
}

// ---- Identity ----

type authn struct{ w *world }

func (a authn) Authenticate(_ context.Context, c security.Credentials) (domain.Principal, error) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	a.w.counts["authenticate"]++
	if c.BearerToken != "" {
		if p, ok := a.w.tokens[c.BearerToken]; ok {
			return p, nil
		}
		return domain.Principal{}, errs.New(errs.CodeUnauthenticated, "unknown credential")
	}
	if c.PeerUID != nil && *c.PeerUID == 1000 {
		return domain.Principal{Email: "admin@example.com", Method: domain.AuthPeer}, nil
	}
	return domain.Principal{}, errs.New(errs.CodeUnauthenticated, "no identity")
}

func (a authn) AuthenticateWorker(_ context.Context, c security.Credentials) (string, error) {
	if c.BearerToken == tokenWorker {
		return "worker-1", nil
	}
	return "", errs.New(errs.CodeUnauthenticated, "unknown worker token")
}

type authz struct{ w *world }

func (a authz) Resolve(_ context.Context, p domain.Principal, tenant domain.TenantID) (domain.Principal, error) {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	role, ok := a.w.members[p.Email+"/"+string(tenant)]
	if !ok {
		return domain.Principal{}, errs.NotFound("tenant")
	}
	p.TenantID, p.Role = tenant, role
	return p, nil
}

func (a authz) Require(_ context.Context, p domain.Principal, perm domain.Permission) error {
	if p.Can(perm) {
		return nil
	}
	return errs.Forbidden(string(perm))
}

type auditor struct{ w *world }

func (a auditor) Record(_ context.Context, e security.AuditEntry) error {
	a.w.mu.Lock()
	defer a.w.mu.Unlock()
	if a.w.auditErr != nil {
		return a.w.auditErr
	}
	a.w.audit = append(a.w.audit, e)
	a.w.calls = append(a.w.calls, recorded{Name: "audit:" + e.Result})
	return nil
}

func (a auditor) Verify(context.Context, domain.TenantID) (domain.AuditVerification, error) {
	return domain.AuditVerification{}, nil
}

func (a auditor) Export(context.Context, domain.TenantID) (string, error) { return "", nil }

// ---- Services ----

type fakeWorkflows struct {
	w     *world
	specs map[string]api.WorkflowDetail
}

func (f *fakeWorkflows) List(_ context.Context, s service.Scope) ([]api.WorkflowSummary, error) {
	return nil, f.w.called("workflows.List", s, nil)
}

func (f *fakeWorkflows) Get(_ context.Context, s service.Scope, workflowID, version string) (api.WorkflowDetail, error) {
	if err := f.w.called("workflows.Get", s, workflowID+"@"+version); err != nil {
		return api.WorkflowDetail{}, err
	}
	if d, ok := f.specs[workflowID]; ok {
		return d, nil
	}
	return api.WorkflowDetail{}, nil
}

func (f *fakeWorkflows) Validate(_ context.Context, s service.Scope, req api.ValidateWorkflowRequest) (api.ValidationResult, error) {
	return api.ValidationResult{Valid: true}, f.w.called("workflows.Validate", s, req)
}

func (f *fakeWorkflows) Run(_ context.Context, s service.Scope, workflowID string, req api.RunWorkflowRequest) (api.RunWorkflowResponse, error) {
	if err := f.w.called("workflows.Run:"+workflowID, s, req); err != nil {
		return api.RunWorkflowResponse{}, err
	}
	if req.DryRun {
		return api.RunWorkflowResponse{Workflow: workflowID, Validation: &api.ValidationResult{Valid: true}}, nil
	}
	return api.RunWorkflowResponse{JobID: "JOB-001", Status: domain.JobQueued, Workflow: workflowID}, nil
}

func (f *fakeWorkflows) RunNode(_ context.Context, s service.Scope, nodeType string, req api.RunNodeRequest) (api.RunWorkflowResponse, error) {
	if err := f.w.called("workflows.RunNode:"+nodeType, s, req); err != nil {
		return api.RunWorkflowResponse{}, err
	}
	return api.RunWorkflowResponse{JobID: "JOB-002", Status: domain.JobQueued}, nil
}

func (f *fakeWorkflows) CreateClip(_ context.Context, s service.Scope, req api.CreateClipRequest) (api.RunWorkflowResponse, error) {
	if err := f.w.called("workflows.CreateClip", s, req); err != nil {
		return api.RunWorkflowResponse{}, err
	}
	return api.RunWorkflowResponse{JobID: "JOB-003", Status: domain.JobQueued, Workflow: "create-clip"}, nil
}

type fakeJobs struct {
	w      *world
	stream func(ctx context.Context, afterSeq int64) (<-chan domain.JobEvent, error)
}

func (f *fakeJobs) List(_ context.Context, s service.Scope, q api.JobQuery) (api.List[domain.Job], error) {
	return api.List[domain.Job]{}, f.w.called("jobs.List", s, q)
}

func (f *fakeJobs) Get(_ context.Context, s service.Scope, ref id.Ref) (api.JobDetail, error) {
	return api.JobDetail{}, f.w.called("jobs.Get", s, ref)
}

func (f *fakeJobs) Cancel(_ context.Context, s service.Scope, ref id.Ref, r api.JobActionRequest) (domain.Job, error) {
	return domain.Job{}, f.w.called("jobs.Cancel", s, r)
}

func (f *fakeJobs) Retry(_ context.Context, s service.Scope, ref id.Ref, r api.JobActionRequest) (domain.Job, error) {
	return domain.Job{}, f.w.called("jobs.Retry", s, r)
}

func (f *fakeJobs) Pause(_ context.Context, s service.Scope, ref id.Ref, r api.JobActionRequest) (domain.Job, error) {
	return domain.Job{}, f.w.called("jobs.Pause", s, r)
}

func (f *fakeJobs) Resume(_ context.Context, s service.Scope, ref id.Ref, r api.JobActionRequest) (domain.Job, error) {
	return domain.Job{}, f.w.called("jobs.Resume", s, r)
}

func (f *fakeJobs) Resolve(_ context.Context, s service.Scope, ref id.Ref, r api.ResolveJobRequest) (domain.Job, error) {
	return domain.Job{}, f.w.called("jobs.Resolve", s, r)
}

func (f *fakeJobs) Logs(_ context.Context, s service.Scope, ref id.Ref, q api.PageQuery) (api.List[domain.JobEvent], error) {
	return api.List[domain.JobEvent]{}, f.w.called("jobs.Logs", s, q)
}

func (f *fakeJobs) Subscribe(ctx context.Context, s service.Scope, ref id.Ref, afterSeq int64) (<-chan domain.JobEvent, error) {
	if err := f.w.called("jobs.Subscribe", s, afterSeq); err != nil {
		return nil, err
	}
	if f.stream != nil {
		return f.stream(ctx, afterSeq)
	}
	ch := make(chan domain.JobEvent)
	close(ch)
	return ch, nil
}

type fakeApprovals struct{ w *world }

func (f fakeApprovals) List(_ context.Context, s service.Scope, q api.ApprovalQuery) (api.List[domain.Approval], error) {
	return api.List[domain.Approval]{}, f.w.called("approvals.List", s, q)
}

func (f fakeApprovals) Get(_ context.Context, s service.Scope, ref id.Ref) (domain.Approval, error) {
	return domain.Approval{}, f.w.called("approvals.Get", s, ref)
}

func (f fakeApprovals) Approve(_ context.Context, s service.Scope, ref id.Ref, r api.ApproveRequest) (domain.Approval, error) {
	return domain.Approval{}, f.w.called("approvals.Approve", s, r)
}

func (f fakeApprovals) Reject(_ context.Context, s service.Scope, ref id.Ref, r api.RejectRequest) (domain.Approval, error) {
	return domain.Approval{}, f.w.called("approvals.Reject", s, r)
}

type fakeEvents struct {
	w      *world
	stream func(ctx context.Context, afterSeq int64) (<-chan service.StreamedEvent, error)
}

func (f *fakeEvents) List(_ context.Context, s service.Scope, q api.EventQuery) (api.List[event.Envelope], error) {
	return api.List[event.Envelope]{}, f.w.called("events.List", s, q)
}

func (f *fakeEvents) Deliveries(_ context.Context, s service.Scope, q api.DeliveryQuery) (api.List[event.Delivery], error) {
	return api.List[event.Delivery]{}, f.w.called("events.Deliveries", s, q)
}

func (f *fakeEvents) Subscribe(ctx context.Context, s service.Scope, q api.EventQuery, afterSeq int64) (<-chan service.StreamedEvent, error) {
	if err := f.w.called("events.Subscribe", s, afterSeq); err != nil {
		return nil, err
	}
	if f.stream != nil {
		return f.stream(ctx, afterSeq)
	}
	ch := make(chan service.StreamedEvent)
	close(ch)
	return ch, nil
}

type fakeIntrospection struct{ w *world }

func (f fakeIntrospection) Nodes(_ context.Context, s service.Scope) ([]node.Descriptor, error) {
	return nil, f.w.called("introspection.Nodes", s, nil)
}

func (f fakeIntrospection) Node(_ context.Context, s service.Scope, t string) (node.Descriptor, error) {
	return node.Descriptor{}, f.w.called("introspection.Node", s, t)
}

func (f fakeIntrospection) Hooks(_ context.Context, s service.Scope, wf string) ([]api.HookInfo, error) {
	return nil, f.w.called("introspection.Hooks", s, wf)
}

func (f fakeIntrospection) TestHook(_ context.Context, s service.Scope, h string, r api.TestHookRequest) (map[string]any, error) {
	return map[string]any{}, f.w.called("introspection.TestHook", s, r)
}

type fakeAssets struct {
	w        *world
	asset    domain.Asset
	versions []domain.AssetVersion
}

func (f *fakeAssets) List(_ context.Context, s service.Scope, q api.AssetQuery) (api.List[domain.Asset], error) {
	return api.List[domain.Asset]{}, f.w.called("assets.List", s, q)
}

func (f *fakeAssets) Get(_ context.Context, s service.Scope, ref id.Ref) (domain.Asset, error) {
	return f.asset, f.w.called("assets.Get", s, ref)
}

func (f *fakeAssets) Update(_ context.Context, s service.Scope, ref id.Ref, r api.UpdateAssetRequest) (domain.Asset, error) {
	return domain.Asset{}, f.w.called("assets.Update", s, r)
}

func (f *fakeAssets) Lineage(_ context.Context, s service.Scope, ref id.Ref) (api.LineageResponse, error) {
	return api.LineageResponse{}, f.w.called("assets.Lineage", s, ref)
}

func (f *fakeAssets) Versions(_ context.Context, s service.Scope, ref id.Ref) ([]domain.AssetVersion, error) {
	return f.versions, f.w.called("assets.Versions", s, ref)
}

func (f *fakeAssets) SetAIProcessing(_ context.Context, s service.Scope, ref id.Ref, r api.SetAIProcessingRequest) (domain.Asset, error) {
	return domain.Asset{}, f.w.called("assets.SetAIProcessing", s, r)
}

func (f *fakeAssets) Consent(_ context.Context, s service.Scope, ref id.Ref) (api.ConsentResponse, error) {
	return api.ConsentResponse{}, f.w.called("assets.Consent", s, ref)
}

func (f *fakeAssets) Transcript(_ context.Context, s service.Scope, ref id.Ref, v string) (domain.Transcript, error) {
	return domain.Transcript{}, f.w.called("assets.Transcript", s, v)
}

func (f *fakeAssets) ListSensitive(_ context.Context, s service.Scope, ref id.Ref, v string) ([]domain.SensitiveSegment, error) {
	return nil, f.w.called("assets.ListSensitive", s, v)
}

func (f *fakeAssets) AddSensitive(_ context.Context, s service.Scope, ref id.Ref, r api.SensitiveSegmentRequest) error {
	return f.w.called("assets.AddSensitive", s, r)
}

func (f *fakeAssets) RemoveSensitive(_ context.Context, s service.Scope, ref id.Ref, r api.SensitiveSegmentRequest) error {
	return f.w.called("assets.RemoveSensitive", s, r)
}

func (f *fakeAssets) Collections(_ context.Context, s service.Scope) ([]domain.Collection, error) {
	return nil, f.w.called("assets.Collections", s, nil)
}

func (f *fakeAssets) SetCollectionAIProcessing(_ context.Context, s service.Scope, name string, r api.SetAIProcessingRequest) (domain.Collection, error) {
	return domain.Collection{}, f.w.called("assets.SetCollectionAIProcessing", s, name)
}

type fakeSearch struct{ w *world }

func (f fakeSearch) Search(_ context.Context, s service.Scope, q api.SearchQuery) (api.SearchResponse, error) {
	return api.SearchResponse{Mode: "hybrid"}, f.w.called("search.Search", s, q)
}

type fakePublications struct{ w *world }

func (f fakePublications) Request(_ context.Context, s service.Scope, r api.CreatePublicationRequest) (api.RunWorkflowResponse, error) {
	return api.RunWorkflowResponse{JobID: "JOB-004"}, f.w.called("publications.Request", s, r)
}

func (f fakePublications) List(_ context.Context, s service.Scope, q api.PublicationQuery) (api.List[domain.Publication], error) {
	return api.List[domain.Publication]{}, f.w.called("publications.List", s, q)
}

func (f fakePublications) Get(_ context.Context, s service.Scope, ref id.Ref) (domain.Publication, error) {
	return domain.Publication{}, f.w.called("publications.Get", s, ref)
}

func (f fakePublications) Withdraw(_ context.Context, s service.Scope, r api.WithdrawRequest) (api.RunWorkflowResponse, error) {
	return api.RunWorkflowResponse{JobID: "JOB-005"}, f.w.called("publications.Withdraw", s, r)
}

func (f fakePublications) WithdrawOne(_ context.Context, s service.Scope, ref id.Ref) (api.RunWorkflowResponse, error) {
	return api.RunWorkflowResponse{JobID: "JOB-006"}, f.w.called("publications.WithdrawOne", s, ref)
}

func (f fakePublications) Sync(_ context.Context, s service.Scope, ref id.Ref) (domain.Publication, error) {
	return domain.Publication{}, f.w.called("publications.Sync", s, ref)
}

func (f fakePublications) Destinations(_ context.Context, s service.Scope) ([]api.DestinationInfo, error) {
	return nil, f.w.called("publications.Destinations", s, nil)
}

type fakeStorages struct{ w *world }

func (f fakeStorages) List(_ context.Context, s service.Scope) ([]api.StorageInfo, error) {
	return nil, f.w.called("storages.List", s, nil)
}

func (f fakeStorages) Test(_ context.Context, s service.Scope, role domain.StorageRole) (api.StorageTestResponse, error) {
	return api.StorageTestResponse{OK: true}, f.w.called("storages.Test", s, role)
}

func (f fakeStorages) Verify(_ context.Context, s service.Scope, role domain.StorageRole, r api.StorageVerifyRequest) (api.StorageVerifyResponse, error) {
	return api.StorageVerifyResponse{OK: true}, f.w.called("storages.Verify", s, r)
}

func (f fakeStorages) ProxyLink(_ context.Context, s service.Scope, ref id.Ref) (api.ProxyLinkResponse, error) {
	return api.ProxyLinkResponse{}, f.w.called("storages.ProxyLink", s, ref)
}

type fakeSecrets struct{ w *world }

func (f fakeSecrets) List(_ context.Context, s service.Scope) ([]domain.SecretMeta, error) {
	return nil, f.w.called("secrets.List", s, nil)
}

func (f fakeSecrets) Put(_ context.Context, s service.Scope, r api.PutSecretRequest) (domain.SecretMeta, error) {
	return domain.SecretMeta{Name: r.Name, Location: r.Location}, f.w.called("secrets.Put", s, r.Name)
}

func (f fakeSecrets) Delete(_ context.Context, s service.Scope, name string) error {
	return f.w.called("secrets.Delete", s, name)
}

func (f fakeSecrets) Test(_ context.Context, s service.Scope, name string) (api.SecretTestResponse, error) {
	return api.SecretTestResponse{OK: true}, f.w.called("secrets.Test", s, name)
}

func (f fakeSecrets) Rotate(_ context.Context, s service.Scope, name string, r api.RotateSecretRequest) (domain.SecretMeta, error) {
	return domain.SecretMeta{Name: name}, f.w.called("secrets.Rotate", s, name)
}

type fakeDevices struct{ w *world }

func (f fakeDevices) CreatePairingCode(_ context.Context, s service.Scope, r api.CreatePairingCodeRequest) (api.PairingCodeResponse, error) {
	return api.PairingCodeResponse{Code: "ABCDE-FGHIJ"}, f.w.called("devices.CreatePairingCode", s, r)
}

func (f fakeDevices) Pair(_ context.Context, r api.PairDeviceRequest) (api.DeviceCredentialResponse, error) {
	return api.DeviceCredentialResponse{Token: "vdm_issued_once"}, f.w.called("devices.Pair", service.Scope{}, r.DeviceName)
}

func (f fakeDevices) List(_ context.Context, s service.Scope) ([]domain.Device, error) {
	return nil, f.w.called("devices.List", s, nil)
}

func (f fakeDevices) Revoke(_ context.Context, s service.Scope, ref id.Ref) (domain.Device, error) {
	return domain.Device{}, f.w.called("devices.Revoke", s, ref)
}

func (f fakeDevices) Rotate(_ context.Context, s service.Scope, ref id.Ref) (api.DeviceCredentialResponse, error) {
	return api.DeviceCredentialResponse{}, f.w.called("devices.Rotate", s, ref)
}

type fakeTenants struct{ w *world }

func (f fakeTenants) List(_ context.Context, p domain.Principal) ([]api.TenantInfo, error) {
	return nil, f.w.called("tenants.List", service.Scope{Principal: p}, nil)
}

func (f fakeTenants) Create(_ context.Context, p domain.Principal, r api.CreateTenantRequest) (domain.Tenant, error) {
	return domain.Tenant{ID: r.ID}, f.w.called("tenants.Create", service.Scope{Principal: p}, r)
}

type fakeUsers struct{ w *world }

func (f fakeUsers) List(_ context.Context, s service.Scope) ([]domain.Membership, error) {
	return nil, f.w.called("users.List", s, nil)
}

func (f fakeUsers) Add(_ context.Context, s service.Scope, r api.AddUserRequest) (domain.Membership, error) {
	return domain.Membership{}, f.w.called("users.Add", s, r)
}

func (f fakeUsers) Grant(_ context.Context, s service.Scope, user string, r api.GrantRequest) (domain.Membership, error) {
	return domain.Membership{}, f.w.called("users.Grant", s, user)
}

type fakeBudgets struct{ w *world }

func (f fakeBudgets) Status(_ context.Context, s service.Scope) (domain.BudgetStatus, error) {
	return domain.BudgetStatus{Budget: domain.Budget{Currency: "USD"}}, f.w.called("budgets.Status", s, nil)
}

func (f fakeBudgets) Set(_ context.Context, s service.Scope, r api.SetBudgetRequest) (domain.BudgetStatus, error) {
	return domain.BudgetStatus{}, f.w.called("budgets.Set", s, r)
}

func (f fakeBudgets) Override(_ context.Context, s service.Scope, r api.BudgetOverrideRequest) (domain.BudgetStatus, error) {
	return domain.BudgetStatus{}, f.w.called("budgets.Override", s, r)
}

func (f fakeBudgets) Check(context.Context, domain.TenantID) error { return nil }

func (f fakeBudgets) Record(context.Context, domain.TenantID, []domain.UsageRecord) error { return nil }

type fakeAudit struct{ w *world }

func (f fakeAudit) List(_ context.Context, s service.Scope, q api.AuditQuery) (api.List[domain.AuditRecord], error) {
	return api.List[domain.AuditRecord]{}, f.w.called("audit.List", s, q)
}

func (f fakeAudit) Verify(_ context.Context, s service.Scope) (domain.AuditVerification, error) {
	return domain.AuditVerification{OK: true}, f.w.called("audit.Verify", s, nil)
}

func (f fakeAudit) Export(_ context.Context, s service.Scope) (api.AuditExportResponse, error) {
	return api.AuditExportResponse{}, f.w.called("audit.Export", s, nil)
}

type fakePlugins struct{ w *world }

func (f fakePlugins) List(_ context.Context, s service.Scope) ([]domain.PluginRecord, error) {
	return nil, f.w.called("plugins.List", s, nil)
}

func (f fakePlugins) Get(_ context.Context, s service.Scope, p string) (domain.PluginRecord, error) {
	return domain.PluginRecord{}, f.w.called("plugins.Get", s, p)
}

func (f fakePlugins) Install(_ context.Context, s service.Scope, r api.InstallPluginRequest) (domain.PluginRecord, error) {
	return domain.PluginRecord{}, f.w.called("plugins.Install", s, r)
}

type fakeObservability struct {
	w       *world
	metrics http.Handler
	stream  func(ctx context.Context) (<-chan connector.LogRecord, error)
}

func (f *fakeObservability) Logs(_ context.Context, s service.Scope, q api.LogQuery) (api.List[connector.LogRecord], error) {
	return api.List[connector.LogRecord]{}, f.w.called("observability.Logs", s, q)
}

func (f *fakeObservability) SubscribeLogs(ctx context.Context, s service.Scope, q api.LogQuery) (<-chan connector.LogRecord, error) {
	if err := f.w.called("observability.SubscribeLogs", s, q); err != nil {
		return nil, err
	}
	if f.stream != nil {
		return f.stream(ctx)
	}
	ch := make(chan connector.LogRecord)
	close(ch)
	return ch, nil
}

func (f *fakeObservability) Sinks(_ context.Context, s service.Scope) ([]api.ObservabilitySink, error) {
	return nil, f.w.called("observability.Sinks", s, nil)
}

func (f *fakeObservability) TestSink(_ context.Context, s service.Scope, sink string) error {
	return f.w.called("observability.TestSink", s, sink)
}

func (f *fakeObservability) SetSinkEnabled(_ context.Context, s service.Scope, sink string, on bool) error {
	return f.w.called("observability.SetSinkEnabled", s, on)
}

func (f *fakeObservability) MetricsHandler() http.Handler {
	if f.metrics != nil {
		return f.metrics
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = f.w.called("observability.Metrics", service.Scope{}, nil)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(metricsSample))
	})
}

const metricsSample = `# HELP vidyom_jobs Jobs by state.
# TYPE vidyom_jobs gauge
vidyom_jobs{state="running",tenant_id="acme"} 2
vidyom_jobs{state="running",tenant_id="globex"} 7
vidyom_jobs{other_tenant_id="acme",state="x"} 9
vidyom_queue_depth{pool="media"} 3
`

type fakeConnectors struct{ w *world }

func (f fakeConnectors) List() []connector.Info {
	_ = f.w.called("connectors.List", service.Scope{}, nil)
	return nil
}

type fakeDoctor struct{ w *world }

func (f *fakeDoctor) Run(_ context.Context, s service.Scope, check string) (api.DoctorResponse, error) {
	return api.DoctorResponse{}, f.w.called("doctor.Run", s, check)
}

type fakeCoordinator struct{ w *world }

func (f fakeCoordinator) Register(_ context.Context, r workerproto.RegisterRequest) (workerproto.RegisterResponse, error) {
	return workerproto.RegisterResponse{ProtocolVersion: workerproto.ProtocolVersion}, f.w.called("worker.Register", service.Scope{}, r)
}

func (f fakeCoordinator) Heartbeat(_ context.Context, r workerproto.HeartbeatRequest) error {
	return f.w.called("worker.Heartbeat", service.Scope{}, r)
}

func (f fakeCoordinator) Lease(_ context.Context, task string, r workerproto.LeaseRequest) (workerproto.Lease, error) {
	return workerproto.Lease{TaskID: task}, f.w.called("worker.Lease", service.Scope{}, task)
}

func (f fakeCoordinator) TaskHeartbeat(_ context.Context, task string, r workerproto.TaskHeartbeatRequest) (workerproto.TaskHeartbeatResponse, error) {
	return workerproto.TaskHeartbeatResponse{Cancel: true}, f.w.called("worker.TaskHeartbeat", service.Scope{}, task)
}

func (f fakeCoordinator) TaskLogs(_ context.Context, task string, r workerproto.TaskLogsRequest) error {
	return f.w.called("worker.TaskLogs", service.Scope{}, task)
}

func (f fakeCoordinator) Complete(_ context.Context, task string, r workerproto.CompleteRequest) error {
	return f.w.called("worker.Complete", service.Scope{}, task)
}

func (f fakeCoordinator) Fail(_ context.Context, task string, r workerproto.FailRequest) error {
	return f.w.called("worker.Fail", service.Scope{}, task)
}

func (f fakeCoordinator) Connection(_ context.Context, task string, r workerproto.ConnectionRequest) (workerproto.ConnectionResponse, error) {
	return workerproto.ConnectionResponse{Credentials: []byte("fictional-credential")}, f.w.called("worker.Connection", service.Scope{}, task)
}

func (f fakeCoordinator) CacheKey(_ context.Context, task string, r workerproto.CacheKeyRequest) (workerproto.CacheKeyResponse, error) {
	return workerproto.CacheKeyResponse{Key: []byte("fictional-key")}, f.w.called("worker.CacheKey", service.Scope{}, task)
}

type fakeTenantConfig struct{}

func (fakeTenantConfig) TenantConfig(context.Context, domain.TenantID) (config.Tenant, error) {
	return config.Tenant{Budget: config.BudgetCosts{PerUnitMinor: map[string]float64{archiveRetrievalOperation: 2}}}, nil
}
