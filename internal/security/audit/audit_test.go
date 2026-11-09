package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/persistence"
	"github.com/rafaelp/vidyom/internal/persistence/sqlitedriver"
)

var ctx = context.Background()

// fakeArchive is a small archive role: objects in memory, checksum enforced.
type fakeArchive struct {
	connector.Storage
	objects map[string][]byte
	err     error
}

func (f *fakeArchive) Put(_ context.Context, key string, r io.Reader, opts connector.PutOptions) (connector.ObjectInfo, error) {
	if f.err != nil {
		return connector.ObjectInfo{}, f.err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return connector.ObjectInfo{}, err
	}
	sum := sha256.Sum256(data)
	if opts.SHA256 != hex.EncodeToString(sum[:]) {
		return connector.ObjectInfo{}, errors.New("checksum mismatch")
	}
	f.objects[key] = data
	return connector.ObjectInfo{Key: key, SizeBytes: int64(len(data)), SHA256: opts.SHA256}, nil
}

type fakeResolver struct {
	archive *fakeArchive
	roles   []domain.StorageRole
	tenants []domain.TenantID
}

func (f *fakeResolver) Storage(_ context.Context, tenant domain.TenantID, role domain.StorageRole) (connector.Storage, error) {
	f.roles = append(f.roles, role)
	f.tenants = append(f.tenants, tenant)
	return f.archive, nil
}

type fixture struct {
	dir     string
	stores  *persistence.Stores
	store   repo.TenantStore
	auditor *Auditor
	archive *fakeArchive
	storage *fakeResolver
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stores.Close() })
	store, err := stores.Tenants.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	archive := &fakeArchive{objects: map[string][]byte{}}
	resolver := &fakeResolver{archive: archive}
	return &fixture{dir, stores, store, New(stores.Tenants, Options{System: stores.System, Storage: resolver}), archive, resolver}
}

