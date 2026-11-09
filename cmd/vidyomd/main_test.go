package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	apiserver "github.com/rafaelp/vidyom/internal/api"
	"github.com/rafaelp/vidyom/internal/api/fakestore"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/version"
	"github.com/rafaelp/vidyom/internal/daemon"
)

// The test binary doubles as the daemon: with childEnv set, it runs main's logic in
// its own process, so exit codes, signals, sockets and --detach are all real. Only
// the wiring is fake, since component implementations are not imported here.
const (
	childEnv  = "VIDYOMD_TEST_CHILD"
	childWire = "wired"
	childNone = "unwired"
	sealedEnv = "VIDYOMD_TEST_SEALED"
)

func TestMain(m *testing.M) {
	switch os.Getenv(childEnv) {
	case childWire:
		wireDeps = testWire
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	case childNone:
		wireDeps = nil
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

type testProvider struct{}

func (testProvider) Name() string                   { return "test" }
func (testProvider) Sealing() security.SealingLevel { return security.SealingFile }
func (testProvider) Unseal(context.Context) ([]byte, error) {
	if os.Getenv(sealedEnv) != "" {
		return nil, errors.New("sealed")
	}
	return make([]byte, 32), nil
}

type testAuthn struct{}

func (testAuthn) Authenticate(_ context.Context, c security.Credentials) (domain.Principal, error) {
	if c.PeerUID != nil && *c.PeerUID == os.Getuid() {
		return domain.Principal{Email: "admin@example.com", SystemAdmin: true, Method: domain.AuthPeer}, nil
	}
	return domain.Principal{}, errs.New(errs.CodeUnauthenticated, "no identity")
}

func (testAuthn) AuthenticateWorker(context.Context, security.Credentials) (string, error) {
	return "", errs.New(errs.CodeUnauthenticated, "no worker")
}

type testAuthz struct{}

func (testAuthz) Resolve(_ context.Context, p domain.Principal, t domain.TenantID) (domain.Principal, error) {
	p.TenantID, p.Role = t, domain.RoleAdmin
	return p, nil
}

func (testAuthz) Require(_ context.Context, p domain.Principal, perm domain.Permission) error {
	if p.Can(perm) {
		return nil
	}
	return errs.Forbidden(string(perm))
}

type testAudit struct{}

func (testAudit) Record(context.Context, security.AuditEntry) error { return nil }
func (testAudit) Verify(context.Context, domain.TenantID) (domain.AuditVerification, error) {
	return domain.AuditVerification{}, nil
}
func (testAudit) Export(context.Context, domain.TenantID) (string, error) { return "", nil }

// testWire reports cfg.Bootstrapping through a marker, and refuses a second
// bootstrap as core's does.
func testWire(_ context.Context, cfg config.Daemon) (daemon.Deps, func(), error) {
	marker := filepath.Join(cfg.DataDir, "bootstrapped")
	return daemon.Deps{
		API: apiserver.Deps{
			Authenticator: testAuthn{},
			Authorizer:    testAuthz{},
			Auditor:       testAudit{},
			TenantStores:  fakestore.New("acme"),
		},
		MasterKey: testProvider{},
		Bootstrap: func(_ context.Context, req daemon.BootstrapRequest) (daemon.BootstrapResult, error) {
			if !cfg.Bootstrapping {
				return daemon.BootstrapResult{}, errors.New("Bootstrapping was not set for init")
			}
			if _, err := os.Stat(marker); err == nil {
				return daemon.BootstrapResult{}, errs.New(errs.CodeConflict, "a tenant already exists")
			}
			if err := os.WriteFile(marker, []byte(req.AdminEmail), 0o600); err != nil {
				return daemon.BootstrapResult{}, err
			}
			return daemon.BootstrapResult{
				PairingCode:      "ABCDE-FGHIJ-KLMNO-PQRST",
				PairingExpiresAt: time.Now().Add(15 * time.Minute),
				WorkerToken:      "vdw_fictional_worker_token",
			}, nil
		},
	}, func() {}, nil
}

// fixture is a configuration directory and a data directory short enough for a
// socket path.
type fixture struct {
	configDir string
	dataDir   string
	socket    string
}

func newFixture(t *testing.T, settings map[string]any) *fixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "vc")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fixture{
		configDir: filepath.Join(dir, "etc"),
		dataDir:   filepath.Join(dir, "data"),
		socket:    filepath.Join(dir, "run", "d.sock"),
	}
	if err := os.MkdirAll(f.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"dataDir":   f.dataDir,
		"redis":     map[string]any{"url": "redis://127.0.0.1:1"},
		"vault":     map[string]any{"unseal": map[string]any{"provider": "file"}},
		"listeners": map[string]any{"unix": map[string]any{"path": f.socket}},
	}
	for k, v := range settings {
		if v == nil {
			delete(cfg, k)
		} else {
			cfg[k] = v
		}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.configDir, daemon.ConfigFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func child(mode string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), childEnv+"="+mode)
	return cmd
}

