// Package memstore is an in-memory repo.TenantStores for this component's tests,
// because component core may not be imported in the parallel stage (plan section 8).
// It follows the behavior core's persistence states: invalid status transitions are
// refused, files and transcript info are derived on asset reads, one approval is
// decided once, and InTx rolls back. It is not used by any binary.
package memstore

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// Stores implements repo.TenantStores.
type Stores struct {
	mu      sync.Mutex
	tenants map[domain.TenantID]*Store
}

var _ repo.TenantStores = (*Stores)(nil)

// New returns stores with the given tenants already created.
func New(tenants ...domain.TenantID) *Stores {
	s := &Stores{tenants: map[domain.TenantID]*Store{}}
	for _, t := range tenants {
		s.tenants[t] = newStore(t)
	}
	return s
}

// Create implements repo.TenantStores.
func (s *Stores) Create(_ context.Context, tenant domain.TenantID) (repo.TenantStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.tenants[tenant]; dup {
		return nil, errs.Newf(errs.CodeConflict, "tenant %s exists", tenant)
	}
	st := newStore(tenant)
	s.tenants[tenant] = st
	return st, nil
}

// ForTenant implements repo.TenantStores.
func (s *Stores) ForTenant(_ context.Context, tenant domain.TenantID) (repo.TenantStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.tenants[tenant]
	if !ok {
		return nil, errs.NotFound("tenant")
	}
	return st, nil
}

// Tenants implements repo.TenantStores.
func (s *Stores) Tenants(context.Context) ([]domain.TenantID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.TenantID, 0, len(s.tenants))
	for t := range s.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Close implements repo.TenantStores.
func (s *Stores) Close() error { return nil }

// Store implements repo.TenantStore.
type Store struct {
	tenant domain.TenantID
	mu     sync.Mutex
	st     *state
	// Fail, when set, makes InTx fail with this error once; for failure tests.
	Fail error
}

type outboxRow struct {
	seq        int64
	env        event.Envelope
	dispatched bool
}

type state struct {
	seq         map[id.Prefix]uint64
	assets      map[id.ULID]domain.Asset
	history     []domain.HistoryEntry
	versions    map[string]domain.AssetVersion
	collections map[string]domain.Collection
	transcripts map[string]domain.Transcript
	analyses    map[string]domain.Analysis
	sensitive   []domain.SensitiveSegment
	lexical     map[string][]domain.SearchChunk
	vectors     map[string]repo.EmbeddingBatch
	activeModel string
	jobs        map[id.ULID]domain.Job
	jobOrder    []id.ULID
	nodes       map[id.ULID]map[string]domain.NodeExecution
	jobEvents   map[id.ULID][]domain.JobEvent
	approvals   map[id.ULID]domain.Approval
	apprOrder   []id.ULID
	pubs        map[id.ULID]domain.Publication
	pubOrder    []id.ULID
	uploads     map[id.ULID]domain.Upload
	audit       []domain.AuditRecord
	outbox      []outboxRow
	deliveries  map[string]event.Delivery
	secrets     map[string]repo.SecretRecord
	budget      *domain.Budget
	usage       []domain.UsageRecord
	override    *time.Time
	idem        map[string]repo.IdempotentResponse
}

func newState() *state {
	return &state{
		seq: map[id.Prefix]uint64{}, assets: map[id.ULID]domain.Asset{}, versions: map[string]domain.AssetVersion{},
		collections: map[string]domain.Collection{}, transcripts: map[string]domain.Transcript{}, analyses: map[string]domain.Analysis{},
		lexical: map[string][]domain.SearchChunk{}, vectors: map[string]repo.EmbeddingBatch{},
		jobs: map[id.ULID]domain.Job{}, nodes: map[id.ULID]map[string]domain.NodeExecution{}, jobEvents: map[id.ULID][]domain.JobEvent{},
		approvals: map[id.ULID]domain.Approval{}, pubs: map[id.ULID]domain.Publication{}, uploads: map[id.ULID]domain.Upload{},
		deliveries: map[string]event.Delivery{}, secrets: map[string]repo.SecretRecord{}, idem: map[string]repo.IdempotentResponse{},
	}
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *state) clone() *state {
	c := *s
	c.seq = copyMap(s.seq)
	c.assets = copyMap(s.assets)
	c.history = append([]domain.HistoryEntry(nil), s.history...)
	c.versions = copyMap(s.versions)
	c.collections = copyMap(s.collections)
	c.transcripts = copyMap(s.transcripts)
	c.analyses = copyMap(s.analyses)
	c.sensitive = append([]domain.SensitiveSegment(nil), s.sensitive...)
	c.lexical = copyMap(s.lexical)
	c.vectors = copyMap(s.vectors)
	c.jobs = copyMap(s.jobs)
	c.jobOrder = append([]id.ULID(nil), s.jobOrder...)
	c.nodes = make(map[id.ULID]map[string]domain.NodeExecution, len(s.nodes))
	for k, v := range s.nodes {
		c.nodes[k] = copyMap(v)
	}
	c.jobEvents = make(map[id.ULID][]domain.JobEvent, len(s.jobEvents))
	for k, v := range s.jobEvents {
		c.jobEvents[k] = append([]domain.JobEvent(nil), v...)
	}
	c.approvals = copyMap(s.approvals)
	c.apprOrder = append([]id.ULID(nil), s.apprOrder...)
	c.pubs = copyMap(s.pubs)
	c.pubOrder = append([]id.ULID(nil), s.pubOrder...)
	c.uploads = copyMap(s.uploads)
	c.audit = append([]domain.AuditRecord(nil), s.audit...)
	c.outbox = append([]outboxRow(nil), s.outbox...)
	c.deliveries = copyMap(s.deliveries)
	c.secrets = copyMap(s.secrets)
	c.usage = append([]domain.UsageRecord(nil), s.usage...)
	c.idem = copyMap(s.idem)
	return &c
}

func newStore(t domain.TenantID) *Store { return &Store{tenant: t, st: newState()} }

// Tenant implements repo.TenantStore.
func (s *Store) Tenant() domain.TenantID { return s.tenant }

