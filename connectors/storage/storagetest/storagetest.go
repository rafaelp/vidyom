// Package storagetest is the conformance suite every storage driver runs: the
// standard operations of §7.2 with the semantics the nodes depend on. It imports
// the testing package, so only test files may import it.
package storagetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Open returns an instance for a tenant in a fresh, empty namespace shared by
// every call of the same test.
type Open func(t *testing.T, tenant domain.TenantID) connector.Storage

// Options adapts the suite to what a driver cannot do in its test environment.
type Options struct {
	// TamperObject overwrites a stored object's bytes behind the driver's back, as
	// corruption or an outside writer would. Optional.
	TamperObject func(t *testing.T, tenant domain.TenantID, key string, content []byte)
}

// Sum returns the SHA-256 of b in hex.
func Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func put(t *testing.T, s connector.Storage, key string, content []byte) connector.ObjectInfo {
	t.Helper()
	info, err := s.Put(context.Background(), key, bytes.NewReader(content), connector.PutOptions{
		SHA256: Sum(content), SizeBytes: int64(len(content)), ContentType: "video/mp4",
	})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return info
}

func read(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer func() {
		if err := rc.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return data
}

func wantNotFound(t *testing.T, what string, err error) {
	t.Helper()
	if !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("%s of a missing object: %v, want not_found", what, err)
	}
}

