package loki

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// mockLoki is a local stand-in for Loki's push API. It is not a real Loki (I12).
type mockLoki struct {
	mu       sync.Mutex
	requests []pushRequest
	headers  []http.Header
	status   int
	body     string
}

func (m *mockLoki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.URL.Path != pushPath || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req pushRequest
	if err := json.NewDecoder(gz).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	m.requests = append(m.requests, req)
	m.headers = append(m.headers, r.Header.Clone())
	if m.status != 0 {
		w.WriteHeader(m.status)
		io.WriteString(w, m.body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type secrets map[string]string

func (s secrets) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	v, ok := s[ref]
	if !ok {
		return nil, errors.New("no such secret")
	}
	return []byte(v), nil
}

func open(t *testing.T, serverURL, credentialsRef string, res connector.SecretResolver, extra map[string]any) connector.LogSink {
	t.Helper()
	params := map[string]any{"url": serverURL}
	for k, v := range extra {
		params[k] = v
	}
	data, _ := json.Marshal(params)
	sink, err := Factory(context.Background(), config.Sink{ID: "loki", Driver: Driver, Enabled: true, CredentialsRef: credentialsRef, Parameters: data}, res)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close(context.Background()) })
	return sink
}

func TestPushFormatAndLabels(t *testing.T) {
	mock := &mockLoki{}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	sink := open(t, srv.URL, "", nil, map[string]any{"labels": map[string]string{"env": "dev", "level": "ignored"}, "orgId": "acme-org"})

	ts := time.Date(2026, 11, 9, 10, 0, 0, 5, time.UTC)
	batch := []connector.LogRecord{
		{Timestamp: ts, Level: "info", Service: "vidyomd", Event: "job.started", TenantID: "acme", JobID: "JOB-001"},
		{Timestamp: ts.Add(time.Second), Level: "info", Service: "vidyomd", Event: "job.progress", TenantID: "acme", JobID: "JOB-001"},
		{Timestamp: ts.Add(2 * time.Second), Level: "error", Service: "vidyomd", Event: "job.failed", TenantID: "other", JobID: "JOB-001"},
		{Timestamp: ts.Add(3 * time.Second), Level: "warn", Service: "vidyomd", Event: "daemon.note"},
	}
	if err := sink.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mock.requests) != 1 {
		t.Fatalf("requests: %d", len(mock.requests))
	}
	h := mock.headers[0]
	if h.Get("Content-Encoding") != "gzip" || h.Get("Content-Type") != "application/json" || h.Get("X-Scope-OrgID") != "acme-org" {
		t.Errorf("headers: %v", h)
	}
	if h.Get("Authorization") != "" {
		t.Error("no credential was configured")
	}
	streams := mock.requests[0].Streams
	if len(streams) != 3 {
		t.Fatalf("streams: %+v", streams)
	}
	first := streams[0]
	if first.Stream["service"] != "vidyomd" || first.Stream["tenant_id"] != "acme" || first.Stream["env"] != "dev" || first.Stream["level"] != "info" {
		t.Errorf("labels: %+v", first.Stream)
	}
	if _, ok := first.Stream["job_id"]; ok {
		t.Error("job_id would be high-cardinality as a label; it belongs in the line")
	}
	if len(first.Values) != 2 {
		t.Fatalf("values: %+v", first.Values)
	}
	if first.Values[0][0] != strconv.FormatInt(ts.UnixNano(), 10) {
		t.Errorf("timestamp: %s", first.Values[0][0])
	}
	var line connector.LogRecord
	if err := json.Unmarshal([]byte(first.Values[0][1]), &line); err != nil || line.JobID != "JOB-001" || line.Event != "job.started" {
		t.Errorf("line: %s %v", first.Values[0][1], err)
	}
	if _, ok := streams[2].Stream["tenant_id"]; ok {
		t.Errorf("a record with no tenant has no tenant label: %+v", streams[2].Stream)
	}
}