func (f *fixture) record(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		err := f.auditor.Record(ctx, security.AuditEntry{
			TenantID:   "acme",
			Principal:  domain.Principal{Email: "a@example.com", Method: domain.AuthDevice},
			Action:     "asset.update",
			Permission: domain.PermAssetWrite,
			Resource:   "ORI-001",
			RequestID:  "r",
			Details:    map[string]any{"i": i},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// rawDB opens the tenant file directly, the way someone with host access would.
func (f *fixture) rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitedriver.Open(filepath.Join(f.dir, "tenants", "acme", "tenant.db"), sqlitedriver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustVerify(t *testing.T, f *fixture) domain.AuditVerification {
	t.Helper()
	v, err := f.auditor.Verify(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyGoodChain(t *testing.T) {
	f := setup(t)
	if v := mustVerify(t, f); !v.OK || v.Records != 0 {
		t.Fatalf("empty chain = %+v", v)
	}
	f.record(t, 600) // crosses a scan batch
	v := mustVerify(t, f)
	if !v.OK || v.Records != 600 || v.FirstBadSeq != 0 {
		t.Fatalf("v = %+v", v)
	}
}

// §1.1 step 12: tampering a row makes verify fail. The triggers stop an application
// bug, so an attacker removes them first; the chain still catches it.
func TestVerifyDetectsTampering(t *testing.T) {
	cases := map[string]struct {
		sql    string
		badSeq int64
		reason string
	}{
		"changed actor":   {`UPDATE audit_log SET actor = 'mallory@example.com' WHERE seq = 3`, 3, "does not match its hash"},
		"changed details": {`UPDATE audit_log SET details = '{"i":99}' WHERE seq = 2`, 2, "does not match its hash"},
		"changed result":  {`UPDATE audit_log SET result = 'denied' WHERE seq = 5`, 5, "does not match its hash"},
		"changed time":    {`UPDATE audit_log SET at = '2020-01-01T00:00:00.000000000Z' WHERE seq = 1`, 1, "does not match its hash"},
		"deleted middle":  {`DELETE FROM audit_log WHERE seq = 4`, 4, "missing"},
		"deleted first":   {`DELETE FROM audit_log WHERE seq = 1`, 1, "missing"},
		"recomputed row":  {`UPDATE audit_log SET hash = '` + strings.Repeat("a", 64) + `' WHERE seq = 2`, 2, "does not match its hash"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.record(t, 6)
			db := f.rawDB(t)
			// An application write hits the triggers.
			if _, err := db.Exec(c.sql); err == nil {
				t.Fatal("the trigger should have refused this")
			}
			for _, q := range []string{`DROP TRIGGER audit_log_no_update`, `DROP TRIGGER audit_log_no_delete`, c.sql} {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			v := mustVerify(t, f)
			if v.OK || v.FirstBadSeq != c.badSeq || !strings.Contains(v.Reason, c.reason) {
				t.Fatalf("v = %+v", v)
			}
		})
	}
}

// Someone who rewrites a record and every later hash consistently gets past the chain
// itself, but not past what was already exported (D23).
func TestVerifyDetectsARewrittenChainThroughExports(t *testing.T) {
	f := setup(t)
	f.record(t, 3)
	if _, err := f.auditor.Export(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	r := f.store.Audit()
	records, err := r.Scan(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	db := f.rawDB(t)
	if _, err := db.Exec(`DROP TRIGGER audit_log_no_update`); err != nil {
		t.Fatal(err)
	}
	prev := domain.AuditGenesisHash
	for _, rec := range records {
		rec.Actor = "mallory@example.com"
		rec.PrevHash = prev
		rec.Hash, err = rec.ComputeHash()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE audit_log SET actor = ?, prev_hash = ?, hash = ? WHERE seq = ?`, rec.Actor, rec.PrevHash, rec.Hash, rec.Seq); err != nil {
			t.Fatal(err)
		}
		prev = rec.Hash
	}
	v := mustVerify(t, f)
	if v.OK || v.FirstBadSeq != 3 || !strings.Contains(v.Reason, "exported") {
		t.Fatalf("v = %+v", v)
	}
}

func TestVerifyDetectsTruncationOfExportedRecords(t *testing.T) {
	f := setup(t)
	f.record(t, 5)
	if _, err := f.auditor.Export(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	db := f.rawDB(t)
	for _, q := range []string{`DROP TRIGGER audit_log_no_delete`, `DELETE FROM audit_log WHERE seq > 3`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	v := mustVerify(t, f)
	if v.OK || v.FirstBadSeq != 4 || !strings.Contains(v.Reason, "exported") {
		t.Fatalf("v = %+v", v)
	}
}

func TestExport(t *testing.T) {
	f := setup(t)
	now := time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)
	f.auditor.clock = func() time.Time { return now }
	if key, err := f.auditor.Export(ctx, "acme"); err != nil || key != "" {
		t.Fatalf("nothing to export: %q, %v", key, err)
	}
	if len(f.archive.objects) != 0 {
		t.Fatal("nothing to export must write nothing")
	}
	f.record(t, 3)
	key, err := f.auditor.Export(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if key != "audit/2026/11/09/00000001-00000003.jsonl" {
		t.Fatalf("key = %s", key)
	}
	if err := domain.ValidateStorageKey(key); err != nil {
		t.Fatal(err)
	}
	if f.storage.roles[0] != domain.RoleArchive || f.storage.tenants[0] != "acme" {
		t.Fatalf("storage = %v %v: export goes to the tenant's archive", f.storage.roles, f.storage.tenants)
	}

	// The file verifies offline, with nothing but itself.
	prev := domain.AuditGenesisHash
	scanner := bufio.NewScanner(bytes.NewReader(f.archive.objects[key]))
	lines := 0
	for scanner.Scan() {
		var rec domain.AuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		want, err := rec.ComputeHash()
		if err != nil {
			t.Fatal(err)
		}
		if want != rec.Hash || rec.PrevHash != prev {
			t.Fatalf("line %d does not verify offline: %+v", lines, rec)
		}
		prev = rec.Hash
		lines++
	}
	if lines != 3 {
		t.Fatalf("lines = %d", lines)
	}

	// Nothing new: the last key, no write.
	again, err := f.auditor.Export(ctx, "acme")
	if err != nil || again != key || len(f.archive.objects) != 1 {
		t.Fatalf("again = %q, %v, objects = %d", again, err, len(f.archive.objects))
	}

	// Only what is new, continuing the chain.
	f.record(t, 2)
	next, err := f.auditor.Export(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if next != "audit/2026/11/09/00000004-00000005.jsonl" {
		t.Fatalf("next = %s", next)
	}
	var first domain.AuditRecord
	if err := json.Unmarshal(bytes.SplitN(f.archive.objects[next], []byte("\n"), 2)[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.PrevHash != prev {
		t.Fatal("the second export must continue the chain")
	}
	if v := mustVerify(t, f); !v.OK {
		t.Fatalf("v = %+v", v)
	}
}

func TestExportFailureKeepsTheWatermark(t *testing.T) {
	f := setup(t)
	f.record(t, 2)
	f.archive.err = errors.New("storage down")
	if _, err := f.auditor.Export(ctx, "acme"); err == nil {
		t.Fatal("a failed put must fail the export")
	}
	m := f.store.Audit().(repo.AuditMaintainer)
	if _, found, err := m.LastExport(ctx); err != nil || found {
		t.Fatalf("watermark moved: %v, %v", found, err)
	}
	f.archive.err = nil
	key, err := f.auditor.Export(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(key, "00000001-00000002.jsonl") {
		t.Fatalf("key = %s", key)
	}
}

func TestExportRefusals(t *testing.T) {
	f := setup(t)
	if _, err := f.auditor.Export(ctx, ""); !errs.IsCode(err, errs.CodeNotImplemented) {
		t.Fatalf("system chain: %v", err)
	}
	noStorage := New(f.stores.Tenants, Options{})
	if _, err := noStorage.Export(ctx, "acme"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("no storage: %v", err)
	}
	if _, err := f.auditor.Export(ctx, "unknown"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
}

func TestPruneOnlyExportedAndOldEnough(t *testing.T) {
	f := setup(t)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	f.auditor.clock = func() time.Time { return now }
	f.record(t, 3)
	now = start.AddDate(1, 2, 0) // 14 months later
	f.record(t, 2)

	// Nothing exported yet: nothing goes, however old.
	removed, err := f.auditor.Prune(ctx, "acme", 365)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d", removed)
	}
	if _, err := f.auditor.Export(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	// A retention below the floor is raised to it (D23): 30 days would take all five.
	removed, err = f.auditor.Prune(ctx, "acme", 30)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d", removed)
	}
	// What remains still verifies from the anchor, and continues.
	v := mustVerify(t, f)
	if !v.OK || v.Records != 2 {
		t.Fatalf("v = %+v", v)
	}
	f.record(t, 1)
	if v := mustVerify(t, f); !v.OK || v.Records != 3 {
		t.Fatalf("v = %+v", v)
	}
	// An export after a prune starts where the last one stopped.
	key, err := f.auditor.Export(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(key, "00000006-00000006") {
		t.Fatalf("key = %s", key)
	}
}

func TestSecretLikeDetailsAreRedacted(t *testing.T) {
	f := setup(t)
	err := f.auditor.Record(ctx, security.AuditEntry{
		TenantID: "acme",
		Action:   "x",
		Details: map[string]any{
			"token":      "fictional-token",
			"api_key":    "fictional-key",
			"nested":     map[string]any{"password": "fictional-password", "fine": 1},
			"list":       []any{map[string]any{"credentialsRef": "fictional-ref"}},
			"provider":   "file",
			"PrivateKey": "fictional-private",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.store.Audit().Scan(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	details := string(records[0].Details)
	if strings.Contains(details, "fictional") {
		t.Fatalf("details leak: %s", details)
	}
	if !strings.Contains(details, `"provider":"file"`) || !strings.Contains(details, `"fine":1`) {
		t.Fatalf("ordinary details must stay: %s", details)
	}
}

func TestRecordFailurePropagates(t *testing.T) {
	f := setup(t)
	if err := f.auditor.Record(ctx, security.AuditEntry{TenantID: "unknown", Action: "x"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
	if err := f.auditor.Record(ctx, security.AuditEntry{TenantID: "acme"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no action: %v", err)
	}
	bad := map[string]any{"x": func() {}}
	if err := f.auditor.Record(ctx, security.AuditEntry{TenantID: "acme", Action: "x", Details: bad}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad details: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.auditor.Record(cancelled, security.AuditEntry{TenantID: "acme", Action: "x"}); err == nil {
		t.Fatal("a cancelled write must not report success")
	}
}

func TestSystemChain(t *testing.T) {
	f := setup(t)
	entry := security.AuditEntry{Principal: domain.Principal{Method: domain.AuthSystem}, Action: "daemon.unseal", Details: map[string]any{"sealing": "file"}}
	if err := f.auditor.Record(ctx, entry); err != nil {
		t.Fatal(err)
	}
	v, err := f.auditor.Verify(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK || v.Records != 1 {
		t.Fatalf("v = %+v", v)
	}
	// It is a separate chain: the tenant's stays empty.
	if v := mustVerify(t, f); v.Records != 0 {
		t.Fatalf("tenant chain = %+v", v)
	}
	noSystem := New(f.stores.Tenants, Options{})
	if err := noSystem.Record(ctx, entry); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("no system store: %v", err)
	}
}

func TestChainsAreIndependentAcrossTenants(t *testing.T) {
	f := setup(t)
	if _, err := f.stores.Tenants.Create(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	f.record(t, 2)
	beta, err := f.auditor.Verify(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if !beta.OK || beta.Records != 0 {
		t.Fatalf("beta = %+v", beta)
	}
	// A record copied into another tenant's chain does not verify.
	record, err := f.store.Audit().Scan(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlitedriver.Open(filepath.Join(f.dir, "tenants", "beta", "tenant.db"), sqlitedriver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := record[0]
	if _, err := db.Exec(`INSERT INTO audit_log (seq, ulid, at, actor, method, action, permission, resource, result, request_id, details, prev_hash, hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.Seq, string(r.ULID), r.At.Format("2006-01-02T15:04:05.000000000Z"), r.Actor, string(r.Method), r.Action, string(r.Permission), r.Resource, r.Result, r.RequestID, string(r.Details), r.PrevHash, r.Hash); err != nil {
		t.Fatal(err)
	}
	moved, err := f.auditor.Verify(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if moved.OK {
		t.Fatal("a record moved between tenants must not verify")
	}
}

func TestAppendTxCommitsWithTheStateChange(t *testing.T) {
	f := setup(t)
	boom := errs.New(errs.CodeConflict, "abort")
	err := f.store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := AppendTx(ctx, tx.Audit(), security.AuditEntry{Action: "x"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		return boom
	})
	if !errs.IsCode(err, errs.CodeConflict) {
		t.Fatalf("err = %v", err)
	}
	if v := mustVerify(t, f); v.Records != 0 {
		t.Fatalf("v = %+v", v)
	}
	err = f.store.InTx(ctx, func(tx repo.TenantRepos) error {
		return AppendTx(ctx, tx.Audit(), security.AuditEntry{Action: "x"}, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := mustVerify(t, f); v.Records != 1 || !v.OK {
		t.Fatalf("v = %+v", v)
	}
}

func TestAuditService(t *testing.T) {
	f := setup(t)
	svc := f.auditor.Service()
	scope := service.Scope{Tenant: "acme", Principal: domain.Principal{Email: "admin@example.com"}, RequestID: "req-1"}
	f.record(t, 3)
	if err := f.auditor.Record(ctx, security.AuditEntry{TenantID: "acme", Principal: domain.Principal{Email: "b@example.com"}, Action: "secret.used", Resource: "secret:x"}); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(ctx, scope, api.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 4 || list.Items[0].Action != "secret.used" {
		t.Fatalf("list = %+v", list)
	}
	for _, q := range []api.AuditQuery{{Actor: "b@example.com"}, {Action: "secret.used"}, {Resource: "secret:x"}, {Since: "1h", Actor: "b@example.com"}} {
		got, err := svc.List(ctx, scope, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Items) != 1 {
			t.Fatalf("query %+v = %+v", q, got)
		}
	}
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	got, err := svc.List(ctx, scope, api.AuditQuery{Since: future})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 0 || got.Items == nil {
		t.Fatalf("empty list must be an empty array: %+v", got)
	}
	if _, err := svc.List(ctx, scope, api.AuditQuery{Since: "yesterday"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad since: %v", err)
	}
	if _, err := svc.List(ctx, scope, api.AuditQuery{Since: "-1h"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("negative since: %v", err)
	}
	paged, err := svc.List(ctx, scope, api.AuditQuery{PageQuery: api.PageQuery{Limit: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paged.Items) != 2 || paged.NextCursor == "" {
		t.Fatalf("paged = %+v", paged)
	}
	v, err := svc.Verify(ctx, scope)
	if err != nil || !v.OK || v.Records != 4 {
		t.Fatalf("v = %+v, %v", v, err)
	}
	if _, err := svc.Verify(ctx, service.Scope{}); !errs.IsCode(err, errs.CodeTenantRequired) {
		t.Fatalf("no tenant: %v", err)
	}
	res, err := svc.Export(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Key, "audit/") {
		t.Fatalf("key = %s", res.Key)
	}
	// The export itself is audited, with the permission that authorized it.
	exportRecord, err := svc.List(ctx, scope, api.AuditQuery{Action: "audit.export"})
	if err != nil {
		t.Fatal(err)
	}
	if len(exportRecord.Items) != 1 || exportRecord.Items[0].Permission != domain.PermAuditExport || exportRecord.Items[0].RequestID != "req-1" || exportRecord.Items[0].Actor != "admin@example.com" {
		t.Fatalf("record = %+v", exportRecord)
	}
	if _, err := svc.Export(ctx, service.Scope{}); !errs.IsCode(err, errs.CodeTenantRequired) {
		t.Fatalf("no tenant: %v", err)
	}
	if _, err := svc.List(ctx, service.Scope{Tenant: "unknown"}, api.AuditQuery{}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	got, err := ParseSince("24h", now)
	if err != nil || !got.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("24h = %v, %v", got, err)
	}
	got, err = ParseSince("2026-11-01T00:00:00Z", now)
	if err != nil || got.Month() != 11 || got.Day() != 1 {
		t.Fatalf("rfc3339 = %v, %v", got, err)
	}
	got, err = ParseSince("", now)
	if err != nil || got != nil {
		t.Fatalf("empty = %v, %v", got, err)
	}
}
