package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/apiclient/apitest"
	"github.com/rafaelp/vidyom/internal/cli"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// TestMain lets the test binary stand in for the vidyom binary, so a real process can
// be killed mid-transfer. It never uses the real OS keyring.
func TestMain(m *testing.M) {
	if os.Getenv("VIDYOM_CLI_FAKE_SSH") == "1" {
		fakeSSHForward()
	}
	if os.Getenv("VIDYOM_CLI_HELPER") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		os.Exit(cli.Run(ctx, os.Args[1:], cli.Options{Keyring: newMemoryKeyring()}))
	}
	os.Exit(m.Run())
}

// fakeSSHForward stands in for `ssh -N -L local:remote`.
func fakeSSHForward() {
	cmdArgs := os.Args[1:]
	var forward string
	for i, arg := range cmdArgs {
		if arg == "-L" && i+1 < len(cmdArgs) {
			forward = cmdArgs[i+1]
		}
	}
	local, remote, ok := strings.Cut(forward, ":")
	if !ok {
		os.Exit(2)
	}
	listener, err := net.Listen("unix", local)
	if err != nil {
		os.Exit(3)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func() {
			defer conn.Close()
			upstream, err := net.Dial("unix", remote)
			if err != nil {
				return
			}
			defer upstream.Close()
			go func() { _, _ = io.Copy(upstream, conn) }()
			_, _ = io.Copy(conn, upstream)
		}()
	}
}

// memoryKeyring is the fake keyring: tests never touch the real one.
type memoryKeyring struct {
	mu      sync.Mutex
	entries map[string]string
	// Failure injection.
	getErr, setErr, deleteErr error
	failSetAfter              int // fail Set after this many successful calls; 0 is off
	sets                      int
}

func newMemoryKeyring() *memoryKeyring { return &memoryKeyring{entries: map[string]string{}} }

func (k *memoryKeyring) Get(service, account string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.getErr != nil {
		return "", k.getErr
	}
	secret, ok := k.entries[service+"/"+account]
	if !ok {
		return "", security.ErrKeyringNotFound
	}
	return secret, nil
}

func (k *memoryKeyring) Set(service, account, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.setErr != nil {
		return k.setErr
	}
	k.sets++
	if k.failSetAfter > 0 && k.sets > k.failSetAfter {
		return security.ErrKeyringUnavailable
	}
	k.entries[service+"/"+account] = secret
	return nil
}

func (k *memoryKeyring) Delete(service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.deleteErr != nil {
		return k.deleteErr
	}
	if _, ok := k.entries[service+"/"+account]; !ok {
		return security.ErrKeyringNotFound
	}
	delete(k.entries, service+"/"+account)
	return nil
}

func (k *memoryKeyring) dump() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	raw, _ := json.Marshal(k.entries)
	return string(raw)
}

// harness runs the CLI in process against the fake daemon.
type harness struct {
	t        *testing.T
	srv      *apitest.Server
	socket   string
	dir      string
	keyring  *memoryKeyring
	env      map[string]string
	stdin    string
	terminal bool
	password func() ([]byte, error)
	now      time.Time

	stdout, stderr bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv := apitest.New(t)
	h := &harness{
		t: t, srv: srv, socket: srv.ServeUnix(), dir: t.TempDir(), keyring: newMemoryKeyring(),
		env: map[string]string{}, now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
	}
	h.env["VIDYOM_SOCKET"] = h.socket
	return h
}

func (h *harness) options(ctx context.Context) cli.Options {
	return cli.Options{
		Stdin: strings.NewReader(h.stdin), Stdout: &h.stdout, Stderr: &h.stderr,
		Keyring: h.keyring, ConfigDir: h.dir,
		Getenv:           func(k string) string { return h.env[k] },
		Now:              func() time.Time { return h.now },
		StdinIsTerminal:  func() bool { return h.terminal },
		StderrIsTerminal: func() bool { return false },
		ReadPassword: func(context.Context) ([]byte, error) {
			if h.password == nil {
				return nil, errors.New("no terminal in this test")
			}
			return h.password()
		},
		Sleep:       func(context.Context, time.Duration) error { return nil },
		SSHBinary:   os.Args[0],
		Hostname:    func() (string, error) { return "workstation", nil },
		UserHomeDir: func() (string, error) { return h.dir, nil },
	}
}