// InTx implements repo.TenantStore: all or nothing.
func (s *Store) InTx(_ context.Context, fn func(repo.TenantRepos) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		err := s.Fail
		s.Fail = nil
		return err
	}
	tx := s.st.clone()
	if err := fn(repos{store: s, tx: tx}); err != nil {
		return err
	}
	s.st = tx
	return nil
}

type repos struct {
	store *Store
	tx    *state
}

func (r repos) do(fn func(*state) error) error {
	if r.tx != nil {
		return fn(r.tx)
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return fn(r.store.st)
}

func (s *Store) top() repos { return repos{store: s} }

func (s *Store) IDs() repo.IDAllocator              { return idRepo{s.top()} }
func (s *Store) Assets() repo.AssetRepo             { return assetRepo{s.top()} }
func (s *Store) Versions() repo.VersionRepo         { return versionRepo{s.top()} }
func (s *Store) Collections() repo.CollectionRepo   { return collectionRepo{s.top()} }
func (s *Store) Transcripts() repo.TranscriptRepo   { return transcriptRepo{s.top()} }
func (s *Store) Search() repo.SearchRepo            { return searchRepo{s.top()} }
func (s *Store) Jobs() repo.JobRepo                 { return jobRepo{s.top()} }
func (s *Store) Approvals() repo.ApprovalRepo       { return approvalRepo{s.top()} }
func (s *Store) Publications() repo.PublicationRepo { return publicationRepo{s.top()} }
func (s *Store) Uploads() repo.UploadRepo           { return uploadRepo{s.top()} }
func (s *Store) Audit() repo.AuditRepo              { return auditRepo{s.top()} }
func (s *Store) Outbox() repo.OutboxRepo            { return outboxRepo{s.top()} }
func (s *Store) Secrets() repo.SecretRepo           { return secretRepo{s.top()} }
func (s *Store) Budgets() repo.BudgetRepo           { return budgetRepo{s.top()} }
func (s *Store) Idempotency() repo.IdempotencyRepo  { return idemRepo{s.top()} }

func (r repos) IDs() repo.IDAllocator              { return idRepo{r} }
func (r repos) Assets() repo.AssetRepo             { return assetRepo{r} }
func (r repos) Versions() repo.VersionRepo         { return versionRepo{r} }
func (r repos) Collections() repo.CollectionRepo   { return collectionRepo{r} }
func (r repos) Transcripts() repo.TranscriptRepo   { return transcriptRepo{r} }
func (r repos) Search() repo.SearchRepo            { return searchRepo{r} }
func (r repos) Jobs() repo.JobRepo                 { return jobRepo{r} }
func (r repos) Approvals() repo.ApprovalRepo       { return approvalRepo{r} }
func (r repos) Publications() repo.PublicationRepo { return publicationRepo{r} }
func (r repos) Uploads() repo.UploadRepo           { return uploadRepo{r} }
func (r repos) Audit() repo.AuditRepo              { return auditRepo{r} }
func (r repos) Outbox() repo.OutboxRepo            { return outboxRepo{r} }
func (r repos) Secrets() repo.SecretRepo           { return secretRepo{r} }
func (r repos) Budgets() repo.BudgetRepo           { return budgetRepo{r} }
func (r repos) Idempotency() repo.IdempotencyRepo  { return idemRepo{r} }

// ---- IDs ----

type idRepo struct{ repos }

func (r idRepo) Next(_ context.Context, p id.Prefix) (id.Friendly, error) {
	var out id.Friendly
	err := r.do(func(s *state) error {
		s.seq[p]++
		out = id.Format(p, s.seq[p])
		return nil
	})
	return out, err
}

// ---- Assets ----

type assetRepo struct{ repos }

func vkey(a id.ULID, version string) string { return string(a) + "/" + version }

func (s *state) findAsset(ref id.Ref) (domain.Asset, bool) {
	if a, ok := s.assets[id.ULID(ref)]; ok {
		return a, true
	}
	for _, a := range s.assets {
		if string(a.ID) == string(ref) {
			return a, true
		}
	}
	return domain.Asset{}, false
}

// hydrate derives what core derives on read: files of the current version, the
// transcript info and publications.
func (s *state) hydrate(a domain.Asset) domain.Asset {
	a.SourceOriginal, a.WorkingMaster, a.Proxy, a.Backup, a.Thumbnail, a.Transcript, a.Provenance = nil, nil, nil, nil, nil, nil, nil
	if v, ok := s.versions[vkey(a.ULID, a.CurrentVersion)]; ok {
		for i := range v.Files {
			f := v.Files[i]
			if f.Name != "" {
				continue
			}
			file := f.File
			switch f.Purpose {
			case domain.FileSourceOriginal:
				a.SourceOriginal = &file
			case domain.FileWorkingMaster:
				a.WorkingMaster = &file
			case domain.FileProxy:
				a.Proxy = &file
			case domain.FileBackup:
				a.Backup = &file
			case domain.FileThumbnail:
				a.Thumbnail = &file
			}
		}
		a.Provenance = v.Provenance
	}
	if t, ok := s.transcripts[vkey(a.ULID, a.CurrentVersion)]; ok {
		info := &domain.TranscriptInfo{Format: "json", TimestampBasis: t.TimestampBasis, Engine: t.Engine}
		_, lexical := s.lexical[vkey(a.ULID, a.CurrentVersion)]
		b, vector := s.vectors[vkey(a.ULID, a.CurrentVersion)]
		info.Indexed = lexical && vector
		if vector {
			info.EmbeddingModel = b.Model
		}
		a.Transcript = info
	}
	a.Publications = nil
	for _, u := range s.pubOrder {
		if p := s.pubs[u]; p.AssetID == a.ID {
			a.Publications = append(a.Publications, p)
		}
	}
	return a
}

func (r assetRepo) Create(_ context.Context, a domain.Asset) error {
	return r.do(func(s *state) error {
		if a.ID == "" || a.ULID == "" {
			return errs.New(errs.CodeInvalidArgument, "an asset needs a friendly ID and a ULID")
		}
		if _, dup := s.assets[a.ULID]; dup {
			return errs.New(errs.CodeConflict, "asset exists")
		}
		if a.Status == "" {
			a.Status = domain.AssetRegistered
		}
		if a.Visibility == "" {
			a.Visibility = domain.VisibilityPrivate
		}
		if a.AIProcessing == "" {
			a.AIProcessing = domain.AILocalOnly
		}
		if a.AIProcessing == domain.AIExternalAllowed && a.AIConsent == nil {
			return errs.New(errs.CodeFailedPrecondition, "external_allowed needs a consent record")
		}
		if a.CatalogSync == "" {
			a.CatalogSync = domain.CatalogSyncNone
		}
		if a.CurrentVersion == "" {
			a.CurrentVersion = "v1"
		}
		if a.CreatedAt.IsZero() {
			a.CreatedAt = time.Now().UTC()
		}
		a.UpdatedAt = a.CreatedAt
		s.assets[a.ULID] = a
		return nil
	})
}

func (r assetRepo) Get(_ context.Context, ref id.Ref) (domain.Asset, error) {
	var out domain.Asset
	err := r.do(func(s *state) error {
		a, ok := s.findAsset(ref)
		if !ok {
			return errs.NotFound("asset")
		}
		out = s.hydrate(a)
		return nil
	})
	return out, err
}

func (r assetRepo) List(_ context.Context, f repo.AssetFilter, p repo.Page) ([]domain.Asset, string, error) {
	var out []domain.Asset
	err := r.do(func(s *state) error {
		for _, a := range s.assets {
			if a.DeletedAt != nil && !f.IncludeDeleted {
				continue
			}
			if f.Kind != "" && a.Kind != f.Kind {
				continue
			}
			if f.Status != "" && a.Status != f.Status {
				continue
			}
			if f.Collection != "" && a.Collection != f.Collection {
				continue
			}
			if f.SourceID != "" && string(a.ParentAssetID) != string(f.SourceID) {
				continue
			}
			out = append(out, s.hydrate(a))
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ULID < out[j].ULID })
	return pageOf(out, p)
}

func pageOf[T any](items []T, p repo.Page) ([]T, string, error) {
	start := 0
	if p.Cursor != "" {
		n, err := strconv.Atoi(p.Cursor)
		if err != nil {
			return nil, "", errs.New(errs.CodeInvalidArgument, "bad cursor")
		}
		start = n
	}
	if start > len(items) {
		start = len(items)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 100
	}
	end := start + limit
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	} else {
		end = len(items)
	}
	return items[start:end], next, nil
}

func (r assetRepo) Update(_ context.Context, a domain.Asset) error {
	return r.do(func(s *state) error {
		cur, ok := s.findAsset(id.Ref(a.ULID))
		if !ok {
			return errs.NotFound("asset")
		}
		if !cur.Status.CanTransition(a.Status) {
			return errs.Newf(errs.CodeFailedPrecondition, "asset %s cannot move from %s to %s", cur.ID, cur.Status, a.Status)
		}
		a.UpdatedAt = time.Now().UTC()
		a.CreatedAt = cur.CreatedAt
		s.assets[a.ULID] = a
		return nil
	})
}

func (r assetRepo) AppendHistory(_ context.Context, h domain.HistoryEntry) error {
	return r.do(func(s *state) error {
		s.history = append(s.history, h)
		return nil
	})
}

func (r assetRepo) History(_ context.Context, ref id.Ref, p repo.Page) ([]domain.HistoryEntry, string, error) {
	var out []domain.HistoryEntry
	_ = r.do(func(s *state) error {
		a, ok := s.findAsset(ref)
		if !ok {
			return nil
		}
		for _, h := range s.history {
			if h.AssetID == a.ID {
				out = append(out, h)
			}
		}
		return nil
	})
	return pageOf(out, p)
}

func (r assetRepo) SoftDelete(_ context.Context, ref id.Ref, at time.Time) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(ref)
		if !ok {
			return errs.NotFound("asset")
		}
		if a.DeletedAt == nil {
			a.DeletedAt = &at
			s.assets[a.ULID] = a
		}
		return nil
	})
}

