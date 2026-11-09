// Package fakestore is an in-memory repo.TenantStore for the publishing component's
// tests, because this component may not import component core's repositories
// (master plan, section 8). It implements what publishing uses, with the rules the
// contract states: R43 on publications, the asset state machine, immutable
// versions, and InTx that commits or rolls back as one. A repository publishing
// does not use is left nil, so reaching for one panics in a test. It is not
// production code and nothing outside tests imports it.
package fakestore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// data is everything the store holds, copied whole for InTx rollback.
type data struct {
	Seq          map[string]uint64
	Assets       map[string]*domain.Asset
	Versions     map[string]map[string]*domain.AssetVersion
	History      []domain.HistoryEntry
	Transcripts  map[string]domain.Transcript
	Analyses     map[string]domain.Analysis
	Sensitive    map[string][]domain.SensitiveSegment
	Approvals    map[string]*domain.Approval
	Publications map[string]*domain.Publication
	Audit        []domain.AuditRecord
	Outbox       []event.Envelope
	ActiveModel  string
	Embeddings   map[string]map[string][]domain.EmbeddedChunk
}

func newData() *data {
	return &data{
		Seq:          map[string]uint64{},
		Assets:       map[string]*domain.Asset{},
		Versions:     map[string]map[string]*domain.AssetVersion{},
		Transcripts:  map[string]domain.Transcript{},
		Analyses:     map[string]domain.Analysis{},
		Sensitive:    map[string][]domain.SensitiveSegment{},
		Approvals:    map[string]*domain.Approval{},
		Publications: map[string]*domain.Publication{},
		Embeddings:   map[string]map[string][]domain.EmbeddedChunk{},
	}
}

func (d *data) clone() *data {
	raw, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	out := newData()
	if err := json.Unmarshal(raw, out); err != nil {
		panic(err)
	}
	return out
}

// Store is the fake tenant database.
type Store struct {
	tenant domain.TenantID
	mu     sync.Mutex
	d      *data

	// NoEmbeddingLister makes Search() hide repo.EmbeddingLister, as a store
	// without the optional capability would.
	NoEmbeddingLister bool
	// SensitiveErr fails Sensitive for one asset, to prove the export fails closed.
	SensitiveErr func(asset id.Ref, version string) error
}

var _ repo.TenantStore = (*Store)(nil)

// New returns an empty store for tenant.
func New(tenant domain.TenantID) *Store { return &Store{tenant: tenant, d: newData()} }

func (s *Store) Tenant() domain.TenantID { return s.tenant }

// repos is one view of the store. Inside InTx the lock is already held.
type repos struct {
	s    *Store
	inTx bool
}

func (r repos) lock() func() {
	if r.inTx {
		return func() {}
	}
	r.s.mu.Lock()
	return r.s.mu.Unlock
}

// InTx commits everything fn does, or nothing.
func (s *Store) InTx(_ context.Context, fn func(repo.TenantRepos) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved := s.d.clone()
	if err := fn(repos{s: s, inTx: true}); err != nil {
		s.d = saved
		return err
	}
	return nil
}

func (s *Store) view() repos { return repos{s: s} }

func (s *Store) IDs() repo.IDAllocator              { return s.view() }
func (s *Store) Assets() repo.AssetRepo             { return assets{s.view()} }
func (s *Store) Versions() repo.VersionRepo         { return versions{s.view()} }
func (s *Store) Collections() repo.CollectionRepo   { return nil }
func (s *Store) Transcripts() repo.TranscriptRepo   { return transcripts{s.view()} }
func (s *Store) Search() repo.SearchRepo            { return s.view().Search() }
func (s *Store) Jobs() repo.JobRepo                 { return nil }
func (s *Store) Approvals() repo.ApprovalRepo       { return approvals{s.view()} }
func (s *Store) Publications() repo.PublicationRepo { return publications{s.view()} }
func (s *Store) Uploads() repo.UploadRepo           { return nil }
func (s *Store) Audit() repo.AuditRepo              { return audit{s.view()} }
func (s *Store) Outbox() repo.OutboxRepo            { return outbox{s.view()} }
func (s *Store) Secrets() repo.SecretRepo           { return nil }
func (s *Store) Budgets() repo.BudgetRepo           { return nil }
func (s *Store) Idempotency() repo.IdempotencyRepo  { return nil }

