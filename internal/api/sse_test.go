package apiserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

// open starts a stream on a real server, so flushing and disconnects are real.
func open(t *testing.T, h http.Handler, path, token string, headers map[string]string) (*http.Response, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(api.HeaderTenant, string(tenantA))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, cancel
}

type sseEvent struct {
	id, event, data string
}

// readEvent returns the next non-comment event, or ok false at the end of the stream.
func readEvent(t *testing.T, r *bufio.Reader) (sseEvent, bool) {
	t.Helper()
	var ev sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return ev, false
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if ev.event != "" || ev.data != "" {
				return ev, true
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id: "):
			ev.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			ev.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func jobStream(events ...domain.JobEvent) func(ctx context.Context, after int64) (<-chan domain.JobEvent, error) {
	return func(ctx context.Context, after int64) (<-chan domain.JobEvent, error) {
		ch := make(chan domain.JobEvent, len(events))
		for _, e := range events {
			if e.Seq > after {
				ch <- e
			}
		}
		close(ch)
		return ch, nil
	}
}

func TestJobEventsFramesAndEnd(t *testing.T) {
	w := newWorld(t)
	w.jobs.stream = jobStream(
		domain.JobEvent{Seq: 1, JobID: "JOB-001", Kind: domain.JobEventState, Message: "queued"},
		domain.JobEvent{Seq: 2, JobID: "JOB-001", Kind: domain.JobEventState, Message: "line one\nline two"},
	)
	resp, _ := open(t, w.serve(), "/api/v1/jobs/JOB-001/events", tokenViewer, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != api.SSEContentType {
		t.Fatalf("Content-Type = %q", got)
	}
	if resp.Header.Get(api.HeaderRequestID) == "" {
		t.Error("no request ID on a stream")
	}
	r := bufio.NewReader(resp.Body)

	first, ok := readEvent(t, r)
	if !ok || first.id != "1" || first.event != api.SSEJobEvent {
		t.Fatalf("first = %+v", first)
	}
	var decoded domain.JobEvent
	if err := json.Unmarshal([]byte(first.data), &decoded); err != nil || decoded.Message != "queued" {
		t.Fatalf("data = %q, %v", first.data, err)
	}

	second, ok := readEvent(t, r)
	if !ok || second.id != "2" {
		t.Fatalf("second = %+v", second)
	}
	if err := json.Unmarshal([]byte(second.data), &decoded); err != nil || decoded.Message != "line one\nline two" {
		t.Fatalf("a newline broke the frame: %q, %v", second.data, err)
	}

	end, ok := readEvent(t, r)
	if !ok || end.event != api.SSEEnd {
		t.Fatalf("end = %+v", end)
	}
	if _, ok := readEvent(t, r); ok {
		t.Fatal("frames after end")
	}
}

func TestLastEventIDResumes(t *testing.T) {
	w := newWorld(t)
	w.jobs.stream = jobStream(
		domain.JobEvent{Seq: 1}, domain.JobEvent{Seq: 2}, domain.JobEvent{Seq: 3},
	)
	resp, _ := open(t, w.serve(), "/api/v1/jobs/JOB-001/events", tokenViewer, map[string]string{api.HeaderLastEventID: "2"})
	r := bufio.NewReader(resp.Body)
	ev, ok := readEvent(t, r)
	if !ok || ev.id != "3" {
		t.Fatalf("resumed at %+v", ev)
	}
	if got := w.lastCall(t).Args; got != any(int64(2)) {
		t.Fatalf("afterSeq = %v", got)
	}
}

func TestBadLastEventID(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for _, bad := range []string{"abc", "-1", "1.5"} {
		rec := do(h, request{method: "GET", path: "/api/v1/jobs/JOB-001/events", token: tokenViewer, tenant: tenantA, headers: map[string]string{api.HeaderLastEventID: bad}})
		wantError(t, rec, http.StatusBadRequest, errs.CodeInvalidArgument)
	}
	if n := countCalls(w, "jobs.Subscribe"); n != 0 {
		t.Fatal("subscribed on a bad position")
	}
}

func TestSubscribeErrorsAreOrdinaryErrors(t *testing.T) {
	w := newWorld(t)
	w.err = errs.NotFound("job")
	rec := do(w.serve(), request{method: "GET", path: "/api/v1/jobs/JOB-404/events", token: tokenViewer, tenant: tenantA})
	wantError(t, rec, http.StatusNotFound, errs.CodeNotFound)
}

// §5.5: a disconnect cancels the subscription and calls nothing that mutates.
func TestDisconnectNeverTouchesTheJob(t *testing.T) {
	w := newWorld(t)
	gone := make(chan struct{})
	w.jobs.stream = func(ctx context.Context, _ int64) (<-chan domain.JobEvent, error) {
		ch := make(chan domain.JobEvent, 1)
		ch <- domain.JobEvent{Seq: 1}
		go func() {
			<-ctx.Done()
			close(gone)
			close(ch)
		}()
		return ch, nil
	}
	resp, cancel := open(t, w.serve(), "/api/v1/jobs/JOB-001/events", tokenEditor, nil)
	r := bufio.NewReader(resp.Body)
	if _, ok := readEvent(t, r); !ok {
		t.Fatal("no first event")
	}

	cancel()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription outlived the client")
	}

	for _, name := range w.callNames() {
		switch name {
		case "jobs.Subscribe":
		default:
			t.Fatalf("a stream called %s", name)
		}
	}
	if records := w.auditRecords(); len(records) != 0 {
		t.Fatalf("a stream wrote the audit: %+v", records)
	}
}

func TestKeepAliveAndRevocationCutTheStream(t *testing.T) {
	w := newWorld(t)
	done := make(chan struct{})
	w.jobs.stream = func(ctx context.Context, _ int64) (<-chan domain.JobEvent, error) {
		ch := make(chan domain.JobEvent)
		go func() {
			<-ctx.Done()
			close(done)
		}()
		return ch, nil
	}
	h := w.serve(func(d *Deps) { d.SSEKeepAlive = 20 * time.Millisecond })
	resp, _ := open(t, h, "/api/v1/jobs/JOB-001/events", tokenViewer, nil)
	r := bufio.NewReader(resp.Body)

	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, ": keep-alive") {
		t.Fatalf("line = %q, %v", line, err)
	}

	// Revoke the device: the next keep-alive ends the stream, with no end event.
	w.mu.Lock()
	delete(w.tokens, tokenViewer)
	w.mu.Unlock()

	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rest), "event: end") {
		t.Fatalf("end event after a revocation: %q", rest)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription survived the revocation")
	}
}

