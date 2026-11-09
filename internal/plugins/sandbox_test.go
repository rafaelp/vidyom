package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/plugin"
)

// These run the real OS sandbox against a real child process, on hosts that have one:
// macOS (Seatbelt) and Linux (Landlock and seccomp). What is denied is observed from
// inside the plugin, so they prove enforcement rather than configuration.

func tempReal(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSandboxFilesystem(t *testing.T) {
	requireSandbox(t, 0)
	data := tempReal(t) // stands for the daemon's data directory: tenant.db, system.db
	secretFile := filepath.Join(data, "tenant.db")
	if err := os.WriteFile(secretFile, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	readable := tempReal(t)
	readableFile := filepath.Join(readable, "models.txt")
	if err := os.WriteFile(readableFile, []byte("model data"), 0o600); err != nil {
		t.Fatal(err)
	}
	writable := tempReal(t)
	f := newFixture(t)
	f.installProbe("p", plugin.Permissions{Filesystem: []string{PermJobTemp, "ro:" + readable, writable}})
	f.installProbe("nothing", plugin.Permissions{})

	var who map[string]any
	if err := json.Unmarshal(f.mustCall("p", "whoami", nil).Parameters, &who); err != nil {
		t.Fatal(err)
	}
	tempDir := who["temp"].(string)
	if tempDir == "" {
		t.Fatal("job-temp was granted")
	}

	allowed := []struct {
		name, action string
		data         map[string]string
	}{
		{"read a ro: grant", "read-file", map[string]string{"path": readableFile}},
		{"write the private temp directory", "write-file", map[string]string{"path": filepath.Join(tempDir, "scratch"), "data": "x"}},
		{"write a rw grant", "write-file", map[string]string{"path": filepath.Join(writable, "out"), "data": "x"}},
		{"read the plugin's own files", "read-file", map[string]string{"path": "vidyom-plugin.json"}},
	}
	for _, c := range allowed {
		if result := f.mustCall("p", c.action, c.data); !result.Allow {
			t.Errorf("%s was denied: %+v", c.name, result)
		}
	}
	if got, err := os.ReadFile(filepath.Join(writable, "out")); err != nil || string(got) != "x" {
		t.Errorf("the write did not land: %q %v", got, err)
	}

	store := f.r.store.path("p", "1.0.0", mustRecord(t, f, "p").SHA256)
	denied := []struct {
		name, plugin, action string
		data                 map[string]string
	}{
		{"read the daemon's data", "p", "read-file", map[string]string{"path": secretFile}},
		{"write the daemon's data", "p", "write-file", map[string]string{"path": filepath.Join(data, "x"), "data": "x"}},
		{"write a ro: grant", "p", "write-file", map[string]string{"path": filepath.Join(readable, "x"), "data": "x"}},
		{"change its own manifest", "p", "write-file", map[string]string{"path": filepath.Join(store, "vidyom-plugin.json"), "data": "x"}},
		{"add to its own store", "p", "write-file", map[string]string{"path": filepath.Join(store, "x"), "data": "x"}},
		{"read a grant it was not given", "nothing", "read-file", map[string]string{"path": readableFile}},
		{"use another process's temp directory", "nothing", "read-file", map[string]string{"path": filepath.Join(tempDir, "scratch")}},
		{"write to the temp directory with no grant", "nothing", "write-file", map[string]string{"path": filepath.Join(os.TempDir(), "plugin-escape")}},
	}
	for _, c := range denied {
		result := f.mustCall(c.plugin, c.action, c.data)
		if result.Allow {
			t.Errorf("%s was allowed: %+v", c.name, result)
		}
		if !strings.Contains(result.Reason, "not permitted") && !strings.Contains(result.Reason, "permission denied") {
			t.Errorf("%s failed for another reason: %s", c.name, result.Reason)
		}
	}
	if got, err := os.ReadFile(secretFile); err != nil || string(got) != "private" {
		t.Errorf("the data file changed: %q %v", got, err)
	}
	var nothing map[string]any
	if err := json.Unmarshal(f.mustCall("nothing", "whoami", nil).Parameters, &nothing); err != nil {
		t.Fatal(err)
	}
	if nothing["temp"] != "" {
		t.Errorf("no job-temp grant, no temp directory: %v", nothing["temp"])
	}
}

func mustRecord(t *testing.T, f *fixture, id string) domain.PluginRecord {
	t.Helper()
	rec, err := f.repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestSandboxOneTenantCannotReachAnother(t *testing.T) {
	requireSandbox(t, 0)
	f := newFixture(t)
	f.installProbe("p", plugin.Permissions{Filesystem: []string{PermJobTemp}})
	f.grants.set("other", "p", Grant{Enabled: true, Permissions: plugin.Permissions{Filesystem: []string{PermJobTemp}}})
	ctx := context.Background()

	var who map[string]any
	if err := json.Unmarshal(f.mustCall("p", "whoami", nil).Parameters, &who); err != nil {
		t.Fatal(err)
	}
	acmeFile := filepath.Join(who["temp"].(string), "acme-only")
	if result := f.mustCall("p", "write-file", map[string]string{"path": acmeFile, "data": "acme"}); !result.Allow {
		t.Fatalf("own temp directory: %+v", result)
	}
	result, err := f.r.RunHookAction(ctx, invocation("other", "p", "read-file", map[string]string{"path": acmeFile}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Allow {
		t.Errorf("tenant other read tenant acme's file: %+v", result)
	}
}

func TestSandboxCommandExecution(t *testing.T) {
	requireSandbox(t, 0)
	f := newFixture(t)
	f.installProbe("p", plugin.Permissions{})
	for _, command := range []string{"/bin/sh", "/usr/bin/env", "/bin/ls"} {
		result := f.mustCall("p", "exec", map[string]string{"command": command})
		if result.Allow {
			t.Errorf("%s ran: %+v", command, result)
		}
	}
	// What it ships in its own directory is not arbitrary execution (I265).
	result := f.mustCall("p", "spawn-child", nil)
	if !result.Allow {
		t.Errorf("its own binary should be executable: %+v", result)
	}
}

func TestSandboxOperatorRuntimePathsAllowExec(t *testing.T) {
	requireSandbox(t, 0)
	// /usr/bin/true ignores the arguments the probe passes. /bin/sh is no good here: on
	// macOS it is a shim that executes another shell.
	const allowed = "/usr/bin/true"
	f := newFixture(t, func(o *Options) { o.RuntimePaths = []string{allowed} })
	f.installProbe("p", plugin.Permissions{})
	if result := f.mustCall("p", "exec", map[string]string{"command": allowed}); !result.Allow {
		t.Errorf("an operator-declared runtime: %+v", result)
	}
	if result := f.mustCall("p", "exec", map[string]string{"command": "/bin/ls"}); result.Allow {
		t.Errorf("anything else stays denied: %+v", result)
	}
	without := newFixture(t)
	without.installProbe("p", plugin.Permissions{})
	if result := without.mustCall("p", "exec", map[string]string{"command": allowed}); result.Allow {
		t.Errorf("not declared, not allowed: %+v", result)
	}
}

func TestSandboxNetwork(t *testing.T) {
	requireSandbox(t, 0)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "reached") }))
	defer upstream.Close()
	addr := upstream.Listener.Addr().String()

	unixDir, err := os.MkdirTemp("", "vu")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(unixDir)
	sockPath := filepath.Join(unixDir, "s")
	unixListener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unixListener.Close()

	f := newFixture(t)
	f.installProbe("none", plugin.Permissions{})

	// No grant: nothing at all works.
	for name, c := range map[string]struct {
		action string
		data   map[string]string
	}{
		"TCP":    {"dial", map[string]string{"addr": addr}},
		"UDP":    {"udp", map[string]string{"addr": "8.8.8.8:53"}},
		"Unix":   {"unix", map[string]string{"addr": sockPath}},
		"lookup": {"http-get", map[string]string{"url": upstream.URL}},
	} {
		if result := f.mustCall("none", c.action, c.data); result.Allow {
			t.Errorf("%s with no grant worked: %+v", name, result)
		}
	}

	// A grant: the proxy reaches the granted host, and a direct connection still fails.
	if err := defaultIsolator().check(policy{proxyPort: 1}); err != nil {
		t.Skipf("this kernel cannot restrict TCP to a port: %v", err)
	}
	f.installProbe("granted", plugin.Permissions{Network: []string{addr}})
	result := f.mustCall("granted", "http-get", map[string]string{"url": upstream.URL})
	if !result.Allow || !strings.Contains(result.Reason, "reached") {
		t.Errorf("through the proxy: %+v", result)
	}
	if result := f.mustCall("granted", "dial", map[string]string{"addr": addr}); result.Allow {
		t.Errorf("a direct connection bypasses the allow-list: %+v", result)
	}
	if result := f.mustCall("granted", "unix", map[string]string{"addr": sockPath}); result.Allow {
		t.Errorf("a Unix socket with a network grant: %+v", result)
	}
	f.installProbe("elsewhere", plugin.Permissions{Network: []string{"127.0.0.1:1"}})
	result = f.mustCall("elsewhere", "http-get", map[string]string{"url": upstream.URL})
	if result.Allow || !strings.Contains(result.Reason, "403") {
		t.Errorf("a host not on the list: %+v", result)
	}
}

func TestSandboxSamplePluginEndToEnd(t *testing.T) {
	requireSandbox(t, 1)
	var token string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	f := newFixture(t)
	f.secrets["notifier-token"] = "notifier-secret-value"
	installSample(t, f, upstream.Listener.Addr().String())
	result := f.mustCall("sample-notifier", "notify", map[string]string{"url": upstream.URL})
	if !result.Allow {
		t.Fatalf("notify: %+v\n%s", result, f.logs.String())
	}
	if token != "Bearer notifier-secret-value" {
		t.Errorf("the granted secret should reach the granted host: %q", token)
	}
	inv := invocation("acme", "sample-notifier", "record-finish", nil)
	if _, err := f.r.RunHookAction(context.Background(), inv); err != nil {
		t.Errorf("job-temp under the sandbox: %v", err)
	}
	if strings.Contains(f.logs.String(), "notifier-secret-value") {
		t.Errorf("the secret reached the logs: %s", f.logs.String())
	}
}

func TestSandboxReportsIsolationHonestly(t *testing.T) {
	requireSandbox(t, 0)
	f := newFixture(t)
	f.installProbe("p", plugin.Permissions{})
	rec, err := f.r.Get(context.Background(), admin(), "p")
	if err != nil {
		t.Fatal(err)
	}
	want := IsolationSeatbelt
	if runtime.GOOS == "linux" {
		want = IsolationLandlock
	}
	if rec.Isolation != want {
		t.Errorf("isolation %q, want %q", rec.Isolation, want)
	}
	checks, err := f.r.Verify(context.Background())
	if err != nil || len(checks) != 1 || checks[0].Isolation != want || !checks[0].OK {
		t.Errorf("verify: %+v %v", checks, err)
	}
}
