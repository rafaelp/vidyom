package cache_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/workers/cache"
)

var (
	keyA = bytes.Repeat([]byte{0xA1}, 32)
	keyB = bytes.Repeat([]byte{0xB2}, 32)
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newCache(t *testing.T, opts cache.Options) (*cache.Cache, *clock) {
	t.Helper()
	clk := &clock{now: time.Now()}
	if opts.Root == "" {
		opts.Root = t.TempDir()
	}
	opts.Now = clk.Now
	c, err := cache.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, clk
}

func from(content []byte, calls *atomic.Int64) cache.Download {
	return func(context.Context) (io.ReadCloser, error) {
		if calls != nil {
			calls.Add(1)
		}
		return io.NopCloser(bytes.NewReader(content)), nil
	}
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	files, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".vcache") {
			out = append(out, filepath.Join(dir, f.Name()))
		}
	}
	return out
}

func fetch(t *testing.T, tc *cache.Tenant, content []byte, key string, calls *atomic.Int64) (cache.Result, string) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "plain")
	res, err := tc.Fetch(context.Background(), cache.Request{Role: domain.RoleMaster, Key: key, SHA256: sum(content)}, dst, from(content, calls))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("delivered %d bytes, want %d", len(got), len(content))
	}
	return res, dst
}

func TestMissThenVerifiedHit(t *testing.T) {
	c, _ := newCache(t, cache.Options{})
	tc, err := c.ForTenant("acme", keyA)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("working master "), 200_000) // about 3 MiB, several chunks
	var calls atomic.Int64
	res, _ := fetch(t, tc, content, "originals/ORI-001/v1/master.mp4", &calls)
	if res.Hit || res.SHA256 != sum(content) || res.SizeBytes != int64(len(content)) {
		t.Fatalf("first fetch %+v", res)
	}
	res, _ = fetch(t, tc, content, "originals/ORI-001/v1/master.mp4", &calls)
	if !res.Hit || res.SizeBytes != int64(len(content)) {
		t.Fatalf("second fetch %+v", res)
	}
	if calls.Load() != 1 {
		t.Fatalf("downloaded %d times", calls.Load())
	}
}

func TestEmptyObjectAndExactChunkBoundaries(t *testing.T) {
	c, _ := newCache(t, cache.Options{ChunkSize: 1024})
	tc, _ := c.ForTenant("acme", keyA)
	for _, size := range []int{0, 1, 1023, 1024, 1025, 2048, 4096} {
		content := bytes.Repeat([]byte{byte(size)}, size)
		var calls atomic.Int64
		key := "k" + string(rune('a'+size%26))
		fetch(t, tc, content, key, &calls)
		res, _ := fetch(t, tc, content, key, &calls)
		if !res.Hit || calls.Load() != 1 {
			t.Errorf("size %d: %+v, %d downloads", size, res, calls.Load())
		}
	}
}

func TestEncryptedAtRest(t *testing.T) {
	root := t.TempDir()
	c, _ := newCache(t, cache.Options{Root: root})
	tc, _ := c.ForTenant("acme", keyA)
	marker := []byte("PLAINTEXT-MARKER-of-a-recording")
	content := bytes.Repeat(marker, 1000)
	objectKey := "originals/ORI-001/v1/secret-title-master.mp4"
	fetch(t, tc, content, objectKey, nil)
	files := entries(t, filepath.Join(root, "acme"))
	if len(files) != 1 {
		t.Fatalf("%d entries", len(files))
	}
	if strings.Contains(filepath.Base(files[0]), "secret-title") {
		t.Fatal("the file name reveals the key")
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, marker) {
		t.Fatal("media content is readable on disk")
	}
	if bytes.Contains(data, []byte(objectKey)) || bytes.Contains(data, []byte(sum(content))) {
		t.Fatal("the header is readable on disk")
	}
}