func (r assetRepo) ClipsOverlapping(_ context.Context, source id.Ref, segments []domain.Segment) ([]domain.Asset, error) {
	var out []domain.Asset
	err := r.do(func(s *state) error {
		src, ok := s.findAsset(source)
		if !ok {
			return errs.NotFound("asset")
		}
		for _, a := range s.assets {
			if a.ParentAssetID != src.ID || a.Lineage == nil || a.DeletedAt != nil {
				continue
			}
			hit := false
			for _, have := range a.Lineage.Segments {
				for _, want := range segments {
					if have.StartSeconds < want.EndSeconds && want.StartSeconds < have.EndSeconds {
						hit = true
					}
				}
			}
			if hit {
				out = append(out, a)
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, err
}

// ---- Versions ----

type versionRepo struct{ repos }

func (r versionRepo) Create(_ context.Context, v domain.AssetVersion) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(id.Ref(v.AssetID))
		if !ok {
			return errs.NotFound("asset")
		}
		if _, dup := s.versions[vkey(a.ULID, v.Version)]; dup {
			return errs.New(errs.CodeConflict, "version exists")
		}
		if v.CreatedAt.IsZero() {
			v.CreatedAt = time.Now().UTC()
		}
		s.versions[vkey(a.ULID, v.Version)] = v
		return nil
	})
}

func (r versionRepo) Get(_ context.Context, asset id.Ref, version string) (domain.AssetVersion, error) {
	var out domain.AssetVersion
	err := r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		v, ok := s.versions[vkey(a.ULID, version)]
		if !ok {
			return errs.NotFound("version")
		}
		out = v
		return nil
	})
	return out, err
}

func (r versionRepo) List(_ context.Context, asset id.Ref) ([]domain.AssetVersion, error) {
	var out []domain.AssetVersion
	err := r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		prefix := string(a.ULID) + "/"
		for k, v := range s.versions {
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				out = append(out, v)
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return versionNumber(out[i].Version) < versionNumber(out[j].Version) })
	return out, err
}

func versionNumber(v string) int {
	if len(v) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(v[1:])
	return n
}

