package videonodes_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg/fixture"
	"github.com/rafaelp/vidyom/internal/workers/nodeutil"
	"github.com/rafaelp/vidyom/internal/workers/provenance"
	"github.com/rafaelp/vidyom/internal/workers/workertest"
	storagenodes "github.com/rafaelp/vidyom/nodes/storage"
	videonodes "github.com/rafaelp/vidyom/nodes/video"
	"github.com/rafaelp/vidyom/schemas"
)

var ctx = context.Background()

func registry() node.Registry {
	r := node.NewRegistry()
	videonodes.Register(r)
	storagenodes.Register(r)
	return r
}

func nodeOf(t *testing.T, nodeType string) node.Node {
	t.Helper()
	n, ok := registry().Lookup(nodeType, 1)
	if !ok {
		t.Fatalf("%s is not registered", nodeType)
	}
	return n
}

func probeFile(t *testing.T, path string) ffmpeg.Info {
	t.Helper()
	info, err := ffmpeg.Default().Probe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// withServerRoot makes a directory reachable through server: inputs.
func withServerRoot(t *testing.T, h *workertest.Harness) string {
	t.Helper()
	root := t.TempDir()
	h.Deps.IngestRoots = []string{root}
	return root
}

func TestEveryCatalogVideoNodeIsRegistered(t *testing.T) {
	r := registry()
	catalog, err := schemas.NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range catalog.Nodes {
		if !strings.HasPrefix(entry.Type, "video.") {
			continue
		}
		n, ok := r.Lookup(entry.Type, entry.TypeVersion)
		if !ok {
			t.Errorf("%s is in the catalog and not registered", entry.Type)
			continue
		}
		if err := schemas.CheckDescriptor(n.Descriptor()); err != nil {
			t.Error(err)
		}
	}
}

func TestProbeOutput(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Name: "class.MOV", Seconds: 2, Width: 640, Height: 360, Extra: []string{"-f", "mov"}})
	out := h.MustRun(ctx, nodeOf(t, "video.probe"), "probe", map[string]any{"source": "server:" + raw})
	if out.Number("width") != 640 || out.Number("height") != 360 || out.Output["vfr"] != false || out.Output["broken_timestamps"] != false {
		t.Fatalf("%+v", out.Output)
	}
	if out.Field("ext") != "mov" || out.Field("video_codec") != "h264" || out.Field("audio_codec") != "aac" {
		t.Fatalf("%+v", out.Output)
	}
	if math.Abs(out.Number("duration_seconds")-2) > 0.1 {
		t.Fatalf("%+v", out.Output)
	}
}

// R26: a spooled upload has no extension; original.<ext> still needs one.
func TestProbeExtensionOfAnUpload(t *testing.T) {
	h := workertest.New(t)
	upload := id.New()
	path := filepath.Join(h.Deps.SpoolDir, "acme", string(upload))
	raw := fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 1, Name: "x.mkv"})
	data, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out := h.MustRun(ctx, nodeOf(t, "video.probe"), "probe", map[string]any{"source": "upload:" + string(upload)})
	if out.Field("ext") != "mkv" {
		t.Fatalf("%+v", out.Output)
	}
}

func TestProbeDetectsVFRAndRefusesArchiveAndNonMedia(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	out := h.MustRun(ctx, nodeOf(t, "video.probe"), "probe", map[string]any{"source": "server:" + fixture.VFR(t, root, 4)})
	if out.Output["vfr"] != true {
		t.Fatalf("%+v", out.Output)
	}
	h.PutFile(domain.RoleArchive, "raw.mp4", fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 1}))
	if _, err := h.Run(ctx, nodeOf(t, "video.probe"), "p2", map[string]any{"source": "storage://archive/raw.mp4"}); !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Errorf("archive: %v", err)
	}
	notMedia := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(notMedia, []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := h.Run(ctx, nodeOf(t, "video.probe"), "p3", map[string]any{"source": "server:" + notMedia})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Errorf("not media: %v", err)
	}
	hostile := fixture.Hostile(t, root)
	if _, err := h.Run(ctx, nodeOf(t, "video.probe"), "p4", map[string]any{"source": "server:" + hostile}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Errorf("hostile playlist: %v", err)
	}
}