func TestSplitsLargeBatches(t *testing.T) {
	mock := &mockLoki{}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	sink := open(t, srv.URL, "", nil, map[string]any{"maxRequestBytes": 500})
	var batch []connector.LogRecord
	for i := range 50 {
		batch = append(batch, connector.LogRecord{Level: "info", Service: "vidyomd", Event: "e", Message: fmt.Sprintf("m%d-%s", i, strings.Repeat("x", 50))})
	}
	if err := sink.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if len(mock.requests) < 5 {
		t.Errorf("expected several requests, got %d", len(mock.requests))
	}
	total := 0
	for _, r := range mock.requests {
		for _, s := range r.Streams {
			total += len(s.Values)
		}
	}
	if total != 50 {
		t.Errorf("sent %d of 50", total)
	}
}

func TestAuthHeaders(t *testing.T) {
	mock := &mockLoki{}
	srv := httptest.NewServer(mock)
	defer srv.Close()
	res := secrets{"loki-auth": "svc:hunter2", "loki-token": "abc123"}
	rec := []connector.LogRecord{{Level: "info", Service: "vidyomd", Event: "e"}}

	if err := open(t, srv.URL, "loki-auth", res, nil).Write(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := open(t, srv.URL, "loki-token", res, nil).Write(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if got := mock.headers[0].Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("user:password should be Basic: %q", got)
	}
	if got := mock.headers[1].Get("Authorization"); got != "Bearer abc123" {
		t.Errorf("a token should be Bearer: %q", got)
	}
}

func TestErrorsAreReturnedForTheManagerToRetry(t *testing.T) {
	mock := &mockLoki{status: http.StatusServiceUnavailable, body: "ingester down"}
	srv := httptest.NewServer(mock)
	sink := open(t, srv.URL, "", nil, nil)
	rec := []connector.LogRecord{{Level: "info", Service: "vidyomd", Event: "e"}}
	err := sink.Write(context.Background(), rec)
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "ingester down") {
		t.Errorf("error: %v", err)
	}
	srv.Close()
	if err := sink.Write(context.Background(), rec); err == nil {
		t.Error("a dead server must fail the write")
	}
	if err := sink.Write(context.Background(), nil); err != nil {
		t.Errorf("an empty batch does nothing: %v", err)
	}
}

func TestContextEndsAHungRequest(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	sink := open(t, srv.URL, "", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := sink.Write(ctx, []connector.LogRecord{{Level: "info", Event: "e"}}); err == nil {
		t.Error("expected a timeout")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the write ignored its context")
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	sink := open(t, srv.URL, "t", secrets{"t": "abc123"}, nil)
	if err := sink.Write(context.Background(), []connector.LogRecord{{Level: "info", Event: "e"}}); err == nil {
		t.Error("a redirect must not count as success")
	}
	if hits != 0 {
		t.Error("the credential must not follow a redirect")
	}
}

func TestFactoryRefusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		cfg    config.Sink
		secret connector.SecretResolver
	}{
		{"bad JSON", config.Sink{Parameters: json.RawMessage(`{`)}, nil},
		{"no URL", config.Sink{Parameters: json.RawMessage(`{}`)}, nil},
		{"credentials in URL", config.Sink{Parameters: json.RawMessage(`{"url":"https://u:p@loki.example.internal"}`)}, nil},
		{"credential over HTTP", config.Sink{CredentialsRef: "t", Parameters: json.RawMessage(`{"url":"http://loki.example.internal"}`)}, secrets{"t": "x"}},
		{"unknown credential", config.Sink{CredentialsRef: "t", Parameters: json.RawMessage(`{"url":"https://loki.example.internal"}`)}, secrets{}},
		{"no resolver", config.Sink{CredentialsRef: "t", Parameters: json.RawMessage(`{"url":"https://loki.example.internal"}`)}, nil},
	}
	for _, c := range cases {
		if _, err := Factory(ctx, c.cfg, c.secret); err == nil {
			t.Errorf("%s should be refused", c.name)
		}
	}
	reg := connector.NewRegistry()
	Register(reg)
	if _, err := reg.LogSink(Driver); err != nil {
		t.Fatal(err)
	}
	if list := reg.List(); len(list) != 1 || !list[0].Available {
		t.Errorf("registry: %+v", list)
	}
}
