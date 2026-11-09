package ffmpeg_test

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg/fixture"
	"github.com/rafaelp/vidyom/profiles"
)

func tool(t *testing.T) *ffmpeg.Tool {
	t.Helper()
	fixture.Require(t)
	return ffmpeg.New("", "")
}

func probe(t *testing.T, tl *ffmpeg.Tool, path string) ffmpeg.Info {
	t.Helper()
	info, err := tl.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	return info
}

func encode(t *testing.T, tl *ffmpeg.Tool, src ffmpeg.Info, profile string) ffmpeg.Info {
	t.Helper()
	p, err := profiles.Lookup(profile)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out."+p.Extension())
	args, err := ffmpeg.TranscodeArgs(src, out, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := tl.Run(context.Background(), args, ffmpeg.RunOptions{}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return probe(t, tl, out)
}

func within(t *testing.T, what string, got, want, tolerance float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Errorf("%s: %.3f, want %.3f within %.3f", what, got, want, tolerance)
	}
}

func TestCheckAcceptsThisHost(t *testing.T) {
	tl := tool(t)
	if _, err := tl.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	version, err := tl.Version(context.Background())
	if err != nil || version == "" {
		t.Fatalf("version %q: %v", version, err)
	}
}

func TestCheckFailsWithoutBinary(t *testing.T) {
	tl := ffmpeg.New(filepath.Join(t.TempDir(), "no-ffmpeg"), filepath.Join(t.TempDir(), "no-ffprobe"))
	if _, err := tl.Check(context.Background()); err == nil {
		t.Fatal("a missing binary must fail the check")
	}
}

func TestProbeConstantRate(t *testing.T) {
	tl := tool(t)
	path := fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 3, Width: 320, Height: 240, Rate: "30000/1001"})
	info := probe(t, tl, path)
	if info.VFR || info.Broken {
		t.Errorf("CFR classified as vfr=%v broken=%v", info.VFR, info.Broken)
	}
	if info.Width != 320 || info.Height != 240 || !info.HasAudio || !info.HasVideo {
		t.Errorf("%+v", info)
	}
	if info.FrameRate != (ffmpeg.Rational{Num: 30000, Den: 1001}) {
		t.Errorf("rate %v", info.FrameRate)
	}
	if info.VideoCodec != "h264" || info.AudioCodec != "aac" || info.Ext != "mp4" {
		t.Errorf("%+v", info)
	}
	within(t, "duration", info.DurationSeconds, 3, 0.1)
}

func TestProbeDetectsVariableRate(t *testing.T) {
	tl := tool(t)
	info := probe(t, tl, fixture.VFR(t, t.TempDir(), 4))
	if !info.VFR {
		t.Fatalf("VFR not detected: %+v", info)
	}
}

func TestProbeDetectsBrokenTimestamps(t *testing.T) {
	tl := tool(t)
	info := probe(t, tl, fixture.BrokenTimestamps(t, t.TempDir(), 3))
	if !info.Broken {
		t.Fatalf("broken timestamps not detected: %+v", info)
	}
}

func TestProbeRotationGivesDisplayDimensions(t *testing.T) {
	tl := tool(t)
	info := probe(t, tl, fixture.Rotated(t, t.TempDir(), 640, 360, 1))
	if info.Rotation%180 == 0 {
		t.Fatalf("rotation %d", info.Rotation)
	}
	if info.Width != 360 || info.Height != 640 {
		t.Fatalf("display %dx%d, want 360x640", info.Width, info.Height)
	}
	if info.EffectiveHeight() != 360 {
		t.Fatalf("effective height %d", info.EffectiveHeight())
	}
}

func TestProbeRefusesAPlaylistInDisguise(t *testing.T) {
	tl := tool(t)
	if _, err := tl.Probe(context.Background(), fixture.Hostile(t, t.TempDir())); err == nil {
		t.Fatal("a playlist with a video extension must not open")
	}
}

func TestExtensionFor(t *testing.T) {
	cases := []struct{ path, format, brand, codec, want string }{
		{"/data/class.MOV", "mov,mp4,m4a,3gp,3g2,mj2", "qt  ", "h264", "mov"},
		{"/spool/acme/01J0000", "mov,mp4,m4a,3gp,3g2,mj2", "qt  ", "h264", "mov"},
		{"/spool/acme/01J0000", "mov,mp4,m4a,3gp,3g2,mj2", "isom", "h264", "mp4"},
		{"/spool/acme/01J0000", "matroska,webm", "", "h264", "mkv"},
		{"/spool/acme/01J0000", "matroska,webm", "", "vp9", "webm"},
		{"/spool/acme/01J0000", "mpegts", "", "h264", "ts"},
		{"/spool/acme/01J0000", "mxf", "", "h264", "mxf"},
		{"/spool/acme/01J0000", "", "", "", "bin"},
	}
	for _, c := range cases {
		if got := ffmpeg.ExtensionFor(c.path, c.format, c.brand, c.codec); got != c.want {
			t.Errorf("%v: %s", c, got)
		}
	}
}

