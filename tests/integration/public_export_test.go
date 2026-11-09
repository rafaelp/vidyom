package integration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/catalog"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/persistence"
	"github.com/rafaelp/vidyom/internal/publicexport"
)

const (
	tenant        = domain.TenantID("acme")
	sharedText    = "the breath moves through the body without any effort"
	sensitiveText = "she told the room about her diagnosis in great detail"
	closingText   = "and the class ends with a long and quiet rest"
)

type keys struct{}

func (keys) CacheKey(context.Context, domain.TenantID) ([]byte, error) {
	return bytes.Repeat([]byte{1}, 32), nil
}

func (keys) PublicIDKey(context.Context, domain.TenantID) ([]byte, error) {
	return bytes.Repeat([]byte{2}, 32), nil
}

// failingEmbedder proves a sync never re-embeds what the store already has.
type failingEmbedder struct{ t *testing.T }

func (f failingEmbedder) Info() connector.Info { return connector.Info{Driver: "failing"} }
func (f failingEmbedder) Embed(context.Context, connector.EmbedRequest) (connector.EmbedResult, error) {
	f.t.Error("the exporter called the embedder although the store holds vectors")
	return connector.EmbedResult{}, errors.New("must not be called")
}

type world struct {
	t       *testing.T
	ctx     context.Context
	store   repo.TenantStore
	stores  *persistence.Stores
	dataDir string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	store, err := stores.Tenants.Create(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, ctx: ctx, store: store, stores: stores, dataDir: dataDir}
}

func (w *world) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

// addAsset creates an asset whose transcript has one 10 s segment per text. With a
// source, it covers source seconds 0 to 10*len(texts), on the same words.
func (w *world) addAsset(kind domain.AssetKind, friendly string, source *domain.Asset, status domain.AssetStatus, texts ...string) domain.Asset {
	w.t.Helper()
	span := float64(10 * len(texts))
	asset := domain.Asset{
		ID: id.Friendly(friendly), ULID: id.New(), TenantID: tenant, Kind: kind,
		Title: "Title of " + friendly, Language: "en", DurationSeconds: span,
		Status: status, Visibility: domain.VisibilityPrivate, CurrentVersion: "v1",
	}
	if source != nil {
		root := source.ID
		if source.Lineage != nil {
			root = source.Lineage.RootAssetID
		}
		asset.ParentAssetID = source.ID
		asset.Lineage = &domain.Lineage{
			Method: domain.LineageComputed, SourceAssetID: source.ID, RootAssetID: root,
			Segments: []domain.Segment{{SourceAssetID: source.ID, SourceVersion: "v1", StartSeconds: 0, EndSeconds: span}},
		}
	}
	w.must(w.store.Assets().Create(w.ctx, asset))
	w.must(w.store.Versions().Create(w.ctx, domain.AssetVersion{AssetID: asset.ID, Version: "v1"}))
	w.must(w.store.Versions().PutFile(w.ctx, id.Ref(asset.ID), "v1", domain.VersionFile{Purpose: domain.FileWorkingMaster, File: domain.MediaFile{
		Storage: domain.RoleMaster, Key: "keys/" + friendly + "/master.mp4", SHA256: strings.Repeat("a", 64), SizeBytes: 1, Verified: true,
	}}))
	transcript := domain.Transcript{AssetID: asset.ID, Version: "v1", Language: "en", DurationSeconds: span}
	chunks := make([]domain.EmbeddedChunk, 0, len(texts))
	lexical := make([]domain.SearchChunk, 0, len(texts))
	for i, text := range texts {
		start := float64(10 * i)
		transcript.Segments = append(transcript.Segments, domain.TranscriptSegment{Start: start, End: start + 10, Text: text})
		chunk := domain.SearchChunk{Level: domain.ChunkWindow, StartSeconds: start, EndSeconds: start + 10, Text: text}
		lexical = append(lexical, chunk)
		chunks = append(chunks, domain.EmbeddedChunk{SearchChunk: chunk, Vector: []float32{1, float32(i), 0}})
	}
	w.must(w.store.Transcripts().Save(w.ctx, transcript))
	w.must(w.store.Search().IndexLexical(w.ctx, id.Ref(asset.ID), "v1", lexical))
	w.must(w.store.Search().IndexVectors(w.ctx, repo.EmbeddingBatch{
		AssetID: id.Ref(asset.ID), Version: "v1", Model: "fake-hash-embed@1", Chunking: "test@1", Chunks: chunks,
	}))
	stored, err := w.store.Assets().Get(w.ctx, id.Ref(asset.ID))
	w.must(err)
	return stored
}

