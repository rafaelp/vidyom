package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/persistence/repotest"
	"github.com/rafaelp/vidyom/internal/persistence/sqlitedriver"
)

func openStores(t *testing.T) *Stores {
	t.Helper()
	s, err := Open(context.Background(), Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The whole conformance suite runs against the real driver: SQLite, FTS5, sqlite-vec.
func TestConformance(t *testing.T) {
	repotest.Run(t, func(t *testing.T) (repo.SystemStore, repo.TenantStores) {
		s := openStores(t)
		return s.System, s.Tenants
	})
}

func TestLayoutAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(context.Background(), Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Tenants.Create(context.Background(), "acme"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Tenants.ForTenant(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	// A committed write keeps the -wal and -shm files around while the handle is open.
	if _, err := st.IDs().Next(context.Background(), id.PrefixJob); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"system.db", "tenants/acme/tenant.db", "tenants/acme/tenant.db-wal", "tenants/acme/tenant.db-shm"} {
		info, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %o, want 600: it holds private data", p, info.Mode().Perm())
		}
	}
	for _, p := range []string{".", "tenants", "tenants/acme"} {
		info, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %o, want 700", p, info.Mode().Perm())
		}
	}
}

// I1: the durable settings hold on the databases this package actually opens.
func TestDurableSettings(t *testing.T) {
	s := openStores(t)
	st, err := s.Tenants.Create(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	dbs := map[string]*database{"system": s.System.(*systemStore).d, "tenant": st.(*tenantStore).d}
	for name, d := range dbs {
		var journal string
		var synchronous, autocheckpoint, foreignKeys int
		if err := d.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if err := d.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if err := d.db.QueryRow(`PRAGMA wal_autocheckpoint`).Scan(&autocheckpoint); err != nil {
			t.Fatal(err)
		}
		if err := d.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if journal != "wal" || synchronous != 2 || autocheckpoint <= 0 || foreignKeys != 1 {
			t.Errorf("%s: journal=%s synchronous=%d autocheckpoint=%d foreign_keys=%d", name, journal, synchronous, autocheckpoint, foreignKeys)
		}
		var version int
		if err := d.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version == 0 {
			t.Errorf("%s: user_version = 0", name)
		}
	}
}

func TestRequireExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	_, err := Open(context.Background(), Options{DataDir: dir, RequireExisting: true})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("a refused open must not create the data directory")
	}
	s, err := Open(context.Background(), Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(context.Background(), Options{DataDir: dir, RequireExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(context.Background(), Options{}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no data dir: %v", err)
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Tenants.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	friendly, err := st.IDs().Next(ctx, id.PrefixOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Assets().Create(ctx, domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindOriginal, Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Search().IndexVectors(ctx, repo.EmbeddingBatch{AssetID: id.Ref(friendly), Version: "v1", Model: "m@1",
		Chunks: []domain.EmbeddedChunk{{SearchChunk: domain.SearchChunk{EndSeconds: 1, Text: "hello"}, Vector: []float32{1, 0, 0, 0}}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tenants.ForTenant(ctx, "acme"); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("closed: %v", err)
	}

	s, err = Open(ctx, Options{DataDir: dir, RequireExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err = s.Tenants.ForTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Assets().Get(ctx, id.Ref(friendly)); err != nil {
		t.Fatal(err)
	}
	next, err := st.IDs().Next(ctx, id.PrefixOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if next != "ORI-002" {
		t.Fatalf("next = %s", next)
	}
	hits, err := st.Search().SearchVector(ctx, repo.VectorQuery{Vector: []float32{1, 0, 0, 0}, Model: "m@1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
}

// §15.6 and I103: a tenant database copied into another tenant's directory is
// refused, rather than serving the wrong tenant's data.
func TestMisplacedTenantDatabaseIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tenants.Create(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.MkdirAll(filepath.Join(dir, "tenants", "beta"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tenants", "acme", "tenant.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tenants", "beta", "tenant.db"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Tenants.ForTenant(ctx, "beta"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("misplaced database: %v", err)
	}
	if _, err := s.Tenants.ForTenant(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
}

// I103: a crash between creating the file and the first commit leaves an empty file.
// The next attempt must be able to finish, not find a database with no owner.
func TestEmptyFileFromACrashedCreateIsAdopted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	path := filepath.Join(dir, "tenants", "acme", "tenant.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tenants.Create(ctx, "acme"); !errs.IsCode(err, errs.CodeConflict) {
		t.Fatalf("create over a leftover file: %v", err)
	}
	st, err := s.Tenants.ForTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if st.Tenant() != "acme" {
		t.Fatalf("tenant = %s", st.Tenant())
	}
	// It now records its owner, so a later copy elsewhere is still refused.
	var owner string
	if err := st.(*tenantStore).d.db.QueryRow(`SELECT value FROM meta WHERE key = 'tenant_id'`).Scan(&owner); err != nil || owner != "acme" {
		t.Fatalf("owner = %q, %v", owner, err)
	}
}

func TestMigrationGuards(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.db")
	opts := Options{Clock: time.Now}

	// Opening the wrong kind of database is refused.
	d, err := openDatabase(ctx, path, kindSystem, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	d.close()
	if _, err := openDatabase(ctx, path, kindTenant, "", opts); err == nil {
		t.Fatal("a system database opened as a tenant database")
	}

	// A database from a newer binary is refused.
	raw, err := sqlitedriver.Open(path, sqlitedriver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations (version, name, sha256, applied_at) VALUES (9999, 'future', 'x', 'x')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := openDatabase(ctx, path, kindSystem, "", opts); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("newer database: %v", err)
	}

	// An applied migration whose text changed is refused.
	path2 := filepath.Join(dir, "y.db")
	d, err = openDatabase(ctx, path2, kindSystem, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	d.close()
	raw, err = sqlitedriver.Open(path2, sqlitedriver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE schema_migrations SET sha256 = 'changed' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := openDatabase(ctx, path2, kindSystem, "", opts); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("changed migration: %v", err)
	}

	// Opening twice is a no-op.
	path3 := filepath.Join(dir, "z.db")
	for range 2 {
		d, err := openDatabase(ctx, path3, kindTenant, "", opts)
		if err != nil {
			t.Fatal(err)
		}
		d.close()
	}
}

// §15.5, D23: triggers refuse changes to the audit table; only the prune may delete.
func TestAuditTableIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	s := openStores(t)
	st, err := s.Tenants.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Audit().Append(ctx, domain.AuditRecord{Actor: "a", Action: "x", Result: "allowed"}); err != nil {
		t.Fatal(err)
	}
	db := st.(*tenantStore).d.db
	if _, err := db.Exec(`UPDATE audit_log SET actor = 'b'`); err == nil {
		t.Fatal("audit_log accepted an update")
	}
	if _, err := db.Exec(`DELETE FROM audit_log`); err == nil {
		t.Fatal("audit_log accepted a delete")
	}
}

// R5: concurrent creates through real transactions never share an ID, whichever
// connection they land on, and survive across handles.
func TestConcurrentCreatesAcrossTenants(t *testing.T) {
	ctx := context.Background()
	s := openStores(t)
	for _, name := range []domain.TenantID{"acme", "beta"} {
		if _, err := s.Tenants.Create(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 40)
	for i := range 40 {
		tenant := domain.TenantID("acme")
		if i%2 == 0 {
			tenant = "beta"
		}
		go func() {
			st, err := s.Tenants.ForTenant(ctx, tenant)
			if err != nil {
				done <- err
				return
			}
			done <- st.InTx(ctx, func(tx repo.TenantRepos) error {
				friendly, err := tx.IDs().Next(ctx, id.PrefixClip)
				if err != nil {
					return err
				}
				return tx.Assets().Create(ctx, domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindClip, Title: "x"})
			})
		}()
	}
	for range 40 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []domain.TenantID{"acme", "beta"} {
		st, _ := s.Tenants.ForTenant(ctx, name)
		items, _, err := st.Assets().List(ctx, repo.AssetFilter{}, repo.Page{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 20 {
			t.Fatalf("%s has %d clips", name, len(items))
		}
		if items[0].ID != "CLIP-020" {
			t.Fatalf("%s: newest = %s", name, items[0].ID)
		}
	}
}

// I43, I106: above the cap, tenant-wide KNN visits only the newest assets.
func TestTenantWideVectorCap(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Options{DataDir: t.TempDir(), VectorScanCap: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.Tenants.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var created []id.Friendly
	for i := range 4 {
		friendly, err := st.IDs().Next(ctx, id.PrefixOriginal)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Assets().Create(ctx, domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindOriginal, Title: "x"}); err != nil {
			t.Fatal(err)
		}
		chunks := []domain.EmbeddedChunk{
			{SearchChunk: domain.SearchChunk{StartSeconds: 0, EndSeconds: 1, Text: fmt.Sprintf("a%d", i)}, Vector: []float32{1, 0}},
		}
		if err := st.Search().IndexVectors(ctx, repo.EmbeddingBatch{AssetID: id.Ref(friendly), Version: "v1", Model: "m@1", Chunks: chunks}); err != nil {
			t.Fatal(err)
		}
		created = append(created, friendly)
	}
	hits, err := st.Search().SearchVector(ctx, repo.VectorQuery{Vector: []float32{1, 0}, Model: "m@1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits = %+v", hits)
	}
	for _, h := range hits {
		if h.AssetID == created[0] {
			t.Fatalf("the oldest asset is past the cap: %+v", hits)
		}
	}
	// Asset-scoped search is not capped.
	hits, err = st.Search().SearchVector(ctx, repo.VectorQuery{Vector: []float32{1, 0}, Model: "m@1", AssetID: id.Ref(created[0])})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
}

// A vector search must find enough hits even when stale versions crowd the top.
func TestVectorSearchOverFetches(t *testing.T) {
	ctx := context.Background()
	s := openStores(t)
	st, err := s.Tenants.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	friendly, err := st.IDs().Next(ctx, id.PrefixOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Assets().Create(ctx, domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindOriginal, Title: "x", CurrentVersion: "v2"}); err != nil {
		t.Fatal(err)
	}
	var stale []domain.EmbeddedChunk
	for i := range 40 {
		stale = append(stale, domain.EmbeddedChunk{SearchChunk: domain.SearchChunk{StartSeconds: float64(i), EndSeconds: float64(i + 1), Text: "stale"}, Vector: []float32{1, 0}})
	}
	if err := st.Search().IndexVectors(ctx, repo.EmbeddingBatch{AssetID: id.Ref(friendly), Version: "v1", Model: "m@1", Chunks: stale}); err != nil {
		t.Fatal(err)
	}
	current := []domain.EmbeddedChunk{{SearchChunk: domain.SearchChunk{EndSeconds: 1, Text: "current"}, Vector: []float32{0.9, 0.1}}}
	if err := st.Search().IndexVectors(ctx, repo.EmbeddingBatch{AssetID: id.Ref(friendly), Version: "v2", Model: "m@1", Chunks: current}); err != nil {
		t.Fatal(err)
	}
	hits, err := st.Search().SearchVector(ctx, repo.VectorQuery{Vector: []float32{1, 0}, Model: "m@1", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Text != "current" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestFTSQuery(t *testing.T) {
	cases := map[string]string{
		"conscious breathing": `"conscious" OR "breathing"`,
		`a "b" NEAR(c)`:       `"a" OR "b" OR "NEAR" OR "c"`,
		"respiração, ok!":     `"respiração" OR "ok"`,
		"  ":                  "",
		`*-:^`:                "",
	}
	for in, want := range cases {
		if got := ftsQuery(in); got != want {
			t.Errorf("ftsQuery(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.TrimSpace(strings.Repeat("word ", maxQueryTokens+10))
	if got := strings.Count(ftsQuery(long), " OR ") + 1; got != maxQueryTokens {
		t.Errorf("long query has %d tokens", got)
	}
}

func TestTimeTextOrdersAndRoundTrips(t *testing.T) {
	a := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	b := a.Add(500 * time.Millisecond)
	if !(timeText(a) < timeText(b)) {
		t.Fatalf("%s must sort before %s", timeText(a), timeText(b))
	}
	got, err := parseTime(timeText(b))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(b) {
		t.Fatalf("round trip = %v", got)
	}
	// Any zone is normalized.
	zone := time.FixedZone("BRT", -3*3600)
	if timeText(a.In(zone)) != timeText(a) {
		t.Fatal("timestamps must be stored in UTC")
	}
}

func TestErrorsNeverCarryDriverText(t *testing.T) {
	ctx := context.Background()
	s := openStores(t)
	st, err := s.Tenants.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	a := domain.Asset{ID: "ORI-001", ULID: id.New(), Kind: domain.KindOriginal, Title: "x"}
	if err := st.Assets().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	err = st.Assets().Create(ctx, a)
	public, marshalErr := json.Marshal(errs.Public(err))
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, leak := range []string{"UNIQUE", "constraint", "assets."} {
		if strings.Contains(string(public), leak) {
			t.Fatalf("public error leaks driver text: %s", public)
		}
	}
}

// I102: a write through the outer store from inside InTx fails rather than hanging.
// It waits for the gate, so the context bounds this test.
func TestWriteOutsideTransactionInsideInTxFails(t *testing.T) {
	ctx := context.Background()
	s := openStores(t)
	st, err := s.Tenants.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	inner := st.InTx(ctx, func(tx repo.TenantRepos) error {
		_, err := st.IDs().Next(short, id.PrefixClip)
		return err
	})
	if !errs.IsCode(inner, errs.CodeTimeout) {
		t.Fatalf("err = %v", inner)
	}
}