func (r repos) IDs() repo.IDAllocator              { return r }
func (r repos) Assets() repo.AssetRepo             { return assets{r} }
func (r repos) Versions() repo.VersionRepo         { return versions{r} }
func (r repos) Collections() repo.CollectionRepo   { return nil }
func (r repos) Transcripts() repo.TranscriptRepo   { return transcripts{r} }
func (r repos) Jobs() repo.JobRepo                 { return nil }
func (r repos) Approvals() repo.ApprovalRepo       { return approvals{r} }
func (r repos) Publications() repo.PublicationRepo { return publications{r} }
func (r repos) Uploads() repo.UploadRepo           { return nil }
func (r repos) Audit() repo.AuditRepo              { return audit{r} }
func (r repos) Outbox() repo.OutboxRepo            { return outbox{r} }
func (r repos) Secrets() repo.SecretRepo           { return nil }
func (r repos) Budgets() repo.BudgetRepo           { return nil }
func (r repos) Idempotency() repo.IdempotencyRepo  { return nil }
func (r repos) Search() repo.SearchRepo {
	if r.s.NoEmbeddingLister {
		return bareSearch{search{r}}
	}
	return search{r}
}

// Next allocates a friendly ID.
func (r repos) Next(_ context.Context, prefix id.Prefix) (id.Friendly, error) {
	defer r.lock()()
	r.s.d.Seq[string(prefix)]++
	return id.Format(prefix, r.s.d.Seq[string(prefix)]), nil
}

func clone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("clone %T: %v", v, err))
	}
	return out
}

// ---- Assets ----

type assets struct{ r repos }

func (a assets) find(ref id.Ref) (*domain.Asset, error) {
	for _, x := range a.r.s.d.Assets {
		if string(x.ID) == string(ref) || string(x.ULID) == string(ref) {
			return x, nil
		}
	}
	return nil, errs.NotFound("asset")
}

// hydrate denormalizes a read the way the real repository does (I105).
func (a assets) hydrate(x domain.Asset) domain.Asset {
	if vs, ok := a.r.s.d.Versions[string(x.ID)]; ok {
		if v, ok := vs[x.CurrentVersion]; ok {
			for _, f := range v.Files {
				file := f.File
				if f.Name != "" {
					continue
				}
				switch f.Purpose {
				case domain.FileWorkingMaster:
					x.WorkingMaster = &file
				case domain.FileThumbnail:
					x.Thumbnail = &file
				case domain.FileProxy:
					x.Proxy = &file
				case domain.FileBackup:
					x.Backup = &file
				case domain.FileSourceOriginal:
					x.SourceOriginal = &file
				}
			}
		}
	}
	return x
}

func (a assets) Create(_ context.Context, x domain.Asset) error {
	defer a.r.lock()()
	if _, err := a.find(id.Ref(x.ID)); err == nil {
		return errs.New(errs.CodeConflict, "asset exists")
	}
	if x.Status == "" {
		x.Status = domain.AssetRegistered
	}
	if x.Visibility == "" {
		x.Visibility = domain.VisibilityPrivate
	}
	if x.CurrentVersion == "" {
		x.CurrentVersion = "v1"
	}
	a.r.s.d.Assets[string(x.ID)] = &x
	return nil
}

func (a assets) Get(_ context.Context, ref id.Ref) (domain.Asset, error) {
	defer a.r.lock()()
	x, err := a.find(ref)
	if err != nil {
		return domain.Asset{}, err
	}
	return a.hydrate(clone(*x)), nil
}

func (a assets) List(context.Context, repo.AssetFilter, repo.Page) ([]domain.Asset, string, error) {
	panic("fakestore: Assets.List is not used by publishing")
}

func (a assets) Update(_ context.Context, x domain.Asset) error {
	defer a.r.lock()()
	cur, err := a.find(id.Ref(x.ID))
	if err != nil {
		return err
	}
	if !cur.Status.CanTransition(x.Status) {
		return errs.Newf(errs.CodeFailedPrecondition, "asset cannot move from %s to %s", cur.Status, x.Status)
	}
	x.WorkingMaster, x.Thumbnail, x.Proxy, x.Backup, x.SourceOriginal = nil, nil, nil, nil, nil
	a.r.s.d.Assets[string(x.ID)] = &x
	return nil
}

func (a assets) AppendHistory(_ context.Context, h domain.HistoryEntry) error {
	defer a.r.lock()()
	a.r.s.d.History = append(a.r.s.d.History, h)
	return nil
}

