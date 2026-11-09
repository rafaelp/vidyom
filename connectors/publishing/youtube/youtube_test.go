package youtube_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/connectors/publishing/youtube"
	"github.com/rafaelp/vidyom/connectors/publishing/youtube/youtubetest"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

const (
	key       = "9f2b7c1e0d4a4f3e8a6b5c7d9e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b"
	chunkSize = 256 * 1024
)

type secrets struct {
	values map[string][]byte
	err    error
}

func (s *secrets) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	v, ok := s.values[ref]
	if !ok {
		return nil, errs.NotFound("secret")
	}
	return v, nil
}

type fixture struct {
	t       *testing.T
	server  *youtubetest.Server
	secrets *secrets
	params  map[string]any
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	server := youtubetest.New()
	t.Cleanup(server.Close)
	return &fixture{
		t:       t,
		server:  server,
		secrets: &secrets{values: map[string][]byte{"youtube-oauth": youtubetest.Credentials()}},
		params:  map[string]any{},
	}
}

// open builds a fresh connector, which stands for a restarted daemon.
func (f *fixture) open() *youtube.Connector {
	f.t.Helper()
	reg := connector.NewRegistry()
	youtube.Register(reg)
	factory, err := reg.Publishing(youtube.Driver)
	if err != nil {
		f.t.Fatal(err)
	}
	conn, err := factory(context.Background(), connector.PublishingParams{
		Tenant: "acme", Destination: "youtube-main", Secrets: f.secrets,
		Config: config.Destination{Driver: youtube.Driver, CredentialsRef: "youtube-oauth", Parameters: f.server.Parameters(f.params)},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return conn.(*youtube.Connector)
}

func content(size int) string {
	var b strings.Builder
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "fictional video byte run %d ", i)
	}
	return b.String()[:size]
}

func shaOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func upload(data string) connector.UploadRequest {
	return connector.UploadRequest{
		IdempotencyKey: key,
		File:           strings.NewReader(data),
		SizeBytes:      int64(len(data)),
		SHA256:         shaOf(data),
		ContentType:    "video/mp4",
		Metadata: connector.VideoMetadata{
			Title: "Conscious breathing", Description: "A clip about breath.",
			Tags: []string{"yoga", "breathing"}, Language: "en", Visibility: domain.VisibilityPrivate,
		},
	}
}

func TestUploadSendsChunksWithMetadataAndProgress(t *testing.T) {
	f := newFixture(t)
	data := content(3*chunkSize + 100)
	req := upload(data)
	var progress []int64
	req.Progress = func(n int64) { progress = append(progress, n) }

	remote, err := f.open().Upload(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if remote.Status != connector.RemoteProcessing || remote.URL != "" || remote.Visibility != domain.VisibilityPrivate {
		t.Fatalf("a video not yet processed has no URL (R43): %+v", remote)
	}
	if f.server.Count(youtubetest.OpInitiate) != 1 || f.server.Count(youtubetest.OpChunk) != 3 || f.server.Count(youtubetest.OpFinal) != 1 {
		t.Fatalf("initiate %d chunks %d final %d", f.server.Count(youtubetest.OpInitiate), f.server.Count(youtubetest.OpChunk), f.server.Count(youtubetest.OpFinal))
	}
	v, ok := f.server.Video(remote.RemoteID)
	if !ok {
		t.Fatal("no video")
	}
	if v.SHA256 != shaOf(data) || v.Size != int64(len(data)) {
		t.Fatalf("bytes differ: %+v", v)
	}
	if v.Title != "Conscious breathing" || v.Description != "A clip about breath." || v.Privacy != "private" || v.CategoryID != "22" || v.DefaultLanguage != "en" {
		t.Fatalf("metadata %+v", v)
	}
	if len(v.Tags) != 3 || v.Tags[0] != "vidyom-"+key[:32] || v.Tags[1] != "yoga" {
		t.Fatalf("tags %v", v.Tags)
	}
	if len(progress) != 3 || progress[0] != chunkSize || progress[2] != 3*chunkSize {
		t.Fatalf("progress %v", progress)
	}
}

func TestUploadRepeatWithTheSameKeyMakesNoSecondVideo(t *testing.T) {
	f := newFixture(t)
	conn := f.open()
	data := content(chunkSize)
	first, err := conn.Upload(context.Background(), upload(data))
	if err != nil {
		t.Fatal(err)
	}
	again, err := conn.Upload(context.Background(), upload(data))
	if err != nil {
		t.Fatal(err)
	}
	if again.RemoteID != first.RemoteID || len(f.server.Videos()) != 1 || f.server.Count(youtubetest.OpInitiate) != 1 {
		t.Fatalf("second answer %+v, videos %d", again, len(f.server.Videos()))
	}
}

func TestUploadResumesAfterServerErrorsAndLostConnections(t *testing.T) {
	cases := map[string]func(*youtubetest.Server){
		"503 on a chunk": func(s *youtubetest.Server) {
			s.SkipThenFail(youtubetest.OpChunk, 1, youtubetest.Fault{Status: 503, Reason: "backendError"})
		},
		"connection lost before the chunk is read": func(s *youtubetest.Server) {
			s.SkipThenFail(youtubetest.OpChunk, 1, youtubetest.Fault{Drop: true})
		},
		"part of a chunk stored before the connection is lost": func(s *youtubetest.Server) {
			s.SkipThenFail(youtubetest.OpChunk, 1, youtubetest.Fault{KeepBytes: 1000})
		},
		"a chunk stored but its answer lost": func(s *youtubetest.Server) {
			s.SkipThenFail(youtubetest.OpChunk, 1, youtubetest.Fault{DropAfter: true})
		},
		"the status query fails too": func(s *youtubetest.Server) {
			s.SkipThenFail(youtubetest.OpChunk, 1, youtubetest.Fault{Status: 502})
			s.FailNext(youtubetest.OpStatus, youtubetest.Fault{Status: 503}, youtubetest.Fault{Drop: true})
		},
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			inject(f.server)
			data := content(3*chunkSize + 7)
			remote, err := f.open().Upload(context.Background(), upload(data))
			if err != nil {
				t.Fatal(err)
			}
			if f.server.Count(youtubetest.OpStatus) == 0 {
				t.Fatal("recovery must ask the session where it stands")
			}
			if f.server.Count(youtubetest.OpInitiate) != 1 {
				t.Fatal("recovery must not open a new session")
			}
			v, _ := f.server.Video(remote.RemoteID)
			if v.SHA256 != shaOf(data) {
				t.Fatal("the bytes differ after resuming")
			}
			if len(f.server.Videos()) != 1 {
				t.Fatal("exactly one video")
			}
		})
	}
}