func TestTargetDimensionsNeverUpscales(t *testing.T) {
	cases := []struct{ w, h, ceiling, wantW, wantH int }{
		{320, 240, 1080, 320, 240},
		{1280, 720, 1080, 1280, 720},
		{1920, 1080, 1080, 1920, 1080},
		{2560, 1440, 1080, 1920, 1080},
		{3840, 2160, 720, 1280, 720},
		{1080, 1920, 1080, 1080, 1920}, // portrait is not capped by its long side
		{2160, 3840, 1080, 1080, 1920},
		{321, 241, 1080, 320, 240}, // odd sizes round down, never up
		{1, 1, 1080, 2, 2},
	}
	for _, c := range cases {
		w, h := ffmpeg.TargetDimensions(c.w, c.h, c.ceiling)
		if w != c.wantW || h != c.wantH {
			t.Errorf("%dx%d under %d: %dx%d, want %dx%d", c.w, c.h, c.ceiling, w, h, c.wantW, c.wantH)
		}
	}
}

func TestTargetRate(t *testing.T) {
	cfr := ffmpeg.Info{FrameRate: ffmpeg.Rational{Num: 30000, Den: 1001}, FPS: 29.97}
	if r := ffmpeg.TargetRate(cfr); r != cfr.FrameRate {
		t.Errorf("CFR keeps its rate: %v", r)
	}
	cases := map[float64]ffmpeg.Rational{
		7.3:   {Num: 24000, Den: 1001},
		24:    {Num: 24, Den: 1},
		29.5:  {Num: 30000, Den: 1001},
		29.97: {Num: 30000, Den: 1001},
		45:    {Num: 50, Den: 1},
		60:    {Num: 60, Den: 1},
		119.7: {Num: 120, Den: 1},
		240:   {Num: 120, Den: 1},
		0:     {Num: 30, Den: 1},
	}
	for avg, want := range cases {
		info := ffmpeg.Info{VFR: true, FPS: avg}
		if got := ffmpeg.TargetRate(info); got != want {
			t.Errorf("avg %.2f: %v, want %v", avg, got, want)
		}
	}
}

func TestToleranceIsMaxOfHundredMillisAndOneFrame(t *testing.T) {
	cases := map[float64]float64{0: 0.1, 30: 0.1, 10: 0.1, 5: 0.2, 1: 1}
	for fps, want := range cases {
		within(t, "tolerance", ffmpeg.Tolerance(fps), want, 1e-9)
	}
}

func TestTranscodeKeepsTimelineAndNeverUpscales(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 2, Width: 320, Height: 240}))
	for _, profile := range []string{"master-1080p", "proxy-720p"} {
		out := encode(t, tl, src, profile)
		if out.Width != 320 || out.Height != 240 {
			t.Errorf("%s upscaled to %dx%d", profile, out.Width, out.Height)
		}
		within(t, profile+" duration", out.DurationSeconds, src.DurationSeconds, ffmpeg.Tolerance(src.FPS))
		if out.VideoCodec != "h264" || out.AudioCodec != "aac" {
			t.Errorf("%s codecs: %+v", profile, out)
		}
		if out.VFR {
			t.Errorf("%s not CFR", profile)
		}
	}
}

func TestTranscodeDownscalesToCeiling(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 0.5, Width: 1920, Height: 1080}))
	out := encode(t, tl, src, "proxy-720p")
	if out.Width != 1280 || out.Height != 720 {
		t.Fatalf("proxy of 1080p: %dx%d", out.Width, out.Height)
	}
}

func TestTranscodeNormalizesVariableRate(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.VFR(t, t.TempDir(), 4))
	if !src.VFR {
		t.Fatal("fixture is not VFR")
	}
	out := encode(t, tl, src, "master-1080p")
	if out.VFR {
		t.Fatalf("master still VFR: %+v", out)
	}
	within(t, "duration", out.DurationSeconds, src.DurationSeconds, ffmpeg.Tolerance(src.FPS))
}

