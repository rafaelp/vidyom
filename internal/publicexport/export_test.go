package publicexport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/persistence/sqlitedriver"
	"github.com/rafaelp/vidyom/internal/publicexport/fakestore"
)

const (
	bucketName    = "videos-standard-acme"
	privateTitle  = "Private original: Hatha Yoga Class recording"
	sensitiveText = "the student named Maria told us about her diagnosis"
	publicText    = "conscious breathing begins at the base of the spine"
)

type keys struct{ err error }

func (k keys) CacheKey(context.Context, domain.TenantID) ([]byte, error) { return nil, nil }
func (k keys) PublicIDKey(_ context.Context, t domain.TenantID) ([]byte, error) {
	if k.err != nil {
		return nil, k.err
	}
	return []byte("fictional key of " + string(t)), nil
}

// fixture holds a tenant with a private original and one public clip derived from it.
type fixture struct {
	t        *testing.T
	store    *fakestore.Store
	exporter *Exporter
	dataDir  string
	original domain.Asset
	clip     domain.Asset
	pub      domain.Publication
}

var fixedNow = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func newFixture(t *testing.T, tenant domain.TenantID) *fixture {
	t.Helper()
	f := &fixture{t: t, store: fakestore.New(tenant), dataDir: t.TempDir()}
	f.exporter = New(Options{DataDir: f.dataDir, Keys: keys{}, Now: func() time.Time { return fixedNow }})
	f.original = f.addOriginal("ORI-001")
	f.clip, f.pub = f.addPublicClip("CLIP-001", "PUB-001", "APR-001", f.original.ID, 100, 160)
	return f
}

func (f *fixture) ctx() context.Context { return context.Background() }