func (a assets) History(context.Context, id.Ref, repo.Page) ([]domain.HistoryEntry, string, error) {
	panic("fakestore: Assets.History is not used by publishing")
}

func (a assets) SoftDelete(_ context.Context, ref id.Ref, at time.Time) error {
	defer a.r.lock()()
	x, err := a.find(ref)
	if err != nil {
		return err
	}
	x.DeletedAt = &at
	return nil
}

func (a assets) ClipsOverlapping(context.Context, id.Ref, []domain.Segment) ([]domain.Asset, error) {
	panic("fakestore: Assets.ClipsOverlapping is not used by publishing")
}

// ---- Versions ----

type versions struct{ r repos }

func (v versions) asset(ref id.Ref) (string, error) {
	x, err := assets{v.r}.find(ref)
	if err != nil {
		return "", err
	}
	return string(x.ID), nil
}

func (v versions) Create(_ context.Context, x domain.AssetVersion) error {
	defer v.r.lock()()
	key, err := v.asset(id.Ref(x.AssetID))
	if err != nil {
		return err
	}
	if v.r.s.d.Versions[key] == nil {
		v.r.s.d.Versions[key] = map[string]*domain.AssetVersion{}
	}
	v.r.s.d.Versions[key][x.Version] = &x
	return nil
}

func (v versions) Get(_ context.Context, ref id.Ref, version string) (domain.AssetVersion, error) {
	defer v.r.lock()()
	key, err := v.asset(ref)
	if err != nil {
		return domain.AssetVersion{}, err
	}
	x, ok := v.r.s.d.Versions[key][version]
	if !ok {
		return domain.AssetVersion{}, errs.NotFound("asset version")
	}
	return clone(*x), nil
}

