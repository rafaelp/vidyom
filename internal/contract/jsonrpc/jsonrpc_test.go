package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type pipePair struct{ a, b *Conn }

func newPair(t *testing.T, aHandler, bHandler Handler) pipePair {
	t.Helper()
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	a := NewConn(ar, aw, closerFunc(func() error { aw.Close(); return ar.Close() }), aHandler)
	b := NewConn(br, bw, closerFunc(func() error { bw.Close(); return br.Close() }), bHandler)
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	go b.Run(ctx)
	t.Cleanup(func() { cancel(); a.Close(); b.Close() })
	return pipePair{a, b}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestCallAndError(t *testing.T) {
	p := newPair(t, nil, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "echo":
			var v map[string]string
			if err := json.Unmarshal(params, &v); err != nil {
				return nil, err
			}
			return v, nil
		case "fail":
			return nil, &Error{Code: CodeUnavailable, Message: "no model"}
		case "boom":
			return nil, errors.New("plain")
		}
		return nil, &Error{Code: CodeMethodNotFound, Message: "method not found"}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out map[string]string
	if err := p.a.Call(ctx, "echo", map[string]string{"x": "y"}, &out); err != nil || out["x"] != "y" {
		t.Fatalf("echo: %v %v", out, err)
	}
	var rpcErr *Error
	if err := p.a.Call(ctx, "fail", struct{}{}, nil); !errors.As(err, &rpcErr) || rpcErr.Code != CodeUnavailable {
		t.Fatalf("fail: %v", err)
	}
	if err := p.a.Call(ctx, "boom", struct{}{}, nil); !errors.As(err, &rpcErr) || rpcErr.Code != CodeInternalError {
		t.Fatalf("boom: %v", err)
	}
	if err := p.a.Call(ctx, "missing", struct{}{}, nil); !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestConcurrentCallsAndNotifications(t *testing.T) {
	progress := make(chan ProgressParams, 1)
	release := make(chan struct{})
	var p pipePair
	ready := make(chan struct{})
	p = newPair(t, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method == MethodProgress {
			var v ProgressParams
			_ = json.Unmarshal(params, &v)
			progress <- v
		}
		return nil, nil
	}, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		<-ready
		if method == "slow" {
			_ = p.b.Notify(ctx, MethodProgress, ProgressParams{Token: "t", Percent: 50})
			<-release
		}
		return "ok", nil
	})
	close(ready)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		var s string
		done <- p.a.Call(ctx, "slow", struct{}{}, &s)
	}()
	select {
	case v := <-progress:
		if v.Percent != 50 {
			t.Fatalf("progress = %+v", v)
		}
	case <-ctx.Done():
		t.Fatal("no progress received while a call is running")
	}
	// A second call must not be blocked by the slow one.
	var s string
	if err := p.a.Call(ctx, "fast", struct{}{}, &s); err != nil || s != "ok" {
		t.Fatalf("fast: %v %q", err, s)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCallCancelSendsCancelRequest(t *testing.T) {
	cancelled := make(chan string, 1)
	p := newPair(t, nil, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case MethodCancelRequest:
			var v CancelParams
			_ = json.Unmarshal(params, &v)
			cancelled <- v.ID
		case "hang":
			<-ctx.Done()
		}
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := p.a.Call(ctx, "hang", struct{}{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	select {
	case id := <-cancelled:
		if id == "" {
			t.Fatal("no request ID")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no $/cancelRequest sent")
	}
}

func TestCallFailsWhenClosed(t *testing.T) {
	p := newPair(t, nil, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, nil
	})
	done := make(chan error, 1)
	go func() { done <- p.a.Call(context.Background(), "hang", struct{}{}, nil) }()
	time.Sleep(50 * time.Millisecond)
	p.a.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return after close")
	}
}

func TestWireFormatIsOneObjectPerLine(t *testing.T) {
	var sb strings.Builder
	c := NewConn(strings.NewReader(""), &sb, nil, nil)
	if err := c.Notify(context.Background(), "x", map[string]string{"text": "a\nb"}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("not NDJSON: %q", out)
	}
	if !strings.Contains(out, `"jsonrpc":"2.0"`) {
		t.Fatalf("missing version: %q", out)
	}
}