// publish makes an asset public the way the workflows leave it: an approved approval and
// a published record.
func (w *world) publish(asset domain.Asset, publication, approval string) {
	w.t.Helper()
	decided := time.Now().UTC()
	job, err := w.store.IDs().Next(w.ctx, id.PrefixJob)
	w.must(err)
	w.must(w.store.Jobs().Create(w.ctx, domain.Job{
		ID: job, ULID: id.New(), TenantID: tenant, WorkflowID: "publish-video", WorkflowVersion: "1.0.0",
		WorkflowSHA256: strings.Repeat("b", 64), Snapshot: []byte(`{}`), Status: domain.JobCompleted,
		Trigger: domain.JobTrigger{Type: "cli"}, AssetID: asset.ID, SubmittedBy: "publisher@example.com",
	}))
	w.must(w.store.Approvals().Create(w.ctx, domain.Approval{
		ID: id.Friendly(approval), ULID: id.New(), TenantID: tenant, Kind: domain.ApprovalPublication,
		Status: domain.ApprovalApproved, AssetID: asset.ID, AssetVersion: "v1", JobID: job, NodeID: "gate",
		Approvers: []string{"publisher"}, DecidedBy: "publisher@example.com", DecidedAt: &decided,
	}))
	now := time.Now().UTC()
	w.must(w.store.Publications().Create(w.ctx, domain.Publication{
		ID: id.Friendly(publication), ULID: id.New(), TenantID: tenant, AssetID: asset.ID, Destination: "mock-main",
		AssetVersion: "v1", RemoteID: "mock-1", URL: "https://media.example.internal/watch/mock-1",
		Status: domain.PubPublished, Visibility: domain.VisibilityPublic, ApprovalID: id.Friendly(approval),
		Title: asset.Title, IdempotencyKey: publication, PublishedAt: &now,
	}))
	asset.Status = domain.AssetApproved
	w.must(w.store.Assets().Update(w.ctx, asset))
	asset.Status, asset.Visibility = domain.AssetPublished, domain.VisibilityPublic
	w.must(w.store.Assets().Update(w.ctx, asset))
}

func (w *world) export(embedder connector.Embedder) (publicexport.Result, []byte) {
	w.t.Helper()
	exporter := publicexport.New(publicexport.Options{DataDir: w.dataDir, Keys: keys{}})
	result, err := exporter.Export(w.ctx, publicexport.Request{
		Tenant: tenant, Store: w.store, Drivers: map[string]string{"mock-main": "mock"}, Embedder: embedder,
	})
	w.must(err)
	raw, err := os.ReadFile(filepath.Join(w.dataDir, "tenants", string(tenant), "public.db"))
	w.must(err)
	return result, raw
}

