//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// composeProject is what the suite runs under, away from any real deployment.
const composeProject = "vidyom-dev-integration"

// composeStack runs the same scenario against deploy/docker-compose.yml: the images are
// what ships, the CLI stays on this host and speaks HTTPS with a pinned certificate, as a
// person's would (D10, D41, D49).
type composeStack struct {
	repo   string
	tmp    string
	config string
	key    string

	httpsPort, libraryPort int
}

func newCompose(t *testing.T) stack {
	t.Helper()
	tmp, err := os.MkdirTemp("", "vdmcompose")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	return &composeStack{
		repo:        repoRoot(t),
		tmp:         tmp,
		config:      filepath.Join(tmp, "config"),
		key:         filepath.Join(tmp, "master.key"),
		httpsPort:   freePort(t),
		libraryPort: freePort(t),
	}
}

func (s *composeStack) Name() string { return "compose" }

func (s *composeStack) env() []string {
	return append(os.Environ(),
		"COMPOSE_PROJECT_NAME="+composeProject,
		"VIDYOM_CONFIG_SOURCE="+s.config,
		"VIDYOM_MASTER_KEY_FILE="+s.key,
		fmt.Sprintf("VIDYOM_HTTPS_PORT=%d", s.httpsPort),
		fmt.Sprintf("VIDYOM_LIBRARY_PORT=%d", s.libraryPort),
	)
}

func (s *composeStack) argv(args ...string) []string {
	return append([]string{"docker", "compose", "-f", filepath.Join(s.repo, "deploy", "docker-compose.yml"), "--profile", "demo"}, args...)
}

func (s *composeStack) compose(t *testing.T, args ...string) string {
	t.Helper()
	return must(t, s.env(), s.repo, s.argv(args...)...)
}

func (s *composeStack) try(args ...string) (string, string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return run(ctx, s.env(), s.repo, s.argv(args...)...)
}

func (s *composeStack) Up(t *testing.T) {
	t.Helper()
	copyTree(t, filepath.Join(s.repo, "config", "compose"), s.config)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.key, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	// Anything left by an earlier run under this name goes first.
	s.compose(t, "down", "--volumes", "--remove-orphans")
	s.compose(t, "build")
	s.compose(t, "up", "--detach", "--wait", "redis")
}

func (s *composeStack) Init(t *testing.T) initResult {
	t.Helper()
	out := s.compose(t, "run", "--rm", "-T", "--no-deps", "daemon", "init", "--config-dir", "/etc/vidyom",
		"--tenant", "acme", "--admin", "admin@example.com",
		"--worker-token-file", "/var/lib/vidyom/worker-token/worker.token", "--json")
	var result initResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode init output %q: %v", out, err)
	}
	return result
}

func (s *composeStack) Refused(t *testing.T, variant string) (int, string) {
	t.Helper()
	stdout, stderr, code, err := s.try("run", "--rm", "-T", "--no-deps", "daemon", "start", "--config-dir", "/etc/vidyom/"+variant)
	if err != nil {
		t.Fatalf("run the daemon: %v", err)
	}
	return code, stdout + stderr
}

func (s *composeStack) StartDaemon(t *testing.T) { s.compose(t, "up", "--detach", "daemon") }

func (s *composeStack) KillDaemon(t *testing.T) { s.compose(t, "kill", "--signal", "SIGKILL", "daemon") }

func (s *composeStack) StartMockLLM(t *testing.T) { s.compose(t, "up", "--detach", "mock-llm") }

func (s *composeStack) StartAIWorker(t *testing.T) {
	t.Helper()
	s.compose(t, "up", "--detach", "ai-engine")
}

func (s *composeStack) StartWorkers(t *testing.T) {
	t.Helper()
	s.compose(t, "up", "--detach", "--no-deps", "worker-media", "worker-ai")
}

func (s *composeStack) StopWorkers(t *testing.T) {
	t.Helper()
	s.compose(t, "stop", "worker-media", "worker-ai")
}

func (s *composeStack) StartLibrary(t *testing.T) {
	t.Helper()
	s.compose(t, "up", "--detach", "library")
	waitFor(t, 60*time.Second, "the library server", func() (bool, string) {
		code, _, err := plainGet(s.LibraryURL() + "/api/health")
		return err == nil && code > 0, fmt.Sprint(code, err)
	})
}

func (s *composeStack) Redis(t *testing.T, args ...string) string {
	t.Helper()
	return s.compose(t, append([]string{"exec", "-T", "redis", "redis-cli"}, args...)...)
}

func (s *composeStack) HTTPSURL() string { return fmt.Sprintf("https://127.0.0.1:%d", s.httpsPort) }

func (s *composeStack) LibraryURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.libraryPort)
}

// inside maps a path under the stack's root to where the daemon's container has it.
func inside(rel string) string { return "/var/lib/vidyom/" + filepath.ToSlash(rel) }

func (s *composeStack) Fetch(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.MkdirTemp(s.tmp, "fetch")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, filepath.Base(rel))
	s.compose(t, "cp", "daemon:"+inside(rel), target)
	return target
}

func (s *composeStack) Exists(t *testing.T, rel string) bool {
	t.Helper()
	_, _, code, err := s.try("exec", "-T", "daemon", "test", "-e", inside(rel))
	if err != nil {
		t.Fatal(err)
	}
	return code == 0
}

func (s *composeStack) SQL(t *testing.T, rel, statements string) string {
	t.Helper()
	return s.compose(t, "exec", "-T", "daemon", "sqlite3", inside(rel), statements)
}

func (s *composeStack) PlaceIngest(t *testing.T, local string) string {
	t.Helper()
	if err := os.Chmod(local, 0o644); err != nil {
		t.Fatal(err)
	}
	target := inside(filepath.Join("ingest", filepath.Base(local)))
	s.compose(t, "cp", local, "daemon:"+target)
	return target
}

func (s *composeStack) InstallPlugin(t *testing.T, built string) string {
	t.Helper()
	target := inside(filepath.Join("plugins", filepath.Base(built)))
	s.compose(t, "cp", built, "daemon:"+target)
	return target
}

func (s *composeStack) WriteTenantConfig(t *testing.T, tenant, name, content string) {
	t.Helper()
	dir := filepath.Join(s.config, "tenants", tenant)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s.compose(t, "kill", "--signal", "SIGHUP", "daemon")
	time.Sleep(time.Second)
}

func (s *composeStack) Dump(t *testing.T) {
	t.Helper()
	stdout, _, _, _ := s.try("ps", "--all")
	t.Logf("--- docker compose ps\n%s", stdout)
	stdout, _, _, _ = s.try("logs", "--no-color", "--tail", "40")
	t.Logf("--- docker compose logs\n%s", stdout)
}

func (s *composeStack) Down(t *testing.T) {
	t.Helper()
	if os.Getenv("VIDYOM_E2E_KEEP") != "" {
		t.Logf("kept project %s and %s", composeProject, s.tmp)
		return
	}
	if _, stderr, code, err := s.try("down", "--volumes", "--remove-orphans"); err != nil || code != 0 {
		t.Logf("compose down: exit %d %v %s", code, err, stderr)
	}
	_ = os.RemoveAll(s.tmp)
}

func dockerArch(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(must(t, os.Environ(), "", "docker", "version", "--format", "{{.Server.Arch}}"))
}
