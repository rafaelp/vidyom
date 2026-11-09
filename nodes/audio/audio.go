package audionodes

import (
	"context"
	"math"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
	"github.com/rafaelp/vidyom/internal/workers/nodeutil"
	"github.com/rafaelp/vidyom/internal/workers/provenance"
	"github.com/rafaelp/vidyom/profiles"
)

const defaultProfile = "speech-opus"

// Register adds every audio.* node.
func Register(r node.Registrar) {
	r.RegisterNode(extractNode{nodeutil.NewBase("audio.extract")})
}

type extractNode struct{ nodeutil.Base }

type extractParams struct {
	Source  string `json:"source"`
	Profile string `json:"profile"`
}

type extractOutput struct {
	File            string  `json:"file"`
	SHA256          string  `json:"sha256"`
	SizeBytes       int64   `json:"size_bytes"`
	DurationSeconds float64 `json:"duration_seconds"`
	Profile         string  `json:"profile"`
	FFmpegVersion   string  `json:"ffmpeg_version,omitempty"`
}

// fromMaster enforces D56: the source must be the master role, or a scratch file whose
// recorded origin says it is a master. A raw upload, a proxy or an unknown file is
// refused rather than transcribed on a different timeline.
func fromMaster(uri domain.InputURI, path string) error {
	refusal := errs.New(errs.CodeForbiddenStorageRole, "ASR audio comes only from the working master (D56)")
	switch uri.Kind {
	case domain.URIStorage:
		if uri.Role != domain.RoleMaster {
			return refusal
		}
		return nil
	case domain.URIScratch:
		origin, known, err := provenance.Read(path)
		if err != nil {
			return errs.Wrap(errs.CodeFailedPrecondition, err, "a scratch file's origin record is unreadable")
		}
		if known && (origin.Role == domain.RoleMaster || origin.Family == profiles.FamilyMaster) {
			return nil
		}
		return refusal
	default:
		return refusal
	}
}

// Execute extracts audio with the profile's limits, never above the source's own
// rate or bitrate. A track shorter than the file is padded, and the output's duration
// is checked here (R10, D21).
func (extractNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	var p extractParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	uri, err := domain.ParseInputURI(p.Source)
	if err != nil {
		return node.Result{}, errs.Wrap(errs.CodeInvalidArgument, err, "invalid source reference")
	}
	if err := uri.ForbidRole(domain.RoleArchive, domain.RoleProxy); err != nil {
		return node.Result{}, err
	}
	if uri.Kind != domain.URIStorage && uri.Kind != domain.URIScratch {
		return node.Result{}, errs.New(errs.CodeForbiddenStorageRole, "ASR audio comes only from the working master (D56)")
	}
	input, err := env.OpenInput(ctx, p.Source, domain.RoleArchive, domain.RoleProxy)
	if err != nil {
		return node.Result{}, nodeutil.Classify(err)
	}
	if err := fromMaster(uri, input.Path); err != nil {
		return node.Result{}, err
	}
	name := p.Profile
	if name == "" {
		name = defaultProfile
	}
	profile, err := profiles.Lookup(name)
	if err != nil {
		return node.Result{}, err
	}
	if !profile.AudioOnly() {
		return node.Result{}, errs.Newf(errs.CodeInvalidArgument, "profile %s has video; use video.transcode", profile.Ref())
	}
	t := ffmpeg.Default()
	src, err := t.Probe(ctx, input.Path)
	if err != nil {
		return node.Result{}, nodeutil.ProbeError(err)
	}
	if !src.HasAudio {
		return node.Result{}, errs.New(errs.CodeFailedPrecondition, "this recording has no audio to transcribe")
	}
	file := profile.ID + "." + profile.Extension()
	out, err := env.Scratch().Path(file)
	if err != nil {
		return node.Result{}, err
	}
	temp := nodeutil.TempFor(out)
	args, err := ffmpeg.AudioExtractArgs(src, temp, profile)
	if err != nil {
		return node.Result{}, errs.Wrap(errs.CodeInternal, err, "cannot build the extraction")
	}
	if err := nodeutil.Encode(ctx, env, t, args, src.DurationSeconds, "extracting audio"); err != nil {
		return node.Result{}, err
	}
	info, err := t.Probe(ctx, temp)
	if err != nil {
		return node.Result{}, nodeutil.ProbeError(err)
	}
	tolerance := ffmpeg.Tolerance(src.FPS)
	if math.Abs(info.DurationSeconds-src.DurationSeconds) > tolerance {
		return node.Result{}, errs.Newf(errs.CodeValidationFailed, "the extracted audio left the timeline tolerance of %.3f s", tolerance).
			With("reference_duration", src.DurationSeconds).
			With("duration", info.DurationSeconds)
	}
	sum, size, err := provenance.HashFile(ctx, temp)
	if err != nil {
		return node.Result{}, err
	}
	if err := nodeutil.Publish(temp, out); err != nil {
		return node.Result{}, err
	}
	tools, version := nodeutil.Tools(ctx, t)
	result, err := node.JSONResult(extractOutput{
		File:            env.Scratch().URI(file),
		SHA256:          sum,
		SizeBytes:       size,
		DurationSeconds: info.DurationSeconds,
		Profile:         profile.Ref(),
		FFmpegVersion:   version,
	})
	if err != nil {
		return node.Result{}, err
	}
	result.Tools = tools
	return result, nil
}
