package file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func at(day string, hour int) time.Time {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		panic(err)
	}
	return t.Add(time.Duration(hour) * time.Hour)
}

func newSink(t *testing.T, c *clock, params Parameters) (*Sink, string) {
	t.Helper()
	dir := t.TempDir()
	params.Path = filepath.Join(dir, "logs", "vidyomd.jsonl")
	s, err := New(params, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, filepath.Dir(params.Path)
}

func rec(tenant, job, level, msg string, ts time.Time) connector.LogRecord {
	return connector.LogRecord{Timestamp: ts, Level: level, Service: "vidyomd", Event: "test", Message: msg, TenantID: tenant, JobID: job}
}

func write(t *testing.T, s *Sink, records ...connector.LogRecord) {
	t.Helper()
	if err := s.Write(context.Background(), records); err != nil {
		t.Fatal(err)
	}
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestWritesJSONLWithSpecFields(t *testing.T) {
	c := &clock{t: at("2026-11-09", 10)}
	s, dir := newSink(t, c, Parameters{})
	r := rec("acme", "JOB-001", "info", "hello", c.now())
	r.WorkflowID, r.NodeID, r.AssetID, r.TraceID, r.DurationMS, r.Status = "create-clip", "render", "ORI-001", "trace-1", 12, "ok"
	write(t, s, r, rec("acme", "", "warn", "two", c.now()))
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "vidyomd.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"timestamp", "level", "service", "event", "tenant_id", "job_id", "workflow_id", "node_id", "asset_id", "trace_id", "duration_ms", "status"} {
		if _, ok := got[field]; !ok {
			t.Errorf("§10.1 field %s is missing: %s", field, lines[0])
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "vidyomd.jsonl")); err != nil || info.Mode().Perm()&0o007 != 0 {
		t.Errorf("log file must not be world-readable: %v %v", info.Mode(), err)
	}
}

func TestRotatesOnUTCDayAndKeepsNames(t *testing.T) {
	c := &clock{t: at("2026-11-08", 23)}
	s, dir := newSink(t, c, Parameters{})
	write(t, s, rec("acme", "", "info", "day one", c.now()))
	c.set(at("2026-11-09", 0))
	write(t, s, rec("acme", "", "info", "day two", c.now()))
	got := files(t, dir)
	if len(got) != 2 || got[0] != "vidyomd-2026-11-08.jsonl" || got[1] != "vidyomd.jsonl" {
		t.Fatalf("files: %v", got)
	}
	old, _ := os.ReadFile(filepath.Join(dir, "vidyomd-2026-11-08.jsonl"))
	if !strings.Contains(string(old), "day one") || strings.Contains(string(old), "day two") {
		t.Errorf("rotated content: %s", old)
	}
	cur, _ := os.ReadFile(filepath.Join(dir, "vidyomd.jsonl"))
	if !strings.Contains(string(cur), "day two") || strings.Contains(string(cur), "day one") {
		t.Errorf("active content: %s", cur)
	}
}

func TestDaysWithNoRecordsMakeNoFile(t *testing.T) {
	c := &clock{t: at("2026-11-08", 23)}
	s, dir := newSink(t, c, Parameters{})
	write(t, s, rec("acme", "", "info", "one", c.now()))
	c.set(at("2026-11-12", 1))
	write(t, s, rec("acme", "", "info", "two", c.now()))
	c.set(at("2026-11-13", 1))
	write(t, s, nil...)
	if got := files(t, dir); len(got) != 2 || got[0] != "vidyomd-2026-11-08.jsonl" {
		t.Errorf("files: %v", got)
	}
}

func TestRotationNoneKeepsOneFile(t *testing.T) {
	c := &clock{t: at("2026-11-08", 23)}
	s, dir := newSink(t, c, Parameters{Rotation: "none"})
	write(t, s, rec("acme", "", "info", "one", c.now()))
	c.set(at("2026-11-20", 1))
	write(t, s, rec("acme", "", "info", "two", c.now()))
	if got := files(t, dir); len(got) != 1 {
		t.Errorf("files: %v", got)
	}
}

func TestRetentionSweepsRotatedFilesAndLeavesOthers(t *testing.T) {
	c := &clock{t: at("2026-11-09", 10)}
	dir := t.TempDir()
	for _, name := range []string{"vidyomd-2026-11-08.jsonl", "vidyomd-2026-10-30.jsonl", "vidyomd-2026-10-29.jsonl", "vidyomd-2026-10-01-1.jsonl", "unrelated.jsonl", "vidyomd-notes.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	s, err := New(Parameters{Path: filepath.Join(dir, "vidyomd.jsonl"), RetentionDays: 10}, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	got := strings.Join(files(t, dir), " ")
	for _, gone := range []string{"vidyomd-2026-10-29.jsonl", "vidyomd-2026-10-01-1.jsonl"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s should be swept: %s", gone, got)
		}
	}
	for _, kept := range []string{"vidyomd-2026-11-08.jsonl", "vidyomd-2026-10-30.jsonl", "unrelated.jsonl", "vidyomd-notes.jsonl"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%s should stay: %s", kept, got)
		}
	}
	// A later rotation sweeps again.
	write(t, s, rec("acme", "", "info", "x", c.now()))
	c.set(at("2026-11-20", 1))
	write(t, s, rec("acme", "", "info", "y", c.now()))
	if got := strings.Join(files(t, dir), " "); strings.Contains(got, "vidyomd-2026-10-30.jsonl") {
		t.Errorf("rotation should sweep too: %s", got)
	}
}

