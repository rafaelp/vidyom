package apiclient_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/apiclient/apitest"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

const testChunk = 1024

// fixture writes a file of size bytes with content that detects misplaced chunks.
func fixture(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/251)
	}
	path := filepath.Join(t.TempDir(), "class.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

type uploadEnv struct {
	srv     *apitest.Server
	uploads *apitest.Uploads
	client  *apiclient.Client
	store   apiclient.FileCheckpoints
}

func newUploadEnv(t *testing.T) *uploadEnv {
	t.Helper()
	srv := apitest.New(t)
	uploads := srv.EnableUploads(testChunk)
	return &uploadEnv{
		srv: srv, uploads: uploads, client: newUnixClient(t, srv, "acme"),
		store: apiclient.FileCheckpoints{Dir: filepath.Join(t.TempDir(), "uploads")},
	}
}

func (e *uploadEnv) options(path string) apiclient.UploadOptions {
	return apiclient.UploadOptions{
		Path: path, CheckpointKey: "test|acme|" + path, Store: e.store,
		MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
	}
}

func (e *uploadEnv) assertReceived(t *testing.T, ref string, want []byte) {
	t.Helper()
	if !bytes.Equal(e.uploads.Data(ref), want) {
		t.Fatalf("server holds %d bytes that differ from the %d byte file", len(e.uploads.Data(ref)), len(want))
	}
}

func TestUploadHappyPath(t *testing.T) {
	env := newUploadEnv(t)
	path, data := fixture(t, testChunk*3+17)
	var phases []apiclient.UploadPhase
	opts := env.options(path)
	opts.OnProgress = func(phase apiclient.UploadPhase, _, _ int64) {
		if len(phases) == 0 || phases[len(phases)-1] != phase {
			phases = append(phases, phase)
		}
	}
	result, err := env.client.Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if result.Upload.Status != domain.UploadComplete || result.Upload.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("upload = %+v", result.Upload)
	}
	if result.Upload.ID != "UPL-001" {
		t.Fatalf("id = %s", result.Upload.ID)
	}
	if result.Upload.Filename != "class.mp4" {
		t.Fatalf("the filename must be the base name only, got %q", result.Upload.Filename)
	}
	env.assertReceived(t, "UPL-001", data)
	if env.uploads.TotalAccepted() != int64(len(data)) {
		t.Fatalf("accepted %d bytes for a %d byte file", env.uploads.TotalAccepted(), len(data))
	}
	if len(env.srv.Calls(api.RouteUploadsChunk)) != 4 {
		t.Fatalf("chunks = %d", len(env.srv.Calls(api.RouteUploadsChunk)))
	}
	if len(phases) != 2 || phases[0] != apiclient.PhaseHashing || phases[1] != apiclient.PhaseUploading {
		t.Fatalf("phases = %v", phases)
	}
	// The checkpoint stays until the caller's follow-up was accepted.
	if cp, _ := env.store.Load(opts.CheckpointKey); cp == nil || cp.UploadID != "UPL-001" {
		t.Fatalf("checkpoint = %+v", cp)
	}
}

func TestUploadCarriesChecksumAndOffset(t *testing.T) {
	env := newUploadEnv(t)
	path, _ := fixture(t, testChunk+5)
	if _, err := env.client.Upload(context.Background(), env.options(path)); err != nil {
		t.Fatal(err)
	}
	calls := env.srv.Calls(api.RouteUploadsChunk)
	if len(calls) != 2 {
		t.Fatalf("chunks = %d", len(calls))
	}
	if calls[0].Header.Get(api.HeaderUploadOffset) != "0" || calls[1].Header.Get(api.HeaderUploadOffset) != "1024" {
		t.Fatalf("offsets = %s, %s", calls[0].Header.Get(api.HeaderUploadOffset), calls[1].Header.Get(api.HeaderUploadOffset))
	}
	for _, c := range calls {
		if !strings.HasPrefix(c.Header.Get(api.HeaderUploadChecksum), "sha256 ") {
			t.Fatalf("checksum header = %q", c.Header.Get(api.HeaderUploadChecksum))
		}
	}
}

