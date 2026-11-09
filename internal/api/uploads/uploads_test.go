package uploads_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/api/fakestore"
	"github.com/rafaelp/vidyom/internal/api/uploads"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

const tenantA, tenantB = domain.TenantID("acme"), domain.TenantID("globex")

type fixture struct {
	svc    *uploads.Service
	stores *fakestore.Stores
	spool  string
	scope  service.Scope
	clock  time.Time
	mu     sync.Mutex
}

func (f *fixture) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		stores: fakestore.New(tenantA, tenantB),
		spool:  t.TempDir(),
		scope:  service.Scope{Tenant: tenantA, Principal: domain.Principal{Email: "editor@example.com"}},
		clock:  time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
	}
	svc, err := uploads.New(uploads.Options{Stores: f.stores, SpoolDir: f.spool, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func sumRaw(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func (f *fixture) create(t *testing.T, data []byte) domain.Upload {
	t.Helper()
	u, err := f.svc.Create(context.Background(), f.scope, api.CreateUploadRequest{
		Filename: "class.mp4", SizeBytes: int64(len(data)), SHA256: sumHex(data),
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *fixture) chunk(u domain.Upload, offset int64, body []byte) (domain.Upload, error) {
	return f.svc.AppendChunk(context.Background(), f.scope, id.Ref(u.ID), uploads.Chunk{Offset: offset, Body: bytes.NewReader(body)})
}

func wantCode(t *testing.T, err error, code errs.Code) {
	t.Helper()
	if !errs.IsCode(err, code) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestCreateValidates(t *testing.T) {
	f := newFixture(t)
	good := sumHex([]byte("x"))
	cases := []struct {
		name string
		req  api.CreateUploadRequest
	}{
		{"path in filename", api.CreateUploadRequest{Filename: "../x.mp4", SizeBytes: 1, SHA256: good}},
		{"backslash in filename", api.CreateUploadRequest{Filename: `a\b.mp4`, SizeBytes: 1, SHA256: good}},
		{"control character", api.CreateUploadRequest{Filename: "a\nb.mp4", SizeBytes: 1, SHA256: good}},
		{"empty filename", api.CreateUploadRequest{SizeBytes: 1, SHA256: good}},
		{"zero size", api.CreateUploadRequest{Filename: "x.mp4", SHA256: good}},
		{"bad hash", api.CreateUploadRequest{Filename: "x.mp4", SizeBytes: 1, SHA256: "nope"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.svc.Create(context.Background(), f.scope, c.req)
			wantCode(t, err, errs.CodeInvalidArgument)
		})
	}
}

func TestChunkSizeIsClampedAndHashNormalized(t *testing.T) {
	f := newFixture(t)
	create := func(chunk int64) domain.Upload {
		u, err := f.svc.Create(context.Background(), f.scope, api.CreateUploadRequest{
			Filename: "x.mp4", SizeBytes: 10, SHA256: strings.ToUpper(sumHex([]byte("x"))), ChunkBytes: chunk,
		})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if got := create(0).ChunkBytes; got != api.DefaultChunkBytes {
		t.Errorf("default chunk = %d", got)
	}
	if got := create(10).ChunkBytes; got != uploads.MinChunkBytes {
		t.Errorf("small chunk = %d", got)
	}
	if got := create(1 << 40).ChunkBytes; got != uploads.MaxChunkBytes {
		t.Errorf("large chunk = %d", got)
	}
	if got := create(0).SHA256; got != sumHex([]byte("x")) {
		t.Errorf("hash not normalized: %s", got)
	}
}

func TestResumableFlow(t *testing.T) {
	f := newFixture(t)
	data := []byte(strings.Repeat("vidyom", 1000))
	u := f.create(t, data)

	if u.ID != "UPL-001" || u.Status != domain.UploadPending || u.ReceivedBytes != 0 {
		t.Fatalf("created %+v", u)
	}
	info, err := os.Stat(uploads.Path(f.spool, tenantA, u.ULID))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Errorf("spool file is accessible to others: %o", perm)
	}

	got, err := f.chunk(u, 0, data[:100])
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceivedBytes != 100 {
		t.Fatalf("offset %d", got.ReceivedBytes)
	}

	// Resuming from the wrong offset tells the client where to continue.
	_, err = f.chunk(u, 50, data[50:100])
	wantCode(t, err, errs.CodeConflict)
	var coded *errs.Error
	if !errors.As(err, &coded) || coded.Details["offset"] != int64(100) {
		t.Fatalf("details = %+v", coded)
	}

	// A client that lost state asks.
	state, err := f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	if err != nil || state.ReceivedBytes != 100 {
		t.Fatalf("get: %+v %v", state, err)
	}

	// Completing early refuses.
	_, err = f.svc.Complete(context.Background(), f.scope, id.Ref(u.ID))
	wantCode(t, err, errs.CodeFailedPrecondition)

	// The rest, by ULID this time, with a checksum.
	got, err = f.svc.AppendChunk(context.Background(), f.scope, id.Ref(u.ULID), uploads.Chunk{
		Offset: 100, SHA256: sumRaw(data[100:]), Body: bytes.NewReader(data[100:]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceivedBytes != int64(len(data)) {
		t.Fatalf("offset %d", got.ReceivedBytes)
	}

	done, err := f.svc.Complete(context.Background(), f.scope, id.Ref(u.ID))
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != domain.UploadComplete {
		t.Fatalf("status %s", done.Status)
	}
	onDisk, err := os.ReadFile(uploads.Path(f.spool, tenantA, u.ULID))
	if err != nil || !bytes.Equal(onDisk, data) {
		t.Fatalf("spool content differs: %v", err)
	}

	// Complete again is a no-op; more bytes are refused.
	again, err := f.svc.Complete(context.Background(), f.scope, id.Ref(u.ID))
	if err != nil || again.Status != domain.UploadComplete {
		t.Fatalf("repeat complete: %+v %v", again, err)
	}
	_, err = f.chunk(u, int64(len(data)), []byte("x"))
	wantCode(t, err, errs.CodeFailedPrecondition)
}

func TestBadChunkChecksumRollsBack(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789")
	u := f.create(t, data)
	_, err := f.svc.AppendChunk(context.Background(), f.scope, id.Ref(u.ID), uploads.Chunk{
		Offset: 0, SHA256: sumRaw([]byte("other")), Body: bytes.NewReader(data),
	})
	wantCode(t, err, errs.CodeValidationFailed)

	state, _ := f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	if state.ReceivedBytes != 0 {
		t.Fatalf("offset moved to %d", state.ReceivedBytes)
	}
	info, _ := os.Stat(uploads.Path(f.spool, tenantA, u.ULID))
	if info.Size() != 0 {
		t.Fatalf("file holds %d rejected bytes", info.Size())
	}
}

func TestChunkLimits(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789")
	u := f.create(t, data)

	_, err := f.chunk(u, 0, append(append([]byte{}, data...), 'x'))
	wantCode(t, err, errs.CodeConflict)

	_, err = f.chunk(u, 0, nil)
	wantCode(t, err, errs.CodeInvalidArgument)

	big := bytes.Repeat([]byte("v"), uploads.MinChunkBytes*3)
	v, err := f.svc.Create(context.Background(), f.scope, api.CreateUploadRequest{
		Filename: "x.mp4", SizeBytes: int64(len(big)), SHA256: sumHex(big), ChunkBytes: uploads.MinChunkBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.chunk(v, 0, big[:uploads.MinChunkBytes+1])
	wantCode(t, err, errs.CodePayloadTooLarge)
	info, _ := os.Stat(uploads.Path(f.spool, tenantA, v.ULID))
	if info.Size() != 0 {
		t.Fatalf("file holds %d rejected bytes", info.Size())
	}
}

type failingReader struct{ data []byte }

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errors.New("connection lost")
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestInterruptedChunkLeavesNothing(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789")
	u := f.create(t, data)
	_, err := f.svc.AppendChunk(context.Background(), f.scope, id.Ref(u.ID), uploads.Chunk{
		Offset: 0, Body: &failingReader{data: data[:5]},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	state, _ := f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	if state.ReceivedBytes != 0 {
		t.Fatalf("offset moved to %d", state.ReceivedBytes)
	}
	info, _ := os.Stat(uploads.Path(f.spool, tenantA, u.ULID))
	if info.Size() != 0 {
		t.Fatalf("file holds %d partial bytes", info.Size())
	}
}

func TestCrashBetweenWriteAndCommit(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789")
	u := f.create(t, data)
	path := uploads.Path(f.spool, tenantA, u.ULID)

	// The commit fails: the offset stays and the bytes are rolled back.
	store := f.stores.Store(tenantA)
	store.FailUploadUpdate = errors.New("disk trouble")
	if _, err := f.chunk(u, 0, data[:4]); err == nil {
		t.Fatal("expected the failed commit to surface")
	}
	if info, _ := os.Stat(path); info.Size() != 0 {
		t.Fatalf("file holds %d uncommitted bytes", info.Size())
	}

	// A crash leaves a longer file than the record: the next chunk truncates it.
	if err := os.WriteFile(path, []byte("GARBAGE-FROM-CRASH"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := f.chunk(u, 0, data)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceivedBytes != int64(len(data)) {
		t.Fatalf("offset %d", got.ReceivedBytes)
	}
	if _, err := f.svc.Complete(context.Background(), f.scope, id.Ref(u.ID)); err != nil {
		t.Fatalf("complete after recovery: %v", err)
	}
	onDisk, _ := os.ReadFile(path)
	if !bytes.Equal(onDisk, data) {
		t.Fatalf("content = %q", onDisk)
	}
}

func TestShorterFileLowersTheOffset(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789")
	u := f.create(t, data)
	if _, err := f.chunk(u, 0, data[:6]); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(uploads.Path(f.spool, tenantA, u.ULID), 2); err != nil {
		t.Fatal(err)
	}
	_, err := f.chunk(u, 6, data[6:])
	wantCode(t, err, errs.CodeConflict)
	state, _ := f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	if state.ReceivedBytes != 2 {
		t.Fatalf("offset = %d, want 2", state.ReceivedBytes)
	}
	if _, err := f.chunk(u, 2, data[2:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Complete(context.Background(), f.scope, id.Ref(u.ID)); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteDetectsTamperedFile(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789")
	u := f.create(t, data)
	if _, err := f.chunk(u, 0, data); err != nil {
		t.Fatal(err)
	}
	path := uploads.Path(f.spool, tenantA, u.ULID)
	if err := os.WriteFile(path, []byte("9876543210"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Complete(context.Background(), f.scope, id.Ref(u.ID))
	wantCode(t, err, errs.CodeValidationFailed)

	state, _ := f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	if state.Status != domain.UploadFailed {
		t.Fatalf("status %s", state.Status)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed upload's file kept: %v", err)
	}
	_, err = f.svc.Complete(context.Background(), f.scope, id.Ref(u.ID))
	wantCode(t, err, errs.CodeFailedPrecondition)
}

func TestConcurrentChunksCommitOnce(t *testing.T) {
	f := newFixture(t)
	data := []byte(strings.Repeat("x", 1000))
	u := f.create(t, data)
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = f.chunk(u, 0, data[:500])
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		} else if !errs.IsCode(err, errs.CodeConflict) {
			t.Errorf("unexpected error %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d chunks accepted at the same offset", ok)
	}
	state, _ := f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	if state.ReceivedBytes != 500 {
		t.Fatalf("offset %d", state.ReceivedBytes)
	}
}

func TestTenantIsolation(t *testing.T) {
	f := newFixture(t)
	u := f.create(t, []byte("0123456789"))
	other := service.Scope{Tenant: tenantB}

	_, err := f.svc.Get(context.Background(), other, id.Ref(u.ID))
	wantCode(t, err, errs.CodeNotFound)
	_, err = f.svc.AppendChunk(context.Background(), other, id.Ref(u.ULID), uploads.Chunk{Body: strings.NewReader("x")})
	wantCode(t, err, errs.CodeNotFound)
	_, err = f.svc.Complete(context.Background(), other, id.Ref(u.ID))
	wantCode(t, err, errs.CodeNotFound)
	wantCode(t, f.svc.Abort(context.Background(), other, id.Ref(u.ID)), errs.CodeNotFound)

	// Same friendly number in the other tenant is another file in another directory.
	v, err := f.svc.Create(context.Background(), other, api.CreateUploadRequest{Filename: "y.mp4", SizeBytes: 1, SHA256: sumHex([]byte("y"))})
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != "UPL-001" {
		t.Fatalf("friendly numbering is per tenant: %s", v.ID)
	}
	if filepath.Dir(uploads.Path(f.spool, tenantA, u.ULID)) == filepath.Dir(uploads.Path(f.spool, tenantB, v.ULID)) {
		t.Fatal("tenants share a spool directory")
	}

	// An invalid tenant cannot reach the file system.
	_, err = f.svc.Create(context.Background(), service.Scope{Tenant: "../evil"}, api.CreateUploadRequest{Filename: "y.mp4", SizeBytes: 1, SHA256: sumHex([]byte("y"))})
	wantCode(t, err, errs.CodeInvalidArgument)
	if _, err := os.Stat(filepath.Join(f.spool, "..", "evil")); !os.IsNotExist(err) {
		t.Fatal("a directory escaped the spool")
	}
}

func TestAbortAndBurnedID(t *testing.T) {
	f := newFixture(t)
	u := f.create(t, []byte("0123456789"))
	if err := f.svc.Abort(context.Background(), f.scope, id.Ref(u.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(uploads.Path(f.spool, tenantA, u.ULID)); !os.IsNotExist(err) {
		t.Fatal("file kept after abort")
	}
	_, err := f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	wantCode(t, err, errs.CodeNotFound)
	wantCode(t, f.svc.Abort(context.Background(), f.scope, id.Ref(u.ID)), errs.CodeNotFound)

	next := f.create(t, []byte("abc"))
	if next.ID != "UPL-002" {
		t.Fatalf("friendly ID reused: %s", next.ID)
	}
}

func TestExpiry(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789")
	u := f.create(t, data)

	f.advance(uploads.DefaultTTL + time.Hour)
	state, err := f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	if err != nil || state.Status != domain.UploadExpired {
		t.Fatalf("state %+v, %v", state, err)
	}
	_, err = f.chunk(u, 0, data)
	wantCode(t, err, errs.CodeGone)
	_, err = f.svc.Complete(context.Background(), f.scope, id.Ref(u.ID))
	wantCode(t, err, errs.CodeGone)

	if err := f.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(uploads.Path(f.spool, tenantA, u.ULID)); !os.IsNotExist(err) {
		t.Fatal("expired file kept")
	}
	_, err = f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	wantCode(t, err, errs.CodeNotFound)
}

func TestSweepCatchesCompleteUploadsNobodyUsed(t *testing.T) {
	f := newFixture(t)
	// The spool walk compares the ULID's real time with the clock, so use real time.
	f.mu.Lock()
	f.clock = time.Now()
	f.mu.Unlock()
	data := []byte("0123456789")
	u := f.create(t, data)
	if _, err := f.chunk(u, 0, data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Complete(context.Background(), f.scope, id.Ref(u.ID)); err != nil {
		t.Fatal(err)
	}

	// Inside the TTL it stays: a job may be about to read it.
	if err := f.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := uploads.Path(f.spool, tenantA, u.ULID)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("a recent complete upload was removed")
	}

	f.advance(uploads.DefaultTTL + time.Hour)
	if err := f.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an expired complete upload was kept")
	}
	_, err := f.svc.Get(context.Background(), f.scope, id.Ref(u.ID))
	wantCode(t, err, errs.CodeNotFound)
}

func TestActivityExtendsExpiry(t *testing.T) {
	f := newFixture(t)
	data := []byte("0123456789")
	u := f.create(t, data)
	f.advance(uploads.DefaultTTL - time.Hour)
	got, err := f.chunk(u, 0, data[:5])
	if err != nil {
		t.Fatal(err)
	}
	if !got.ExpiresAt.After(u.ExpiresAt) {
		t.Fatal("a chunk did not extend the expiry")
	}
	f.advance(2 * time.Hour)
	if err := f.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.chunk(u, 5, data[5:]); err != nil {
		t.Fatalf("an active upload was swept: %v", err)
	}
}

func TestSweepRemovesOldOrphansOnly(t *testing.T) {
	f := newFixture(t)
	f.mu.Lock()
	f.clock = time.Now()
	f.mu.Unlock()
	u := f.create(t, []byte("0123456789"))
	dir := filepath.Dir(uploads.Path(f.spool, tenantA, u.ULID))

	// A ULID from now is a create in progress as far as the sweep can tell.
	fresh := filepath.Join(dir, string(id.New()))
	notUlid := filepath.Join(dir, "keep-me")
	for _, p := range []string{fresh, notUlid} {
		if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	f.advance(time.Hour)
	if err := f.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(notUlid); err != nil {
		t.Fatal("a file that is not an upload was removed")
	}

	f.advance(uploads.DefaultTTL + time.Hour)
	if err := f.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatal("an old orphan was kept")
	}
	if _, err := os.Stat(notUlid); err != nil {
		t.Fatal("a file that is not an upload was removed")
	}
}

func TestNewRequiresOptions(t *testing.T) {
	if _, err := uploads.New(uploads.Options{SpoolDir: "/x"}); err == nil {
		t.Error("no stores accepted")
	}
	if _, err := uploads.New(uploads.Options{Stores: fakestore.New()}); err == nil {
		t.Error("no spool accepted")
	}
}
