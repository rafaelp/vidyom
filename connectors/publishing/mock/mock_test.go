package mock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

const key = "9f2b7c1e0d4a4f3e8a6b5c7d9e1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b"

func openMock(t *testing.T, stateDir string, extra map[string]any) *Destination {
	t.Helper()
	reg := connector.NewRegistry()
	Register(reg)
	f, err := reg.Publishing(Driver)
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"stateDir": stateDir}
	for k, v := range extra {
		params[k] = v
	}
	raw, _ := json.Marshal(params)
	p, err := f(context.Background(), connector.PublishingParams{
		Tenant: "acme", Destination: "mock-main",
		Config: config.Destination{Driver: Driver, Parameters: raw},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.(*Destination)
}

func upload(content string) connector.UploadRequest {
	sum := sha256.Sum256([]byte(content))
	return connector.UploadRequest{
		IdempotencyKey: key,
		File:           strings.NewReader(content),
		SizeBytes:      int64(len(content)),
		SHA256:         hex.EncodeToString(sum[:]),
		ContentType:    "video/mp4",
		Metadata:       connector.VideoMetadata{Title: "A fictional clip", Visibility: domain.VisibilityPrivate},
	}
}

func state(t *testing.T, stateDir string) State {
	t.Helper()
	s, err := ReadState(stateDir, "acme", "mock-main")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUploadIsIdempotentAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	first, err := openMock(t, dir, nil).Upload(ctx, upload("video bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if first.RemoteID != RemoteID(key) || !strings.HasPrefix(first.URL, BaseURL+"/watch/") {
		t.Fatalf("got %+v", first)
	}
	if first.Status != connector.RemoteReady || first.Visibility != domain.VisibilityPrivate {
		t.Fatalf("got %+v", first)
	}

	// A new instance stands for a restarted daemon.
	again, err := openMock(t, dir, nil).Upload(ctx, upload("video bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("repeat answered %+v, want %+v", again, first)
	}
	s := state(t, dir)
	if s.Uploads != 1 || s.Repeats != 1 || len(s.Videos) != 1 {
		t.Fatalf("state %+v", s)
	}
}

func TestNoURLWhileProcessing(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	d := openMock(t, dir, map[string]any{"processingSeconds": 60})
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }

	v, err := d.Upload(ctx, upload("x"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != connector.RemoteProcessing || v.URL != "" {
		t.Fatalf("got %+v: no URL while processing (R43)", v)
	}
	if _, err := d.GetURL(ctx, v.RemoteID); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("GetURL: %v", err)
	}

	clock = clock.Add(61 * time.Second)
	v, err = d.GetStatus(ctx, v.RemoteID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != connector.RemoteReady || v.URL == "" {
		t.Fatalf("got %+v", v)
	}
	if got, err := d.GetURL(ctx, v.RemoteID); err != nil || got != v.URL {
		t.Fatalf("GetURL: %q, %v", got, err)
	}
}

func TestFailUploadResumesAndFiresOnce(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	opts := map[string]any{"inject": map[string]int{InjectFailUpload: 1}}
	content := strings.Repeat("v", 3*chunkBytes)

	if _, err := openMock(t, dir, opts).Upload(ctx, upload(content)); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("got %v", err)
	}
	if connector.IsOutcomeUnknown(nil) {
		t.Fatal("nil is not unknown")
	}
	if s := state(t, dir); s.Uploads != 0 || s.Partial[key] == 0 {
		t.Fatalf("state %+v", s)
	}

	var progress []int64
	req := upload(content)
	req.Progress = func(n int64) { progress = append(progress, n) }
	if _, err := openMock(t, dir, opts).Upload(ctx, req); err != nil {
		t.Fatalf("the injection must fire once across restarts: %v", err)
	}
	s := state(t, dir)
	if s.Uploads != 1 || s.Resumes != 1 || len(s.Partial) != 0 {
		t.Fatalf("state %+v", s)
	}
	if len(progress) == 0 || progress[len(progress)-1] != int64(len(content)) {
		t.Fatalf("progress %v", progress)
	}
	if progress[0] <= int64(len(content))/2 {
		t.Fatalf("progress restarted from zero: %v", progress)
	}
}

func TestOutcomeUnknownThenReconcileFindsIt(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	d := openMock(t, dir, map[string]any{"inject": map[string]int{InjectOutcomeUnknown: 1}})

	if got, err := d.Reconcile(ctx, key); got != nil || err != nil {
		t.Fatalf("before upload: %+v, %v", got, err)
	}
	_, err := d.Upload(ctx, upload("x"))
	if !connector.IsOutcomeUnknown(err) {
		t.Fatalf("got %v", err)
	}
	got, err := d.Reconcile(ctx, key)
	if err != nil || got == nil || got.RemoteID != RemoteID(key) {
		t.Fatalf("reconcile: %+v, %v", got, err)
	}
	if s := state(t, dir); s.Uploads != 1 {
		t.Fatalf("state %+v", s)
	}
}

func TestOutcomeUnknownLostThenReconcileFindsNothing(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	d := openMock(t, dir, map[string]any{"inject": map[string]int{InjectOutcomeUnknownLost: 1}})

	_, err := d.Upload(ctx, upload("x"))
	if !connector.IsOutcomeUnknown(err) {
		t.Fatalf("got %v", err)
	}
	if got, err := d.Reconcile(ctx, key); got != nil || err != nil {
		t.Fatalf("reconcile: %+v, %v", got, err)
	}
	if _, err := d.Upload(ctx, upload("x")); err != nil {
		t.Fatal(err)
	}
	if s := state(t, dir); s.Uploads != 1 {
		t.Fatalf("state %+v", s)
	}
}

func TestReconcileCanBeUnsupported(t *testing.T) {
	d := openMock(t, t.TempDir(), map[string]any{"reconcile": false})
	if d.Capabilities().Reconcile || d.Info().Capabilities["reconcile"] {
		t.Fatal("must declare no reconcile")
	}
	if _, err := d.Reconcile(context.Background(), key); !errs.IsCode(err, errs.CodeCapabilityUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestRejectIsDistinctFromFailure(t *testing.T) {
	d := openMock(t, t.TempDir(), map[string]any{"inject": map[string]int{InjectReject: 1}})
	v, err := d.Upload(context.Background(), upload("x"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != connector.RemoteRejected || v.Reason == "" || v.URL != "" {
		t.Fatalf("got %+v", v)
	}
}

func TestChecksumAndSizeAreVerified(t *testing.T) {
	dir := t.TempDir()
	d := openMock(t, dir, nil)
	req := upload("x")
	req.SHA256 = strings.Repeat("0", 64)
	if _, err := d.Upload(context.Background(), req); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	req = upload("x")
	req.SizeBytes = 2
	if _, err := d.Upload(context.Background(), req); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	if s := state(t, dir); s.Uploads != 0 {
		t.Fatalf("state %+v", s)
	}
}

func TestCancelledUploadLeavesNoVideo(t *testing.T) {
	dir := t.TempDir()
	d := openMock(t, dir, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Upload(ctx, upload("x")); err == nil {
		t.Fatal("must fail")
	}
	if s := state(t, dir); s.Uploads != 0 {
		t.Fatalf("state %+v", s)
	}
}

func TestVisibilityDeleteAndRepeatedRemoval(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	d := openMock(t, dir, map[string]any{"inject": map[string]int{InjectFailRemove: 1}})
	v, err := d.Upload(ctx, upload("x"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.SetVisibility(ctx, v.RemoteID, domain.VisibilityPublic)
	if err != nil || got.Visibility != domain.VisibilityPublic {
		t.Fatalf("%+v, %v", got, err)
	}
	if _, err := d.SetVisibility(ctx, v.RemoteID, domain.VisibilityPrivate); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("injected failure: %v", err)
	}
	got, err = d.Get(ctx, v.RemoteID)
	if err != nil || got.Visibility != domain.VisibilityPublic {
		t.Fatalf("a refused change must change nothing: %+v, %v", got, err)
	}
	got, err = d.SetVisibility(ctx, v.RemoteID, domain.VisibilityPrivate)
	if err != nil || got.Visibility != domain.VisibilityPrivate {
		t.Fatalf("%+v, %v", got, err)
	}
	for range 2 {
		if err := d.Delete(ctx, v.RemoteID); err != nil {
			t.Fatalf("delete must be repeatable: %v", err)
		}
	}
	got, err = d.GetStatus(ctx, v.RemoteID)
	if err != nil || got.Status != connector.RemoteRemoved || got.URL != "" {
		t.Fatalf("%+v, %v", got, err)
	}
	if _, err := d.SetVisibility(ctx, v.RemoteID, domain.VisibilityPublic); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := d.Delete(ctx, "unknown"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestScheduling(t *testing.T) {
	ctx := context.Background()
	d := openMock(t, t.TempDir(), nil)
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	v, err := d.Upload(ctx, upload("x"))
	if err != nil {
		t.Fatal(err)
	}
	at := clock.Add(time.Hour)
	got, err := d.Update(ctx, v.RemoteID, connector.VideoMetadata{Title: "t", Visibility: domain.VisibilityPrivate, ScheduledAt: &at})
	if err != nil || got.Status != connector.RemoteScheduled {
		t.Fatalf("%+v, %v", got, err)
	}
	clock = at.Add(time.Second)
	got, err = d.GetStatus(ctx, v.RemoteID)
	if err != nil || got.Status != connector.RemoteReady || got.Visibility != domain.VisibilityPublic {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestThumbnailsAndCaptionsAreRecorded(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	d := openMock(t, dir, nil)
	v, err := d.Upload(ctx, upload("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UploadThumbnail(ctx, v.RemoteID, bytes.NewReader([]byte("jpeg")), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"old text", "new text"} {
		if err := d.UploadCaptions(ctx, v.RemoteID, "en", "vtt", strings.NewReader(text)); err != nil {
			t.Fatal(err)
		}
	}
	video := state(t, dir).Videos[v.RemoteID]
	if video.ThumbnailSHA256 == "" || video.Captions["en"].Text != "new text" {
		t.Fatalf("video %+v", video)
	}
}

func TestPublicLinks(t *testing.T) {
	d := openMock(t, t.TempDir(), nil)
	links := d.PublicLinks(connector.RemoteVideo{RemoteID: "mock-1"})
	if links.EmbedURL == "" || !links.SupportsTimestamps || links.ThumbnailURL != "" {
		t.Fatalf("got %+v", links)
	}
	if got := d.PublicLinks(connector.RemoteVideo{}); got != (connector.PublicLinks{}) {
		t.Fatalf("got %+v", got)
	}
}

func TestRefusesBadConfiguration(t *testing.T) {
	ctx := context.Background()
	for name, p := range map[string]connector.PublishingParams{
		"no state dir": {Tenant: "acme", Destination: "d", Config: config.Destination{Parameters: json.RawMessage(`{}`)}},
		"no params":    {Tenant: "acme", Destination: "d"},
		"bad json":     {Tenant: "acme", Destination: "d", Config: config.Destination{Parameters: json.RawMessage(`{`)}},
		"bad tenant":   {Tenant: "../x", Destination: "d", Config: config.Destination{Parameters: json.RawMessage(`{"stateDir":"/tmp/x"}`)}},
		"bad dest":     {Tenant: "acme", Destination: "../d", Config: config.Destination{Parameters: json.RawMessage(`{"stateDir":"/tmp/x"}`)}},
		"bad inject":   {Tenant: "acme", Destination: "d", Config: config.Destination{Parameters: json.RawMessage(`{"stateDir":"/tmp/x","inject":{"nope":1}}`)}},
	} {
		if _, err := open(ctx, p); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestStateFileIsPrivateAndNotLeftPartial(t *testing.T) {
	dir := t.TempDir()
	d := openMock(t, dir, nil)
	if _, err := d.Upload(context.Background(), upload("x")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir + "/acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "mock-main.json" {
		t.Fatalf("files %v", entries)
	}
	info, _ := os.Stat(d.path)
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}
