package videonodes

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"path/filepath"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
	"github.com/rafaelp/vidyom/internal/workers/nodeutil"
	"github.com/rafaelp/vidyom/internal/workers/provenance"
	"github.com/rafaelp/vidyom/profiles"
)

// Register adds every video.* node.
func Register(r node.Registrar) {
	r.RegisterNode(probeNode{nodeutil.NewBase("video.probe")})
	r.RegisterNode(transcodeNode{nodeutil.NewBase("video.transcode")})
	r.RegisterNode(validateNode{nodeutil.NewBase("video.validate_timeline")})
	r.RegisterNode(previewNode{nodeutil.NewBase("video.preview")})
	r.RegisterNode(cutNode{nodeutil.NewBase("video.cut")})
	r.RegisterNode(thumbnailNode{nodeutil.NewBase("video.thumbnail")})
}

func tool() *ffmpeg.Tool { return ffmpeg.Default() }

// openOperational opens a source that must not come from archive. The one exception
// is a scratch file read from archive by a job holding storage.archive.restore, which
// is how restore-archived re-encodes: storage.get already enforced it (D42).
func openOperational(ctx context.Context, env node.Env, source string) (node.LocalInput, error) {
	uri, err := domain.ParseInputURI(source)
	if err != nil {
		return node.LocalInput{}, errs.Wrap(errs.CodeInvalidArgument, err, "invalid source reference")
	}
	if uri.Kind == domain.URIScratch && env.HasPermission(domain.PermStorageArchiveRestore) {
		return env.OpenInput(ctx, source)
	}
	return env.OpenInput(ctx, source, domain.RoleArchive)
}

// keyOf returns a reference's own file name, when it has one.
func nameOf(source string) string {
	uri, err := domain.ParseInputURI(source)
	if err != nil {
		return ""
	}
	switch uri.Kind {
	case domain.URIStorage:
		return path.Base(uri.Key)
	case domain.URIServer:
		return filepath.Base(uri.Path)
	case domain.URIScratch:
		return path.Base(uri.File)
	}
	return ""
}

// ---- video.probe ----

type probeNode struct{ nodeutil.Base }

type probeParams struct {
	Source string `json:"source"`
}

type probeOutput struct {
	DurationSeconds  float64 `json:"duration_seconds"`
	Width            int     `json:"width"`
	Height           int     `json:"height"`
	FPS              float64 `json:"fps"`
	VFR              bool    `json:"vfr"`
	BrokenTimestamps bool    `json:"broken_timestamps"`
	VideoCodec       string  `json:"video_codec"`
	AudioCodec       string  `json:"audio_codec"`
	Container        string  `json:"container"`
	Ext              string  `json:"ext"`
}

// Execute inspects a file: display dimensions, frame rate, and whether its timestamps
// need normalizing (D21). A spooled upload has no extension, so ext falls back to the
// container, keeping original.<ext> truthful (R26).
func (probeNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p probeParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	input, err := openOperational(ctx, env, p.Source)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	info, err := tool().Probe(ctx, input.Path)
	if err != nil {
		return node.Result{}, nodeutil.ProbeError(err)
	}
	ext := info.Ext
	if name := nameOf(p.Source); name != "" {
		if own := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")); own != "" {
			ext = ffmpeg.ExtensionFor(name, info.Container, "", info.VideoCodec)
		}
	}
	result, err := node.JSONResult(probeOutput{
		DurationSeconds:  info.DurationSeconds,
		Width:            info.Width,
		Height:           info.Height,
		FPS:              info.FPS,
		VFR:              info.VFR,
		BrokenTimestamps: info.Broken,
		VideoCodec:       info.VideoCodec,
		AudioCodec:       info.AudioCodec,
		Container:        info.Container,
		Ext:              ext,
	})
	if err != nil {
		return node.Result{}, err
	}
	result.Tools, _ = nodeutil.Tools(ctx, tool())
	return result, nil
}

// ---- video.transcode ----

type transcodeNode struct{ nodeutil.Base }

type transcodeParams struct {
	Source  string `json:"source"`
	Profile string `json:"profile"`
}