func TestTranscodeNormalizesBrokenTimestamps(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.BrokenTimestamps(t, t.TempDir(), 3))
	if !src.Broken {
		t.Fatal("fixture is not broken")
	}
	out := encode(t, tl, src, "master-1080p")
	if out.Broken || out.VFR {
		t.Fatalf("master still irregular: %+v", out)
	}
	within(t, "duration", out.DurationSeconds, src.DurationSeconds, ffmpeg.Tolerance(src.FPS))
}

func TestTranscodeResetsOffsetStartToZero(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.OffsetStart(t, t.TempDir(), 2, 30))
	if src.StartTime < 29 {
		t.Fatalf("fixture start %.3f", src.StartTime)
	}
	out := encode(t, tl, src, "master-1080p")
	within(t, "start", out.StartTime, 0, 0.05)
	within(t, "duration", out.DurationSeconds, src.DurationSeconds, ffmpeg.Tolerance(src.FPS))
}

func TestTranscodeAppliesRotationAndDropsIt(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.Rotated(t, t.TempDir(), 640, 360, 1))
	out := encode(t, tl, src, "proxy-720p")
	if out.Rotation != 0 || out.Width != 360 || out.Height != 640 {
		t.Fatalf("rotation %d, %dx%d", out.Rotation, out.Width, out.Height)
	}
}

func TestTranscodeWithoutAudio(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 1, NoAudio: true}))
	out := encode(t, tl, src, "master-1080p")
	if out.HasAudio {
		t.Fatal("an audio stream appeared")
	}
}

func TestTranscodeTenBitToEightBitHigh(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.Video(t, t.TempDir(), fixture.Spec{
		Seconds: 0.5,
		Codec:   []string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le", "-profile:v", "high10"},
	}))
	if src.BitDepth != 10 {
		t.Fatalf("fixture depth %d", src.BitDepth)
	}
	out := encode(t, tl, src, "master-1080p")
	if out.BitDepth != 8 {
		t.Fatalf("depth %d", out.BitDepth)
	}
}

func TestHEVCMasterKeepsResolutionAndTag(t *testing.T) {
	tl := tool(t)
	if !tl.HasEncoder(context.Background(), "libx265") {
		t.Skip("no libx265")
	}
	src := probe(t, tl, fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 0.4, Width: 2560, Height: 1440, Rate: "10"}))
	p, err := profiles.Resolve("auto:master", src.EffectiveHeight())
	if err != nil || p.ID != "master-2160p-hevc" {
		t.Fatalf("%+v %v", p, err)
	}
	out := encode(t, tl, src, p.ID)
	if out.VideoCodec != "hevc" || out.Height != 1440 {
		t.Fatalf("%+v", out)
	}
}