func TestTranscodeMasterAndProxyAreNeverUpscaled(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 2, Width: 320, Height: 240})
	master := h.MustRun(ctx, nodeOf(t, "video.transcode"), "master", map[string]any{"source": "server:" + raw, "profile": "auto:master"})
	if master.Field("profile") != "master-1080p@1" {
		t.Fatalf("%+v", master.Output)
	}
	if master.Number("width") != 320 || master.Number("height") != 240 {
		t.Fatalf("upscaled: %+v", master.Output)
	}
	if master.Result.Tools["ffmpeg"] == "" || master.Field("ffmpeg_version") == "" {
		t.Fatal("no FFmpeg version for provenance (R21)")
	}
	proxy := h.MustRun(ctx, nodeOf(t, "video.transcode"), "proxy", map[string]any{"source": master.Field("file"), "profile": "proxy-720p"})
	if proxy.Number("height") != 240 || proxy.Field("profile") != "proxy-720p@1" {
		t.Fatalf("%+v", proxy.Output)
	}
	origin, known, err := provenance.Read(h.ScratchPath(master.Field("file")))
	if err != nil || !known || origin.Family != "master" || origin.Profile != "master-1080p@1" {
		t.Fatalf("%+v %v %v", origin, known, err)
	}
	sum, size, err := provenance.HashFile(ctx, h.ScratchPath(master.Field("file")))
	if err != nil || sum != master.Field("sha256") || float64(size) != master.Number("size_bytes") {
		t.Fatalf("%s %d %v", sum, size, err)
	}
	info := probeFile(t, h.ScratchPath(master.Field("file")))
	if info.VFR || info.AudioCodec != "aac" {
		t.Fatalf("%+v", info)
	}
}

func TestTranscodeRoundsOddDimensionsDown(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 0.5, Width: 322, Height: 242, Filter: "crop=321:241"})
	out := h.MustRun(ctx, nodeOf(t, "video.transcode"), "master", map[string]any{"source": "server:" + raw, "profile": "auto:master"})
	if out.Number("width") != 320 || out.Number("height") != 240 {
		t.Fatalf("%+v", out.Output)
	}
}

// I184: a portrait 1080p recording is 1080p, not a 1920-line source to shrink or to
// promote to HEVC.
func TestAutoMasterKeepsAPortraitRecordingWhole(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 0.3, Width: 1080, Height: 1920, Rate: "10"})
	out := h.MustRun(ctx, nodeOf(t, "video.transcode"), "master", map[string]any{"source": "server:" + raw, "profile": "auto:master"})
	if out.Field("profile") != "master-1080p@1" || out.Number("width") != 1080 || out.Number("height") != 1920 {
		t.Fatalf("%+v", out.Output)
	}
}

// D14: sources of 2160p keep 4K, in HEVC.
func TestAutoMasterOf2160pIsHEVCAt2160p(t *testing.T) {
	if !ffmpeg.Default().HasEncoder(ctx, "libx265") {
		t.Skip("no libx265")
	}
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 0.4, Width: 3840, Height: 2160, Rate: "5", NoAudio: true})
	out := h.MustRun(ctx, nodeOf(t, "video.transcode"), "master", map[string]any{"source": "server:" + raw, "profile": "auto:master"})
	if out.Field("profile") != "master-2160p-hevc@1" || out.Number("height") != 2160 {
		t.Fatalf("%+v", out.Output)
	}
	if info := probeFile(t, h.ScratchPath(out.Field("file"))); info.VideoCodec != "hevc" {
		t.Fatalf("%+v", info)
	}
}

