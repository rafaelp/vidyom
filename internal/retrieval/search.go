package retrieval

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

// Search modes of GET /search.
const (
	ModeHybrid   = "hybrid"
	ModeLexical  = "lexical"
	ModeSemantic = "semantic"
)

const (
	// FusionK is the reciprocal rank fusion constant (I43).
	FusionK = 60

	defaultLimit  = 20
	maximumLimit  = 100
	listsPerLimit = 3
	maxQueryWords = 32
)

// Fuse combines ranked lists with reciprocal rank fusion. A chunk found in several
// lists sums its reciprocal ranks and reports Source "hybrid". Only ranks count, so
// lexical and vector scores need no common scale.
func Fuse(lists ...[]domain.SearchHit) []domain.SearchHit {
	type entry struct {
		hit   domain.SearchHit
		score float64
		lists int
	}
	byKey := map[string]*entry{}
	for _, list := range lists {
		seen := map[string]bool{}
		rank := 0
		for _, hit := range list {
			key := fusionKey(hit)
			if seen[key] {
				continue
			}
			seen[key] = true
			rank++
			current, ok := byKey[key]
			if !ok {
				current = &entry{hit: hit}
				byKey[key] = current
			}
			current.score += 1 / float64(FusionK+rank)
			current.lists++
			if current.hit.Text == "" {
				current.hit.Text = hit.Text
			}
		}
	}
	out := make([]domain.SearchHit, 0, len(byKey))
	for _, current := range byKey {
		hit := current.hit
		hit.Score = current.score
		if current.lists > 1 {
			hit.Source = "hybrid"
		}
		out = append(out, hit)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		if out[a].AssetID != out[b].AssetID {
			return out[a].AssetID < out[b].AssetID
		}
		if out[a].StartSeconds != out[b].StartSeconds {
			return out[a].StartSeconds < out[b].StartSeconds
		}
		return out[a].Level < out[b].Level
	})
	return out
}

// NormalizedScore maps a fused score of two lists to 0-1: rank 1 in both gives 1.
func NormalizedScore(score float64) float64 {
	return min(1, score*float64(FusionK+1)/2)
}

func fusionKey(hit domain.SearchHit) string {
	return string(hit.AssetID) + "\x00" + hit.Version + "\x00" + string(hit.Level) + "\x00" +
		strconv.FormatInt(int64(math.Round(hit.StartSeconds*1000)), 10) + "\x00" +
		strconv.FormatInt(int64(math.Round(hit.EndSeconds*1000)), 10)
}

// HybridRequest is one retrieval.
type HybridRequest struct {
	Text    string
	AssetID id.Ref // empty searches the tenant
	Mode    string
	Limit   int
}

// HybridResult is what retrieval found and how.
type HybridResult struct {
	Hits  []domain.SearchHit
	Mode  string // the mode that really ran
	Model string // the embedding model used, when any
}

// Retriever runs hybrid search over one tenant's repositories.
type Retriever struct {
	Search   repo.SearchRepo
	Embedder connector.Embedder // nil means no semantic search is possible
	Logger   *slog.Logger
}

// Hybrid runs the request. Hybrid mode falls back to lexical when semantic search cannot
// run, and says so in Mode; semantic mode fails instead.
func (r Retriever) Hybrid(ctx context.Context, req HybridRequest) (HybridResult, error) {
	mode := req.Mode
	if mode == "" {
		mode = ModeHybrid
	}
	if mode != ModeHybrid && mode != ModeLexical && mode != ModeSemantic {
		return HybridResult{}, errs.Newf(errs.CodeInvalidArgument, "unknown search mode %q", mode)
	}
	text := limitWords(req.Text, maxQueryWords)
	if text == "" {
		return HybridResult{}, errs.New(errs.CodeInvalidArgument, "the search text is empty")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maximumLimit)
	fetch := limit * listsPerLimit

	var vector []domain.SearchHit
	model := ""
	if mode != ModeLexical {
		hits, usedModel, err := r.semantic(ctx, text, req.AssetID, fetch)
		switch {
		case err == nil:
			vector, model = hits, usedModel
		case ctx.Err() != nil:
			return HybridResult{}, ctx.Err()
		case mode == ModeSemantic:
			return HybridResult{}, err
		default:
			if r.Logger != nil {
				r.Logger.Warn("semantic search unavailable; using lexical search only", "reason", errs.CodeOf(err))
			}
			mode = ModeLexical
		}
	}

	var lexical []domain.SearchHit
	if mode != ModeSemantic {
		hits, err := r.Search.SearchLexical(ctx, repo.LexicalQuery{Text: text, AssetID: req.AssetID, Limit: fetch})
		if err != nil {
			return HybridResult{}, err
		}
		lexical = hits
	}

	fused := Fuse(lexical, vector)
	if len(fused) > limit {
		fused = fused[:limit]
	}
	return HybridResult{Hits: fused, Mode: mode, Model: model}, nil
}