func TestLostMembershipCutsTheStream(t *testing.T) {
	w := newWorld(t)
	w.jobs.stream = func(ctx context.Context, _ int64) (<-chan domain.JobEvent, error) {
		return make(chan domain.JobEvent), nil
	}
	h := w.serve(func(d *Deps) { d.SSEKeepAlive = 20 * time.Millisecond })
	resp, _ := open(t, h, "/api/v1/jobs/JOB-001/events", tokenViewer, nil)
	w.mu.Lock()
	delete(w.members, "viewer@example.com/acme")
	w.mu.Unlock()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownEndsStreamsWithoutEnd(t *testing.T) {
	w := newWorld(t)
	w.jobs.stream = func(ctx context.Context, _ int64) (<-chan domain.JobEvent, error) {
		ch := make(chan domain.JobEvent, 1)
		ch <- domain.JobEvent{Seq: 1}
		return ch, nil
	}
	resp, _ := open(t, w.serve(), "/api/v1/jobs/JOB-001/events", tokenViewer, nil)
	r := bufio.NewReader(resp.Body)
	if _, ok := readEvent(t, r); !ok {
		t.Fatal("no first event")
	}
	close(w.shutdown)
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rest), "event: end") {
		t.Fatalf("clients must reconnect after a shutdown, not stop: %q", rest)
	}
}

func TestEventStream(t *testing.T) {
	w := newWorld(t)
	w.events.stream = func(ctx context.Context, after int64) (<-chan service.StreamedEvent, error) {
		ch := make(chan service.StreamedEvent, 1)
		ch <- service.StreamedEvent{Seq: after + 1, Event: event.Envelope{Type: "clip.created"}}
		close(ch)
		return ch, nil
	}
	resp, _ := open(t, w.serve(), "/api/v1/events/stream?type=clip.created", tokenViewer, map[string]string{api.HeaderLastEventID: "41"})
	r := bufio.NewReader(resp.Body)
	ev, ok := readEvent(t, r)
	if !ok || ev.id != "42" || ev.event != api.SSEEvent || !strings.Contains(ev.data, "clip.created") {
		t.Fatalf("event = %+v", ev)
	}
}

func TestLogStream(t *testing.T) {
	w := newWorld(t)
	w.obs.stream = func(ctx context.Context) (<-chan connector.LogRecord, error) {
		ch := make(chan connector.LogRecord, 1)
		ch <- connector.LogRecord{Message: "transcode started"}
		close(ch)
		return ch, nil
	}
	resp, _ := open(t, w.serve(), "/api/v1/logs/stream?level=info&job_id=JOB-001", tokenViewer, nil)
	r := bufio.NewReader(resp.Body)
	ev, ok := readEvent(t, r)
	if !ok || ev.id != "" || ev.event != api.SSELog || !strings.Contains(ev.data, "transcode started") {
		t.Fatalf("event = %+v", ev)
	}
	if got := w.lastCall(t).Args.(api.LogQuery); got.Level != "info" || got.JobID != "JOB-001" {
		t.Fatalf("query = %+v", got)
	}
}