func TestTamperedEntryIsDroppedAndDownloadedAgain(t *testing.T) {
	damage := map[string]func(t *testing.T, path string){
		"bit flip in a chunk": func(t *testing.T, path string) {
			data, _ := os.ReadFile(path)
			data[len(data)/2] ^= 0xFF
			_ = os.WriteFile(path, data, 0o600)
		},
		"truncated mid chunk": func(t *testing.T, path string) {
			st, _ := os.Stat(path)
			_ = os.Truncate(path, st.Size()-10)
		},
		"truncated at a chunk boundary": func(t *testing.T, path string) {
			data, _ := os.ReadFile(path)
			// The final chunk has 100 plaintext bytes, the tag 16, the length 4.
			_ = os.WriteFile(path, data[:len(data)-120], 0o600)
		},
		"empty file": func(t *testing.T, path string) {
			_ = os.Truncate(path, 0)
		},
		"bad magic": func(t *testing.T, path string) {
			data, _ := os.ReadFile(path)
			data[0] ^= 0xFF
			_ = os.WriteFile(path, data, 0o600)
		},
	}
	for name, corrupt := range damage {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			c, _ := newCache(t, cache.Options{Root: root, ChunkSize: 1024})
			tc, _ := c.ForTenant("acme", keyA)
			content := bytes.Repeat([]byte("x"), 2048+100)
			var calls atomic.Int64
			fetch(t, tc, content, "k", &calls)
			corrupt(t, entries(t, filepath.Join(root, "acme"))[0])
			res, _ := fetch(t, tc, content, "k", &calls)
			if res.Hit || calls.Load() != 2 {
				t.Fatalf("damage was not caught: %+v, %d downloads", res, calls.Load())
			}
			res, _ = fetch(t, tc, content, "k", &calls)
			if !res.Hit || calls.Load() != 2 {
				t.Fatalf("the replacement is not reused: %+v, %d downloads", res, calls.Load())
			}
		})
	}
}