// runChild runs to completion and returns the exit code, stdout and stderr.
func runChild(t *testing.T, cmd *exec.Cmd) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run: %v", err)
		}
		code = exit.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

func unixClient(path string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}
}

func waitForSocket(t *testing.T, path string, exited <-chan error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			t.Fatalf("the daemon exited: %v", err)
		default:
		}
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the socket never appeared")
}

func noSocket(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a socket exists at %s: %v", path, err)
	}
}

func TestUsageAndVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Errorf("no arguments: %d", code)
	}
	if code := run([]string{"frobnicate"}, &out, &errOut); code != 2 {
		t.Errorf("unknown command: %d", code)
	}
	out.Reset()
	if code := run([]string{"version"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), version.Version) {
		t.Errorf("version: %d %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"help"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "vidyomd start") {
		t.Errorf("help: %d %q", code, out.String())
	}
	if code := run([]string{"start", "--nope"}, &out, &errOut); code != 2 {
		t.Errorf("bad flag: %d", code)
	}
	if code := run([]string{"start", "extra"}, &out, &errOut); code != 2 {
		t.Errorf("extra argument: %d", code)
	}
	if code := run([]string{"init", "--tenant", "acme"}, &out, &errOut); code != 2 {
		t.Errorf("init without an admin: %d", code)
	}
}

func TestNotWiredIsClearAndListensOnNothing(t *testing.T) {
	f := newFixture(t, nil)
	code, _, stderr := runChild(t, child(childNone, "start", "--config-dir", f.configDir))
	if code != 1 || !strings.Contains(stderr, "not wired") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	noSocket(t, f.socket)

	code, _, stderr = runChild(t, child(childNone, "init", "--config-dir", f.configDir, "--tenant", "acme", "--admin", "a@example.com"))
	if code != 1 || !strings.Contains(stderr, "not wired") {
		t.Fatalf("init: exit %d: %s", code, stderr)
	}
}

// §1.1 step 1: with no master key configured the daemon exits non-zero and opens no
// listener.
func TestNoMasterKeyConfigured(t *testing.T) {
	f := newFixture(t, map[string]any{"vault": nil})
	code, _, stderr := runChild(t, child(childWire, "start", "--config-dir", f.configDir))
	if code == 0 || !strings.Contains(stderr, "master key") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	noSocket(t, f.socket)
}

func TestSealedKeyListensOnNothing(t *testing.T) {
	f := newFixture(t, nil)
	cmd := child(childWire, "start", "--config-dir", f.configDir)
	cmd.Env = append(cmd.Env, sealedEnv+"=1")
	code, _, stderr := runChild(t, cmd)
	if code == 0 || !strings.Contains(stderr, "unsealed") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	noSocket(t, f.socket)
}

