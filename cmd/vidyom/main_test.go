package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/apiclient/apitest"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// forbiddenDeps must match the Makefile's FORBIDDEN_DEPS.
var forbiddenDeps = []string{"rqlite", "sqlite-vec", "internal/persistence"}

func build(t *testing.T, tags ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "vidyom")
	args := []string{"build", "-trimpath", "-o", bin}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	cmd := exec.Command("go", append(args, ".")...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build with CGO_ENABLED=0: %v\n%s", err, out)
	}
	return bin
}

func runBinary(t *testing.T, bin string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(append(os.Environ(), "VIDYOM_CONFIG_DIR="+t.TempDir()), env...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatal(err)
	}
	return outBuf.String(), errBuf.String(), code
}

func TestDependenciesAreFreeOfDatabasePackages(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, forbidden := range forbiddenDeps {
			if strings.Contains(line, forbidden) {
				t.Errorf("forbidden dependency: %s", line)
			}
		}
	}
	if !strings.Contains(string(out), "internal/security/keyring") {
		t.Error("the default build must use the OS keyring")
	}
}

func TestDefaultBinaryHasNoTestKeyring(t *testing.T) {
	raw, err := os.ReadFile(build(t))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("VIDYOM_TEST_KEYRING_FILE")) {
		t.Fatal("the plaintext test keyring must not be compiled into a normal build")
	}
}

func TestBinaryAgainstARealSocket(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteHealth, 200, api.HealthResponse{Status: "ok", Version: "1.2.3", Sealing: "file"})
	socket := srv.ServeUnix()
	bin := build(t)
	env := []string{"VIDYOM_SOCKET=" + socket}

	stdout, stderr, code := runBinary(t, bin, env, "daemon", "health", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var health api.HealthResponse
	if err := json.Unmarshal([]byte(stdout), &health); err != nil || health.Sealing != "file" {
		t.Fatalf("health = %s, %v", stdout, err)
	}

	_, stderr, code = runBinary(t, bin, []string{"VIDYOM_SOCKET=" + filepath.Join(t.TempDir(), "absent.sock")}, "daemon", "health")
	if code != 1 || !strings.Contains(stderr, "cannot reach the daemon") {
		t.Fatalf("exit %d: %s", code, stderr)
	}

	_, _, code = runBinary(t, bin, nil, "nonsense")
	if code != 2 {
		t.Fatalf("usage error: exit %d", code)
	}
	stdout, _, code = runBinary(t, bin, nil, "--version")
	if code != 0 || !strings.Contains(stdout, "vidyom") {
		t.Fatalf("--version: exit %d: %s", code, stdout)
	}
}

// A real SIGINT on `job attach`: the process disconnects and the job is untouched.
func TestInterruptOnAttachOnlyDisconnects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses SIGINT")
	}
	srv := apitest.New(t)
	srv.JSON(api.RouteJobsGet, 200, api.JobDetail{Job: domain.Job{ID: "JOB-001", WorkflowID: "create-clip", Status: domain.JobRunning}})
	connected := make(chan struct{}, 1)
	srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, r *http.Request) {
		apitest.WriteSSEHeaders(w)
		apitest.WriteSSE(w, apitest.SSEFrame{ID: "1", Name: api.SSEJobEvent, Data: domain.JobEvent{Seq: 1, JobID: "JOB-001", Kind: domain.JobEventLog, Message: "working"}})
		connected <- struct{}{}
		<-r.Context().Done()
	})
	socket := srv.ServeUnix()

	cmd := exec.Command(build(t), "job", "attach", "JOB-001", "--tenant", "acme")
	cmd.Env = append(os.Environ(), "VIDYOM_CONFIG_DIR="+t.TempDir(), "VIDYOM_SOCKET="+socket)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("never attached: %s", stderr.String())
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("exit = %v, stderr: %s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "keeps running") {
		t.Fatalf("stderr: %s", stderr.String())
	}
	if mutations := srv.Mutations(); len(mutations) != 0 {
		t.Fatalf("Ctrl+C changed the job: %+v", mutations)
	}
}

// The tagged build is what E2E uses so it never writes to a person's OS keyring (I168).
func TestTestKeyringBuild(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteDevicesPair, 201, api.DeviceCredentialResponse{
		Device: domain.Device{ID: "CLIENT-001", Email: "editor@example.com"},
		Token:  "vdm_fictional_credential",
	})
	socket := srv.ServeUnix()
	bin := build(t, "vidyom_test_keyring")
	configDir := t.TempDir()
	keyringFile := filepath.Join(t.TempDir(), "keyring.json")
	env := []string{"VIDYOM_SOCKET=" + socket, "VIDYOM_CONFIG_DIR=" + configDir}

	// Without the variable there is no keyring at all: the fail-closed path.
	_, stderr, code := runBinary(t, bin, env, "server", "pair", "local", "--code", "CODE")
	if code != 1 || !strings.Contains(stderr, "no usable OS keyring") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(srv.Calls(api.RouteDevicesPair)) != 0 {
		t.Fatal("the code must not be spent")
	}

	stdout, stderr, code := runBinary(t, bin, append(env, "VIDYOM_TEST_KEYRING_FILE="+keyringFile), "server", "pair", "local", "--code", "CODE")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "plaintext") {
		t.Fatalf("the test build must warn on every use: %s", stderr)
	}
	if strings.Contains(stdout+stderr, "vdm_fictional_credential") {
		t.Fatal("the credential was printed")
	}
	raw, err := os.ReadFile(keyringFile)
	if err != nil || !strings.Contains(string(raw), "vdm_fictional_credential") {
		t.Fatalf("keyring file: %v", err)
	}
	info, err := os.Stat(keyringFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("keyring file mode: %v, %v", info.Mode(), err)
	}
	config, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil || strings.Contains(string(config), "vdm_fictional_credential") {
		t.Fatalf("config: %v", err)
	}
}
