package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/connectors/storage/local"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg/fixture"
	videonodes "github.com/rafaelp/vidyom/nodes/video"
)

// oneTaskQueue delivers one pointer, then nothing.
type oneTaskQueue struct {
	mu      sync.Mutex
	pointer *jobqueue.Pointer
	acked   chan string
}

func (q *oneTaskQueue) Read(ctx context.Context, opts jobqueue.ReadOptions) ([]jobqueue.Delivery, error) {
	q.mu.Lock()
	p := q.pointer
	q.pointer = nil
	q.mu.Unlock()
	if p != nil {
		return []jobqueue.Delivery{{Pointer: *p, StreamID: "1-0"}}, nil
	}
	select {
	case <-time.After(opts.Block):
	case <-ctx.Done():
	}
	return nil, ctx.Err()
}

func (q *oneTaskQueue) Ack(_ context.Context, _ node.Pool, _, taskID string) error {
	q.acked <- taskID
	return nil
}

func (q *oneTaskQueue) Enqueue(context.Context, jobqueue.Pointer) error { return nil }
func (q *oneTaskQueue) Inflight(context.Context, string) (bool, error)  { return false, nil }
func (q *oneTaskQueue) ClaimStale(context.Context, node.Pool, string, time.Duration, int) ([]jobqueue.Delivery, error) {
	return nil, nil
}
func (q *oneTaskQueue) Depth(context.Context, node.Pool) (jobqueue.Depth, error) {
	return jobqueue.Depth{}, nil
}
func (q *oneTaskQueue) Ping(context.Context) error { return nil }
func (q *oneTaskQueue) Close() error               { return nil }

// TestRunServesATask drives the command itself: flags, token file, seams, a daemon
// over a Unix socket, a real video.probe, then a graceful stop on SIGTERM.
func TestRunServesATask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix socket or SIGTERM here")
	}
	fixture.Require(t)
	clearSeams(t)

	root := t.TempDir()
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 1})
	job := id.New()
	taskID := jobqueue.TaskID(job, "probe", 1)
	params, _ := json.Marshal(map[string]any{"source": "server:" + raw})

	var mu sync.Mutex
	var registered workerproto.RegisterRequest
	completed := make(chan workerproto.CompleteRequest, 1)
	mux := http.NewServeMux()
	route := func(name string, handler func(r *http.Request, body []byte) any) {
		for _, candidate := range api.Routes {
			if candidate.Name == name {
				mux.HandleFunc(candidate.Method+" "+api.Prefix+candidate.Path, func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer fictional-token" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					body, _ := io.ReadAll(r.Body)
					_ = json.NewEncoder(w).Encode(handler(r, body))
				})
			}
		}
	}
	route(api.RouteWorkerRegister, func(_ *http.Request, body []byte) any {
		mu.Lock()
		defer mu.Unlock()
		_ = json.Unmarshal(body, &registered)
		return workerproto.RegisterResponse{HeartbeatSeconds: 1, ProtocolVersion: workerproto.ProtocolVersion}
	})
	route(api.RouteWorkerHeartbeat, func(*http.Request, []byte) any { return struct{}{} })
	route(api.RouteWorkerTaskHeartbeat, func(*http.Request, []byte) any { return workerproto.TaskHeartbeatResponse{} })
	route(api.RouteWorkerTaskLogs, func(*http.Request, []byte) any { return struct{}{} })
	route(api.RouteWorkerLease, func(*http.Request, []byte) any {
		return workerproto.Lease{
			TaskID: taskID, LeaseID: "l", ExpiresAt: time.Now().Add(time.Minute), TenantID: "acme", JobID: "JOB-001",
			JobULID: job, NodeID: "probe", NodeType: "video.probe", TypeVersion: 1, Attempt: 1, Parameters: params,
		}
	})
	route(api.RouteWorkerComplete, func(_ *http.Request, body []byte) any {
		var req workerproto.CompleteRequest
		_ = json.Unmarshal(body, &req)
		completed <- req
		return struct{}{}
	})
	socketDir, err := os.MkdirTemp("", "vwm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	_ = srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	queue := &oneTaskQueue{
		pointer: &jobqueue.Pointer{TaskID: taskID, TenantID: "acme", Pool: node.PoolMedia, NodeType: "video.probe"},
		acked:   make(chan string, 1),
	}
	var gotRedis config.Redis
	wireRegistries = func(n node.Registrar, c connector.Registrar) error {
		videonodes.Register(n)
		local.Register(c)
		return nil
	}
	wireQueue = func(_ context.Context, redis config.Redis) (jobqueue.Queue, func(), error) {
		gotRedis = redis
		return queue, func() {}, nil
	}

	daemonJSON, _ := json.Marshal(config.Daemon{
		ScratchDir:  filepath.Join(root, "scratch"),
		CacheDir:    filepath.Join(root, "cache"),
		SpoolDir:    filepath.Join(root, "spool"),
		IngestRoots: []string{root},
		Redis:       config.Redis{URL: "redis://127.0.0.1:6400"},
		Listeners:   config.Listeners{Unix: &config.UnixListener{Path: socket}},
	})
	configPath := filepath.Join(root, "daemon.json")
	if err := os.WriteFile(configPath, daemonJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(root, "token")
	if err := os.WriteFile(tokenPath, []byte("fictional-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	exit := make(chan int, 1)
	go func() {
		exit <- run([]string{"--config", configPath, "--token-file", tokenPath, "--pools", "media", "--worker-id", "w1", "--shutdown-grace", "5s"}, &stderr)
	}()

	select {
	case req := <-completed:
		var out map[string]any
		if err := json.Unmarshal(req.Output, &out); err != nil {
			t.Fatal(err)
		}
		if out["video_codec"] != "h264" {
			t.Fatalf("%+v", out)
		}
	case code := <-exit:
		t.Fatalf("exited with %d: %s", code, stderr.String())
	case <-time.After(60 * time.Second):
		t.Fatal("the task did not complete")
	}
	select {
	case got := <-queue.acked:
		if got != taskID {
			t.Fatalf("acked %s", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no ack")
	}
	mu.Lock()
	if registered.WorkerID != "w1" || registered.Resources.CPUCores == 0 {
		t.Errorf("%+v", registered)
	}
	mu.Unlock()
	if gotRedis.URL != "redis://127.0.0.1:6400" {
		t.Errorf("redis %+v", gotRedis)
	}

	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the worker did not stop on SIGTERM")
	}
}