func TestConfigurationIsRequired(t *testing.T) {
	cmd := child(childWire, "start")
	cmd.Env = append(cmd.Env, daemon.ConfigDirEnv+"=")
	code, _, stderr := runChild(t, cmd)
	if code == 0 || !strings.Contains(stderr, "configuration directory") {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	f := newFixture(t, map[string]any{"listners": map[string]any{}})
	code, _, stderr = runChild(t, child(childWire, "start", "--config-dir", f.configDir))
	if code == 0 || !strings.Contains(stderr, "listners") {
		t.Fatalf("a misspelled key: exit %d: %s", code, stderr)
	}
	noSocket(t, f.socket)
}

func TestConfigDirFromEnvironment(t *testing.T) {
	f := newFixture(t, nil)
	cmd := child(childWire, "init", "--tenant", "acme", "--admin", "a@example.com")
	cmd.Env = append(cmd.Env, daemon.ConfigDirEnv+"="+f.configDir)
	if code, _, stderr := runChild(t, cmd); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestStartServesAndStopsOnSignal(t *testing.T) {
	f := newFixture(t, nil)
	cmd := child(childWire, "start", "--config-dir", f.configDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitForSocket(t, f.socket, exited)

	info, err := os.Stat(f.socket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", info, err)
	}

	resp, err := unixClient(f.socket).Get("http://vidyomd" + api.Prefix + "/health")
	if err != nil {
		t.Fatal(err)
	}
	var health api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || health.Sealing != "file" {
		t.Fatalf("health: %d %+v", resp.StatusCode, health)
	}

	// The kernel's identity of this test process reaches the authenticator.
	req, err := http.NewRequest("GET", "http://vidyomd"+api.Prefix+"/doctor", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = unixClient(f.socket).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("doctor over peer identity: %d", resp.StatusCode)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v\n%s", err, stderr.String())
		}
	case <-time.After(45 * time.Second):
		t.Fatal("the daemon did not stop on SIGTERM")
	}
	noSocket(t, f.socket)

	// Logs are JSON lines with the fields of §10.1.
	first, _, _ := strings.Cut(stderr.String(), "\n")
	var line map[string]any
	if err := json.Unmarshal([]byte(first), &line); err != nil {
		t.Fatalf("log line %q: %v", first, err)
	}
	for _, key := range []string{"timestamp", "level", "service", "event"} {
		if _, ok := line[key]; !ok {
			t.Errorf("log line lacks %s: %s", key, first)
		}
	}
}

func TestDetachReportsReadiness(t *testing.T) {
	f := newFixture(t, nil)
	code, stdout, stderr := runChild(t, child(childWire, "start", "--config-dir", f.configDir, "--detach"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "started") {
		t.Errorf("stdout = %q", stdout)
	}

	// It was already listening when the parent returned, and the pid file names it.
	resp, err := unixClient(f.socket).Get("http://vidyomd" + api.Prefix + "/version")
	if err != nil {
		t.Fatalf("the parent reported success before the daemon listened: %v", err)
	}
	_ = resp.Body.Close()
	data, err := os.ReadFile(filepath.Join(f.dataDir, "vidyomd.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !strings.Contains(stdout, strconv.Itoa(pid)) {
		t.Fatalf("pid %q in %q: %v", data, stdout, err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Kill() })

	// A second start fails closed, while the first keeps running.
	code, _, stderr = runChild(t, child(childWire, "start", "--config-dir", f.configDir))
	if code == 0 || !strings.Contains(stderr, "another") {
		t.Fatalf("second daemon: exit %d: %s", code, stderr)
	}
	resp, err = unixClient(f.socket).Get("http://vidyomd" + api.Prefix + "/version")
	if err != nil {
		t.Fatalf("the refused start harmed the running daemon: %v", err)
	}
	_ = resp.Body.Close()

	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		if _, err := os.Lstat(f.socket); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached daemon did not stop on SIGTERM")
		}
		time.Sleep(20 * time.Millisecond)
	}
	logData, err := os.ReadFile(filepath.Join(f.dataDir, "logs", "vidyomd.log"))
	if err != nil || !strings.Contains(string(logData), "daemon.ready") {
		t.Fatalf("log: %q, %v", logData, err)
	}
}

// A daemon that fails closed in the background must not be reported as started.
func TestDetachReportsFailure(t *testing.T) {
	f := newFixture(t, nil)
	cmd := child(childWire, "start", "--config-dir", f.configDir, "--detach")
	cmd.Env = append(cmd.Env, sealedEnv+"=1")
	code, stdout, stderr := runChild(t, cmd)
	if code != 1 || !strings.Contains(stderr, "did not start") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "started") {
		t.Fatalf("reported as started: %q", stdout)
	}
	noSocket(t, f.socket)
	logData, err := os.ReadFile(filepath.Join(f.dataDir, "logs", "vidyomd.log"))
	if err != nil || !strings.Contains(string(logData), "unsealed") {
		t.Fatalf("the reason is not in the log: %q, %v", logData, err)
	}
}

func TestInit(t *testing.T) {
	f := newFixture(t, nil)
	args := []string{"init", "--config-dir", f.configDir, "--tenant", "acme", "--admin", "admin@example.com"}
	code, stdout, stderr := runChild(t, child(childWire, args...))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"ABCDE-FGHIJ-KLMNO-PQRST", "acme", "admin@example.com", "worker.token"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q: %s", want, stdout)
		}
	}
	// The worker token goes to its file, never to the terminal.
	if strings.Contains(stdout+stderr, "vdw_fictional_worker_token") {
		t.Fatalf("the worker token was printed: %s %s", stdout, stderr)
	}
	token, err := os.ReadFile(filepath.Join(f.dataDir, "secrets", "worker.token"))
	if err != nil || strings.TrimSpace(string(token)) != "vdw_fictional_worker_token" {
		t.Fatalf("token: %q, %v", token, err)
	}

	// §I17: an initialized data directory is refused.
	code, _, stderr = runChild(t, child(childWire, args...))
	if code == 0 || !strings.Contains(stderr, "initialized") {
		t.Fatalf("second init: exit %d: %s", code, stderr)
	}
}