func TestTranscodeNormalizesAVariableRateSource(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.VFR(t, root, 4)
	src := probeFile(t, raw)
	out := h.MustRun(ctx, nodeOf(t, "video.transcode"), "master", map[string]any{"source": "server:" + raw, "profile": "master-1080p"})
	info := probeFile(t, h.ScratchPath(out.Field("file")))
	if info.VFR || info.Broken {
		t.Fatalf("the master is irregular: %+v", info)
	}
	if math.Abs(info.DurationSeconds-src.DurationSeconds) > ffmpeg.Tolerance(src.FPS) {
		t.Fatalf("%.3f vs %.3f", info.DurationSeconds, src.DurationSeconds)
	}
}

func TestTranscodeRefusals(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 1})
	n := nodeOf(t, "video.transcode")
	cases := []struct {
		name    string
		source  string
		profile string
		code    errs.Code
	}{
		{"unknown profile", "server:" + raw, "nope", errs.CodeInvalidArgument},
		{"audio profile", "server:" + raw, "speech-opus", errs.CodeInvalidArgument},
		{"unknown version", "server:" + raw, "master-1080p@9", errs.CodeInvalidArgument},
		{"import", "import:youtube/abc", "master-1080p", errs.CodeNotImplemented},
		{"archive", "storage://archive/raw.mp4", "master-1080p", errs.CodeForbiddenStorageRole},
	}
	for _, c := range cases {
		if _, err := h.Run(ctx, n, "t", map[string]any{"source": c.source, "profile": c.profile}); !errs.IsCode(err, c.code) {
			t.Errorf("%s: %v, want %s", c.name, err, c.code)
		}
	}
}

func TestFailedRunLeavesNoOutput(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	notMedia := filepath.Join(root, "x.mp4")
	if err := os.WriteFile(notMedia, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := h.Run(ctx, nodeOf(t, "video.transcode"), "bad", map[string]any{"source": "server:" + notMedia, "profile": "proxy-720p"})
	if err == nil {
		t.Fatal("garbage encoded")
	}
	assertNoOutput(t, h, "bad")
}

// assertNoOutput fails if a node's scratch directory holds an output or a temp file.
func assertNoOutput(t *testing.T, h *workertest.Harness, nodeID string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.Deps.ScratchDir, "acme", string(h.Job), nodeID))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".input-") {
			continue // the task's inputs, removed when its environment closes
		}
		t.Fatalf("left behind in %s: %s", nodeID, entry.Name())
	}
}

// Each encode has its own temp file, so a stale attempt cannot write into a successor's.
func TestTempNamesAreUniquePerRun(t *testing.T) {
	a := nodeutil.TempFor("/scratch/acme/job/node/clip.mp4")
	b := nodeutil.TempFor("/scratch/acme/job/node/clip.mp4")
	if a == b {
		t.Fatalf("both runs got %s", a)
	}
	if filepath.Dir(a) != "/scratch/acme/job/node" || !strings.HasSuffix(a, "-clip.mp4") {
		t.Fatalf("%s", a)
	}
}

func TestErrorMessagesHideToolOutputAndPaths(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	notMedia := filepath.Join(root, "secret-title-of-recording.mp4")
	if err := os.WriteFile(notMedia, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := h.Run(ctx, nodeOf(t, "video.transcode"), "bad", map[string]any{"source": "server:" + notMedia, "profile": "proxy-720p"})
	if err == nil {
		t.Fatal("garbage encoded")
	}
	public := errs.Public(err)
	if strings.Contains(public.Message, "secret-title") || strings.Contains(public.Message, "Invalid data") {
		t.Fatalf("tool output reached a client message: %q", public.Message)
	}
}

func TestValidateTimeline(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Name: "raw.mp4", Seconds: 2})
	master := h.MustRun(ctx, nodeOf(t, "video.transcode"), "master", map[string]any{"source": "server:" + raw, "profile": "master-1080p"})
	n := nodeOf(t, "video.validate_timeline")
	out := h.MustRun(ctx, n, "validate", map[string]any{"reference": "server:" + raw, "candidates": []string{master.Field("file")}})
	if out.Output["valid"] != true || out.Number("tolerance_seconds") != 0.1 || out.Number("reference_duration") <= 0 {
		t.Fatalf("%+v", out.Output)
	}
	durations, _ := out.Output["durations"].(map[string]any)
	if durations[master.Field("file")] == nil {
		t.Fatalf("%+v", out.Output)
	}

	short := fixture.Video(t, root, fixture.Spec{Name: "short.mp4", Seconds: 1.5})
	_, err := h.Run(ctx, n, "validate2", map[string]any{"reference": "server:" + raw, "candidates": []string{"server:" + short}})
	if !errs.IsCode(err, errs.CodeValidationFailed) {
		t.Fatalf("a shorter candidate passed: %v", err)
	}
	public := errs.Public(err)
	if public.Details["tolerance_seconds"] != 0.1 || public.Details["reference_duration"] == nil || public.Details["durations"] == nil {
		t.Fatalf("no durations in the error: %+v", public.Details)
	}
	// Details do not reach the job's record, so the figures must be in the message.
	if !strings.Contains(public.Message, "2.0") || !strings.Contains(public.Message, "1.5") {
		t.Fatalf("no durations in the message: %q", public.Message)
	}
	if strings.Contains(public.Message, root) {
		t.Fatalf("a path reached the message: %q", public.Message)
	}
}

