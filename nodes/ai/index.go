package ainodes

import (
	"context"
	"math"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/retrieval"
)

type indexNode struct{ base }

type indexParams struct {
	AssetID    string            `json:"assetId"`
	Version    string            `json:"version"`
	Transcript domain.Transcript `json:"transcript"`
	Analysis   *domain.Analysis  `json:"analysis"`
	Embeddings *embedOutput      `json:"embeddings"`
}

type indexOutput struct {
	Indexed          bool   `json:"indexed"`
	LexicalDocuments int    `json:"lexical_documents"`
	Vectors          int    `json:"vectors"`
	EmbeddingModel   string `json:"embedding_model"`
}

// indexedPayload is the events' payload. Events never carry transcript content (§8.2).
type indexedPayload struct {
	Version          string `json:"version"`
	EmbeddingModel   string `json:"embedding_model"`
	LexicalDocuments int    `json:"lexical_documents"`
	Vectors          int    `json:"vectors"`
}

const listPageSize = 200

// Execute stores a transcript and its indexes in one transaction with their events
// (§8.2, D5). Readiness reads these indexes (R11), so asset.indexed and
// original.indexed fire only when both are non-empty, and not again when a retry finds
// the work of a committed attempt.
func (n indexNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	daemon, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p indexParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	asset, err := readAsset(ctx, daemon, p.AssetID)
	if err != nil {
		return node.Result{}, err
	}
	version := p.Version
	if version == "" {
		version = asset.CurrentVersion
	}
	if version == "" {
		return node.Result{}, invalidParameter("the asset has no version to index")
	}
	if err := validateTranscript(p.Transcript); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if err := validateEmbeddings(p.Embeddings); err != nil {
		return node.Result{}, node.Permanent(err)
	}

	lexical, model, vectors := indexContents(p)
	store := daemon.Store()
	alreadyCommitted := false
	if in.Attempt > 1 && model != "" {
		hasLexical, hasVectors, err := store.Search().HasIndex(ctx, id.Ref(asset.ID), version, model)
		if err != nil {
			return node.Result{}, classify(err)
		}
		alreadyCommitted = hasLexical && hasVectors
	}
	out := indexOutput{
		Indexed:          len(lexical) > 0 && vectors > 0,
		LexicalDocuments: len(lexical),
		Vectors:          vectors,
		EmbeddingModel:   model,
	}

	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		transcript := p.Transcript
		transcript.AssetID = asset.ID
		transcript.Version = version
		if transcript.TimestampBasis == "" {
			transcript.TimestampBasis = "working_master"
		}
		if err := tx.Transcripts().Save(ctx, transcript); err != nil {
			return err
		}
		if p.Analysis != nil {
			analysis := *p.Analysis
			analysis.AssetID = asset.ID
			analysis.Version = version
			if err := tx.Transcripts().SaveAnalysis(ctx, analysis); err != nil {
				return err
			}
		}
		if err := tx.Search().IndexLexical(ctx, id.Ref(asset.ID), version, lexical); err != nil {
			return err
		}
		if vectors > 0 {
			if err := tx.Search().IndexVectors(ctx, repo.EmbeddingBatch{
				AssetID:  id.Ref(asset.ID),
				Version:  version,
				Model:    model,
				Chunking: p.Embeddings.Chunking,
				Chunks:   p.Embeddings.Chunks,
			}); err != nil {
				return err
			}
		}
		if err := carrySensitiveMarks(ctx, tx, asset, version); err != nil {
			return err
		}
		if asset.Language == "" && transcript.Language != "" {
			current, err := tx.Assets().Get(ctx, id.Ref(asset.ID))
			if err != nil {
				return err
			}
			current.Language = transcript.Language
			if err := tx.Assets().Update(ctx, current); err != nil {
				return err
			}
		}
		if !out.Indexed || alreadyCommitted {
			return nil
		}
		payload := indexedPayload{Version: version, EmbeddingModel: model, LexicalDocuments: len(lexical), Vectors: vectors}
		if err := daemon.Emit(ctx, tx, event.Draft{Type: event.AssetIndexed, AssetID: string(asset.ID), Payload: payload}); err != nil {
			return err
		}
		if asset.Kind == domain.KindOriginal {
			return daemon.Emit(ctx, tx, event.Draft{Type: event.OriginalIndexed, AssetID: string(asset.ID), Payload: payload})
		}
		return nil
	})
	if err != nil {
		return node.Result{}, classify(err)
	}
	if vectors > 0 {
		if err := swapActiveModelWhenComplete(ctx, daemon, store, model); err != nil {
			return node.Result{}, classify(err)
		}
	}
	if !out.Indexed {
		env.Logger().Warn("nothing to index: the asset has no searchable speech", "asset", asset.ID, "version", version)
	}
	result, err := node.JSONResult(out)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	return result, nil
}

