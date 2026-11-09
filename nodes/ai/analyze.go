package ainodes

import (
	"context"
	"errors"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/retrieval"
)

type analyzeNode struct{ base }

type analyzeParams struct {
	AssetID    string            `json:"assetId"`
	Transcript domain.Transcript `json:"transcript"`
}

// Execute runs the analysis through an allowed endpoint. With none, or with one that
// fails, the result is available: false with a reason, because readiness (R11) does not
// depend on analysis and an LLM outage must not hold an ingest back (I28, I206).
func (n analyzeNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	daemon, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p analyzeParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	asset, err := readAsset(ctx, daemon, p.AssetID)
	if err != nil {
		return node.Result{}, err
	}
	analysis, usage, err := analyzeWithPolicy(ctx, daemon, asset, p.Transcript)
	if err != nil {
		return node.Result{}, err
	}
	analysis.AssetID = asset.ID
	analysis.Version = asset.CurrentVersion
	result, err := node.JSONResult(analysis)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	result.Usage = usageRecords(ctx, daemon, usage)
	result.Tools = toolsOf(usage)
	return result, nil
}

func analyzeWithPolicy(ctx context.Context, env node.DaemonEnv, asset domain.Asset, transcript domain.Transcript) (domain.Analysis, retrieval.Usage, error) {
	if len(retrieval.SplitSentences(transcript)) == 0 {
		return domain.Analysis{UnavailableReason: retrieval.ReasonNoSpeech}, retrieval.Usage{}, nil
	}
	llm, reason := allowedLLM(ctx, env, asset)
	if llm == nil {
		return domain.Analysis{UnavailableReason: reason}, retrieval.Usage{}, nil
	}
	marks, err := sensitiveMarks(ctx, env.Store(), asset, asset.CurrentVersion, 0)
	if err != nil {
		return domain.Analysis{}, retrieval.Usage{}, classify(err)
	}
	analysis, usage, err := retrieval.Analyze(ctx, llm, transcript, marks)
	switch {
	case err == nil:
		return analysis, usage, nil
	case ctx.Err() != nil:
		return domain.Analysis{}, usage, ctx.Err()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return domain.Analysis{}, usage, err
	default:
		env.Logger().Warn("analysis failed; the asset continues without it", "asset", asset.ID, "endpoint", llm.Name(), "reason", errs.CodeOf(err))
		return domain.Analysis{UnavailableReason: retrieval.ReasonEndpointFailed, Endpoint: string(llm.Class())}, usage, nil
	}
}

type editorialNode struct{ base }

type editorialParams struct {
	AssetID    string             `json:"assetId"`
	Transcript *domain.Transcript `json:"transcript"`
	Prompt     string             `json:"prompt"`
}

// Execute drafts title, description and tags (§3.2 step 12). With no usable endpoint it
// returns the fallback of I28, with generated: false.
func (n editorialNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	daemon, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p editorialParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	asset, err := readAsset(ctx, daemon, p.AssetID)
	if err != nil {
		return node.Result{}, err
	}
	var usage retrieval.Usage
	draft, generated, err := draftWithPolicy(ctx, daemon, asset, p, &usage)
	if err != nil {
		return node.Result{}, err
	}
	if !generated {
		draft = retrieval.FallbackEditorial(asset.Title, sourceTitle(ctx, daemon, asset), p.Prompt, asset.Tags)
	}
	result, err := node.JSONResult(draft)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	result.Usage = usageRecords(ctx, daemon, usage)
	result.Tools = toolsOf(usage)
	return result, nil
}

func draftWithPolicy(ctx context.Context, env node.DaemonEnv, asset domain.Asset, p editorialParams, usage *retrieval.Usage) (retrieval.Editorial, bool, error) {
	llm, _ := allowedLLM(ctx, env, asset)
	if llm == nil {
		return retrieval.Editorial{}, false, nil
	}
	var transcript domain.Transcript
	if p.Transcript != nil {
		transcript = *p.Transcript
	} else {
		stored, err := env.Store().Transcripts().Get(ctx, id.Ref(asset.ID), asset.CurrentVersion)
		if err != nil && !errs.IsCode(err, errs.CodeNotFound) {
			return retrieval.Editorial{}, false, classify(err)
		}
		transcript = stored
	}
	marks, err := sensitiveMarks(ctx, env.Store(), asset, asset.CurrentVersion, 0)
	if err != nil {
		return retrieval.Editorial{}, false, classify(err)
	}
	draft, consumed, err := retrieval.DraftEditorial(ctx, llm, asset.Title, p.Prompt, transcript, marks)
	*usage = consumed
	switch {
	case err == nil:
		return draft, true, nil
	case ctx.Err() != nil:
		return retrieval.Editorial{}, false, ctx.Err()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return retrieval.Editorial{}, false, err
	default:
		env.Logger().Warn("editorial draft failed; using the fallback", "asset", asset.ID, "endpoint", llm.Name(), "reason", errs.CodeOf(err))
		return retrieval.Editorial{}, false, nil
	}
}

// sourceTitle is best effort: a missing parent only makes the fallback shorter.
func sourceTitle(ctx context.Context, env node.DaemonEnv, asset domain.Asset) string {
	if asset.Lineage == nil || asset.Lineage.SourceAssetID == "" {
		return ""
	}
	source, err := env.Store().Assets().Get(ctx, id.Ref(asset.Lineage.SourceAssetID))
	if err != nil {
		return ""
	}
	return source.Title
}