func TestDefaultRetentionMatchesTheSpec(t *testing.T) {
	c := &clock{t: at("2026-11-09", 10)}
	s, _ := newSink(t, c, Parameters{})
	if s.params.RetentionDays != config.DefaultLogDays || s.params.Rotation != "daily" {
		t.Errorf("params: %+v", s.params)
	}
}

func TestRestartRotatesAPreviousDayAndAppendsSameDay(t *testing.T) {
	c := &clock{t: at("2026-11-09", 10)}
	dir := t.TempDir()
	path := filepath.Join(dir, "vidyomd.jsonl")
	s1, err := New(Parameters{Path: path}, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	write(t, s1, rec("acme", "", "info", "first run", c.now()))
	s1.Close(context.Background())

	s2, err := New(Parameters{Path: path}, Options{Now: func() time.Time { return time.Now() }})
	if err != nil {
		t.Fatal(err)
	}
	write(t, s2, rec("acme", "", "info", "second run", c.now()))
	s2.Close(context.Background())
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "first run") || !strings.Contains(string(data), "second run") {
		t.Errorf("a same-day restart must append: %s", data)
	}

	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	s3, err := New(Parameters{Path: path}, Options{Now: func() time.Time { return tomorrow }})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close(context.Background())
	if got := files(t, dir); len(got) != 2 {
		t.Errorf("a restart on a later day must rotate: %v", got)
	}
}

func TestRotationCollisionGetsASuffix(t *testing.T) {
	c := &clock{t: at("2026-11-08", 10)}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vidyomd-2026-11-08.jsonl"), []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := New(Parameters{Path: filepath.Join(dir, "vidyomd.jsonl")}, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	write(t, s, rec("acme", "", "info", "x", c.now()))
	c.set(at("2026-11-09", 10))
	write(t, s, rec("acme", "", "info", "y", c.now()))
	got := strings.Join(files(t, dir), " ")
	if !strings.Contains(got, "vidyomd-2026-11-08-1.jsonl") {
		t.Errorf("files: %s", got)
	}
}

func TestParametersAreValidated(t *testing.T) {
	for _, p := range []Parameters{
		{},
		{Path: "relative/vidyomd.jsonl"},
		{Path: "/tmp/x.jsonl", Rotation: "hourly"},
		{Path: "/tmp/x.jsonl", RetentionDays: -1},
	} {
		if _, err := New(p, Options{}); err == nil {
			t.Errorf("%+v should be refused", p)
		}
	}
	if _, err := Factory(context.Background(), config.Sink{Parameters: json.RawMessage(`{"path":`)}, nil); err == nil {
		t.Error("invalid JSON should be refused")
	}
}

