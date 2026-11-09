package publishingnodes

import (
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/connectors/publishing/youtube"
	"github.com/rafaelp/vidyom/connectors/publishing/youtube/youtubetest"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/persistence/sqlitedriver"
)

const youtubeDest = "youtube-main"

// withYouTube adds a destination pointing at the local mock (I12).
func withYouTube(t *testing.T, h *harness, extra map[string]any) *youtubetest.Server {
	t.Helper()
	server := youtubetest.New()
	t.Cleanup(server.Close)
	h.secrets["youtube-oauth"] = youtubetest.Credentials()
	h.cfg.Publishing.Destinations[youtubeDest] = config.Destination{
		Driver:         youtube.Driver,
		CredentialsRef: "youtube-oauth",
		Visibility:     domain.VisibilityPrivate,
		Parameters:     server.Parameters(extra),
	}
	return server
}

func prepareYouTube(h *harness) string {
	h.t.Helper()
	return run[prepareOutput](h, "publish.prepare", map[string]any{
		"assetId": "CLIP-001", "destination": youtubeDest, "visibility": "public", "approvalId": "APR-001",
	}).PublicationID
}

// The whole publication through the real connector against the mock API.
func TestYouTubeLifecycle(t *testing.T) {
	h := newHarness(t)
	server := withYouTube(t, h, nil)
	server.ProcessingPolls = 2

	pub := prepareYouTube(h)
	up := run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	videos := server.Videos()
	if len(videos) != 1 || videos[0].ID != up.RemoteID {
		t.Fatalf("videos %+v", videos)
	}
	video := videos[0]
	if video.Privacy != "private" || video.SHA256 != sha(masterBytes) || video.Title != "Conscious breathing" {
		t.Fatalf("the upload stays private until finalize: %+v", video)
	}
	if record := h.publication(pub); record.Status != domain.PubProcessing || record.URL != "" {
		t.Fatalf("no URL while processing (R43): %+v", record)
	}

	await := run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": pub})
	if !await.Ready || await.Status != "processing" {
		t.Fatalf("await %+v", await)
	}

	final := run[finalizeOutput](h, "publish.finalize", map[string]any{
		"publicationId": pub, "visibility": "public", "captions": map[string]any{"language": "en"},
	})
	want := "https://www.youtube.com/watch?v=" + up.RemoteID
	if final.Status != "published" || final.URL != want || final.Captions != stepSent {
		t.Fatalf("finalize %+v", final)
	}
	if v, _ := server.Video(up.RemoteID); v.Privacy != "public" {
		t.Fatalf("destination shows %s", v.Privacy)
	}
	tracks := server.Tracks()
	if len(tracks) != 1 || strings.Contains(tracks[0].Text, "Maria") || !strings.Contains(tracks[0].Text, publicText) {
		t.Fatalf("captions must exclude sensitive segments (D30): %+v", tracks)
	}

	// public.db carries the driver name and links the connector derived.
	run[syncOutput](h, "catalog.public.sync", map[string]any{})
	db, err := sqlitedriver.Open(h.publicDB(), sqlitedriver.Options{ReadOnly: true})
	h.must(err)
	defer db.Close()
	var driver, url, embed, thumb string
	var ts int
	row := db.QueryRow(`SELECT d.destination, d.url, d.embed_url, d.supports_timestamps, v.thumbnail_url FROM destinations d JOIN videos v USING (public_id)`)
	if err := row.Scan(&driver, &url, &embed, &ts, &thumb); err != nil {
		t.Fatal(err)
	}
	if driver != "youtube" || url != want || embed != "https://www.youtube.com/embed/"+up.RemoteID || ts != 1 || !strings.Contains(thumb, up.RemoteID) {
		t.Fatalf("row %q %q %q %d %q", driver, url, embed, ts, thumb)
	}

	// Withdrawal is repeatable, and "delete" really removes it.
	out := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true, "mode": "delete"})
	if len(out.Withdrawn) != 1 || len(server.Videos()) != 0 {
		t.Fatalf("withdraw %+v, videos %d", out, len(server.Videos()))
	}
	again := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true, "mode": "delete"})
	if len(again.Already) != 1 || hasEvent(h.store.Events(), event.PublicationWithdrawn) != 1 {
		t.Fatalf("repeat %+v", again)
	}
}