// A sync reuses the vectors stored at indexing, so it does not re-embed the tenant each
// time (I232). The exporter takes a stored vector from the real SearchRepo.
func TestExportReusesStoredVectors(t *testing.T) {
	w := newWorld(t)
	original := w.addAsset(domain.KindOriginal, "ORI-001", nil, domain.AssetReadyForClipping, sharedText, closingText)
	clip := w.addAsset(domain.KindClip, "CLIP-001", &original, domain.AssetEditorialReview, sharedText, closingText)
	w.publish(clip, "PUB-001", "APR-001")

	if _, ok := w.store.Search().(repo.EmbeddingLister); !ok {
		t.Fatal("the real SearchRepo does not implement repo.EmbeddingLister")
	}
	result, raw := w.export(failingEmbedder{t})
	if result.EmbeddingSource != publicexport.EmbeddingsStored || result.EmbeddingModel != "fake-hash-embed@1" {
		t.Fatalf("embedding source %q, model %q", result.EmbeddingSource, result.EmbeddingModel)
	}
	if !bytes.Contains(raw, []byte(sharedText)) {
		t.Error("the transcript is not in public.db")
	}
}

// D30 on the real store: a mark on a grandparent, added after a clip of a clip was cut,
// keeps that text out of public.db and out of a viewer's transcript (I284).
func TestLateMarkOnAGrandparentReachesAClipOfAClip(t *testing.T) {
	w := newWorld(t)
	original := w.addAsset(domain.KindOriginal, "ORI-001", nil, domain.AssetReadyForClipping, sharedText, sensitiveText, closingText)
	parent := w.addAsset(domain.KindClip, "CLIP-001", &original, domain.AssetEditorialReview, sharedText, sensitiveText, closingText)
	child := w.addAsset(domain.KindClip, "CLIP-002", &parent, domain.AssetEditorialReview, sharedText, sensitiveText, closingText)
	w.publish(child, "PUB-001", "APR-001")

	_, before := w.export(nil)
	if !bytes.Contains(before, []byte(sensitiveText)) {
		t.Fatal("the text should be public before the mark")
	}

	w.must(w.store.Transcripts().MarkSensitive(w.ctx, domain.SensitiveSegment{
		AssetID: original.ID, Version: "v1", StartSeconds: 10, EndSeconds: 20, Reason: "health",
	}))

	stored, err := w.store.Assets().Get(w.ctx, id.Ref(child.ID))
	w.must(err)
	marks, err := publicexport.SensitiveIntervals(w.ctx, w.store, stored, "v1")
	w.must(err)
	if len(marks) != 1 || marks[0].Start != 10 || marks[0].End != 20 {
		t.Fatalf("marks mapped onto the clip of a clip: %+v", marks)
	}

	result, after := w.export(nil)
	if bytes.Contains(after, []byte(sensitiveText)) {
		t.Error("the grandparent's sensitive text is in public.db")
	}
	if !bytes.Contains(after, []byte(sharedText)) || result.Records != 1 {
		t.Errorf("the clip should stay, without the marked sentence: %d records", result.Records)
	}

	// Private reads and search agree, through the same chain.
	assets := catalog.New(w.stores.Tenants, catalog.Options{})
	viewer := service.Scope{Tenant: tenant, Principal: domain.Principal{TenantID: tenant, Role: domain.RoleViewer}}
	transcript, err := assets.Transcript(w.ctx, viewer, id.Ref(child.ID), "v1")
	w.must(err)
	for _, seg := range transcript.Segments {
		if strings.Contains(seg.Text, sensitiveText) {
			t.Error("a viewer read the grandparent's sensitive text")
		}
	}
	search := catalog.NewSearchGuard(stubSearch{hit: domain.SearchHit{AssetID: child.ID, Version: "v1", StartSeconds: 10, EndSeconds: 20, Text: sensitiveText}}, w.stores.Tenants)
	found, err := search.Search(w.ctx, viewer, api.SearchQuery{Q: "diagnosis"})
	w.must(err)
	if len(found.Items) != 0 {
		t.Errorf("a viewer found the marked sentence through search: %+v", found.Items)
	}
}

type stubSearch struct{ hit domain.SearchHit }

func (s stubSearch) Search(context.Context, service.Scope, api.SearchQuery) (api.SearchResponse, error) {
	return api.SearchResponse{Items: []domain.SearchHit{s.hit}}, nil
}
