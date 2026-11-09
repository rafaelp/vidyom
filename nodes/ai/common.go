package ainodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/retrieval"
	"github.com/rafaelp/vidyom/schemas"
)

// Register adds every ai.* node of the catalog.
func Register(r node.Registrar) {
	r.RegisterNode(transcribeNode{describe("ai.transcribe")})
	r.RegisterNode(embedNode{describe("ai.embed")})
	r.RegisterNode(exportNode{describe("ai.export_transcript")})
	r.RegisterNode(analyzeNode{describe("ai.analyze")})
	r.RegisterNode(indexNode{describe("ai.index")})
	r.RegisterNode(findSegmentNode{describe("ai.find_segment")})
	r.RegisterNode(deriveNode{describe("ai.derive_transcript")})
	r.RegisterNode(editorialNode{describe("ai.editorial")})
}

type base struct{ descriptor node.Descriptor }

func (b base) Descriptor() node.Descriptor { return b.descriptor }

// describe panics on a type missing from the embedded catalog: that is a build defect,
// and registration happens at process start.
func describe(nodeType string) base {
	d, err := schemas.DescriptorFor(nodeType, 1)
	if err != nil {
		panic(err)
	}
	return base{d}
}

// classify marks an error for the engine's retry logic. Cancellation stays as it is.
func classify(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch errs.CodeOf(err) {
	case errs.CodeUnavailable, errs.CodeTimeout, errs.CodeRateLimited:
		return node.Transient(err)
	default:
		return node.Permanent(err)
	}
}

// readAsset resolves a reference in the job's tenant.
func readAsset(ctx context.Context, env node.DaemonEnv, ref string) (domain.Asset, error) {
	asset, err := env.Store().Assets().Get(ctx, id.Ref(ref))
	if err != nil {
		return domain.Asset{}, classify(err)
	}
	return asset, nil
}

// assetRefOf reads ai.find_segment's `asset`: a reference, or catalog.get's output, of
// which only the reference is used. Everything else comes from the store (I208).
func assetRefOf(raw json.RawMessage) (string, error) {
	var ref string
	if err := json.Unmarshal(raw, &ref); err == nil && ref != "" {
		return ref, nil
	}
	var object struct {
		ID   string `json:"id"`
		ULID string `json:"ulid"`
	}
	if err := json.Unmarshal(raw, &object); err == nil {
		if object.ID != "" {
			return object.ID, nil
		}
		if object.ULID != "" {
			return object.ULID, nil
		}
	}
	return "", errs.New(errs.CodeInvalidArgument, "asset must be a reference or an asset object")
}

// readTranscript returns a version's transcript, or failed_precondition when the asset
// has none yet.
func readTranscript(ctx context.Context, env node.DaemonEnv, asset domain.Asset, version string) (domain.Transcript, error) {
	transcript, err := env.Store().Transcripts().Get(ctx, id.Ref(asset.ID), version)
	if errs.IsCode(err, errs.CodeNotFound) {
		return domain.Transcript{}, node.Permanent(errs.Newf(errs.CodeFailedPrecondition, "asset %s has no transcript for version %s", asset.ID, version))
	}
	if err != nil {
		return domain.Transcript{}, classify(err)
	}
	return transcript, nil
}

// allowedLLM applies D8. The effective policy comes from the stored asset, and the
// endpoint's class is checked again here so D8 does not rest on one implementation of
// env.LLM (I208). A nil result means nothing may be sent, with the reason.
func allowedLLM(ctx context.Context, env node.DaemonEnv, asset domain.Asset) (connector.LLM, string) {
	effective := asset
	effective.AIProcessing = retrieval.EffectivePolicy(asset.AIProcessing, asset.AIConsent != nil)
	llm, err := env.LLM(ctx, effective)
	if err != nil {
		if errs.IsCode(err, errs.CodeFailedPrecondition) {
			return nil, retrieval.ReasonNoEndpoint
		}
		env.Logger().Warn("no LLM endpoint could be opened", "asset", asset.ID, "reason", errs.CodeOf(err))
		return nil, retrieval.ReasonEndpointFailed
	}
	if llm == nil {
		return nil, retrieval.ReasonNoEndpoint
	}
	if !retrieval.PolicyAllows(effective.AIProcessing, llm) {
		env.Logger().Warn("refusing a non-local LLM endpoint for local_only content", "asset", asset.ID, "endpoint", llm.Name())
		return nil, retrieval.ReasonNoEndpoint
	}
	if retrieval.IsExternal(llm) {
		env.Logger().Info("content goes to an external LLM endpoint under a consent record", "asset", asset.ID, "endpoint", llm.Name())
	}
	return llm, ""
}

