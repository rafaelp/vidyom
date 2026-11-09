package apiclient_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/apiclient/apitest"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func jobEvents(from, to int64) []apitest.SSEFrame {
	var frames []apitest.SSEFrame
	for seq := from; seq <= to; seq++ {
		frames = append(frames, apitest.SSEFrame{
			ID: strconv.FormatInt(seq, 10), Name: api.SSEJobEvent,
			Data: domain.JobEvent{Seq: seq, JobID: "JOB-001", Kind: domain.JobEventLog, Message: "line"},
		})
	}
	return frames
}

func streamOpts(onEvent func(apiclient.Event) error) apiclient.StreamOptions {
	return apiclient.StreamOptions{
		Call:       apiclient.Call{Route: api.RouteJobsEvents, Path: apiclient.PathParam("id", "JOB-001")},
		OnEvent:    onEvent,
		MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
	}
}

func TestStreamDeliversEventsAndEnds(t *testing.T) {
	srv := apitest.New(t)
	srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != api.SSEContentType {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		apitest.WriteSSEHeaders(w)
		apitest.WriteSSEComment(w)
		for _, f := range jobEvents(1, 3) {
			apitest.WriteSSE(w, f)
			apitest.WriteSSEComment(w)
		}
		apitest.WriteSSE(w, apitest.SSEFrame{Name: api.SSEEnd})
	})
	c := newUnixClient(t, srv, "acme")
	var ids []string
	err := c.Stream(context.Background(), streamOpts(func(ev apiclient.Event) error {
		if ev.Name != api.SSEJobEvent {
			t.Errorf("event name = %q", ev.Name)
		}
		ids = append(ids, ev.ID)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != "1" || ids[2] != "3" {
		t.Fatalf("ids = %v", ids)
	}
	if len(srv.Mutations()) != 0 {
		t.Fatal("a stream must not issue a mutating request")
	}
}

func TestStreamReconnectsWithLastEventID(t *testing.T) {
	srv := apitest.New(t)
	var connections atomic.Int32
	var resumedFrom atomic.Int64
	srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, r *http.Request) {
		apitest.WriteSSEHeaders(w)
		switch connections.Add(1) {
		case 1:
			for _, f := range jobEvents(1, 2) {
				apitest.WriteSSE(w, f)
			}
			// Dropped with no end frame, as when the daemon restarts.
		case 2:
			resumedFrom.Store(apitest.LastEventID(r))
			for _, f := range jobEvents(apitest.LastEventID(r)+1, 4) {
				apitest.WriteSSE(w, f)
			}
			apitest.WriteSSE(w, apitest.SSEFrame{Name: api.SSEEnd})
		default:
			t.Error("too many connections")
		}
	})
	c := newUnixClient(t, srv, "acme")
	var seen []string
	reconnects := 0
	opts := streamOpts(func(ev apiclient.Event) error { seen = append(seen, ev.ID); return nil })
	opts.OnReconnect = func(error, time.Duration) { reconnects++ }
	if err := c.Stream(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if resumedFrom.Load() != 2 {
		t.Fatalf("resumed from %d, want 2", resumedFrom.Load())
	}
	if want := []string{"1", "2", "3", "4"}; len(seen) != 4 || seen[0] != want[0] || seen[3] != want[3] {
		t.Fatalf("seen = %v: an event was duplicated or lost", seen)
	}
	if reconnects != 1 {
		t.Fatalf("reconnects = %d", reconnects)
	}
}

func TestStreamRetriesThroughServerUnavailable(t *testing.T) {
	srv := apitest.New(t)
	var connections atomic.Int32
	srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, _ *http.Request) {
		if connections.Add(1) <= 2 {
			apitest.WriteError(w, errs.New(errs.CodeSealed, "the vault is sealed"))
			return
		}
		apitest.WriteSSEHeaders(w)
		apitest.WriteSSE(w, apitest.SSEFrame{Name: api.SSEEnd})
	})
	c := newUnixClient(t, srv, "acme")
	if err := c.Stream(context.Background(), streamOpts(nil)); err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 3 {
		t.Fatalf("connections = %d", connections.Load())
	}
}

func TestStreamStopsOnRefusal(t *testing.T) {
	for _, code := range []errs.Code{errs.CodeNotFound, errs.CodeForbidden, errs.CodeUnauthenticated} {
		srv := apitest.New(t)
		srv.Error(api.RouteJobsEvents, errs.New(code, "refused"))
		c := newUnixClient(t, srv, "acme")
		err := c.Stream(context.Background(), streamOpts(nil))
		if !errs.IsCode(err, code) {
			t.Fatalf("%s: err = %v", code, err)
		}
		if n := len(srv.Calls(api.RouteJobsEvents)); n != 1 {
			t.Fatalf("%s: %d requests; a refusal must not be retried", code, n)
		}
	}
}