// Run runs the suite. Tests run one after another, each with its own namespace.
func Run(t *testing.T, open Open, opts Options) {
	t.Helper()
	ctx := context.Background()
	content := []byte("vidyom conformance content: not real media")

	t.Run("PutGetStatRoundTrip", func(t *testing.T) {
		s := open(t, "acme")
		info := put(t, s, "originals/ORI-001/v1/original.mp4", content)
		if info.Key != "originals/ORI-001/v1/original.mp4" {
			t.Errorf("key %q is not relative to the role and tenant", info.Key)
		}
		if info.SizeBytes != int64(len(content)) || info.SHA256 != Sum(content) {
			t.Errorf("put info %+v", info)
		}
		if info.Location.Driver == "" {
			t.Errorf("put info has no location (R30): %+v", info)
		}
		rc, got, err := s.Get(ctx, info.Key)
		if err != nil {
			t.Fatal(err)
		}
		if data := read(t, rc); !bytes.Equal(data, content) {
			t.Errorf("get returned %q", data)
		}
		if got.SHA256 != Sum(content) || got.SizeBytes != int64(len(content)) {
			t.Errorf("get info %+v", got)
		}
		stat, err := s.Stat(ctx, info.Key)
		if err != nil {
			t.Fatal(err)
		}
		if stat.SHA256 != Sum(content) || stat.SizeBytes != int64(len(content)) || stat.ContentType != "video/mp4" {
			t.Errorf("stat %+v", stat)
		}
		if stat.ModTime.IsZero() {
			t.Error("stat has no modification time")
		}
		if ok, err := s.Exists(ctx, info.Key); err != nil || !ok {
			t.Errorf("exists: %v %v", ok, err)
		}
	})

	t.Run("MissingObjects", func(t *testing.T) {
		s := open(t, "acme")
		if ok, err := s.Exists(ctx, "no/such"); err != nil || ok {
			t.Errorf("exists: %v %v", ok, err)
		}
		_, err := s.Stat(ctx, "no/such")
		wantNotFound(t, "stat", err)
		_, _, err = s.Get(ctx, "no/such")
		wantNotFound(t, "get", err)
		_, err = s.DownloadRange(ctx, "no/such", 0, 1)
		wantNotFound(t, "range", err)
		wantNotFound(t, "delete", s.Delete(ctx, "no/such"))
		_, err = s.Checksum(ctx, "no/such")
		wantNotFound(t, "checksum", err)
		_, err = s.Verify(ctx, "no/such", Sum(content))
		wantNotFound(t, "verify", err)
		_, err = s.Copy(ctx, "no/such", "dst")
		wantNotFound(t, "copy", err)
		_, err = s.Move(ctx, "no/such", "dst")
		wantNotFound(t, "move", err)
	})

	t.Run("WrongChecksumLeavesNothing", func(t *testing.T) {
		s := open(t, "acme")
		_, err := s.Put(ctx, "bad", bytes.NewReader(content), connector.PutOptions{SHA256: Sum([]byte("other"))})
		if err == nil {
			t.Fatal("a wrong checksum must fail")
		}
		if ok, err := s.Exists(ctx, "bad"); err != nil || ok {
			t.Errorf("a bad object remained: %v %v", ok, err)
		}
	})

	t.Run("FailedSourceLeavesNothing", func(t *testing.T) {
		s := open(t, "acme")
		src := errorAfter(strings.NewReader(strings.Repeat("x", 1<<20)), 1<<16)
		_, err := s.Put(ctx, "partial", src, connector.PutOptions{})
		if err == nil {
			t.Fatal("a failing source must fail the write")
		}
		if ok, err := s.Exists(ctx, "partial"); err != nil || ok {
			t.Errorf("a partial object remained: %v %v", ok, err)
		}
	})

	t.Run("CancelledPutLeavesNothing", func(t *testing.T) {
		s := open(t, "acme")
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Put(cancelled, "cancelled", bytes.NewReader(content), connector.PutOptions{}); err == nil {
			t.Fatal("a cancelled write must fail")
		}
		if ok, err := s.Exists(ctx, "cancelled"); err != nil || ok {
			t.Errorf("a cancelled object remained: %v %v", ok, err)
		}
	})

	t.Run("OverwriteAndGenerations", func(t *testing.T) {
		s := open(t, "acme")
		first := put(t, s, "replaced", []byte("first"))
		second := put(t, s, "replaced", []byte("second version"))
		if second.SHA256 == first.SHA256 {
			t.Fatal("checksums did not change")
		}
		rc, _, err := s.Get(ctx, "replaced")
		if err != nil {
			t.Fatal(err)
		}
		if data := read(t, rc); string(data) != "second version" {
			t.Errorf("got %q", data)
		}
		if first.Location.Generation == "" || first.Location.Generation == second.Location.Generation {
			t.Errorf("generations %q and %q (R30)", first.Location.Generation, second.Location.Generation)
		}
	})

	t.Run("EmptyObject", func(t *testing.T) {
		s := open(t, "acme")
		info := put(t, s, "empty", nil)
		if info.SizeBytes != 0 || info.SHA256 != Sum(nil) {
			t.Errorf("%+v", info)
		}
		rc, _, err := s.Get(ctx, "empty")
		if err != nil {
			t.Fatal(err)
		}
		if data := read(t, rc); len(data) != 0 {
			t.Errorf("got %q", data)
		}
	})

	t.Run("UnknownSizeAndNoChecksum", func(t *testing.T) {
		s := open(t, "acme")
		info, err := s.Put(ctx, "plain", bytes.NewReader(content), connector.PutOptions{SizeBytes: -1})
		if err != nil {
			t.Fatal(err)
		}
		if info.SHA256 != Sum(content) {
			t.Errorf("the driver must compute the checksum: %+v", info)
		}
	})

	t.Run("Progress", func(t *testing.T) {
		s := open(t, "acme")
		var last int64
		if _, err := s.Put(ctx, "progress", bytes.NewReader(content), connector.PutOptions{
			Progress: func(done int64) { last = done },
		}); err != nil {
			t.Fatal(err)
		}
		if last != int64(len(content)) {
			t.Errorf("progress stopped at %d of %d", last, len(content))
		}
	})

	t.Run("DownloadRange", func(t *testing.T) {
		s := open(t, "acme")
		put(t, s, "ranged", content)
		cases := []struct {
			offset, length int64
			want           []byte
		}{
			{0, 7, content[:7]},
			{7, 11, content[7:18]},
			{8, -1, content[8:]},
			{int64(len(content)) - 3, 3, content[len(content)-3:]},
		}
		for _, c := range cases {
			rc, err := s.DownloadRange(ctx, "ranged", c.offset, c.length)
			if err != nil {
				t.Errorf("range %d+%d: %v", c.offset, c.length, err)
				continue
			}
			if data := read(t, rc); !bytes.Equal(data, c.want) {
				t.Errorf("range %d+%d: %q", c.offset, c.length, data)
			}
		}
		if _, err := s.DownloadRange(ctx, "ranged", int64(len(content))+10, 1); err == nil {
			t.Error("a range past the end must fail")
		}
	})

	t.Run("ListPagesAndPrefix", func(t *testing.T) {
		s := open(t, "acme")
		keys := []string{"a/1", "a/2", "a/3", "b/1", "b/sub/2"}
		for _, key := range keys {
			put(t, s, key, []byte(key))
		}
		var got []string
		cursor := ""
		for page := 0; ; page++ {
			if page > 10 {
				t.Fatal("listing does not end")
			}
			res, err := s.List(ctx, connector.ListOptions{Limit: 2, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Objects) > 2 {
				t.Fatalf("page of %d objects", len(res.Objects))
			}
			for _, obj := range res.Objects {
				got = append(got, obj.Key)
				if obj.SizeBytes != int64(len(obj.Key)) {
					t.Errorf("listed %+v", obj)
				}
			}
			if res.NextCursor == "" {
				break
			}
			cursor = res.NextCursor
		}
		if !slices.Equal(got, keys) {
			t.Errorf("listed %v, want %v", got, keys)
		}
		res, err := s.List(ctx, connector.ListOptions{Prefix: "b/"})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Objects) != 2 || res.Objects[0].Key != "b/1" || res.NextCursor != "" {
			t.Errorf("prefix listing %+v", res)
		}
		res, err = s.List(ctx, connector.ListOptions{Prefix: "none/"})
		if err != nil || len(res.Objects) != 0 {
			t.Errorf("empty listing %+v %v", res, err)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		s := open(t, "acme")
		put(t, s, "gone", content)
		if err := s.Delete(ctx, "gone"); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.Exists(ctx, "gone"); err != nil || ok {
			t.Errorf("exists after delete: %v %v", ok, err)
		}
	})

	t.Run("CopyAndMove", func(t *testing.T) {
		s := open(t, "acme")
		put(t, s, "src", content)
		copied, err := s.Copy(ctx, "src", "dst/copy")
		if err != nil {
			t.Fatal(err)
		}
		if copied.Key != "dst/copy" || copied.SHA256 != Sum(content) || copied.SizeBytes != int64(len(content)) {
			t.Errorf("copy %+v", copied)
		}
		if ok, _ := s.Exists(ctx, "src"); !ok {
			t.Error("copy removed the source")
		}
		moved, err := s.Move(ctx, "src", "dst/moved")
		if err != nil {
			t.Fatal(err)
		}
		if moved.Key != "dst/moved" || moved.SHA256 != Sum(content) {
			t.Errorf("move %+v", moved)
		}
		if ok, _ := s.Exists(ctx, "src"); ok {
			t.Error("move kept the source")
		}
		rc, _, err := s.Get(ctx, "dst/moved")
		if err != nil {
			t.Fatal(err)
		}
		if data := read(t, rc); !bytes.Equal(data, content) {
			t.Errorf("moved content %q", data)
		}
	})

	t.Run("ChecksumAndVerify", func(t *testing.T) {
		s := open(t, "acme")
		put(t, s, "sum", content)
		sum, err := s.Checksum(ctx, "sum")
		if err != nil || sum != Sum(content) {
			t.Errorf("checksum %q: %v", sum, err)
		}
		res, err := s.Verify(ctx, "sum", Sum(content))
		if err != nil || !res.OK || res.SHA256 != Sum(content) || res.SizeBytes != int64(len(content)) {
			t.Errorf("verify %+v: %v", res, err)
		}
		res, err = s.Verify(ctx, "sum", Sum([]byte("other")))
		if err != nil || res.OK {
			t.Errorf("verify against the wrong sum: %+v %v", res, err)
		}
	})

	if opts.TamperObject != nil {
		t.Run("VerifyDetectsTampering", func(t *testing.T) {
			s := open(t, "acme")
			put(t, s, "tampered", content)
			opts.TamperObject(t, "acme", "tampered", []byte("something else entirely"))
			res, err := s.Verify(ctx, "tampered", Sum(content))
			if err != nil {
				t.Fatal(err)
			}
			if res.OK {
				t.Fatal("verify accepted a changed object")
			}
		})
	}

	t.Run("KeysCannotEscape", func(t *testing.T) {
		s := open(t, "acme")
		for _, key := range []string{"../other", "a/../../b", "/abs", "", "a//b", "a\\b", "./a"} {
			if _, err := s.Put(ctx, key, bytes.NewReader(content), connector.PutOptions{}); err == nil {
				t.Errorf("put accepted key %q", key)
			}
			if _, err := s.Stat(ctx, key); err == nil {
				t.Errorf("stat accepted key %q", key)
			}
		}
	})

	t.Run("TenantsAreIsolated", func(t *testing.T) {
		a := open(t, "acme")
		b := open(t, "globex")
		put(t, a, "shared/key", content)
		if ok, err := b.Exists(ctx, "shared/key"); err != nil || ok {
			t.Errorf("tenant globex sees acme's object: %v %v", ok, err)
		}
		res, err := b.List(ctx, connector.ListOptions{})
		if err != nil || len(res.Objects) != 0 {
			t.Errorf("tenant globex lists %+v, %v", res.Objects, err)
		}
		put(t, b, "shared/key", []byte("b's own"))
		rc, _, err := a.Get(ctx, "shared/key")
		if err != nil {
			t.Fatal(err)
		}
		if data := read(t, rc); !bytes.Equal(data, content) {
			t.Errorf("tenant globex overwrote acme's object: %q", data)
		}
	})

	t.Run("Capabilities", func(t *testing.T) {
		s := open(t, "acme")
		caps := s.Capabilities()
		if !caps.RangeReads || !caps.ServerSideCopy || !caps.ChecksumVerification {
			t.Errorf("capabilities %+v", caps)
		}
		info := s.Info()
		if info.Family != connector.FamilyStorage || info.Driver == "" || info.Version == "" || !info.Available {
			t.Errorf("info %+v", info)
		}
		if s.Role() == "" {
			t.Error("no role")
		}
	})
}

func errorAfter(r io.Reader, n int64) io.Reader {
	return io.MultiReader(io.LimitReader(r, n), failingReader{})
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("source failed") }

// Fill returns n bytes of repeating content, for large-object tests.
func Fill(n int) []byte {
	block := []byte(fmt.Sprintf("%064d", n))
	return bytes.Repeat(block, n/len(block)+1)[:n]
}
