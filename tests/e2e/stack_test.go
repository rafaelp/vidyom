//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stack is one way to run the product. The scenario speaks only to this, so it runs
// unchanged natively and against Docker Compose (I286).
type stack interface {
	Name() string
	// Up prepares configuration and starts Redis.
	Up(t *testing.T)
	// Init runs `vidyomd init`.
	Init(t *testing.T) initResult
	// Refused tries to start the daemon with an unusable master key configuration
	// and returns its exit code and output. Variants: no-provider, missing-key.
	Refused(t *testing.T, variant string) (int, string)
	StartDaemon(t *testing.T)
	// KillDaemon sends SIGKILL.
	KillDaemon(t *testing.T)
	StartMockLLM(t *testing.T)
	StartAIWorker(t *testing.T)
	StartWorkers(t *testing.T)
	StopWorkers(t *testing.T)
	StartLibrary(t *testing.T)
	Redis(t *testing.T, args ...string) string
	// HTTPSURL is the daemon as a client on this machine reaches it.
	HTTPSURL() string
	LibraryURL() string
	// Fetch returns a local path holding the file at this path under the stack's root.
	Fetch(t *testing.T, rel string) string
	Exists(t *testing.T, rel string) bool
	// SQL runs statements on a SQLite file under the root, through the sqlite3 tool.
	SQL(t *testing.T, rel, statements string) string
	// PlaceIngest puts a local file inside the daemon's ingestRoots, and returns the
	// path as the server sees it.
	PlaceIngest(t *testing.T, local string) string
	// InstallPlugin puts a plugin tree where the daemon may install from, and returns
	// the path as the daemon sees it.
	InstallPlugin(t *testing.T, built string) string
	// WriteTenantConfig creates a file under the tenant's configuration directory and
	// asks the daemon to reload.
	WriteTenantConfig(t *testing.T, tenant, name, content string)
	// WorkerMetricsURL is where a worker's Prometheus endpoint can be read from this
	// machine, or empty when it cannot.
	WorkerMetricsURL() string
	// UnixSocket is the daemon's socket path on this machine, or empty.
	UnixSocket() string
	// Dump logs what a person needs to read after a failure.
	Dump(t *testing.T)
	Down(t *testing.T)
}

