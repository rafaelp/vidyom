package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// Binaries built once for the whole package: real child processes.
var (
	sampleBinary string
	probeBinary  string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vidyom-plugin-bins-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	build := func(pkg, name string) string {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s\n", pkg, err, output)
			os.Exit(1)
		}
		return out
	}
	sampleBinary = build("github.com/rafaelp/vidyom/plugins/sample-notifier", "sample-notifier")
	probeBinary = build("github.com/rafaelp/vidyom/internal/plugins/testdata/probe", "probe")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---- fakes ----

type fakeRepo struct {
	mu      sync.Mutex
	records map[string]domain.PluginRecord
	err     error
}

func newFakeRepo() *fakeRepo { return &fakeRepo{records: map[string]domain.PluginRecord{}} }

func (f *fakeRepo) Upsert(_ context.Context, p domain.PluginRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.records[p.ID] = p
	return nil
}

func (f *fakeRepo) Get(_ context.Context, id string) (domain.PluginRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[id]
	if !ok {
		return rec, errs.NotFound("plugin")
	}
	return rec, nil
}

func (f *fakeRepo) List(context.Context) ([]domain.PluginRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.PluginRecord, 0, len(f.records))
	for _, rec := range f.records {
		out = append(out, rec)
	}
	return out, nil
}

func (f *fakeRepo) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.records, id)
	return nil
}

type fakeAuditor struct {
	mu      sync.Mutex
	entries []security.AuditEntry
	err     error
}

func (a *fakeAuditor) Record(_ context.Context, e security.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.entries = append(a.entries, e)
	return nil
}
func (a *fakeAuditor) Verify(context.Context, domain.TenantID) (domain.AuditVerification, error) {
	return domain.AuditVerification{}, nil
}
func (a *fakeAuditor) Export(context.Context, domain.TenantID) (string, error) { return "", nil }

func (a *fakeAuditor) find(action string) []security.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []security.AuditEntry
	for _, e := range a.entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

type secretMap map[string]string

func (s secretMap) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	v, ok := s[ref]
	if !ok {
		return nil, errors.New("no such secret: and this internal text must not leak")
	}
	return []byte(v), nil
}

// grantMap is an in-memory, changeable GrantSource.
type grantMap struct {
	mu     sync.Mutex
	grants map[string]Grant
	err    error
}

func (g *grantMap) set(tenant, pluginID string, grant Grant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.grants == nil {
		g.grants = map[string]Grant{}
	}
	g.grants[tenant+"/"+pluginID] = grant
}

func (g *grantMap) PluginGrant(_ context.Context, tenant domain.TenantID, pluginID string) (Grant, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return Grant{}, false, g.err
	}
	grant, ok := g.grants[string(tenant)+"/"+pluginID]
	return grant, ok, nil
}

type recordingMetrics struct {
	mu     sync.Mutex
	counts map[string][]telemetry.Labels
}

func (m *recordingMetrics) note(name string, labels telemetry.Labels) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts = map[string][]telemetry.Labels{}
	}
	m.counts[name] = append(m.counts[name], labels)
}
func (m *recordingMetrics) Add(name string, _ float64, l telemetry.Labels) { m.note(name, l) }
func (m *recordingMetrics) Set(name string, _ float64, l telemetry.Labels) { m.note(name, l) }
func (m *recordingMetrics) Observe(name string, _ float64, l telemetry.Labels) {
	m.note(name, l)
}
func (m *recordingMetrics) get(name string) []telemetry.Labels {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]telemetry.Labels(nil), m.counts[name]...)
}

// logCapture is a slog handler keeping everything, to scan for leaks.
type logCapture struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (c *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

// unavailableIsolator is a host with no sandbox.
type unavailableIsolator struct{}

func (unavailableIsolator) name() string { return IsolationNone }
func (unavailableIsolator) check(policy) error {
	return errors.New("no sandbox here")
}
func (unavailableIsolator) start(*exec.Cmd, policy) error { return errors.New("no sandbox here") }

// ---- fixture ----

type fixture struct {
	t       *testing.T
	repo    *fakeRepo
	audit   *fakeAuditor
	grants  *grantMap
	metrics *recordingMetrics
	logs    *logCapture
	secrets secretMap
	root    string // install root
	r       *Runtime
	opts    Options
}

// makeWritable lets t.TempDir's cleanup delete the read-only plugin store.
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o750)
		}
		return nil
	})
}