type transcodeOutput struct {
	File            string  `json:"file"`
	SHA256          string  `json:"sha256"`
	SizeBytes       int64   `json:"size_bytes"`
	DurationSeconds float64 `json:"duration_seconds"`
	Profile         string  `json:"profile"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	FFmpegVersion   string  `json:"ffmpeg_version,omitempty"`
}

// Execute encodes to a profile, or to auto:master by the source's height (I25, I184).
// The output is constant frame rate, starts at the source's zero instant, and is
// rejected here when its duration leaves the tolerance (R10, D21).
func (transcodeNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p transcodeParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	input, err := openOperational(ctx, env, p.Source)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	t := tool()
	src, err := t.Probe(ctx, input.Path)
	if err != nil {
		return node.Result{}, nodeutil.ProbeError(err)
	}
	if !src.HasVideo {
		return node.Result{}, errs.New(errs.CodeFailedPrecondition, "this file has no video stream")
	}
	profile, err := profiles.Resolve(p.Profile, src.EffectiveHeight())
	if err != nil {
		return node.Result{}, err
	}
	if profile.AudioOnly() {
		return node.Result{}, errs.Newf(errs.CodeInvalidArgument, "profile %s is audio-only; use audio.extract", profile.Ref())
	}
	name := profile.ID + "." + profile.Extension()
	out, err := env.Scratch().Path(name)
	if err != nil {
		return node.Result{}, err
	}
	temp := nodeutil.TempFor(out)
	args, err := ffmpeg.TranscodeArgs(src, temp, profile)
	if err != nil {
		return node.Result{}, errs.Wrap(errs.CodeInternal, err, "cannot build the encode")
	}
	env.Progress(0, "transcoding to "+profile.Ref())
	if err := nodeutil.Encode(ctx, env, t, args, src.DurationSeconds, "transcoding to "+profile.Ref()); err != nil {
		return node.Result{}, err
	}
	encoded, err := t.Probe(ctx, temp)
	if err != nil {
		return node.Result{}, nodeutil.ProbeError(err)
	}
	tolerance := ffmpeg.Tolerance(src.FPS)
	if diff := math.Abs(encoded.DurationSeconds - src.DurationSeconds); diff > tolerance {
		return node.Result{}, timelineError(src.DurationSeconds, tolerance, map[string]float64{name: encoded.DurationSeconds})
	}
	sum, size, err := provenance.HashFile(ctx, temp)
	if err != nil {
		return node.Result{}, err
	}
	if err := nodeutil.Publish(temp, out); err != nil {
		return node.Result{}, err
	}
	if err := provenance.Write(out, provenance.Origin{SHA256: sum, Profile: profile.Ref(), Family: profile.Family()}); err != nil {
		return node.Result{}, errs.Wrap(errs.CodeUnavailable, err, "cannot record a file's origin")
	}
	tools, version := nodeutil.Tools(ctx, t)
	result, err := node.JSONResult(transcodeOutput{
		File:            env.Scratch().URI(name),
		SHA256:          sum,
		SizeBytes:       size,
		DurationSeconds: encoded.DurationSeconds,
		Profile:         profile.Ref(),
		Width:           encoded.Width,
		Height:          encoded.Height,
		FFmpegVersion:   version,
	})
	if err != nil {
		return node.Result{}, err
	}
	result.Tools = tools
	return result, nil
}

func timelineError(reference, tolerance float64, durations map[string]float64) error {
	return errs.New(errs.CodeValidationFailed, fmt.Sprintf("a file left the timeline tolerance of %.3f s", tolerance)).
		With("reference_duration", reference).
		With("tolerance_seconds", tolerance).
		With("durations", durations)
}

// ---- video.validate_timeline ----

type validateNode struct{ nodeutil.Base }

type validateParams struct {
	Reference  string   `json:"reference"`
	Candidates []string `json:"candidates"`
}

type validateOutput struct {
	Valid             bool               `json:"valid"`
	ToleranceSeconds  float64            `json:"tolerance_seconds"`
	ReferenceDuration float64            `json:"reference_duration"`
	Durations         map[string]float64 `json:"durations"`
}

// Execute rejects any candidate further from the reference than max(100 ms, 1 frame)
// (R10, D21). Rejection is a permanent failure, so an asset with a bad timeline can
// never reach ready_for_clipping.
func (validateNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p validateParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	if len(p.Candidates) == 0 {
		return node.Result{}, errs.New(errs.CodeInvalidArgument, "no candidates to validate")
	}
	t := tool()
	input, err := openOperational(ctx, env, p.Reference)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	reference, err := t.Probe(ctx, input.Path)
	if err != nil {
		return node.Result{}, nodeutil.ProbeError(err)
	}
	tolerance := ffmpeg.Tolerance(reference.FPS)
	durations := make(map[string]float64, len(p.Candidates))
	valid := true
	for _, candidate := range p.Candidates {
		opened, err := openOperational(ctx, env, candidate)
		if err != nil {
			return node.Result{}, nodeutil.Classify(err)
		}
		info, err := t.Probe(ctx, opened.Path)
		if err != nil {
			return node.Result{}, nodeutil.ProbeError(err)
		}
		durations[candidate] = info.DurationSeconds
		if math.Abs(info.DurationSeconds-reference.DurationSeconds) > tolerance {
			valid = false
		}
	}
	if !valid {
		return node.Result{}, timelineError(reference.DurationSeconds, tolerance, durations)
	}
	return node.JSONResult(validateOutput{
		Valid:             true,
		ToleranceSeconds:  tolerance,
		ReferenceDuration: reference.DurationSeconds,
		Durations:         durations,
	})
}

// ---- segments ----

const maxSegments = 64

// segment is the catalog's segment. Its identity fields pass through untouched.
type segment struct {
	SourceAssetID string  `json:"source_asset_id,omitempty"`
	SourceVersion string  `json:"source_version,omitempty"`
	StartSeconds  float64 `json:"start_seconds"`
	EndSeconds    float64 `json:"end_seconds"`
}

// checkSegments validates intervals against the source. Clamping within the tolerance
// is rounding, while anything further out changes what was approved, so it fails.
func checkSegments(segments []segment, duration, tolerance float64) ([]ffmpeg.Segment, error) {
	if len(segments) == 0 {
		return nil, errs.New(errs.CodeInvalidArgument, "no segments")
	}
	if len(segments) > maxSegments {
		return nil, errs.Newf(errs.CodeInvalidArgument, "at most %d segments can be rendered together", maxSegments)
	}
	out := make([]ffmpeg.Segment, 0, len(segments))
	for i, s := range segments {
		switch {
		case s.StartSeconds < 0 || s.EndSeconds <= s.StartSeconds:
			return nil, errs.Newf(errs.CodeInvalidArgument, "segment %d has an invalid interval: %.3f to %.3f", i+1, s.StartSeconds, s.EndSeconds)
		case s.StartSeconds >= duration:
			return nil, errs.Newf(errs.CodeInvalidArgument, "segment %d starts at %.3f, past the source's %.3f s", i+1, s.StartSeconds, duration)
		case s.EndSeconds > duration+tolerance:
			return nil, errs.Newf(errs.CodeInvalidArgument, "segment %d ends at %.3f, past the source's %.3f s", i+1, s.EndSeconds, duration)
		}
		out = append(out, ffmpeg.Segment{Start: s.StartSeconds, End: math.Min(s.EndSeconds, duration)})
	}
	return out, nil
}

func totalLength(segments []ffmpeg.Segment) float64 {
	var total float64
	for _, s := range segments {
		total += s.End - s.Start
	}
	return total
}

// renderOutput holds what a segment render produced.
type renderOutput struct {
	sum      string
	size     int64
	duration float64
}

// renderSegments encodes to temp, checks the duration and publishes to the scratch name.
func renderSegments(ctx context.Context, env node.Env, src ffmpeg.Info, segments []ffmpeg.Segment, name string, enc ffmpeg.SegmentEncode, what string) (renderOutput, string, error) {
	t := tool()
	out, err := env.Scratch().Path(name)
	if err != nil {
		return renderOutput{}, "", err
	}
	temp := nodeutil.TempFor(out)
	args, err := ffmpeg.SegmentArgs(src, segments, temp, enc)
	if err != nil {
		return renderOutput{}, "", errs.Wrap(errs.CodeInternal, err, "cannot build the render")
	}
	length := totalLength(segments)
	if err := nodeutil.Encode(ctx, env, t, args, length, what); err != nil {
		return renderOutput{}, "", err
	}
	info, err := t.Probe(ctx, temp)
	if err != nil {
		return renderOutput{}, "", nodeutil.ProbeError(err)
	}
	tolerance := math.Max(0.1, float64(len(segments))/math.Max(src.FPS, 1))
	if math.Abs(info.DurationSeconds-length) > tolerance {
		return renderOutput{}, "", timelineError(length, tolerance, map[string]float64{name: info.DurationSeconds})
	}
	sum, size, err := provenance.HashFile(ctx, temp)
	if err != nil {
		return renderOutput{}, "", err
	}
	if err := nodeutil.Publish(temp, out); err != nil {
		return renderOutput{}, "", err
	}
	return renderOutput{sum: sum, size: size, duration: info.DurationSeconds}, out, nil
}

// ---- video.preview ----

type previewNode struct{ nodeutil.Base }

type previewParams struct {
	Source   string    `json:"source"`
	Segments []segment `json:"segments"`
}

type artifactOutput struct {
	File            string  `json:"file"`
	SHA256          string  `json:"sha256"`
	SizeBytes       int64   `json:"size_bytes"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
}

