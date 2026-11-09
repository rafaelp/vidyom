package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/workflows"
)

// memStorage is a storage role in memory: what catalog.delete, catalog.export_package
// and listing archives touch.
type memStorage struct {
	connector.Storage
	mu   sync.Mutex
	role domain.StorageRole
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
	return connector.ObjectInfo{Key: key, SizeBytes: int64(len(data))}, nil
}

func (m *memStorage) Get(_ context.Context, key string) (io.ReadCloser, connector.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objs[key]
	if !ok {
		return nil, connector.ObjectInfo{}, errs.NotFound("object")
	}
	return io.NopCloser(bytes.NewReader(data)), connector.ObjectInfo{Key: key, SizeBytes: int64(len(data))}, nil
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

type memResolver struct {
	roles map[domain.StorageRole]*memStorage
}

func (r memResolver) Storage(_ context.Context, _ domain.TenantID, role domain.StorageRole) (connector.Storage, error) {
	return r.roles[role], nil
}

func newResolver() memResolver {
	r := memResolver{roles: map[domain.StorageRole]*memStorage{}}
	for _, role := range domain.StorageRoles {
		r.roles[role] = &memStorage{role: role, objs: map[string][]byte{}}
	}
	return r
}

// studio is a harness with the shipped workflow files, and fakes of the nodes of
// other components that behave like their catalog entries.
type studio struct {
	*harness
	storage memResolver
	uploads atomic.Int32
	reads   sync.Map // URI -> true: everything a render or a transcode read
	file    string
}

func newStudio(t *testing.T, register ...func(node.Registry)) *studio {
	t.Helper()
	opts := harnessOpts{noEngine: true}
	if len(register) > 0 {
		opts.register = register[0]
	}
	h := newHarness(t, opts)
	h.stopWorker()
	s := &studio{harness: h, storage: newResolver()}
	h.deps.Storage = s.storage
	if err := h.wfs.LoadFS(workflows.FS, "workflows"); err != nil {
		t.Fatal(err)
	}
	if err := h.wfs.Check(); err != nil {
		t.Fatal(err)
	}
	s.fakes()
	h.startEngine()
	h.startWorker()
	s.file = filepath.Join(h.deps.Daemon.IngestRoots[0], "class.mov")
	if err := os.WriteFile(s.file, []byte("raw"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func scratchOf(env node.Env, in node.Input, name string) string {
	if w, ok := env.(*workerEnv); ok {
		return fmt.Sprintf("scratch://%s/%s/%s", w.lease.JobULID, w.lease.NodeID, name)
	}
	return fmt.Sprintf("scratch://%s/%s/%s", env.JobULID(), env.NodeID(), name)
}

func (s *studio) fakes() {
	sha := func(in string) string { return fmt.Sprintf("%064x", len(in)) }
	s.on("video.probe", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"ext": "mov", "duration_seconds": 3600, "width": 1920, "height": 1080})
	})
	s.on("storage.put", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		var p struct {
			Storage domain.StorageRole `json:"storage"`
			Source  string             `json:"source"`
			Key     string             `json:"key"`
		}
		if err := in.Bind(&p); err != nil {
			return node.Result{}, node.Permanent(err)
		}
		s.reads.Store(p.Source, true)
		uri := domain.InputURI{Kind: domain.URIStorage, Role: p.Storage, Key: p.Key}
		if st := s.storage.roles[p.Storage]; st != nil {
			st.mu.Lock()
			st.objs[p.Key] = []byte("object")
			st.mu.Unlock()
		}
		return node.JSONResult(map[string]any{"uri": uri.String(), "storage": p.Storage, "key": p.Key, "sha256": sha(p.Key), "size_bytes": 1024, "verified": true})
	})
	s.on("storage.get", func(_ context.Context, env node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"file": scratchOf(env, in, "raw.mov")})
	})
	s.on("storage.copy", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		var p struct {
			Storage domain.StorageRole `json:"storage"`
			Key     string             `json:"key"`
		}
		_ = in.Bind(&p)
		uri := domain.InputURI{Kind: domain.URIStorage, Role: p.Storage, Key: p.Key}
		return node.JSONResult(map[string]any{"uri": uri.String(), "storage": p.Storage, "key": p.Key, "verified": true})
	})
	s.on("storage.verify", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"verified": true})
	})
	s.on("storage.delete", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"deleted": true})
	})
	s.on("video.transcode", func(_ context.Context, env node.Env, in node.Input) (node.Result, error) {
		var p struct {
			Profile string `json:"profile"`
		}
		_ = in.Bind(&p)
		return node.JSONResult(map[string]any{"file": scratchOf(env, in, "out.mp4"), "profile": p.Profile + "@1", "duration_seconds": 3600.0, "ffmpeg_version": "7.1", "sha256": sha(p.Profile)})
	})
	s.on("video.thumbnail", func(_ context.Context, env node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"file": scratchOf(env, in, "thumb.jpg")})
	})
	s.on("audio.extract", func(_ context.Context, env node.Env, in node.Input) (node.Result, error) {
		var p struct {
			Source string `json:"source"`
		}
		_ = in.Bind(&p)
		s.reads.Store("audio:"+p.Source, true)
		return node.JSONResult(map[string]any{"file": scratchOf(env, in, "speech.ogg")})
	})
	s.on("video.validate_timeline", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"valid": true})
	})
	s.on("video.preview", func(_ context.Context, env node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"file": scratchOf(env, in, "preview.mp4")})
	})
	s.on("video.cut", func(_ context.Context, env node.Env, in node.Input) (node.Result, error) {
		var p struct {
			Source   string           `json:"source"`
			Segments []domain.Segment `json:"segments"`
		}
		_ = in.Bind(&p)
		s.reads.Store("cut:"+p.Source, true)
		u, err := domain.ParseInputURI(p.Source)
		if err != nil {
			return node.Result{}, node.Permanent(err)
		}
		if err := u.RequireRole(domain.RoleMaster, false); err != nil {
			return node.Result{}, node.Permanent(err)
		}
		return node.JSONResult(map[string]any{"file": scratchOf(env, in, "clip.mp4"), "segments": p.Segments, "duration_seconds": 30.0})
	})
	transcript := domain.Transcript{Language: "en", DurationSeconds: 3600, TimestampBasis: "working_master", Engine: "fake/canned@1", Segments: []domain.TranscriptSegment{{Start: 10, End: 20, Text: "conscious breathing"}}}
	s.on("ai.transcribe", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"transcript": transcript, "engine": transcript.Engine, "language": "en", "duration_seconds": 3600})
	})
	s.on("ai.derive_transcript", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"transcript": transcript})
	})
	s.on("ai.analyze", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"available": false})
	})
	s.on("ai.embed", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"model": "fake-hash-embed@1", "dimensions": 4, "chunking": "window", "chunks": []any{}})
	})
	s.on("ai.index", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		d, err := node.RequireDaemon(env)
		if err != nil {
			return node.Result{}, err
		}
		var p struct {
			AssetID string `json:"assetId"`
			Version string `json:"version"`
		}
		_ = in.Bind(&p)
		a, err := d.Store().Assets().Get(ctx, id.Ref(p.AssetID))
		if err != nil {
			return node.Result{}, node.Permanent(err)
		}
		saved := transcript
		saved.AssetID, saved.Version = a.ID, p.Version
		err = d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
			if err := tx.Transcripts().Save(ctx, saved); err != nil {
				return err
			}
			chunk := domain.SearchChunk{Level: domain.ChunkWindow, StartSeconds: 10, EndSeconds: 20, Text: "conscious breathing"}
			if err := tx.Search().IndexLexical(ctx, id.Ref(a.ID), p.Version, []domain.SearchChunk{chunk}); err != nil {
				return err
			}
			if err := tx.Search().IndexVectors(ctx, repo.EmbeddingBatch{AssetID: id.Ref(a.ID), Version: p.Version, Model: "fake-hash-embed@1", Chunking: "window",
				Chunks: []domain.EmbeddedChunk{{SearchChunk: chunk, Vector: []float32{1, 0, 0, 0}}}}); err != nil {
				return err
			}
			if err := d.Emit(ctx, tx, event.Draft{Type: event.AssetIndexed, AssetID: string(a.ID)}); err != nil {
				return err
			}
			if a.Kind == domain.KindOriginal {
				return d.Emit(ctx, tx, event.Draft{Type: event.OriginalIndexed, AssetID: string(a.ID)})
			}
			return nil
		})
		if err != nil {
			return node.Result{}, node.Transient(err)
		}
		return node.JSONResult(map[string]any{"indexed": true, "lexical_documents": 1, "vectors": 1, "embedding_model": "fake-hash-embed@1"})
	})
	s.on("ai.export_transcript", func(_ context.Context, env node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"files": map[string]any{
			"json": map[string]any{"file": scratchOf(env, in, "t.json")},
			"srt":  map[string]any{"file": scratchOf(env, in, "t.srt")},
			"vtt":  map[string]any{"file": scratchOf(env, in, "t.vtt")},
		}})
	})
	s.on("ai.find_segment", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		seg := map[string]any{"start_seconds": 648.0, "end_seconds": 1080.0}
		return node.JSONResult(map[string]any{
			"segments": []any{seg}, "ambiguous": false, "confidence": 0.8, "excerpt": "the story",
			"candidates": []any{map[string]any{"segments": []any{seg}, "excerpt": "the story", "confidence": 0.8}},
		})
	})
	s.on("ai.editorial", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"title": "A generated title", "description": "A draft", "tags": []string{"yoga"}, "generated": true})
	})
	s.on("publish.prepare", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		d, err := node.RequireDaemon(env)
		if err != nil {
			return node.Result{}, err
		}
		var p struct {
			AssetID     string `json:"assetId"`
			Destination string `json:"destination"`
			ApprovalID  string `json:"approvalId"`
			Visibility  string `json:"visibility"`
			Title       string `json:"title"`
		}
		_ = in.Bind(&p)
		if p.ApprovalID == "" {
			return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition, "no approval"))
		}
		a, err := d.Store().Assets().Get(ctx, id.Ref(p.AssetID))
		if err != nil {
			return node.Result{}, node.Permanent(err)
		}
		if existing, found, _ := d.Store().Publications().FindByIdempotencyKey(ctx, in.IdempotencyKey); found {
			return node.JSONResult(map[string]any{"publication_id": existing.ID, "asset_version": existing.AssetVersion})
		}
		var pub domain.Publication
		err = d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
			next, err := tx.IDs().Next(ctx, id.PrefixPublication)
			if err != nil {
				return err
			}
			pub = domain.Publication{ID: next, ULID: id.New(), TenantID: env.Tenant(), AssetID: a.ID, Destination: p.Destination, AssetVersion: a.CurrentVersion,
				Status: domain.PubPendingApproval, Visibility: domain.Visibility(p.Visibility), ApprovalID: id.Friendly(p.ApprovalID), Title: p.Title, IdempotencyKey: in.IdempotencyKey}
			if err := tx.Publications().Create(ctx, pub); err != nil {
				return err
			}
			return d.Emit(ctx, tx, event.Draft{Type: event.PublicationCreated, AssetID: string(a.ID)})
		})
		if err != nil {
			return node.Result{}, node.Transient(err)
		}
		return node.JSONResult(map[string]any{"publication_id": pub.ID, "asset_version": pub.AssetVersion, "source_uri": a.WorkingMaster.URI()})
	})
	s.on("publish.upload", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		s.uploads.Add(1)
		return node.JSONResult(map[string]any{"remote_id": "r1", "status": "processing"})
	})
	s.on("publish.await_processing", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"status": "processing"})
	})
	s.on("publish.finalize", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		d, err := node.RequireDaemon(env)
		if err != nil {
			return node.Result{}, err
		}
		var p struct {
			PublicationID string `json:"publicationId"`
		}
		_ = in.Bind(&p)
		const url = "https://media.example.internal/watch/r1"
		err = d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
			pub, err := tx.Publications().Get(ctx, id.Ref(p.PublicationID))
			if err != nil {
				return err
			}
			pub.Status, pub.RemoteID, pub.URL = domain.PubPublished, "r1", url
			if err := tx.Publications().Update(ctx, pub); err != nil {
				return err
			}
			a, err := tx.Assets().Get(ctx, id.Ref(pub.AssetID))
			if err != nil {
				return err
			}
			a.Status = domain.AssetPublished
			a.Visibility = domain.VisibilityPublic
			if err := tx.Assets().Update(ctx, a); err != nil {
				return err
			}
			if err := tx.Versions().MarkImmutable(ctx, id.Ref(a.ID), pub.AssetVersion); err != nil {
				return err
			}
			return d.Emit(ctx, tx, event.Draft{Type: event.PublicationCompleted, AssetID: string(a.ID), Payload: map[string]any{"visibility": "public"}})
		})
		if err != nil {
			return node.Result{}, node.Transient(err)
		}
		return node.JSONResult(map[string]any{"publication_id": p.PublicationID, "status": "published", "url": url, "visibility": "public"})
	})
	s.on("publish.withdraw", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		d, err := node.RequireDaemon(env)
		if err != nil {
			return node.Result{}, err
		}
		var p struct {
			AssetID string `json:"assetId"`
		}
		_ = in.Bind(&p)
		withdrawn := []string{}
		err = d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
			pubs, _, err := tx.Publications().List(ctx, repo.PublicationFilter{AssetID: id.Ref(p.AssetID)}, repo.Page{})
			if err != nil {
				return err
			}
			for _, pub := range pubs {
				if pub.Status == domain.PubWithdrawn {
					continue
				}
				pub.Status = domain.PubWithdrawn
				if err := tx.Publications().Update(ctx, pub); err != nil {
					return err
				}
				withdrawn = append(withdrawn, string(pub.ID))
				if err := d.Emit(ctx, tx, event.Draft{Type: event.PublicationWithdrawn, AssetID: string(pub.AssetID)}); err != nil {
					return err
				}
			}
			a, err := tx.Assets().Get(ctx, id.Ref(p.AssetID))
			if err != nil {
				return err
			}
			if len(withdrawn) > 0 {
				a.Status = domain.AssetWithdrawn
				a.Visibility = domain.VisibilityPrivate
				return tx.Assets().Update(ctx, a)
			}
			return nil
		})
		if err != nil {
			return node.Result{}, node.Transient(err)
		}
		return node.JSONResult(map[string]any{"withdrawn": withdrawn, "already": []string{}})
	})
	s.on("catalog.public.sync", func(_ context.Context, _ node.Env, in node.Input) (node.Result, error) {
		return node.JSONResult(map[string]any{"records": 1, "sha256": "x", "destination": "local-file"})
	})
}

