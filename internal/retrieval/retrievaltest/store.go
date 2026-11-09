// Package retrievaltest holds a small in-memory tenant store for the AI component's
// tests: assets, transcripts and search with FTS5's and sqlite-vec's observable
// behavior as `core` implements them (its I106). Only test code may import it. Any
// repository it does not implement panics, which also shows a node reaching for what
// it must not use.
package retrievaltest

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

type vectorSet struct {
	chunking string
	chunks   []domain.EmbeddedChunk
}

// Store implements repo.TenantStore over maps.
type Store struct {
	repo.TenantRepos // nil: any other repository panics

	TenantID domain.TenantID

	mu          sync.Mutex
	assets      map[id.Friendly]domain.Asset
	transcripts map[string]domain.Transcript
	analyses    map[string]domain.Analysis
	sensitive   map[string][]domain.SensitiveSegment
	lexical     map[string][]domain.SearchChunk
	vectors     map[string]map[string]vectorSet
	active      string

	// Rollbacks counts failed transactions.
	Rollbacks int
}

// New returns an empty store.
func New(tenant domain.TenantID) *Store {
	return &Store{
		TenantID:    tenant,
		assets:      map[id.Friendly]domain.Asset{},
		transcripts: map[string]domain.Transcript{},
		analyses:    map[string]domain.Analysis{},
		sensitive:   map[string][]domain.SensitiveSegment{},
		lexical:     map[string][]domain.SearchChunk{},
		vectors:     map[string]map[string]vectorSet{},
	}
}

var _ repo.TenantStore = (*Store)(nil)

func (s *Store) Tenant() domain.TenantID { return s.TenantID }

// InTx runs fn and restores the previous state if it fails.
func (s *Store) InTx(ctx context.Context, fn func(repo.TenantRepos) error) error {
	s.mu.Lock()
	assets, transcripts, analyses := maps.Clone(s.assets), maps.Clone(s.transcripts), maps.Clone(s.analyses)
	sensitive, lexical := maps.Clone(s.sensitive), maps.Clone(s.lexical)
	vectors := map[string]map[string]vectorSet{}
	for model, sets := range s.vectors {
		vectors[model] = maps.Clone(sets)
	}
	active := s.active
	s.mu.Unlock()

	if err := fn(s); err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.assets, s.transcripts, s.analyses = assets, transcripts, analyses
		s.sensitive, s.lexical, s.vectors, s.active = sensitive, lexical, vectors, active
		s.Rollbacks++
		return err
	}
	return nil
}

func (s *Store) Assets() repo.AssetRepo           { return assetRepo{s: s} }
func (s *Store) Transcripts() repo.TranscriptRepo { return transcriptRepo{s: s} }
func (s *Store) Search() repo.SearchRepo          { return searchRepo{s: s} }

// PutAsset adds or replaces an asset directly. The default version is v1.
func (s *Store) PutAsset(a domain.Asset) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.CurrentVersion == "" {
		a.CurrentVersion = "v1"
	}
	s.assets[a.ID] = a
}

// LexicalChunks returns what is indexed for a version.
func (s *Store) LexicalChunks(asset id.Friendly, version string) []domain.SearchChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lexical[key(asset, version)]
}

// VectorChunks returns the vectors of a model, and its chunking strategy.
func (s *Store) VectorChunks(model string, asset id.Friendly, version string) ([]domain.EmbeddedChunk, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.vectors[model][key(asset, version)]
	return set.chunks, set.chunking
}

// HasModel reports whether a model has any vectors.
func (s *Store) HasModel(model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.vectors[model]) > 0
}

func key(asset id.Friendly, version string) string { return string(asset) + "\x00" + version }

func (s *Store) resolve(ref id.Ref) (domain.Asset, error) {
	for _, a := range s.assets {
		if string(a.ID) == string(ref) || string(a.ULID) == string(ref) {
			return a, nil
		}
	}
	return domain.Asset{}, errs.NotFound("asset")
}

type assetRepo struct {
	repo.AssetRepo
	s *Store
}

