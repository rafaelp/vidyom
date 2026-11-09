package audionodes_test

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg/fixture"
	"github.com/rafaelp/vidyom/internal/workers/workertest"
	audionodes "github.com/rafaelp/vidyom/nodes/audio"
	videonodes "github.com/rafaelp/vidyom/nodes/video"
	"github.com/rafaelp/vidyom/schemas"
)

var ctx = context.Background()

func registry() node.Registry {
	r := node.NewRegistry()
	audionodes.Register(r)
	videonodes.Register(r)
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

func TestCatalogDescriptor(t *testing.T) {
	if err := schemas.CheckDescriptor(nodeOf(t, "audio.extract").Descriptor()); err != nil {
		t.Fatal(err)
	}
}

func TestExtractFromTheStoredMaster(t *testing.T) {
	h := workertest.New(t)
	master := fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 2})
	h.PutFile(domain.RoleMaster, "originals/ORI-001/v1/master.mp4", master)
	out := h.MustRun(ctx, nodeOf(t, "audio.extract"), "asr", map[string]any{"source": "storage://master/originals/ORI-001/v1/master.mp4"})
	info, err := ffmpeg.Default().Probe(ctx, h.ScratchPath(out.Field("file")))
	if err != nil {
		t.Fatal(err)
	}
	if info.HasVideo || info.AudioCodec != "opus" || info.AudioChannels != 1 {
		t.Fatalf("%+v", info)
	}
	if out.Field("profile") != "speech-opus@1" || out.Field("sha256") == "" {
		t.Fatalf("%+v", out.Output)
	}
	if math.Abs(out.Number("duration_seconds")-2) > 0.1 {
		t.Fatalf("duration %.3f", out.Number("duration_seconds"))
	}
	if out.Result.Tools["ffmpeg"] == "" {
		t.Fatal("no FFmpeg version for provenance")
	}
}

func TestExtractFromAScratchMaster(t *testing.T) {
	h := workertest.New(t)
	root := t.TempDir()
	h.Deps.IngestRoots = []string{root}
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 1})
	master := h.MustRun(ctx, nodeOf(t, "video.transcode"), "compress", map[string]any{"source": "server:" + raw, "profile": "master-1080p"})
	h.MustRun(ctx, nodeOf(t, "audio.extract"), "asr", map[string]any{"source": master.Field("file")})
}

// D56: ASR audio is always from the master.
func TestExtractRefusesEverythingElse(t *testing.T) {
	h := workertest.New(t)
	root := t.TempDir()
	h.Deps.IngestRoots = []string{root}
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 1})
	h.PutFile(domain.RoleProxy, "p.mp4", raw)
	h.PutFile(domain.RoleArchive, "a.mp4", raw)
	proxy := h.MustRun(ctx, nodeOf(t, "video.transcode"), "proxy", map[string]any{"source": "server:" + raw, "profile": "proxy-720p"})

	env := h.Env("loose", "test")
	loosePath, err := env.Scratch().Path("loose.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFile(raw, loosePath); err != nil {
		t.Fatal(err)
	}

	n := nodeOf(t, "audio.extract")
	for name, source := range map[string]string{
		"proxy role":             "storage://proxy/p.mp4",
		"archive role":           "storage://archive/a.mp4",
		"raw server file":        "server:" + raw,
		"raw upload":             "upload:UPL-001",
		"scratch proxy":          proxy.Field("file"),
		"scratch with no origin": env.Scratch().URI("loose.mp4"),
	} {
		if _, err := h.Run(ctx, n, "asr", map[string]any{"source": source}); !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if reads := h.ArchiveReads(); len(reads) != 0 {
		t.Fatalf("archive was read: %+v", reads)
	}
}

func TestExtractWithoutAudioFailsPlainly(t *testing.T) {
	h := workertest.New(t)
	h.PutFile(domain.RoleMaster, "silent.mp4", fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 1, NoAudio: true}))
	_, err := h.Run(ctx, nodeOf(t, "audio.extract"), "asr", map[string]any{"source": "storage://master/silent.mp4"})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("%v", err)
	}
}

func TestExtractPadsToTheMastersTimeline(t *testing.T) {
	h := workertest.New(t)
	h.PutFile(domain.RoleMaster, "short.mp4", fixture.ShortAudio(t, t.TempDir(), 3, 1))
	out := h.MustRun(ctx, nodeOf(t, "audio.extract"), "asr", map[string]any{"source": "storage://master/short.mp4"})
	if math.Abs(out.Number("duration_seconds")-3) > 0.1 {
		t.Fatalf("duration %.3f", out.Number("duration_seconds"))
	}
}

func TestExtractRefusesAVideoProfile(t *testing.T) {
	h := workertest.New(t)
	h.PutFile(domain.RoleMaster, "m.mp4", fixture.Video(t, t.TempDir(), fixture.Spec{Seconds: 1}))
	_, err := h.Run(ctx, nodeOf(t, "audio.extract"), "asr", map[string]any{"source": "storage://master/m.mp4", "profile": "proxy-720p"})
	if !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("%v", err)
	}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}