type initResult struct {
	Tenant         string `json:"tenant"`
	Admin          string `json:"admin"`
	PairingCode    string `json:"pairing_code"`
	TLSFingerprint string `json:"tls_fingerprint"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// freePort asks the OS for a port that is free right now, on loopback.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitFor polls until cond holds, failing the test with what's last described.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ok, detail := cond()
		if ok {
			return
		}
		last = detail
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s; last: %s", timeout, what, last)
}

// proc is one child process owned by the harness.
type proc struct {
	name string
	cmd  *exec.Cmd
	log  *os.File
	done chan struct{}
	err  error
}

func start(t *testing.T, logDir, name string, env []string, dir string, argv ...string) *proc {
	t.Helper()
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	p := &proc{name: name, cmd: cmd, log: logFile, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		logFile.Close()
		close(p.done)
	}()
	return p
}

func (p *proc) running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// stop sends a signal to the process group and waits, escalating to SIGKILL.
func (p *proc) stop(sig syscall.Signal, grace time.Duration) {
	if !p.running() {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, sig)
	select {
	case <-p.done:
	case <-time.After(grace):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

// run executes a command and returns its combined output and exit code.
func run(ctx context.Context, env []string, dir string, argv ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
			err = nil
		}
	}
	return stdout.String(), stderr.String(), code, err
}

func must(t *testing.T, env []string, dir string, argv ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	stdout, stderr, code, err := run(ctx, env, dir, argv...)
	if err != nil || code != 0 {
		t.Fatalf("%s: exit %d, error %v\nstdout:\n%s\nstderr:\n%s", strings.Join(argv, " "), code, err, stdout, stderr)
	}
	return stdout
}

// client is one simulated person: own configuration directory and keyring file.
type client struct {
	name    string
	bin     string
	dir     string
	keyring string
	baseURL string
	pin     string
}

func newClient(t *testing.T, name, root, bin, baseURL, pin string) *client {
	t.Helper()
	dir := filepath.Join(root, "clients", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &client{name: name, bin: bin, dir: dir, keyring: dir + ".keyring", baseURL: baseURL, pin: pin}
}

func (c *client) env() []string {
	return []string{
		"VIDYOM_CONFIG_DIR=" + c.dir,
		"VIDYOM_TEST_KEYRING_FILE=" + c.keyring,
		"HOME=" + c.dir,
		"PATH=" + os.Getenv("PATH"),
	}
}

// try runs the CLI, returning stdout, stderr without the test build's warning, and the
// exit code.
func (c *client) try(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	stdout, stderr, code, err := run(ctx, c.env(), c.dir, append([]string{c.bin}, args...)...)
	if err != nil {
		t.Fatalf("vidyom %s: %v", strings.Join(args, " "), err)
	}
	var kept []string
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.HasPrefix(line, "warning: test build") {
			kept = append(kept, line)
		}
	}
	return stdout, strings.Join(kept, "\n"), code
}

func (c *client) ok(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, code := c.try(t, args...)
	if code != 0 {
		t.Fatalf("%s: vidyom %s: exit %d\nstdout:\n%s\nstderr:\n%s", c.name, strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// jsonOut runs a command with --json and decodes its one document.
func (c *client) jsonOut(t *testing.T, out any, args ...string) {
	t.Helper()
	stdout := c.ok(t, append([]string{"--json"}, args...)...)
	if err := json.Unmarshal([]byte(stdout), out); err != nil {
		t.Fatalf("vidyom %s: cannot decode %q: %v", strings.Join(args, " "), stdout, err)
	}
}

// refused runs a command that must fail with this API error code, read from the JSON
// error body, and returns the message.
func (c *client) refused(t *testing.T, code string, args ...string) string {
	t.Helper()
	stdout, stderr, exit := c.try(t, append([]string{"--json"}, args...)...)
	if exit == 0 {
		t.Fatalf("%s: vidyom %s succeeded: %s", c.name, strings.Join(args, " "), stdout)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &body); err != nil {
		t.Fatalf("%s: vidyom %s: exit %d, error body %q: %v", c.name, strings.Join(args, " "), exit, stderr, err)
	}
	if body.Error.Code != code {
		t.Fatalf("%s: vidyom %s: code %q, want %q: %s", c.name, strings.Join(args, " "), body.Error.Code, code, body.Error.Message)
	}
	return body.Error.Message
}

// credential reads this client's device credential from its test keyring.
func (c *client) credential(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(c.keyring)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	for key, value := range entries {
		if strings.Contains(key, "device/") {
			return value
		}
	}
	t.Fatalf("no device credential in %s", c.keyring)
	return ""
}

// pinnedClient talks HTTPS to a server identified only by its certificate's hash.
func pinnedClient(pin string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS13,
				InsecureSkipVerify: true, // checked below, against the pin
				VerifyConnection: func(cs tls.ConnectionState) error {
					if len(cs.PeerCertificates) == 0 {
						return errors.New("no certificate")
					}
					return checkPin(cs.PeerCertificates[0], pin)
				},
			},
		},
	}
}

func checkPin(cert *x509.Certificate, pin string) error {
	sum := sha256.Sum256(cert.Raw)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != pin {
		return fmt.Errorf("certificate %s does not match the pin %s", got, pin)
	}
	return nil
}

// httpCall is a direct API request, for evidence that is about HTTP itself.
func httpCall(t *testing.T, c *http.Client, method, url string, headers map[string]string, body []byte) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

// tail returns the last lines of a file, for failure reports.
func tail(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}