func (r assetRepo) Get(_ context.Context, ref id.Ref) (domain.Asset, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.s.resolve(ref)
}

func (r assetRepo) List(_ context.Context, _ repo.AssetFilter, _ repo.Page) ([]domain.Asset, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := slices.Collect(maps.Values(r.s.assets))
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, "", nil
}

func (r assetRepo) Update(_ context.Context, a domain.Asset) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if _, ok := r.s.assets[a.ID]; !ok {
		return errs.NotFound("asset")
	}
	r.s.assets[a.ID] = a
	return nil
}

type transcriptRepo struct {
	repo.TranscriptRepo
	s *Store
}

func (r transcriptRepo) Save(_ context.Context, t domain.Transcript) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if t.Version == "" {
		return errs.New(errs.CodeInvalidArgument, "a transcript needs a version")
	}
	a, err := r.s.resolve(id.Ref(t.AssetID))
	if err != nil {
		return err
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	t.AssetID = a.ID
	r.s.transcripts[key(a.ID, t.Version)] = t
	return nil
}

func (r transcriptRepo) Get(_ context.Context, ref id.Ref, version string) (domain.Transcript, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	a, err := r.s.resolve(ref)
	if err != nil {
		return domain.Transcript{}, err
	}
	t, ok := r.s.transcripts[key(a.ID, version)]
	if !ok {
		return domain.Transcript{}, errs.NotFound("transcript")
	}
	return t, nil
}

func (r transcriptRepo) SaveAnalysis(_ context.Context, a domain.Analysis) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if a.Version == "" {
		return errs.New(errs.CodeInvalidArgument, "an analysis needs a version")
	}
	asset, err := r.s.resolve(id.Ref(a.AssetID))
	if err != nil {
		return err
	}
	a.AssetID = asset.ID
	r.s.analyses[key(asset.ID, a.Version)] = a
	return nil
}

func (r transcriptRepo) Analysis(_ context.Context, ref id.Ref, version string) (domain.Analysis, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	asset, err := r.s.resolve(ref)
	if err != nil {
		return domain.Analysis{}, err
	}
	a, ok := r.s.analyses[key(asset.ID, version)]
	if !ok {
		return domain.Analysis{}, errs.NotFound("analysis")
	}
	return a, nil
}

func (r transcriptRepo) MarkSensitive(_ context.Context, mark domain.SensitiveSegment) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if mark.Version == "" {
		return errs.New(errs.CodeInvalidArgument, "a sensitive segment needs a version")
	}
	if mark.StartSeconds < 0 || mark.EndSeconds <= mark.StartSeconds {
		return errs.New(errs.CodeInvalidArgument, "a sensitive segment needs 0 <= start < end")
	}
	a, err := r.s.resolve(id.Ref(mark.AssetID))
	if err != nil {
		return err
	}
	mark.AssetID = a.ID
	k := key(a.ID, mark.Version)
	marks := slices.Clone(r.s.sensitive[k])
	for i, existing := range marks {
		if existing.StartSeconds == mark.StartSeconds && existing.EndSeconds == mark.EndSeconds {
			marks[i] = mark
			r.s.sensitive[k] = marks
			return nil
		}
	}
	r.s.sensitive[k] = append(marks, mark)
	return nil
}

func (r transcriptRepo) Sensitive(_ context.Context, ref id.Ref, version string) ([]domain.SensitiveSegment, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	a, err := r.s.resolve(ref)
	if err != nil {
		return nil, err
	}
	return slices.Clone(r.s.sensitive[key(a.ID, version)]), nil
}

type searchRepo struct {
	repo.SearchRepo
	s *Store
}

func (r searchRepo) IndexLexical(_ context.Context, ref id.Ref, version string, chunks []domain.SearchChunk) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	a, err := r.s.resolve(ref)
	if err != nil {
		return err
	}
	r.s.lexical[key(a.ID, version)] = slices.Clone(chunks)
	return nil
}

