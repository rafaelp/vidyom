package local_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/connectors/storage/local"
	"github.com/rafaelp/vidyom/connectors/storage/storagetest"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func openLocal(t *testing.T, root string, tenant domain.TenantID, role domain.StorageRole, inst config.StorageInstance) *local.Storage {
	t.Helper()
	inst.Driver = local.Driver
	inst.Root = root
	s, err := local.Open(context.Background(), connector.StorageParams{Tenant: tenant, Role: role, Instance: inst})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.(*local.Storage).Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s.(*local.Storage)
}

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T, tenant domain.TenantID) connector.Storage {
		return openLocal(t, sharedRoot(t), tenant, domain.RoleMaster, config.StorageInstance{Prefix: "masters/", Usage: config.UsageOperational})
	}, storagetest.Options{
		TamperObject: func(t *testing.T, tenant domain.TenantID, key string, content []byte) {
			path := filepath.Join(sharedRoot(t), "masters", string(tenant), key)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	})
}

var (
	rootsMu sync.Mutex
	roots   = map[string]string{}
)

// sharedRoot gives every open of one subtest the same root, as storagetest requires.
func sharedRoot(t *testing.T) string {
	t.Helper()
	rootsMu.Lock()
	defer rootsMu.Unlock()
	root, ok := roots[t.Name()]
	if !ok {
		root = t.TempDir()
		roots[t.Name()] = root
		// Under -count, the next run must get its own root: only the call to TempDir
		// schedules a removal, so a reused path would keep the previous run's objects.
		name := t.Name()
		t.Cleanup(func() {
			rootsMu.Lock()
			defer rootsMu.Unlock()
			delete(roots, name)
		})
	}
	return root
}

func accessLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("bad log line %q: %v", scanner.Text(), err)
		}
		out = append(out, line)
	}
	return out
}

func TestAccessLogClassifiesOperations(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "access.jsonl")
	params, _ := json.Marshal(map[string]any{"accessLog": logPath})
	s := openLocal(t, root, "acme", domain.RoleArchive, config.StorageInstance{Parameters: params})
	ctx := context.Background()
	content := []byte("raw recording")
	if _, err := s.Put(ctx, "k", bytes.NewReader(content), connector.PutOptions{SHA256: storagetest.Sum(content)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, "k", storagetest.Sum(content)); err != nil {
		t.Fatal(err)
	}
	wantClass := func(op, class string) {
		t.Helper()
		for _, line := range accessLines(t, logPath) {
			if line["op"] == op {
				if line["class"] != class || line["role"] != "archive" || line["tenant"] != "acme" || line["key"] != "k" {
					t.Errorf("%s logged as %v", op, line)
				}
				return
			}
		}
		t.Errorf("no %s entry", op)
	}
	wantClass("put", "write")
	wantClass("stat", "metadata")
	wantClass("verify", "verify")
	for _, line := range accessLines(t, logPath) {
		if line["class"] == "read" {
			t.Fatalf("a write-and-verify logged a media read: %v", line)
		}
	}
	rc, _, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	wantClass("get", "read")
}

func TestAccessLogFailureFailsTheOperation(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "access.jsonl")
	params, _ := json.Marshal(map[string]any{"accessLog": logPath})
	s := openLocal(t, root, "acme", domain.RoleArchive, config.StorageInstance{Parameters: params})
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("x"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// Closing the instance closes the log: further logging must fail closed, since a
	// proof that can silently stop logging proves nothing.
	inst, err := local.Open(ctx, connector.StorageParams{Tenant: "acme", Role: domain.RoleArchive, Instance: config.StorageInstance{Root: root, Parameters: params}})
	if err != nil {
		t.Fatal(err)
	}
	s2 := inst.(*local.Storage)
	if err := s2.CloseLogForTest(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if _, _, err := s2.Get(ctx, "k"); err == nil {
		t.Fatal("a read that cannot be logged must fail")
	}
}

func TestDirectAccessComesFromUsageNotDriver(t *testing.T) {
	root := t.TempDir()
	cases := map[config.StorageUsage]bool{
		config.UsageOperational: true,
		config.UsageBackupOnly:  false,
		"":                      false, // undeclared fails closed (I187)
	}
	for usage, want := range cases {
		s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{Usage: usage})
		if got := s.Capabilities().DirectAccess; got != want {
			t.Errorf("usage %q: direct access %v", usage, got)
		}
	}
}