func TestLostFinalAnswerIsSettledByTheSession(t *testing.T) {
	f := newFixture(t)
	f.server.FailNext(youtubetest.OpFinal, youtubetest.Fault{DropAfter: true})
	remote, err := f.open().Upload(context.Background(), upload(content(chunkSize+1)))
	if err != nil {
		t.Fatalf("the session knows it finished: %v", err)
	}
	if remote.RemoteID == "" || len(f.server.Videos()) != 1 {
		t.Fatalf("remote %+v, videos %d", remote, len(f.server.Videos()))
	}
}

// D11: unknown only when the last byte may have arrived.
func TestOutcomeClassification(t *testing.T) {
	t.Run("giving up before the last byte is plainly unavailable", func(t *testing.T) {
		f := newFixture(t)
		f.server.SkipThenFail(youtubetest.OpChunk, 1, repeat(youtubetest.Fault{Status: 503}, 20)...)
		f.server.FailNext(youtubetest.OpStatus, repeat(youtubetest.Fault{Status: 503}, 20)...)
		conn := f.open()
		_, err := conn.Upload(context.Background(), upload(content(3*chunkSize+1)))
		if connector.IsOutcomeUnknown(err) || !errs.IsCode(err, errs.CodeUnavailable) {
			t.Fatalf("got %v", err)
		}
		if len(f.server.Videos()) != 0 {
			t.Fatal("no video may exist")
		}
	})

	t.Run("giving up after the last byte is unknown, and reconcile settles it", func(t *testing.T) {
		f := newFixture(t)
		f.server.FailNext(youtubetest.OpFinal, youtubetest.Fault{DropAfter: true})
		f.server.FailNext(youtubetest.OpStatus, repeat(youtubetest.Fault{Status: 503}, 4)...)
		conn := f.open()
		data := content(chunkSize + 1)
		_, err := conn.Upload(context.Background(), upload(data))
		if !connector.IsOutcomeUnknown(err) {
			t.Fatalf("got %v", err)
		}
		found, err := conn.Reconcile(context.Background(), key)
		if err != nil || found == nil {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
		// The video stays the only one, and a repeat answers with it.
		again, err := conn.Upload(context.Background(), upload(data))
		if err != nil || again.RemoteID != found.RemoteID || len(f.server.Videos()) != 1 {
			t.Fatalf("repeat: %+v, %v, videos %d", again, err, len(f.server.Videos()))
		}
	})

	t.Run("a finishing answer that cannot be read is unknown", func(t *testing.T) {
		f := newFixture(t)
		garbled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, "<html>not json</html>")
		}))
		defer garbled.Close()
		// Initiation answers with a session whose chunks get the garbled answer.
		sessionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", garbled.URL+"/session")
			w.WriteHeader(http.StatusOK)
		}))
		defer sessionServer.Close()
		f.params["uploadBaseUrl"] = sessionServer.URL
		_, err := f.open().Upload(context.Background(), upload(content(chunkSize)))
		if !connector.IsOutcomeUnknown(err) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("cancelling mid-upload is not unknown", func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := upload(content(3*chunkSize + 1))
		req.Progress = func(int64) { cancel() }
		_, err := f.open().Upload(ctx, req)
		if !errors.Is(err, context.Canceled) || connector.IsOutcomeUnknown(err) {
			t.Fatalf("got %v", err)
		}
		if len(f.server.Videos()) != 0 {
			t.Fatal("no video may exist")
		}
	})

	t.Run("cancelling while the last byte is out is unknown", func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := upload(content(chunkSize))
		req.File = cancellingReader{ReaderAt: req.File, cancel: cancel}
		_, err := f.open().Upload(ctx, req)
		if !connector.IsOutcomeUnknown(err) {
			t.Fatalf("got %v", err)
		}
	})
}

// cancellingReader cancels as soon as the body is read, leaving the request in flight.
type cancellingReader struct {
	io.ReaderAt
	cancel func()
}

func (c cancellingReader) ReadAt(p []byte, off int64) (int, error) {
	c.cancel()
	return c.ReaderAt.ReadAt(p, off)
}

func repeat(f youtubetest.Fault, n int) []youtubetest.Fault {
	out := make([]youtubetest.Fault, n)
	for i := range out {
		out[i] = f
	}
	return out
}