func TestFactoryAndRegister(t *testing.T) {
	reg := connector.NewRegistry()
	Register(reg)
	f, err := reg.LogSink(Driver)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	params, _ := json.Marshal(Parameters{Path: filepath.Join(dir, "a.jsonl"), Rotation: "daily", RetentionDays: 30})
	sink, err := f(context.Background(), config.Sink{ID: "local", Driver: Driver, Enabled: true, Parameters: params}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close(context.Background())
	if info := sink.Info(); !info.Available || info.Driver != Driver || info.Family != connector.FamilyLogSink {
		t.Errorf("info: %+v", info)
	}
	if _, ok := sink.(connector.LogQuerier); !ok {
		t.Error("the file sink must answer queries")
	}
}

func TestWriteFailsCleanlyAfterDirectoryIsGone(t *testing.T) {
	c := &clock{t: at("2026-11-08", 10)}
	s, dir := newSink(t, c, Parameters{})
	write(t, s, rec("acme", "", "info", "x", c.now()))
	s.mu.Lock()
	s.f.Close() // simulate a failing descriptor
	s.mu.Unlock()
	if err := s.Write(context.Background(), []connector.LogRecord{rec("acme", "", "info", "lost", c.now())}); err == nil {
		t.Fatal("a closed descriptor must fail")
	}
	s.mu.Lock()
	f, err := os.OpenFile(s.active, os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.f = f
	s.mu.Unlock()
	write(t, s, rec("acme", "", "info", "after", c.now()))
	data, _ := os.ReadFile(filepath.Join(dir, "vidyomd.jsonl"))
	if !strings.Contains(string(data), "\n\n") && !strings.HasSuffix(string(data), "\n") {
		t.Errorf("recovery should start on a fresh line: %q", data)
	}
	page, err := s.QueryLogs(context.Background(), connector.LogQuery{})
	if err != nil || len(page.Records) != 2 {
		t.Errorf("query after recovery: %v %+v", err, page)
	}
}

func TestQueryFiltersByTenantLevelJobAndSince(t *testing.T) {
	c := &clock{t: at("2026-11-09", 10)}
	s, _ := newSink(t, c, Parameters{})
	write(t, s,
		rec("acme", "JOB-001", "info", "a1", at("2026-11-09", 1)),
		rec("other", "JOB-001", "error", "other tenant", at("2026-11-09", 2)),
		rec("acme", "JOB-002", "error", "a2", at("2026-11-09", 3)),
		rec("acme", "JOB-001", "debug", "a3", at("2026-11-09", 4)),
		rec("", "", "info", "system", at("2026-11-09", 5)),
	)
	query := func(q connector.LogQuery) []string {
		t.Helper()
		page, err := s.QueryLogs(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range page.Records {
			out = append(out, r.Message)
		}
		return out
	}
	if got := fmt.Sprint(query(connector.LogQuery{TenantID: "acme"})); got != "[a3 a2 a1]" {
		t.Errorf("tenant filter and newest first: %s", got)
	}
	if got := fmt.Sprint(query(connector.LogQuery{TenantID: "acme", MinLevel: "warn"})); got != "[a2]" {
		t.Errorf("level: %s", got)
	}
	if got := fmt.Sprint(query(connector.LogQuery{TenantID: "acme", JobID: "JOB-001"})); got != "[a3 a1]" {
		t.Errorf("job: %s", got)
	}
	if got := fmt.Sprint(query(connector.LogQuery{TenantID: "acme", Since: at("2026-11-09", 2)})); got != "[a3 a2]" {
		t.Errorf("since: %s", got)
	}
	if got := fmt.Sprint(query(connector.LogQuery{TenantID: "ghost"})); got != "[]" {
		t.Errorf("no records for an unknown tenant: %s", got)
	}
	if got := fmt.Sprint(query(connector.LogQuery{})); !strings.Contains(got, "system") {
		t.Errorf("an empty tenant is the daemon-level view: %s", got)
	}
}

func TestQueryPagesAcrossRotatedFiles(t *testing.T) {
	c := &clock{t: at("2026-11-07", 10)}
	s, _ := newSink(t, c, Parameters{})
	want := []string{}
	for day := 7; day <= 9; day++ {
		c.set(at(fmt.Sprintf("2026-11-%02d", day), 10))
		for i := range 7 {
			msg := fmt.Sprintf("d%d-%d", day, i)
			write(t, s, rec("acme", "", "info", msg, c.now()))
			want = append([]string{msg}, want...)
		}
	}
	var got []string
	q := connector.LogQuery{TenantID: "acme", Limit: 5}
	pages := 0
	for {
		page, err := s.QueryLogs(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(page.Records) > 5 {
			t.Fatalf("page too large: %d", len(page.Records))
		}
		for _, r := range page.Records {
			got = append(got, r.Message)
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("pages lost or repeated records:\n got %v\nwant %v", got, want)
	}
	if pages != 5 {
		t.Errorf("pages %d", pages)
	}
}

func TestQueryCursorSurvivesRotationBetweenPages(t *testing.T) {
	c := &clock{t: at("2026-11-08", 10)}
	s, _ := newSink(t, c, Parameters{})
	for i := range 6 {
		write(t, s, rec("acme", "", "info", fmt.Sprintf("old-%d", i), c.now()))
	}
	page, err := s.QueryLogs(context.Background(), connector.LogQuery{TenantID: "acme", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next page")
	}
	c.set(at("2026-11-09", 1))
	write(t, s, rec("acme", "", "info", "new", c.now()))
	rest, err := s.QueryLogs(context.Background(), connector.LogQuery{TenantID: "acme", Limit: 100, Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Records) != 4 || rest.Records[0].Message != "old-3" || rest.Records[3].Message != "old-0" {
		t.Errorf("records after rotation: %+v", rest.Records)
	}
}

func TestQuerySkipsCorruptLinesAndRefusesBadCursor(t *testing.T) {
	c := &clock{t: at("2026-11-09", 10)}
	s, dir := newSink(t, c, Parameters{})
	write(t, s, rec("acme", "", "info", "good1", c.now()))
	f, err := os.OpenFile(filepath.Join(dir, "vidyomd.jsonl"), os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{not json\n" + strings.Repeat("x", 100<<10) + "\n")
	f.Close()
	write(t, s, rec("acme", "", "info", "good2", c.now()))
	page, err := s.QueryLogs(context.Background(), connector.LogQuery{TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 2 || page.Records[0].Message != "good2" {
		t.Errorf("records: %+v", page.Records)
	}
	if _, err := s.QueryLogs(context.Background(), connector.LogQuery{Cursor: "!!!"}); err == nil {
		t.Error("a bad cursor must be refused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.QueryLogs(ctx, connector.LogQuery{}); err == nil {
		t.Error("a cancelled query must stop")
	}
}