// run executes a command with a fresh output and returns its exit code.
func (h *harness) run(args ...string) int {
	h.t.Helper()
	return h.runContext(context.Background(), args...)
}

func (h *harness) runContext(ctx context.Context, args ...string) int {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	return cli.Run(ctx, args, h.options(ctx))
}

// mustRun fails the test unless the command succeeds.
func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	if code := h.run(args...); code != 0 {
		h.t.Fatalf("vidyom %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, h.stdout.String(), h.stderr.String())
	}
	return h.stdout.String()
}

// useTenant configures the harness's socket as the current server with a tenant.
func (h *harness) useTenant(tenant string) {
	h.t.Helper()
	h.mustRun("server", "add", "test", "--socket", h.socket)
	h.srv.JSON(api.RouteTenantsList, 200, api.List[api.TenantInfo]{Items: []api.TenantInfo{{Tenant: domain.Tenant{ID: domain.TenantID(tenant)}, Role: domain.RoleAdmin}}})
	h.mustRun("tenant", "use", tenant)
	h.srv.Reset()
}

func (h *harness) config() string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.dir, "config.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

// assertNoLeak fails when secret appears anywhere a person or a file could see it.
func (h *harness) assertNoLeak(secret string) {
	h.t.Helper()
	for name, content := range map[string]string{"stdout": h.stdout.String(), "stderr": h.stderr.String()} {
		if strings.Contains(content, secret) {
			h.t.Errorf("the secret reached %s: %s", name, content)
		}
	}
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(h.dir, entry.Name())
		if entry.IsDir() {
			continue
		}
		if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), secret) {
			h.t.Errorf("the secret reached %s", path)
		}
	}
}

// ---- Sample answers for every user route ----

var sampleTime = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func sampleRun() api.RunWorkflowResponse {
	return api.RunWorkflowResponse{JobID: "JOB-001", JobULID: "01JC0000000000000000000000", Status: domain.JobQueued, Workflow: "create-clip", WorkflowVersion: "1.0.0"}
}

func sampleAsset() domain.Asset {
	return domain.Asset{
		ID: "ORI-001", ULID: "01JC0000000000000000000001", TenantID: "acme", Kind: domain.KindOriginal,
		Title: "Hatha Yoga Class", Status: domain.AssetReadyForClipping, Visibility: domain.VisibilityPrivate,
		AIProcessing: domain.AILocalOnly, CurrentVersion: "v1", MasterAvailable: true, BackupVerified: true,
		WorkingMaster: &domain.MediaFile{Storage: domain.RoleMaster, Key: "originals/ORI-001/master.mp4", SizeBytes: 2048, Verified: true},
		Lineage:       &domain.Lineage{Method: domain.LineageComputed, SourceAssetID: "ORI-001", RootAssetID: "ORI-001"},
	}
}

func sampleApproval(kind domain.ApprovalKind) domain.Approval {
	return domain.Approval{
		ID: "APR-001", ULID: "01JC0000000000000000000002", TenantID: "acme", Kind: kind, Status: domain.ApprovalPending,
		AssetID: "ORI-001", AssetVersion: "v1", JobID: "JOB-001", NodeID: "gate", AllowAdjust: true,
		Evidence: json.RawMessage(`{"excerpt":"the story","confidence":0.9}`), Preview: "https://media.example.internal/preview/1",
		CreatedAt: sampleTime,
	}
}