// A file moved from another entry has a valid seal but names another object.
func TestEntrySwappedForAnotherIsRefused(t *testing.T) {
	root := t.TempDir()
	c, _ := newCache(t, cache.Options{Root: root})
	tc, _ := c.ForTenant("acme", keyA)
	first := []byte("first object")
	second := []byte("second object")
	fetch(t, tc, first, "first", nil)
	dir := filepath.Join(root, "acme")
	firstPath := entries(t, dir)[0]
	fetch(t, tc, second, "second", nil)
	var secondPath string
	for _, p := range entries(t, dir) {
		if p != firstPath {
			secondPath = p
		}
	}
	data, _ := os.ReadFile(firstPath)
	if err := os.WriteFile(secondPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	res, _ := fetch(t, tc, second, "second", &calls)
	if res.Hit || calls.Load() != 1 {
		t.Fatalf("a swapped entry was served: %+v", res)
	}
}

// §15.6: a worker for tenant A cannot open tenant B's cache.
func TestAnotherTenantsKeyOpensNothing(t *testing.T) {
	root := t.TempDir()
	c, _ := newCache(t, cache.Options{Root: root})
	a, _ := c.ForTenant("acme", keyA)
	content := []byte("acme's master")
	fetch(t, a, content, "k", nil)
	b, _ := c.ForTenant("globex", keyB)
	var calls atomic.Int64
	res, _ := fetch(t, b, content, "k", &calls)
	if res.Hit || calls.Load() != 1 {
		t.Fatalf("a tenant reused another's entry: %+v", res)
	}
	// Even with the file copied into its directory under the same name.
	aFile := entries(t, filepath.Join(root, "acme"))[0]
	data, _ := os.ReadFile(aFile)
	bDir := filepath.Join(root, "globex")
	for _, p := range entries(t, bDir) {
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, _ = fetch(t, b, content, "k", &calls)
	if res.Hit {
		t.Fatal("a foreign entry authenticated")
	}
	if !strings.HasPrefix(entries(t, bDir)[0], bDir) {
		t.Fatal("entry outside the tenant's directory")
	}
}

func TestRoleAndOriginAreRecorded(t *testing.T) {
	c, _ := newCache(t, cache.Options{})
	tc, _ := c.ForTenant("acme", keyA)
	content := []byte("same bytes, two roles")
	var calls atomic.Int64
	dst := filepath.Join(t.TempDir(), "plain")
	ctx := context.Background()
	if _, err := tc.Fetch(ctx, cache.Request{Role: domain.RoleMaster, Key: "k", SHA256: sum(content)}, dst, from(content, &calls)); err != nil {
		t.Fatal(err)
	}
	res, err := tc.Fetch(ctx, cache.Request{Role: domain.RoleProxy, Key: "k", SHA256: sum(content)}, dst, from(content, &calls))
	if err != nil || res.Hit {
		t.Fatalf("an entry of another role was reused: %+v %v", res, err)
	}
	// A changed origin checksum means a stale entry.
	changed := []byte("the object changed at the origin")
	res, err = tc.Fetch(ctx, cache.Request{Role: domain.RoleMaster, Key: "k", SHA256: sum(changed)}, dst, from(changed, &calls))
	if err != nil || res.Hit {
		t.Fatalf("a stale entry was reused: %+v %v", res, err)
	}
}

func TestBadDownloadInstallsNothing(t *testing.T) {
	root := t.TempDir()
	c, _ := newCache(t, cache.Options{Root: root})
	tc, _ := c.ForTenant("acme", keyA)
	dst := filepath.Join(t.TempDir(), "plain")
	_, err := tc.Fetch(context.Background(), cache.Request{Role: domain.RoleMaster, Key: "k", SHA256: sum([]byte("expected"))}, dst, from([]byte("different"), nil))
	if !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("%v", err)
	}
	if len(entries(t, filepath.Join(root, "acme"))) != 0 {
		t.Fatal("a bad download was installed")
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a bad plaintext copy remained")
	}
	files, _ := os.ReadDir(filepath.Join(root, "acme"))
	if len(files) != 0 {
		t.Fatalf("temp files remained: %v", files)
	}
}

func TestDownloadErrorPropagates(t *testing.T) {
	c, _ := newCache(t, cache.Options{})
	tc, _ := c.ForTenant("acme", keyA)
	want := errs.New(errs.CodeForbiddenStorageRole, "refused")
	_, err := tc.Fetch(context.Background(), cache.Request{Role: domain.RoleMaster, Key: "k", SHA256: "abc"}, filepath.Join(t.TempDir(), "p"), func(context.Context) (io.ReadCloser, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("%v", err)
	}
}

func TestIdleExpiryAfterSevenDays(t *testing.T) {
	root := t.TempDir()
	c, clk := newCache(t, cache.Options{Root: root})
	tc, _ := c.ForTenant("acme", keyA)
	content := []byte("old")
	fetch(t, tc, content, "k", nil)
	dir := filepath.Join(root, "acme")

	clk.Advance(6 * 24 * time.Hour)
	if err := c.Sweep(); err != nil {
		t.Fatal(err)
	}
	if len(entries(t, dir)) != 1 {
		t.Fatal("expired too early")
	}
	// A reuse refreshes the idle clock.
	if res, _ := fetch(t, tc, content, "k", nil); !res.Hit {
		t.Fatal("expected a hit")
	}
	clk.Advance(6 * 24 * time.Hour)
	if err := c.Sweep(); err != nil {
		t.Fatal(err)
	}
	if len(entries(t, dir)) != 1 {
		t.Fatal("a reused entry expired")
	}
	clk.Advance(2 * 24 * time.Hour)
	if err := c.Sweep(); err != nil {
		t.Fatal(err)
	}
	if len(entries(t, dir)) != 0 {
		t.Fatal("an entry idle past 7 days survived")
	}
}

func TestLRUEvictsLeastRecentlyUsedPerTenant(t *testing.T) {
	root := t.TempDir()
	c, clk := newCache(t, cache.Options{Root: root, MaxBytes: 2500, ChunkSize: 1024})
	tc, _ := c.ForTenant("acme", keyA)
	other, _ := c.ForTenant("globex", keyB)
	object := func(b byte) []byte { return bytes.Repeat([]byte{b}, 1000) }
	var calls atomic.Int64

	fetch(t, other, object('z'), "z", nil) // another tenant's entry counts apart
	fetch(t, tc, object('a'), "a", &calls)
	clk.Advance(time.Minute)
	fetch(t, tc, object('b'), "b", &calls)
	clk.Advance(time.Minute)
	if res, _ := fetch(t, tc, object('a'), "a", &calls); !res.Hit { // a is now newer than b
		t.Fatal("expected a hit on a")
	}
	clk.Advance(time.Minute)
	fetch(t, tc, object('c'), "c", &calls) // over the limit: b goes

	calls.Store(0)
	if res, _ := fetch(t, tc, object('a'), "a", &calls); !res.Hit {
		t.Error("the recently used entry was evicted")
	}
	if res, _ := fetch(t, tc, object('c'), "c", &calls); !res.Hit {
		t.Error("the newest entry was evicted")
	}
	if res, _ := fetch(t, other, object('z'), "z", nil); !res.Hit {
		t.Error("another tenant's entry was evicted")
	}
	if res, _ := fetch(t, tc, object('b'), "b", &calls); res.Hit {
		t.Error("the least recently used entry survived")
	}
}

func TestConcurrentFetchesDownloadOnce(t *testing.T) {
	c, _ := newCache(t, cache.Options{})
	tc, _ := c.ForTenant("acme", keyA)
	content := bytes.Repeat([]byte("shared "), 100_000)
	var calls atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		dst := filepath.Join(t.TempDir(), "plain")
		go func() {
			defer wg.Done()
			req := cache.Request{Role: domain.RoleMaster, Key: "k", SHA256: sum(content)}
			if _, err := tc.Fetch(context.Background(), req, dst, from(content, &calls)); err != nil {
				t.Errorf("fetch: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("downloaded %d times", calls.Load())
	}
}

func TestCancelledFetchLeavesNothing(t *testing.T) {
	root := t.TempDir()
	c, _ := newCache(t, cache.Options{Root: root, ChunkSize: 1024})
	tc, _ := c.ForTenant("acme", keyA)
	ctx, cancel := context.WithCancel(context.Background())
	dst := filepath.Join(t.TempDir(), "plain")
	body := &cancellingReader{cancel: cancel, data: bytes.Repeat([]byte("x"), 8192)}
	_, err := tc.Fetch(ctx, cache.Request{Role: domain.RoleMaster, Key: "k", SHA256: sum(body.data)}, dst, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(body), nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	files, _ := os.ReadDir(filepath.Join(root, "acme"))
	if len(files) != 0 {
		t.Fatalf("files remained: %v", files)
	}
}

type cancellingReader struct {
	cancel func()
	data   []byte
	done   int
}

func (r *cancellingReader) Read(p []byte) (int, error) {
	if r.done >= 2048 {
		r.cancel()
	}
	n := copy(p, r.data[r.done:])
	r.done += n
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func TestInvalidInputs(t *testing.T) {
	c, _ := newCache(t, cache.Options{})
	if _, err := c.ForTenant("../escape", keyA); err == nil {
		t.Error("an unsafe tenant opened")
	}
	if _, err := c.ForTenant("acme", []byte("short")); err == nil {
		t.Error("a short key opened")
	}
	tc, _ := c.ForTenant("acme", keyA)
	if _, err := tc.Fetch(context.Background(), cache.Request{Role: domain.RoleMaster, Key: "k"}, filepath.Join(t.TempDir(), "p"), nil); err == nil {
		t.Error("a request with no origin checksum was accepted")
	}
	if _, err := cache.New(cache.Options{}); err == nil {
		t.Error("a cache with no directory opened")
	}
}