// Preview settings: disposable, so fixed rather than a versioned profile (I192).
var (
	previewVideo = profiles.Video{Codec: "libx264", Preset: "veryfast", CRF: 30, MaxHeight: 480, PixelFormat: "yuv420p"}
	previewAudio = profiles.Audio{Codec: "aac", Bitrate: "64k"}
)

// Execute renders the proposed segments for review, from the proxy (§3.2 step 6). A
// preview never pulls the master from storage, and never touches archive.
func (previewNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p previewParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	input, err := env.OpenInput(ctx, p.Source, domain.RoleArchive, domain.RoleMaster)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	t := tool()
	src, err := t.Probe(ctx, input.Path)
	if err != nil {
		return node.Result{}, nodeutil.ProbeError(err)
	}
	segments, err := checkSegments(p.Segments, src.DurationSeconds, ffmpeg.Tolerance(src.FPS))
	if err != nil {
		return node.Result{}, err
	}
	render, _, err := renderSegments(ctx, env, src, segments, "preview.mp4", ffmpeg.SegmentEncode{
		Video: previewVideo, Audio: previewAudio, Container: "mp4", Faststart: true, MaxShort: previewVideo.MaxHeight,
	}, "rendering the preview")
	if err != nil {
		return node.Result{}, err
	}
	result, err := node.JSONResult(artifactOutput{
		File: env.Scratch().URI("preview.mp4"), SHA256: render.sum, SizeBytes: render.size, DurationSeconds: render.duration,
	})
	if err != nil {
		return node.Result{}, err
	}
	result.Tools, _ = nodeutil.Tools(ctx, t)
	return result, nil
}