func TestUploadRetriesTransientFailures(t *testing.T) {
	cases := []struct {
		name   string
		inject func(*apitest.Uploads) func(http.ResponseWriter, *http.Request) bool
	}{
		{"unavailable", func(*apitest.Uploads) func(http.ResponseWriter, *http.Request) bool {
			return apitest.Status(errs.CodeUnavailable)
		}},
		{"internal", func(*apitest.Uploads) func(http.ResponseWriter, *http.Request) bool {
			return apitest.Status(errs.CodeInternal)
		}},
		{"rate limited", func(*apitest.Uploads) func(http.ResponseWriter, *http.Request) bool {
			return apitest.Status(errs.CodeRateLimited)
		}},
		{"dropped connection", func(*apitest.Uploads) func(http.ResponseWriter, *http.Request) bool {
			return apitest.DropConnection()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newUploadEnv(t)
			path, data := fixture(t, testChunk*3)
			for i := 0; i < 3; i++ {
				env.uploads.FailNextChunk(tc.inject(env.uploads))
			}
			result, err := env.client.Upload(context.Background(), env.options(path))
			if err != nil {
				t.Fatal(err)
			}
			env.assertReceived(t, string(result.Upload.ID), data)
			if env.uploads.TotalAccepted() != int64(len(data)) {
				t.Fatalf("accepted %d bytes for a %d byte file", env.uploads.TotalAccepted(), len(data))
			}
		})
	}
}

func TestUploadResumesFromPartialChunkAfterDrop(t *testing.T) {
	env := newUploadEnv(t)
	path, data := fixture(t, testChunk*2)
	env.uploads.FailNextChunk(env.uploads.PartialWrite(100))
	result, err := env.client.Upload(context.Background(), env.options(path))
	if err != nil {
		t.Fatal(err)
	}
	env.assertReceived(t, string(result.Upload.ID), data)
}

func TestUploadRecoversFromWrongOffset(t *testing.T) {
	env := newUploadEnv(t)
	path, data := fixture(t, testChunk*2)
	// A 409 where the server's offset is not what the client sent.
	env.uploads.FailNextChunk(env.uploads.PartialWrite(0))
	env.uploads.FailNextChunk(func(w http.ResponseWriter, _ *http.Request) bool {
		apitest.WriteError(w, errs.New(errs.CodeConflict, "wrong offset"))
		return true
	})
	result, err := env.client.Upload(context.Background(), env.options(path))
	if err != nil {
		t.Fatal(err)
	}
	env.assertReceived(t, string(result.Upload.ID), data)
}

func TestUploadGivesUpWithoutProgressAndKeepsCheckpoint(t *testing.T) {
	env := newUploadEnv(t)
	path, _ := fixture(t, testChunk)
	for i := 0; i < 20; i++ {
		env.uploads.FailNextChunk(apitest.Status(errs.CodeUnavailable))
	}
	opts := env.options(path)
	opts.MaxAttempts = 3
	_, err := env.client.Upload(context.Background(), opts)
	if !errs.IsCode(err, errs.CodeUnavailable) || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("err = %v", err)
	}
	if n := len(env.srv.Calls(api.RouteUploadsChunk)); n > 3 {
		t.Fatalf("%d chunk attempts for a budget of 3", n)
	}
	if cp, _ := env.store.Load(opts.CheckpointKey); cp == nil || cp.UploadID == "" {
		t.Fatal("a resumable failure must keep its checkpoint")
	}
}

func TestUploadPermanentErrorStopsAtOnce(t *testing.T) {
	env := newUploadEnv(t)
	path, _ := fixture(t, testChunk)
	env.uploads.FailNextChunk(apitest.Status(errs.CodeForbidden))
	_, err := env.client.Upload(context.Background(), env.options(path))
	if !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("err = %v", err)
	}
	if n := len(env.srv.Calls(api.RouteUploadsChunk)); n != 1 {
		t.Fatalf("%d attempts; a permanent error must not be retried", n)
	}
}

