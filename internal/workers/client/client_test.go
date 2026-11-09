package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/workers/client"
)

const token = "fictional-worker-token"

func unixServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "vw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	_ = srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return "unix://" + path
}

func TestEveryCoordinatorCallUsesItsRoute(t *testing.T) {
	seen := map[string]bool{}
	daemon := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("token not sent: %q", r.Header.Get("Authorization"))
		}
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		seen[r.URL.Path] = true
		_, _ = w.Write([]byte(`{"task_id":"t1","heartbeat_seconds":7,"key":"AAEC"}`))
	}))
	c, err := client.New(client.Options{Daemon: daemon, Token: []byte(token + "\n")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	reg, err := c.Register(ctx, workerproto.RegisterRequest{WorkerID: "w"})
	if err != nil || reg.HeartbeatSeconds != 7 {
		t.Fatalf("%+v %v", reg, err)
	}
	if err := c.Heartbeat(ctx, workerproto.HeartbeatRequest{}); err != nil {
		t.Fatal(err)
	}
	lease, err := c.Lease(ctx, "J.n.1", workerproto.LeaseRequest{})
	if err != nil || lease.TaskID != "t1" {
		t.Fatalf("%+v %v", lease, err)
	}
	if _, err := c.TaskHeartbeat(ctx, "J.n.1", workerproto.TaskHeartbeatRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := c.TaskLogs(ctx, "J.n.1", workerproto.TaskLogsRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(ctx, "J.n.1", workerproto.CompleteRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Fail(ctx, "J.n.1", workerproto.FailRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Connection(ctx, "J.n.1", workerproto.ConnectionRequest{}); err != nil {
		t.Fatal(err)
	}
	key, err := c.CacheKey(ctx, "J.n.1", workerproto.CacheKeyRequest{})
	if err != nil || len(key.Key) != 3 {
		t.Fatalf("%+v %v", key, err)
	}
	for _, r := range api.Routes {
		if r.Auth != api.AuthWorker {
			continue
		}
		path := api.Prefix + strings.ReplaceAll(r.Path, "{id}", "J.n.1")
		if !seen[path] {
			t.Errorf("route %s (%s) was never called", r.Name, path)
		}
	}
}

func TestErrorsKeepTheirCodes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   errs.Code
	}{
		{http.StatusGone, `{"error":{"code":"gone","message":"obsolete"}}`, errs.CodeGone},
		{http.StatusConflict, `{"error":{"code":"conflict","message":"elsewhere"}}`, errs.CodeConflict},
		{http.StatusGone, `not json`, errs.CodeGone},
		{http.StatusConflict, ``, errs.CodeConflict},
		{http.StatusUnauthorized, ``, errs.CodeUnauthenticated},
		{http.StatusBadGateway, `<html>`, errs.CodeUnavailable},
	}
	for _, c := range cases {
		daemon := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		cl, err := client.New(client.Options{Daemon: daemon, Token: []byte(token)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = cl.Lease(context.Background(), "x", workerproto.LeaseRequest{})
		if !errs.IsCode(err, c.want) {
			t.Errorf("%d %q: %v, want %s", c.status, c.body, err, c.want)
		}
	}
}

func TestUnreachableDaemonIsUnavailable(t *testing.T) {
	dir, err := os.MkdirTemp("", "vw")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	cl, err := client.New(client.Options{Daemon: "unix://" + filepath.Join(dir, "absent.sock"), Token: []byte(token)})
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Heartbeat(context.Background(), workerproto.HeartbeatRequest{}); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cl.Heartbeat(ctx, workerproto.HeartbeatRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestNewRefusesUnsafeOptions(t *testing.T) {
	cases := map[string]client.Options{
		"no token":        {Daemon: "unix:///x.sock"},
		"plain http":      {Daemon: "http://media.example.internal:9000", Token: []byte(token)},
		"relative socket": {Daemon: "unix://x.sock", Token: []byte(token)},
		"bad pin":         {Daemon: "https://media.example.internal", Token: []byte(token), TLSFingerprint: "md5:abc"},
		"short pin":       {Daemon: "https://media.example.internal", Token: []byte(token), TLSFingerprint: "sha256:abc"},
		"missing CA file": {Daemon: "https://media.example.internal", Token: []byte(token), TLSCAFile: "/no/such/ca.pem"},
	}
	for name, opts := range cases {
		if _, err := client.New(opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFingerprintPinning(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(workerproto.RegisterResponse{HeartbeatSeconds: 3})
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	good, err := client.New(client.Options{Daemon: srv.URL, Token: []byte(token), TLSFingerprint: "sha256:" + hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal(err)
	}
	if reg, err := good.Register(context.Background(), workerproto.RegisterRequest{}); err != nil || reg.HeartbeatSeconds != 3 {
		t.Fatalf("%+v %v", reg, err)
	}
	wrong := sha256.Sum256([]byte("another certificate"))
	bad, err := client.New(client.Options{Daemon: srv.URL, Token: []byte(token), TLSFingerprint: "sha256:" + hex.EncodeToString(wrong[:])})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Register(context.Background(), workerproto.RegisterRequest{}); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("a wrong pin connected: %v", err)
	}
	unpinned, err := client.New(client.Options{Daemon: srv.URL, Token: []byte(token)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unpinned.Register(context.Background(), workerproto.RegisterRequest{}); err == nil {
		t.Fatal("an untrusted certificate connected")
	}
}

func TestTokenDoesNotFollowRedirects(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked = true
		}
	}))
	defer target.Close()
	daemon := unixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	cl, err := client.New(client.Options{Daemon: daemon, Token: []byte(token)})
	if err != nil {
		t.Fatal(err)
	}
	_ = cl.Heartbeat(context.Background(), workerproto.HeartbeatRequest{})
	if leaked {
		t.Fatal("the token followed a redirect")
	}
}