func (s *studio) ingest() domain.Asset {
	s.t.Helper()
	j := s.runAs(editor, "ingest-original", map[string]any{"file": s.file, "title": "Hatha Yoga Class", "category": "yoga_class"})
	done := s.waitStatus(j, domain.JobCompleted)
	a, err := s.store.Assets().Get(context.Background(), id.Ref(done.AssetID))
	if err != nil {
		s.t.Fatal(err)
	}
	return a
}

func TestIngestOriginalEndToEnd(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	j := s.runAs(editor, "ingest-original", map[string]any{"file": s.file, "title": "Hatha Yoga Class", "category": "yoga_class"})
	done := s.waitStatus(j, domain.JobCompleted)
	if done.AssetID != "ORI-001" {
		t.Fatalf("asset %q", done.AssetID)
	}
	a, err := s.store.Assets().Get(ctx, id.Ref("ORI-001"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.AssetReadyForClipping || !a.ClipEligible() {
		t.Fatalf("asset %+v", a)
	}
	if a.Visibility != domain.VisibilityPrivate || a.Category != "yoga_class" || a.DurationSeconds != 3600 {
		t.Fatalf("asset %+v", a)
	}
	// Roles, versioned keys and the original extension (R26, §14.1).
	if a.SourceOriginal == nil || a.SourceOriginal.Storage != domain.RoleArchive || a.SourceOriginal.Key != "originals/ORI-001/v1/original.mov" || !a.SourceOriginal.Verified {
		t.Fatalf("raw %+v", a.SourceOriginal)
	}
	if a.WorkingMaster == nil || a.WorkingMaster.Storage != domain.RoleMaster || a.WorkingMaster.Key != "originals/ORI-001/v1/master.mp4" {
		t.Fatalf("master %+v", a.WorkingMaster)
	}
	if a.Proxy == nil || a.Proxy.Storage != domain.RoleProxy || a.Proxy.Key != "originals/ORI-001/v1/proxy.mp4" {
		t.Fatalf("proxy %+v", a.Proxy)
	}
	if a.Thumbnail == nil || a.Transcript == nil || !a.Transcript.Indexed {
		t.Fatalf("asset %+v", a)
	}
	// ASR audio comes from the stored master (D56).
	if _, read := s.reads.Load("audio:storage://master/originals/ORI-001/v1/master.mp4"); !read {
		t.Fatal("audio must be extracted from the working master")
	}
	// Permanent provenance (R21).
	v, err := s.store.Versions().Get(ctx, id.Ref(a.ID), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Provenance == nil || v.Provenance.JobID != done.ID || v.Provenance.Workflow != "ingest-original@1.0.0" || v.Provenance.WorkflowSHA256 != done.WorkflowSHA256 {
		t.Fatalf("provenance %+v", v.Provenance)
	}
	if v.Provenance.Tools["ffmpeg"] != "7.1" || v.Provenance.Tools["asr"] != "fake/canned@1" || len(v.Provenance.Profiles) != 2 || v.Provenance.NodeTypes["video.transcode"] != 1 {
		t.Fatalf("provenance %+v", v.Provenance)
	}
	// Events of §3.1 step 16.
	for _, typ := range []event.Type{event.AssetCreated, event.StorageBackupVerified, event.AssetIndexed, event.OriginalIndexed, event.AssetReadyForClipping, event.WorkflowCompleted} {
		if countEvents(t, s.store, typ) != 1 {
			t.Errorf("%s fired %d times", typ, countEvents(t, s.store, typ))
		}
	}
	if countEvents(t, s.store, event.ClipCreated) != 0 {
		t.Error("clip.created is for clips")
	}
	var outs map[string]any
	_ = json.Unmarshal(done.Outputs, &outs)
	if outs["asset_id"] != "ORI-001" || outs["status"] != "ready_for_clipping" {
		t.Fatalf("outputs %v", outs)
	}
	// A server: path is never deleted (R9).
	if _, err := os.Stat(s.file); err != nil {
		t.Fatalf("the input was removed: %v", err)
	}
}

func TestIngestFailureAndRetry(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	var fail atomic.Bool
	fail.Store(true)
	s.on("video.validate_timeline", func(context.Context, node.Env, node.Input) (node.Result, error) {
		if fail.Load() {
			return node.Result{}, node.Permanent(errs.New(errs.CodeValidationFailed, "duration off by 2 s"))
		}
		return node.JSONResult(map[string]any{"valid": true})
	})
	j := s.waitStatus(s.runAs(editor, "ingest-original", map[string]any{"file": s.file, "title": "T"}), domain.JobFailed)
	a, err := s.store.Assets().Get(ctx, id.Ref(j.AssetID))
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != domain.AssetIngestFailed || a.ClipEligible() {
		t.Fatalf("asset %+v (§2.5)", a)
	}
	fail.Store(false)
	if _, err := s.engine.Jobs().Retry(ctx, scopeOf(editor), id.Ref(j.ID), api.JobActionRequest{}); err != nil {
		t.Fatal(err)
	}
	s.waitStatus(j, domain.JobCompleted)
	a, _ = s.store.Assets().Get(ctx, id.Ref(j.AssetID))
	if a.Status != domain.AssetReadyForClipping {
		t.Fatalf("status %s", a.Status)
	}
	if len(s.callsOf("compress")) != 1 || len(s.callsOf("backup")) != 1 {
		t.Fatalf("checkpoints: compress %d backup %d", len(s.callsOf("compress")), len(s.callsOf("backup")))
	}
	// One ID, one asset, in spite of everything.
	if assets, _, _ := s.store.Assets().List(ctx, repo.AssetFilter{}, repo.Page{}); len(assets) != 1 {
		t.Fatalf("assets %+v", assets)
	}
}

func TestReadinessIsEnforcedByMarkReady(t *testing.T) {
	s := newStudio(t)
	// A storage.put that reports unverified: the flag stays false, so no readiness.
	real := s.behaviors["storage.put"]
	s.on("storage.put", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		res, err := real(ctx, env, in)
		if err != nil {
			return res, err
		}
		var out map[string]any
		_ = json.Unmarshal(res.Output, &out)
		if out["storage"] == "archive" {
			out["verified"] = false
		}
		return node.JSONResult(out)
	})
	j := s.waitStatus(s.runAs(editor, "ingest-original", map[string]any{"file": s.file, "title": "T"}), domain.JobFailed)
	if j.Error.NodeID != "ready" || !strings.Contains(j.Error.Message, "backup not verified") {
		t.Fatalf("error %+v", j.Error)
	}
}

func TestCreateClipThroughBothGates(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	source := s.ingest()
	apps := s.engine.Approvals()

	resp, err := s.engine.Workflows().CreateClip(ctx, scopeOf(editor), api.CreateClipRequest{
		SourceID: string(source.ID), Prompt: "The story of the 90-year-old woman", Destinations: []string{"mock-main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := s.job(resp.JobID)
	s.waitStatus(parent, domain.JobWaitingApproval)
	selection, found, err := s.store.Approvals().GetForNode(ctx, parent.ULID, "approve_selection")
	if err != nil || !found {
		t.Fatalf("selection: %v %v", found, err)
	}
	if selection.Kind != domain.ApprovalClipSelection || selection.Preview == "" || !selection.AllowAdjust {
		t.Fatalf("selection %+v", selection)
	}
	var evidence map[string]any
	if err := json.Unmarshal(selection.Evidence, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence["excerpt"] != "the story" || evidence["confidence"] != 0.8 || evidence["overlap"] == nil || evidence["summary"] == nil {
		t.Fatalf("evidence %v (D51, D47)", evidence)
	}
	// No render before the gate.
	if len(s.callsOf("render")) != 0 {
		t.Fatal("a render ran before the selection gate")
	}
	if _, err := apps.Approve(ctx, scopeOf(viewer), id.Ref(selection.ID), api.ApproveRequest{}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("viewer: %v", err)
	}
	start, end := 650.5, 1070.0
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(selection.ID), api.ApproveRequest{StartSeconds: &start, EndSeconds: &end}); err != nil {
		t.Fatal(err)
	}

	// The render reads the master, with the approved segments (R27, §3.2).
	var ingest domain.Job
	s.waitFor("the clip to be ingested", func() bool {
		ingest = childOf(t, s.harness, parent, "ingest_derived")
		return ingest.Status == domain.JobCompleted
	})
	if _, read := s.reads.Load("cut:storage://master/originals/ORI-001/v1/master.mp4"); !read {
		t.Fatal("the render must read the master role")
	}
	var cut struct {
		Segments []domain.Segment `json:"segments"`
	}
	_ = json.Unmarshal(s.callsOf("render")[0].Params, &cut)
	if len(cut.Segments) != 1 || cut.Segments[0].StartSeconds != 650.5 || cut.Segments[0].EndSeconds != 1070 {
		t.Fatalf("render segments %+v", cut.Segments)
	}

	// A new asset with lineage (R27, §12.3), in review, with its own keys.
	clip, err := s.store.Assets().Get(ctx, id.Ref("CLIP-001"))
	if err != nil {
		t.Fatal(err)
	}
	if clip.Lineage == nil || clip.Lineage.SourceAssetID != source.ID || clip.Lineage.RootAssetID != source.ID || clip.Lineage.Method != domain.LineageComputed {
		t.Fatalf("lineage %+v", clip.Lineage)
	}
	if clip.Lineage.SelectionApprovalID != selection.ID || clip.Lineage.Segments[0].StartSeconds != 650.5 || clip.Lineage.Segments[0].SourceVersion != "v1" {
		t.Fatalf("lineage %+v", clip.Lineage)
	}
	if clip.ParentAssetID != source.ID || clip.Backup == nil || clip.Backup.Key != "clips/CLIP-001/v1/master.mp4" || clip.WorkingMaster == nil || clip.WorkingMaster.Storage != domain.RoleMaster {
		t.Fatalf("clip %+v", clip)
	}
	if countEvents(t, s.store, event.ClipCreated) != 1 {
		t.Fatal("clip.created must fire")
	}

	// The publication gate: an editor gets 403, a publisher approves (D7).
	publish := childOf(t, s.harness, parent, "publish")
	s.waitStatus(publish, domain.JobWaitingApproval)
	s.waitStatus(parent, domain.JobWaitingApproval)
	gate, found, err := s.store.Approvals().GetForNode(ctx, publish.ULID, "approve_publication")
	if err != nil || !found {
		t.Fatalf("publication gate: %v %v", found, err)
	}
	if gate.Kind != domain.ApprovalPublication || gate.AssetID != clip.ID || gate.ID != "APR-002" {
		t.Fatalf("gate %+v", gate)
	}
	if s.uploads.Load() != 0 {
		t.Fatal("no upload before approval (D37)")
	}
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(gate.ID), api.ApproveRequest{}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("editor: %v", err)
	}
	if _, err := apps.Approve(ctx, scopeOf(publisher), id.Ref(gate.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	done := s.waitStatus(parent, domain.JobCompleted)

	// Published through publish-one, and the library follows through the event.
	if s.uploads.Load() != 1 {
		t.Fatalf("uploads %d", s.uploads.Load())
	}
	pubs, _, _ := s.store.Publications().List(ctx, repo.PublicationFilter{AssetID: id.Ref(clip.ID)}, repo.Page{})
	if len(pubs) != 1 || pubs[0].Status != domain.PubPublished || pubs[0].ApprovalID != gate.ID || pubs[0].Title != "A generated title" {
		t.Fatalf("publications %+v", pubs)
	}
	s.waitFor("the public library job", func() bool {
		jobs, _, _ := s.store.Jobs().List(ctx, repo.JobFilter{WorkflowID: "update-public-library"}, repo.Page{})
		return len(jobs) == 1 && jobs[0].Status == domain.JobCompleted && jobs[0].Trigger.Type == "event"
	})
	var outs map[string]any
	_ = json.Unmarshal(done.Outputs, &outs)
	if outs["decision"] != "approved" || outs["clip_id"] != "CLIP-001" || outs["publication_decision"] != "approved" {
		t.Fatalf("outputs %v", outs)
	}
	got, _ := s.store.Assets().Get(ctx, id.Ref(clip.ID))
	if got.Status != domain.AssetPublished {
		t.Fatalf("status %s", got.Status)
	}

	// D36: a clip of that clip renders from the root's master, on the root's timeline.
	resp, err = s.engine.Workflows().CreateClip(ctx, scopeOf(editor), api.CreateClipRequest{SourceID: "CLIP-001", Prompt: "a shorter part"})
	if err != nil {
		t.Fatal(err)
	}
	second := s.job(resp.JobID)
	s.waitStatus(second, domain.JobWaitingApproval)
	g2, _, _ := s.store.Approvals().GetForNode(ctx, second.ULID, "approve_selection")
	a, b := 5.0, 15.0
	if _, err := apps.Approve(ctx, scopeOf(editor), id.Ref(g2.ID), api.ApproveRequest{StartSeconds: &a, EndSeconds: &b}); err != nil {
		t.Fatal(err)
	}
	s.waitStatus(second, domain.JobCompleted)
	calls := s.callsOf("render")
	_ = json.Unmarshal(calls[len(calls)-1].Params, &cut)
	if cut.Segments[0].StartSeconds != 655.5 || cut.Segments[0].EndSeconds != 665.5 || cut.Segments[0].SourceAssetID != source.ID {
		t.Fatalf("the segment must be on the root's timeline: %+v", cut.Segments)
	}
	clip2, err := s.store.Assets().Get(ctx, id.Ref("CLIP-002"))
	if err != nil {
		t.Fatal(err)
	}
	if clip2.Lineage.RootAssetID != source.ID || clip2.Lineage.SourceAssetID != "CLIP-001" || len(clip2.Lineage.Chain) != 2 || clip2.Lineage.Segments[0].StartSeconds != 5 {
		t.Fatalf("lineage %+v (D36)", clip2.Lineage)
	}

	// Withdrawal: immediate, repeatable, and the library follows.
	unpub := func() domain.Job {
		return s.waitStatus(s.runAs(publisher, "unpublish-video", map[string]any{"assetId": "CLIP-001", "all": true}), domain.JobCompleted)
	}
	first := unpub()
	if !strings.Contains(string(first.Outputs), string(pubs[0].ID)) {
		t.Fatalf("outputs %s", first.Outputs)
	}
	unpub()
	if countEvents(t, s.store, event.PublicationWithdrawn) != 1 {
		t.Fatalf("withdrawn fired %d times (§15.8)", countEvents(t, s.store, event.PublicationWithdrawn))
	}
	s.waitFor("the library to drop it", func() bool {
		jobs, _, _ := s.store.Jobs().List(ctx, repo.JobFilter{WorkflowID: "public-library-on-withdrawn"}, repo.Page{})
		return len(jobs) == 1 && jobs[0].Status == domain.JobCompleted
	})
	// An editor cannot withdraw (route and workflow agree).
	if _, err := s.engine.Workflows().Run(ctx, scopeOf(editor), "unpublish-video", api.RunWorkflowRequest{Inputs: json.RawMessage(`{"assetId":"CLIP-001","all":true}`)}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("editor: %v", err)
	}
}

func TestRejectedSelectionMakesNoClip(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	source := s.ingest()
	resp, err := s.engine.Workflows().CreateClip(ctx, scopeOf(editor), api.CreateClipRequest{SourceID: string(source.ID), Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	j := s.job(resp.JobID)
	s.waitStatus(j, domain.JobWaitingApproval)
	gate, _, _ := s.store.Approvals().GetForNode(ctx, j.ULID, "approve_selection")
	if _, err := s.engine.Approvals().Reject(ctx, scopeOf(editor), id.Ref(gate.ID), api.RejectRequest{Reason: "incomplete excerpt"}); err != nil {
		t.Fatal(err)
	}
	done := s.waitStatus(j, domain.JobCompleted)
	if len(s.callsOf("render")) != 0 {
		t.Fatal("a rejected selection must not render")
	}
	if clips, _, _ := s.store.Assets().List(ctx, repo.AssetFilter{Kind: domain.KindClip}, repo.Page{}); len(clips) != 0 {
		t.Fatalf("clips %+v", clips)
	}
	if !strings.Contains(string(done.Outputs), `"decision":"rejected"`) {
		t.Fatalf("outputs %s", done.Outputs)
	}
}

func TestCreateClipRefusesAnIneligibleSource(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	source := s.ingest()
	// The master went missing (§2.5): no automatic restoration, and an explicit error.
	a, _ := s.store.Assets().Get(ctx, id.Ref(source.ID))
	a.MasterAvailable = false
	if err := s.store.Assets().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	resp, err := s.engine.Workflows().CreateClip(ctx, scopeOf(editor), api.CreateClipRequest{SourceID: string(source.ID), Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	j := s.waitStatus(s.job(resp.JobID), domain.JobFailed)
	if j.Error.NodeID != "source" || !strings.Contains(j.Error.Message, "no available master") {
		t.Fatalf("error %+v", j.Error)
	}
	if len(s.callsOf("fetch_raw")) != 0 {
		t.Fatal("nothing may reach for the archive")
	}
}

func TestPublicationRejectionAndRequireApprovalFalse(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	source := s.ingest()
	// Publishing an original exists too: from ready_for_clipping through review.
	j := s.runAs(publisher, "publish-video", map[string]any{"assetId": source.ID, "destinations": []string{"mock-main"}})
	s.waitStatus(j, domain.JobWaitingApproval)
	gate, _, _ := s.store.Approvals().GetForNode(ctx, j.ULID, "approve_publication")
	if _, err := s.engine.Approvals().Reject(ctx, scopeOf(publisher), id.Ref(gate.ID), api.RejectRequest{Reason: "not this one"}); err != nil {
		t.Fatal(err)
	}
	s.waitStatus(j, domain.JobCompleted)
	a, _ := s.store.Assets().Get(ctx, id.Ref(source.ID))
	if a.Status != domain.AssetRejected || s.uploads.Load() != 0 {
		t.Fatalf("status %s uploads %d", a.Status, s.uploads.Load())
	}

	// requireApproval false lifts nothing (I143).
	j = s.waitStatus(s.runAs(editor, "publish-video", map[string]any{"assetId": source.ID, "destinations": []string{"mock-main"}, "requireApproval": false}), domain.JobCompleted)
	if s.uploads.Load() != 0 {
		t.Fatal("no gate, no publication")
	}
	if n := s.nodes(j)["approve_publication"]; n.Status != domain.NodeSkipped {
		t.Fatalf("gate %s", n.Status)
	}
}

func TestDeleteAssetWorkflow(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	source := s.ingest()
	if _, err := s.engine.Workflows().Run(ctx, scopeOf(editor), "delete-asset", api.RunWorkflowRequest{Inputs: json.RawMessage(`{"asset_id":"ORI-001"}`)}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("an editor cannot delete: %v", err)
	}
	// Active publications block it (R16).
	if err := s.store.Publications().Create(ctx, domain.Publication{ID: "PUB-001", ULID: id.New(), AssetID: source.ID, Destination: "d", AssetVersion: "v1", Status: domain.PubPublished, RemoteID: "r", URL: "https://media.example.internal/r"}); err != nil {
		t.Fatal(err)
	}
	j := s.waitStatus(s.runAs(admin, "delete-asset", map[string]any{"asset_id": "ORI-001"}), domain.JobFailed)
	if !strings.Contains(j.Error.Message, "withdraw it first") {
		t.Fatalf("error %+v", j.Error)
	}
	pub, _ := s.store.Publications().Get(ctx, id.Ref("PUB-001"))
	pub.Status = domain.PubWithdrawn
	if err := s.store.Publications().Update(ctx, pub); err != nil {
		t.Fatal(err)
	}
	master := s.storage.roles[domain.RoleMaster]
	if ok, _ := master.Exists(ctx, source.WorkingMaster.Key); !ok {
		t.Fatal("the master should exist")
	}
	done := s.waitStatus(s.runAs(admin, "delete-asset", map[string]any{"asset_id": "ORI-001"}), domain.JobCompleted)
	var outs map[string]any
	_ = json.Unmarshal(done.Outputs, &outs)
	if outs["asset_id"] != "ORI-001" || outs["archive_purge_after"] == nil {
		t.Fatalf("outputs %v", outs)
	}
	if ok, _ := master.Exists(ctx, source.WorkingMaster.Key); ok {
		t.Fatal("the operational copy must go")
	}
	archive := s.storage.roles[domain.RoleArchive]
	if ok, _ := archive.Exists(ctx, source.SourceOriginal.Key); !ok {
		t.Fatal("the archive copy stays until its minimum duration (D17)")
	}
	a, err := s.store.Assets().Get(ctx, id.Ref(source.ID))
	if err != nil || a.DeletedAt == nil {
		t.Fatalf("asset %+v %v", a, err)
	}
	// Deleted means gone for clipping, and the audit keeps the fact, without content.
	audit, _, _ := s.store.Audit().List(ctx, repo.AuditFilter{Action: "asset.deleted"}, repo.Page{})
	if len(audit) != 1 || strings.Contains(string(audit[0].Details), "Hatha") {
		t.Fatalf("audit %+v", audit)
	}
	if countEvents(t, s.store, event.AssetDeleted) != 1 {
		t.Fatal("asset.deleted must fire")
	}
	gone := s.waitStatus(s.runAs(editor, "reindex-asset", map[string]any{"assetId": "ORI-001"}), domain.JobFailed)
	if gone.Error.Code != string(errs.CodeNotFound) {
		t.Fatalf("a deleted asset is gone: %+v", gone.Error)
	}
	// The purge sweep removes the archive copy, once its duration is over.
	purge := s.waitStatus(s.runAs(admin, "purge-deleted-archives", nil), domain.JobCompleted)
	if n := s.nodes(purge)["each"]; n.Status != domain.NodeCompleted {
		t.Fatalf("each %+v", n)
	}
}

func TestMaintenanceWorkflows(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	source := s.ingest()

	// reindex-asset reads the stored transcript.
	s.waitStatus(s.runAs(editor, "reindex-asset", map[string]any{"assetId": source.ID}), domain.JobCompleted)

	// verify-storage flips master_available when a master is bad, with no restoration.
	s.waitStatus(s.runAs(editor, "verify-storage", nil), domain.JobCompleted)
	a, _ := s.store.Assets().Get(ctx, id.Ref(source.ID))
	if !a.MasterAvailable {
		t.Fatal("a good master stays available")
	}
	// An outage says nothing about the master.
	s.on("storage.verify", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, node.Permanent(errs.New(errs.CodeUnavailable, "storage is down"))
	})
	s.waitStatus(s.runAs(editor, "verify-storage", nil), domain.JobCompleted)
	if a, _ = s.store.Assets().Get(ctx, id.Ref(source.ID)); !a.MasterAvailable {
		t.Fatal("an unreachable storage must not mark the master missing")
	}
	s.on("storage.verify", func(context.Context, node.Env, node.Input) (node.Result, error) {
		return node.Result{}, node.Permanent(errs.NotFound("object"))
	})
	s.waitStatus(s.runAs(editor, "verify-storage", nil), domain.JobCompleted)
	a, _ = s.store.Assets().Get(ctx, id.Ref(source.ID))
	if a.MasterAvailable || a.ClipEligible() {
		t.Fatalf("a missing master must leave create-clip: %+v", a)
	}

	// restore-archived needs its permission, and restores an original by transcoding.
	if _, err := s.engine.Workflows().Run(ctx, scopeOf(publisher), "restore-archived", api.RunWorkflowRequest{Inputs: json.RawMessage(`{"asset_id":"ORI-001"}`)}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("publisher: %v", err)
	}
	j := s.waitStatus(s.runAs(admin, "restore-archived", map[string]any{"asset_id": "ORI-001"}), domain.JobCompleted)
	nodes := s.nodes(j)
	if nodes["fetch_raw"].Status != domain.NodeCompleted || nodes["copy_backup"].Status != domain.NodeSkipped {
		t.Fatalf("nodes %+v", nodes)
	}
	if !strings.Contains(string(j.Snapshot), "allowArchive") || len(j.Permissions) == 0 {
		t.Fatalf("job %+v", j.Permissions)
	}
	a, _ = s.store.Assets().Get(ctx, id.Ref(source.ID))
	if !a.MasterAvailable {
		t.Fatal("restored")
	}

	// bulk-create-clips starts one create-clip per prompt, each with its own gate.
	bulk := s.runAs(editor, "bulk-create-clips", map[string]any{"source_id": source.ID, "prompts": []string{"one", "two"}})
	var pending []domain.Approval
	s.waitFor("two gates", func() bool {
		list, _ := s.engine.Approvals().List(ctx, scopeOf(viewer), api.ApprovalQuery{Status: domain.ApprovalPending})
		pending = list.Items
		return len(pending) == 2
	})
	for _, gate := range pending {
		if _, err := s.engine.Approvals().Reject(ctx, scopeOf(editor), id.Ref(gate.ID), api.RejectRequest{Reason: "not now"}); err != nil {
			t.Fatal(err)
		}
	}
	s.waitStatus(bulk, domain.JobCompleted)

	// refresh-publication passes through.
	s.waitStatus(s.runAs(editor, "refresh-publication", map[string]any{"assetId": source.ID}), domain.JobCompleted)
}

func TestExpireUnapprovedRenders(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	s.deps.Daemon.Retention.UnapprovedRenderDays = 30
	h := s.harness
	master := domain.MediaFile{Storage: domain.RoleMaster, Key: "clips/CLIP-001/v1/master.mp4"}
	old := h.newAsset(domain.KindClip)
	if err := h.store.Versions().PutFile(ctx, id.Ref(old.ID), "v1", domain.VersionFile{Purpose: domain.FileWorkingMaster, File: master}); err != nil {
		t.Fatal(err)
	}
	a, _ := h.store.Assets().Get(ctx, id.Ref(old.ID))
	a.MasterAvailable = true
	a.Status = domain.AssetIngesting
	_ = h.store.Assets().Update(ctx, a)
	a.Status = domain.AssetReadyForClipping
	_ = h.store.Assets().Update(ctx, a)
	a.Status = domain.AssetEditorialReview
	if err := h.store.Assets().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Not old enough: nothing happens. (Age is covered by nodes/catalog's tests.)
	j := s.waitStatus(s.runAs(admin, "expire-unapproved-renders", nil), domain.JobCompleted)
	if out := outputMap(t, s.nodes(j)["list"]); len(out["items"].([]any)) != 0 {
		t.Fatalf("items %v", out)
	}
	// The per-render workflow itself.
	done := s.waitStatus(s.runAs(admin, "expire-render", map[string]any{"render": map[string]any{"asset_id": old.ID, "master_uri": master.URI()}}), domain.JobCompleted)
	if s.nodes(done)["status"].Status != domain.NodeCompleted {
		t.Fatalf("nodes %+v", s.nodes(done))
	}
	got, _ := h.store.Assets().Get(ctx, id.Ref(old.ID))
	if got.Status != domain.AssetArchivedOnly || got.MasterAvailable {
		t.Fatalf("asset %+v", got)
	}
}

func TestPublishPrivateFirst(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	source := s.ingest()
	// This fake's publish.prepare wants an approval; the real one permits a private
	// upload when the destination allows it (D37).
	real := s.behaviors["publish.prepare"]
	s.on("publish.prepare", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		var p map[string]any
		_ = json.Unmarshal(in.Parameters, &p)
		if p["approvalId"] == nil {
			return node.JSONResult(map[string]any{"publication_id": "PUB-777"})
		}
		return real(ctx, env, in)
	})
	if _, err := s.engine.Workflows().Run(ctx, scopeOf(editor), "publish-private-first", api.RunWorkflowRequest{Inputs: json.RawMessage(`{"assetId":"ORI-001","destinations":["d"]}`)}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("an upload before the gate needs publication.create: %v", err)
	}
	j := s.runAs(publisher, "publish-private-first", map[string]any{"assetId": source.ID, "destinations": []string{"d"}})
	publish := childOf(t, s.harness, j, "publish")
	s.waitStatus(publish, domain.JobWaitingApproval)
	if s.uploads.Load() != 1 {
		t.Fatalf("the private upload comes before the gate: %d", s.uploads.Load())
	}
	gate, _, _ := s.store.Approvals().GetForNode(ctx, publish.ULID, "approve_publication")
	if _, err := s.engine.Approvals().Approve(ctx, scopeOf(publisher), id.Ref(gate.ID), api.ApproveRequest{}); err != nil {
		t.Fatal(err)
	}
	s.waitStatus(j, domain.JobCompleted)
}