func TestDisabledCapabilitiesAreDeclaredAndRefused(t *testing.T) {
	root := t.TempDir()
	params, _ := json.Marshal(map[string]any{"disableCapabilities": []string{"range_reads", "server_side_copy", "checksum_verification"}})
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{Parameters: params})
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("data"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	caps := s.Capabilities()
	if caps.RangeReads || caps.ServerSideCopy || caps.ChecksumVerification {
		t.Fatalf("capabilities %+v", caps)
	}
	if s.Info().Capabilities["range_reads"] {
		t.Fatal("info still claims range reads")
	}
	check := func(what string, err error) {
		t.Helper()
		if !errs.IsCode(err, errs.CodeCapabilityUnsupported) {
			t.Errorf("%s: %v", what, err)
		}
	}
	_, err := s.DownloadRange(ctx, "k", 0, 1)
	check("range", err)
	_, err = s.Copy(ctx, "k", "c")
	check("copy", err)
	_, err = s.Move(ctx, "k", "c")
	check("move", err)
	_, err = s.Checksum(ctx, "k")
	check("checksum", err)
	_, err = s.Verify(ctx, "k", "x")
	check("verify", err)
}

func TestBadParametersFailClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for name, inst := range map[string]config.StorageInstance{
		"relative root":      {Root: "relative/path"},
		"typo in parameters": {Root: root, Parameters: json.RawMessage(`{"accesLog":"/x"}`)},
		"unknown capability": {Root: root, Parameters: json.RawMessage(`{"disableCapabilities":["nope"]}`)},
		"relative log":       {Root: root, Parameters: json.RawMessage(`{"accessLog":"x.jsonl"}`)},
		"escaping prefix":    {Root: root, Prefix: "../up"},
	} {
		if _, err := local.Open(ctx, connector.StorageParams{Tenant: "acme", Role: domain.RoleMaster, Instance: inst}); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	if _, err := local.Open(ctx, connector.StorageParams{Tenant: "../up", Role: domain.RoleMaster, Instance: config.StorageInstance{Root: root}}); err == nil {
		t.Error("an invalid tenant opened")
	}
}

func TestSymlinkCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	if _, err := s.Put(context.Background(), "a", strings.NewReader("x"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "acme", "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, _, err := s.Get(context.Background(), "link"); err == nil {
		t.Fatal("a symlink read a file outside the root")
	}
}