func (r searchRepo) IndexVectors(_ context.Context, batch repo.EmbeddingBatch) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if batch.Model == "" {
		return errs.New(errs.CodeInvalidArgument, "vectors need an embedding model")
	}
	a, err := r.s.resolve(batch.AssetID)
	if err != nil {
		return err
	}
	sets := maps.Clone(r.s.vectors[batch.Model])
	if sets == nil {
		sets = map[string]vectorSet{}
	}
	sets[key(a.ID, batch.Version)] = vectorSet{batch.Chunking, slices.Clone(batch.Chunks)}
	r.s.vectors[batch.Model] = sets
	if r.s.active == "" && len(batch.Chunks) > 0 {
		r.s.active = batch.Model
	}
	return nil
}

func tokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(c rune) bool {
		return !unicode.IsLetter(c) && !unicode.IsDigit(c)
	})
}

func (r searchRepo) SearchLexical(_ context.Context, q repo.LexicalQuery) ([]domain.SearchHit, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	wanted := map[string]bool{}
	for _, token := range tokens(q.Text) {
		wanted[token] = true
	}
	var out []domain.SearchHit
	for _, a := range r.s.assets {
		if q.AssetID != "" && string(a.ID) != string(q.AssetID) && string(a.ULID) != string(q.AssetID) {
			continue
		}
		if a.DeletedAt != nil {
			continue
		}
		for _, chunk := range r.s.lexical[key(a.ID, a.CurrentVersion)] {
			score := 0.0
			for _, token := range tokens(chunk.Text) {
				if wanted[token] {
					score++
				}
			}
			if score > 0 {
				out = append(out, domain.SearchHit{AssetID: a.ID, Version: a.CurrentVersion, Level: chunk.Level,
					StartSeconds: chunk.StartSeconds, EndSeconds: chunk.EndSeconds, Text: chunk.Text, Score: score, Source: "lexical"})
			}
		}
	}
	sortHits(out)
	return limited(out, q.Limit), nil
}

func sortHits(hits []domain.SearchHit) {
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].Score != hits[b].Score {
			return hits[a].Score > hits[b].Score
		}
		return hits[a].StartSeconds < hits[b].StartSeconds
	})
}

func limited(hits []domain.SearchHit, limit int) []domain.SearchHit {
	if limit > 0 && len(hits) > limit {
		return hits[:limit]
	}
	return hits
}

func (r searchRepo) SearchVector(_ context.Context, q repo.VectorQuery) ([]domain.SearchHit, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []domain.SearchHit
	for _, a := range r.s.assets {
		if q.AssetID != "" && string(a.ID) != string(q.AssetID) && string(a.ULID) != string(q.AssetID) {
			continue
		}
		if a.DeletedAt != nil {
			continue
		}
		for _, chunk := range r.s.vectors[q.Model][key(a.ID, a.CurrentVersion)].chunks {
			out = append(out, domain.SearchHit{AssetID: a.ID, Version: a.CurrentVersion, Level: chunk.Level,
				StartSeconds: chunk.StartSeconds, EndSeconds: chunk.EndSeconds, Text: chunk.Text,
				Score: cosine(q.Vector, chunk.Vector), Source: "vector"})
		}
	}
	sortHits(out)
	return limited(out, q.Limit), nil
}

func cosine(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range min(len(a), len(b)) {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / math.Sqrt(normA*normB)
}

func (r searchRepo) ActiveModel(context.Context) (string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return r.s.active, nil
}

func (r searchRepo) SetActiveModel(_ context.Context, model string) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if _, ok := r.s.vectors[model]; !ok {
		return errs.NotFound("embedding model")
	}
	r.s.active = model
	return nil
}

func (r searchRepo) DeleteModel(_ context.Context, model string) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.active == model {
		return errs.Newf(errs.CodeFailedPrecondition, "model %s is active", model)
	}
	delete(r.s.vectors, model)
	return nil
}

func (r searchRepo) HasIndex(_ context.Context, ref id.Ref, version, model string) (bool, bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	a, err := r.s.resolve(ref)
	if err != nil {
		return false, false, err
	}
	k := key(a.ID, version)
	return len(r.s.lexical[k]) > 0, len(r.s.vectors[model][k].chunks) > 0, nil
}