// ---- video.cut ----

type cutNode struct{ nodeutil.Base }

type cutParams struct {
	Source             string               `json:"source"`
	Segments           []segment            `json:"segments"`
	Profile            string               `json:"profile"`
	ForbidStorageRoles []domain.StorageRole `json:"forbidStorageRoles"`
}

type cutOutput struct {
	File            string    `json:"file"`
	SHA256          string    `json:"sha256"`
	SizeBytes       int64     `json:"size_bytes"`
	DurationSeconds float64   `json:"duration_seconds"`
	Segments        []segment `json:"segments"`
	Profile         string    `json:"profile"`
	FFmpegVersion   string    `json:"ffmpeg_version,omitempty"`
}

// Execute renders a clip master. The source must be the master role or a verified
// local copy of it: the checks run on the reference, on the environment's answer and
// on the checksum, and there is no fallback to archive (§3.2, §6.4). The master
// instance must also declare direct access (§7.2).
func (cutNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p cutParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	uri, err := domain.ParseInputURI(p.Source)
	if err != nil {
		return node.Result{}, errs.Wrap(errs.CodeInvalidArgument, err, "invalid source reference")
	}
	if err := uri.RequireRole(domain.RoleMaster, true); err != nil {
		return node.Result{}, err
	}
	master, err := env.Storage(ctx, domain.RoleMaster)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	if !master.Capabilities().DirectAccess {
		return node.Result{}, errs.New(errs.CodeCapabilityUnsupported,
			"the master role's instance does not declare direct access: set usage to operational in storages.json (§7.2)")
	}
	forbid := append([]domain.StorageRole{domain.RoleArchive, domain.RoleProxy}, p.ForbidStorageRoles...)
	input, err := env.OpenInput(ctx, p.Source, forbid...)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	if input.Role != domain.RoleMaster || input.SHA256 == "" {
		return node.Result{}, errs.New(errs.CodeForbiddenStorageRole, "a clip renders only from the master role or a verified copy of it")
	}
	t := tool()
	src, err := t.Probe(ctx, input.Path)
	if err != nil {
		return node.Result{}, nodeutil.ProbeError(err)
	}
	if !src.HasVideo {
		return node.Result{}, errs.New(errs.CodeFailedPrecondition, "this file has no video stream")
	}
	spec := p.Profile
	if spec == "" {
		spec = "auto:master"
	}
	profile, err := profiles.Resolve(spec, src.EffectiveHeight())
	if err != nil {
		return node.Result{}, err
	}
	if profile.AudioOnly() {
		return node.Result{}, errs.Newf(errs.CodeInvalidArgument, "profile %s is audio-only", profile.Ref())
	}
	segments, err := checkSegments(p.Segments, src.DurationSeconds, ffmpeg.Tolerance(src.FPS))
	if err != nil {
		return node.Result{}, err
	}
	name := "clip." + profile.Extension()
	render, out, err := renderSegments(ctx, env, src, segments, name, ffmpeg.SegmentEncode{
		Video: *profile.Video, Audio: profile.Audio, Container: profile.Container, Faststart: profile.Faststart, MaxShort: profile.Video.MaxHeight,
	}, "rendering the clip")
	if err != nil {
		return node.Result{}, err
	}
	if err := provenance.Write(out, provenance.Origin{SHA256: render.sum, Profile: profile.Ref(), Family: profiles.FamilyMaster}); err != nil {
		return node.Result{}, errs.Wrap(errs.CodeUnavailable, err, "cannot record a file's origin")
	}
	tools, version := nodeutil.Tools(ctx, t)
	result, err := node.JSONResult(cutOutput{
		File:            env.Scratch().URI(name),
		SHA256:          render.sum,
		SizeBytes:       render.size,
		DurationSeconds: render.duration,
		Segments:        p.Segments,
		Profile:         profile.Ref(),
		FFmpegVersion:   version,
	})
	if err != nil {
		return node.Result{}, err
	}
	result.Tools = tools
	return result, nil
}

