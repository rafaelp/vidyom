package retrieval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/aiworker"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/retrieval/retrievaltest"
)

// hashEmbedder is a local embedder with the fake engine's model.
type hashEmbedder struct {
	external bool
	model    string
	fail     error
	queries  []string
}

func (e *hashEmbedder) Info() connector.Info {
	return connector.Info{Capabilities: map[string]bool{"external": e.external}}
}

func (e *hashEmbedder) Embed(_ context.Context, req connector.EmbedRequest) (connector.EmbedResult, error) {
	if e.fail != nil {
		return connector.EmbedResult{}, e.fail
	}
	model := e.model
	if model == "" {
		model = aiworker.FakeEmbeddingModel
	}
	out := connector.EmbedResult{Model: model, Dimensions: aiworker.FakeEmbeddingDimensions}
	for _, text := range req.Texts {
		e.queries = append(e.queries, text)
		out.Vectors = append(out.Vectors, aiworker.FakeHashEmbed(text))
	}
	return out, nil
}

const tenant = domain.TenantID("acme")

func indexed(t *testing.T, store *retrievaltest.Store, asset id.Friendly, chunks []domain.SearchChunk) {
	t.Helper()
	ctx := context.Background()
	store.PutAsset(domain.Asset{ID: asset, TenantID: tenant, Title: string(asset)})
	if err := store.Search().IndexLexical(ctx, id.Ref(asset), "v1", chunks); err != nil {
		t.Fatal(err)
	}
	embedded := make([]domain.EmbeddedChunk, len(chunks))
	for i, chunk := range chunks {
		embedded[i] = domain.EmbeddedChunk{SearchChunk: chunk, Vector: aiworker.FakeHashEmbed(chunk.Text)}
	}
	if err := store.Search().IndexVectors(ctx, repo.EmbeddingBatch{AssetID: id.Ref(asset), Version: "v1", Model: aiworker.FakeEmbeddingModel, Chunking: ChunkingStrategy, Chunks: embedded}); err != nil {
		t.Fatal(err)
	}
}

func window(start, end float64, text string) domain.SearchChunk {
	return domain.SearchChunk{Level: domain.ChunkWindow, StartSeconds: start, EndSeconds: end, Text: text}
}