func (v versions) List(_ context.Context, ref id.Ref) ([]domain.AssetVersion, error) {
	defer v.r.lock()()
	key, err := v.asset(ref)
	if err != nil {
		return nil, err
	}
	var out []domain.AssetVersion
	for _, x := range v.r.s.d.Versions[key] {
		out = append(out, clone(*x))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func (v versions) PutFile(_ context.Context, ref id.Ref, version string, f domain.VersionFile) error {
	defer v.r.lock()()
	key, err := v.asset(ref)
	if err != nil {
		return err
	}
	x, ok := v.r.s.d.Versions[key][version]
	if !ok {
		return errs.NotFound("asset version")
	}
	if x.Immutable {
		return errs.New(errs.CodeFailedPrecondition, "this version is immutable")
	}
	x.Files = slices.DeleteFunc(x.Files, func(o domain.VersionFile) bool {
		return o.Purpose == f.Purpose && o.Name == f.Name
	})
	x.Files = append(x.Files, f)
	return nil
}

func (v versions) MarkImmutable(_ context.Context, ref id.Ref, version string) error {
	defer v.r.lock()()
	key, err := v.asset(ref)
	if err != nil {
		return err
	}
	x, ok := v.r.s.d.Versions[key][version]
	if !ok {
		return errs.NotFound("asset version")
	}
	x.Immutable = true
	return nil
}

func (v versions) SetProvenance(context.Context, id.Ref, string, domain.Provenance) error {
	panic("fakestore: Versions.SetProvenance is not used by publishing")
}

// ---- Transcripts ----

type transcripts struct{ r repos }

func versionKey(asset, version string) string { return asset + "/" + version }

func (t transcripts) key(ref id.Ref, version string) (string, error) {
	x, err := assets{t.r}.find(ref)
	if err != nil {
		return "", err
	}
	return versionKey(string(x.ID), version), nil
}

func (t transcripts) Save(_ context.Context, x domain.Transcript) error {
	defer t.r.lock()()
	key, err := t.key(id.Ref(x.AssetID), x.Version)
	if err != nil {
		return err
	}
	t.r.s.d.Transcripts[key] = x
	return nil
}

func (t transcripts) Get(_ context.Context, ref id.Ref, version string) (domain.Transcript, error) {
	defer t.r.lock()()
	key, err := t.key(ref, version)
	if err != nil {
		return domain.Transcript{}, err
	}
	x, ok := t.r.s.d.Transcripts[key]
	if !ok {
		return domain.Transcript{}, errs.NotFound("transcript")
	}
	return clone(x), nil
}

func (t transcripts) SaveAnalysis(_ context.Context, x domain.Analysis) error {
	defer t.r.lock()()
	key, err := t.key(id.Ref(x.AssetID), x.Version)
	if err != nil {
		return err
	}
	t.r.s.d.Analyses[key] = x
	return nil
}

func (t transcripts) Analysis(_ context.Context, ref id.Ref, version string) (domain.Analysis, error) {
	defer t.r.lock()()
	key, err := t.key(ref, version)
	if err != nil {
		return domain.Analysis{}, err
	}
	x, ok := t.r.s.d.Analyses[key]
	if !ok {
		return domain.Analysis{}, errs.NotFound("analysis")
	}
	return clone(x), nil
}

func (t transcripts) MarkSensitive(_ context.Context, x domain.SensitiveSegment) error {
	defer t.r.lock()()
	key, err := t.key(id.Ref(x.AssetID), x.Version)
	if err != nil {
		return err
	}
	t.r.s.d.Sensitive[key] = append(t.r.s.d.Sensitive[key], x)
	return nil
}

func (t transcripts) UnmarkSensitive(_ context.Context, ref id.Ref, version string, start, end float64) error {
	defer t.r.lock()()
	key, err := t.key(ref, version)
	if err != nil {
		return err
	}
	t.r.s.d.Sensitive[key] = slices.DeleteFunc(t.r.s.d.Sensitive[key], func(o domain.SensitiveSegment) bool {
		return o.StartSeconds == start && o.EndSeconds == end
	})
	return nil
}

func (t transcripts) Sensitive(_ context.Context, ref id.Ref, version string) ([]domain.SensitiveSegment, error) {
	defer t.r.lock()()
	if t.r.s.SensitiveErr != nil {
		if err := t.r.s.SensitiveErr(ref, version); err != nil {
			return nil, err
		}
	}
	key, err := t.key(ref, version)
	if err != nil {
		return nil, err
	}
	return clone(t.r.s.d.Sensitive[key]), nil
}

// ---- Search: only the active model and stored vectors ----

type search struct{ r repos }

// bareSearch hides ListEmbeddings.
type bareSearch struct{ search search }

func (b bareSearch) IndexLexical(ctx context.Context, a id.Ref, v string, c []domain.SearchChunk) error {
	return b.search.IndexLexical(ctx, a, v, c)
}
func (b bareSearch) IndexVectors(ctx context.Context, batch repo.EmbeddingBatch) error {
	return b.search.IndexVectors(ctx, batch)
}
func (b bareSearch) SearchLexical(ctx context.Context, q repo.LexicalQuery) ([]domain.SearchHit, error) {
	return b.search.SearchLexical(ctx, q)
}
func (b bareSearch) SearchVector(ctx context.Context, q repo.VectorQuery) ([]domain.SearchHit, error) {
	return b.search.SearchVector(ctx, q)
}
func (b bareSearch) DeleteAsset(ctx context.Context, a id.Ref) error {
	return b.search.DeleteAsset(ctx, a)
}
func (b bareSearch) DeleteModel(ctx context.Context, m string) error {
	return b.search.DeleteModel(ctx, m)
}
func (b bareSearch) ActiveModel(ctx context.Context) (string, error) {
	return b.search.ActiveModel(ctx)
}
func (b bareSearch) SetActiveModel(ctx context.Context, m string) error {
	return b.search.SetActiveModel(ctx, m)
}
func (b bareSearch) HasIndex(ctx context.Context, a id.Ref, v, m string) (bool, bool, error) {
	return b.search.HasIndex(ctx, a, v, m)
}

func (x search) IndexLexical(context.Context, id.Ref, string, []domain.SearchChunk) error {
	panic("fakestore: Search.IndexLexical is not used by publishing")
}

func (x search) IndexVectors(_ context.Context, b repo.EmbeddingBatch) error {
	defer x.r.lock()()
	key, err := transcripts{x.r}.key(b.AssetID, b.Version)
	if err != nil {
		return err
	}
	if x.r.s.d.Embeddings[key] == nil {
		x.r.s.d.Embeddings[key] = map[string][]domain.EmbeddedChunk{}
	}
	x.r.s.d.Embeddings[key][b.Model] = b.Chunks
	if x.r.s.d.ActiveModel == "" {
		x.r.s.d.ActiveModel = b.Model
	}
	return nil
}

func (x search) SearchLexical(context.Context, repo.LexicalQuery) ([]domain.SearchHit, error) {
	panic("fakestore: Search.SearchLexical is not used by publishing")
}

func (x search) SearchVector(context.Context, repo.VectorQuery) ([]domain.SearchHit, error) {
	panic("fakestore: Search.SearchVector is not used by publishing")
}

func (x search) DeleteAsset(context.Context, id.Ref) error {
	panic("fakestore: Search.DeleteAsset is not used by publishing")
}

func (x search) DeleteModel(context.Context, string) error {
	panic("fakestore: Search.DeleteModel is not used by publishing")
}

func (x search) ActiveModel(context.Context) (string, error) {
	defer x.r.lock()()
	return x.r.s.d.ActiveModel, nil
}

func (x search) SetActiveModel(_ context.Context, m string) error {
	defer x.r.lock()()
	x.r.s.d.ActiveModel = m
	return nil
}

func (x search) HasIndex(context.Context, id.Ref, string, string) (bool, bool, error) {
	panic("fakestore: Search.HasIndex is not used by publishing")
}

// ListEmbeddings implements repo.EmbeddingLister.
func (x search) ListEmbeddings(_ context.Context, ref id.Ref, version, model string) ([]domain.EmbeddedChunk, error) {
	defer x.r.lock()()
	key, err := transcripts{x.r}.key(ref, version)
	if err != nil {
		return nil, err
	}
	return clone(x.r.s.d.Embeddings[key][model]), nil
}

// ---- Approvals ----

type approvals struct{ r repos }

func (a approvals) find(ref id.Ref) (*domain.Approval, error) {
	for _, x := range a.r.s.d.Approvals {
		if string(x.ID) == string(ref) || string(x.ULID) == string(ref) {
			return x, nil
		}
	}
	return nil, errs.NotFound("approval")
}

func (a approvals) Create(_ context.Context, x domain.Approval) error {
	defer a.r.lock()()
	a.r.s.d.Approvals[string(x.ID)] = &x
	return nil
}

func (a approvals) Get(_ context.Context, ref id.Ref) (domain.Approval, error) {
	defer a.r.lock()()
	x, err := a.find(ref)
	if err != nil {
		return domain.Approval{}, err
	}
	return clone(*x), nil
}

func (a approvals) GetForNode(context.Context, id.ULID, string) (domain.Approval, bool, error) {
	panic("fakestore: Approvals.GetForNode is not used by publishing")
}

func (a approvals) List(context.Context, repo.ApprovalFilter, repo.Page) ([]domain.Approval, string, error) {
	panic("fakestore: Approvals.List is not used by publishing")
}

func (a approvals) Decide(_ context.Context, ref id.Ref, d repo.ApprovalDecision) (domain.Approval, error) {
	defer a.r.lock()()
	x, err := a.find(ref)
	if err != nil {
		return domain.Approval{}, err
	}
	if x.Status != domain.ApprovalPending {
		return domain.Approval{}, errs.New(errs.CodeFailedPrecondition, "this approval is already decided")
	}
	x.Status, x.DecidedBy, x.DecidedAt, x.Reason = d.Status, d.DecidedBy, &d.DecidedAt, d.Reason
	return clone(*x), nil
}

func (a approvals) ExpiringBefore(context.Context, time.Time) ([]domain.Approval, error) {
	panic("fakestore: Approvals.ExpiringBefore is not used by publishing")
}

// ---- Publications ----

type publications struct{ r repos }

func (p publications) find(ref id.Ref) (*domain.Publication, error) {
	for _, x := range p.r.s.d.Publications {
		if string(x.ID) == string(ref) || string(x.ULID) == string(ref) {
			return x, nil
		}
	}
	return nil, errs.NotFound("publication")
}

func (p publications) Create(_ context.Context, x domain.Publication) error {
	defer p.r.lock()()
	if err := x.Validate(); err != nil {
		return errs.Wrap(errs.CodeFailedPrecondition, err, err.Error())
	}
	p.r.s.d.Publications[string(x.ID)] = &x
	return nil
}

func (p publications) Get(_ context.Context, ref id.Ref) (domain.Publication, error) {
	defer p.r.lock()()
	x, err := p.find(ref)
	if err != nil {
		return domain.Publication{}, err
	}
	return clone(*x), nil
}

func (p publications) FindByIdempotencyKey(_ context.Context, key string) (domain.Publication, bool, error) {
	defer p.r.lock()()
	for _, x := range p.r.s.d.Publications {
		if x.IdempotencyKey == key {
			return clone(*x), true, nil
		}
	}
	return domain.Publication{}, false, nil
}

func (p publications) List(_ context.Context, f repo.PublicationFilter, _ repo.Page) ([]domain.Publication, string, error) {
	defer p.r.lock()()
	var out []domain.Publication
	var asset string
	if f.AssetID != "" {
		x, err := assets{p.r}.find(f.AssetID)
		if err != nil {
			return nil, "", err
		}
		asset = string(x.ID)
	}
	for _, x := range p.r.s.d.Publications {
		if asset != "" && string(x.AssetID) != asset {
			continue
		}
		if f.Destination != "" && x.Destination != f.Destination {
			continue
		}
		if f.Status != "" && x.Status != f.Status {
			continue
		}
		out = append(out, clone(*x))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Number() < out[j].ID.Number() })
	return out, "", nil
}

func (p publications) Update(_ context.Context, x domain.Publication) error {
	defer p.r.lock()()
	if err := x.Validate(); err != nil {
		return errs.Wrap(errs.CodeFailedPrecondition, err, err.Error())
	}
	if _, err := p.find(id.Ref(x.ID)); err != nil {
		return err
	}
	p.r.s.d.Publications[string(x.ID)] = &x
	return nil
}

func (p publications) ListPublic(_ context.Context) ([]domain.Publication, error) {
	defer p.r.lock()()
	var out []domain.Publication
	for _, x := range p.r.s.d.Publications {
		if x.IsPublic() {
			out = append(out, clone(*x))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Number() < out[j].ID.Number() })
	return out, nil
}

// ---- Audit and outbox: append only, read back with AuditRecords and Events ----

type audit struct{ r repos }

func (a audit) Append(_ context.Context, x domain.AuditRecord) (domain.AuditRecord, error) {
	defer a.r.lock()()
	x.Seq = int64(len(a.r.s.d.Audit) + 1)
	a.r.s.d.Audit = append(a.r.s.d.Audit, x)
	return x, nil
}

func (a audit) List(context.Context, repo.AuditFilter, repo.Page) ([]domain.AuditRecord, string, error) {
	panic("fakestore: Audit.List is not used by publishing")
}

func (a audit) Scan(context.Context, int64, int) ([]domain.AuditRecord, error) {
	panic("fakestore: Audit.Scan is not used by publishing")
}

type outbox struct{ r repos }

func (o outbox) Append(_ context.Context, e event.Envelope) (int64, error) {
	defer o.r.lock()()
	o.r.s.d.Outbox = append(o.r.s.d.Outbox, e)
	return int64(len(o.r.s.d.Outbox)), nil
}

func (o outbox) Pending(context.Context, int) ([]repo.OutboxEntry, error) {
	panic("fakestore: Outbox.Pending is not used by publishing")
}
func (o outbox) MarkDispatched(context.Context, int64) error {
	panic("fakestore: Outbox.MarkDispatched is not used by publishing")
}
func (o outbox) PutDelivery(context.Context, event.Delivery) error {
	panic("fakestore: Outbox.PutDelivery is not used by publishing")
}
func (o outbox) DueDeliveries(context.Context, time.Time, int) ([]event.Delivery, error) {
	panic("fakestore: Outbox.DueDeliveries is not used by publishing")
}
func (o outbox) GetEvent(context.Context, id.ULID) (event.Envelope, error) {
	panic("fakestore: Outbox.GetEvent is not used by publishing")
}
func (o outbox) ListEvents(context.Context, repo.OutboxFilter, repo.Page) ([]event.Envelope, string, error) {
	panic("fakestore: Outbox.ListEvents is not used by publishing")
}
func (o outbox) ListDeliveries(context.Context, repo.DeliveryFilter, repo.Page) ([]event.Delivery, string, error) {
	panic("fakestore: Outbox.ListDeliveries is not used by publishing")
}
func (o outbox) Stream(context.Context, int64, int) ([]repo.OutboxEntry, error) {
	panic("fakestore: Outbox.Stream is not used by publishing")
}

// ---- Test accessors ----

// Events returns every event appended to the outbox, in order.
func (s *Store) Events() []event.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.d.Outbox)
}

// EventsOfType returns the events of one type.
func (s *Store) EventsOfType(t event.Type) []event.Envelope {
	var out []event.Envelope
	for _, e := range s.Events() {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// AuditRecords returns the audit trail.
func (s *Store) AuditRecords() []domain.AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.d.Audit)
}

// HistoryEntries returns asset history entries.
func (s *Store) HistoryEntries() []domain.HistoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.d.History)
}