func TestStreamCancellationDisconnectsWithoutMutation(t *testing.T) {
	srv := apitest.New(t)
	connected := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, r *http.Request) {
		apitest.WriteSSEHeaders(w)
		for _, f := range jobEvents(1, 1) {
			apitest.WriteSSE(w, f)
		}
		connected <- struct{}{}
		<-r.Context().Done()
		disconnected <- struct{}{}
	})
	c := newUnixClient(t, srv, "acme")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Stream(ctx, streamOpts(nil)) }()
	<-connected
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not see the client disconnect")
	}
	if len(srv.Mutations()) != 0 {
		t.Fatalf("disconnecting issued %v", srv.Mutations())
	}
}

func TestStreamHandlerErrorStops(t *testing.T) {
	srv := apitest.New(t)
	srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteSSEHeaders(w)
		for _, f := range jobEvents(1, 2) {
			apitest.WriteSSE(w, f)
		}
	})
	c := newUnixClient(t, srv, "acme")
	stop := errors.New("broken pipe on stdout")
	err := c.Stream(context.Background(), streamOpts(func(apiclient.Event) error { return stop }))
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamIdleWatchdogReconnects(t *testing.T) {
	srv := apitest.New(t)
	var connections atomic.Int32
	srv.Handle(api.RouteJobsEvents, func(w http.ResponseWriter, r *http.Request) {
		apitest.WriteSSEHeaders(w)
		if connections.Add(1) == 1 {
			<-r.Context().Done() // silent, not even a keep-alive
			return
		}
		apitest.WriteSSE(w, apitest.SSEFrame{Name: api.SSEEnd})
	})
	c := newUnixClient(t, srv, "acme")
	opts := streamOpts(nil)
	opts.IdleTimeout = 30 * time.Millisecond
	if err := c.Stream(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 2 {
		t.Fatalf("connections = %d", connections.Load())
	}
}

func TestStreamParsesWireFormat(t *testing.T) {
	srv := apitest.New(t)
	srv.Handle(api.RouteEventsStream, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteSSEHeaders(w)
		// CRLF, a comment, a field with no space after the colon, multi-line data, an
		// unknown field and a retry hint.
		_, _ = w.Write([]byte(": hello\r\nid:7\r\nevent:event\r\ndata: one\r\ndata: two\r\nfoo: bar\r\nretry: 5\r\n\r\n"))
		_, _ = w.Write([]byte("data: only-data\n\n"))
		_, _ = w.Write([]byte("id: 9\n\n")) // an id-only block dispatches nothing
		_, _ = w.Write([]byte("event: end\ndata: {}\n\n"))
	})
	c := newUnixClient(t, srv, "acme")
	var got []apiclient.Event
	err := c.Stream(context.Background(), apiclient.StreamOptions{
		Call: apiclient.Call{Route: api.RouteEventsStream},
		OnEvent: func(ev apiclient.Event) error {
			got = append(got, apiclient.Event{ID: ev.ID, Name: ev.Name, Data: append([]byte(nil), ev.Data...)})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got = %+v", got)
	}
	if got[0].ID != "7" || got[0].Name != "event" || string(got[0].Data) != "one\ntwo" {
		t.Errorf("first = %+v, data %q", got[0], got[0].Data)
	}
	if got[1].ID != "7" || string(got[1].Data) != "only-data" {
		t.Errorf("second = %+v", got[1])
	}
}

func TestStreamRejectsNonStreamAnswer(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteLogsStream, 200, map[string]string{"x": "y"})
	c := newUnixClient(t, srv, "acme")
	err := c.Stream(context.Background(), apiclient.StreamOptions{Call: apiclient.Call{Route: api.RouteLogsStream}})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestStreamRefusesNonStreamRoute(t *testing.T) {
	srv := apitest.New(t)
	c := newUnixClient(t, srv, "acme")
	if err := c.Stream(context.Background(), apiclient.StreamOptions{Call: apiclient.Call{Route: api.RouteJobsCancel, Path: apiclient.PathParam("id", "JOB-001")}}); err == nil {
		t.Fatal("expected an error")
	}
	if len(srv.Calls()) != 0 {
		t.Fatal("a non-stream route must not be requested")
	}
}