// ---- video.thumbnail ----

type thumbnailNode struct{ nodeutil.Base }

type thumbnailParams struct {
	Source    string   `json:"source"`
	AtSeconds *float64 `json:"atSeconds"`
}

type thumbnailOutput struct {
	File      string  `json:"file"`
	SHA256    string  `json:"sha256"`
	SizeBytes int64   `json:"size_bytes"`
	AtSeconds float64 `json:"at_seconds"`
}

// Execute extracts one JPEG. A time past the end is clamped, which is harmless for a
// short clip (I192), and the output records the time used.
func (thumbnailNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p thumbnailParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	input, err := openOperational(ctx, env, p.Source)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	t := tool()
	src, err := t.Probe(ctx, input.Path)
	if err != nil {
		return node.Result{}, nodeutil.ProbeError(err)
	}
	if !src.HasVideo {
		return node.Result{}, errs.New(errs.CodeFailedPrecondition, "this file has no video stream")
	}
	at := src.DurationSeconds / 10
	if p.AtSeconds != nil {
		at = *p.AtSeconds
	}
	frame := 1 / math.Max(src.FPS, 1)
	at = math.Max(0, math.Min(at, math.Max(0, src.DurationSeconds-2*frame)))
	out, err := env.Scratch().Path("thumbnail.jpg")
	if err != nil {
		return node.Result{}, err
	}
	temp := nodeutil.TempFor(out)
	args, err := ffmpeg.ThumbnailArgs(src, at, temp)
	if err != nil {
		return node.Result{}, errs.Wrap(errs.CodeInternal, err, "cannot build the thumbnail")
	}
	if err := nodeutil.FFmpegError(t.Run(ctx, args, ffmpeg.RunOptions{}), "extracting the thumbnail"); err != nil {
		return node.Result{}, err
	}
	sum, size, err := provenance.HashFile(ctx, temp)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return node.Result{}, err
		}
		return node.Result{}, errs.Wrap(errs.CodeFailedPrecondition, err, "the thumbnail was not written")
	}
	if size == 0 {
		return node.Result{}, errs.New(errs.CodeFailedPrecondition, "the thumbnail is empty")
	}
	if err := nodeutil.Publish(temp, out); err != nil {
		return node.Result{}, err
	}
	result, err := node.JSONResult(thumbnailOutput{File: env.Scratch().URI("thumbnail.jpg"), SHA256: sum, SizeBytes: size, AtSeconds: at})
	if err != nil {
		return node.Result{}, err
	}
	result.Tools, _ = nodeutil.Tools(ctx, t)
	return result, nil
}
