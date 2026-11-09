package catalognodes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/events"
	"github.com/rafaelp/vidyom/internal/jobs/memstore"
	"github.com/rafaelp/vidyom/schemas"
)

// ---- a small DaemonEnv over memstore ----

type memStorage struct {
	connector.Storage
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *memStorage) Put(_ context.Context, key string, r io.Reader, _ connector.PutOptions) (connector.ObjectInfo, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return connector.ObjectInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = data
	return connector.ObjectInfo{Key: key}, nil
}

func (m *memStorage) Get(_ context.Context, key string) (io.ReadCloser, connector.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objs[key]
	if !ok {
		return nil, connector.ObjectInfo{}, errs.NotFound("object")
	}
	return io.NopCloser(bytes.NewReader(data)), connector.ObjectInfo{Key: key}, nil
}

func (m *memStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

func (m *memStorage) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objs[key]
	return ok, nil
}

type testEnv struct {
	store   repo.TenantStore
	job     domain.Job
	perms   []domain.Permission
	cfg     config.Tenant
	daemon  config.Daemon
	storage map[domain.StorageRole]*memStorage
}

func (e *testEnv) Tenant() domain.TenantID  { return e.store.Tenant() }
func (e *testEnv) JobID() id.Friendly       { return e.job.ID }
func (e *testEnv) JobULID() id.ULID         { return e.job.ULID }
func (e *testEnv) NodeID() string           { return "n" }
func (e *testEnv) TraceID() string          { return "trace" }
func (e *testEnv) Logger() *slog.Logger     { return slog.Default() }
func (e *testEnv) Progress(float64, string) {}
func (e *testEnv) Scratch() node.Scratch    { return nil }
func (e *testEnv) Storage(_ context.Context, role domain.StorageRole) (connector.Storage, error) {
	return e.storage[role], nil
}
func (e *testEnv) OpenInput(context.Context, string, ...domain.StorageRole) (node.LocalInput, error) {
	return node.LocalInput{}, errs.NotImplemented("not in this test")
}
func (e *testEnv) HasPermission(p domain.Permission) bool {
	for _, have := range e.perms {
		if have == p {
			return true
		}
	}
	return false
}
func (e *testEnv) Store() repo.TenantStore { return e.store }
func (e *testEnv) Emit(ctx context.Context, tx repo.TenantRepos, d event.Draft) error {
	if tx != nil {
		_, err := events.Append(ctx, tx, e.Tenant(), events.JobSource(e.job), d, time.Now().UTC())
		return err
	}
	return e.store.InTx(ctx, func(tx repo.TenantRepos) error {
		_, err := events.Append(ctx, tx, e.Tenant(), events.JobSource(e.job), d, time.Now().UTC())
		return err
	})
}
func (e *testEnv) Publishing(context.Context, string) (connector.Publishing, error) {
	return nil, errs.NotImplemented("not in this test")
}
func (e *testEnv) TenantConfig(context.Context) (config.Tenant, error) { return e.cfg, nil }
func (e *testEnv) Embedder(context.Context) (connector.Embedder, error) {
	return nil, errs.NotImplemented("not in this test")
}
func (e *testEnv) LLM(context.Context, domain.Asset) (connector.LLM, error) {
	return nil, errs.NotImplemented("not in this test")
}
func (e *testEnv) Secrets() connector.SecretResolver { return nil }
func (e *testEnv) DaemonConfig() config.Daemon       { return e.daemon }

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	stores := memstore.New("acme")
	store, err := stores.ForTenant(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	job := domain.Job{ID: "JOB-001", ULID: id.New(), TenantID: "acme", WorkflowID: "wf", WorkflowVersion: "1.0.0", WorkflowSHA256: "abc", Status: domain.JobRunning, SubmittedBy: "editor@example.com", TraceID: "trace"}
	if err := store.Jobs().Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	return &testEnv{store: store, job: job, daemon: config.Daemon{SpoolDir: t.TempDir()}, storage: map[domain.StorageRole]*memStorage{
		domain.RoleArchive: {objs: map[string][]byte{}}, domain.RoleMaster: {objs: map[string][]byte{}}, domain.RoleProxy: {objs: map[string][]byte{}},
	}}
}

func registry() node.Registry {
	r := node.NewRegistry()
	Register(r)
	return r
}