// One frame of a 5 fps source is 200 ms, so max(100 ms, 1 frame) allows more.
func TestToleranceWidensToOneFrame(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Name: "slow.mp4", Seconds: 2, Rate: "5", NoAudio: true})
	near := fixture.Video(t, root, fixture.Spec{Name: "near.mp4", Seconds: 1.85, Rate: "30", NoAudio: true})
	out := h.MustRun(ctx, nodeOf(t, "video.validate_timeline"), "validate", map[string]any{"reference": "server:" + raw, "candidates": []string{"server:" + near}})
	if out.Number("tolerance_seconds") != 0.2 {
		t.Fatalf("%+v", out.Output)
	}
}

func clipHarness(t *testing.T, seconds float64) *workertest.Harness {
	t.Helper()
	h := workertest.New(t)
	h.PutFile(domain.RoleMaster, "originals/ORI-001/v1/master.mp4", fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: seconds}))
	h.PutFile(domain.RoleProxy, "originals/ORI-001/v1/proxy.mp4", fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: seconds}))
	h.PutFile(domain.RoleArchive, "originals/ORI-001/v1/original.mp4", fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: seconds}))
	h.ResetAccess()
	return h
}

const (
	masterURI  = "storage://master/originals/ORI-001/v1/master.mp4"
	proxyURI   = "storage://proxy/originals/ORI-001/v1/proxy.mp4"
	archiveURI = "storage://archive/originals/ORI-001/v1/original.mp4"
)

func seg(start, end float64) []map[string]any {
	return []map[string]any{{"start_seconds": start, "end_seconds": end}}
}

func TestCutRendersFromMasterAndNeverReadsArchive(t *testing.T) {
	h := clipHarness(t, 4)
	out := h.MustRun(ctx, nodeOf(t, "video.cut"), "render", map[string]any{
		"source":   masterURI,
		"segments": []map[string]any{{"source_asset_id": "ORI-001", "source_version": "v1", "start_seconds": 1.0, "end_seconds": 2.5}},
	})
	if math.Abs(out.Number("duration_seconds")-1.5) > 0.1 {
		t.Fatalf("%+v", out.Output)
	}
	segments, _ := out.Output["segments"].([]any)
	first, _ := segments[0].(map[string]any)
	if first["source_asset_id"] != "ORI-001" || first["start_seconds"] != 1.0 {
		t.Fatalf("the interval map was lost: %+v", out.Output)
	}
	if out.Field("profile") != "master-1080p@1" || out.Field("ffmpeg_version") == "" {
		t.Fatalf("%+v", out.Output)
	}
	if reads := h.ArchiveReads(); len(reads) != 0 {
		t.Fatalf("clipping read archive: %+v", reads)
	}
	if len(h.Reads(domain.RoleMaster)) == 0 {
		t.Fatal("clipping did not read master")
	}
	origin, known, err := provenance.Read(h.ScratchPath(out.Field("file")))
	if err != nil || !known || origin.Family != "master" {
		t.Fatalf("clip master has no family: %+v %v %v", origin, known, err)
	}
}