func TestUploadResumeAcrossRunsReusesUploadAndHash(t *testing.T) {
	env := newUploadEnv(t)
	path, data := fixture(t, testChunk*4)
	opts := env.options(path)

	// First run stops after two chunks, as if the process died.
	ctx, cancel := context.WithCancel(context.Background())
	env.uploads.OnChunk = func(received int64) {
		if received >= testChunk*2 {
			cancel()
		}
	}
	if _, err := env.client.Upload(ctx, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v", err)
	}
	env.uploads.OnChunk = nil

	// Second run: nothing is hashed again and nothing is sent twice.
	hashed := false
	opts.OnProgress = func(phase apiclient.UploadPhase, _, _ int64) {
		if phase == apiclient.PhaseHashing {
			hashed = true
		}
	}
	result, err := env.client.Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if hashed {
		t.Error("an unchanged file must not be hashed again")
	}
	if !result.Resumed || result.ResumedFrom != testChunk*2 {
		t.Errorf("resumed = %v from %d", result.Resumed, result.ResumedFrom)
	}
	if env.uploads.Created() != 1 {
		t.Errorf("created %d uploads", env.uploads.Created())
	}
	env.assertReceived(t, string(result.Upload.ID), data)
	if env.uploads.TotalAccepted() != int64(len(data)) {
		t.Errorf("accepted %d bytes for a %d byte file", env.uploads.TotalAccepted(), len(data))
	}
}

func TestUploadKillBeforeCreateAnswerReusesIdempotencyKey(t *testing.T) {
	env := newUploadEnv(t)
	path, _ := fixture(t, testChunk)
	opts := env.options(path)

	// The create reached the server, but the client died before saving the ID: the
	// checkpoint exists with its key and without an upload ID.
	first, err := env.client.Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	cp, _ := env.store.Load(opts.CheckpointKey)
	cp.UploadID, cp.UploadULID = "", ""
	if err := env.store.Save(opts.CheckpointKey, *cp); err != nil {
		t.Fatal(err)
	}

	second, err := env.client.Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Upload.ID != first.Upload.ID || env.uploads.Created() != 1 {
		t.Fatalf("the same key must find the same upload: %s, %s, created %d", first.Upload.ID, second.Upload.ID, env.uploads.Created())
	}
}

