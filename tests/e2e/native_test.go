//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// nativeStack runs the real binaries on this host.
type nativeStack struct {
	repo  string
	root  string
	bins  string
	redis string

	redisPort, httpsPort, mockPort, libraryPort int

	procs map[string]*proc
}

func newNative(t *testing.T) *nativeStack {
	t.Helper()
	// A short root: a Unix socket path must fit in about 100 bytes on macOS.
	root, err := os.MkdirTemp("", "vdme2e")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if len(root) > 70 {
		_ = os.RemoveAll(root)
		if root, err = os.MkdirTemp("/tmp", "vdme2e"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return &nativeStack{
		repo:        repoRoot(t),
		root:        root,
		bins:        filepath.Join(root, "bin"),
		redis:       os.Getenv("VIDYOM_E2E_REDIS_SERVER"),
		redisPort:   freePort(t),
		httpsPort:   freePort(t),
		mockPort:    freePort(t),
		libraryPort: freePort(t),
		procs:       map[string]*proc{},
	}
}

func (s *nativeStack) Name() string { return "native" }

func (s *nativeStack) path(parts ...string) string {
	return filepath.Join(append([]string{s.root}, parts...)...)
}

func (s *nativeStack) env(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, extra...)
}

// Up builds the binaries, renders the configuration, starts Redis.
func (s *nativeStack) Up(t *testing.T) {
	t.Helper()
	for _, dir := range []string{"bin", "data", "work", "run", "secrets", "storage", "ingest", "logs", "clients", "mock", "public", "fixtures", "plugins"} {
		if err := os.MkdirAll(s.path(dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	build := func(env []string, output, pkg string, flags ...string) {
		args := append([]string{"build", "-o", filepath.Join(s.bins, output)}, flags...)
		args = append(args, pkg)
		must(t, append(os.Environ(), env...), s.repo, append([]string{"go"}, args...)...)
	}
	build([]string{"CGO_CFLAGS=-g -O2 -Wno-deprecated-declarations"}, "vidyomd", "./cmd/vidyomd")
	build([]string{"CGO_ENABLED=0"}, "vidyom-worker", "./cmd/vidyom-worker")
	build([]string{"CGO_ENABLED=0"}, "vidyom", "./cmd/vidyom", "-tags", "vidyom_test_keyring")
	build([]string{"CGO_ENABLED=0"}, "vidyom-mock-llm", "./connectors/ai/openai/mockllm/cmd/vidyom-mock-llm")

	s.render(t, filepath.Join(s.repo, "config", "local"), s.path("config"))
	// A variant with no provider and one whose key does not exist, for step 1.
	s.renderVariant(t, "no-provider", func(cfg map[string]any) { delete(cfg, "vault") })
	s.renderVariant(t, "missing-key", func(cfg map[string]any) {
		cfg["vault"] = map[string]any{"unseal": map[string]any{
			"provider": "file", "path": s.path("secrets", "absent.key"), "acknowledgeFileSealed": true,
		}}
	})

	server := s.redis
	if server == "" {
		server = "redis-server"
	}
	s.procs["redis"] = start(t, s.path("logs"), "redis", s.env(), s.root,
		server, "--port", strconv.Itoa(s.redisPort), "--bind", "127.0.0.1",
		"--appendonly", "yes", "--dir", s.path("work"), "--save", "")
	waitFor(t, 20*time.Second, "redis", func() (bool, string) {
		out := s.tryRedis("PING")
		return strings.TrimSpace(out) == "PONG", out
	})
}

func (s *nativeStack) render(t *testing.T, from, to string) {
	t.Helper()
	replace := strings.NewReplacer(
		"@ROOT@", s.root,
		"@REDIS_PORT@", strconv.Itoa(s.redisPort),
		"@HTTPS_PORT@", strconv.Itoa(s.httpsPort),
		"@MOCK_LLM_PORT@", strconv.Itoa(s.mockPort),
	)
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, []byte(replace.Replace(string(data))), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (s *nativeStack) renderVariant(t *testing.T, name string, change func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(s.path("config", "daemon.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	change(cfg)
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := s.path("config-" + name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (s *nativeStack) Init(t *testing.T) initResult {
	t.Helper()
	must(t, s.env(), s.root, filepath.Join(s.bins, "vidyomd"), "init", "--generate-master-key", s.path("secrets", "master.key"))
	out := must(t, s.env(), s.root, filepath.Join(s.bins, "vidyomd"), "init", "--config-dir", s.path("config"), "--tenant", "acme", "--admin", "admin@example.com", "--json")
	var result initResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode init output %q: %v", out, err)
	}
	return result
}

func (s *nativeStack) Refused(t *testing.T, variant string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, stderr, code, err := run(ctx, s.env(), s.root, filepath.Join(s.bins, "vidyomd"), "start", "--config-dir", s.path("config-"+variant))
	if err != nil {
		t.Fatalf("run vidyomd: %v", err)
	}
	if _, statErr := os.Stat(s.path("run", "vidyomd.sock")); statErr == nil {
		t.Errorf("a refused daemon left a socket behind")
	}
	return code, stdout + stderr
}

func (s *nativeStack) StartDaemon(t *testing.T) {
	t.Helper()
	s.procs["vidyomd"] = start(t, s.path("logs"), "vidyomd", s.env(), s.root,
		filepath.Join(s.bins, "vidyomd"), "start", "--config-dir", s.path("config"))
	waitFor(t, 30*time.Second, "the daemon's socket", func() (bool, string) {
		if !s.procs["vidyomd"].running() {
			t.Fatalf("the daemon exited: %v\n%s", s.procs["vidyomd"].err, tail(s.path("logs", "vidyomd.log"), 30))
		}
		_, err := os.Stat(s.path("run", "vidyomd.sock"))
		return err == nil, fmt.Sprint(err)
	})
}

func (s *nativeStack) KillDaemon(t *testing.T) {
	t.Helper()
	p := s.procs["vidyomd"]
	if p == nil {
		t.Fatal("no daemon to kill")
	}
	p.stop(syscall.SIGKILL, 5*time.Second)
}

func (s *nativeStack) StartMockLLM(t *testing.T) {
	t.Helper()
	s.procs["mock-llm"] = start(t, s.path("logs"), "mock-llm", s.env(), s.root,
		filepath.Join(s.bins, "vidyom-mock-llm"), "--listen", fmt.Sprintf("127.0.0.1:%d", s.mockPort))
}

func (s *nativeStack) StartAIWorker(t *testing.T) {
	t.Helper()
	s.procs["ai-python"] = start(t, s.path("logs"), "ai-python", s.env("PYTHONDONTWRITEBYTECODE=1"), filepath.Join(s.repo, "aiworker"),
		pythonCommand(), "-m", "vidyom", "--engine", "fake", "--socket", s.path("run", "ai.sock"))
	waitFor(t, 30*time.Second, "the AI worker's socket", func() (bool, string) {
		if !s.procs["ai-python"].running() {
			t.Fatalf("the Python worker exited: %v\n%s", s.procs["ai-python"].err, tail(s.path("logs", "ai-python.log"), 30))
		}
		_, err := os.Stat(s.path("run", "ai.sock"))
		return err == nil, fmt.Sprint(err)
	})
}

func pythonCommand() string {
	if python := os.Getenv("VIDYOM_E2E_PYTHON"); python != "" {
		return python
	}
	return "python3"
}

func (s *nativeStack) StartWorkers(t *testing.T) {
	t.Helper()
	worker := func(name, pool string, extra ...string) {
		args := append([]string{
			filepath.Join(s.bins, "vidyom-worker"),
			"--config", s.path("config", "daemon.json"),
			"--pools", pool,
			"--token-file", s.path("data", "secrets", "worker.token"),
			"--cpu-cores", strconv.Itoa(max(runtime.NumCPU(), 8)),
			"--memory-mb", "32768",
			"--disk-mb", "204800",
			"--max-concurrency", "4",
		}, extra...)
		s.procs[name] = start(t, s.path("logs"), name, s.env(), s.root, args...)
	}
	worker("worker-media", "media")
	worker("worker-ai", "ai")
}

func (s *nativeStack) StopWorkers(t *testing.T) {
	t.Helper()
	for _, name := range []string{"worker-media", "worker-ai"} {
		if p := s.procs[name]; p != nil {
			p.stop(syscall.SIGTERM, 40*time.Second)
		}
	}
}

// StartLibrary runs the real Next.js production server against the shipped file. It
// is given no address of the daemon, because it has none to use (D34).
func (s *nativeStack) StartLibrary(t *testing.T) {
	t.Helper()
	library := filepath.Join(s.repo, "library")
	if _, err := os.Stat(filepath.Join(library, ".next", "BUILD_ID")); err != nil {
		t.Fatalf("the library is not built: run make e2e, which builds it first (%v)", err)
	}
	env := s.env(
		"PORT="+strconv.Itoa(s.libraryPort),
		"HOSTNAME=127.0.0.1",
		"NEXT_TELEMETRY_DISABLED=1",
		"VIDYOM_PUBLIC_DB="+s.path("public", "acme", "public.db"),
		"VIDYOM_LIBRARY_TITLE=Acme video library",
	)
	s.procs["library"] = start(t, s.path("logs"), "library", env, library, "npm", "start")
	waitFor(t, 60*time.Second, "the library server", func() (bool, string) {
		if !s.procs["library"].running() {
			t.Fatalf("the library exited: %v\n%s", s.procs["library"].err, tail(s.path("logs", "library.log"), 30))
		}
		code, _, err := plainGet(s.LibraryURL() + "/api/health")
		return err == nil && code > 0, fmt.Sprint(code, err)
	})
}

func (s *nativeStack) tryRedis(args ...string) string {
	cli := strings.Replace(s.redisServer(), "redis-server", "redis-cli", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stdout, stderr, code, err := run(ctx, s.env(), s.root, append([]string{cli, "-p", strconv.Itoa(s.redisPort)}, args...)...)
	if err != nil || code != 0 {
		return fmt.Sprintf("error: %v %s", err, stderr)
	}
	return stdout
}

func (s *nativeStack) redisServer() string {
	if s.redis != "" {
		return s.redis
	}
	if path, err := exec.LookPath("redis-server"); err == nil {
		return path
	}
	return "redis-server"
}

func (s *nativeStack) Redis(t *testing.T, args ...string) string {
	t.Helper()
	out := s.tryRedis(args...)
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("redis %v: %s", args, out)
	}
	return out
}

func (s *nativeStack) HTTPSURL() string { return fmt.Sprintf("https://127.0.0.1:%d", s.httpsPort) }

func (s *nativeStack) LibraryURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.libraryPort)
}

func (s *nativeStack) Fetch(t *testing.T, rel string) string {
	t.Helper()
	path := s.path(rel)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fetch %s: %v", rel, err)
	}
	return path
}

func (s *nativeStack) Exists(t *testing.T, rel string) bool {
	t.Helper()
	_, err := os.Stat(s.path(rel))
	return err == nil
}

func (s *nativeStack) SQL(t *testing.T, rel, statements string) string {
	t.Helper()
	return must(t, s.env(), s.root, "sqlite3", s.path(rel), statements)
}

func (s *nativeStack) InstallPlugin(t *testing.T, built string) string {
	t.Helper()
	target := s.path("plugins", filepath.Base(built))
	copyTree(t, built, target)
	return target
}

func (s *nativeStack) PlaceIngest(t *testing.T, local string) string {
	t.Helper()
	target := s.path("ingest", filepath.Base(local))
	data, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return target
}

func (s *nativeStack) WriteTenantConfig(t *testing.T, tenant, name, content string) {
	t.Helper()
	dir := s.path("config", "tenants", tenant)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := s.procs["vidyomd"]; p != nil && p.running() {
		if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (s *nativeStack) Dump(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(s.path("logs"))
	for _, entry := range entries {
		t.Logf("--- %s\n%s", entry.Name(), tail(s.path("logs", entry.Name()), 40))
	}
	t.Logf("--- data/logs/vidyomd.jsonl\n%s", tail(s.path("data", "logs", "vidyomd.jsonl"), 40))
}

func (s *nativeStack) Down(t *testing.T) {
	t.Helper()
	for _, name := range []string{"library", "worker-media", "worker-ai", "vidyomd", "ai-python", "mock-llm", "redis"} {
		if p := s.procs[name]; p != nil {
			p.stop(syscall.SIGTERM, 40*time.Second)
		}
	}
	if os.Getenv("VIDYOM_E2E_KEEP") != "" {
		t.Logf("kept %s", s.root)
		return
	}
	// The plugin store is read-only on purpose.
	_ = filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(s.root); err != nil {
		t.Logf("remove %s: %v", s.root, err)
	}
}