func (f *fixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func storedFile(purpose domain.FilePurpose, role domain.StorageRole, key string) domain.VersionFile {
	return domain.VersionFile{Purpose: purpose, File: domain.MediaFile{
		Storage: role, Key: key, SHA256: strings.Repeat("a", 64), SizeBytes: 10, Verified: true,
		Location: &domain.Location{Driver: "gcs", Bucket: bucketName, Prefix: "masters"},
	}}
}

func (f *fixture) addOriginal(friendly string) domain.Asset {
	f.t.Helper()
	a := domain.Asset{
		ID: id.Friendly(friendly), ULID: id.New(), TenantID: f.store.Tenant(), Kind: domain.KindOriginal,
		Title: privateTitle, Description: "private description of the recording",
		Status: domain.AssetReadyForClipping, Visibility: domain.VisibilityPrivate, CurrentVersion: "v1",
	}
	f.must(f.store.Assets().Create(f.ctx(), a))
	f.must(f.store.Versions().Create(f.ctx(), domain.AssetVersion{
		AssetID: a.ID, Version: "v1",
		Files: []domain.VersionFile{
			storedFile(domain.FileSourceOriginal, domain.RoleArchive, "originals/"+friendly+"/v1/original.mov"),
			storedFile(domain.FileWorkingMaster, domain.RoleMaster, "originals/"+friendly+"/v1/master.mp4"),
		},
	}))
	f.must(f.store.Transcripts().Save(f.ctx(), domain.Transcript{
		AssetID: a.ID, Version: "v1", Language: "en", Segments: []domain.TranscriptSegment{
			{Start: 0, End: 10, Text: "words only in the private original, outside any clip"},
		},
	}))
	return a
}

func (f *fixture) addPublicClip(friendly, pubID, aprID string, source id.Friendly, start, end float64) (domain.Asset, domain.Publication) {
	f.t.Helper()
	clip := domain.Asset{
		ID: id.Friendly(friendly), ULID: id.New(), TenantID: f.store.Tenant(), Kind: domain.KindClip,
		Category: "yoga_class", Title: "Conscious breathing", Description: "A clip about breath.",
		Tags: []string{"breathing", "yoga"}, Language: "en", DurationSeconds: end - start,
		Status: domain.AssetPublished, Visibility: domain.VisibilityPublic, CurrentVersion: "v1",
		ParentAssetID: source,
		Lineage: &domain.Lineage{
			Method: domain.LineageComputed, SourceAssetID: source, RootAssetID: source,
			Segments: []domain.Segment{{SourceAssetID: source, SourceVersion: "v1", StartSeconds: start, EndSeconds: end}},
		},
	}
	f.must(f.store.Assets().Create(f.ctx(), clip))
	f.must(f.store.Versions().Create(f.ctx(), domain.AssetVersion{
		AssetID: clip.ID, Version: "v1", Immutable: true,
		Files: []domain.VersionFile{
			storedFile(domain.FileWorkingMaster, domain.RoleMaster, "clips/"+friendly+"/v1/master.mp4"),
			storedFile(domain.FileProxy, domain.RoleProxy, "clips/"+friendly+"/v1/proxy.mp4"),
			storedFile(domain.FileBackup, domain.RoleArchive, "clips/"+friendly+"/v1/master.mp4"),
		},
	}))
	f.must(f.store.Transcripts().Save(f.ctx(), domain.Transcript{
		AssetID: clip.ID, Version: "v1", Language: "en", DurationSeconds: end - start,
		Segments: []domain.TranscriptSegment{
			{Start: 0, End: 10, Text: publicText},
			{Start: 10, End: 20, Text: sensitiveText},
			{Start: 20, End: 30, Text: "and the exhale follows without effort"},
		},
	}))
	f.must(f.store.Transcripts().SaveAnalysis(f.ctx(), domain.Analysis{
		AssetID: clip.ID, Version: "v1", Available: true, Summary: "internal analysis summary never released",
		Chapters: []domain.Chapter{
			{Title: "Breath basics", StartSeconds: 0, EndSeconds: 10, Summary: "How to begin."},
			{Title: "A chapter about Maria", StartSeconds: 10, EndSeconds: 20, Summary: "Her diagnosis."},
		},
	}))
	f.must(f.store.Transcripts().MarkSensitive(f.ctx(), domain.SensitiveSegment{
		AssetID: clip.ID, Version: "v1", StartSeconds: 11, EndSeconds: 19, Reason: "health",
	}))
	f.must(f.store.Approvals().Create(f.ctx(), domain.Approval{
		ID: id.Friendly(aprID), ULID: id.New(), TenantID: f.store.Tenant(), Kind: domain.ApprovalPublication,
		Status: domain.ApprovalApproved, AssetID: clip.ID, AssetVersion: "v1",
	}))
	publishedAt := fixedNow.Add(-time.Hour)
	pub := domain.Publication{
		ID: id.Friendly(pubID), ULID: id.New(), TenantID: f.store.Tenant(), AssetID: clip.ID,
		Destination: "mock-main", AssetVersion: "v1", PublishedFileSHA256: strings.Repeat("a", 64),
		RemoteID: "mock-" + hex.EncodeToString([]byte(friendly)), URL: "https://media.example.internal/watch/mock-" + hex.EncodeToString([]byte(friendly)),
		Status: domain.PubPublished, Visibility: domain.VisibilityPublic, ApprovalID: id.Friendly(aprID),
		Title: "Conscious breathing, published", Description: "Published description.",
		IdempotencyKey: "key-" + pubID, PublishedAt: &publishedAt,
	}
	f.must(f.store.Publications().Create(f.ctx(), pub))
	return clip, pub
}

func (f *fixture) request() Request {
	return Request{
		Tenant: f.store.Tenant(), Store: f.store,
		Drivers: map[string]string{"mock-main": "mock"},
		Links: func(_ context.Context, p domain.Publication, driver string) connector.PublicLinks {
			return connector.PublicLinks{EmbedURL: "https://media.example.internal/embed/" + p.RemoteID, SupportsTimestamps: true}
		},
	}
}

func (f *fixture) export() Result {
	f.t.Helper()
	res, err := f.exporter.Export(f.ctx(), f.request())
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *fixture) target() string {
	return filepath.Join(f.dataDir, "tenants", string(f.store.Tenant()), "public.db")
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlitedriver.Open(path, sqlitedriver.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestExportWritesTheSchemaWithReleasedFieldsOnly(t *testing.T) {
	f := newFixture(t, "acme")
	res := f.export()
	if res.Records != 1 || res.Artifact.SchemaVersion != 1 || res.Artifact.Path != f.target() || res.Artifact.Tenant != "acme" {
		t.Fatalf("result %+v", res)
	}
	db := openDB(t, f.target())

	meta := map[string]string{}
	rows, err := db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		meta[k] = v
	}
	if meta["schema_version"] != "1" || meta["generated_at"] != "2026-11-09T10:00:00Z" {
		t.Fatalf("meta %v", meta)
	}
	if v, ok := meta["embedding_model"]; !ok || v != "" {
		t.Fatalf("embedding_model %q present=%v", v, ok)
	}
	if v, ok := meta["embedding_dimensions"]; !ok || v != "0" {
		t.Fatalf("embedding_dimensions %q present=%v", v, ok)
	}

	var publicID, title, description, kind, category, language, tags, publishedAt, thumbnail, group string
	var duration float64
	err = db.QueryRow(`SELECT public_id, title, description, kind, category, language, tags, duration_seconds, published_at, thumbnail_url, source_group FROM videos`).
		Scan(&publicID, &title, &description, &kind, &category, &language, &tags, &duration, &publishedAt, &thumbnail, &group)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Conscious breathing, published" || description != "Published description." || kind != "clip" ||
		category != "yoga_class" || language != "en" || tags != `["breathing","yoga"]` || duration != 60 ||
		publishedAt != "2026-11-09T09:00:00Z" || thumbnail != "" || group == "" {
		t.Fatalf("row: %q %q %q %q %q %q %v %q %q %q", title, description, kind, category, language, tags, duration, publishedAt, thumbnail, group)
	}
	if !regexp.MustCompile(`^[a-z2-7]{24}$`).MatchString(publicID) {
		t.Fatalf("public_id %q is not opaque lowercase base32", publicID)
	}

	var driver, url, embed string
	var ts int
	if err := db.QueryRow(`SELECT destination, url, embed_url, supports_timestamps FROM destinations`).Scan(&driver, &url, &embed, &ts); err != nil {
		t.Fatal(err)
	}
	if driver != "mock" || url != f.pub.URL || !strings.Contains(embed, "/embed/") || ts != 1 {
		t.Fatalf("destination %q %q %q %d", driver, url, embed, ts)
	}

	// The lexical index holds titles, descriptions, chapters and released text, and
	// chapter and transcript rows carry numeric timestamps, which the library needs.
	for source, want := range map[string]int{"title": 1, "description": 1, "chapter": 1, "transcript": 2} {
		if got := count(t, db, `SELECT count(*) FROM search_fts WHERE source = ?`, source); got != want {
			t.Errorf("search_fts %s: %d rows, want %d", source, got, want)
		}
	}
	if got := count(t, db, `SELECT count(*) FROM search_fts WHERE search_fts MATCH 'breathing' AND source = 'transcript'`); got != 1 {
		t.Errorf("transcript search found %d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM search_fts WHERE source IN ('chapter','transcript') AND typeof(start_seconds) != 'real' AND typeof(start_seconds) != 'integer'`); got != 0 {
		t.Errorf("%d timed rows without a number", got)
	}
}

// The test the plan names: nothing private reaches the file's raw bytes (R44, D30).
func TestRawFileHoldsNoStorageKeyULIDOrPrivateData(t *testing.T) {
	f := newFixture(t, "acme")
	f.must(f.store.Versions().PutFile(f.ctx(), id.Ref(f.original.ID), "v1",
		storedFile(domain.FileProxy, domain.RoleProxy, "originals/ORI-001/v1/proxy.mp4")))

	// An asset no one approved, one withdrawn, one approved but private.
	draft, draftPub := f.addPublicClip("CLIP-002", "PUB-002", "APR-002", f.original.ID, 0, 10)
	f.must(f.store.Approvals().Create(f.ctx(), domain.Approval{
		ID: "APR-002", ULID: id.New(), TenantID: "acme", Kind: domain.ApprovalPublication,
		Status: domain.ApprovalPending, AssetID: draft.ID,
	}))
	draftPub.Title = "Unapproved clip title"
	f.must(f.store.Publications().Update(f.ctx(), draftPub))

	withdrawn, withdrawnPub := f.addPublicClip("CLIP-003", "PUB-003", "APR-003", f.original.ID, 20, 30)
	withdrawnPub.Status = domain.PubWithdrawn
	withdrawnPub.Title = "Withdrawn clip title"
	f.must(f.store.Publications().Update(f.ctx(), withdrawnPub))
	withdrawn.Status = domain.AssetWithdrawn
	f.must(f.store.Assets().Update(f.ctx(), withdrawn))

	res := f.export()
	if res.Records != 1 {
		t.Fatalf("records %d, withheld %+v", res.Records, res.Withheld)
	}
	raw, err := os.ReadFile(f.target())
	if err != nil {
		t.Fatal(err)
	}

	forbidden := []string{
		"storage://", bucketName, "gs://", "originals/", "clips/", "master.mp4", "proxy.mp4", "original.mov",
		"ORI-001", "CLIP-001", "CLIP-002", "CLIP-003", "PUB-001", "PUB-002", "PUB-003", "APR-001", "APR-002", "JOB-",
		"mock-main",
		privateTitle, "private description of the recording", "words only in the private original",
		"Unapproved clip title", "Withdrawn clip title",
		"Maria", "diagnosis", "internal analysis summary",
		"acme",
		string(f.original.ULID), string(f.clip.ULID), string(f.pub.ULID),
		string(draft.ULID), string(withdrawn.ULID),
	}
	for _, s := range forbidden {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("public.db contains %q", s)
		}
	}
	if m := regexp.MustCompile(`[0-9A-HJKMNP-TV-Z]{26}`).Find(raw); m != nil {
		t.Errorf("public.db contains a ULID-shaped value: %q", m)
	}
	// The scan itself must be able to see stored text.
	if !bytes.Contains(raw, []byte(publicText)) {
		t.Error("the scan sees no released text; it would prove nothing")
	}
}

func TestFileIsOneSelfContainedReadOnlyDatabase(t *testing.T) {
	f := newFixture(t, "acme")
	res := f.export()

	dir := filepath.Dir(f.target())
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "public.db" {
		t.Fatalf("directory holds %v", entries)
	}
	info, err := os.Stat(f.target())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != res.Artifact.SizeBytes {
		t.Fatalf("mode %v size %d", info.Mode().Perm(), info.Size())
	}
	raw, _ := os.ReadFile(f.target())
	if raw[18] != 1 || raw[19] != 1 {
		t.Fatalf("header says %d %d, want rollback journal mode", raw[18], raw[19])
	}

	// A read-only mount: no side file may be needed.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	db := openDB(t, f.target())
	if got := count(t, db, `SELECT count(*) FROM videos`); got != 1 {
		t.Fatalf("videos %d", got)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity %q, %v", integrity, err)
	}
	if _, err := db.Exec(`DELETE FROM videos`); err == nil {
		t.Fatal("a read-only reader must not be able to write")
	}
}

func TestFailedExportLeavesThePreviousFile(t *testing.T) {
	f := newFixture(t, "acme")
	first := f.export()
	before, _ := os.ReadFile(f.target())

	f.exporter = New(Options{DataDir: f.dataDir, Keys: keys{err: errors.New("vault sealed")}})
	if _, err := f.exporter.Export(f.ctx(), f.request()); !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("a missing key must fail closed: %v", err)
	}

	f.exporter = New(Options{DataDir: f.dataDir, Keys: keys{}})
	ctx, cancel := context.WithCancel(f.ctx())
	cancel()
	if _, err := f.exporter.Export(ctx, f.request()); err == nil {
		t.Fatal("a cancelled export must fail")
	}

	after, _ := os.ReadFile(f.target())
	if !bytes.Equal(before, after) {
		t.Fatal("the previous file changed")
	}
	entries, _ := os.ReadDir(filepath.Dir(f.target()))
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	if first.Artifact.SHA256 == "" {
		t.Fatal("no checksum")
	}
}

func TestExportClearsWhatAKilledExportLeftBehind(t *testing.T) {
	f := newFixture(t, "acme")
	dir := filepath.Dir(f.target())
	f.must(os.MkdirAll(dir, 0o700))
	f.must(os.WriteFile(filepath.Join(dir, "public.db.tmp-123"), []byte("junk"), 0o600))
	f.export()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "public.db" {
		t.Fatalf("directory holds %v", entries)
	}
}

func TestWithdrawalDisappearsOnRebuildAndAnEmptyFileIsStillWritten(t *testing.T) {
	f := newFixture(t, "acme")
	if res := f.export(); res.Records != 1 {
		t.Fatalf("records %d", res.Records)
	}
	f.pub.Status = domain.PubWithdrawn
	f.must(f.store.Publications().Update(f.ctx(), f.pub))

	res := f.export()
	if res.Records != 0 {
		t.Fatalf("records %d", res.Records)
	}
	db := openDB(t, f.target())
	for _, table := range []string{"videos", "destinations", "chapters", "transcript_segments", "embeddings", "search_fts"} {
		if got := count(t, db, `SELECT count(*) FROM `+table); got != 0 {
			t.Errorf("%s has %d rows", table, got)
		}
	}
	if got := count(t, db, `SELECT count(*) FROM meta WHERE key = 'schema_version'`); got != 1 {
		t.Fatal("an empty file must still be valid for the library")
	}
}

func TestSensitiveSegmentsChaptersAndVectorsAreDroppedWhole(t *testing.T) {
	f := newFixture(t, "acme")
	f.must(f.store.Search().IndexVectors(f.ctx(), repo.EmbeddingBatch{
		AssetID: id.Ref(f.clip.ID), Version: "v1", Model: "fake-hash-embed@1", Chunks: []domain.EmbeddedChunk{
			{SearchChunk: domain.SearchChunk{Level: domain.ChunkWindow, StartSeconds: 0, EndSeconds: 10, Text: publicText}, Vector: []float32{1, 0}},
			{SearchChunk: domain.SearchChunk{Level: domain.ChunkWindow, StartSeconds: 5, EndSeconds: 15, Text: sensitiveText}, Vector: []float32{0, 1}},
			{SearchChunk: domain.SearchChunk{Level: domain.ChunkChapter, StartSeconds: 10, EndSeconds: 20, Text: "A chapter about Maria"}, Vector: []float32{0, 1}},
			{SearchChunk: domain.SearchChunk{Level: domain.ChunkChapter, StartSeconds: 0, EndSeconds: 10, Text: "Breath basics"}, Vector: []float32{1, 1}},
		},
	}))
	res := f.export()
	if res.EmbeddingSource != EmbeddingsStored || res.EmbeddingModel != "fake-hash-embed@1" {
		t.Fatalf("result %+v", res)
	}
	db := openDB(t, f.target())
	if got := count(t, db, `SELECT count(*) FROM transcript_segments`); got != 2 {
		t.Errorf("segments %d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM chapters`); got != 1 {
		t.Errorf("chapters %d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM embeddings`); got != 2 {
		t.Errorf("embeddings %d", got)
	}
	raw, _ := os.ReadFile(f.target())
	if bytes.Contains(raw, []byte("Maria")) {
		t.Error("sensitive text in the file")
	}
}

func TestMarkOnTheSourceAfterTheClipWasDerivedStillCounts(t *testing.T) {
	f := newFixture(t, "acme")
	// The clip covers 100-160 of the source. Source 122-125 is clip 22-25, inside the
	// third segment. ai.index copied nothing, because this mark came later.
	f.must(f.store.Transcripts().MarkSensitive(f.ctx(), domain.SensitiveSegment{
		AssetID: f.original.ID, Version: "v1", StartSeconds: 122, EndSeconds: 125,
	}))
	// Outside the clip: must not affect it.
	f.must(f.store.Transcripts().MarkSensitive(f.ctx(), domain.SensitiveSegment{
		AssetID: f.original.ID, Version: "v1", StartSeconds: 0, EndSeconds: 50,
	}))
	f.export()
	db := openDB(t, f.target())
	if got := count(t, db, `SELECT count(*) FROM transcript_segments`); got != 1 {
		t.Fatalf("segments %d: the source's mark must reach the clip", got)
	}
	var text string
	if err := db.QueryRow(`SELECT text FROM transcript_segments`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != publicText {
		t.Fatalf("left %q", text)
	}
}

func TestSensitiveIntervalsAcrossSeveralSegments(t *testing.T) {
	f := newFixture(t, "acme")
	f.clip.Lineage.Segments = []domain.Segment{
		{SourceAssetID: f.original.ID, SourceVersion: "v1", StartSeconds: 100, EndSeconds: 110},
		{SourceAssetID: f.original.ID, SourceVersion: "v1", StartSeconds: 200, EndSeconds: 220},
	}
	f.must(f.store.Transcripts().MarkSensitive(f.ctx(), domain.SensitiveSegment{
		AssetID: f.original.ID, Version: "v1", StartSeconds: 205, EndSeconds: 300,
	}))
	got, err := SensitiveIntervals(f.ctx(), f.store, f.clip, "v1")
	if err != nil {
		t.Fatal(err)
	}
	// Own mark 11-19, then source 205-220 mapped to clip 15-30.
	want := []Interval{{11, 19}, {15, 30}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}

	if _, err := SensitiveIntervals(f.ctx(), f.store, f.clip, "v0"); err == nil {
		t.Fatal("a source mark that cannot be placed on an older version's timeline must be an error")
	}
}

func TestUnreadableMarksWithholdAllTimedText(t *testing.T) {
	f := newFixture(t, "acme")
	f.must(f.store.Search().IndexVectors(f.ctx(), repo.EmbeddingBatch{
		AssetID: id.Ref(f.clip.ID), Version: "v1", Model: "m@1", Chunks: []domain.EmbeddedChunk{
			{SearchChunk: domain.SearchChunk{Level: domain.ChunkWindow, StartSeconds: 0, EndSeconds: 5, Text: "safe"}, Vector: []float32{1}},
		},
	}))
	f.store.SensitiveErr = func(id.Ref, string) error { return errors.New("database is busy") }
	res := f.export()
	if res.Records != 1 || res.TextWithheld != 1 {
		t.Fatalf("result %+v", res)
	}
	db := openDB(t, f.target())
	for _, table := range []string{"transcript_segments", "chapters", "embeddings"} {
		if got := count(t, db, `SELECT count(*) FROM `+table); got != 0 {
			t.Errorf("%s has %d rows", table, got)
		}
	}
	if got := count(t, db, `SELECT count(*) FROM videos`); got != 1 {
		t.Errorf("the video itself stays listed: %d", got)
	}
}

func TestSelectionFailsClosed(t *testing.T) {
	cases := map[string]func(f *fixture){
		"no approval id": func(f *fixture) {
			f.pub.ApprovalID = ""
			f.must(f.store.Publications().Update(f.ctx(), f.pub))
		},
		"unknown approval": func(f *fixture) {
			f.pub.ApprovalID = "APR-999"
			f.must(f.store.Publications().Update(f.ctx(), f.pub))
		},
		"rejected approval": func(f *fixture) {
			f.must(f.store.Approvals().Create(f.ctx(), domain.Approval{
				ID: "APR-001", ULID: id.New(), Kind: domain.ApprovalPublication, Status: domain.ApprovalRejected, AssetID: f.clip.ID,
			}))
		},
		"approval of the selection, not the publication": func(f *fixture) {
			f.must(f.store.Approvals().Create(f.ctx(), domain.Approval{
				ID: "APR-001", ULID: id.New(), Kind: domain.ApprovalClipSelection, Status: domain.ApprovalApproved, AssetID: f.clip.ID,
			}))
		},
		"approval of another asset": func(f *fixture) {
			f.must(f.store.Approvals().Create(f.ctx(), domain.Approval{
				ID: "APR-001", ULID: id.New(), Kind: domain.ApprovalPublication, Status: domain.ApprovalApproved, AssetID: "CLIP-999",
			}))
		},
		"private publication": func(f *fixture) {
			f.pub.Visibility = domain.VisibilityPrivate
			f.must(f.store.Publications().Update(f.ctx(), f.pub))
		},
		"unlisted publication": func(f *fixture) {
			f.pub.Visibility = domain.VisibilityUnlisted
			f.must(f.store.Publications().Update(f.ctx(), f.pub))
		},
		"processing publication": func(f *fixture) {
			f.pub.Status = domain.PubProcessing
			f.must(f.store.Publications().Update(f.ctx(), f.pub))
		},
		"deleted asset": func(f *fixture) {
			f.must(f.store.Assets().SoftDelete(f.ctx(), id.Ref(f.clip.ID), fixedNow))
		},
		"private asset": func(f *fixture) {
			f.clip.Visibility = domain.VisibilityPrivate
			f.must(f.store.Assets().Update(f.ctx(), f.clip))
		},
		"withdrawn asset": func(f *fixture) {
			f.clip.Status = domain.AssetWithdrawn
			f.must(f.store.Assets().Update(f.ctx(), f.clip))
		},
		"a URL that is not http": func(f *fixture) {
			f.pub.URL = "file:///etc/passwd"
			f.must(f.store.Publications().Update(f.ctx(), f.pub))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "acme")
			setup(f)
			res := f.export()
			if res.Records != 0 {
				t.Fatalf("records %d", res.Records)
			}
			if len(res.Withheld) == 0 && name != "processing publication" && name != "private publication" && name != "unlisted publication" {
				t.Fatal("a refused record must be reported")
			}
			if got := count(t, openDB(t, f.target()), `SELECT count(*) FROM videos`); got != 0 {
				t.Fatalf("videos %d", got)
			}
		})
	}
}

func TestPublishedOriginalNeedsNoPublishedStatus(t *testing.T) {
	f := newFixture(t, "acme")
	f.original.Visibility = domain.VisibilityPublic
	f.must(f.store.Assets().Update(f.ctx(), f.original))
	f.must(f.store.Approvals().Create(f.ctx(), domain.Approval{
		ID: "APR-009", ULID: id.New(), Kind: domain.ApprovalPublication, Status: domain.ApprovalApproved, AssetID: f.original.ID,
	}))
	published := fixedNow
	f.must(f.store.Publications().Create(f.ctx(), domain.Publication{
		ID: "PUB-009", ULID: id.New(), AssetID: f.original.ID, Destination: "mock-main", AssetVersion: "v1",
		RemoteID: "mock-9", URL: "https://media.example.internal/watch/mock-9", Status: domain.PubPublished,
		Visibility: domain.VisibilityPublic, ApprovalID: "APR-009", PublishedAt: &published,
	}))
	res := f.export()
	if res.Records != 2 {
		t.Fatalf("records %d: %+v", res.Records, res.Withheld)
	}
	db := openDB(t, f.target())
	if got := count(t, db, `SELECT count(*) FROM videos WHERE kind = 'original' AND source_group = ''`); got != 1 {
		t.Fatalf("originals %d", got)
	}
}

func TestPublicIDsAreStablePerTenantAndUnlinkableBetweenTenants(t *testing.T) {
	publicID := func(f *fixture) (string, string) {
		db := openDB(f.t, f.target())
		var p, g string
		if err := db.QueryRow(`SELECT public_id, source_group FROM videos`).Scan(&p, &g); err != nil {
			f.t.Fatal(err)
		}
		return p, g
	}
	a := newFixture(t, "acme")
	a.export()
	idA, groupA := publicID(a)
	a.export()
	if again, groupAgain := publicID(a); again != idA || groupAgain != groupA {
		t.Fatalf("IDs changed on rebuild: %s %s", again, groupAgain)
	}
	if idA == groupA {
		t.Fatal("public_id and source_group must use separate labels")
	}

	// The same ULID in another tenant derives another ID.
	b := newFixture(t, "globex")
	b.clip.ULID = a.clip.ULID
	b.must(b.store.Assets().Update(b.ctx(), b.clip))
	b.export()
	idB, _ := publicID(b)
	if idA == idB {
		t.Fatal("two tenants produced the same public ID")
	}
	if a.target() == b.target() || !strings.Contains(b.target(), "globex") {
		t.Fatalf("tenants share a path: %s", b.target())
	}
}

func TestClipsOfOneSourceShareAGroup(t *testing.T) {
	f := newFixture(t, "acme")
	f.addPublicClip("CLIP-002", "PUB-002", "APR-002", f.original.ID, 0, 10)
	if res := f.export(); res.Records != 2 {
		t.Fatalf("records %d", res.Records)
	}
	db := openDB(t, f.target())
	if got := count(t, db, `SELECT count(DISTINCT source_group) FROM videos`); got != 1 {
		t.Fatalf("groups %d", got)
	}
	if got := count(t, db, `SELECT count(DISTINCT public_id) FROM videos`); got != 2 {
		t.Fatalf("ids %d", got)
	}
}

func TestSeveralDestinationsAndVersions(t *testing.T) {
	f := newFixture(t, "acme")
	later := fixedNow.Add(time.Minute)
	f.must(f.store.Publications().Create(f.ctx(), domain.Publication{
		ID: "PUB-002", ULID: id.New(), AssetID: f.clip.ID, Destination: "youtube-main", AssetVersion: "v2",
		RemoteID: "yt-1", URL: "https://www.youtube.com/watch?v=yt-1", Status: domain.PubPublished,
		Visibility: domain.VisibilityPublic, ApprovalID: "APR-001", Title: "Second version title", PublishedAt: &later,
	}))
	req := f.request()
	req.Drivers["youtube-main"] = "youtube"
	if _, err := f.exporter.Export(f.ctx(), req); err != nil {
		t.Fatal(err)
	}
	db := openDB(t, f.target())
	if got := count(t, db, `SELECT count(*) FROM destinations`); got != 2 {
		t.Fatalf("destinations %d", got)
	}
	var title, published string
	if err := db.QueryRow(`SELECT title, published_at FROM videos`).Scan(&title, &published); err != nil {
		t.Fatal(err)
	}
	if title != "Second version title" || published != "2026-11-09T09:00:00Z" {
		t.Fatalf("title %q (newest wins), published %q (earliest wins)", title, published)
	}
	// v2 has no transcript, so nothing from v1's is carried onto another timeline.
	if got := count(t, db, `SELECT count(*) FROM transcript_segments`); got != 0 {
		t.Fatalf("segments %d", got)
	}
}

func TestAnUnknownDestinationNeverLeaksItsLogicalName(t *testing.T) {
	f := newFixture(t, "acme")
	req := f.request()
	req.Drivers = nil
	if _, err := f.exporter.Export(f.ctx(), req); err != nil {
		t.Fatal(err)
	}
	var driver string
	if err := openDB(t, f.target()).QueryRow(`SELECT destination FROM destinations`).Scan(&driver); err != nil {
		t.Fatal(err)
	}
	if driver != "other" {
		t.Fatalf("destination %q", driver)
	}
}

func TestGuardDropsOnlyThePoisonedVideo(t *testing.T) {
	cases := map[string]func(*fixture, *domain.Publication, *domain.Asset){
		"storage key in the title": func(_ *fixture, p *domain.Publication, _ *domain.Asset) {
			p.Title = "see clips/CLIP-002/v1/master.mp4"
		},
		"storage URI in the description": func(_ *fixture, p *domain.Publication, _ *domain.Asset) {
			p.Description = "from storage://master/somewhere"
		},
		"ULID in the title": func(_ *fixture, p *domain.Publication, _ *domain.Asset) {
			p.Title = "ref " + string(id.New())
		},
		"ULID in a tag": func(_ *fixture, _ *domain.Publication, a *domain.Asset) {
			a.Tags = []string{string(id.New())}
		},
		"storage key in the URL": func(_ *fixture, p *domain.Publication, _ *domain.Asset) {
			p.URL = "https://media.example.internal/clips/CLIP-002/v1/master.mp4"
		},
	}
	for name, poison := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "acme")
			clip, pub := f.addPublicClip("CLIP-002", "PUB-002", "APR-002", f.original.ID, 0, 10)
			poison(f, &pub, &clip)
			f.must(f.store.Publications().Update(f.ctx(), pub))
			f.must(f.store.Assets().Update(f.ctx(), clip))
			res := f.export()
			if res.Records != 1 || len(res.Withheld) != 1 || res.Withheld[0].Asset != "CLIP-002" {
				t.Fatalf("result %+v", res)
			}
			if strings.Contains(res.Withheld[0].Reason, "CLIP-002/v1") {
				t.Fatalf("the reason must not repeat the value: %q", res.Withheld[0].Reason)
			}
		})
	}
}