func TestExpiredSession(t *testing.T) {
	t.Run("before the last byte it starts over, with one video", func(t *testing.T) {
		f := newFixture(t)
		f.server.SkipThenFail(youtubetest.OpChunk, 1, youtubetest.Fault{Status: 404, Reason: "uploadSessionNotFound"})
		data := content(3*chunkSize + 1)
		remote, err := f.open().Upload(context.Background(), upload(data))
		if err != nil {
			t.Fatal(err)
		}
		if f.server.Count(youtubetest.OpInitiate) != 2 || len(f.server.Videos()) != 1 {
			t.Fatalf("initiations %d, videos %d", f.server.Count(youtubetest.OpInitiate), len(f.server.Videos()))
		}
		if v, _ := f.server.Video(remote.RemoteID); v.SHA256 != shaOf(data) {
			t.Fatal("bytes differ")
		}
	})

	t.Run("keeps failing, it gives up", func(t *testing.T) {
		f := newFixture(t)
		f.server.FailNext(youtubetest.OpChunk, repeat(youtubetest.Fault{Status: 404}, 3)...)
		_, err := f.open().Upload(context.Background(), upload(content(3*chunkSize+1)))
		if !errs.IsCode(err, errs.CodeUnavailable) || connector.IsOutcomeUnknown(err) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("after the last byte it is unknown, and the tag scan settles it", func(t *testing.T) {
		f := newFixture(t)
		conn := f.open()
		f.server.FailNext(youtubetest.OpFinal, youtubetest.Fault{DropAfter: true})
		f.server.FailNext(youtubetest.OpStatus, youtubetest.Fault{Status: 404})
		_, err := conn.Upload(context.Background(), upload(content(chunkSize)))
		if !connector.IsOutcomeUnknown(err) {
			t.Fatalf("got %v", err)
		}
		f.server.ExpireSessions()
		found, err := conn.Reconcile(context.Background(), key)
		if err != nil || found == nil || found.RemoteID != f.server.Videos()[0].ID {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
		if f.server.Count(youtubetest.OpPlaylist) == 0 {
			t.Fatal("must have scanned")
		}
	})
}

func TestRestartWithPersistedSessions(t *testing.T) {
	newPersistent := func(t *testing.T) *fixture {
		f := newFixture(t)
		f.params["stateDir"] = t.TempDir()
		return f
	}

	t.Run("a killed upload resumes where the session stands", func(t *testing.T) {
		f := newPersistent(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		data := content(4*chunkSize + 9)
		req := upload(data)
		chunks := 0
		req.Progress = func(int64) {
			chunks++
			if chunks == 2 {
				cancel()
			}
		}
		if _, err := f.open().Upload(ctx, req); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}

		conn := f.open() // the daemon restarted
		found, err := conn.Reconcile(context.Background(), key)
		if err != nil || found != nil {
			t.Fatalf("an open session has made no video: %+v, %v", found, err)
		}
		if f.server.Count(youtubetest.OpPlaylist) != 0 {
			t.Fatal("no scan is needed with a session record")
		}
		before := f.server.Count(youtubetest.OpChunk)
		remote, err := conn.Upload(context.Background(), upload(data))
		if err != nil {
			t.Fatal(err)
		}
		if f.server.Count(youtubetest.OpInitiate) != 1 {
			t.Fatal("the session must be reused")
		}
		if sent := f.server.Count(youtubetest.OpChunk) - before; sent != 2 {
			t.Fatalf("resumed with %d more chunks, want 2", sent)
		}
		if v, _ := f.server.Video(remote.RemoteID); v.SHA256 != shaOf(data) {
			t.Fatal("bytes differ")
		}
	})

	t.Run("a crash after finishing finds the video through its session", func(t *testing.T) {
		f := newPersistent(t)
		// The final answer is lost and contact is gone, so the session stays unfinished
		// in the record while YouTube has finished it.
		f.server.FailNext(youtubetest.OpFinal, youtubetest.Fault{DropAfter: true})
		f.server.FailNext(youtubetest.OpStatus, repeat(youtubetest.Fault{Status: 503}, 4)...)
		if _, err := f.open().Upload(context.Background(), upload(content(chunkSize))); !connector.IsOutcomeUnknown(err) {
			t.Fatalf("got %v", err)
		}
		found, err := f.open().Reconcile(context.Background(), key)
		if err != nil || found == nil || found.RemoteID == "" {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
		if f.server.Count(youtubetest.OpPlaylist) != 0 {
			t.Fatal("the session is the answer, no scan")
		}
	})

	t.Run("a finished record answers from the video", func(t *testing.T) {
		f := newPersistent(t)
		data := content(chunkSize)
		first, err := f.open().Upload(context.Background(), upload(data))
		if err != nil {
			t.Fatal(err)
		}
		conn := f.open()
		found, err := conn.Reconcile(context.Background(), key)
		if err != nil || found == nil || found.RemoteID != first.RemoteID {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
		again, err := conn.Upload(context.Background(), upload(data))
		if err != nil || again.RemoteID != first.RemoteID || f.server.Count(youtubetest.OpInitiate) != 1 {
			t.Fatalf("repeat: %+v, %v", again, err)
		}
		// Deleted at YouTube since: reconcile says so, rather than "never existed".
		if err := conn.Delete(context.Background(), first.RemoteID); err != nil {
			t.Fatal(err)
		}
		gone, err := conn.Reconcile(context.Background(), key)
		if err != nil || gone == nil || gone.Status != connector.RemoteRemoved {
			t.Fatalf("reconcile: %+v, %v", gone, err)
		}
	})

	t.Run("no record proves nothing was sent", func(t *testing.T) {
		f := newPersistent(t)
		found, err := f.open().Reconcile(context.Background(), key)
		if err != nil || found != nil {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
		if f.server.Count(youtubetest.OpPlaylist) != 0 {
			t.Fatal("no scan is needed")
		}
	})

	t.Run("the record is sealed on disk", func(t *testing.T) {
		f := newPersistent(t)
		if _, err := f.open().Upload(context.Background(), upload(content(chunkSize))); err != nil {
			t.Fatal(err)
		}
		var files []string
		dir := f.params["stateDir"].(string)
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				files = append(files, path)
			}
			return err
		})
		if err != nil || len(files) != 1 {
			t.Fatalf("files %v, %v", files, err)
		}
		if !strings.Contains(files[0], filepath.Join("acme", "youtube-main")) {
			t.Fatalf("records must be per tenant and destination: %s", files[0])
		}
		info, _ := os.Stat(files[0])
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", info.Mode().Perm())
		}
		raw, _ := os.ReadFile(files[0])
		for _, plain := range []string{"upload_id", "session", "uri", "http", key} {
			if bytes.Contains(raw, []byte(plain)) {
				t.Fatalf("record holds %q in the clear", plain)
			}
		}
	})

	t.Run("a rotated credential cannot read old records and falls back to a scan", func(t *testing.T) {
		f := newPersistent(t)
		first, err := f.open().Upload(context.Background(), upload(content(chunkSize)))
		if err != nil {
			t.Fatal(err)
		}
		other, _ := json.Marshal(map[string]any{"access_token": youtubetest.AccessToken})
		f.secrets.values["youtube-oauth"] = other
		found, err := f.open().Reconcile(context.Background(), key)
		if err != nil || found == nil || found.RemoteID != first.RemoteID {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
		if f.server.Count(youtubetest.OpPlaylist) == 0 {
			t.Fatal("must have scanned")
		}
	})
}

func TestRestartWithoutPersistedSessions(t *testing.T) {
	t.Run("an abandoned session makes no video and the retry starts over", func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		data := content(3*chunkSize + 1)
		req := upload(data)
		req.Progress = func(int64) { cancel() }
		if _, err := f.open().Upload(ctx, req); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		conn := f.open()
		found, err := conn.Reconcile(context.Background(), key)
		if err != nil || found != nil {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
		remote, err := conn.Upload(context.Background(), upload(data))
		if err != nil {
			t.Fatal(err)
		}
		if f.server.Count(youtubetest.OpInitiate) != 2 || len(f.server.Videos()) != 1 {
			t.Fatalf("initiations %d, videos %d", f.server.Count(youtubetest.OpInitiate), len(f.server.Videos()))
		}
		if v, _ := f.server.Video(remote.RemoteID); v.SHA256 != shaOf(data) {
			t.Fatal("bytes differ")
		}
	})

	t.Run("a finished upload is found by its tag", func(t *testing.T) {
		f := newFixture(t)
		first, err := f.open().Upload(context.Background(), upload(content(chunkSize)))
		if err != nil {
			t.Fatal(err)
		}
		f.server.AddVideo(youtubetest.Video{ID: "someone-elses", UploadStatus: "processed", Privacy: "public", Tags: []string{"vidyom-other"}})
		found, err := f.open().Reconcile(context.Background(), key)
		if err != nil || found == nil || found.RemoteID != first.RemoteID {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
	})
}

func TestReconcileScanLimits(t *testing.T) {
	addVideos := func(s *youtubetest.Server, n int) {
		for i := range n {
			s.AddVideo(youtubetest.Video{ID: fmt.Sprintf("other%d", i), UploadStatus: "processed", Privacy: "public"})
		}
	}

	t.Run("pages through the playlist", func(t *testing.T) {
		f := newFixture(t)
		first, err := f.open().Upload(context.Background(), upload(content(chunkSize)))
		if err != nil {
			t.Fatal(err)
		}
		addVideos(f.server, 120) // the target is now on the third page
		found, err := f.open().Reconcile(context.Background(), key)
		if err != nil || found == nil || found.RemoteID != first.RemoteID {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
		if f.server.Count(youtubetest.OpPlaylist) != 3 {
			t.Fatalf("pages %d", f.server.Count(youtubetest.OpPlaylist))
		}
	})

	t.Run("nothing found with the whole channel read", func(t *testing.T) {
		f := newFixture(t)
		addVideos(f.server, 60)
		found, err := f.open().Reconcile(context.Background(), key)
		if err != nil || found != nil {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
	})

	t.Run("nothing found with pages left is not an answer", func(t *testing.T) {
		f := newFixture(t)
		f.params["reconcileScanLimit"] = 50
		addVideos(f.server, 60)
		found, err := f.open().Reconcile(context.Background(), key)
		if found != nil || !errs.IsCode(err, errs.CodeUnavailable) {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
	})

	t.Run("an API error is not an answer", func(t *testing.T) {
		f := newFixture(t)
		f.server.FailNext(youtubetest.OpPlaylist, youtubetest.Fault{Status: 500})
		found, err := f.open().Reconcile(context.Background(), key)
		if found != nil || !errs.IsCode(err, errs.CodeUnavailable) {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
	})

	t.Run("a session that cannot answer is not an answer", func(t *testing.T) {
		f := newFixture(t)
		conn := f.open()
		f.server.FailNext(youtubetest.OpChunk, repeat(youtubetest.Fault{Status: 503}, 20)...)
		f.server.FailNext(youtubetest.OpStatus, repeat(youtubetest.Fault{Status: 503}, 20)...)
		if _, err := conn.Upload(context.Background(), upload(content(3*chunkSize+1))); err == nil {
			t.Fatal("must fail")
		}
		found, err := conn.Reconcile(context.Background(), key)
		if found != nil || !errs.IsCode(err, errs.CodeUnavailable) {
			t.Fatalf("reconcile: %+v, %v", found, err)
		}
	})
}

func TestStatusMapping(t *testing.T) {
	cases := map[string]struct {
		change func(*youtubetest.Video)
		want   connector.RemoteVideo
	}{
		"uploaded": {
			func(v *youtubetest.Video) { v.UploadStatus = "uploaded" },
			connector.RemoteVideo{Status: connector.RemoteProcessing, Visibility: domain.VisibilityPrivate},
		},
		"processed": {
			func(v *youtubetest.Video) { v.UploadStatus = "processed" },
			connector.RemoteVideo{Status: connector.RemoteReady, Visibility: domain.VisibilityPrivate},
		},
		"scheduled": {
			func(v *youtubetest.Video) { v.UploadStatus = "processed"; v.PublishAt = "2030-01-01T00:00:00Z" },
			connector.RemoteVideo{Status: connector.RemoteScheduled, Visibility: domain.VisibilityPrivate},
		},
		"rejected": {
			func(v *youtubetest.Video) { v.UploadStatus = "rejected"; v.RejectionReason = "copyright" },
			connector.RemoteVideo{Status: connector.RemoteRejected, Visibility: domain.VisibilityPrivate, Reason: "rejected by YouTube: copyright"},
		},
		"failed": {
			func(v *youtubetest.Video) { v.UploadStatus = "failed"; v.FailureReason = "codec" },
			connector.RemoteVideo{Status: connector.RemoteFailed, Visibility: domain.VisibilityPrivate, Reason: "failed at YouTube: codec"},
		},
		"deleted": {
			func(v *youtubetest.Video) { v.UploadStatus = "deleted" },
			connector.RemoteVideo{Status: connector.RemoteRemoved, Visibility: domain.VisibilityPrivate, Reason: "deleted at YouTube"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.server.ProcessingPolls = 1000 // only the test decides
			conn := f.open()
			uploaded, err := conn.Upload(context.Background(), upload(content(chunkSize)))
			if err != nil {
				t.Fatal(err)
			}
			f.server.SetVideo(uploaded.RemoteID, tc.change)
			got, err := conn.GetStatus(context.Background(), uploaded.RemoteID)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			want.RemoteID = uploaded.RemoteID
			if want.Status == connector.RemoteReady || want.Status == connector.RemoteScheduled {
				want.URL = "https://www.youtube.com/watch?v=" + uploaded.RemoteID
			}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

// R43, D45: the URL appears only once the API confirmed the video processed.
func TestURLOnlyWhenConfirmed(t *testing.T) {
	f := newFixture(t)
	f.server.ProcessingPolls = 2
	conn := f.open()
	ctx := context.Background()
	v, err := conn.Upload(ctx, upload(content(chunkSize)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.GetURL(ctx, v.RemoteID); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
	if got, _ := conn.GetStatus(ctx, v.RemoteID); got.URL != "" || got.Status != connector.RemoteProcessing {
		t.Fatalf("got %+v", got)
	}
	got, err := conn.GetStatus(ctx, v.RemoteID)
	if err != nil || got.Status != connector.RemoteReady || got.URL == "" {
		t.Fatalf("%+v, %v", got, err)
	}
	if u, err := conn.GetURL(ctx, v.RemoteID); err != nil || u != "https://www.youtube.com/watch?v="+v.RemoteID {
		t.Fatalf("%q, %v", u, err)
	}
	if _, err := conn.GetStatus(ctx, "no-such-video"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestSetVisibilityKeepsOtherStatusFields(t *testing.T) {
	f := newFixture(t)
	conn := f.open()
	ctx := context.Background()
	v, err := conn.Upload(ctx, upload(content(chunkSize)))
	if err != nil {
		t.Fatal(err)
	}
	f.server.SetVideo(v.RemoteID, func(video *youtubetest.Video) {
		video.Embeddable = false
		video.License = "creativeCommon"
		video.PublicStatsViewable = false
		video.PublishAt = "2030-01-01T00:00:00Z"
	})
	got, err := conn.SetVisibility(ctx, v.RemoteID, domain.VisibilityPublic)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visibility != domain.VisibilityPublic {
		t.Fatalf("got %+v", got)
	}
	video, _ := f.server.Video(v.RemoteID)
	if video.Privacy != "public" || video.Embeddable || video.License != "creativeCommon" || video.PublicStatsViewable {
		t.Fatalf("a field was reset: %+v", video)
	}
	if video.PublishAt != "" {
		t.Fatal("publishAt is only valid on a private video")
	}
	if _, err := conn.SetVisibility(ctx, "no-such-video", domain.VisibilityPrivate); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateAndScheduling(t *testing.T) {
	f := newFixture(t)
	conn := f.open()
	ctx := context.Background()
	v, err := conn.Upload(ctx, upload(content(chunkSize)))
	if err != nil {
		t.Fatal(err)
	}
	f.server.SetVideo(v.RemoteID, func(video *youtubetest.Video) { video.CategoryID = "27"; video.License = "creativeCommon" })
	if _, err := conn.GetStatus(ctx, v.RemoteID); err != nil { // processed by now
		t.Fatal(err)
	}
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	got, err := conn.Update(ctx, v.RemoteID, connector.VideoMetadata{
		Title: "New <title>", Description: "New description", Tags: []string{"new"}, Language: "pt-BR",
		Visibility: domain.VisibilityPublic, ScheduledAt: &at,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != connector.RemoteScheduled {
		t.Fatalf("got %+v", got)
	}
	video, _ := f.server.Video(v.RemoteID)
	if video.Title != "New title" || video.Description != "New description" || video.DefaultLanguage != "pt-BR" {
		t.Fatalf("snippet %+v", video)
	}
	if video.CategoryID != "27" || video.License != "creativeCommon" {
		t.Fatalf("a field was reset: %+v", video)
	}
	if video.PublishAt != "2030-01-02T03:04:05Z" || video.Privacy != "private" {
		t.Fatalf("a schedule needs a private video: %+v", video)
	}
	if len(video.Tags) != 2 || video.Tags[0] != "vidyom-"+key[:32] || video.Tags[1] != "new" {
		t.Fatalf("the reconcile marker must survive: %v", video.Tags)
	}
	if _, err := conn.Update(ctx, "no-such-video", connector.VideoMetadata{Title: "x"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteIsRepeatable(t *testing.T) {
	f := newFixture(t)
	conn := f.open()
	ctx := context.Background()
	v, err := conn.Upload(ctx, upload(content(chunkSize)))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := conn.Delete(ctx, v.RemoteID); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.server.Videos()) != 0 {
		t.Fatal("still there")
	}
	f.server.FailNext(youtubetest.OpDelete, youtubetest.Fault{Status: 503})
	if err := conn.Delete(ctx, "x"); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestThumbnails(t *testing.T) {
	f := newFixture(t)
	conn := f.open()
	ctx := context.Background()
	v, err := conn.Upload(ctx, upload(content(chunkSize)))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.UploadThumbnail(ctx, v.RemoteID, strings.NewReader("png bytes"), "image/png"); err != nil {
		t.Fatal(err)
	}
	video, _ := f.server.Video(v.RemoteID)
	if string(video.Thumbnail) != "png bytes" || video.ThumbnailType != "image/png" {
		t.Fatalf("thumbnail %q %q", video.Thumbnail, video.ThumbnailType)
	}
	if err := conn.UploadThumbnail(ctx, v.RemoteID, strings.NewReader(strings.Repeat("x", 2<<20+1)), "image/png"); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	f.server.FailNext(youtubetest.OpThumb, youtubetest.Fault{Status: 403, Reason: "forbidden"})
	if err := conn.UploadThumbnail(ctx, v.RemoteID, strings.NewReader("x"), ""); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestCaptionsReplaceTheirTrack(t *testing.T) {
	f := newFixture(t)
	conn := f.open()
	ctx := context.Background()
	v, err := conn.Upload(ctx, upload(content(chunkSize)))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"WEBVTT\n\nfirst", "WEBVTT\n\nsecond"} {
		if err := conn.UploadCaptions(ctx, v.RemoteID, "en", "vtt", strings.NewReader(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.UploadCaptions(ctx, v.RemoteID, "pt-BR", "vtt", strings.NewReader("WEBVTT\n\noutra")); err != nil {
		t.Fatal(err)
	}
	tracks := f.server.Tracks()
	if len(tracks) != 2 || tracks[0].Text != "WEBVTT\n\nsecond" || tracks[0].Language != "en" || tracks[1].Language != "pt-BR" {
		t.Fatalf("tracks %+v", tracks)
	}
}

func TestPublicLinksAreDerivedWithNoCall(t *testing.T) {
	f := newFixture(t)
	f.secrets.err = errors.New("the vault must not be reached")
	links := f.open().PublicLinks(connector.RemoteVideo{RemoteID: "abc123"})
	if links.EmbedURL != "https://www.youtube.com/embed/abc123" || !links.SupportsTimestamps || !strings.Contains(links.ThumbnailURL, "abc123") {
		t.Fatalf("links %+v", links)
	}
	if got := f.open().PublicLinks(connector.RemoteVideo{}); got != (connector.PublicLinks{}) {
		t.Fatalf("links %+v", got)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		fault youtubetest.Fault
		code  errs.Code
	}{
		{youtubetest.Fault{Status: 400, Reason: "invalidVideoMetadata"}, errs.CodeInvalidArgument},
		{youtubetest.Fault{Status: 403, Reason: "quotaExceeded"}, errs.CodeRateLimited},
		{youtubetest.Fault{Status: 403, Reason: "uploadLimitExceeded"}, errs.CodeRateLimited},
		{youtubetest.Fault{Status: 403, Reason: "forbidden"}, errs.CodeForbidden},
		{youtubetest.Fault{Status: 429, Reason: "tooMany"}, errs.CodeRateLimited},
		{youtubetest.Fault{Status: 409, Reason: "conflict"}, errs.CodeConflict},
		{youtubetest.Fault{Status: 500, Reason: "backendError"}, errs.CodeUnavailable},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d %s", tc.fault.Status, tc.fault.Reason), func(t *testing.T) {
			f := newFixture(t)
			f.server.FailNext(youtubetest.OpInitiate, tc.fault)
			_, err := f.open().Upload(context.Background(), upload(content(chunkSize)))
			if !errs.IsCode(err, tc.code) {
				t.Fatalf("got %v", err)
			}
			if connector.IsOutcomeUnknown(err) {
				t.Fatal("an initiation refused is not unknown")
			}
			if tc.fault.Reason != "" && !strings.Contains(err.Error(), tc.fault.Reason) {
				t.Fatalf("the reason should be kept: %v", err)
			}
			if strings.Contains(err.Error(), "mock says") {
				t.Fatalf("the body's text must stay out: %v", err)
			}
		})
	}
}

func TestChunkRefusedByTheDestinationIsPermanent(t *testing.T) {
	f := newFixture(t)
	f.server.SkipThenFail(youtubetest.OpChunk, 1, youtubetest.Fault{Status: 403, Reason: "quotaExceeded"})
	_, err := f.open().Upload(context.Background(), upload(content(3*chunkSize+1)))
	if !errs.IsCode(err, errs.CodeRateLimited) {
		t.Fatalf("got %v", err)
	}
}

func TestTokens(t *testing.T) {
	t.Run("an expired access token is refreshed", func(t *testing.T) {
		f := newFixture(t)
		raw, _ := json.Marshal(map[string]any{
			"client_id": youtubetest.ClientID, "client_secret": youtubetest.ClientSecret, "refresh_token": youtubetest.RefreshToken,
			"access_token": "old", "expiry": "2001-01-01T00:00:00Z",
		})
		f.secrets.values["youtube-oauth"] = raw
		if _, err := f.open().Upload(context.Background(), upload(content(chunkSize))); err != nil {
			t.Fatal(err)
		}
		if f.server.Count(youtubetest.OpToken) != 1 {
			t.Fatalf("refreshes %d", f.server.Count(youtubetest.OpToken))
		}
	})

	t.Run("no expiry and no access token also refreshes", func(t *testing.T) {
		f := newFixture(t)
		raw, _ := json.Marshal(map[string]any{
			"client_id": youtubetest.ClientID, "client_secret": youtubetest.ClientSecret, "refresh_token": youtubetest.RefreshToken,
		})
		f.secrets.values["youtube-oauth"] = raw
		if _, err := f.open().Upload(context.Background(), upload(content(chunkSize))); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a refused refresh names no secret", func(t *testing.T) {
		f := newFixture(t)
		const secretRefresh = "fictional-revoked-refresh-token"
		raw, _ := json.Marshal(map[string]any{
			"client_id": youtubetest.ClientID, "client_secret": youtubetest.ClientSecret, "refresh_token": secretRefresh,
		})
		f.secrets.values["youtube-oauth"] = raw
		_, err := f.open().Upload(context.Background(), upload(content(chunkSize)))
		if !errs.IsCode(err, errs.CodeUnauthenticated) {
			t.Fatalf("got %v", err)
		}
		if strings.Contains(fmt.Sprintf("%+v", err), secretRefresh) {
			t.Fatalf("the error holds the token: %v", err)
		}
	})

	t.Run("a credential rotated in the vault takes effect after a 401", func(t *testing.T) {
		f := newFixture(t)
		conn := f.open()
		ctx := context.Background()
		v, err := conn.Upload(ctx, upload(content(chunkSize)))
		if err != nil {
			t.Fatal(err)
		}
		f.server.RevokeAccessTokens()
		f.server.AcceptAccessToken("rotated-token")
		raw, _ := json.Marshal(map[string]any{"access_token": "rotated-token"})
		f.secrets.values["youtube-oauth"] = raw
		if _, err := conn.GetStatus(ctx, v.RemoteID); err != nil {
			t.Fatalf("rotation must be picked up: %v", err)
		}
	})

	t.Run("a 401 that persists is unauthenticated", func(t *testing.T) {
		f := newFixture(t)
		conn := f.open()
		f.server.RevokeAccessTokens()
		if _, err := conn.GetStatus(context.Background(), "x"); !errs.IsCode(err, errs.CodeUnauthenticated) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCredentialsFailClosed(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(*fixture){
		"not in the vault": func(f *fixture) { delete(f.secrets.values, "youtube-oauth") },
		"not JSON":         func(f *fixture) { f.secrets.values["youtube-oauth"] = []byte("fictional-secret-value{") },
		"no token":         func(f *fixture) { f.secrets.values["youtube-oauth"] = []byte(`{"client_id":"x"}`) },
		"refresh with no client": func(f *fixture) {
			f.secrets.values["youtube-oauth"] = []byte(`{"refresh_token":"fictional-secret-value"}`)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			setup(f)
			conn := f.open()
			_, err := conn.Upload(ctx, upload(content(chunkSize)))
			if !errs.IsCode(err, errs.CodeFailedPrecondition) {
				t.Fatalf("got %v", err)
			}
			if strings.Contains(err.Error(), "fictional-secret-value") {
				t.Fatalf("the error holds the secret: %v", err)
			}
			if f.server.Count(youtubetest.OpInitiate) != 0 {
				t.Fatal("nothing may be sent")
			}
		})
	}

	t.Run("sealed vault", func(t *testing.T) {
		f := newFixture(t)
		f.secrets.err = errs.New(errs.CodeSealed, "sealed")
		if _, err := f.open().GetStatus(ctx, "x"); !errs.IsCode(err, errs.CodeUnavailable) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("no credentialsRef", func(t *testing.T) {
		f := newFixture(t)
		reg := connector.NewRegistry()
		youtube.Register(reg)
		factory, _ := reg.Publishing(youtube.Driver)
		conn, err := factory(ctx, connector.PublishingParams{
			Tenant: "acme", Destination: "d", Secrets: f.secrets,
			Config: config.Destination{Parameters: f.server.Parameters(nil)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.GetStatus(ctx, "x"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
			t.Fatalf("got %v", err)
		}
	})
}

// A session URI is a bearer capability: it must never reach an error or a log (§15.6).
func TestErrorsHoldNoSessionURIOrToken(t *testing.T) {
	f := newFixture(t)
	conn := f.open()
	f.server.FailNext(youtubetest.OpChunk, repeat(youtubetest.Fault{Drop: true}, 20)...)
	f.server.FailNext(youtubetest.OpStatus, repeat(youtubetest.Fault{Drop: true}, 20)...)
	_, err := conn.Upload(context.Background(), upload(content(3*chunkSize+1)))
	if err == nil {
		t.Fatal("must fail")
	}
	text := fmt.Sprintf("%v %+v", err, err)
	for _, secret := range []string{"upload_id", "session0", youtubetest.AccessToken, youtubetest.RefreshToken, youtubetest.ClientSecret} {
		if strings.Contains(text, secret) {
			t.Fatalf("the error holds %q: %s", secret, text)
		}
	}

	// The same when the whole server goes away, which is what prints a URL.
	f.server.Close()
	_, err = conn.Reconcile(context.Background(), key)
	if err == nil {
		t.Fatal("must fail")
	}
	text = fmt.Sprintf("%v %+v", err, err)
	if strings.Contains(text, "upload_id") {
		t.Fatalf("the error holds the session URI: %s", text)
	}
	if !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	f := newFixture(t)
	f.server.Redirect = elsewhere.URL
	_, err := f.open().GetStatus(context.Background(), "x")
	if err == nil {
		t.Fatal("a redirect must not read as an answer")
	}
	if leaked.Load() != 0 {
		t.Fatal("the bearer token left for another host")
	}
}

func TestUploadRefusals(t *testing.T) {
	f := newFixture(t)
	conn := f.open()
	ctx := context.Background()
	for name, change := range map[string]func(*connector.UploadRequest){
		"no key":   func(r *connector.UploadRequest) { r.IdempotencyKey = "" },
		"no file":  func(r *connector.UploadRequest) { r.File = nil },
		"no size":  func(r *connector.UploadRequest) { r.SizeBytes = 0 },
		"no title": func(r *connector.UploadRequest) { r.Metadata.Title = "" },
	} {
		req := upload("x")
		change(&req)
		if _, err := conn.Upload(ctx, req); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if f.server.Count(youtubetest.OpInitiate) != 0 {
		t.Fatal("nothing may be sent")
	}
}

func TestMetadataIsCleanedToYouTubesLimits(t *testing.T) {
	f := newFixture(t)
	req := upload(content(chunkSize))
	req.Metadata.Title = strings.Repeat("ã<", 150)
	req.Metadata.Description = strings.Repeat("é", 4000)
	for i := range 200 {
		req.Metadata.Tags = append(req.Metadata.Tags, fmt.Sprintf("tag-number-%03d", i))
	}
	v, err := f.open().Upload(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	video, _ := f.server.Video(v.RemoteID)
	if got := len([]rune(video.Title)); got != 100 || strings.Contains(video.Title, "<") {
		t.Fatalf("title %d runes: %q", got, video.Title)
	}
	if len(video.Description) > 5000 || !strings.HasSuffix(video.Description, "é") {
		t.Fatalf("description %d bytes", len(video.Description))
	}
	total := 0
	for _, tag := range video.Tags {
		total += len(tag) + 3
	}
	if total > 500 || video.Tags[0] != "vidyom-"+key[:32] {
		t.Fatalf("tags total %d, first %q", total, video.Tags[0])
	}
}

func TestSelfDeclaredMadeForKidsIsOnlySentWhenConfigured(t *testing.T) {
	f := newFixture(t)
	v, err := f.open().Upload(context.Background(), upload(content(chunkSize)))
	if err != nil {
		t.Fatal(err)
	}
	if video, _ := f.server.Video(v.RemoteID); video.MadeForKids != nil {
		t.Fatal("a legal declaration must not be invented")
	}

	f = newFixture(t)
	f.params["selfDeclaredMadeForKids"] = false
	v, err = f.open().Upload(context.Background(), upload(content(chunkSize)))
	if err != nil {
		t.Fatal(err)
	}
	if video, _ := f.server.Video(v.RemoteID); video.MadeForKids == nil || *video.MadeForKids {
		t.Fatalf("declaration %v", video.MadeForKids)
	}
}

func TestConfiguration(t *testing.T) {
	ctx := context.Background()
	reg := connector.NewRegistry()
	youtube.Register(reg)
	factory, err := reg.Publishing(youtube.Driver)
	if err != nil {
		t.Fatal(err)
	}
	listed := reg.List()
	if len(listed) != 1 || !listed[0].Available || !strings.Contains(listed[0].Reason, "mock") {
		t.Fatalf("the documented gap must show: %+v", listed)
	}

	for name, params := range map[string]string{
		"plaintext API":    `{"apiBaseUrl":"http://media.example.internal"}`,
		"plaintext upload": `{"uploadBaseUrl":"http://media.example.internal"}`,
		"plaintext oauth":  `{"oauthUrl":"http://media.example.internal/token"}`,
		"not a URL":        `{"apiBaseUrl":"::"}`,
		"bad JSON":         `{`,
	} {
		_, err := factory(ctx, connector.PublishingParams{Tenant: "acme", Destination: "d", Config: config.Destination{Parameters: json.RawMessage(params)}})
		if !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	_, err = factory(ctx, connector.PublishingParams{Tenant: "../up", Destination: "d"})
	if !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("bad tenant: got %v", err)
	}
	_, err = factory(ctx, connector.PublishingParams{Tenant: "acme", Destination: "../d"})
	if !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("bad destination: got %v", err)
	}

	// With no parameters and no secret it opens and touches nothing: real Google URLs
	// are the default, and nothing may reach them until a credential is used.
	conn, err := factory(ctx, connector.PublishingParams{Tenant: "acme", Destination: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if !conn.Capabilities().ResumableUpload || !conn.Capabilities().Reconcile || len(conn.Capabilities().Visibilities) != 3 {
		t.Fatalf("capabilities %+v", conn.Capabilities())
	}
	if _, err := conn.GetStatus(ctx, "x"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
}
