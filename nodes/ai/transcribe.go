package ainodes

import (
	"context"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/retrieval"
)

type transcribeNode struct{ base }

type transcribeParams struct {
	Source         string              `json:"source"`
	AssetID        string              `json:"assetId"`
	AIProcessing   domain.AIProcessing `json:"aiProcessing"`
	Language       string              `json:"language"`
	WordTimestamps *bool               `json:"wordTimestamps"`
}

type transcribeOutput struct {
	Transcript      domain.Transcript `json:"transcript"`
	Language        string            `json:"language"`
	Engine          string            `json:"engine"`
	DurationSeconds float64           `json:"duration_seconds"`
}

// Execute transcribes audio that must come from the working master: `archive` is never
// read for this and neither is `proxy`, so the transcript keeps the master's timeline
// (D21, D56). Scratch files from audio.extract, the normal input, have no role.
func (n transcribeNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	ai, err := node.RequireAI(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p transcribeParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	transcriber, err := ai.Transcriber(ctx)
	if err != nil {
		return node.Result{}, classify(err)
	}
	// This pool has no store, so the policy comes as a parameter and is read fail
	// closed: only an explicit external_allowed lets an external engine run (D8, I208).
	if transcriber.Info().Capabilities["external"] && p.AIProcessing != domain.AIExternalAllowed {
		return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition,
			"this transcription engine is external and the asset is local_only (D8)"))
	}
	input, err := env.OpenInput(ctx, p.Source, domain.RoleArchive, domain.RoleProxy)
	if err != nil {
		return node.Result{}, classify(err)
	}
	wordTimestamps := p.WordTimestamps == nil || *p.WordTimestamps
	transcript, err := transcriber.Transcribe(ctx, connector.TranscribeRequest{
		AudioPath:      input.Path,
		Language:       p.Language,
		WordTimestamps: wordTimestamps,
		Progress:       env.Progress,
	})
	if err != nil {
		return node.Result{}, classify(err)
	}
	if id.IsFriendly(p.AssetID) {
		transcript.AssetID = id.Friendly(p.AssetID)
	}
	result, err := node.JSONResult(transcribeOutput{
		Transcript:      transcript,
		Language:        transcript.Language,
		Engine:          transcript.Engine,
		DurationSeconds: transcript.DurationSeconds,
	})
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	result.Tools = map[string]string{"asr": transcript.Engine}
	result.Usage = []domain.UsageRecord{{
		TenantID:  env.Tenant(),
		Operation: "ai.transcription",
		Provider:  transcript.Engine,
		Quantity:  transcript.DurationSeconds,
		Unit:      "seconds",
		JobID:     string(env.JobID()),
		NodeID:    env.NodeID(),
		At:        time.Now().UTC(),
	}}
	return result, nil
}

type embedNode struct{ base }

type embedParams struct {
	Transcript domain.Transcript `json:"transcript"`
	Analysis   *domain.Analysis  `json:"analysis"`
	AssetID    string            `json:"assetId"`
}

// embedOutput is also ai.index's `embeddings` parameter.
type embedOutput struct {
	Model      string                 `json:"model"`
	Dimensions int                    `json:"dimensions"`
	Chunking   string                 `json:"chunking"`
	Chunks     []domain.EmbeddedChunk `json:"chunks"`
}

// Execute chunks a transcript per D15 and embeds it. No asset policy reaches this pool,
// so an external embedder is refused outright (D8, I215).
func (n embedNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	ai, err := node.RequireAI(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p embedParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	chunks := retrieval.ChunkTranscript(p.Transcript, p.Analysis)
	out := embedOutput{Chunking: retrieval.ChunkingStrategy, Chunks: []domain.EmbeddedChunk{}}
	if len(chunks) > 0 {
		embedder, err := ai.Embedder(ctx)
		if err != nil {
			return node.Result{}, classify(err)
		}
		if embedder.Info().Capabilities["external"] {
			return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition,
				"the embedding connector is external; embeddings of an asset stay local (D8)"))
		}
		texts := make([]string, len(chunks))
		for i, chunk := range chunks {
			texts[i] = chunk.Text
		}
		env.Progress(0, "embedding")
		embedded, err := embedder.Embed(ctx, connector.EmbedRequest{Texts: texts, Kind: connector.EmbedPassage})
		if err != nil {
			return node.Result{}, classify(err)
		}
		if len(embedded.Vectors) != len(chunks) {
			return node.Result{}, node.Permanent(errs.Newf(errs.CodeInternal, "%d vectors for %d chunks", len(embedded.Vectors), len(chunks)))
		}
		out.Model, out.Dimensions = embedded.Model, embedded.Dimensions
		out.Chunks = make([]domain.EmbeddedChunk, len(chunks))
		for i, chunk := range chunks {
			out.Chunks[i] = domain.EmbeddedChunk{SearchChunk: chunk, Vector: embedded.Vectors[i]}
		}
	}
	result, err := node.JSONResult(out)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if out.Model != "" {
		result.Tools = map[string]string{"embedding": out.Model, "chunking": out.Chunking}
		result.Usage = []domain.UsageRecord{{
			TenantID:  env.Tenant(),
			Operation: "ai.embedding",
			Provider:  out.Model,
			Quantity:  float64(len(out.Chunks)),
			Unit:      "chunks",
			JobID:     string(env.JobID()),
			NodeID:    env.NodeID(),
			At:        time.Now().UTC(),
		}}
	}
	return result, nil
}
