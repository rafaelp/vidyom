package ainodes

import (
	"context"
	"encoding/json"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/retrieval"
)

// retrievalLimit is how many fused hits feed story detection.
const retrievalLimit = 24

type findSegmentNode struct{ base }

type findSegmentParams struct {
	Asset                 json.RawMessage `json:"asset"`
	Query                 string          `json:"query"`
	Mode                  string          `json:"mode"`
	TargetDurationSeconds float64         `json:"targetDurationSeconds"`
}

// Execute proposes clips with their evidence (§3.2 steps 3-5, D51). The editor's
// approval always follows (D53), and nothing here can skip it.
func (n findSegmentNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	daemon, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p findSegmentParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	ref, err := assetRefOf(p.Asset)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	asset, err := readAsset(ctx, daemon, ref)
	if err != nil {
		return node.Result{}, err
	}
	version := asset.CurrentVersion
	transcript, err := readTranscript(ctx, daemon, asset, version)
	if err != nil {
		return node.Result{}, err
	}
	store := daemon.Store()
	marks, err := sensitiveMarks(ctx, store, asset, version, 0)
	if err != nil {
		return node.Result{}, classify(err)
	}
	var analysis *domain.Analysis
	switch stored, err := store.Transcripts().Analysis(ctx, id.Ref(asset.ID), version); {
	case err == nil:
		analysis = &stored
	case !errs.IsCode(err, errs.CodeNotFound):
		return node.Result{}, classify(err)
	}

	retriever := retrieval.Retriever{Search: store.Search(), Embedder: openEmbedder(ctx, daemon), Logger: env.Logger()}
	hits, err := retriever.Hybrid(ctx, retrieval.HybridRequest{Text: p.Query, AssetID: id.Ref(asset.ID), Limit: retrievalLimit})
	if err != nil {
		return node.Result{}, classify(err)
	}

	llm, reason := allowedLLM(ctx, daemon, asset)
	found, err := retrieval.Find(ctx, retrieval.FindRequest{
		Asset:          asset,
		Version:        version,
		Transcript:     transcript,
		Analysis:       analysis,
		Sensitive:      marks,
		Hits:           hits.Hits,
		Query:          p.Query,
		Mode:           p.Mode,
		TargetDuration: p.TargetDurationSeconds,
		LLM:            llm,
	})
	if err != nil {
		return node.Result{}, classify(err)
	}
	if llm == nil && reason != "" {
		found.Reason = reason
	}
	result, err := node.JSONResult(found)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	result.Usage = usageRecords(ctx, daemon, found.Usage)
	result.Tools = toolsOf(found.Usage)
	return result, nil
}

// openEmbedder returns nil when none can be opened: search then runs lexical (I212).
func openEmbedder(ctx context.Context, env node.DaemonEnv) connector.Embedder {
	embedder, err := env.Embedder(ctx)
	if err != nil {
		env.Logger().Warn("no embedding connector; searching lexically only", "reason", errs.CodeOf(err))
		return nil
	}
	return embedder
}

type deriveNode struct{ base }

type deriveParams struct {
	SourceID string           `json:"sourceId"`
	Segments []domain.Segment `json:"segments"`
}

// Execute maps a source's transcript onto a clip's timeline (§4.3). Sensitive marks
// are carried later, by ai.index, which knows the clip (I207).
func (n deriveNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	daemon, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p deriveParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	asset, err := readAsset(ctx, daemon, p.SourceID)
	if err != nil {
		return node.Result{}, err
	}
	cuts := make([]domain.Segment, len(p.Segments))
	for i, segment := range p.Segments {
		// One source per clip in the MVP (R12).
		if segment.SourceAssetID != "" && segment.SourceAssetID != asset.ID && string(segment.SourceAssetID) != string(asset.ULID) {
			return node.Result{}, invalidParameter("segment %d names another source asset", i)
		}
		cuts[i] = segment
		if cuts[i].SourceVersion == "" {
			cuts[i].SourceVersion = asset.CurrentVersion
		}
	}
	cache := map[string]domain.Transcript{}
	transcript, err := retrieval.DeriveTranscript(cuts, func(version string) (domain.Transcript, error) {
		if cached, ok := cache[version]; ok {
			return cached, nil
		}
		source, err := readTranscript(ctx, daemon, asset, version)
		if err != nil {
			return domain.Transcript{}, err
		}
		cache[version] = source
		return source, nil
	})
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	result, err := node.JSONResult(map[string]any{"transcript": transcript})
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	result.Tools = map[string]string{"asr": transcript.Engine}
	return result, nil
}