// §15.5: a daemon killed around the upload produces no second video.
func TestYouTubeRestartNeverUploadsTwice(t *testing.T) {
	t.Run("answer lost after the video was made", func(t *testing.T) {
		h := newHarness(t)
		server := withYouTube(t, h, map[string]any{"stateDir": t.TempDir()})
		pub := prepareYouTube(h)
		server.FailNext(youtubetest.OpFinal, youtubetest.Fault{DropAfter: true})
		server.FailNext(youtubetest.OpStatus,
			youtubetest.Fault{Status: 503}, youtubetest.Fault{Status: 503}, youtubetest.Fault{Status: 503}, youtubetest.Fault{Status: 503})

		_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
		if !node.IsUnknownOutcome(err) {
			t.Fatalf("got %v", err)
		}
		if record := h.publication(pub); record.Status != domain.PubUploading || record.RemoteID != "" {
			t.Fatalf("record %+v", record)
		}

		// The engine reconciles first; the connector is opened anew as after a restart.
		h.resolver = freshResolver(h)
		ctx, e, in := h.input("publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
		outcome, res, err := h.node("publish.upload").(node.Reconciler).Reconcile(ctx, e, in)
		if err != nil || outcome != node.ReconcileDone {
			t.Fatalf("%s, %v", outcome, err)
		}
		h.checkOutput("publish.upload", res)
		if record := h.publication(pub); record.RemoteID != server.Videos()[0].ID {
			t.Fatalf("record %+v", record)
		}
		if len(server.Videos()) != 1 || server.Count(youtubetest.OpInitiate) != 1 {
			t.Fatalf("videos %d, sessions %d", len(server.Videos()), server.Count(youtubetest.OpInitiate))
		}
	})

	t.Run("killed mid-upload resumes the same session", func(t *testing.T) {
		h := newHarness(t)
		server := withYouTube(t, h, map[string]any{"stateDir": t.TempDir()})
		big := strings.Repeat("x", 3*256*1024+5)
		h.storages[domain.RoleMaster].objects[masterKey] = big
		h.must(h.store.Versions().PutFile(h.ctx(), "CLIP-001", "v1", domain.VersionFile{
			Purpose: domain.FileWorkingMaster,
			File:    domain.MediaFile{Storage: domain.RoleMaster, Key: masterKey, SHA256: sha(big), SizeBytes: int64(len(big))},
		}))
		pub := prepareYouTube(h)
		server.SkipThenFail(youtubetest.OpChunk, 1, youtubetest.Fault{Status: 503})
		server.FailNext(youtubetest.OpStatus,
			youtubetest.Fault{Status: 503}, youtubetest.Fault{Status: 503}, youtubetest.Fault{Status: 503}, youtubetest.Fault{Status: 503})
		_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
		if !node.IsTransient(err) {
			t.Fatalf("got %v", err)
		}
		if sessions := server.OpenSessions(); len(sessions) != 1 || sessions[0] != 256*1024 {
			t.Fatalf("sessions %v", sessions)
		}

		h.resolver = freshResolver(h)
		out := run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
		if out.RemoteID == "" || server.Count(youtubetest.OpInitiate) != 1 || len(server.Videos()) != 1 {
			t.Fatalf("output %+v, sessions %d, videos %d", out, server.Count(youtubetest.OpInitiate), len(server.Videos()))
		}
		if v, _ := server.Video(out.RemoteID); v.SHA256 != sha(big) {
			t.Fatal("bytes differ")
		}
	})
}

func TestYouTubeRejectionAndQuota(t *testing.T) {
	t.Run("rejected by the platform", func(t *testing.T) {
		h := newHarness(t)
		server := withYouTube(t, h, nil)
		pub := prepareYouTube(h)
		up := run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		server.SetVideo(up.RemoteID, func(v *youtubetest.Video) { v.UploadStatus = "rejected"; v.RejectionReason = "copyright" })
		out := run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": pub})
		if out.Status != string(domain.PubRejectedByDestination) {
			t.Fatalf("await %+v", out)
		}
		if reason := h.publication(pub).StatusReason; !strings.Contains(reason, "copyright") {
			t.Fatalf("reason %q", reason)
		}
		if _, err := h.runErr("publish.finalize", map[string]any{"publicationId": pub}); !permanent(err) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("quota is transient and never marks the record failed", func(t *testing.T) {
		h := newHarness(t)
		server := withYouTube(t, h, nil)
		pub := prepareYouTube(h)
		server.FailNext(youtubetest.OpInitiate, youtubetest.Fault{Status: 403, Reason: "quotaExceeded"})
		_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
		if !node.IsTransient(err) || !errs.IsCode(err, errs.CodeRateLimited) {
			t.Fatalf("got %v", err)
		}
		if got := h.publication(pub).Status; got != domain.PubUploading {
			t.Fatalf("status %s", got)
		}
	})

	t.Run("revoked credentials fail with no secret in the record", func(t *testing.T) {
		h := newHarness(t)
		server := withYouTube(t, h, nil)
		pub := prepareYouTube(h)
		server.RevokeAccessTokens()
		_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
		if !permanent(err) || !errs.IsCode(err, errs.CodeUnauthenticated) {
			t.Fatalf("got %v", err)
		}
		record := h.publication(pub)
		if record.Status != domain.PubFailed || strings.Contains(record.StatusReason, youtubetest.AccessToken) {
			t.Fatalf("record %+v", record)
		}
	})
}