func (r versionRepo) PutFile(_ context.Context, asset id.Ref, version string, f domain.VersionFile) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		v, ok := s.versions[vkey(a.ULID, version)]
		if !ok {
			return errs.NotFound("version")
		}
		if v.Immutable {
			return errs.Newf(errs.CodeFailedPrecondition, "version %s is immutable", version)
		}
		files := make([]domain.VersionFile, 0, len(v.Files)+1)
		for _, e := range v.Files {
			if e.Purpose != f.Purpose || e.Name != f.Name {
				files = append(files, e)
			}
		}
		v.Files = append(files, f)
		s.versions[vkey(a.ULID, version)] = v
		return nil
	})
}

func (r versionRepo) MarkImmutable(_ context.Context, asset id.Ref, version string) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		v, ok := s.versions[vkey(a.ULID, version)]
		if !ok {
			return errs.NotFound("version")
		}
		v.Immutable = true
		s.versions[vkey(a.ULID, version)] = v
		return nil
	})
}

func (r versionRepo) SetProvenance(_ context.Context, asset id.Ref, version string, p domain.Provenance) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		v, ok := s.versions[vkey(a.ULID, version)]
		if !ok {
			return errs.NotFound("version")
		}
		v.Provenance = &p
		s.versions[vkey(a.ULID, version)] = v
		return nil
	})
}

// ---- Collections ----

type collectionRepo struct{ repos }

func (r collectionRepo) Get(_ context.Context, name string) (domain.Collection, error) {
	var out domain.Collection
	err := r.do(func(s *state) error {
		c, ok := s.collections[name]
		if !ok {
			return errs.NotFound("collection")
		}
		out = c
		return nil
	})
	return out, err
}

func (r collectionRepo) List(context.Context) ([]domain.Collection, error) {
	var out []domain.Collection
	err := r.do(func(s *state) error {
		for _, c := range s.collections {
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

func (r collectionRepo) Upsert(_ context.Context, c domain.Collection) error {
	return r.do(func(s *state) error {
		s.collections[c.Name] = c
		return nil
	})
}

// ---- Transcripts ----

type transcriptRepo struct{ repos }

func (r transcriptRepo) Save(_ context.Context, t domain.Transcript) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(id.Ref(t.AssetID))
		if !ok {
			return errs.NotFound("asset")
		}
		s.transcripts[vkey(a.ULID, t.Version)] = t
		return nil
	})
}

func (r transcriptRepo) Get(_ context.Context, asset id.Ref, version string) (domain.Transcript, error) {
	var out domain.Transcript
	err := r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		t, ok := s.transcripts[vkey(a.ULID, version)]
		if !ok {
			return errs.NotFound("transcript")
		}
		out = t
		return nil
	})
	return out, err
}

func (r transcriptRepo) SaveAnalysis(_ context.Context, an domain.Analysis) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(id.Ref(an.AssetID))
		if !ok {
			return errs.NotFound("asset")
		}
		s.analyses[vkey(a.ULID, an.Version)] = an
		return nil
	})
}

func (r transcriptRepo) Analysis(_ context.Context, asset id.Ref, version string) (domain.Analysis, error) {
	var out domain.Analysis
	err := r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		an, ok := s.analyses[vkey(a.ULID, version)]
		if !ok {
			return errs.NotFound("analysis")
		}
		out = an
		return nil
	})
	return out, err
}

func (r transcriptRepo) MarkSensitive(_ context.Context, m domain.SensitiveSegment) error {
	return r.do(func(s *state) error {
		s.sensitive = append(s.sensitive, m)
		return nil
	})
}

func (r transcriptRepo) UnmarkSensitive(_ context.Context, asset id.Ref, version string, start, end float64) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		var keep []domain.SensitiveSegment
		for _, m := range s.sensitive {
			if m.AssetID == a.ID && m.Version == version && m.StartSeconds == start && m.EndSeconds == end {
				continue
			}
			keep = append(keep, m)
		}
		s.sensitive = keep
		return nil
	})
}

func (r transcriptRepo) Sensitive(_ context.Context, asset id.Ref, version string) ([]domain.SensitiveSegment, error) {
	var out []domain.SensitiveSegment
	err := r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		for _, m := range s.sensitive {
			if m.AssetID == a.ID && (version == "" || m.Version == version) {
				out = append(out, m)
			}
		}
		return nil
	})
	return out, err
}

// ---- Search ----

type searchRepo struct{ repos }

func (r searchRepo) IndexLexical(_ context.Context, asset id.Ref, version string, chunks []domain.SearchChunk) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		s.lexical[vkey(a.ULID, version)] = chunks
		return nil
	})
}

func (r searchRepo) IndexVectors(_ context.Context, b repo.EmbeddingBatch) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(b.AssetID)
		if !ok {
			return errs.NotFound("asset")
		}
		s.vectors[vkey(a.ULID, b.Version)] = b
		if s.activeModel == "" {
			s.activeModel = b.Model
		}
		return nil
	})
}

func (r searchRepo) SearchLexical(context.Context, repo.LexicalQuery) ([]domain.SearchHit, error) {
	return nil, nil
}
func (r searchRepo) SearchVector(context.Context, repo.VectorQuery) ([]domain.SearchHit, error) {
	return nil, nil
}

func (r searchRepo) DeleteAsset(_ context.Context, asset id.Ref) error {
	return r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		prefix := string(a.ULID) + "/"
		for k := range s.lexical {
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				delete(s.lexical, k)
			}
		}
		for k := range s.vectors {
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				delete(s.vectors, k)
			}
		}
		return nil
	})
}

func (r searchRepo) DeleteModel(context.Context, string) error { return nil }
func (r searchRepo) ActiveModel(context.Context) (string, error) {
	var out string
	err := r.do(func(s *state) error { out = s.activeModel; return nil })
	return out, err
}
func (r searchRepo) SetActiveModel(_ context.Context, model string) error {
	return r.do(func(s *state) error { s.activeModel = model; return nil })
}
func (r searchRepo) HasIndex(_ context.Context, asset id.Ref, version, model string) (bool, bool, error) {
	var lexical, vector bool
	err := r.do(func(s *state) error {
		a, ok := s.findAsset(asset)
		if !ok {
			return errs.NotFound("asset")
		}
		_, lexical = s.lexical[vkey(a.ULID, version)]
		b, has := s.vectors[vkey(a.ULID, version)]
		vector = has && (model == "" || b.Model == model)
		return nil
	})
	return lexical, vector, err
}