func TestCutRefusesEverythingButTheMaster(t *testing.T) {
	h := clipHarness(t, 2)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 1})
	loose := h.MustRun(ctx, nodeOf(t, "video.transcode"), "loose", map[string]any{"source": "server:" + raw, "profile": "master-1080p"})
	n := nodeOf(t, "video.cut")
	for name, source := range map[string]string{
		"archive":              archiveURI,
		"proxy":                proxyURI,
		"server file":          "server:" + raw,
		"upload":               "upload:UPL-001",
		"import":               "import:youtube/abc",
		"scratch with no role": loose.Field("file"),
	} {
		_, err := h.Run(ctx, n, "render", map[string]any{"source": source, "segments": seg(0, 0.5)})
		if !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
			t.Errorf("%s: %v, want forbidden_storage_role", name, err)
		}
	}
	if reads := h.ArchiveReads(); len(reads) != 0 {
		t.Fatalf("a refused cut read archive: %+v", reads)
	}
	// No fallback: with master missing, archive is not tried.
	_, err := h.Run(ctx, n, "render", map[string]any{"source": "storage://master/originals/ORI-001/v1/gone.mp4", "segments": seg(0, 0.5)})
	if !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("missing master: %v", err)
	}
	if reads := h.ArchiveReads(); len(reads) != 0 {
		t.Fatalf("a missing master fell back to archive: %+v", reads)
	}
}

func TestCutAcceptsAVerifiedLocalCopyOfTheMaster(t *testing.T) {
	h := clipHarness(t, 2)
	copied := h.MustRun(ctx, nodeOf(t, "storage.get"), "fetch", map[string]any{"source": masterURI})
	h.MustRun(ctx, nodeOf(t, "video.cut"), "render", map[string]any{"source": copied.Field("file"), "segments": seg(0, 1)})

	// A damaged copy no longer matches its origin's checksum.
	path := h.ScratchPath(copied.Field("file"))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("appended")
	_ = f.Close()
	_, err = h.Run(ctx, nodeOf(t, "video.cut"), "render2", map[string]any{"source": copied.Field("file"), "segments": seg(0, 1)})
	if !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Fatalf("a tampered copy was accepted: %v", err)
	}
}

func TestCutRefusesACachedCopyOfAnotherRole(t *testing.T) {
	h := clipHarness(t, 2)
	copied := h.MustRun(ctx, nodeOf(t, "storage.get"), "fetch", map[string]any{"source": proxyURI})
	_, err := h.Run(ctx, nodeOf(t, "video.cut"), "render", map[string]any{"source": copied.Field("file"), "segments": seg(0, 1)})
	if !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Fatalf("a copy of the proxy was accepted: %v", err)
	}
}