// indexContents uses the chunks ai.embed made, so a lexical hit and a vector hit of the
// same window fuse; without embeddings it chunks here.
func indexContents(p indexParams) ([]domain.SearchChunk, string, int) {
	if p.Embeddings == nil || len(p.Embeddings.Chunks) == 0 {
		return retrieval.ChunkTranscript(p.Transcript, p.Analysis), "", 0
	}
	lexical := make([]domain.SearchChunk, len(p.Embeddings.Chunks))
	for i, chunk := range p.Embeddings.Chunks {
		lexical[i] = chunk.SearchChunk
	}
	return lexical, p.Embeddings.Model, len(p.Embeddings.Chunks)
}

func validateTranscript(t domain.Transcript) error {
	for i, segment := range t.Segments {
		if !finite(segment.Start) || !finite(segment.End) || segment.Start < 0 || segment.End < segment.Start {
			return errs.Newf(errs.CodeInvalidArgument, "transcript segment %d has invalid timestamps", i)
		}
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validateEmbeddings(e *embedOutput) error {
	if e == nil || len(e.Chunks) == 0 {
		return nil
	}
	if e.Model == "" {
		return errs.New(errs.CodeInvalidArgument, "vectors need the model that produced them (R18)")
	}
	if e.Chunking == "" {
		return errs.New(errs.CodeInvalidArgument, "vectors need their segmentation strategy (R18)")
	}
	for i, chunk := range e.Chunks {
		if len(chunk.Vector) == 0 || (e.Dimensions != 0 && len(chunk.Vector) != e.Dimensions) {
			return errs.Newf(errs.CodeInvalidArgument, "chunk %d has %d dimensions, expected %d", i, len(chunk.Vector), e.Dimensions)
		}
		if chunk.EndSeconds < chunk.StartSeconds {
			return errs.Newf(errs.CodeInvalidArgument, "chunk %d has invalid timestamps", i)
		}
	}
	return nil
}

// carrySensitiveMarks stores a derivative's inherited marks as its own, because a
// clip's marks are what later readers see first: without this, cutting a sensitive
// interval into a clip would launder it (D30, I207).
func carrySensitiveMarks(ctx context.Context, tx repo.TenantRepos, asset domain.Asset, version string) error {
	if version != asset.CurrentVersion {
		return nil // an older version may have been cut from other segments
	}
	marks, err := inheritedMarks(ctx, tx, asset, 0)
	if err != nil {
		return err
	}
	for _, mark := range marks {
		if err := tx.Transcripts().MarkSensitive(ctx, mark); err != nil {
			return err
		}
	}
	return nil
}

// swapActiveModelWhenComplete finishes side-by-side reindexing (D15): once every asset
// that has vectors of the active model at its current version also has the new model's,
// searches move over and the old vectors go (I213).
func swapActiveModelWhenComplete(ctx context.Context, env node.DaemonEnv, store repo.TenantStore, model string) error {
	active, err := store.Search().ActiveModel(ctx)
	if err != nil {
		return err
	}
	if active == "" || active == model {
		return nil
	}
	cursor := ""
	for {
		assets, next, err := store.Assets().List(ctx, repo.AssetFilter{}, repo.Page{Limit: listPageSize, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, asset := range assets {
			_, hasActive, err := store.Search().HasIndex(ctx, id.Ref(asset.ID), asset.CurrentVersion, active)
			if err != nil {
				return err
			}
			if !hasActive {
				continue
			}
			_, hasNew, err := store.Search().HasIndex(ctx, id.Ref(asset.ID), asset.CurrentVersion, model)
			if err != nil {
				return err
			}
			if !hasNew {
				return nil
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if err := store.Search().SetActiveModel(ctx, model); err != nil {
		return err
	}
	env.Logger().Info("reindexing finished; searches moved to the new embedding model", "model", model, "previous", active)
	return store.Search().DeleteModel(ctx, active)
}
