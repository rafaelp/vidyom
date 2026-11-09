package persistence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// tenantStores opens tenant databases and caches their handles.
type tenantStores struct {
	opts Options

	mu     sync.Mutex
	open   map[domain.TenantID]*tenantStore
	closed bool
}

var _ repo.TenantStores = (*tenantStores)(nil)

func newTenantStores(opts Options) *tenantStores {
	return &tenantStores{opts: opts, open: map[domain.TenantID]*tenantStore{}}
}

func (s *tenantStores) dbPath(tenant domain.TenantID) string {
	return filepath.Join(s.opts.DataDir, tenantsDir, string(tenant), tenantDBName)
}

// Create initializes a new tenant database. A tenant that already has one is a
// conflict; the caller decides whether to adopt it (I114).
func (s *tenantStores) Create(ctx context.Context, tenant domain.TenantID) (repo.TenantStore, error) {
	if err := tenant.Validate(); err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid tenant ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errs.New(errs.CodeUnavailable, "persistence is closed")
	}
	path := s.dbPath(tenant)
	if _, err := os.Stat(path); err == nil {
		return nil, errs.Newf(errs.CodeConflict, "tenant %s already has a database", tenant)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat tenant database: %w", err)
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	d, err := openDatabase(ctx, path, kindTenant, s.opts)
	if err != nil {
		return nil, err
	}
	err = d.inTx(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('tenant_id', ?)`, string(tenant))
		return mapErr(err)
	})
	if err != nil {
		d.close()
		return nil, err
	}
	store := newTenantStore(d, tenant)
	s.open[tenant] = store
	return store, nil
}

// ForTenant opens an existing tenant database. An unknown or malformed tenant is
// not found, so no request creates one and the header cannot traverse paths.
func (s *tenantStores) ForTenant(ctx context.Context, tenant domain.TenantID) (repo.TenantStore, error) {
	if err := tenant.Validate(); err != nil {
		return nil, errs.NotFound("tenant")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errs.New(errs.CodeUnavailable, "persistence is closed")
	}
	if store, ok := s.open[tenant]; ok {
		return store, nil
	}
	path := s.dbPath(tenant)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errs.NotFound("tenant")
		}
		return nil, fmt.Errorf("stat tenant database: %w", err)
	}
	d, err := openDatabase(ctx, path, kindTenant, s.opts)
	if err != nil {
		return nil, err
	}
	var stored string
	if err := d.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'tenant_id'`).Scan(&stored); err != nil {
		d.close()
		return nil, errs.Wrap(errs.CodeFailedPrecondition, err, "the tenant database does not record its tenant")
	}
	if stored != string(tenant) {
		d.close()
		return nil, errs.Newf(errs.CodeFailedPrecondition, "the database in the directory of tenant %s belongs to another tenant", tenant)
	}
	store := newTenantStore(d, tenant)
	s.open[tenant] = store
	return store, nil
}

// Tenants lists tenants with a database, for startup reconciliation (D4).
func (s *tenantStores) Tenants(ctx context.Context) ([]domain.TenantID, error) {
	entries, err := os.ReadDir(filepath.Join(s.opts.DataDir, tenantsDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list tenant databases: %w", err)
	}
	var out []domain.TenantID
	for _, e := range entries {
		tenant := domain.TenantID(e.Name())
		if !e.IsDir() || tenant.Validate() != nil {
			continue
		}
		if _, err := os.Stat(s.dbPath(tenant)); err == nil {
			out = append(out, tenant)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (s *tenantStores) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var all []error
	for tenant, store := range s.open {
		all = append(all, store.d.close())
		delete(s.open, tenant)
	}
	return errors.Join(all...)
}

// tenantStore is one tenant.db.
type tenantStore struct {
	tenantRepos
}

// tenantRepos binds the repositories to a connection or a transaction.
type tenantRepos struct {
	conn
	tenant domain.TenantID
}

var (
	_ repo.TenantStore      = (*tenantStore)(nil)
	_ repo.OutboxMaintainer = outboxRepo{}
)

func newTenantStore(d *database, tenant domain.TenantID) *tenantStore {
	return &tenantStore{tenantRepos{conn{d: d}, tenant}}
}

func (s *tenantStore) Tenant() domain.TenantID { return s.tenant }

func (s *tenantStore) InTx(ctx context.Context, fn func(repo.TenantRepos) error) error {
	return s.d.inTx(ctx, func(q querier) error {
		return fn(tenantRepos{conn{d: s.d, tx: q}, s.tenant})
	})
}

func (r tenantRepos) IDs() repo.IDAllocator            { return idAllocator{r.conn} }
func (r tenantRepos) Assets() repo.AssetRepo           { return assetRepo{r.conn, r.tenant} }
func (r tenantRepos) Versions() repo.VersionRepo       { return versionRepo{r.conn, r.tenant} }
func (r tenantRepos) Collections() repo.CollectionRepo { return collectionRepo{r.conn, r.tenant} }
func (r tenantRepos) Transcripts() repo.TranscriptRepo { return transcriptRepo{r.conn} }
func (r tenantRepos) Search() repo.SearchRepo          { return searchRepo{r.conn} }
func (r tenantRepos) Jobs() repo.JobRepo               { return jobRepo{r.conn, r.tenant} }
func (r tenantRepos) Approvals() repo.ApprovalRepo     { return approvalRepo{r.conn, r.tenant} }
func (r tenantRepos) Publications() repo.PublicationRepo {
	return publicationRepo{r.conn, r.tenant}
}
func (r tenantRepos) Uploads() repo.UploadRepo          { return uploadRepo{r.conn, r.tenant} }
func (r tenantRepos) Audit() repo.AuditRepo             { return auditRepo{r.conn, r.tenant} }
func (r tenantRepos) Outbox() repo.OutboxRepo           { return outboxRepo{r.conn, r.tenant} }
func (r tenantRepos) Secrets() repo.SecretRepo          { return secretRepo{r.conn, r.tenant} }
func (r tenantRepos) Budgets() repo.BudgetRepo          { return budgetRepo{r.conn, r.tenant} }
func (r tenantRepos) Idempotency() repo.IdempotencyRepo { return idempotencyRepo{r.conn} }

// ---- Friendly IDs ----

type idAllocator struct{ conn }

// Next allocates a friendly ID transactionally (R5, D46). CLIENT numbering is
// system-wide and lives in system.db (I14), so it is refused here.
func (r idAllocator) Next(ctx context.Context, prefix id.Prefix) (id.Friendly, error) {
	known := false
	for _, p := range id.Prefixes {
		if p == prefix {
			known = true
			break
		}
	}
	if !known {
		return "", invalidArg("unknown ID prefix %q", prefix)
	}
	if prefix == id.PrefixClient {
		return "", invalidArg("CLIENT IDs are system-wide: use the device repository")
	}
	var out id.Friendly
	err := r.write(ctx, func(q querier) error {
		var err error
		out, err = nextSequence(ctx, q, prefix)
		return err
	})
	return out, err
}