func newFixture(t *testing.T, tweak ...func(*Options)) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		t:       t,
		repo:    newFakeRepo(),
		audit:   &fakeAuditor{},
		grants:  &grantMap{},
		metrics: &recordingMetrics{},
		logs:    &logCapture{},
		secrets: secretMap{},
		root:    filepath.Join(root, "sources"),
	}
	if err := os.MkdirAll(f.root, 0o750); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "store")
	t.Cleanup(func() { makeWritable(store) })
	f.opts = Options{
		Repo:         f.repo,
		Dir:          store,
		InstallRoots: []string{f.root},
		Grants:       f.grants,
		Auditor:      f.audit,
		Metrics:      f.metrics,
		Logger:       f.logs.logger(),
		Secrets: func(domain.TenantID) connector.SecretResolver {
			return f.secrets
		},
		InitTimeout:     30 * time.Second, // the first run of a new binary can be slow on a loaded host
		ShutdownTimeout: time.Second,
		KillTimeout:     500 * time.Millisecond,
		IdleTimeout:     time.Hour,
	}
	for _, change := range tweak {
		change(&f.opts)
	}
	r, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.r = r
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.Close(ctx)
	})
	return f
}

func admin() service.Scope {
	return service.Scope{
		Tenant:    "acme",
		RequestID: "req-1",
		Principal: domain.Principal{Email: "admin@example.com", SystemAdmin: true, Role: domain.RoleAdmin, TenantID: "acme"},
	}
}

// writeTree creates a plugin directory in the install root.
func (f *fixture) writeTree(name, binary string, manifest map[string]any) string {
	f.t.Helper()
	dir := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o750); err != nil {
		f.t.Fatal(err)
	}
	if binary != "" {
		data, err := os.ReadFile(binary)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bin", "plugin"), data, 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugin.ManifestFile), data, 0o640); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

// probeActions are what the probe plugin answers.
var probeActions = []string{
	"env", "whoami", "echo-data", "read-file", "write-file", "exec", "dial", "udp", "unix", "http-get", "keyctl", "secret",
	"leak-secret", "fail-with-secret", "sleep", "ignore-cancel", "crash", "oversized", "flood", "big-parameters", "long-reason",
	"spawn-child", "spawn-detached", "stderr-only",
}

func probeManifest(id string, perms map[string]any, args ...string) map[string]any {
	if perms == nil {
		perms = map[string]any{}
	}
	entry := map[string]any{"command": "bin/plugin"}
	if len(args) > 0 {
		entry["args"] = args
	}
	return map[string]any{
		"id":          id,
		"version":     "1.0.0",
		"runtime":     "process",
		"entrypoint":  entry,
		"provides":    map[string]any{"hookActions": probeActions, "eventSubscriptions": []string{"publication.completed"}},
		"permissions": mergePerms(perms),
	}
}

func mergePerms(perms map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range perms {
		out[k] = v
	}
	events, _ := out["events"].([]string)
	if !strings.Contains(strings.Join(events, ","), "publication.completed") {
		out["events"] = append(events, "publication.completed")
	}
	return out
}

// installProbe installs the probe, granting exactly what perms says for acme.
func (f *fixture) installProbe(id string, perms plugin.Permissions, args ...string) domain.PluginRecord {
	f.t.Helper()
	declared := map[string]any{}
	if len(perms.Network) > 0 {
		declared["network"] = perms.Network
	}
	if len(perms.Secrets) > 0 {
		declared["secrets"] = perms.Secrets
	}
	if len(perms.Filesystem) > 0 {
		declared["filesystem"] = perms.Filesystem
	}
	if len(perms.Events) > 0 {
		declared["events"] = perms.Events
	}
	dir := f.writeTree(id, probeBinary, probeManifest(id, declared, args...))
	rec, err := f.r.Install(context.Background(), admin(), installRequest(dir))
	if err != nil {
		f.t.Fatalf("install %s: %v", id, err)
	}
	f.grants.set("acme", id, Grant{Enabled: true, Permissions: perms})
	return rec
}

func invocation(tenant, pluginID, action string, data any) hook.Invocation {
	inv := hook.Invocation{
		Point:    hook.NodeBefore,
		TenantID: domain.TenantID(tenant),
		JobID:    "JOB-001",
		NodeID:   "cut",
		Hook:     wfdef.HookDecl{ID: "h1", Plugin: pluginID, Action: action, TimeoutSeconds: 10},
	}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		inv.Data = raw
	}
	return inv
}

// call runs one probe action for acme.
func (f *fixture) call(pluginID, action string, data any) (hook.Result, error) {
	return f.r.RunHookAction(context.Background(), invocation("acme", pluginID, action, data))
}

func (f *fixture) mustCall(pluginID, action string, data any) hook.Result {
	f.t.Helper()
	result, err := f.call(pluginID, action, data)
	if err != nil {
		f.t.Fatalf("%s/%s: %v", pluginID, action, err)
	}
	return result
}

// requireSandbox skips a test where this host cannot enforce the sandbox, so the rest
// of the suite still runs. The final report says which hosts were exercised.
func requireSandbox(t *testing.T, proxyPort int) {
	t.Helper()
	if err := defaultIsolator().check(policy{proxyPort: proxyPort}); err != nil {
		t.Skipf("no OS sandbox on this host: %v", err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