func TestAudioExtractKeepsTimelineMonoAndRate(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 2}))
	p, _ := profiles.Lookup("speech-opus")
	out := filepath.Join(t.TempDir(), "speech.opus")
	args, err := ffmpeg.AudioExtractArgs(src, out, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := tl.Run(context.Background(), args, ffmpeg.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	info := probe(t, tl, out)
	if info.HasVideo || info.AudioCodec != "opus" || info.AudioChannels != 1 {
		t.Fatalf("%+v", info)
	}
	within(t, "duration", info.DurationSeconds, src.DurationSeconds, 0.1)
}

func TestAudioExtractPadsAShortTrack(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.ShortAudio(t, t.TempDir(), 3, 1))
	p, _ := profiles.Lookup("speech-opus")
	out := filepath.Join(t.TempDir(), "speech.opus")
	args, err := ffmpeg.AudioExtractArgs(src, out, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := tl.Run(context.Background(), args, ffmpeg.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	within(t, "duration", probe(t, tl, out).DurationSeconds, src.DurationSeconds, 0.1)
}

func TestExtractRatesNeverUpscale(t *testing.T) {
	p, _ := profiles.Lookup("speech-opus")
	cases := map[int]int{48000: 16000, 44100: 16000, 16000: 16000, 12000: 12000, 11025: 8000, 8000: 8000, 0: 16000}
	for source, want := range cases {
		if got := ffmpeg.ExtractSampleRate(p.Audio, source); got != want {
			t.Errorf("source %d Hz: %d, want %d", source, got, want)
		}
	}
	bitrates := map[int]string{192000: "24k", 24000: "24k", 16000: "16k", 0: "24k", 3000: "24k"}
	for source, want := range bitrates {
		if got := ffmpeg.ExtractBitrate(p.Audio, source); got != want {
			t.Errorf("source %d bps: %s, want %s", source, got, want)
		}
	}
}

func TestSegmentsCutOnTheFrameNotTheKeyframe(t *testing.T) {
	tl := tool(t)
	// Red for 2 s, blue for 2 s, with no keyframe at the boundary.
	src := probe(t, tl, fixture.TwoColors(t, t.TempDir(), 4))
	p, _ := profiles.Lookup("master-1080p")
	out := filepath.Join(t.TempDir(), "cut.mp4")
	args, err := ffmpeg.SegmentArgs(src, []ffmpeg.Segment{{Start: 2.0, End: 3.0}}, out, ffmpeg.SegmentEncode{
		Video: *p.Video, Audio: p.Audio, Container: "mp4", Faststart: true, MaxShort: p.Video.MaxHeight,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tl.Run(context.Background(), args, ffmpeg.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	info := probe(t, tl, out)
	within(t, "duration", info.DurationSeconds, 1, ffmpeg.Tolerance(src.FPS))
	red, blue := firstFrameColor(t, tl, out)
	if blue <= red {
		t.Fatalf("first frame is not blue: r=%d b=%d", red, blue)
	}
}

func firstFrameColor(t *testing.T, tl *ffmpeg.Tool, path string) (red, blue int) {
	t.Helper()
	raw := filepath.Join(t.TempDir(), "frame.rgb")
	args := append(ffmpeg.InputArgs(path), "-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "rgb24", raw)
	if err := tl.Run(context.Background(), args, ffmpeg.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(data) / 2
	mid -= mid % 3
	return int(data[mid]), int(data[mid+2])
}

func TestSegmentsJoinIntoOneTimeline(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 4}))
	p, _ := profiles.Lookup("master-1080p")
	out := filepath.Join(t.TempDir(), "joined.mp4")
	segments := []ffmpeg.Segment{{Start: 0.5, End: 1.5}, {Start: 2.5, End: 3.25}}
	args, err := ffmpeg.SegmentArgs(src, segments, out, ffmpeg.SegmentEncode{
		Video: *p.Video, Audio: p.Audio, Container: "mp4", Faststart: true, MaxShort: p.Video.MaxHeight,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tl.Run(context.Background(), args, ffmpeg.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	info := probe(t, tl, out)
	within(t, "duration", info.DurationSeconds, 1.75, 2*ffmpeg.Tolerance(src.FPS))
	if !info.HasAudio {
		t.Fatal("audio lost")
	}
}

func TestThumbnailIsAJPEGNeverUpscaled(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 1, Width: 320, Height: 240}))
	out := filepath.Join(t.TempDir(), "thumb.jpg")
	args, err := ffmpeg.ThumbnailArgs(src, 0.5, out)
	if err != nil {
		t.Fatal(err)
	}
	if err := tl.Run(context.Background(), args, ffmpeg.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 3 || data[0] != 0xff || data[1] != 0xd8 {
		t.Fatal("not a JPEG")
	}
}

func TestProgressAndCancellation(t *testing.T) {
	tl := tool(t)
	src := probe(t, tl, fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 1, Width: 320, Height: 240}))
	p, _ := profiles.Lookup("proxy-720p")
	out := filepath.Join(t.TempDir(), "out.mp4")
	args, err := ffmpeg.TranscodeArgs(src, out, p)
	if err != nil {
		t.Fatal(err)
	}
	var last float64
	if err := tl.Run(context.Background(), args, ffmpeg.RunOptions{DurationSeconds: src.DurationSeconds, OnProgress: func(v float64) { last = v }}); err != nil {
		t.Fatal(err)
	}
	if last <= 0 || last > 100 {
		t.Fatalf("progress %.1f", last)
	}

	// An endless encode must stop promptly on cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tl.Run(ctx, []string{"-re", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30", "-c:v", "libx264", "-preset", "ultrafast", "-f", "null", "-"}, ffmpeg.RunOptions{})
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ffmpeg did not stop")
	}
}

func TestRunErrorKeepsStderrInternally(t *testing.T) {
	tl := tool(t)
	err := tl.Run(context.Background(), ffmpeg.InputArgs(filepath.Join(t.TempDir(), "absent.mp4")), ffmpeg.RunOptions{})
	var runErr *ffmpeg.RunError
	if !errors.As(err, &runErr) || runErr.ExitCode == 0 || !strings.Contains(runErr.Stderr, "absent.mp4") {
		t.Fatalf("%v", err)
	}
}

func TestMinimalEnvironmentHidesWorkerSecrets(t *testing.T) {
	t.Setenv("VIDYOM_TEST_SECRET", "s3cret-value-for-test")
	if strings.Contains(strings.Join(ffmpeg.ExportedMinimalEnvForTest(), "\n"), "s3cret") {
		t.Fatal("a worker environment variable reached the child")
	}
}