func TestFriendlyIDInADescriptionIsNotAnError(t *testing.T) {
	f := newFixture(t, "acme")
	f.pub.Description = "Part two of CLIP-001."
	f.must(f.store.Publications().Update(f.ctx(), f.pub))
	if res := f.export(); res.Records != 1 {
		t.Fatalf("records %d: %+v", res.Records, res.Withheld)
	}
}

type fakeEmbedder struct {
	model    string
	external bool
	err      error
	texts    []string
}

func (e *fakeEmbedder) Info() connector.Info {
	return connector.Info{Driver: "fake", Capabilities: map[string]bool{"external": e.external}}
}

func (e *fakeEmbedder) Embed(_ context.Context, req connector.EmbedRequest) (connector.EmbedResult, error) {
	if e.err != nil {
		return connector.EmbedResult{}, e.err
	}
	e.texts = append(e.texts, req.Texts...)
	out := connector.EmbedResult{Model: e.model, Dimensions: 3}
	for range req.Texts {
		out.Vectors = append(out.Vectors, []float32{0.5, -0.25, 1.5})
	}
	return out, nil
}

func TestComputedVectorsUseOnlyReleasedText(t *testing.T) {
	f := newFixture(t, "acme")
	f.store.NoEmbeddingLister = true
	emb := &fakeEmbedder{model: "fake-hash-embed@1"}
	req := f.request()
	req.Embedder = emb
	res, err := f.exporter.Export(f.ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.EmbeddingSource != EmbeddingsComputed || res.EmbeddingModel != "fake-hash-embed@1" {
		t.Fatalf("result %+v", res)
	}
	joined := strings.Join(emb.texts, " ")
	if strings.Contains(joined, "Maria") || strings.Contains(joined, "diagnosis") {
		t.Fatalf("sensitive text sent to the embedder: %q", joined)
	}
	if !strings.Contains(joined, "conscious breathing") {
		t.Fatalf("released text missing: %q", joined)
	}

	db := openDB(t, f.target())
	var model, dims string
	db.QueryRow(`SELECT value FROM meta WHERE key = 'embedding_model'`).Scan(&model)
	db.QueryRow(`SELECT value FROM meta WHERE key = 'embedding_dimensions'`).Scan(&dims)
	if model != "fake-hash-embed@1" || dims != "3" {
		t.Fatalf("meta %q %q", model, dims)
	}
	// A window per contiguous run around the sensitive interval, and one chapter.
	if got := count(t, db, `SELECT count(*) FROM embeddings WHERE level = 'window'`); got != 2 {
		t.Errorf("windows %d", got)
	}
	if got := count(t, db, `SELECT count(*) FROM embeddings WHERE level = 'chapter'`); got != 1 {
		t.Errorf("chapter vectors %d", got)
	}

	// Vectors are little-endian float32, so the site reads them with no extension.
	var blob []byte
	if err := db.QueryRow(`SELECT vector FROM embeddings LIMIT 1`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if len(blob) != 12 {
		t.Fatalf("blob %d bytes", len(blob))
	}
	for i, want := range []float32{0.5, -0.25, 1.5} {
		if got := math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:])); got != want {
			t.Errorf("component %d: %v, want %v", i, got, want)
		}
	}
}

