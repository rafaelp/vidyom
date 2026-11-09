package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apiserver "github.com/rafaelp/vidyom/internal/api"
	"github.com/rafaelp/vidyom/internal/api/fakestore"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

// tempDir is short enough for a socket path, which t.TempDir may not be on macOS.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vd")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type fakeProvider struct {
	key     []byte
	err     error
	sealing security.SealingLevel
	mu      sync.Mutex
	calls   int
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) Sealing() security.SealingLevel {
	if p.sealing == "" {
		return security.SealingOS
	}
	return p.sealing
}

func (p *fakeProvider) Unseal(context.Context) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return append([]byte(nil), p.key...), nil
}

func goodProvider() *fakeProvider { return &fakeProvider{key: make([]byte, masterKeySize)} }

type fakeAuthn struct {
	mu       sync.Mutex
	peerUIDs []int
}

func (a *fakeAuthn) Authenticate(_ context.Context, c security.Credentials) (domain.Principal, error) {
	if c.PeerUID != nil {
		a.mu.Lock()
		a.peerUIDs = append(a.peerUIDs, *c.PeerUID)
		a.mu.Unlock()
		if *c.PeerUID == os.Getuid() {
			return domain.Principal{Email: "root@example.com", SystemAdmin: true, Method: domain.AuthPeer}, nil
		}
	}
	if c.BearerToken == "good-token" {
		return domain.Principal{Email: "root@example.com", SystemAdmin: true, Method: domain.AuthDevice}, nil
	}
	return domain.Principal{}, errs.New(errs.CodeUnauthenticated, "no identity")
}

func (a *fakeAuthn) AuthenticateWorker(context.Context, security.Credentials) (string, error) {
	return "", errs.New(errs.CodeUnauthenticated, "no")
}

type fakeAuthz struct{}

func (fakeAuthz) Resolve(_ context.Context, p domain.Principal, tenant domain.TenantID) (domain.Principal, error) {
	p.TenantID, p.Role = tenant, domain.RoleAdmin
	return p, nil
}

func (fakeAuthz) Require(_ context.Context, p domain.Principal, perm domain.Permission) error {
	if p.Can(perm) {
		return nil
	}
	return errs.Forbidden(string(perm))
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

func testDeps(authn *fakeAuthn) Deps {
	return Deps{
		API: apiserver.Deps{
			Authenticator: authn,
			Authorizer:    fakeAuthz{},
			Auditor:       &fakeAuditor{},
			TenantStores:  fakestore.New("acme"),
		},
		MasterKey: goodProvider(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func baseConfig(t *testing.T) config.Daemon {
	t.Helper()
	dir := tempDir(t)
	cfg := config.Daemon{
		DataDir: filepath.Join(dir, "data"),
		Redis:   config.Redis{URL: "redis://127.0.0.1:1"},
		Vault:   config.Vault{Unseal: config.Unseal{Provider: config.UnsealEnv}},
		Listeners: config.Listeners{
			Unix: &config.UnixListener{Path: filepath.Join(dir, "run", "d.sock")},
		},
	}
	if err := ApplyDefaults(&cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// running is a daemon under test. done closes once Run has returned err.
type running struct {
	infos  []ListenerInfo
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
		return r.err
	case <-time.After(45 * time.Second):
		t.Fatal("the daemon did not stop")
		return nil
	}
}

// start runs the daemon and waits until it reports ready, or fails the test if it
// exits first. Use mustNotStart for a daemon that must refuse.
func start(t *testing.T, cfg config.Daemon, deps Deps) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan []ListenerInfo, 1)
	deps.OnReady = func(infos []ListenerInfo) { ready <- infos }
	r := &running{cancel: cancel, done: make(chan struct{})}
	go func() {
		r.err = Run(ctx, cfg, deps)
		close(r.done)
	}()
	select {
	case r.infos = <-ready:
		t.Cleanup(func() {
			cancel()
			select {
			case <-r.done:
			case <-time.After(45 * time.Second):
			}
		})
		return r
	case <-r.done:
		cancel()
		t.Fatalf("the daemon exited: %v", r.err)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("the daemon did not become ready")
	}
	return nil
}

// mustNotStart runs the daemon, which must return an error without ever being ready.
func mustNotStart(t *testing.T, cfg config.Daemon, deps Deps) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	readied := make(chan struct{}, 1)
	deps.OnReady = func([]ListenerInfo) { readied <- struct{}{} }
	err := Run(ctx, cfg, deps)
	select {
	case <-readied:
		t.Fatalf("the daemon became ready, and returned: %v", err)
	default:
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	return err
}

func unixClient(path string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}

func socketGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket %s exists: %v", path, err)
	}
}