func run(t *testing.T, env *testEnv, nodeType string, params any) (map[string]any, error) {
	t.Helper()
	impl, ok := registry().Lookup(nodeType, 1)
	if !ok {
		t.Fatalf("no node %s", nodeType)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := schemas.NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := cat.Lookup(nodeType, 1)
	if err := schemas.Validate(entry.ParametersRef(), raw); err != nil {
		t.Fatalf("%s parameters do not match the catalog: %v", nodeType, err)
	}
	res, err := impl.Execute(context.Background(), env, node.Input{Parameters: raw, Attempt: 1, IdempotencyKey: node.IdempotencyKey("acme", env.job.ULID, nodeType)})
	if err != nil {
		return nil, err
	}
	if err := schemas.Validate(entry.OutputRef(), res.Output); err != nil {
		t.Fatalf("%s output does not match the catalog: %v\n%s", nodeType, err, res.Output)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func mustRun(t *testing.T, env *testEnv, nodeType string, params any) map[string]any {
	t.Helper()
	out, err := run(t, env, nodeType, params)
	if err != nil {
		t.Fatalf("%s: %v", nodeType, err)
	}
	return out
}

func refused(t *testing.T, env *testEnv, nodeType string, params any, code errs.Code) {
	t.Helper()
	_, err := run(t, env, nodeType, params)
	if !errs.IsCode(err, code) {
		t.Fatalf("%s: got %v, want %s", nodeType, err, code)
	}
	if node.IsTransient(err) {
		t.Fatalf("%s: a business refusal is not worth a retry", nodeType)
	}
}

func events2(t *testing.T, env *testEnv, typ event.Type) int {
	t.Helper()
	items, _, err := env.store.Outbox().ListEvents(context.Background(), repo.OutboxFilter{Type: typ}, repo.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return len(items)
}

// asset gets an asset through catalog.register with a new job key.
func original(t *testing.T, env *testEnv) domain.Asset {
	t.Helper()
	ctx := context.Background()
	var a domain.Asset
	err := env.store.InTx(ctx, func(tx repo.TenantRepos) error {
		friendly, err := tx.IDs().Next(ctx, id.PrefixOriginal)
		if err != nil {
			return err
		}
		a = domain.Asset{ID: friendly, ULID: id.New(), TenantID: "acme", Kind: domain.KindOriginal, Title: "Class", CurrentVersion: "v1", Status: domain.AssetRegistered}
		if err := tx.Assets().Create(ctx, a); err != nil {
			return err
		}
		return tx.Versions().Create(ctx, domain.AssetVersion{AssetID: friendly, Version: "v1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func file(t *testing.T, env *testEnv, asset id.Friendly, version string, purpose domain.FilePurpose, role domain.StorageRole, key string) {
	t.Helper()
	err := env.store.Versions().PutFile(context.Background(), id.Ref(asset), version, domain.VersionFile{
		Purpose: purpose,
		File:    domain.MediaFile{Storage: role, Key: key, SizeBytes: 1 << 30, RecordedAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st := env.storage[role]; st != nil {
		st.objs[key] = []byte("data")
	}
}

// makeReady gives an original everything the consolidated condition needs (R11).
func makeReady(t *testing.T, env *testEnv, asset id.Friendly) {
	t.Helper()
	ctx := context.Background()
	file(t, env, asset, "v1", domain.FileWorkingMaster, domain.RoleMaster, "originals/"+string(asset)+"/v1/master.mp4")
	file(t, env, asset, "v1", domain.FileProxy, domain.RoleProxy, "originals/"+string(asset)+"/v1/proxy.mp4")
	file(t, env, asset, "v1", domain.FileSourceOriginal, domain.RoleArchive, "originals/"+string(asset)+"/v1/original.mov")
	if err := env.store.Transcripts().Save(ctx, domain.Transcript{AssetID: asset, Version: "v1", Language: "en", TimestampBasis: "working_master", Segments: []domain.TranscriptSegment{
		{Start: 0, End: 10, Text: "public words"}, {Start: 10, End: 20, Text: "private name", Words: []domain.Word{{Start: 10, End: 11, Text: "private"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Search().IndexLexical(ctx, id.Ref(asset), "v1", []domain.SearchChunk{{Text: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Search().IndexVectors(ctx, repo.EmbeddingBatch{AssetID: id.Ref(asset), Version: "v1", Model: "m@1"}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, env, "catalog.set_status", map[string]any{"assetId": asset, "status": "ingesting"})
	yes := true
	mustRun(t, env, "catalog.set_flags", map[string]any{"assetId": asset, "masterAvailable": yes, "backupVerified": yes, "proxyAvailable": yes, "timelineValidated": yes})
}

func TestDescriptorsMatchTheCatalog(t *testing.T) {
	r := registry()
	cat, err := schemas.NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, d := range r.List() {
		if err := schemas.CheckDescriptor(d); err != nil {
			t.Error(err)
		}
		seen++
	}
	// Every catalog.* node assigned to this component exists.
	for _, c := range cat.Nodes {
		if c.Owner == "engine" && strings.HasPrefix(c.Type, "catalog.") {
			if _, ok := r.Lookup(c.Type, c.TypeVersion); !ok {
				t.Errorf("%s is not implemented", c.Type)
			}
		}
	}
	if seen == 0 {
		t.Fatal("nothing registered")
	}
}

func TestRegister(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	out := mustRun(t, env, "catalog.register", map[string]any{"kind": "original", "title": "Class", "category": "yoga_class"})
	if out["id"] != "ORI-001" || out["version"] != "v1" || out["key_prefix"] != "originals/ORI-001/v1" {
		t.Fatalf("output %v", out)
	}
	// A re-run after a lost acknowledgment finds the same asset (R5, D11).
	again := mustRun(t, env, "catalog.register", map[string]any{"kind": "original", "title": "Class", "category": "yoga_class"})
	if again["id"] != "ORI-001" || again["ulid"] != out["ulid"] {
		t.Fatalf("again %v", again)
	}
	if events2(t, env, event.AssetCreated) != 1 {
		t.Fatal("asset.created fired twice")
	}
	a, err := env.store.Assets().Get(ctx, id.Ref("ORI-001"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Visibility != domain.VisibilityPrivate || a.Status != domain.AssetRegistered || a.AIProcessing != domain.AILocalOnly || a.Category != "yoga_class" {
		t.Fatalf("asset %+v (§2.5, D8)", a)
	}
	if _, err := env.store.Versions().Get(ctx, id.Ref("ORI-001"), "v1"); err != nil {
		t.Fatal(err)
	}
	// A different job gets a different asset, with the next ID (R5).
	other := newEnv(t)
	other.store = env.store
	other.job.ULID = id.New()
	other.job.ID = "JOB-002"
	if err := other.store.Jobs().Create(ctx, other.job); err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, other, "catalog.register", map[string]any{"kind": "original", "title": "Other"}); got["id"] != "ORI-002" {
		t.Fatalf("second %v", got)
	}

	refused(t, newEnv(t), "catalog.register", map[string]any{"kind": "clip", "title": "No lineage"}, errs.CodeInvalidArgument)
	refused(t, newEnv(t), "catalog.register", map[string]any{"kind": "original", "title": "T", "lineage": map[string]any{"method": "computed"}}, errs.CodeInvalidArgument)
	refused(t, newEnv(t), "catalog.register", map[string]any{"kind": "original", "title": "T", "visibility": "public"}, errs.CodeInvalidArgument)
	// External AI needs someone who may consent (D8).
	refused(t, newEnv(t), "catalog.register", map[string]any{"kind": "original", "title": "T", "aiProcessing": "external_allowed"}, errs.CodeForbidden)
	consent := newEnv(t)
	consent.perms = []domain.Permission{domain.PermAssetAIConsent}
	mustRun(t, consent, "catalog.register", map[string]any{"kind": "original", "title": "T", "aiProcessing": "external_allowed"})
	b, _ := consent.store.Assets().Get(ctx, id.Ref("ORI-001"))
	if b.AIConsent == nil || b.AIConsent.AuthorizedBy != "editor@example.com" || b.AIConsent.Scope != "asset" {
		t.Fatalf("consent %+v", b.AIConsent)
	}
}

func TestRegisterClipLineage(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	source := original(t, env)
	src, _ := env.store.Assets().Get(ctx, id.Ref(source.ID))
	src.Language, src.Collection = "pt", "yoga"
	if err := env.store.Assets().Update(ctx, src); err != nil {
		t.Fatal(err)
	}
	seg := map[string]any{"start_seconds": 10.5, "end_seconds": 20}
	refused(t, env, "catalog.register", map[string]any{"kind": "clip", "title": "T", "lineage": map[string]any{"method": "declared", "source_asset_id": source.ID, "segments": []any{seg}}}, errs.CodeNotImplemented)
	refused(t, env, "catalog.register", map[string]any{"kind": "clip", "title": "T", "lineage": map[string]any{"method": "computed", "source_asset_id": source.ID}}, errs.CodeInvalidArgument)
	refused(t, env, "catalog.register", map[string]any{"kind": "clip", "title": "T", "lineage": map[string]any{"method": "computed", "source_asset_id": source.ID, "segments": []any{map[string]any{"start_seconds": 5, "end_seconds": 2}}}}, errs.CodeInvalidArgument)
	refused(t, env, "catalog.register", map[string]any{"kind": "clip", "title": "T", "lineage": map[string]any{"method": "computed", "source_asset_id": "ORI-999", "segments": []any{seg}}}, errs.CodeNotFound)
	out := mustRun(t, env, "catalog.register", map[string]any{"kind": "clip", "title": "Clip", "lineage": map[string]any{
		"method": "computed", "source_asset_id": source.ID, "selection_approval_id": "APR-001", "segments": []any{seg},
	}})
	if out["id"] != "CLIP-001" || out["key_prefix"] != "clips/CLIP-001/v1" {
		t.Fatalf("output %v", out)
	}
	clip, _ := env.store.Assets().Get(ctx, id.Ref("CLIP-001"))
	if clip.ParentAssetID != source.ID || clip.Lineage.RootAssetID != source.ID || clip.Language != "pt" || clip.Collection != "yoga" {
		t.Fatalf("clip %+v", clip)
	}
	if clip.Lineage.Segments[0].SourceAssetID != source.ID || clip.Lineage.Segments[0].SourceVersion != "v1" || clip.Lineage.SelectionApprovalID != "APR-001" {
		t.Fatalf("lineage %+v", clip.Lineage)
	}
	if events2(t, env, event.AssetCreated) != 1 || events2(t, env, event.ClipCreated) != 1 {
		t.Fatal("both asset.created and clip.created fire (R35)")
	}
}

func TestGetAndReadiness(t *testing.T) {
	env := newEnv(t)
	a := original(t, env)
	out := mustRun(t, env, "catalog.get", map[string]any{"id": a.ID})
	if out["key_prefix"] != "originals/ORI-001/v1" {
		t.Fatalf("output %v", out)
	}
	refused(t, env, "catalog.get", map[string]any{"id": a.ID, "requiredStatus": "ready_for_clipping"}, errs.CodeFailedPrecondition)
	refused(t, env, "catalog.get", map[string]any{"id": a.ID, "requireMasterAvailable": true}, errs.CodeFailedPrecondition)
	refused(t, env, "catalog.get", map[string]any{"id": a.ID, "requireClipEligible": true}, errs.CodeFailedPrecondition)
	refused(t, env, "catalog.get", map[string]any{"id": "ORI-999"}, errs.CodeNotFound)

	// mark_ready says what is missing.
	mustRun(t, env, "catalog.set_status", map[string]any{"assetId": a.ID, "status": "ingesting"})
	_, err := run(t, env, "catalog.mark_ready", map[string]any{"assetId": a.ID})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) || !strings.Contains(err.Error(), "backup not verified") {
		t.Fatalf("mark_ready: %v", err)
	}
	makeReady(t, env, a.ID)
	if got := mustRun(t, env, "catalog.mark_ready", map[string]any{"assetId": a.ID}); got["status"] != "ready_for_clipping" {
		t.Fatalf("output %v", got)
	}
	mustRun(t, env, "catalog.mark_ready", map[string]any{"assetId": a.ID})
	if events2(t, env, event.AssetReadyForClipping) != 1 {
		t.Fatal("the event fires on the transition only")
	}
	ready := mustRun(t, env, "catalog.get", map[string]any{"id": a.ID, "requireClipEligible": true, "requireMasterAvailable": true})
	if ready["working_master_uri"] != "storage://master/originals/ORI-001/v1/master.mp4" || ready["proxy_uri"] != "storage://proxy/originals/ORI-001/v1/proxy.mp4" || ready["archive_uri"] != "storage://archive/originals/ORI-001/v1/original.mov" {
		t.Fatalf("output %v", ready)
	}
	// A bad transition is refused.
	refused(t, env, "catalog.set_status", map[string]any{"assetId": a.ID, "status": "published"}, errs.CodeFailedPrecondition)
	refused(t, env, "catalog.set_status", map[string]any{"assetId": a.ID, "status": "nonsense"}, errs.CodeInvalidArgument)
	// The master goes missing: no longer accepted, no restoration (§2.5).
	no := false
	mustRun(t, env, "catalog.set_flags", map[string]any{"assetId": a.ID, "masterAvailable": no})
	refused(t, env, "catalog.get", map[string]any{"id": a.ID, "requireClipEligible": true}, errs.CodeFailedPrecondition)
	list := mustRun(t, env, "catalog.list", map[string]any{"kind": "original", "limit": 10})
	if len(list["items"].([]any)) != 1 {
		t.Fatalf("list %v", list)
	}
}

func TestRecordFileRulesAndVersions(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := original(t, env)
	object := func(role, key string) map[string]any {
		return map[string]any{"uri": "storage://" + role + "/" + key, "storage": role, "key": key, "sha256": "abc", "size_bytes": 10, "verified": true}
	}
	mustRun(t, env, "catalog.record_file", map[string]any{"assetId": a.ID, "purpose": "working_master", "object": object("master", "k/master.mp4")})
	got, _ := env.store.Assets().Get(ctx, id.Ref(a.ID))
	if got.WorkingMaster == nil || got.WorkingMaster.Key != "k/master.mp4" || !got.WorkingMaster.Verified || got.WorkingMaster.SHA256 != "abc" {
		t.Fatalf("master %+v", got.WorkingMaster)
	}
	// A working master in the archive would be the first step to clipping from it.
	refused(t, env, "catalog.record_file", map[string]any{"assetId": a.ID, "purpose": "working_master", "object": object("archive", "k/m")}, errs.CodeForbiddenStorageRole)
	refused(t, env, "catalog.record_file", map[string]any{"assetId": a.ID, "purpose": "proxy", "object": object("master", "k/p")}, errs.CodeForbiddenStorageRole)
	refused(t, env, "catalog.record_file", map[string]any{"assetId": a.ID, "purpose": "working_master", "object": object("master", "../escape")}, errs.CodeInvalidArgument)

	// Before publication the file is replaced in place (D35).
	if out := mustRun(t, env, "catalog.next_version", map[string]any{"assetId": a.ID}); out["version"] != "v1" || out["created"] != false {
		t.Fatalf("next %v", out)
	}
	mustRun(t, env, "catalog.record_file", map[string]any{"assetId": a.ID, "purpose": "working_master", "object": object("master", "k/replaced.mp4")})
	// After publication it is immutable, and a new render means v2.
	if err := env.store.Versions().MarkImmutable(ctx, id.Ref(a.ID), "v1"); err != nil {
		t.Fatal(err)
	}
	refused(t, env, "catalog.record_file", map[string]any{"assetId": a.ID, "version": "v1", "purpose": "working_master", "object": object("master", "k/again.mp4")}, errs.CodeFailedPrecondition)
	if out := mustRun(t, env, "catalog.next_version", map[string]any{"assetId": a.ID}); out["version"] != "v2" || out["created"] != true {
		t.Fatalf("next %v", out)
	}
	// A retry finds the same version, and creates nothing.
	if out := mustRun(t, env, "catalog.next_version", map[string]any{"assetId": a.ID}); out["version"] != "v2" || out["created"] != false {
		t.Fatalf("next %v", out)
	}
	if v, _ := env.store.Versions().Get(ctx, id.Ref(a.ID), "v1"); v.Files[0].File.Key != "k/replaced.mp4" {
		t.Fatalf("v1 must not change: %+v", v.Files)
	}
	mustRun(t, env, "catalog.record_file", map[string]any{"assetId": a.ID, "purpose": "working_master", "object": object("master", "k/v2.mp4")})
	got, _ = env.store.Assets().Get(ctx, id.Ref(a.ID))
	if got.CurrentVersion != "v2" || got.WorkingMaster.Key != "k/v2.mp4" {
		t.Fatalf("asset %+v", got)
	}
}

func TestMetadataEditIsHistoryNotAVersion(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := original(t, env)
	mustRun(t, env, "catalog.update_metadata", map[string]any{"assetId": a.ID, "title": "New", "tags": []string{"x"}})
	got, _ := env.store.Assets().Get(ctx, id.Ref(a.ID))
	if got.Title != "New" || got.CurrentVersion != "v1" {
		t.Fatalf("asset %+v (D35)", got)
	}
	history, _, _ := env.store.Assets().History(ctx, id.Ref(a.ID), repo.Page{})
	if len(history) != 1 || history[0].Actor != "editor@example.com" || !strings.Contains(string(history[0].Changes), "New") {
		t.Fatalf("history %+v", history)
	}
	versions, _ := env.store.Versions().List(ctx, id.Ref(a.ID))
	if len(versions) != 1 {
		t.Fatalf("versions %+v", versions)
	}
	// No change, no history.
	mustRun(t, env, "catalog.update_metadata", map[string]any{"assetId": a.ID, "title": "New"})
	history, _, _ = env.store.Assets().History(ctx, id.Ref(a.ID), repo.Page{})
	if len(history) != 1 {
		t.Fatalf("history %+v", history)
	}
}

func TestProvenance(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := original(t, env)
	if err := env.store.Jobs().PutNode(ctx, domain.NodeExecution{JobULID: env.job.ULID, NodeID: "t", NodeType: "video.transcode", TypeVersion: 1, Status: domain.NodeCompleted}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"status": "completed", "tools": map[string]string{"whisper": "large-v3"}})
	if _, err := env.store.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: env.job.ID, Kind: domain.JobEventNode, NodeID: "t", Data: data}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, env, "catalog.record_provenance", map[string]any{"assetId": a.ID, "inputs": map[string]string{"raw": "aa"}, "outputs": map[string]string{"master": "bb"}, "profiles": []string{"master-1080p@1"}, "tools": map[string]string{"ffmpeg": "7"}})
	v, _ := env.store.Versions().Get(ctx, id.Ref(a.ID), "v1")
	p := v.Provenance
	if p == nil || p.JobID != "JOB-001" || p.Workflow != "wf@1.0.0" || p.WorkflowSHA256 != "abc" || p.NodeTypes["video.transcode"] != 1 {
		t.Fatalf("provenance %+v (R21)", p)
	}
	if p.Tools["ffmpeg"] != "7" || p.Tools["whisper"] != "large-v3" || p.InputSHA256["raw"] != "aa" || p.OutputSHA256["master"] != "bb" {
		t.Fatalf("provenance %+v", p)
	}
}

func clipOf(t *testing.T, env *testEnv, parent domain.Asset, segments []domain.Segment, n string) domain.Asset {
	t.Helper()
	ctx := context.Background()
	root := parent.ID
	if parent.Lineage != nil {
		root = parent.Lineage.RootAssetID
	}
	for i := range segments {
		segments[i].SourceAssetID = parent.ID
		segments[i].SourceVersion = "v1"
	}
	a := domain.Asset{ID: id.Friendly("CLIP-" + n), ULID: id.New(), TenantID: "acme", Kind: domain.KindClip, Title: "Clip", CurrentVersion: "v1", Status: domain.AssetRegistered, ParentAssetID: parent.ID, MasterAvailable: true,
		Lineage: &domain.Lineage{Method: domain.LineageComputed, SourceAssetID: parent.ID, RootAssetID: root, Segments: segments, Chain: []id.Friendly{parent.ID}}}
	if err := env.store.Assets().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Versions().Create(ctx, domain.AssetVersion{AssetID: a.ID, Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	file(t, env, a.ID, "v1", domain.FileWorkingMaster, domain.RoleMaster, "clips/"+string(a.ID)+"/v1/master.mp4")
	return a
}

func TestOverlapWarningD47(t *testing.T) {
	env := newEnv(t)
	source := original(t, env)
	clipOf(t, env, source, []domain.Segment{{StartSeconds: 100, EndSeconds: 200}}, "001")
	seg := func(a, b float64) []any { return []any{map[string]any{"start_seconds": a, "end_seconds": b}} }
	if out := mustRun(t, env, "catalog.check_overlap", map[string]any{"sourceId": source.ID, "segments": seg(300, 400)}); out["warning"] != false {
		t.Fatalf("disjoint %v", out)
	}
	if out := mustRun(t, env, "catalog.check_overlap", map[string]any{"sourceId": source.ID, "segments": seg(190, 290)}); out["warning"] != false {
		t.Fatalf("a small overlap is not substantial: %v", out)
	}
	out := mustRun(t, env, "catalog.check_overlap", map[string]any{"sourceId": source.ID, "segments": seg(120, 220)})
	if out["warning"] != true {
		t.Fatalf("substantial %v", out)
	}
	item := out["overlaps"].([]any)[0].(map[string]any)
	if item["clip_id"] != "CLIP-001" || item["overlap_seconds"] != float64(80) {
		t.Fatalf("overlap %v", item)
	}
}

func TestResolveRenderSourceD36(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	root := original(t, env)
	yes := true
	file(t, env, root.ID, "v1", domain.FileWorkingMaster, domain.RoleMaster, "originals/ORI-001/v1/master.mp4")
	mustRun(t, env, "catalog.set_flags", map[string]any{"assetId": root.ID, "masterAvailable": yes})
	root, _ = env.store.Assets().Get(ctx, id.Ref(root.ID))
	seg := func(a, b float64) []any { return []any{map[string]any{"start_seconds": a, "end_seconds": b}} }

	// Of an original: its own master, as given.
	out := mustRun(t, env, "catalog.resolve_render_source", map[string]any{"sourceId": root.ID, "segments": seg(10, 20)})
	if out["source_uri"] != "storage://master/originals/ORI-001/v1/master.mp4" || out["root_asset_id"] != "ORI-001" {
		t.Fatalf("original %v", out)
	}

	// Of a clip: the root's master, on the root's timeline, through two levels.
	first := clipOf(t, env, root, []domain.Segment{{StartSeconds: 100, EndSeconds: 200}}, "001")
	second := clipOf(t, env, first, []domain.Segment{{StartSeconds: 10, EndSeconds: 50}}, "002")
	out = mustRun(t, env, "catalog.resolve_render_source", map[string]any{"sourceId": second.ID, "segments": seg(5, 15)})
	if out["source_uri"] != "storage://master/originals/ORI-001/v1/master.mp4" {
		t.Fatalf("clip of clip %v", out)
	}
	s0 := out["segments"].([]any)[0].(map[string]any)
	if s0["start_seconds"] != float64(115) || s0["end_seconds"] != float64(125) || s0["source_asset_id"] != "ORI-001" {
		t.Fatalf("mapped %v", s0)
	}
	if out["root_asset_id"] != "ORI-001" {
		t.Fatalf("root %v", out)
	}

	// Across a cut of a non-continuous clip: the clip's own master.
	spliced := clipOf(t, env, root, []domain.Segment{{StartSeconds: 100, EndSeconds: 110}, {StartSeconds: 500, EndSeconds: 510}}, "003")
	out = mustRun(t, env, "catalog.resolve_render_source", map[string]any{"sourceId": spliced.ID, "segments": seg(5, 15)})
	if out["source_uri"] != "storage://master/clips/CLIP-003/v1/master.mp4" || out["segments"].([]any)[0].(map[string]any)["start_seconds"] != float64(5) {
		t.Fatalf("across the cut %v", out)
	}
	// Inside the second interval it is continuous again.
	out = mustRun(t, env, "catalog.resolve_render_source", map[string]any{"sourceId": spliced.ID, "segments": seg(12, 18)})
	if s0 := out["segments"].([]any)[0].(map[string]any); s0["start_seconds"] != float64(502) || s0["end_seconds"] != float64(508) {
		t.Fatalf("inside the second interval %v", out)
	}

	// The root's master is gone: the clip's own master, never the archive.
	no := false
	mustRun(t, env, "catalog.set_flags", map[string]any{"assetId": root.ID, "masterAvailable": no})
	out = mustRun(t, env, "catalog.resolve_render_source", map[string]any{"sourceId": first.ID, "segments": seg(5, 15)})
	if out["source_uri"] != "storage://master/clips/CLIP-001/v1/master.mp4" {
		t.Fatalf("no root master %v", out)
	}
	if strings.Contains(out["source_uri"].(string), "archive") {
		t.Fatal(out)
	}
	// With no master at all, an error and no fallback (§3.2).
	refused(t, env, "catalog.resolve_render_source", map[string]any{"sourceId": root.ID, "segments": seg(1, 2)}, errs.CodeFailedPrecondition)
}

func TestExportPackageRedacts(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := original(t, env)
	makeReady(t, env, a.ID)
	file(t, env, a.ID, "v1", domain.FileThumbnail, domain.RoleProxy, "originals/ORI-001/v1/thumbnail.jpg")
	if err := env.store.Transcripts().SaveAnalysis(ctx, domain.Analysis{AssetID: a.ID, Version: "v1", Available: true, Summary: "A summary", Chapters: []domain.Chapter{
		{Title: "One", StartSeconds: 0, EndSeconds: 5, Summary: "open"}, {Title: "Two", StartSeconds: 10, EndSeconds: 15, Summary: "private chapter"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Transcripts().MarkSensitive(ctx, domain.SensitiveSegment{AssetID: a.ID, Version: "v1", StartSeconds: 11, EndSeconds: 12, Reason: "name"}); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, env, "catalog.export_package", map[string]any{"assetId": a.ID})
	if out["uri"] != "storage://master/exports/ORI-001/metadata.json" {
		t.Fatalf("output %v", out)
	}
	st := env.storage[domain.RoleMaster]
	for _, name := range []string{"metadata.json", "transcript.json", "transcript.srt", "chapters.json", "lineage.json", "versions.json", "publications.json", "thumbnail.jpg"} {
		if _, ok := st.objs["exports/ORI-001/"+name]; !ok {
			t.Errorf("%s missing (§12.5)", name)
		}
	}
	all := ""
	for _, data := range st.objs {
		all += string(data)
	}
	for _, secret := range []string{"private name", "private chapter", "A summary"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q leaked into the export (D30)", secret)
		}
	}
	if !strings.Contains(all, "public words") || !strings.Contains(all, redacted) {
		t.Errorf("the export must keep the rest, with the marked part redacted")
	}
	if srt := string(st.objs["exports/ORI-001/transcript.srt"]); !strings.Contains(srt, "00:00:10,000 --> 00:00:20,000") {
		t.Errorf("srt %q", srt)
	}
	refused(t, env, "catalog.export_package", map[string]any{"assetId": a.ID, "destinationRole": "archive"}, errs.CodeForbiddenStorageRole)
}

func TestListExpiredRenders(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	source := original(t, env)
	old := clipOf(t, env, source, []domain.Segment{{StartSeconds: 1, EndSeconds: 2}}, "001")
	young := clipOf(t, env, source, []domain.Segment{{StartSeconds: 1, EndSeconds: 2}}, "002")
	approved := clipOf(t, env, source, []domain.Segment{{StartSeconds: 1, EndSeconds: 2}}, "003")
	age := func(a domain.Asset, status []domain.AssetStatus, created time.Time) {
		t.Helper()
		got, _ := env.store.Assets().Get(ctx, id.Ref(a.ID))
		for _, s := range status {
			got.Status = s
			if err := env.store.Assets().Update(ctx, got); err != nil {
				t.Fatal(err)
			}
		}
		if err := env.store.InTx(ctx, func(tx repo.TenantRepos) error { return nil }); err != nil {
			t.Fatal(err)
		}
		memstore.SetCreatedAt(env.store, a.ULID, created)
	}
	review := []domain.AssetStatus{domain.AssetIngesting, domain.AssetReadyForClipping, domain.AssetEditorialReview}
	long := time.Now().AddDate(0, 0, -45)
	age(old, append(review, domain.AssetRejected), long)
	age(young, review, time.Now().AddDate(0, 0, -5))
	age(approved, append(review, domain.AssetApproved), long)
	out := mustRun(t, env, "catalog.list_expired_renders", map[string]any{})
	items := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["asset_id"] != "CLIP-001" || items[0].(map[string]any)["master_uri"] != "storage://master/clips/CLIP-001/v1/master.mp4" {
		t.Fatalf("items %v (D16)", items)
	}
	// The retention comes from the parameter, then the daemon, then the default.
	if out := mustRun(t, env, "catalog.list_expired_renders", map[string]any{"olderThanDays": 2}); len(out["items"].([]any)) != 2 {
		t.Fatalf("items %v", out)
	}
	env.daemon.Retention.UnapprovedRenderDays = 90
	if out := mustRun(t, env, "catalog.list_expired_renders", map[string]any{}); len(out["items"].([]any)) != 0 {
		t.Fatalf("items %v", out)
	}
}

func TestReleaseInput(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	u := domain.Upload{ID: "UPL-001", ULID: id.New(), TenantID: "acme", Status: domain.UploadComplete}
	if err := env.store.Uploads().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(env.daemon.SpoolDir, "acme")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, string(u.ULID))
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if out := mustRun(t, env, "catalog.release_input", map[string]any{"source": "upload:" + string(u.ULID)}); out["released"] != true {
			t.Fatalf("output %v", out)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the spool copy must go: %v", err)
	}
	// A server: path is someone's file, and is never deleted (R9).
	server := filepath.Join(t.TempDir(), "class.mp4")
	if err := os.WriteFile(server, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, env, "catalog.release_input", map[string]any{"source": "server:" + server}); out["released"] != false {
		t.Fatalf("output %v", out)
	}
	if _, err := os.Stat(server); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, env, "catalog.release_input", map[string]any{"source": "storage://master/a"}); out["released"] != false {
		t.Fatalf("output %v", out)
	}
	refused(t, env, "catalog.release_input", map[string]any{"source": "relative/path"}, errs.CodeInvalidArgument)
}

func TestDeleteRulesAndPurge(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := original(t, env)
	makeReady(t, env, a.ID)
	env.cfg = config.Tenant{
		Storages: config.Storages{Roles: map[domain.StorageRole]config.StorageInstance{domain.RoleArchive: {Driver: "gcs", StorageClass: "ARCHIVE"}}},
		Budget:   config.BudgetCosts{PerUnitMinor: map[string]float64{EarlyDeletionOperation: 2}},
	}
	refused(t, env, "catalog.delete", map[string]any{"assetId": a.ID}, errs.CodeForbidden)
	env.perms = []domain.Permission{domain.PermAssetDelete}
	if err := env.store.Publications().Create(ctx, domain.Publication{ID: "PUB-001", ULID: id.New(), AssetID: a.ID, Destination: "d", AssetVersion: "v1", Status: domain.PubProcessing}); err != nil {
		t.Fatal(err)
	}
	refused(t, env, "catalog.delete", map[string]any{"assetId": a.ID}, errs.CodeFailedPrecondition)
	pub, _ := env.store.Publications().Get(ctx, id.Ref("PUB-001"))
	pub.Status = domain.PubWithdrawn
	if err := env.store.Publications().Update(ctx, pub); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, env, "catalog.delete", map[string]any{"assetId": a.ID})
	purgeAfter, err := time.Parse(time.RFC3339, out["archive_purge_after"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(purgeAfter).Hours() / 24; d < 364 || d > 366 {
		t.Fatalf("purge after %s (D17)", purgeAfter)
	}
	if est := out["early_deletion_estimate_minor"].(float64); est < 700 || est > 740 {
		t.Fatalf("estimate %v: 1 GiB for 365 days at 2 per GiB-day", est)
	}
	if _, ok := env.storage[domain.RoleMaster].objs["originals/ORI-001/v1/master.mp4"]; ok {
		t.Fatal("the master must go")
	}
	if _, ok := env.storage[domain.RoleArchive].objs["originals/ORI-001/v1/original.mov"]; !ok {
		t.Fatal("the archive copy stays until its duration ends")
	}
	got, _ := env.store.Assets().Get(ctx, id.Ref(a.ID))
	if got.DeletedAt == nil {
		t.Fatal("soft delete")
	}
	if _, lexical := func() (bool, bool) {
		l, v, _ := env.store.Search().HasIndex(ctx, id.Ref(a.ID), "v1", "")
		return v, l
	}(); lexical {
		t.Fatal("indexes must go")
	}
	// Idempotent: the fact is recorded once.
	mustRun(t, env, "catalog.delete", map[string]any{"assetId": a.ID})
	if events2(t, env, event.AssetDeleted) != 1 {
		t.Fatal("asset.deleted must fire once")
	}
	audit, _, _ := env.store.Audit().List(ctx, repo.AuditFilter{Action: "asset.deleted"}, repo.Page{})
	if len(audit) != 1 || audit[0].Permission != domain.PermAssetDelete || audit[0].Actor != "editor@example.com" {
		t.Fatalf("audit %+v", audit)
	}
	// Nothing works on it any more (D17).
	refused(t, env, "catalog.get", map[string]any{"id": a.ID}, errs.CodeNotFound)
	refused(t, env, "catalog.set_status", map[string]any{"assetId": a.ID, "status": "ingesting"}, errs.CodeNotFound)
	refused(t, env, "catalog.get_transcript", map[string]any{"assetId": a.ID}, errs.CodeNotFound)
	refused(t, env, "catalog.export_package", map[string]any{"assetId": a.ID}, errs.CodeNotFound)

	// The archive is not purgeable until the minimum duration ended.
	if out := mustRun(t, env, "catalog.list_purgeable_archives", map[string]any{}); len(out["items"].([]any)) != 0 {
		t.Fatalf("too early: %v", out)
	}
	env.cfg.Storages.Roles[domain.RoleArchive] = config.StorageInstance{Driver: "local", Parameters: json.RawMessage(`{"minStorageDays":0}`)}
	out = mustRun(t, env, "catalog.list_purgeable_archives", map[string]any{})
	items := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["archive_uri"] != "storage://archive/originals/ORI-001/v1/original.mov" {
		t.Fatalf("items %v", items)
	}
	delete(env.storage[domain.RoleArchive].objs, "originals/ORI-001/v1/original.mov")
	if out := mustRun(t, env, "catalog.list_purgeable_archives", map[string]any{}); len(out["items"].([]any)) != 0 {
		t.Fatalf("already purged: %v", out)
	}
}

func TestGetTranscript(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := original(t, env)
	refused(t, env, "catalog.get_transcript", map[string]any{"assetId": a.ID}, errs.CodeNotFound)
	if err := env.store.Transcripts().Save(ctx, domain.Transcript{AssetID: a.ID, Version: "v1", Language: "en"}); err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, env, "catalog.get_transcript", map[string]any{"assetId": a.ID})
	if out["version"] != "v1" || out["transcript"].(map[string]any)["language"] != "en" {
		t.Fatalf("output %v", out)
	}
	if _, has := out["analysis"]; has {
		t.Fatalf("no analysis stored: %v", out)
	}
	if err := env.store.Transcripts().SaveAnalysis(ctx, domain.Analysis{AssetID: a.ID, Version: "v1", Available: true}); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, env, "catalog.get_transcript", map[string]any{"assetId": a.ID}); out["analysis"] == nil {
		t.Fatalf("output %v", out)
	}
}