func TestVersioningKeepsEarlierGenerations(t *testing.T) {
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	ctx := context.Background()
	if !s.Capabilities().Versioning {
		t.Fatal("versioning is on by default")
	}
	first, err := s.Put(ctx, "k", strings.NewReader("one"), connector.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "k", strings.NewReader("two"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	gens, err := s.Generations(ctx, "k")
	if err != nil || len(gens) != 1 || gens[0] != first.Location.Generation {
		t.Fatalf("generations %v, want [%s]: %v", gens, first.Location.Generation, err)
	}
	rc, err := s.GetGeneration(ctx, "k", gens[0])
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != "one" {
		t.Fatalf("generation holds %q", data)
	}
	if _, err := s.GetGeneration(ctx, "k", "../../../secret"); err == nil {
		t.Fatal("an unsafe generation opened")
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	gens, err = s.Generations(ctx, "k")
	if err != nil || len(gens) != 0 {
		t.Fatalf("delete kept generations: %v %v", gens, err)
	}
}

func TestVersioningCanBeTurnedOff(t *testing.T) {
	root := t.TempDir()
	params, _ := json.Marshal(map[string]any{"versioning": false})
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{Parameters: params})
	ctx := context.Background()
	for _, content := range []string{"one", "two"} {
		if _, err := s.Put(ctx, "k", strings.NewReader(content), connector.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if gens, err := s.Generations(ctx, "k"); err != nil || len(gens) != 0 {
		t.Fatalf("generations %v %v", gens, err)
	}
}

// failAfter is a seekable source that fails on the first pass, after n bytes.
type failAfter struct {
	*strings.Reader
	limit int
	read  int
	armed bool
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.armed && f.read >= f.limit {
		return 0, errors.New("connection lost")
	}
	if f.armed && f.read+len(p) > f.limit {
		p = p[:f.limit-f.read]
	}
	n, err := f.Reader.Read(p)
	f.read += n
	return n, err
}

func TestResumableUploadContinuesFromThePartialFile(t *testing.T) {
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	ctx := context.Background()
	content := strings.Repeat("resumable content ", 1<<16)
	src := &failAfter{Reader: strings.NewReader(content), limit: 1 << 18, armed: true}
	opts := connector.PutOptions{IdempotencyKey: "job-node-key", SHA256: storagetest.Sum([]byte(content))}
	if _, err := s.Put(ctx, "big", src, opts); err == nil {
		t.Fatal("first attempt should fail")
	}
	if ok, _ := s.Exists(ctx, "big"); ok {
		t.Fatal("a partial object is visible")
	}
	var firstReport int64 = -1
	opts.Progress = func(done int64) {
		if firstReport < 0 {
			firstReport = done
		}
	}
	src2 := strings.NewReader(content)
	info, err := s.Put(ctx, "big", src2, opts)
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 != opts.SHA256 {
		t.Fatalf("resumed object %+v", info)
	}
	if firstReport <= 1<<17 {
		t.Fatalf("the second attempt started over: first progress %d", firstReport)
	}
}

// Two attempts with one idempotency key must never share a file: a stale writer could
// otherwise append to what the other had already published.
func TestOverlappingAttemptsOwnTheirFiles(t *testing.T) {
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	ctx := context.Background()
	first := strings.Repeat("a", 1<<20)
	second := strings.Repeat("b", 1<<20)
	gate := make(chan struct{})
	stalled := &stallingReader{Reader: strings.NewReader(first), after: 1 << 18, gate: gate, reached: make(chan struct{})}
	opts := connector.PutOptions{IdempotencyKey: "same-key"}
	done := make(chan error, 1)
	go func() {
		_, err := s.Put(ctx, "k", stalled, opts)
		done <- err
	}()
	<-stalled.reached
	// The successor runs start to finish while the first attempt is stalled mid-write.
	info, err := s.Put(ctx, "k", strings.NewReader(second), opts)
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 != storagetest.Sum([]byte(second)) {
		t.Fatalf("%+v", info)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("the stale attempt: %v", err)
	}
	// Whichever renamed last, the object is one attempt's bytes, whole, and it matches
	// its record: never a mix.
	rc, got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != first && string(data) != second {
		t.Fatalf("the object is a mix of two attempts: %d bytes", len(data))
	}
	if got.SHA256 != storagetest.Sum(data) {
		t.Fatalf("object and record disagree: %+v", got)
	}
}

type stallingReader struct {
	*strings.Reader
	after   int
	read    int
	gate    chan struct{}
	reached chan struct{}
	once    sync.Once
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.read >= r.after {
		r.once.Do(func() { close(r.reached) })
		<-r.gate
	}
	if len(p) > r.after && r.read < r.after {
		p = p[:r.after-r.read]
	}
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

// A failed overwrite must leave the object answering, with its own checksum.
func TestFailedOverwriteKeepsTheObject(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs directory permissions to take effect")
	}
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	ctx := context.Background()
	old := "the object before"
	if _, err := s.Put(ctx, "dir/k", strings.NewReader(old), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "acme", "dir")
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	if _, err := s.Put(ctx, "dir/k", strings.NewReader("the object after"), connector.PutOptions{}); err == nil {
		t.Fatal("the overwrite should fail")
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, "dir/k")
	if err != nil {
		t.Fatalf("the failed overwrite emptied the key: %v", err)
	}
	if info.SHA256 != storagetest.Sum([]byte(old)) {
		t.Fatalf("the record was lost: %+v", info)
	}
	rc, _, err := s.Get(ctx, "dir/k")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != old {
		t.Fatalf("%q", data)
	}
}

// A write whose record cannot be stored fails and leaves no object without a checksum.
func TestFailedRecordLeavesNoObject(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs directory permissions to take effect")
	}
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	ctx := context.Background()
	meta := filepath.Join(root, ".vidyom", "meta", "acme", "dir")
	if err := os.MkdirAll(meta, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(meta, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(meta, 0o700) })
	if _, err := s.Put(ctx, "dir/k", strings.NewReader("content"), connector.PutOptions{}); err == nil {
		t.Fatal("the write should fail")
	}
	if ok, err := s.Exists(ctx, "dir/k"); err != nil || ok {
		t.Fatalf("an object with no record stayed: %v %v", ok, err)
	}
}

func TestResumableUploadStartsOverOnDifferentContent(t *testing.T) {
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	ctx := context.Background()
	src := &failAfter{Reader: strings.NewReader(strings.Repeat("a", 1<<20)), limit: 1 << 18, armed: true}
	if _, err := s.Put(ctx, "k", src, connector.PutOptions{IdempotencyKey: "same"}); err == nil {
		t.Fatal("first attempt should fail")
	}
	other := strings.Repeat("b", 1<<20)
	info, err := s.Put(ctx, "k", strings.NewReader(other), connector.PutOptions{IdempotencyKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 != storagetest.Sum([]byte(other)) {
		t.Fatal("stale partial bytes reached the object")
	}
}

func TestStatIgnoresARecordThatDisagreesWithTheFile(t *testing.T) {
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	ctx := context.Background()
	if _, err := s.Put(ctx, "k", strings.NewReader("original"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "acme", "k"), []byte("changed outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 != "" {
		t.Fatalf("stat trusted a stale checksum: %+v", info)
	}
}

func TestListHidesInternalFiles(t *testing.T) {
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	ctx := context.Background()
	for _, content := range []string{"one", "two"} {
		if _, err := s.Put(ctx, "k", strings.NewReader(content), connector.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.List(ctx, connector.ListOptions{})
	if err != nil || len(page.Objects) != 1 || page.Objects[0].Key != "k" {
		t.Fatalf("%+v %v", page, err)
	}
}

func TestTemporaryLinkIsAFileURL(t *testing.T) {
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleProxy, config.StorageInstance{Prefix: "proxies/"})
	ctx := context.Background()
	if _, err := s.Put(ctx, "originals/ORI-001/v1/proxy.mp4", strings.NewReader("x"), connector.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	link, err := s.TemporaryLink(ctx, "originals/ORI-001/v1/proxy.mp4", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "file" {
		t.Fatalf("%q: %v", link, err)
	}
	if _, err := os.Stat(filepath.FromSlash(u.Path)); err != nil {
		t.Fatalf("the link does not point at the file: %v", err)
	}
	if _, err := s.TemporaryLink(ctx, "missing", time.Hour); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("link to a missing object: %v", err)
	}
}

func TestConcurrentWritersLeaveACoherentObject(t *testing.T) {
	root := t.TempDir()
	s := openLocal(t, root, "acme", domain.RoleMaster, config.StorageInstance{})
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			content := strings.Repeat(string(rune('a'+i)), 4096)
			if _, err := s.Put(ctx, "k", strings.NewReader(content), connector.PutOptions{SHA256: storagetest.Sum([]byte(content))}); err != nil {
				t.Errorf("put: %v", err)
			}
		}()
	}
	wg.Wait()
	info, err := s.Stat(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Verify(ctx, "k", info.SHA256)
	if err != nil || !res.OK {
		t.Fatalf("object and record disagree: %+v %v", res, err)
	}
}