func samplePublication() domain.Publication {
	return domain.Publication{
		ID: "PUB-001", ULID: "01JC0000000000000000000003", TenantID: "acme", AssetID: "CLIP-001", Destination: "mock-main",
		AssetVersion: "v1", RemoteID: "r1", URL: "https://media.example.internal/watch/r1",
		Status: domain.PubPublished, Visibility: domain.VisibilityPublic,
	}
}

func sampleJob() domain.Job {
	return domain.Job{ID: "JOB-001", ULID: "01JC0000000000000000000000", TenantID: "acme", WorkflowID: "create-clip", WorkflowVersion: "1.0.0", Status: domain.JobRunning, CreatedAt: sampleTime}
}

func sampleBudget() domain.BudgetStatus {
	return domain.BudgetStatus{Budget: domain.Budget{TenantID: "acme", MonthlyMinor: 50000, Currency: "USD"}, Period: "2026-11", SpentMinor: 1200, Percent: 2.4}
}

// sampleAnswers are valid answers per route, so every command has something to show.
func sampleAnswers() map[string]any {
	device := domain.Device{ID: "CLIENT-001", ULID: "01JC0000000000000000000004", Email: "admin@example.com", Name: "laptop", Status: domain.DeviceActive, CreatedAt: sampleTime}
	return map[string]any{
		api.RouteHealth:  api.HealthResponse{Status: "ok", Version: "1.2.3", Sealing: "file", Components: map[string]api.ComponentHealth{"redis": {Status: "ok"}}},
		api.RouteVersion: api.VersionResponse{Version: "1.2.3", APIVersion: "v1"},
		api.RouteDoctor:  api.DoctorResponse{Checks: []api.DoctorCheck{{Name: "sealing", Status: "warn", Detail: "file"}}},

		api.RouteWorkflowsList: api.List[api.WorkflowSummary]{Items: []api.WorkflowSummary{{ID: "create-clip", Version: "1.0.0", SHA256: "abc"}}},
		api.RouteWorkflowsGet: api.WorkflowDetail{WorkflowSummary: api.WorkflowSummary{
			ID: "ingest-original", Version: "1.0.0",
			Inputs: map[string]wfdef.InputSpec{
				"file": {Type: wfdef.InputFile, Required: true}, "title": {Type: wfdef.InputString, Required: true},
				"category": {Type: wfdef.InputString}, "count": {Type: wfdef.InputNumber}, "flag": {Type: wfdef.InputBool},
				"list": {Type: wfdef.InputArray}, "object": {Type: wfdef.InputObject},
			},
		}, Definition: json.RawMessage(`{"id":"ingest-original"}`)},
		api.RouteWorkflowsValidate: api.ValidationResult{Valid: true},
		api.RouteWorkflowsRun:      sampleRun(),
		api.RouteWorkflowsRunBody:  sampleRun(),

		api.RouteJobsList:    api.List[domain.Job]{Items: []domain.Job{sampleJob()}},
		api.RouteJobsGet:     api.JobDetail{Job: sampleJob(), Nodes: []domain.NodeExecution{{NodeID: "cut", NodeType: "video.cut", Status: domain.NodeRunning, Attempt: 1}}},
		api.RouteJobsLogs:    api.List[domain.JobEvent]{Items: []domain.JobEvent{{Seq: 1, JobID: "JOB-001", At: sampleTime, Kind: domain.JobEventLog, Level: "info", Message: "started"}}},
		api.RouteJobsCancel:  sampleJob(),
		api.RouteJobsRetry:   sampleJob(),
		api.RouteJobsPause:   sampleJob(),
		api.RouteJobsResume:  sampleJob(),
		api.RouteJobsResolve: sampleJob(),

		api.RouteAssetsList:         api.List[domain.Asset]{Items: []domain.Asset{sampleAsset()}},
		api.RouteAssetsGet:          sampleAsset(),
		api.RouteAssetsUpdate:       sampleAsset(),
		api.RouteAssetsDelete:       sampleRun(),
		api.RouteAssetsLineage:      api.LineageResponse{AssetID: "CLIP-001", Lineage: &domain.Lineage{Method: domain.LineageComputed, SourceAssetID: "ORI-001", RootAssetID: "ORI-001", Segments: []domain.Segment{{SourceAssetID: "ORI-001", SourceVersion: "v1", StartSeconds: 648, EndSeconds: 1080}}}},
		api.RouteAssetsVersions:     api.List[domain.AssetVersion]{Items: []domain.AssetVersion{{AssetID: "CLIP-001", Version: "v1"}}},
		api.RouteAssetsAIProcessing: sampleAsset(),
		api.RouteAssetsConsent:      api.ConsentResponse{AIProcessing: domain.AIExternalAllowed, Consent: &domain.AIConsent{AuthorizedBy: "admin@example.com", AuthorizedAt: sampleTime, Scope: "asset"}},
		api.RouteAssetsTranscript:   domain.Transcript{AssetID: "ORI-001", Segments: []domain.TranscriptSegment{{Start: 1, End: 2, Text: "breathing"}}},
		api.RouteAssetsProxyLink:    api.ProxyLinkResponse{URL: "https://media.example.internal/proxy/1", ExpiresAt: sampleTime},
		api.RouteAssetsExport:       sampleRun(),
		api.RouteSensitiveList:      api.List[domain.SensitiveSegment]{Items: []domain.SensitiveSegment{{AssetID: "ORI-001", Version: "v1", StartSeconds: 10, EndSeconds: 20}}},
		api.RouteSensitiveAdd:       struct{}{},
		api.RouteSensitiveRemove:    struct{}{},

		api.RouteCollectionsList:         api.List[domain.Collection]{Items: []domain.Collection{{Name: "yoga", AIProcessing: domain.AILocalOnly}}},
		api.RouteCollectionsAIProcessing: domain.Collection{Name: "yoga", AIProcessing: domain.AIExternalAllowed},

		api.RouteClipsCreate: sampleRun(),
		api.RouteSearch:      api.SearchResponse{Mode: "hybrid", Items: []domain.SearchHit{{AssetID: "ORI-001", Version: "v1", StartSeconds: 648, EndSeconds: 700, Text: "conscious breathing", Score: 0.9, Source: "hybrid"}}},

		api.RouteApprovalsList:    api.List[domain.Approval]{Items: []domain.Approval{sampleApproval(domain.ApprovalClipSelection)}},
		api.RouteApprovalsGet:     sampleApproval(domain.ApprovalClipSelection),
		api.RouteApprovalsApprove: sampleApproval(domain.ApprovalClipSelection),
		api.RouteApprovalsReject:  sampleApproval(domain.ApprovalClipSelection),

		api.RoutePublicationsCreate:   sampleRun(),
		api.RoutePublicationsList:     api.List[domain.Publication]{Items: []domain.Publication{samplePublication()}},
		api.RoutePublicationsGet:      samplePublication(),
		api.RoutePublicationsWithdraw: sampleRun(),
		api.RoutePublicationsUnpub:    sampleRun(),
		api.RoutePublicationsSync:     samplePublication(),
		api.RouteDestinationsList:     api.List[api.DestinationInfo]{Items: []api.DestinationInfo{{Name: "mock-main", Driver: "mock", Available: true}}},

		api.RouteNodesList:   api.List[node.Descriptor]{Items: []node.Descriptor{{Type: "video.cut", TypeVersion: 1, Pool: node.PoolMedia, SideEffect: node.EffectIdempotent}}},
		api.RouteNodesGet:    node.Descriptor{Type: "video.cut", TypeVersion: 1, Pool: node.PoolMedia, SideEffect: node.EffectIdempotent, Parameters: json.RawMessage(`{"type":"object"}`)},
		api.RouteNodesRun:    sampleRun(),
		api.RouteConnectors:  api.List[connector.Info]{Items: []connector.Info{{Family: connector.FamilyStorage, Driver: "local", Version: "1", Available: true}}},
		api.RoutePluginsList: api.List[domain.PluginRecord]{Items: []domain.PluginRecord{{ID: "notifications", Version: "1.0.0", Enabled: true}}},
		api.RoutePluginsGet:  domain.PluginRecord{ID: "notifications", Version: "1.0.0", Manifest: json.RawMessage(`{"id":"notifications"}`)},
		api.RoutePluginsAdd:  domain.PluginRecord{ID: "notifications", Version: "1.0.0"},
		api.RouteHooksList:   api.List[api.HookInfo]{Items: []api.HookInfo{{ID: "validate-source", Point: "before", Level: "workflow", Plugin: "notifications", Action: "check"}}},
		api.RouteHooksTest:   map[string]any{"allow": true},
		api.RouteEventsList:  api.List[event.Envelope]{Items: []event.Envelope{{EventID: "01JC0000000000000000000005", Type: event.ClipCreated, OccurredAt: sampleTime, TenantID: "acme"}}},
		api.RouteEventsDeliv: api.List[event.Delivery]{Items: []event.Delivery{{EventID: "01JC0000000000000000000005", Subscriber: "workflow:update-public-library", Status: event.DeliveryStatus("failed"), Attempts: 2}}},

		api.RouteStoragesList:   api.List[api.StorageInfo]{Items: []api.StorageInfo{{Role: domain.RoleMaster, Driver: "local"}}},
		api.RouteStoragesTest:   api.StorageTestResponse{OK: true},
		api.RouteStoragesVerify: api.StorageVerifyResponse{OK: true, Checked: 2},
		api.RouteStoragesCopy:   sampleRun(),

		api.RouteSecretsList:   api.List[domain.SecretMeta]{Items: []domain.SecretMeta{{Name: "gcs-main", TenantID: "acme", Location: domain.SecretServer, CreatedAt: sampleTime}}},
		api.RouteSecretsPut:    domain.SecretMeta{Name: "gcs-main", TenantID: "acme", Location: domain.SecretServer},
		api.RouteSecretsDelete: struct{}{},
		api.RouteSecretsTest:   api.SecretTestResponse{OK: true},
		api.RouteSecretsRotate: domain.SecretMeta{Name: "gcs-main", TenantID: "acme", Location: domain.SecretServer},

		api.RoutePairingCodes:  api.PairingCodeResponse{Code: "ABCDE-FGHJK", ExpiresAt: sampleTime, TLSFingerprint: strings.Repeat("ab", 32)},
		api.RouteDevicesList:   api.List[domain.Device]{Items: []domain.Device{device}},
		api.RouteDevicesRevoke: domain.Device{ID: "CLIENT-002", Status: domain.DeviceRevoked},
		api.RouteDevicesRotate: api.DeviceCredentialResponse{Device: domain.Device{ID: "CLIENT-002"}, Token: "vdm_rotated_token"},

		api.RouteTenantsList: api.List[api.TenantInfo]{Items: []api.TenantInfo{{Tenant: domain.Tenant{ID: "acme", Name: "Acme"}, Role: domain.RoleAdmin}}},
		api.RouteTenantsAdd:  domain.Tenant{ID: "beta", Name: "Beta"},
		api.RouteUsersList:   api.List[domain.Membership]{Items: []domain.Membership{{Email: "maria@example.com", TenantID: "acme", Role: domain.RoleEditor}}},
		api.RouteUsersAdd:    domain.Membership{Email: "maria@example.com", TenantID: "acme", Role: domain.RoleEditor},
		api.RouteUsersGrant:  domain.Membership{Email: "maria@example.com", TenantID: "acme", Role: domain.RoleEditor},
		api.RouteBudgetGet:   sampleBudget(),
		api.RouteBudgetPut:   sampleBudget(),
		api.RouteBudgetOver:  sampleBudget(),
		api.RouteAuditList:   api.List[domain.AuditRecord]{Items: []domain.AuditRecord{{Seq: 1, At: sampleTime, Actor: "admin@example.com", Action: "jobs.cancel", Result: "allowed"}}},
		api.RouteAuditVerify: domain.AuditVerification{OK: true, Records: 12},
		api.RouteAuditExport: api.AuditExportResponse{Key: "audit/2026-11-09.jsonl"},
		api.RouteLogsList:    api.List[connector.LogRecord]{Items: []connector.LogRecord{{Timestamp: sampleTime, Level: "info", Service: "vidyomd", Event: "job.started", Message: "ok"}}},
		api.RouteObsList:     api.List[api.ObservabilitySink]{Items: []api.ObservabilitySink{{ID: "loki", Kind: "log", Driver: "loki", Enabled: true}}},
		api.RouteObsStatus:   api.List[api.ObservabilitySink]{Items: []api.ObservabilitySink{{ID: "loki", Kind: "log", Driver: "loki", Enabled: true, Status: "ok"}}},
		api.RouteObsTest:     struct{}{},
		api.RouteObsEnable:   struct{}{},
		api.RouteObsDisable:  struct{}{},
	}
}

