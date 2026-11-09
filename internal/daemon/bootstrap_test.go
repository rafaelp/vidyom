package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
)

func fakeBootstrap(calls *int) BootstrapFunc {
	return func(_ context.Context, req BootstrapRequest) (BootstrapResult, error) {
		*calls++
		return BootstrapResult{PairingCode: "ABCDE-FGHIJ", PairingExpiresAt: time.Now().Add(time.Hour), WorkerToken: "vdw_fictional"}, nil
	}
}

func initConfig(t *testing.T) config.Daemon {
	t.Helper()
	cfg := baseConfig(t)
	cfg.Listeners.HTTPS = &config.HTTPSListener{Bind: "127.0.0.1", Port: 8443, TLS: config.TLS{Mode: config.TLSAuto}}
	return cfg
}

func TestInit(t *testing.T) {
	cfg := initConfig(t)
	calls := 0
	out, err := Init(context.Background(), cfg, fakeBootstrap(&calls), InitOptions{Tenant: "acme", AdminEmail: "admin@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if out.PairingCode != "ABCDE-FGHIJ" || out.Tenant != "acme" || out.Admin != "admin@example.com" {
		t.Fatalf("result = %+v", out)
	}
	if out.WorkerTokenFile != DefaultWorkerTokenFile(cfg) {
		t.Fatalf("token file = %s", out.WorkerTokenFile)
	}
	data, err := os.ReadFile(out.WorkerTokenFile)
	if err != nil || strings.TrimSpace(string(data)) != "vdw_fictional" {
		t.Fatalf("token: %q %v", data, err)
	}
	info, err := os.Stat(out.WorkerTokenFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode: %v %v", info, err)
	}
	if !strings.HasPrefix(out.TLSFingerprint, "sha256:") {
		t.Fatalf("fingerprint = %q", out.TLSFingerprint)
	}

	// The pin init prints is the one the daemon then serves.
	_, served, err := ensureAutoCert(cfg.DataDir, nil)
	if err != nil || served != out.TLSFingerprint {
		t.Fatalf("served %q, printed %q, %v", served, out.TLSFingerprint, err)
	}
}

func TestInitRefusesAnInitializedDirectory(t *testing.T) {
	cfg := baseConfig(t)
	calls := 0
	opts := InitOptions{Tenant: "acme", AdminEmail: "admin@example.com"}
	if _, err := Init(context.Background(), cfg, fakeBootstrap(&calls), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(context.Background(), cfg, fakeBootstrap(&calls), opts); err == nil || !strings.Contains(err.Error(), "initialized") {
		t.Fatalf("second init: %v", err)
	}
	if calls != 1 {
		t.Fatalf("bootstrap ran %d times", calls)
	}
}

func TestFailedBootstrapLeavesNoTokenFile(t *testing.T) {
	cfg := baseConfig(t)
	_, err := Init(context.Background(), cfg, func(context.Context, BootstrapRequest) (BootstrapResult, error) {
		return BootstrapResult{}, errors.New("a tenant already exists")
	}, InitOptions{Tenant: "acme", AdminEmail: "admin@example.com"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(DefaultWorkerTokenFile(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file kept: %v", err)
	}

	// So it can simply be repeated.
	calls := 0
	if _, err := Init(context.Background(), cfg, fakeBootstrap(&calls), InitOptions{Tenant: "acme", AdminEmail: "admin@example.com"}); err != nil {
		t.Fatal(err)
	}
}

func TestInitValidates(t *testing.T) {
	cfg := baseConfig(t)
	calls := 0
	for name, opts := range map[string]InitOptions{
		"bad tenant": {Tenant: "../evil", AdminEmail: "a@example.com"},
		"no admin":   {Tenant: "acme"},
	} {
		if _, err := Init(context.Background(), cfg, fakeBootstrap(&calls), opts); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := Init(context.Background(), cfg, nil, InitOptions{Tenant: "acme", AdminEmail: "a@example.com"}); err == nil {
		t.Error("no bootstrap accepted")
	}
	if calls != 0 {
		t.Errorf("bootstrap ran %d times", calls)
	}
}

func TestInitCustomTokenFile(t *testing.T) {
	cfg := baseConfig(t)
	path := filepath.Join(tempDir(t), "secrets", "worker")
	calls := 0
	out, err := Init(context.Background(), cfg, fakeBootstrap(&calls), InitOptions{Tenant: "acme", AdminEmail: "a@example.com", WorkerTokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if out.WorkerTokenFile != path {
		t.Fatalf("token file = %s", out.WorkerTokenFile)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestInitRefusesARunningDaemon(t *testing.T) {
	cfg := baseConfig(t)
	r := start(t, cfg, testDeps(&fakeAuthn{}))
	defer r.stop(t)
	calls := 0
	_, err := Init(context.Background(), cfg, fakeBootstrap(&calls), InitOptions{Tenant: "acme", AdminEmail: "a@example.com"})
	if err == nil || !strings.Contains(err.Error(), "another vidyomd") {
		t.Fatalf("error = %v", err)
	}
	if calls != 0 {
		t.Fatal("bootstrap ran beside a running daemon")
	}
}

func TestGenerateMasterKey(t *testing.T) {
	path := filepath.Join(tempDir(t), "keys", "master")
	if err := GenerateMasterKey(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("mode: %v %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != 32 {
		t.Fatalf("key: %d bytes, %v", len(key), err)
	}

	// Never overwrites: losing a master key loses every tenant's secrets.
	if err := GenerateMasterKey(path); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("second generation: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("the key changed")
	}
}