// D22: the checksum is verified on every reuse.
func TestCutReusesTheCacheAndDetectsATamperedEntry(t *testing.T) {
	h := clipHarness(t, 2)
	n := nodeOf(t, "video.cut")
	h.MustRun(ctx, n, "first", map[string]any{"source": masterURI, "segments": seg(0, 0.5)})
	reads := len(h.Reads(domain.RoleMaster))
	h.MustRun(ctx, n, "second", map[string]any{"source": masterURI, "segments": seg(0.5, 1)})
	if got := len(h.Reads(domain.RoleMaster)); got != reads {
		t.Fatalf("the cached master was not reused: %d, then %d", reads, got)
	}
	matches, err := filepath.Glob(filepath.Join(h.Root, "cache", "acme", "*.vcache"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("%v %v", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(matches[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	h.MustRun(ctx, n, "third", map[string]any{"source": masterURI, "segments": seg(1, 1.5)})
	if got := len(h.Reads(domain.RoleMaster)); got != reads+1 {
		t.Fatalf("a tampered entry was not replaced from master: %d, then %d", reads, got)
	}
}

// §7.2 and §15.4: a limitation is declared, and the failure explains itself.
func TestCutExplainsAMasterWithoutDirectAccess(t *testing.T) {
	h := clipHarness(t, 2)
	inst := h.Services.Instances[domain.RoleMaster]
	inst.Usage = config.UsageBackupOnly
	h.Services.Instances[domain.RoleMaster] = inst
	_, err := h.Run(ctx, nodeOf(t, "video.cut"), "render", map[string]any{"source": masterURI, "segments": seg(0, 1)})
	if !errs.IsCode(err, errs.CodeCapabilityUnsupported) || !strings.Contains(errs.Public(err).Message, "direct access") {
		t.Fatalf("%v", err)
	}
}

func TestCutSegmentRules(t *testing.T) {
	h := clipHarness(t, 2)
	n := nodeOf(t, "video.cut")
	bad := map[string][]map[string]any{
		"reversed":        seg(1, 0.5),
		"negative":        seg(-1, 0.5),
		"past the end":    seg(1, 9),
		"starts past end": seg(5, 6),
		"too many":        manySegments(65),
	}
	for name, segments := range bad {
		if _, err := h.Run(ctx, n, "render", map[string]any{"source": masterURI, "segments": segments}); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Just past the end, within the tolerance, is rounding.
	out := h.MustRun(ctx, n, "ok", map[string]any{"source": masterURI, "segments": seg(1.5, 2.05)})
	if math.Abs(out.Number("duration_seconds")-0.5) > 0.11 {
		t.Fatalf("%+v", out.Output)
	}
}

func manySegments(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"start_seconds": 0.0, "end_seconds": 0.1}
	}
	return out
}

// A clip needs video. Where the video stream ends before the audio, the segment is
// refused rather than rendered as audio over nothing or failing inside FFmpeg.
func TestSegmentsMustFallInsideTheVideoStream(t *testing.T) {
	h := workertest.New(t)
	video := fixture.Generate(t, t.TempDir(), "short-video.mp4",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:d=3",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:d=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "64k")
	h.PutFile(domain.RoleMaster, "m.mp4", video)
	h.PutFile(domain.RoleProxy, "p.mp4", video)
	cut := nodeOf(t, "video.cut")
	for name, segments := range map[string][]map[string]any{
		"past the video":     seg(4, 5),
		"crossing its end":   seg(2, 4),
		"second one past it": {{"start_seconds": 1.0, "end_seconds": 2.0}, {"start_seconds": 4.0, "end_seconds": 5.0}},
	} {
		if _, err := h.Run(ctx, cut, "render", map[string]any{"source": "storage://master/m.mp4", "segments": segments}); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("cut %s: %v", name, err)
		}
	}
	if _, err := h.Run(ctx, nodeOf(t, "video.preview"), "preview", map[string]any{"source": "storage://proxy/p.mp4", "segments": seg(4, 5)}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("preview: %v", err)
	}
	out := h.MustRun(ctx, cut, "ok", map[string]any{"source": "storage://master/m.mp4", "segments": seg(1, 2)})
	if math.Abs(out.Number("duration_seconds")-1) > 0.1 {
		t.Fatalf("%+v", out.Output)
	}
}

func TestCutAssemblesSeveralSegments(t *testing.T) {
	h := clipHarness(t, 4)
	out := h.MustRun(ctx, nodeOf(t, "video.cut"), "render", map[string]any{
		"source": masterURI,
		"segments": []map[string]any{
			{"start_seconds": 0.5, "end_seconds": 1.5},
			{"start_seconds": 2.5, "end_seconds": 3.0},
		},
	})
	if math.Abs(out.Number("duration_seconds")-1.5) > 0.2 {
		t.Fatalf("%+v", out.Output)
	}
}

func TestCutWithAProfileNeverUpscales(t *testing.T) {
	h := clipHarness(t, 2)
	out := h.MustRun(ctx, nodeOf(t, "video.cut"), "render", map[string]any{"source": masterURI, "segments": seg(0, 1), "profile": "proxy-720p"})
	if out.Field("profile") != "proxy-720p@1" {
		t.Fatalf("%+v", out.Output)
	}
	if info := probeFile(t, h.ScratchPath(out.Field("file"))); info.Height != 240 {
		t.Fatalf("%+v", info)
	}
	if _, err := h.Run(ctx, nodeOf(t, "video.cut"), "r2", map[string]any{"source": masterURI, "segments": seg(0, 1), "profile": "speech-opus"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("%v", err)
	}
}

func TestPreviewComesFromTheProxy(t *testing.T) {
	h := clipHarness(t, 4)
	n := nodeOf(t, "video.preview")
	out := h.MustRun(ctx, n, "preview", map[string]any{"source": proxyURI, "segments": seg(1, 2)})
	if math.Abs(out.Number("duration_seconds")-1) > 0.1 || out.Field("file") == "" {
		t.Fatalf("%+v", out.Output)
	}
	if info := probeFile(t, h.ScratchPath(out.Field("file"))); info.VideoCodec != "h264" || info.Height != 240 {
		t.Fatalf("%+v", info)
	}
	if reads := h.Reads(domain.RoleMaster); len(reads) != 0 {
		t.Fatalf("a preview read master: %+v", reads)
	}
	for name, source := range map[string]string{"master": masterURI, "archive": archiveURI} {
		if _, err := h.Run(ctx, n, "p2", map[string]any{"source": source, "segments": seg(0, 1)}); !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := h.Run(ctx, n, "p3", map[string]any{"source": proxyURI, "segments": seg(1, 99)}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("past the end: %v", err)
	}
}

func TestThumbnail(t *testing.T) {
	h := clipHarness(t, 2)
	n := nodeOf(t, "video.thumbnail")
	out := h.MustRun(ctx, n, "thumb", map[string]any{"source": proxyURI, "atSeconds": 1.0})
	data, err := os.ReadFile(h.ScratchPath(out.Field("file")))
	if err != nil || len(data) < 3 || data[0] != 0xff || data[1] != 0xd8 {
		t.Fatalf("not a JPEG: %v", err)
	}
	if out.Number("at_seconds") != 1 || out.Field("sha256") == "" {
		t.Fatalf("%+v", out.Output)
	}
	// The default position, and a position past the end, both give a frame.
	if out = h.MustRun(ctx, n, "t2", map[string]any{"source": proxyURI}); out.Number("at_seconds") <= 0 {
		t.Fatalf("%+v", out.Output)
	}
	if out = h.MustRun(ctx, n, "t3", map[string]any{"source": proxyURI, "atSeconds": 99.0}); out.Number("at_seconds") >= 2 {
		t.Fatalf("not clamped: %+v", out.Output)
	}
	if _, err := h.Run(ctx, n, "t4", map[string]any{"source": archiveURI}); !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Errorf("archive: %v", err)
	}
}

func TestCancellationStopsAnEncode(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 20, Width: 1280, Height: 720, Rate: "30"})
	cancelled, cancel := context.WithCancel(ctx)
	env := h.Env("master", "video.transcode")
	n := nodeOf(t, "video.transcode")
	done := make(chan error, 1)
	go func() {
		_, err := n.Execute(cancelled, env, node.Input{
			Parameters: []byte(`{"source":"server:` + raw + `","profile":"master-1080p"}`),
		})
		done <- err
	}()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled encode reported success")
	}
	assertNoOutput(t, h, "master")
}

// A failure after ffmpeg started must clean its unique temp file too, or retries would
// pile them up until the job ended.
func TestCancelledEncodeLeavesNoTempFile(t *testing.T) {
	h := workertest.New(t)
	root := withServerRoot(t, h)
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 30, Width: 1280, Height: 720})
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	env := h.Env("slow", "video.transcode")
	n := nodeOf(t, "video.transcode")
	done := make(chan error, 1)
	go func() {
		_, err := n.Execute(cancelled, env, node.Input{
			Parameters: []byte(`{"source":"server:` + raw + `","profile":"master-1080p"}`),
		})
		done <- err
	}()
	dir := filepath.Join(h.Deps.ScratchDir, "acme", string(h.Job), "slow")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if matches, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); len(matches) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the encode never started writing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled encode reported success")
	}
	assertNoOutput(t, h, "slow")
}