func TestNoVectorsMeansLexicalOnly(t *testing.T) {
	cases := map[string]func(*fixture, *Request){
		"no embedder": func(f *fixture, r *Request) { f.store.NoEmbeddingLister = true },
		"embedder fails": func(f *fixture, r *Request) {
			f.store.NoEmbeddingLister = true
			r.Embedder = &fakeEmbedder{model: "m@1", err: errs.New(errs.CodeUnavailable, "down")}
		},
		"embedder is external": func(f *fixture, r *Request) {
			f.store.NoEmbeddingLister = true
			r.Embedder = &fakeEmbedder{model: "m@1", external: true}
		},
		"store has no active model": func(f *fixture, r *Request) {},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "acme")
			req := f.request()
			setup(f, &req)
			res, err := f.exporter.Export(f.ctx(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.EmbeddingSource != EmbeddingsNone || res.EmbeddingModel != "" || res.Records != 1 {
				t.Fatalf("result %+v", res)
			}
			db := openDB(t, f.target())
			if got := count(t, db, `SELECT count(*) FROM embeddings`); got != 0 {
				t.Fatalf("embeddings %d", got)
			}
			var model string
			if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'embedding_model'`).Scan(&model); err != nil || model != "" {
				t.Fatalf("embedding_model %q, %v", model, err)
			}
		})
	}
}

func TestWindows(t *testing.T) {
	var segs []domain.TranscriptSegment
	for i := range 40 {
		segs = append(segs, domain.TranscriptSegment{Start: float64(i * 5), End: float64(i*5 + 5), Text: "words"})
	}
	windows := Windows(segs, nil)
	if len(windows) < 4 {
		t.Fatalf("windows %d", len(windows))
	}
	for i, w := range windows {
		if d := w.EndSeconds - w.StartSeconds; d > 60 || (d < 30 && i < len(windows)-1) {
			t.Errorf("window %d lasts %v", i, d)
		}
		if i > 0 {
			overlap := windows[i-1].EndSeconds - w.StartSeconds
			if overlap <= 0 || overlap > 10 {
				t.Errorf("window %d overlaps by %v", i, overlap)
			}
		}
	}
	if windows[len(windows)-1].EndSeconds != 200 {
		t.Errorf("the last segment is missing: %+v", windows[len(windows)-1])
	}

	// A single long segment still terminates.
	if got := Windows([]domain.TranscriptSegment{{Start: 0, End: 500, Text: "a"}, {Start: 500, End: 501, Text: "b"}}, nil); len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got := Windows(nil, nil); got != nil {
		t.Fatalf("got %+v", got)
	}

	// Never across a sensitive interval, even by way of overlap.
	marks := []Interval{{98, 102}}
	gapped := ReleasedSegments(domain.Transcript{Segments: segs}, marks)
	for _, w := range Windows(gapped, marks) {
		if Overlaps(marks, w.StartSeconds, w.EndSeconds) {
			t.Errorf("window %v-%v spans the mark", w.StartSeconds, w.EndSeconds)
		}
	}
}

func TestExporterNeedsItsConfiguration(t *testing.T) {
	store := fakestore.New("acme")
	ctx := context.Background()
	if _, err := New(Options{Keys: keys{}}).Export(ctx, Request{Tenant: "acme", Store: store}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
	if _, err := New(Options{DataDir: t.TempDir()}).Export(ctx, Request{Tenant: "acme", Store: store}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
	if _, err := New(Options{DataDir: t.TempDir(), Keys: keys{}}).Export(ctx, Request{Tenant: "../up", Store: store}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("got %v", err)
	}
}