func TestFuseOrdersByReciprocalRank(t *testing.T) {
	hit := func(start float64, source string) domain.SearchHit {
		return domain.SearchHit{AssetID: "ORI-001", Version: "v1", Level: domain.ChunkWindow, StartSeconds: start, EndSeconds: start + 40, Source: source}
	}
	lexical := []domain.SearchHit{hit(0, "lexical"), hit(40, "lexical"), hit(80, "lexical")}
	vector := []domain.SearchHit{hit(40, "vector"), hit(120, "vector")}
	fused := Fuse(lexical, vector)
	if len(fused) != 4 {
		t.Fatalf("fused = %+v", fused)
	}
	if fused[0].StartSeconds != 40 || fused[0].Source != "hybrid" {
		t.Fatalf("a chunk in both lists wins: %+v", fused[0])
	}
	if want := 1.0/61 + 1.0/62; abs(fused[0].Score-want) > 1e-12 {
		t.Fatalf("score = %f, want %f", fused[0].Score, want)
	}
	for _, f := range fused[1:] {
		if f.Source == "hybrid" {
			t.Fatalf("only one chunk is in both lists: %+v", f)
		}
	}
	if fused[1].StartSeconds != 0 {
		t.Fatalf("second = %+v", fused[1])
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestFuseKeepsVersionsAndLevelsApart(t *testing.T) {
	a := domain.SearchHit{AssetID: "ORI-001", Version: "v1", Level: domain.ChunkWindow, StartSeconds: 0, EndSeconds: 1}
	b := a
	b.Version = "v2"
	c := a
	c.Level = domain.ChunkChapter
	d := a
	d.AssetID = "ORI-002"
	if fused := Fuse([]domain.SearchHit{a}, []domain.SearchHit{b, c, d}); len(fused) != 4 {
		t.Fatalf("fused = %+v", fused)
	}
}

func TestFuseCountsADuplicateOnce(t *testing.T) {
	a := domain.SearchHit{AssetID: "ORI-001", Version: "v1", Level: domain.ChunkWindow, StartSeconds: 0, EndSeconds: 1}
	fused := Fuse([]domain.SearchHit{a, a})
	if len(fused) != 1 || abs(fused[0].Score-1.0/61) > 1e-12 {
		t.Fatalf("fused = %+v", fused)
	}
}

func TestNormalizedScore(t *testing.T) {
	if got := NormalizedScore(2.0 / 61); abs(got-1) > 1e-9 {
		t.Fatalf("rank 1 in both = %f", got)
	}
	if got := NormalizedScore(1.0 / 61); abs(got-0.5) > 1e-9 {
		t.Fatalf("rank 1 in one = %f", got)
	}
}

func searchFixture(t *testing.T) (*retrievaltest.Store, *Service, *hashEmbedder) {
	t.Helper()
	store := retrievaltest.New(tenant)
	indexed(t, store, "ORI-001", []domain.SearchChunk{
		window(0, 40, "Welcome to the class and conscious breathing begins"),
		window(34, 80, "The story of a woman who had no remorse"),
	})
	indexed(t, store, "ORI-002", []domain.SearchChunk{window(0, 40, "Another class about conscious breathing")})
	embedder := &hashEmbedder{}
	service := New(Options{
		Stores:   stores{store},
		Embedder: func(context.Context, domain.TenantID) (connector.Embedder, error) { return embedder, nil },
	})
	return store, service, embedder
}

type stores struct{ store *retrievaltest.Store }

func (s stores) Create(context.Context, domain.TenantID) (repo.TenantStore, error) {
	return nil, errors.New("not used")
}
func (s stores) ForTenant(_ context.Context, tenant domain.TenantID) (repo.TenantStore, error) {
	if tenant != s.store.Tenant() {
		return nil, errs.NotFound("tenant")
	}
	return s.store, nil
}
func (s stores) Tenants(context.Context) ([]domain.TenantID, error) { return nil, nil }
func (s stores) Close() error                                       { return nil }

func scope() service.Scope { return service.Scope{Tenant: tenant} }

func TestSearchHybridAcrossTheTenant(t *testing.T) {
	_, service, embedder := searchFixture(t)
	response, err := service.Search(context.Background(), scope(), api.SearchQuery{Q: "conscious breathing"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Mode != ModeHybrid || response.EmbeddingModel != aiworker.FakeEmbeddingModel {
		t.Fatalf("response = %+v", response)
	}
	assets := map[id.Friendly]bool{}
	for _, item := range response.Items {
		assets[item.AssetID] = true
		if item.Source != "hybrid" {
			t.Errorf("a shared-word match is in both lists: %+v", item)
		}
	}
	if !assets["ORI-001"] || !assets["ORI-002"] {
		t.Fatalf("items = %+v", response.Items)
	}
	if len(embedder.queries) != 1 {
		t.Fatalf("query embeddings = %v", embedder.queries)
	}
}

func TestSearchRestrictedToAsset(t *testing.T) {
	_, service, _ := searchFixture(t)
	response, err := service.Search(context.Background(), scope(), api.SearchQuery{Q: "conscious breathing", AssetID: "ORI-002"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].AssetID != "ORI-002" {
		t.Fatalf("items = %+v", response.Items)
	}
}

func TestSearchVectorWithNoSharedWordIsNoMatch(t *testing.T) {
	_, service, _ := searchFixture(t)
	response, err := service.Search(context.Background(), scope(), api.SearchQuery{Q: "quarterly invoice", Mode: ModeSemantic})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 0 {
		t.Fatalf("orthogonal vectors are no match: %+v", response.Items)
	}
	if response.Items == nil {
		t.Fatal("items must encode as an array")
	}
}

func TestSearchModes(t *testing.T) {
	_, service, embedder := searchFixture(t)
	response, err := service.Search(context.Background(), scope(), api.SearchQuery{Q: "remorse", Mode: ModeLexical})
	if err != nil {
		t.Fatal(err)
	}
	if response.Mode != ModeLexical || response.EmbeddingModel != "" || response.Items[0].Source != "lexical" {
		t.Fatalf("response = %+v", response)
	}
	if len(embedder.queries) != 0 {
		t.Fatal("lexical mode must not embed the query")
	}
	response, err = service.Search(context.Background(), scope(), api.SearchQuery{Q: "remorse", Mode: ModeSemantic})
	if err != nil {
		t.Fatal(err)
	}
	if response.Mode != ModeSemantic || response.Items[0].Source != "vector" {
		t.Fatalf("response = %+v", response)
	}
}

func TestSearchBadRequests(t *testing.T) {
	_, service, _ := searchFixture(t)
	cases := map[string]api.SearchQuery{
		"empty":        {Q: "   "},
		"unknown mode": {Q: "x", Mode: "fuzzy"},
		"bad asset":    {Q: "x", AssetID: "nonsense"},
	}
	for name, query := range cases {
		if _, err := service.Search(context.Background(), scope(), query); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := service.Search(context.Background(), scope(), api.SearchQuery{Q: "x", AssetID: "ORI-404"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("unknown asset: err = %v", err)
	}
	if _, err := service.Search(context.Background(), service2Scope(), api.SearchQuery{Q: "x"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("unknown tenant: err = %v", err)
	}
}

func service2Scope() service.Scope { return service.Scope{Tenant: "other"} }

func TestSearchLimit(t *testing.T) {
	store := retrievaltest.New(tenant)
	var chunks []domain.SearchChunk
	for i := range 150 {
		chunks = append(chunks, window(float64(i*40), float64(i*40+40), "breathing"))
	}
	indexed(t, store, "ORI-001", chunks)
	service := New(Options{Stores: stores{store}})
	response, err := service.Search(context.Background(), scope(), api.SearchQuery{Q: "breathing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != defaultLimit {
		t.Fatalf("default: %d", len(response.Items))
	}
	response, err = service.Search(context.Background(), scope(), api.SearchQuery{Q: "breathing", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != maximumLimit {
		t.Fatalf("maximum: %d", len(response.Items))
	}
}

func TestSearchDegradesToLexical(t *testing.T) {
	cases := map[string]func(*Options){
		"no embedder configured": func(o *Options) { o.Embedder = nil },
		"cannot open": func(o *Options) {
			o.Embedder = func(context.Context, domain.TenantID) (connector.Embedder, error) {
				return nil, errs.New(errs.CodeUnavailable, "down")
			}
		},
		"embedder fails": func(o *Options) {
			o.Embedder = func(context.Context, domain.TenantID) (connector.Embedder, error) {
				return &hashEmbedder{fail: errs.New(errs.CodeUnavailable, "down")}, nil
			}
		},
		"another model": func(o *Options) {
			o.Embedder = func(context.Context, domain.TenantID) (connector.Embedder, error) {
				return &hashEmbedder{model: "bge-m3@1"}, nil
			}
		},
		"external embedder": func(o *Options) {
			o.Embedder = func(context.Context, domain.TenantID) (connector.Embedder, error) {
				return &hashEmbedder{external: true}, nil
			}
		},
	}
	for name, mutate := range cases {
		store, _, _ := searchFixture(t)
		options := Options{Stores: stores{store}}
		mutate(&options)
		response, err := New(options).Search(context.Background(), scope(), api.SearchQuery{Q: "remorse"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if response.Mode != ModeLexical || response.EmbeddingModel != "" || len(response.Items) == 0 {
			t.Errorf("%s: response = %+v", name, response)
		}
		for _, item := range response.Items {
			if item.Source != "lexical" {
				t.Errorf("%s: %+v", name, item)
			}
		}
	}
}

func TestSemanticModeFailsInsteadOfDegrading(t *testing.T) {
	store, _, _ := searchFixture(t)
	down := New(Options{Stores: stores{store}, Embedder: func(context.Context, domain.TenantID) (connector.Embedder, error) {
		return &hashEmbedder{fail: errs.New(errs.CodeUnavailable, "down")}, nil
	}})
	if _, err := down.Search(context.Background(), scope(), api.SearchQuery{Q: "remorse", Mode: ModeSemantic}); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("err = %v", err)
	}
	external := New(Options{Stores: stores{store}, Embedder: func(context.Context, domain.TenantID) (connector.Embedder, error) {
		return &hashEmbedder{external: true}, nil
	}})
	if _, err := external.Search(context.Background(), scope(), api.SearchQuery{Q: "remorse", Mode: ModeSemantic}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
}

// R18: a query vector of another model is never compared with stored vectors.
func TestNoModelMixing(t *testing.T) {
	store, _, _ := searchFixture(t)
	service := New(Options{Stores: stores{store}, Embedder: func(context.Context, domain.TenantID) (connector.Embedder, error) {
		return &hashEmbedder{model: "other@1"}, nil
	}})
	if _, err := service.Search(context.Background(), scope(), api.SearchQuery{Q: "remorse", Mode: ModeSemantic}); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestQueryIsCutTo32Words(t *testing.T) {
	store, _, _ := searchFixture(t)
	embedder := &hashEmbedder{}
	retriever := Retriever{Search: store.Search(), Embedder: embedder}
	long := strings.TrimSpace(strings.Repeat("word ", 100))
	if _, err := retriever.Hybrid(context.Background(), HybridRequest{Text: long}); err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(embedder.queries[0])); got != maxQueryWords {
		t.Fatalf("%d words", got)
	}
}

func TestCancellationPropagates(t *testing.T) {
	store, _, _ := searchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	retriever := Retriever{Search: store.Search(), Embedder: &hashEmbedder{fail: ctx.Err()}}
	if _, err := retriever.Hybrid(ctx, HybridRequest{Text: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