func TestInitJSON(t *testing.T) {
	f := newFixture(t, nil)
	code, stdout, stderr := runChild(t, child(childWire, "init", "--config-dir", f.configDir, "--tenant", "acme", "--admin", "a@example.com", "--json"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var out daemon.InitResult
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("output %q: %v", stdout, err)
	}
	if out.PairingCode == "" || out.Tenant != "acme" || out.WorkerTokenFile == "" {
		t.Fatalf("out = %+v", out)
	}
}

func TestGenerateMasterKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	code, _, stderr := runChild(t, child(childNone, "init", "--generate-master-key", path))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "plaintext") {
		t.Errorf("the sealing level is not stated: %s", stderr)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("key: %v %v", info, err)
	}
	code, _, stderr = runChild(t, child(childNone, "init", "--generate-master-key", path))
	if code == 0 || !strings.Contains(stderr, "refusing") {
		t.Fatalf("overwrite: exit %d: %s", code, stderr)
	}
}

func TestGenerateMasterKeyThenInit(t *testing.T) {
	f := newFixture(t, nil)
	key := filepath.Join(f.dataDir+"-keys", "master.key")
	code, stdout, stderr := runChild(t, child(childWire, "init", "--generate-master-key", key, "--config-dir", f.configDir, "--tenant", "acme", "--admin", "a@example.com"))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "ABCDE-FGHIJ-KLMNO-PQRST") {
		t.Fatalf("stdout = %s", stdout)
	}
}

func TestNewLoggerFields(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf).Info("daemon.ready", "version", "x")
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["event"] != "daemon.ready" || line["service"] != "vidyomd" || line["timestamp"] == nil {
		t.Fatalf("line = %v", line)
	}
}