// maxLineageDepth bounds the walk up a lineage; real chains are short (D36).
const maxLineageDepth = 16

// sensitiveMarks returns what must not reach external AI for an asset version: its own
// marks and, at its current version, what its sources mark right now, so a mark added
// after a clip was cut still counts (D30, I207).
func sensitiveMarks(ctx context.Context, repos repo.TenantRepos, asset domain.Asset, version string, depth int) ([]domain.SensitiveSegment, error) {
	own, err := repos.Transcripts().Sensitive(ctx, id.Ref(asset.ID), version)
	if err != nil {
		return nil, err
	}
	if version != asset.CurrentVersion {
		return own, nil
	}
	inherited, err := inheritedMarks(ctx, repos, asset, depth)
	if err != nil {
		return nil, err
	}
	return append(own, inherited...), nil
}

// inheritedMarks maps the sources' marks onto a derivative's timeline. A source no longer
// in the catalog has nothing to add: the marks it passed on were stored when the
// derivative was indexed.
func inheritedMarks(ctx context.Context, repos repo.TenantRepos, asset domain.Asset, depth int) ([]domain.SensitiveSegment, error) {
	if asset.Lineage == nil || depth >= maxLineageDepth {
		return nil, nil
	}
	var out []domain.SensitiveSegment
	offset := 0.0
	for _, cut := range asset.Lineage.Segments {
		span := cut.EndSeconds - cut.StartSeconds
		if span <= 0 {
			continue
		}
		sourceID := cut.SourceAssetID
		if sourceID == "" {
			sourceID = asset.Lineage.SourceAssetID
		}
		start := offset
		offset += span
		if sourceID == "" {
			continue
		}
		source, err := repos.Assets().Get(ctx, id.Ref(sourceID))
		if errs.IsCode(err, errs.CodeNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		version := cut.SourceVersion
		if version == "" {
			version = source.CurrentVersion
		}
		marks, err := sensitiveMarks(ctx, repos, source, version, depth+1)
		if err != nil {
			return nil, err
		}
		onClip := retrieval.MapToDerived(retrieval.MarksToIntervals(marks), []domain.Segment{{StartSeconds: cut.StartSeconds, EndSeconds: cut.EndSeconds}})
		for _, interval := range onClip {
			out = append(out, domain.SensitiveSegment{
				AssetID:      asset.ID,
				Version:      asset.CurrentVersion,
				StartSeconds: start + interval.Start,
				EndSeconds:   start + interval.End,
				Reason:       "inherited from " + string(sourceID),
				MarkedBy:     "system",
			})
		}
	}
	return out, nil
}

// usageRecords turns LLM consumption into budget entries (I216).
func usageRecords(ctx context.Context, env node.DaemonEnv, usage retrieval.Usage) []domain.UsageRecord {
	if usage.Provider == "" {
		return nil
	}
	operation := "ai.local"
	if usage.Class != config.EndpointLocal {
		operation = "ai.external"
	}
	tokens := float64(usage.InputTokens + usage.OutputTokens)
	record := domain.UsageRecord{
		TenantID:  env.Tenant(),
		Operation: operation,
		Provider:  usage.Provider,
		Quantity:  tokens,
		Unit:      "tokens",
		JobID:     string(env.JobID()),
		NodeID:    env.NodeID(),
		At:        time.Now().UTC(),
	}
	if cfg, err := env.TenantConfig(ctx); err == nil {
		record.EstimatedMinor = int64(tokens * cfg.Budget.PerUnitMinor[operation])
	}
	return []domain.UsageRecord{record}
}

// toolsOf reports the LLM model for provenance (R21).
func toolsOf(usage retrieval.Usage) map[string]string {
	if usage.Model == "" {
		return nil
	}
	return map[string]string{"llm": usage.Model}
}

func invalidParameter(format string, args ...any) error {
	return node.Permanent(errs.New(errs.CodeInvalidArgument, fmt.Sprintf(format, args...)))
}