func TestUploadAlreadyCompleteIsNotResent(t *testing.T) {
	env := newUploadEnv(t)
	path, _ := fixture(t, testChunk)
	opts := env.options(path)
	if _, err := env.client.Upload(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	chunks := len(env.srv.Calls(api.RouteUploadsChunk))
	result, err := env.client.Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Upload.Status != domain.UploadComplete {
		t.Fatalf("status = %s", result.Upload.Status)
	}
	if len(env.srv.Calls(api.RouteUploadsChunk)) != chunks || env.uploads.Completed() != 1 {
		t.Fatal("a complete upload must not be sent or completed again")
	}
}

func TestUploadExpiredRestartsOnce(t *testing.T) {
	env := newUploadEnv(t)
	path, data := fixture(t, testChunk*2)
	opts := env.options(path)
	if _, err := env.client.Upload(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	env.uploads.Expire("UPL-001")
	result, err := env.client.Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Upload.ID != "UPL-002" {
		t.Fatalf("id = %s", result.Upload.ID)
	}
	env.assertReceived(t, "UPL-002", data)
}

func TestUploadChangedFileStartsOver(t *testing.T) {
	env := newUploadEnv(t)
	path, _ := fixture(t, testChunk*2)
	opts := env.options(path)
	ctx, cancel := context.WithCancel(context.Background())
	env.uploads.OnChunk = func(int64) { cancel() }
	if _, err := env.client.Upload(ctx, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v", err)
	}
	env.uploads.OnChunk = nil

	changed := bytes.Repeat([]byte("x"), testChunk+9)
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := env.client.Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Upload.ID != "UPL-002" {
		t.Fatalf("id = %s, a changed file must not reuse the old upload", result.Upload.ID)
	}
	env.assertReceived(t, "UPL-002", changed)
	old, _ := env.uploads.Record("UPL-001")
	if old.Status != domain.UploadFailed {
		t.Errorf("the abandoned upload should be aborted, status = %s", old.Status)
	}
	if len(env.srv.Calls(api.RouteUploadsAbort)) != 1 {
		t.Error("expected one abort")
	}
}

func TestUploadDigestRejectionFailsAndForgetsCheckpoint(t *testing.T) {
	env := newUploadEnv(t)
	path, _ := fixture(t, testChunk)
	opts := env.options(path)
	env.srv.Error(api.RouteUploadsComplete, errs.New(errs.CodeValidationFailed, "size or SHA-256 mismatch"))
	_, err := env.client.Upload(context.Background(), opts)
	if !errs.IsCode(err, errs.CodeValidationFailed) {
		t.Fatalf("err = %v", err)
	}
	if cp, _ := env.store.Load(opts.CheckpointKey); cp != nil {
		t.Fatal("a rejected digest must not be resumed into the same rejection")
	}
}

func TestUploadResendsChunkWithBadDigest(t *testing.T) {
	env := newUploadEnv(t)
	path, data := fixture(t, testChunk*2)
	env.uploads.FailNextChunk(apitest.Status(errs.CodeValidationFailed))
	result, err := env.client.Upload(context.Background(), env.options(path))
	if err != nil {
		t.Fatal(err)
	}
	env.assertReceived(t, string(result.Upload.ID), data)
}

func TestUploadCompleteReportingLowerOffsetContinues(t *testing.T) {
	env := newUploadEnv(t)
	path, data := fixture(t, testChunk*2)
	refused := false
	complete := env.srv.Handler(api.RouteUploadsComplete)
	env.srv.Handle(api.RouteUploadsComplete, func(w http.ResponseWriter, r *http.Request) {
		if !refused {
			refused = true
			env.uploads.Truncate("UPL-001", testChunk)
			apitest.WriteError(w, errs.New(errs.CodeFailedPrecondition, "upload has fewer bytes").With("offset", testChunk))
			return
		}
		complete(w, r)
	})
	result, err := env.client.Upload(context.Background(), env.options(path))
	if err != nil {
		t.Fatal(err)
	}
	env.assertReceived(t, string(result.Upload.ID), data)
}

func TestUploadRefusesEmptyAndNonRegular(t *testing.T) {
	env := newUploadEnv(t)
	empty := filepath.Join(t.TempDir(), "empty.mp4")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{empty, t.TempDir(), filepath.Join(t.TempDir(), "missing.mp4")} {
		if _, err := env.client.Upload(context.Background(), env.options(path)); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: err = %v", path, err)
		}
	}
	if len(env.srv.Calls()) != 0 {
		t.Fatal("nothing may reach the server")
	}
}

func TestDerivedKeyIsStableAndDistinct(t *testing.T) {
	if apiclient.DerivedKey("a", "b") != apiclient.DerivedKey("a", "b") {
		t.Fatal("not stable")
	}
	if apiclient.DerivedKey("ab", "c") == apiclient.DerivedKey("a", "bc") {
		t.Fatal("parts must be separated")
	}
}

func TestFileCheckpointsArePrivateAndAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uploads")
	store := apiclient.FileCheckpoints{Dir: dir}
	if err := store.Save("k", apiclient.Checkpoint{Path: "/x", SizeBytes: 1}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	info, _ := entries[0].Info()
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	if dirInfo, _ := os.Stat(dir); dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o", dirInfo.Mode().Perm())
	}
	cp, err := store.Load("k")
	if err != nil || cp == nil || cp.Path != "/x" {
		t.Fatalf("cp = %+v, %v", cp, err)
	}
	if err := store.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("k"); err != nil {
		t.Fatal("deleting twice must succeed")
	}
	if cp, err := store.Load("k"); err != nil || cp != nil {
		t.Fatalf("after delete: %+v, %v", cp, err)
	}
}