// installSamples gives every route an answer, including streams and the restore flow.
func (h *harness) installSamples() {
	for route, answer := range sampleAnswers() {
		h.srv.JSON(route, http.StatusOK, answer)
	}
	h.srv.Handle(api.RouteStoragesRestore, func(w http.ResponseWriter, r *http.Request) {
		var req api.StorageRestoreRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Confirm {
			estimate := int64(1250)
			apitest.WriteJSON(w, 200, api.StorageRestorePreview{ConfirmRequired: true, AssetID: "ORI-001", To: req.To, ArchiveBytes: 1 << 30, EstimateMinor: &estimate, Currency: "USD"})
			return
		}
		apitest.WriteJSON(w, 202, sampleRun())
	})
	h.srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteSSEHeaders(w)
		apitest.WriteSSE(w, apitest.SSEFrame{ID: "1", Name: api.SSEJobEvent, Data: domain.JobEvent{Seq: 1, JobID: "JOB-001", At: sampleTime, Kind: domain.JobEventLog, Level: "info", Message: "line one"}})
		apitest.WriteSSE(w, apitest.SSEFrame{ID: "2", Name: api.SSEJobEvent, Data: domain.JobEvent{Seq: 2, JobID: "JOB-001", At: sampleTime, Kind: domain.JobEventState, Message: "running"}})
		apitest.WriteSSE(w, apitest.SSEFrame{Name: api.SSEEnd})
	})
	h.srv.Handle(api.RouteEventsStream, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteSSEHeaders(w)
		apitest.WriteSSE(w, apitest.SSEFrame{ID: "1", Name: api.SSEEvent, Data: event.Envelope{EventID: "01JC0000000000000000000005", Type: event.ClipCreated, OccurredAt: sampleTime, TenantID: "acme"}})
		apitest.WriteSSE(w, apitest.SSEFrame{Name: api.SSEEnd})
	})
	h.srv.Handle(api.RouteLogsStream, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteSSEHeaders(w)
		apitest.WriteSSE(w, apitest.SSEFrame{Name: api.SSELog, Data: connector.LogRecord{Timestamp: sampleTime, Level: "info", Service: "vidyomd", Event: "job.started"}})
		apitest.WriteSSE(w, apitest.SSEFrame{Name: api.SSEEnd})
	})
}

func fileWith(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertContains(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			t.Errorf("output lacks %q:\n%s", needle, haystack)
		}
	}
}

func decodeJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	return out
}

func describe(call apitest.Call) string {
	return fmt.Sprintf("%s %s?%s %s", call.Method, call.Path, call.Query.Encode(), call.Body)
}