func (r Retriever) semantic(ctx context.Context, text string, asset id.Ref, limit int) ([]domain.SearchHit, string, error) {
	if r.Embedder == nil {
		return nil, "", errs.New(errs.CodeUnavailable, "no embedding connector is available")
	}
	if r.Embedder.Info().Capabilities["external"] {
		// No asset policy reaches a search query, so nothing external is used (I215).
		return nil, "", errs.New(errs.CodeFailedPrecondition, "the embedding connector is external; query embeddings stay local")
	}
	active, err := r.Search.ActiveModel(ctx)
	if err != nil {
		return nil, "", err
	}
	if active == "" {
		return nil, "", errs.New(errs.CodeUnavailable, "nothing has been embedded yet")
	}
	embedded, err := r.Embedder.Embed(ctx, connector.EmbedRequest{Texts: []string{text}, Kind: connector.EmbedQuery})
	if err != nil {
		return nil, "", err
	}
	if embedded.Model != active {
		// A query vector of another model cannot be compared (R18).
		return nil, "", errs.Newf(errs.CodeUnavailable, "the query model %q is not the active model %q", embedded.Model, active)
	}
	if len(embedded.Vectors) != 1 {
		return nil, "", errs.New(errs.CodeInternal, "the embedder returned no query vector")
	}
	hits, err := r.Search.SearchVector(ctx, repo.VectorQuery{Vector: embedded.Vectors[0], Model: active, AssetID: asset, Limit: limit})
	if err != nil {
		return nil, "", err
	}
	kept := hits[:0]
	for _, hit := range hits {
		if hit.Score > 0 {
			kept = append(kept, hit)
		}
	}
	return kept, active, nil
}

// limitWords keeps the first n whitespace-separated words.
func limitWords(text string, n int) string {
	fields := strings.Fields(text)
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}

// Options configures the search service.
type Options struct {
	Stores repo.TenantStores
	// Embedder opens the tenant's embedding connector for query vectors. May be nil,
	// which leaves search lexical.
	Embedder func(ctx context.Context, tenant domain.TenantID) (connector.Embedder, error)
	Logger   *slog.Logger
}

// Service implements service.Search.
type Service struct {
	opts Options
}

var _ service.Search = (*Service)(nil)

// New returns the search service.
func New(opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Service{opts: opts}
}

// Search answers GET /search (R41, I43). An unknown asset answers not_found; the tenant
// store resolves it, so another tenant's asset looks the same as a missing one.
func (s *Service) Search(ctx context.Context, scope service.Scope, q api.SearchQuery) (api.SearchResponse, error) {
	store, err := s.opts.Stores.ForTenant(ctx, scope.Tenant)
	if err != nil {
		return api.SearchResponse{}, err
	}
	asset := id.Ref(q.AssetID)
	if asset != "" {
		if err := asset.Validate(); err != nil {
			return api.SearchResponse{}, errs.Wrap(errs.CodeInvalidArgument, err, "invalid asset reference")
		}
		if _, err := store.Assets().Get(ctx, asset); err != nil {
			return api.SearchResponse{}, err
		}
	}
	retriever := Retriever{Search: store.Search(), Logger: s.opts.Logger}
	if q.Mode != ModeLexical && s.opts.Embedder != nil {
		embedder, err := s.opts.Embedder(ctx, scope.Tenant)
		switch {
		case err == nil:
			retriever.Embedder = embedder
		case ctx.Err() != nil:
			return api.SearchResponse{}, ctx.Err()
		default:
			s.opts.Logger.Warn("cannot open the embedding connector", "tenant", scope.Tenant, "reason", errs.CodeOf(err))
		}
	}
	result, err := retriever.Hybrid(ctx, HybridRequest{Text: q.Q, AssetID: asset, Mode: q.Mode, Limit: q.Limit})
	if err != nil {
		return api.SearchResponse{}, err
	}
	items := result.Hits
	if items == nil {
		items = []domain.SearchHit{}
	}
	return api.SearchResponse{Items: items, Mode: result.Mode, EmbeddingModel: result.Model}, nil
}