// ---- Jobs ----

type jobRepo struct{ repos }

func (s *state) findJob(ref id.Ref) (domain.Job, bool) {
	if j, ok := s.jobs[id.ULID(ref)]; ok {
		return j, true
	}
	for _, j := range s.jobs {
		if string(j.ID) == string(ref) {
			return j, true
		}
	}
	return domain.Job{}, false
}

func (r jobRepo) Create(_ context.Context, j domain.Job) error {
	return r.do(func(s *state) error {
		if j.ID == "" || j.ULID == "" {
			return errs.New(errs.CodeInvalidArgument, "a job needs IDs")
		}
		if _, dup := s.jobs[j.ULID]; dup {
			return errs.New(errs.CodeConflict, "job exists")
		}
		if j.IdempotencyKey != "" {
			for _, u := range s.jobOrder {
				if s.jobs[u].IdempotencyKey == j.IdempotencyKey {
					return errs.New(errs.CodeConflict, "idempotency key exists")
				}
			}
		}
		if j.CreatedAt.IsZero() {
			j.CreatedAt = time.Now().UTC()
		}
		s.jobs[j.ULID] = j
		s.jobOrder = append(s.jobOrder, j.ULID)
		return nil
	})
}

func (r jobRepo) Get(_ context.Context, ref id.Ref) (domain.Job, error) {
	var out domain.Job
	err := r.do(func(s *state) error {
		j, ok := s.findJob(ref)
		if !ok {
			return errs.NotFound("job")
		}
		out = j
		return nil
	})
	return out, err
}

func (r jobRepo) GetByULID(ctx context.Context, job id.ULID) (domain.Job, error) {
	return r.Get(ctx, id.Ref(job))
}

func (r jobRepo) FindByIdempotencyKey(_ context.Context, key string) (domain.Job, bool, error) {
	var out domain.Job
	found := false
	err := r.do(func(s *state) error {
		for _, u := range s.jobOrder {
			if j := s.jobs[u]; j.IdempotencyKey == key && key != "" {
				out, found = j, true
				return nil
			}
		}
		return nil
	})
	return out, found, err
}

func (r jobRepo) List(_ context.Context, f repo.JobFilter, p repo.Page) ([]domain.Job, string, error) {
	var out []domain.Job
	_ = r.do(func(s *state) error {
		for i := len(s.jobOrder) - 1; i >= 0; i-- {
			j := s.jobs[s.jobOrder[i]]
			if f.Status != "" && j.Status != f.Status {
				continue
			}
			if f.WorkflowID != "" && j.WorkflowID != f.WorkflowID {
				continue
			}
			if f.AssetID != "" && string(j.AssetID) != string(f.AssetID) {
				continue
			}
			if f.ParentJob != "" {
				parent, ok := s.findJob(f.ParentJob)
				if !ok || j.Trigger.ParentJobID != string(parent.ULID) {
					continue
				}
			}
			out = append(out, j)
		}
		return nil
	})
	return pageOf(out, p)
}

func (r jobRepo) Update(_ context.Context, j domain.Job) error {
	return r.do(func(s *state) error {
		cur, ok := s.jobs[j.ULID]
		if !ok {
			return errs.NotFound("job")
		}
		if cur.Status != j.Status && !cur.Status.CanTransition(j.Status) {
			return errs.Newf(errs.CodeFailedPrecondition, "job %s cannot move from %s to %s", cur.ID, cur.Status, j.Status)
		}
		cur.Status, cur.StatusReason, cur.Inputs, cur.Outputs, cur.AssetID = j.Status, j.StatusReason, j.Inputs, j.Outputs, j.AssetID
		cur.ProgressPercent, cur.Attention, cur.Error, cur.StartedAt, cur.FinishedAt = j.ProgressPercent, j.Attention, j.Error, j.StartedAt, j.FinishedAt
		s.jobs[j.ULID] = cur
		return nil
	})
}

func (r jobRepo) ListActive(context.Context) ([]domain.Job, error) {
	var out []domain.Job
	err := r.do(func(s *state) error {
		for _, u := range s.jobOrder {
			if j := s.jobs[u]; !j.Status.Terminal() {
				out = append(out, j)
			}
		}
		return nil
	})
	return out, err
}

func (r jobRepo) PutNode(_ context.Context, n domain.NodeExecution) error {
	return r.do(func(s *state) error {
		if len(n.Output) > domain.MaxNodeOutputBytes {
			return errs.New(errs.CodeOutputTooLarge, "node output too large")
		}
		if _, ok := s.jobs[n.JobULID]; !ok {
			return errs.NotFound("job")
		}
		m := s.nodes[n.JobULID]
		if m == nil {
			m = map[string]domain.NodeExecution{}
			s.nodes[n.JobULID] = m
		}
		m[n.NodeID] = n
		return nil
	})
}

func (r jobRepo) GetNode(_ context.Context, job id.ULID, nodeID string) (domain.NodeExecution, error) {
	var out domain.NodeExecution
	err := r.do(func(s *state) error {
		n, ok := s.nodes[job][nodeID]
		if !ok {
			return errs.NotFound("node execution")
		}
		out = n
		return nil
	})
	return out, err
}

func (r jobRepo) GetNodeByTask(_ context.Context, taskID string) (domain.NodeExecution, error) {
	var out domain.NodeExecution
	err := r.do(func(s *state) error {
		for _, m := range s.nodes {
			for _, n := range m {
				if n.TaskID == taskID && taskID != "" {
					out = n
					return nil
				}
			}
		}
		return errs.NotFound("task")
	})
	return out, err
}

