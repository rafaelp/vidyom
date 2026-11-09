package workers_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/workers/client"
)

const testToken = "fictional-worker-token"

// task is one thing the fake daemon can lease.
type task struct {
	lease workerproto.Lease
	// leaseErr answers the lease instead of granting it.
	leaseErr error
	// heartbeat scripts the answers to task heartbeats.
	cancel         bool
	heartbeatErr   error
	completeErrors []error // consumed one per call
	failErrors     []error
}

// fakeDaemon stands in for vidyomd, through the worker API over real HTTP.
type fakeDaemon struct {
	t        *testing.T
	mu       sync.Mutex
	tasks    map[string]*task
	socket   string
	instance config.StorageInstance
	roles    map[domain.StorageRole]config.StorageInstance
	creds    []byte
	key      []byte

	registers  []workerproto.RegisterRequest
	heartbeats []workerproto.HeartbeatRequest
	leases     []string
	taskBeats  map[string]int
	lastBeat   map[string]workerproto.TaskHeartbeatRequest
	logs       map[string][]workerproto.LogLine
	completed  map[string]workerproto.CompleteRequest
	failed     map[string]workerproto.FailRequest
	calls      map[string]int

	registerErr  error
	registerResp workerproto.RegisterResponse
	down         bool
	done         chan string
	finished     map[string]bool
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{
		t:            t,
		tasks:        map[string]*task{},
		roles:        map[domain.StorageRole]config.StorageInstance{},
		key:          []byte("0123456789abcdef0123456789abcdef"),
		taskBeats:    map[string]int{},
		lastBeat:     map[string]workerproto.TaskHeartbeatRequest{},
		logs:         map[string][]workerproto.LogLine{},
		completed:    map[string]workerproto.CompleteRequest{},
		failed:       map[string]workerproto.FailRequest{},
		calls:        map[string]int{},
		registerResp: workerproto.RegisterResponse{HeartbeatSeconds: 1, ProtocolVersion: workerproto.ProtocolVersion},
		done:         make(chan string, 256),
		finished:     map[string]bool{},
	}
	mux := http.NewServeMux()
	for _, route := range api.Routes {
		if route.Auth != api.AuthWorker {
			continue
		}
		name := route.Name
		mux.HandleFunc(route.Method+" "+api.Prefix+route.Path, func(w http.ResponseWriter, r *http.Request) {
			d.serve(name, w, r)
		})
	}
	dir, err := os.MkdirTemp("", "vwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d.socket = filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", d.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	_ = srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	return d
}

func (d *fakeDaemon) coordinator(t *testing.T) workerproto.Coordinator {
	t.Helper()
	c, err := client.New(client.Options{Daemon: "unix://" + d.socket, Token: []byte(testToken)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeError(w http.ResponseWriter, err error) {
	public := errs.Public(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(errs.HTTPStatus(public.Code))
	_ = json.NewEncoder(w).Encode(errs.Body{Error: public})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (d *fakeDaemon) setDown(down bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = down
}

func (d *fakeDaemon) serve(route string, w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		writeError(w, errs.New(errs.CodeUnauthenticated, "bad token"))
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, errs.New(errs.CodeUnavailable, "read"))
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls[route]++
	if d.down {
		writeError(w, errs.New(errs.CodeUnavailable, "daemon restarting"))
		return
	}
	taskID := r.PathValue("id")
	switch route {
	case api.RouteWorkerRegister:
		var req workerproto.RegisterRequest
		_ = json.Unmarshal(body, &req)
		if d.registerErr != nil {
			writeError(w, d.registerErr)
			return
		}
		d.registers = append(d.registers, req)
		writeJSON(w, d.registerResp)
	case api.RouteWorkerHeartbeat:
		var req workerproto.HeartbeatRequest
		_ = json.Unmarshal(body, &req)
		d.heartbeats = append(d.heartbeats, req)
		writeJSON(w, struct{}{})
	case api.RouteWorkerLease:
		tk, ok := d.tasks[taskID]
		if !ok {
			writeError(w, errs.New(errs.CodeGone, "no such task"))
			return
		}
		d.leases = append(d.leases, taskID)
		if tk.leaseErr != nil {
			writeError(w, tk.leaseErr)
			return
		}
		writeJSON(w, tk.lease)
	case api.RouteWorkerTaskHeartbeat:
		var req workerproto.TaskHeartbeatRequest
		_ = json.Unmarshal(body, &req)
		d.taskBeats[taskID]++
		d.lastBeat[taskID] = req
		tk := d.tasks[taskID]
		if tk.heartbeatErr != nil {
			writeError(w, tk.heartbeatErr)
			return
		}
		writeJSON(w, workerproto.TaskHeartbeatResponse{ExpiresAt: time.Now().Add(time.Minute), Cancel: tk.cancel})
	case api.RouteWorkerTaskLogs:
		var req workerproto.TaskLogsRequest
		_ = json.Unmarshal(body, &req)
		d.logs[taskID] = append(d.logs[taskID], req.Lines...)
		writeJSON(w, struct{}{})
	case api.RouteWorkerComplete:
		tk := d.tasks[taskID]
		if len(tk.completeErrors) > 0 {
			err := tk.completeErrors[0]
			tk.completeErrors = tk.completeErrors[1:]
			if err != nil {
				writeError(w, err)
				return
			}
		}
		var req workerproto.CompleteRequest
		_ = json.Unmarshal(body, &req)
		d.completed[taskID] = req
		writeJSON(w, struct{}{})
		d.done <- taskID
	case api.RouteWorkerFail:
		tk := d.tasks[taskID]
		if len(tk.failErrors) > 0 {
			err := tk.failErrors[0]
			tk.failErrors = tk.failErrors[1:]
			if err != nil {
				writeError(w, err)
				return
			}
		}
		var req workerproto.FailRequest
		_ = json.Unmarshal(body, &req)
		d.failed[taskID] = req
		writeJSON(w, struct{}{})
		d.done <- taskID
	case api.RouteWorkerConnection:
		var req workerproto.ConnectionRequest
		_ = json.Unmarshal(body, &req)
		inst, ok := d.roles[req.Role]
		if !ok {
			writeError(w, errs.Newf(errs.CodeFailedPrecondition, "role %s is not configured", req.Role))
			return
		}
		writeJSON(w, workerproto.ConnectionResponse{Instance: inst, Credentials: d.creds})
	case api.RouteWorkerCacheKey:
		writeJSON(w, workerproto.CacheKeyResponse{Key: d.key})
	}
}

func (d *fakeDaemon) addTask(lease workerproto.Lease) *task {
	d.mu.Lock()
	defer d.mu.Unlock()
	tk := &task{lease: lease}
	d.tasks[lease.TaskID] = tk
	return tk
}

func (d *fakeDaemon) with(fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn()
}

// waitDone waits for a task's report to reach the daemon, whatever the order in
// which tasks finish.
func (d *fakeDaemon) waitDone(t *testing.T, taskID string) {
	t.Helper()
	timeout := time.After(60 * time.Second)
	for {
		d.mu.Lock()
		seen := d.finished[taskID]
		d.mu.Unlock()
		if seen {
			return
		}
		select {
		case got := <-d.done:
			d.mu.Lock()
			d.finished[got] = true
			d.mu.Unlock()
		case <-timeout:
			t.Fatalf("task %s did not finish", taskID)
		}
	}
}

func (d *fakeDaemon) completion(t *testing.T, taskID string) map[string]any {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	req, ok := d.completed[taskID]
	if !ok {
		t.Fatalf("task %s did not complete; failed: %+v", taskID, d.failed[taskID])
	}
	var out map[string]any
	if err := json.Unmarshal(req.Output, &out); err != nil {
		t.Fatalf("output of %s: %v", taskID, err)
	}
	return out
}

func (d *fakeDaemon) failure(t *testing.T, taskID string) workerproto.FailRequest {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	req, ok := d.failed[taskID]
	if !ok {
		t.Fatalf("task %s did not fail; completed: %+v", taskID, d.completed[taskID])
	}
	return req
}

// entry is a stream entry of the fake queue.
type entry struct {
	delivery  jobqueue.Delivery
	consumer  string
	delivered time.Time
	pending   bool
}

// fakeQueue is an in-memory jobqueue.Queue with the semantics workers depend on.
type fakeQueue struct {
	mu      sync.Mutex
	next    int
	streams map[node.Pool][]*entry
	acked   map[string]bool
	readErr error
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{streams: map[node.Pool][]*entry{}, acked: map[string]bool{}}
}

var _ jobqueue.Queue = (*fakeQueue)(nil)

func (q *fakeQueue) Enqueue(_ context.Context, p jobqueue.Pointer) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.next++
	q.streams[p.Pool] = append(q.streams[p.Pool], &entry{delivery: jobqueue.Delivery{Pointer: p, StreamID: time.Now().Format("150405.000000")}})
	return nil
}

func (q *fakeQueue) Inflight(_ context.Context, taskID string) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, stream := range q.streams {
		for _, e := range stream {
			if e.delivery.Pointer.TaskID == taskID && !q.acked[taskID] {
				return true, nil
			}
		}
	}
	return false, nil
}

func (q *fakeQueue) Read(ctx context.Context, opts jobqueue.ReadOptions) ([]jobqueue.Delivery, error) {
	deadline := time.Now().Add(opts.Block)
	for {
		q.mu.Lock()
		if q.readErr != nil {
			err := q.readErr
			q.mu.Unlock()
			return nil, err
		}
		for _, e := range q.streams[opts.Pool] {
			if e.delivered.IsZero() {
				e.delivered = time.Now()
				e.consumer = opts.Consumer
				e.pending = true
				q.mu.Unlock()
				return []jobqueue.Delivery{e.delivery}, nil
			}
		}
		q.mu.Unlock()
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, ctx.Err()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (q *fakeQueue) ClaimStale(_ context.Context, pool node.Pool, consumer string, minIdle time.Duration, count int) ([]jobqueue.Delivery, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []jobqueue.Delivery
	for _, e := range q.streams[pool] {
		if e.pending && time.Since(e.delivered) > minIdle && len(out) < count {
			e.delivered = time.Now()
			e.consumer = consumer
			out = append(out, e.delivery)
		}
	}
	return out, nil
}

func (q *fakeQueue) Ack(_ context.Context, pool node.Pool, streamID, taskID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.streams[pool] {
		if e.delivery.StreamID == streamID && e.delivery.Pointer.TaskID == taskID {
			e.pending = false
		}
	}
	q.acked[taskID] = true
	return nil
}

func (q *fakeQueue) Depth(context.Context, node.Pool) (jobqueue.Depth, error) {
	return jobqueue.Depth{}, nil
}
func (q *fakeQueue) Ping(context.Context) error { return nil }
func (q *fakeQueue) Close() error               { return nil }

func (q *fakeQueue) isAcked(taskID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.acked[taskID]
}

// newJob returns a job ULID for a test.
func newJob() id.ULID { return id.New() }