func (r jobRepo) ListNodes(_ context.Context, job id.ULID) ([]domain.NodeExecution, error) {
	var out []domain.NodeExecution
	err := r.do(func(s *state) error {
		for _, n := range s.nodes[job] {
			out = append(out, n)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, err
}

func (r jobRepo) AppendEvent(_ context.Context, e domain.JobEvent) (int64, error) {
	var seq int64
	err := r.do(func(s *state) error {
		j, ok := s.findJob(id.Ref(e.JobID))
		if !ok {
			return errs.NotFound("job")
		}
		seq = int64(len(s.jobEvents[j.ULID])) + 1
		e.Seq = seq
		if e.At.IsZero() {
			e.At = time.Now().UTC()
		}
		s.jobEvents[j.ULID] = append(s.jobEvents[j.ULID], e)
		return nil
	})
	return seq, err
}

func (r jobRepo) ListEvents(_ context.Context, job id.Ref, afterSeq int64, limit int) ([]domain.JobEvent, error) {
	var out []domain.JobEvent
	err := r.do(func(s *state) error {
		j, ok := s.findJob(job)
		if !ok {
			return errs.NotFound("job")
		}
		for _, e := range s.jobEvents[j.ULID] {
			if e.Seq > afterSeq {
				out = append(out, e)
				if limit > 0 && len(out) == limit {
					break
				}
			}
		}
		return nil
	})
	return out, err
}

func (r jobRepo) DeleteFinishedBefore(context.Context, time.Time) (int64, error) { return 0, nil }

// ---- Approvals ----

type approvalRepo struct{ repos }

func (s *state) findApproval(ref id.Ref) (domain.Approval, bool) {
	if a, ok := s.approvals[id.ULID(ref)]; ok {
		return a, true
	}
	for _, a := range s.approvals {
		if string(a.ID) == string(ref) {
			return a, true
		}
	}
	return domain.Approval{}, false
}

func (r approvalRepo) Create(_ context.Context, a domain.Approval) error {
	return r.do(func(s *state) error {
		if a.ID == "" || a.ULID == "" || a.Kind == "" {
			return errs.New(errs.CodeInvalidArgument, "an approval needs IDs and a kind")
		}
		if a.Status == "" {
			a.Status = domain.ApprovalPending
		}
		if a.CreatedAt.IsZero() {
			a.CreatedAt = time.Now().UTC()
		}
		s.approvals[a.ULID] = a
		s.apprOrder = append(s.apprOrder, a.ULID)
		return nil
	})
}

func (r approvalRepo) Get(_ context.Context, ref id.Ref) (domain.Approval, error) {
	var out domain.Approval
	err := r.do(func(s *state) error {
		a, ok := s.findApproval(ref)
		if !ok {
			return errs.NotFound("approval")
		}
		out = a
		return nil
	})
	return out, err
}

func (r approvalRepo) GetForNode(_ context.Context, job id.ULID, nodeID string) (domain.Approval, bool, error) {
	var out domain.Approval
	found := false
	err := r.do(func(s *state) error {
		j, ok := s.jobs[job]
		if !ok {
			return nil
		}
		for i := len(s.apprOrder) - 1; i >= 0; i-- {
			if a := s.approvals[s.apprOrder[i]]; a.JobID == j.ID && a.NodeID == nodeID {
				out, found = a, true
				return nil
			}
		}
		return nil
	})
	return out, found, err
}

func (r approvalRepo) List(_ context.Context, f repo.ApprovalFilter, p repo.Page) ([]domain.Approval, string, error) {
	var out []domain.Approval
	_ = r.do(func(s *state) error {
		for _, u := range s.apprOrder {
			a := s.approvals[u]
			if f.Status != "" && a.Status != f.Status {
				continue
			}
			if f.Kind != "" && a.Kind != f.Kind {
				continue
			}
			if f.AssetID != "" && string(a.AssetID) != string(f.AssetID) {
				continue
			}
			out = append(out, a)
		}
		return nil
	})
	return pageOf(out, p)
}

func (r approvalRepo) Decide(_ context.Context, ref id.Ref, d repo.ApprovalDecision) (domain.Approval, error) {
	var out domain.Approval
	err := r.do(func(s *state) error {
		a, ok := s.findApproval(ref)
		if !ok {
			return errs.NotFound("approval")
		}
		if a.Status != domain.ApprovalPending {
			return errs.Newf(errs.CodeFailedPrecondition, "approval %s was already decided", a.ID)
		}
		a.Status, a.DecidedBy, a.Reason = d.Status, d.DecidedBy, d.Reason
		at := d.DecidedAt
		a.DecidedAt = &at
		if d.Segments != nil {
			a.Segments = d.Segments
		}
		s.approvals[a.ULID] = a
		out = a
		return nil
	})
	return out, err
}

func (r approvalRepo) ExpiringBefore(_ context.Context, t time.Time) ([]domain.Approval, error) {
	var out []domain.Approval
	err := r.do(func(s *state) error {
		for _, u := range s.apprOrder {
			a := s.approvals[u]
			if a.Status == domain.ApprovalPending && a.ExpiresAt != nil && !a.ExpiresAt.After(t) {
				out = append(out, a)
			}
		}
		return nil
	})
	return out, err
}

// ---- Publications ----

type publicationRepo struct{ repos }

func (s *state) findPub(ref id.Ref) (domain.Publication, bool) {
	if p, ok := s.pubs[id.ULID(ref)]; ok {
		return p, true
	}
	for _, p := range s.pubs {
		if string(p.ID) == string(ref) {
			return p, true
		}
	}
	return domain.Publication{}, false
}

func (r publicationRepo) Create(_ context.Context, p domain.Publication) error {
	return r.do(func(s *state) error {
		s.pubs[p.ULID] = p
		s.pubOrder = append(s.pubOrder, p.ULID)
		return nil
	})
}

func (r publicationRepo) Get(_ context.Context, ref id.Ref) (domain.Publication, error) {
	var out domain.Publication
	err := r.do(func(s *state) error {
		p, ok := s.findPub(ref)
		if !ok {
			return errs.NotFound("publication")
		}
		out = p
		return nil
	})
	return out, err
}

func (r publicationRepo) FindByIdempotencyKey(_ context.Context, key string) (domain.Publication, bool, error) {
	var out domain.Publication
	found := false
	err := r.do(func(s *state) error {
		for _, u := range s.pubOrder {
			if p := s.pubs[u]; p.IdempotencyKey == key && key != "" {
				out, found = p, true
			}
		}
		return nil
	})
	return out, found, err
}

func (r publicationRepo) List(_ context.Context, f repo.PublicationFilter, pg repo.Page) ([]domain.Publication, string, error) {
	var out []domain.Publication
	_ = r.do(func(s *state) error {
		asset := id.Friendly("")
		if f.AssetID != "" {
			if a, ok := s.findAsset(f.AssetID); ok {
				asset = a.ID
			} else {
				return nil
			}
		}
		for _, u := range s.pubOrder {
			p := s.pubs[u]
			if f.AssetID != "" && p.AssetID != asset {
				continue
			}
			if f.Destination != "" && p.Destination != f.Destination {
				continue
			}
			if f.Status != "" && p.Status != f.Status {
				continue
			}
			out = append(out, p)
		}
		return nil
	})
	return pageOf(out, pg)
}

func (r publicationRepo) Update(_ context.Context, p domain.Publication) error {
	if err := p.Validate(); err != nil {
		return errs.Wrap(errs.CodeFailedPrecondition, err, err.Error())
	}
	return r.do(func(s *state) error {
		if _, ok := s.pubs[p.ULID]; !ok {
			return errs.NotFound("publication")
		}
		s.pubs[p.ULID] = p
		return nil
	})
}

func (r publicationRepo) ListPublic(context.Context) ([]domain.Publication, error) {
	var out []domain.Publication
	err := r.do(func(s *state) error {
		for _, u := range s.pubOrder {
			if p := s.pubs[u]; p.IsPublic() {
				out = append(out, p)
			}
		}
		return nil
	})
	return out, err
}

// ---- Uploads ----

type uploadRepo struct{ repos }

func (s *state) findUpload(ref id.Ref) (domain.Upload, bool) {
	if u, ok := s.uploads[id.ULID(ref)]; ok {
		return u, true
	}
	for _, u := range s.uploads {
		if string(u.ID) == string(ref) {
			return u, true
		}
	}
	return domain.Upload{}, false
}

func (r uploadRepo) Create(_ context.Context, u domain.Upload) error {
	return r.do(func(s *state) error { s.uploads[u.ULID] = u; return nil })
}

func (r uploadRepo) Get(_ context.Context, ref id.Ref) (domain.Upload, error) {
	var out domain.Upload
	err := r.do(func(s *state) error {
		u, ok := s.findUpload(ref)
		if !ok {
			return errs.NotFound("upload")
		}
		out = u
		return nil
	})
	return out, err
}

func (r uploadRepo) Update(_ context.Context, u domain.Upload) error {
	return r.do(func(s *state) error { s.uploads[u.ULID] = u; return nil })
}

func (r uploadRepo) ListExpired(context.Context, time.Time) ([]domain.Upload, error) { return nil, nil }

func (r uploadRepo) Delete(_ context.Context, ref id.Ref) error {
	return r.do(func(s *state) error {
		u, ok := s.findUpload(ref)
		if !ok {
			return errs.NotFound("upload")
		}
		delete(s.uploads, u.ULID)
		return nil
	})
}

// ---- Audit ----

type auditRepo struct{ repos }

func (r auditRepo) Append(_ context.Context, a domain.AuditRecord) (domain.AuditRecord, error) {
	err := r.do(func(s *state) error {
		a.Seq = int64(len(s.audit)) + 1
		s.audit = append(s.audit, a)
		return nil
	})
	return a, err
}

func (r auditRepo) List(_ context.Context, f repo.AuditFilter, p repo.Page) ([]domain.AuditRecord, string, error) {
	var out []domain.AuditRecord
	_ = r.do(func(s *state) error {
		for _, a := range s.audit {
			if f.Action != "" && a.Action != f.Action {
				continue
			}
			out = append(out, a)
		}
		return nil
	})
	return pageOf(out, p)
}

func (r auditRepo) Scan(_ context.Context, afterSeq int64, limit int) ([]domain.AuditRecord, error) {
	var out []domain.AuditRecord
	err := r.do(func(s *state) error {
		for _, a := range s.audit {
			if a.Seq > afterSeq {
				out = append(out, a)
				if limit > 0 && len(out) == limit {
					break
				}
			}
		}
		return nil
	})
	return out, err
}

// ---- Outbox ----

type outboxRepo struct{ repos }

func (r outboxRepo) Append(_ context.Context, e event.Envelope) (int64, error) {
	var seq int64
	err := r.do(func(s *state) error {
		if e.Type == "" {
			return errs.New(errs.CodeInvalidArgument, "an event needs a type")
		}
		for _, row := range s.outbox {
			if row.env.EventID == e.EventID && e.EventID != "" {
				return errs.New(errs.CodeConflict, "event exists")
			}
		}
		if e.EventID == "" {
			e.EventID = id.New()
		}
		seq = int64(len(s.outbox)) + 1
		s.outbox = append(s.outbox, outboxRow{seq: seq, env: e})
		return nil
	})
	return seq, err
}

func (r outboxRepo) Pending(_ context.Context, limit int) ([]repo.OutboxEntry, error) {
	var out []repo.OutboxEntry
	err := r.do(func(s *state) error {
		for _, row := range s.outbox {
			if !row.dispatched {
				out = append(out, repo.OutboxEntry{Seq: row.seq, Event: row.env})
				if limit > 0 && len(out) == limit {
					break
				}
			}
		}
		return nil
	})
	return out, err
}

func (r outboxRepo) MarkDispatched(_ context.Context, seq int64) error {
	return r.do(func(s *state) error {
		for i := range s.outbox {
			if s.outbox[i].seq == seq {
				s.outbox[i].dispatched = true
				return nil
			}
		}
		return errs.NotFound("event")
	})
}

func (r outboxRepo) PutDelivery(_ context.Context, d event.Delivery) error {
	return r.do(func(s *state) error {
		s.deliveries[string(d.EventID)+"/"+d.Subscriber] = d
		return nil
	})
}

func (r outboxRepo) DueDeliveries(_ context.Context, now time.Time, limit int) ([]event.Delivery, error) {
	var out []event.Delivery
	err := r.do(func(s *state) error {
		keys := make([]string, 0, len(s.deliveries))
		for k := range s.deliveries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			d := s.deliveries[k]
			due := d.NextAttempt == nil || !d.NextAttempt.After(now)
			if d.Status == event.DeliveryPending && due || d.Status == event.DeliveryFailed && d.NextAttempt != nil && due {
				out = append(out, d)
				if limit > 0 && len(out) == limit {
					break
				}
			}
		}
		return nil
	})
	return out, err
}

func (r outboxRepo) GetEvent(_ context.Context, eventID id.ULID) (event.Envelope, error) {
	var out event.Envelope
	err := r.do(func(s *state) error {
		for _, row := range s.outbox {
			if row.env.EventID == eventID {
				out = row.env
				return nil
			}
		}
		return errs.NotFound("event")
	})
	return out, err
}

func (r outboxRepo) ListEvents(_ context.Context, f repo.OutboxFilter, p repo.Page) ([]event.Envelope, string, error) {
	var out []event.Envelope
	_ = r.do(func(s *state) error {
		for _, row := range s.outbox {
			if f.Type != "" && row.env.Type != f.Type {
				continue
			}
			if f.Since != nil && row.env.OccurredAt.Before(*f.Since) {
				continue
			}
			out = append(out, row.env)
		}
		return nil
	})
	return pageOf(out, p)
}

func (r outboxRepo) ListDeliveries(_ context.Context, f repo.DeliveryFilter, p repo.Page) ([]event.Delivery, string, error) {
	var out []event.Delivery
	_ = r.do(func(s *state) error {
		keys := make([]string, 0, len(s.deliveries))
		for k := range s.deliveries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			d := s.deliveries[k]
			if f.Status != "" && d.Status != f.Status {
				continue
			}
			if f.Subscriber != "" && d.Subscriber != f.Subscriber {
				continue
			}
			out = append(out, d)
		}
		return nil
	})
	return pageOf(out, p)
}

func (r outboxRepo) Stream(_ context.Context, afterSeq int64, limit int) ([]repo.OutboxEntry, error) {
	var out []repo.OutboxEntry
	err := r.do(func(s *state) error {
		for _, row := range s.outbox {
			if row.seq > afterSeq {
				out = append(out, repo.OutboxEntry{Seq: row.seq, Event: row.env})
				if limit > 0 && len(out) == limit {
					break
				}
			}
		}
		return nil
	})
	return out, err
}

// ---- Secrets, budgets, idempotency ----

type secretRepo struct{ repos }

func (r secretRepo) Put(_ context.Context, rec repo.SecretRecord) error {
	return r.do(func(s *state) error { s.secrets[rec.Meta.Name] = rec; return nil })
}

func (r secretRepo) Get(_ context.Context, name string) (repo.SecretRecord, error) {
	var out repo.SecretRecord
	err := r.do(func(s *state) error {
		rec, ok := s.secrets[name]
		if !ok {
			return errs.NotFound("secret")
		}
		out = rec
		return nil
	})
	return out, err
}

func (r secretRepo) List(context.Context) ([]domain.SecretMeta, error) {
	var out []domain.SecretMeta
	err := r.do(func(s *state) error {
		for _, rec := range s.secrets {
			out = append(out, rec.Meta)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

func (r secretRepo) Delete(_ context.Context, name string) error {
	return r.do(func(s *state) error { delete(s.secrets, name); return nil })
}

func (r secretRepo) TouchUsed(context.Context, string, time.Time) error { return nil }

type budgetRepo struct{ repos }

func (r budgetRepo) Get(context.Context) (domain.Budget, bool, error) {
	var out domain.Budget
	found := false
	err := r.do(func(s *state) error {
		if s.budget != nil {
			out, found = *s.budget, true
		}
		return nil
	})
	return out, found, err
}

func (r budgetRepo) Set(_ context.Context, b domain.Budget) error {
	return r.do(func(s *state) error { s.budget = &b; return nil })
}

func (r budgetRepo) RecordUsage(_ context.Context, u domain.UsageRecord) error {
	return r.do(func(s *state) error { s.usage = append(s.usage, u); return nil })
}

func (r budgetRepo) SpentMinor(context.Context, string) (int64, error) {
	var total int64
	err := r.do(func(s *state) error {
		for _, u := range s.usage {
			total += u.EstimatedMinor
		}
		return nil
	})
	return total, err
}

func (r budgetRepo) ListUsage(_ context.Context, _ string, p repo.Page) ([]domain.UsageRecord, string, error) {
	var out []domain.UsageRecord
	_ = r.do(func(s *state) error { out = append(out, s.usage...); return nil })
	return pageOf(out, p)
}

func (r budgetRepo) SetOverride(_ context.Context, until time.Time, _, _ string) error {
	return r.do(func(s *state) error { s.override = &until; return nil })
}

func (r budgetRepo) Override(context.Context) (*time.Time, error) {
	var out *time.Time
	err := r.do(func(s *state) error { out = s.override; return nil })
	return out, err
}

type idemRepo struct{ repos }

func (r idemRepo) Get(_ context.Context, key string) (repo.IdempotentResponse, bool, error) {
	var out repo.IdempotentResponse
	found := false
	err := r.do(func(s *state) error {
		out, found = s.idem[key]
		return nil
	})
	return out, found, err
}

func (r idemRepo) Put(_ context.Context, v repo.IdempotentResponse) error {
	return r.do(func(s *state) error { s.idem[v.Key] = v; return nil })
}

func (r idemRepo) DeleteBefore(context.Context, time.Time) (int64, error) { return 0, nil }

// RawJSON is a small helper for tests that build documents.
func RawJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// SetCreatedAt backdates an asset, for retention tests.
func SetCreatedAt(store repo.TenantStore, asset id.ULID, at time.Time) {
	s := store.(*Store)
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.st.assets[asset]
	a.CreatedAt = at
	s.st.assets[asset] = a
}
