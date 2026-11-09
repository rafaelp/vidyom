package publishingnodes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/connectors/publishing/mock"
	"github.com/rafaelp/vidyom/connectors/publishing/vimeo"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/persistence/sqlitedriver"
	"github.com/rafaelp/vidyom/internal/publicexport"
	"github.com/rafaelp/vidyom/schemas"
)

func permanent(err error) bool {
	return err != nil && !node.IsTransient(err) && !node.IsUnknownOutcome(err)
}

func TestEveryOwnedCatalogNodeIsRegisteredAsDeclared(t *testing.T) {
	reg := node.NewRegistry()
	Register(reg)
	cat, err := schemas.NodeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var owned []string
	for _, e := range cat.Nodes {
		if e.Owner != "publishing" {
			continue
		}
		owned = append(owned, e.Type)
		n, ok := reg.Lookup(e.Type, e.TypeVersion)
		if !ok {
			t.Errorf("%s is in the catalog but not registered", e.Type)
			continue
		}
		if err := schemas.CheckDescriptor(n.Descriptor()); err != nil {
			t.Error(err)
		}
		if n.Descriptor().Pool != node.PoolDaemon {
			t.Errorf("%s must run in the daemon pool (I7)", e.Type)
		}
		if n.Descriptor().SideEffect == node.EffectReconcilable {
			if _, ok := n.(node.Reconciler); !ok {
				t.Errorf("%s is reconcilable and must implement node.Reconciler", e.Type)
			}
		}
	}
	slices.Sort(owned)
	want := slices.Clone(Types)
	slices.Sort(want)
	if !slices.Equal(owned, want) {
		t.Fatalf("catalog owns %v, package registers %v", owned, want)
	}
	if len(reg.List()) != len(Types) {
		t.Fatalf("registered %d, want %d", len(reg.List()), len(Types))
	}
}

func TestNodesRefuseAWorkerEnvironment(t *testing.T) {
	h := newHarness(t)
	for _, nodeType := range Types {
		_, err := h.node(nodeType).Execute(h.ctx(), workerOnly{}, node.Input{Parameters: json.RawMessage(`{}`)})
		if !permanent(err) || !strings.Contains(err.Error(), "daemon pool") {
			t.Errorf("%s: %v", nodeType, err)
		}
	}
}

// workerOnly has none of what a daemon node needs. Execute must refuse before using it.
type workerOnly struct{ node.Env }

func TestFullLifecycle(t *testing.T) {
	h := newHarness(t)

	prep := h.prepared()
	if prep.AlreadyPublished || prep.AssetVersion != "v1" || prep.SHA256 != sha(masterBytes) || prep.SourceURI != "storage://master/"+masterKey {
		t.Fatalf("prepare output %+v", prep)
	}
	pub := prep.PublicationID
	record := h.publication(pub)
	if record.Status != domain.PubPendingApproval || record.ApprovalID != "APR-001" || record.Title != "Conscious breathing" {
		t.Fatalf("record %+v", record)
	}
	if record.PublishedFile != nil {
		t.Fatal("identical to the master: no separate file (R4)")
	}

	res, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage) != 1 || res.Usage[0].Operation != "publish.upload" || res.Usage[0].Provider != mock.Driver {
		t.Fatalf("usage %+v", res.Usage)
	}
	record = h.publication(pub)
	if record.Status != domain.PubProcessing || record.RemoteID != mock.RemoteID(record.IdempotencyKey) {
		t.Fatalf("record %+v", record)
	}
	video := h.mockState().Videos[record.RemoteID]
	if video.Visibility != domain.VisibilityPrivate {
		t.Fatalf("the upload must be private until finalize (I224): %+v", video)
	}
	if video.SHA256 != sha(masterBytes) || video.Metadata.Title != "Conscious breathing" || video.Metadata.Tags[0] != "breathing" {
		t.Fatalf("video %+v", video)
	}

	await := run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": pub})
	if !await.Ready || await.Status != "processing" {
		t.Fatalf("await %+v", await)
	}

	final := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
	if final.Status != "published" || final.Visibility != "public" || !strings.HasPrefix(final.URL, mock.BaseURL+"/watch/") {
		t.Fatalf("finalize %+v", final)
	}
	record = h.publication(pub)
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	if !record.IsPublic() || record.PublishedAt == nil {
		t.Fatalf("record %+v", record)
	}
	if h.mockState().Videos[record.RemoteID].Visibility != domain.VisibilityPublic {
		t.Fatal("the destination must show public")
	}
	asset := h.asset("CLIP-001")
	if asset.Status != domain.AssetPublished || asset.Visibility != domain.VisibilityPublic {
		t.Fatalf("asset %s %s", asset.Status, asset.Visibility)
	}
	version, err := h.store.Versions().Get(h.ctx(), "CLIP-001", "v1")
	if err != nil || !version.Immutable {
		t.Fatalf("version must be immutable after publication (D35): %+v, %v", version, err)
	}
	if records := h.store.AuditRecords(); len(records) != 1 || records[0].Resource != pub || records[0].Permission != domain.PermPublicationCreate || records[0].Actor != "job:JOB-001" {
		t.Fatalf("visibility change must be audited: %+v", records)
	}

	events := h.store.Events()
	if hasEvent(events, event.PublicationCreated) != 1 || hasEvent(events, event.PublicationCompleted) != 1 {
		t.Fatalf("events %+v", events)
	}
	var payload map[string]any
	for _, e := range events {
		if e.Type == event.PublicationCompleted {
			h.must(json.Unmarshal(e.Payload, &payload))
			if e.AssetID != "CLIP-001" {
				t.Fatalf("event %+v", e)
			}
		}
	}
	if payload["visibility"] != "public" || payload["publication_id"] != pub {
		t.Fatalf("payload %v", payload)
	}

	synced := run[syncOutput](h, "catalog.public.sync", map[string]any{"assetId": "CLIP-001"})
	if synced.Records != 1 || !synced.Shipped || synced.Destination != "local-file" || synced.SHA256 == "" {
		t.Fatalf("sync %+v", synced)
	}
	if hasEvent(h.store.Events(), event.PublicLibrarySynced) != 1 {
		t.Fatal("public_library.synced must fire")
	}
	if got := videosIn(t, h.publicDB()); got != 1 {
		t.Fatalf("videos %d", got)
	}

	withdrawn := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
	if len(withdrawn.Withdrawn) != 1 || withdrawn.Withdrawn[0] != pub || len(withdrawn.Already) != 0 {
		t.Fatalf("withdraw %+v", withdrawn)
	}
	record = h.publication(pub)
	if record.Status != domain.PubWithdrawn || record.WithdrawnAt == nil || record.IsPublic() {
		t.Fatalf("record %+v", record)
	}
	if h.mockState().Videos[record.RemoteID].Visibility != domain.VisibilityPrivate {
		t.Fatal("the default mode makes the video private at the destination")
	}
	if a := h.asset("CLIP-001"); a.Status != domain.AssetWithdrawn || a.Visibility != domain.VisibilityPrivate {
		t.Fatalf("asset %s %s", a.Status, a.Visibility)
	}
	if hasEvent(h.store.Events(), event.PublicationWithdrawn) != 1 {
		t.Fatal("publication.withdrawn must fire")
	}
	if records := h.store.AuditRecords(); records[len(records)-1].Permission != domain.PermPublicationWithdraw {
		t.Fatalf("the withdrawal is audited under its own permission: %+v", records)
	}

	run[syncOutput](h, "catalog.public.sync", map[string]any{})
	if got := videosIn(t, h.publicDB()); got != 0 {
		t.Fatalf("videos after withdrawal: %d", got)
	}

	// §15.8: repeating produces no error and no duplicate effect.
	before := len(h.store.Events())
	again := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
	if len(again.Withdrawn) != 0 || len(again.Already) != 1 || again.Already[0] != pub {
		t.Fatalf("repeat %+v", again)
	}
	if len(h.store.Events()) != before {
		t.Fatal("a repeated withdrawal must emit nothing")
	}
}

func videosIn(t *testing.T, path string) int {
	t.Helper()
	db, err := sqlitedriver.Open(path, sqlitedriver.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return countRows(t, db, `SELECT count(*) FROM videos`)
}

func countRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEveryNodeIsIdempotentOnRepeat(t *testing.T) {
	h := newHarness(t)
	pub := h.published()
	state := h.mockState()
	events := len(h.store.Events())
	audits := len(h.store.AuditRecords())

	if got := h.prepared(); got.PublicationID != pub || !got.AlreadyPublished {
		t.Fatalf("prepare %+v", got)
	}
	res, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Usage) != 0 {
		t.Fatal("no usage for an upload that did not happen (I239)")
	}
	run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": pub})
	run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})

	if s := h.mockState(); s.Uploads != state.Uploads || s.Uploads != 1 {
		t.Fatalf("uploads %d: a repeat must not upload (§15.5)", s.Uploads)
	}
	if len(h.store.Events()) != events {
		t.Fatalf("a repeat emitted events: %+v", h.store.Events()[events:])
	}
	if len(h.store.AuditRecords()) != audits {
		t.Fatal("a repeat wrote audit records")
	}
}

// R43: while there is no URL, the state is processing.
func TestNoPublishedWithoutAURL(t *testing.T) {
	t.Run("still processing at the destination", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"processingSeconds": 3600})
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})

		_, err := h.runErr("publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
		if !node.IsTransient(err) {
			t.Fatalf("got %v", err)
		}
		record := h.publication(pub)
		if record.Status != domain.PubProcessing || record.URL != "" || record.PublishedAt != nil {
			t.Fatalf("record %+v", record)
		}
		if hasEvent(h.store.Events(), event.PublicationCompleted) != 0 {
			t.Fatal("completed must not fire")
		}
		if v, _ := h.store.Versions().Get(h.ctx(), "CLIP-001", "v1"); v.Immutable {
			t.Fatal("a version stays replaceable until a confirmed publication (D35)")
		}
		if a := h.asset("CLIP-001"); a.Status == domain.AssetPublished {
			t.Fatal("the asset must not read published")
		}
	})

	t.Run("destination says ready but returns no URL", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("odd", func(s *scripted) {
			s.getStatus = func(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
				return connector.RemoteVideo{RemoteID: remoteID, Status: connector.RemoteReady, Visibility: domain.VisibilityPublic}, nil
			}
			s.setVis = func(_ context.Context, remoteID string, v domain.Visibility) (connector.RemoteVideo, error) {
				return connector.RemoteVideo{RemoteID: remoteID, Status: connector.RemoteReady, Visibility: v}, nil
			}
		})
		prep := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "odd", "visibility": "public", "approvalId": "APR-001"})
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": prep.PublicationID})
		_, err := h.runErr("publish.finalize", map[string]any{"publicationId": prep.PublicationID, "visibility": "public"})
		if !node.IsTransient(err) || !strings.Contains(err.Error(), "URL") {
			t.Fatalf("got %v", err)
		}
		if got := h.publication(prep.PublicationID).Status; got != domain.PubProcessing {
			t.Fatalf("status %s", got)
		}
	})

	t.Run("destination does not apply the visibility", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("stubborn", func(s *scripted) {
			s.setVis = func(ctx context.Context, remoteID string, _ domain.Visibility) (connector.RemoteVideo, error) {
				return s.Publishing.GetStatus(ctx, remoteID)
			}
		})
		prep := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "stubborn", "visibility": "public", "approvalId": "APR-001"})
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": prep.PublicationID})
		if _, err := h.runErr("publish.finalize", map[string]any{"publicationId": prep.PublicationID, "visibility": "public"}); !node.IsTransient(err) {
			t.Fatalf("got %v", err)
		}
		if h.publication(prep.PublicationID).IsPublic() {
			t.Fatal("a record must not claim public against the destination's answer")
		}
	})
}

func TestAwaitProcessing(t *testing.T) {
	t.Run("polls until the destination finishes", func(t *testing.T) {
		h := newHarness(t)
		polls := 0
		h.addScripted("slow", func(s *scripted) {
			s.getStatus = func(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
				polls++
				if polls < 3 {
					return connector.RemoteVideo{RemoteID: remoteID, Status: connector.RemoteProcessing}, nil
				}
				return s.Publishing.GetStatus(ctx, remoteID)
			}
		})
		prep := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "slow", "visibility": "public", "approvalId": "APR-001"})
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": prep.PublicationID})
		out := run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": prep.PublicationID})
		if polls != 3 || !out.Ready || out.URL == "" {
			t.Fatalf("polls %d, output %+v", polls, out)
		}
	})

	t.Run("times out as a transient error and keeps the record", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"processingSeconds": 3600})
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		_, err := h.runErr("publish.await_processing", map[string]any{"publicationId": pub, "timeoutSeconds": 1})
		if !node.IsTransient(err) || !errs.IsCode(err, errs.CodeTimeout) {
			t.Fatalf("got %v", err)
		}
		if got := h.publication(pub).Status; got != domain.PubProcessing {
			t.Fatalf("status %s", got)
		}
	})

	t.Run("cancellation stops the wait", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"processingSeconds": 3600})
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		ctx, cancel := context.WithCancel(h.ctx())
		cancel()
		_, err := h.runErr("publish.await_processing", map[string]any{"publicationId": pub}, runOpts{ctx: ctx})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a fresh upload not yet listed is waited for", func(t *testing.T) {
		h := newHarness(t)
		polls := 0
		h.addScripted("lagging", func(s *scripted) {
			s.getStatus = func(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
				polls++
				if polls < 3 {
					return connector.RemoteVideo{}, errs.NotFound("video")
				}
				return s.Publishing.GetStatus(ctx, remoteID)
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "lagging", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": pub})
		if !out.Ready || polls != 3 {
			t.Fatalf("polls %d, await %+v", polls, out)
		}
	})

	t.Run("a destination error stops the wait", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("down", func(s *scripted) {
			s.getStatus = func(context.Context, string) (connector.RemoteVideo, error) {
				return connector.RemoteVideo{}, errs.New(errs.CodeUnavailable, "down")
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "down", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		if _, err := h.runErr("publish.await_processing", map[string]any{"publicationId": pub}); !node.IsTransient(err) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("nothing to wait for", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		if _, err := h.runErr("publish.await_processing", map[string]any{"publicationId": pub}); !permanent(err) {
			t.Fatalf("got %v", err)
		}
	})
}

// D52: a refusal by the platform is its own state, not a failure.
func TestRejectionIsDistinctFromFailure(t *testing.T) {
	h := newHarness(t)
	h.setMockParams(map[string]any{"inject": map[string]int{mock.InjectReject: 1}})
	pub := h.prepared().PublicationID
	up := run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	if up.Status != string(domain.PubRejectedByDestination) {
		t.Fatalf("upload %+v", up)
	}
	record := h.publication(pub)
	if record.Status != domain.PubRejectedByDestination || record.StatusReason == "" || record.RemoteID == "" {
		t.Fatalf("record %+v", record)
	}
	await := run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": pub})
	if await.Status != string(domain.PubRejectedByDestination) || await.Ready {
		t.Fatalf("await %+v", await)
	}
	_, err := h.runErr("publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
	if !permanent(err) || !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
	if h.publication(pub).Status != domain.PubRejectedByDestination {
		t.Fatal("state changed")
	}
	if hasEvent(h.store.Events(), event.PublicationCompleted) != 0 {
		t.Fatal("completed must not fire")
	}
	if hasEvent(h.store.Events(), event.PublicationUpdated) != 1 {
		t.Fatal("updated must fire once")
	}

	// A new attempt with a new job is allowed and does not touch the old record.
	second := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "visibility": "public", "approvalId": "APR-001"}, runOpts{key: "another-job"})
	if second.PublicationID == pub {
		t.Fatal("a rejected record is history")
	}
}

func TestDestinationThatRejectsAfterProcessingOrFails(t *testing.T) {
	cases := map[string]struct {
		remote connector.RemoteStatus
		want   domain.PublicationStatus
	}{
		"rejected": {connector.RemoteRejected, domain.PubRejectedByDestination},
		"failed":   {connector.RemoteFailed, domain.PubFailed},
		"removed":  {connector.RemoteRemoved, domain.PubFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.addScripted("odd", func(s *scripted) {
				s.getStatus = func(_ context.Context, remoteID string) (connector.RemoteVideo, error) {
					return connector.RemoteVideo{RemoteID: remoteID, Status: tc.remote, Reason: "policy"}, nil
				}
			})
			prep := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "odd", "visibility": "public", "approvalId": "APR-001"})
			run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": prep.PublicationID})
			out := run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": prep.PublicationID})
			if out.Status != string(tc.want) || out.Ready {
				t.Fatalf("await %+v", out)
			}
			if _, err := h.runErr("publish.finalize", map[string]any{"publicationId": prep.PublicationID}); !permanent(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestFinalizeFindsARefusalItself(t *testing.T) {
	h := newHarness(t)
	h.addScripted("odd", func(s *scripted) {
		s.getStatus = func(_ context.Context, remoteID string) (connector.RemoteVideo, error) {
			return connector.RemoteVideo{RemoteID: remoteID, Status: connector.RemoteRejected, Reason: "copyright"}, nil
		}
	})
	prep := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "odd", "visibility": "public", "approvalId": "APR-001"})
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": prep.PublicationID})
	_, err := h.runErr("publish.finalize", map[string]any{"publicationId": prep.PublicationID})
	if !permanent(err) || !strings.Contains(err.Error(), "copyright") {
		t.Fatalf("got %v", err)
	}
	if got := h.publication(prep.PublicationID).Status; got != domain.PubRejectedByDestination {
		t.Fatalf("status %s", got)
	}
}

// R4: the master is reused; a separate file is used only when it differs.
func TestPublishedFileDeduplication(t *testing.T) {
	addPublished := func(h *harness, key, content, name string, role domain.StorageRole) {
		h.storages[role].objects[key] = content
		h.must(h.store.Versions().PutFile(h.ctx(), "CLIP-001", "v1", domain.VersionFile{
			Purpose: domain.FilePublishedFile, Name: name,
			File: domain.MediaFile{Storage: role, Key: key, SHA256: sha(content), SizeBytes: int64(len(content)), Verified: true},
		}))
	}

	t.Run("identical file is not a second file", func(t *testing.T) {
		h := newHarness(t)
		addPublished(h, "clips/CLIP-001/v1/published-mock-main.mp4", masterBytes, mockDest, domain.RoleMaster)
		prep := h.prepared()
		if prep.SourceURI != "storage://master/"+masterKey {
			t.Fatalf("source %s", prep.SourceURI)
		}
		if h.publication(prep.PublicationID).PublishedFile != nil {
			t.Fatal("must reference the master")
		}
	})

	t.Run("different file is used and recorded", func(t *testing.T) {
		h := newHarness(t)
		const key = "clips/CLIP-001/v1/published-mock-main.mp4"
		const content = "a file reencoded for this destination"
		addPublished(h, key, content, mockDest, domain.RoleMaster)
		prep := h.prepared()
		if prep.SourceURI != "storage://master/"+key || prep.SHA256 != sha(content) {
			t.Fatalf("prepare %+v", prep)
		}
		record := h.publication(prep.PublicationID)
		if record.PublishedFile == nil || record.PublishedFile.Key != key || record.PublishedFileSHA256 != sha(content) {
			t.Fatalf("record %+v", record)
		}
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": prep.PublicationID})
		if got := h.mockState().Videos[h.publication(prep.PublicationID).RemoteID].SHA256; got != sha(content) {
			t.Fatalf("destination received %s", got)
		}
	})

	t.Run("file named for another destination is ignored", func(t *testing.T) {
		h := newHarness(t)
		addPublished(h, "clips/CLIP-001/v1/published-other.mp4", "other", "other-destination", domain.RoleMaster)
		if prep := h.prepared(); prep.SHA256 != sha(masterBytes) {
			t.Fatalf("prepare %+v", prep)
		}
	})

	t.Run("published file outside the master role is refused", func(t *testing.T) {
		h := newHarness(t)
		addPublished(h, "clips/CLIP-001/v1/published-mock-main.mp4", "elsewhere", mockDest, domain.RoleProxy)
		_, err := h.runErr("publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"})
		if !permanent(err) || !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("second destination reuses the master with no copy", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("second", func(*scripted) {})
		one := h.prepared()
		two := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "second", "approvalId": "APR-001"}, runOpts{key: "second-key"})
		if one.SourceURI != two.SourceURI || one.PublicationID == two.PublicationID {
			t.Fatalf("%+v %+v", one, two)
		}
	})
}

func TestUploadNeverReadsArchive(t *testing.T) {
	h := newHarness(t)
	h.published()
	if len(h.reads[domain.RoleArchive]) != 0 || slices.Contains(h.opened, domain.RoleArchive) {
		t.Fatalf("archive touched: reads %v, opened %v", h.reads[domain.RoleArchive], h.opened)
	}
	if len(h.reads[domain.RoleMaster]) == 0 {
		t.Fatal("the master must be read")
	}
}

func TestMasterOnArchiveIsRefused(t *testing.T) {
	h := newHarness(t)
	h.must(h.store.Versions().PutFile(h.ctx(), "CLIP-001", "v1", domain.VersionFile{
		Purpose: domain.FileWorkingMaster,
		File:    domain.MediaFile{Storage: domain.RoleArchive, Key: masterKey, SHA256: sha(masterBytes)},
	}))
	_, err := h.runErr("publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"})
	if !permanent(err) || !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Fatalf("got %v", err)
	}
}

func TestPrepareRefusals(t *testing.T) {
	cases := map[string]struct {
		setup  func(*harness)
		params map[string]any
		code   errs.Code
	}{
		"unknown destination": {
			params: map[string]any{"assetId": "CLIP-001", "destination": "nowhere", "approvalId": "APR-001"},
			code:   errs.CodeInvalidArgument,
		},
		"unknown asset": {
			params: map[string]any{"assetId": "CLIP-999", "destination": mockDest, "approvalId": "APR-001"},
			code:   errs.CodeNotFound,
		},
		"approval of another version": {
			setup: func(h *harness) {
				h.must(h.store.Versions().Create(h.ctx(), domain.AssetVersion{AssetID: "CLIP-001", Version: "v2", Files: []domain.VersionFile{{
					Purpose: domain.FileWorkingMaster, File: domain.MediaFile{Storage: domain.RoleMaster, Key: "clips/CLIP-001/v2/master.mp4", SHA256: sha("v2")},
				}}}))
			},
			params: map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001", "version": "v2"},
			code:   errs.CodeFailedPrecondition,
		},
		"unknown version": {
			params: map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001", "version": "v9"},
			code:   errs.CodeNotFound,
		},
		"deleted asset": {
			setup:  func(h *harness) { h.must(h.store.Assets().SoftDelete(h.ctx(), "CLIP-001", time.Now())) },
			params: map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"},
			code:   errs.CodeNotFound,
		},
		"rejected asset": {
			setup: func(h *harness) {
				a := h.asset("CLIP-001")
				a.Status = domain.AssetRejected
				h.must(h.store.Assets().Update(h.ctx(), a))
			},
			params: map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"},
			code:   errs.CodeFailedPrecondition,
		},
		"master not available": {
			setup: func(h *harness) {
				a := h.asset("CLIP-001")
				a.MasterAvailable = false
				h.must(h.store.Assets().Update(h.ctx(), a))
			},
			params: map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"},
			code:   errs.CodeFailedPrecondition,
		},
		"skeleton destination": {
			setup: func(h *harness) {
				h.cfg.Publishing.Destinations["vimeo-main"] = config.Destination{Driver: vimeo.Driver}
			},
			params: map[string]any{"assetId": "CLIP-001", "destination": "vimeo-main", "approvalId": "APR-001"},
			code:   errs.CodeNotImplemented,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			_, err := h.runErr("publish.prepare", tc.params)
			if !permanent(err) || !errs.IsCode(err, tc.code) {
				t.Fatalf("got %v, want a permanent %s", err, tc.code)
			}
			if hasEvent(h.store.Events(), event.PublicationCreated) != 0 {
				t.Fatal("no record may be announced")
			}
		})
	}
}

func TestRepeatedPrepareAndAdoption(t *testing.T) {
	h := newHarness(t)
	first := h.prepared()
	if got := h.prepared(); got != first {
		t.Fatalf("same key: %+v vs %+v", got, first)
	}
	// Another job asking for the same thing must not start a second upload (I222).
	other := run[prepareOutput](h, "publish.prepare", map[string]any{
		"assetId": "CLIP-001", "destination": mockDest, "visibility": "public", "approvalId": "APR-001",
	}, runOpts{key: "another-job"})
	if other.PublicationID != first.PublicationID {
		t.Fatalf("adoption: %+v", other)
	}
	if hasEvent(h.store.Events(), event.PublicationCreated) != 1 {
		t.Fatal("one record, one event")
	}
	if key := h.publication(first.PublicationID).IdempotencyKey; key != "prepare-key-"+mockDest {
		t.Fatalf("the first key must stay: %s", key)
	}
}

func TestPrepareMergesTagsWithHistory(t *testing.T) {
	h := newHarness(t)
	params := map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001", "tags": []string{"yoga", "breathing"}}
	run[prepareOutput](h, "publish.prepare", params)
	if got := h.asset("CLIP-001").Tags; !slices.Equal(got, []string{"breathing", "yoga"}) {
		t.Fatalf("tags %v", got)
	}
	if len(h.store.HistoryEntries()) != 1 {
		t.Fatalf("history %+v", h.store.HistoryEntries())
	}
	run[prepareOutput](h, "publish.prepare", params, runOpts{key: "another-job"})
	if len(h.store.HistoryEntries()) != 1 {
		t.Fatal("merging the same tags changes nothing")
	}
}

// D37, default: approval before the upload.
func TestApprovalBeforeUpload(t *testing.T) {
	h := newHarness(t)
	_, err := h.runErr("publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "visibility": "public"})
	if !permanent(err) || !errs.IsCode(err, errs.CodeFailedPrecondition) || !strings.Contains(err.Error(), "D37") {
		t.Fatalf("got %v", err)
	}
	if s := h.mockState(); s.Uploads != 0 {
		t.Fatal("nothing may reach the destination")
	}
}

func TestRequireApprovalFalseDoesNotLiftTheGate(t *testing.T) {
	h := newHarness(t)
	h.cfg.Publishing.Defaults.RequireApproval = false
	_, err := h.runErr("publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest})
	if !permanent(err) || !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
}

func TestApprovalMustAuthorizeThisPublication(t *testing.T) {
	cases := map[string]func(*harness){
		"pending": func(h *harness) {
			h.addApproval("APR-001", domain.ApprovalPublication, domain.ApprovalPending, "CLIP-001", "v1")
		},
		"rejected": func(h *harness) {
			h.addApproval("APR-001", domain.ApprovalPublication, domain.ApprovalRejected, "CLIP-001", "v1")
		},
		"expired": func(h *harness) {
			h.addApproval("APR-001", domain.ApprovalPublication, domain.ApprovalExpired, "CLIP-001", "v1")
		},
		"editor's selection approval": func(h *harness) {
			h.addApproval("APR-001", domain.ApprovalClipSelection, domain.ApprovalApproved, "CLIP-001", "v1")
		},
		"another asset": func(h *harness) {
			h.addApproval("APR-001", domain.ApprovalPublication, domain.ApprovalApproved, "ORI-001", "v1")
		},
		"another version": func(h *harness) {
			h.addApproval("APR-001", domain.ApprovalPublication, domain.ApprovalApproved, "CLIP-001", "v2")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			setup(h)
			_, err := h.runErr("publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"})
			if !permanent(err) || !errs.IsCode(err, errs.CodeFailedPrecondition) {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("does not exist", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.runErr("publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-404"})
		if !permanent(err) || !errs.IsCode(err, errs.CodeFailedPrecondition) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestUploadCannotBypassTheGate(t *testing.T) {
	h := newHarness(t)
	h.cfg.Publishing.Defaults.AllowPrivateUploadBeforeApproval = true
	pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest}).PublicationID
	h.cfg.Publishing.Defaults.AllowPrivateUploadBeforeApproval = false

	_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
	if !permanent(err) || !strings.Contains(err.Error(), "D37") {
		t.Fatalf("got %v", err)
	}
	if h.mockState().Uploads != 0 {
		t.Fatal("nothing may reach the destination")
	}
}

// D37, with the option: private first, approval only to open visibility.
func TestPrivateUploadBeforeApproval(t *testing.T) {
	h := newHarness(t)
	h.cfg.Publishing.Defaults.AllowPrivateUploadBeforeApproval = true
	h.must(h.store.Assets().Update(h.ctx(), func() domain.Asset {
		a := h.asset("CLIP-001")
		a.Status = domain.AssetEditorialReview
		return a
	}()))
	pub := run[prepareOutput](h, "publish.prepare", map[string]any{
		"assetId": "CLIP-001", "destination": mockDest, "visibility": "public",
	}).PublicationID
	record := h.publication(pub)
	if record.Visibility != domain.VisibilityPrivate || record.ApprovalID != "" {
		t.Fatalf("no approval yet means private: %+v", record)
	}
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	if got := h.mockState().Videos[h.publication(pub).RemoteID].Visibility; got != domain.VisibilityPrivate {
		t.Fatalf("destination shows %s", got)
	}

	await := run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": pub})
	if await.Status != string(domain.PubPendingApproval) || !await.Ready {
		t.Fatalf("waiting for its approval: %+v", await)
	}

	// Opening visibility is refused without the approval.
	_, err := h.runErr("publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
	if !permanent(err) || !strings.Contains(err.Error(), "D37") {
		t.Fatalf("got %v", err)
	}
	if h.mockState().Videos[h.publication(pub).RemoteID].Visibility != domain.VisibilityPrivate {
		t.Fatal("the video must stay private")
	}

	// A person approves, and publish-video adopts the record (I143, I222).
	adopted := run[prepareOutput](h, "publish.prepare", map[string]any{
		"assetId": "CLIP-001", "destination": mockDest, "visibility": "public", "approvalId": "APR-001",
	}, runOpts{key: "publish-video-job"})
	if adopted.PublicationID != pub || adopted.AlreadyPublished {
		t.Fatalf("adopted %+v", adopted)
	}
	if h.publication(pub).ApprovalID != "APR-001" {
		t.Fatal("the approval must be recorded")
	}
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	await = run[awaitOutput](h, "publish.await_processing", map[string]any{"publicationId": pub})
	if !await.Ready || await.Status != "processing" {
		t.Fatalf("await %+v", await)
	}
	final := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public", "approvalId": "APR-001"})
	if final.Status != "published" || final.Visibility != "public" {
		t.Fatalf("finalize %+v", final)
	}
	if h.mockState().Uploads != 1 {
		t.Fatal("exactly one upload")
	}
	if a := h.asset("CLIP-001"); a.Status != domain.AssetPublished || a.Visibility != domain.VisibilityPublic {
		t.Fatalf("asset %s %s", a.Status, a.Visibility)
	}
}

func TestDestinationOptionWinsOverDefaults(t *testing.T) {
	yes, no := true, false

	h := newHarness(t)
	dest := h.cfg.Publishing.Destinations[mockDest]
	dest.AllowPrivateUploadBeforeApproval = &yes
	h.cfg.Publishing.Destinations[mockDest] = dest
	if _, err := h.runErr("publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest}); err != nil {
		t.Fatalf("destination allows it: %v", err)
	}

	h = newHarness(t)
	h.cfg.Publishing.Defaults.AllowPrivateUploadBeforeApproval = true
	dest = h.cfg.Publishing.Destinations[mockDest]
	dest.AllowPrivateUploadBeforeApproval = &no
	h.cfg.Publishing.Destinations[mockDest] = dest
	if _, err := h.runErr("publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest}); !permanent(err) {
		t.Fatalf("destination forbids it: %v", err)
	}
}

func TestDestinationVisibilityIsTheDefault(t *testing.T) {
	h := newHarness(t)
	dest := h.cfg.Publishing.Destinations[mockDest]
	dest.Visibility = domain.VisibilityUnlisted
	h.cfg.Publishing.Destinations[mockDest] = dest
	pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"}).PublicationID
	if got := h.publication(pub).Visibility; got != domain.VisibilityUnlisted {
		t.Fatalf("visibility %s", got)
	}
	h2 := newHarness(t)
	pub = run[prepareOutput](h2, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"}).PublicationID
	if got := h2.publication(pub).Visibility; got != domain.VisibilityPrivate {
		t.Fatalf("visibility %s: with no setting a publication stays private", got)
	}
}

func TestPrivateFinalizeNeedsNoApprovalAndStaysOutOfTheLibrary(t *testing.T) {
	h := newHarness(t)
	h.cfg.Publishing.Defaults.AllowPrivateUploadBeforeApproval = true
	pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest}).PublicationID
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	final := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub})
	if final.Status != "published" || final.Visibility != "private" {
		t.Fatalf("finalize %+v", final)
	}
	if v, _ := h.store.Versions().Get(h.ctx(), "CLIP-001", "v1"); !v.Immutable {
		t.Fatal("a confirmed publication makes the version immutable (D35)")
	}
	if a := h.asset("CLIP-001"); a.Status != domain.AssetApproved || a.Visibility != domain.VisibilityPrivate {
		t.Fatalf("no approval means no promotion: %s %s", a.Status, a.Visibility)
	}
	run[syncOutput](h, "catalog.public.sync", map[string]any{})
	if got := videosIn(t, h.publicDB()); got != 0 {
		t.Fatalf("videos %d", got)
	}
}

// D7: changing the visibility of a published video needs a new approval.
func TestChangingAPublishedVideoNeedsANewApproval(t *testing.T) {
	h := newHarness(t)
	pub := h.published()

	for name, params := range map[string]map[string]any{
		"no approval":   {"publicationId": pub, "visibility": "unlisted"},
		"same approval": {"publicationId": pub, "visibility": "unlisted", "approvalId": "APR-001"},
	} {
		if _, err := h.runErr("publish.finalize", params); !permanent(err) || !strings.Contains(err.Error(), "D7") {
			t.Fatalf("%s: got %v", name, err)
		}
	}
	if got := h.publication(pub).Visibility; got != domain.VisibilityPublic {
		t.Fatalf("visibility %s", got)
	}

	h.addApproval("APR-002", domain.ApprovalVisibilityChange, domain.ApprovalApproved, "CLIP-001", "v1")
	out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "unlisted", "approvalId": "APR-002"})
	if out.Visibility != "unlisted" {
		t.Fatalf("finalize %+v", out)
	}
	record := h.publication(pub)
	if record.Status != domain.PubPublished || record.ApprovalID != "APR-002" || record.IsPublic() {
		t.Fatalf("record %+v", record)
	}
	if h.mockState().Videos[record.RemoteID].Visibility != domain.VisibilityUnlisted {
		t.Fatal("destination unchanged")
	}
	if hasEvent(h.store.Events(), event.PublicationCompleted) != 1 || hasEvent(h.store.Events(), event.PublicationUpdated) != 1 {
		t.Fatal("a change is an update, not a second completion")
	}
	if len(h.store.AuditRecords()) != 2 {
		t.Fatal("each visibility change is audited (D37)")
	}
	if a := h.asset("CLIP-001"); a.Visibility != domain.VisibilityUnlisted {
		t.Fatalf("asset visibility %s", a.Visibility)
	}
}

func TestPublishAgainAtAnotherVisibilityReachesFinalize(t *testing.T) {
	h := newHarness(t)
	pub := h.published()
	h.addApproval("APR-002", domain.ApprovalPublication, domain.ApprovalApproved, "CLIP-001", "v1")
	out := run[prepareOutput](h, "publish.prepare", map[string]any{
		"assetId": "CLIP-001", "destination": mockDest, "visibility": "unlisted", "approvalId": "APR-002",
	}, runOpts{key: "another-job"})
	if out.PublicationID != pub || out.AlreadyPublished {
		t.Fatalf("prepare %+v", out)
	}
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	if h.mockState().Uploads != 1 {
		t.Fatal("no second upload")
	}
	run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "unlisted", "approvalId": "APR-002"})
	if got := h.publication(pub).Visibility; got != domain.VisibilityUnlisted {
		t.Fatalf("visibility %s", got)
	}
}

// D11: an unknown outcome is settled before any new attempt.
func TestOutcomeUnknownThenReconcile(t *testing.T) {
	t.Run("the upload did reach the destination", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"inject": map[string]int{mock.InjectOutcomeUnknown: 1}})
		pub := h.prepared().PublicationID
		_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
		if !node.IsUnknownOutcome(err) {
			t.Fatalf("got %v", err)
		}
		if got := h.publication(pub); got.Status != domain.PubUploading || got.RemoteID != "" {
			t.Fatalf("record %+v", got)
		}

		ctx, e, in := h.input("publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
		outcome, res, err := h.node("publish.upload").(node.Reconciler).Reconcile(ctx, e, in)
		if err != nil || outcome != node.ReconcileDone {
			t.Fatalf("%s, %v", outcome, err)
		}
		h.checkOutput("publish.upload", res)
		record := h.publication(pub)
		if record.RemoteID == "" || record.Status != domain.PubProcessing {
			t.Fatalf("record %+v", record)
		}
		if h.mockState().Uploads != 1 {
			t.Fatal("exactly one upload")
		}
	})

	t.Run("the upload did not reach the destination", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"inject": map[string]int{mock.InjectOutcomeUnknownLost: 1}})
		pub := h.prepared().PublicationID
		if _, err := h.runErr("publish.upload", map[string]any{"publicationId": pub}); !node.IsUnknownOutcome(err) {
			t.Fatalf("got %v", err)
		}
		ctx, e, in := h.input("publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
		outcome, _, err := h.node("publish.upload").(node.Reconciler).Reconcile(ctx, e, in)
		if err != nil || outcome != node.ReconcileNotStarted {
			t.Fatalf("%s, %v", outcome, err)
		}
		res, err := h.runErr("publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Usage) != 1 || h.mockState().Uploads != 1 {
			t.Fatalf("usage %+v, uploads %d", res.Usage, h.mockState().Uploads)
		}
	})

	t.Run("a destination that cannot reconcile needs a person", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"reconcile": false, "inject": map[string]int{mock.InjectOutcomeUnknown: 1}})
		pub := h.prepared().PublicationID
		if _, err := h.runErr("publish.upload", map[string]any{"publicationId": pub}); !node.IsUnknownOutcome(err) {
			t.Fatalf("got %v", err)
		}
		ctx, e, in := h.input("publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
		outcome, _, err := h.node("publish.upload").(node.Reconciler).Reconcile(ctx, e, in)
		if outcome != node.ReconcileUnknown || !errs.IsCode(err, errs.CodeCapabilityUnsupported) {
			t.Fatalf("%s, %v", outcome, err)
		}
		// Even if the engine ran it anyway, Execute refuses to upload blind.
		_, err = h.runErr("publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
		if !node.IsUnknownOutcome(err) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a restart with the record still uploading reconciles first", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"inject": map[string]int{mock.InjectOutcomeUnknown: 1}})
		pub := h.prepared().PublicationID
		if _, err := h.runErr("publish.upload", map[string]any{"publicationId": pub}); !node.IsUnknownOutcome(err) {
			t.Fatalf("got %v", err)
		}
		// Attempt 1 again: the engine re-dispatched after a crash, with no retry count.
		out := run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 1})
		if out.RemoteID == "" || h.mockState().Uploads != 1 || h.mockState().Repeats != 0 {
			t.Fatalf("output %+v, state %+v", out, h.mockState())
		}
	})

	t.Run("reconcile with a recorded remote ID asks nobody", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		ctx, e, in := h.input("publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
		outcome, _, err := h.node("publish.upload").(node.Reconciler).Reconcile(ctx, e, in)
		if err != nil || outcome != node.ReconcileDone {
			t.Fatalf("%s, %v", outcome, err)
		}
	})
}

func TestTransientFailureResumes(t *testing.T) {
	h := newHarness(t)
	h.setMockParams(map[string]any{"inject": map[string]int{mock.InjectFailUpload: 1}})
	pub := h.prepared().PublicationID
	_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
	if !node.IsTransient(err) {
		t.Fatalf("got %v", err)
	}
	if got := h.publication(pub).Status; got != domain.PubUploading {
		t.Fatalf("status %s", got)
	}
	out := run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
	if out.Status != "processing" || h.mockState().Resumes != 1 || h.mockState().Uploads != 1 {
		t.Fatalf("output %+v, state %+v", out, h.mockState())
	}
}

func TestPermanentFailureMarksTheRecord(t *testing.T) {
	t.Run("file does not match its checksum", func(t *testing.T) {
		h := newHarness(t)
		h.storages[domain.RoleMaster].objects[masterKey] = "corrupted"
		pub := h.prepared().PublicationID
		_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
		if !permanent(err) {
			t.Fatalf("got %v", err)
		}
		record := h.publication(pub)
		if record.Status != domain.PubFailed || !strings.Contains(record.StatusReason, "checksum") {
			t.Fatalf("record %+v", record)
		}
		if h.mockState().Uploads != 0 {
			t.Fatal("a corrupt file must not go out")
		}
		if hasEvent(h.store.Events(), event.PublicationUpdated) != 1 {
			t.Fatal("updated must fire")
		}

		// The file is repaired and a person retries: the record can go again.
		h.storages[domain.RoleMaster].objects[masterKey] = masterBytes
		out := run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub}, runOpts{attempt: 2})
		if out.Status != "processing" {
			t.Fatalf("output %+v", out)
		}
	})

	t.Run("file missing", func(t *testing.T) {
		h := newHarness(t)
		delete(h.storages[domain.RoleMaster].objects, masterKey)
		pub := h.prepared().PublicationID
		if _, err := h.runErr("publish.upload", map[string]any{"publicationId": pub}); !permanent(err) {
			t.Fatalf("got %v", err)
		}
		if got := h.publication(pub).Status; got != domain.PubFailed {
			t.Fatalf("status %s", got)
		}
	})

	t.Run("master replaced after prepare", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		h.must(h.store.Versions().PutFile(h.ctx(), "CLIP-001", "v1", domain.VersionFile{
			Purpose: domain.FileWorkingMaster, File: domain.MediaFile{Storage: domain.RoleMaster, Key: masterKey, SHA256: sha("other")},
		}))
		_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
		if !permanent(err) || !errs.IsCode(err, errs.CodeFailedPrecondition) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("finished records refuse an upload", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		if _, err := h.runErr("publish.upload", map[string]any{"publicationId": pub}); !permanent(err) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestUploadCancellation(t *testing.T) {
	h := newHarness(t)
	h.addScripted("cancelled", func(s *scripted) {
		s.upload = func(context.Context, connector.UploadRequest) (connector.RemoteVideo, error) {
			return connector.RemoteVideo{}, context.Canceled
		}
	})
	pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "cancelled", "approvalId": "APR-001"}).PublicationID
	_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
	if !node.IsTransient(err) {
		t.Fatalf("got %v", err)
	}
	record := h.publication(pub)
	if record.Status != domain.PubFailed || record.StatusReason != "upload cancelled" {
		t.Fatalf("record %+v", record)
	}
}

func TestOnlyOneTaskWorksOnARecord(t *testing.T) {
	h := newHarness(t)
	pub := h.prepared().PublicationID
	n := h.node("publish.upload").(*uploadNode)
	unlock, ok := n.locks.tryLock(string(h.publication(pub).ULID))
	if !ok {
		t.Fatal("first lock")
	}
	_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
	if !node.IsTransient(err) || !strings.Contains(err.Error(), "another task") {
		t.Fatalf("got %v", err)
	}
	_, err = h.runErr("publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
	if !node.IsTransient(err) {
		t.Fatalf("withdraw during an upload must retry: %v", err)
	}
	if h.publication(pub).Status == domain.PubWithdrawn {
		t.Fatal("must not withdraw a record another task holds")
	}
	unlock()
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
}

func TestUploadWithoutRangeReadsUsesTheLocalCopy(t *testing.T) {
	h := newHarness(t)
	h.storages[domain.RoleMaster].noRangeReads = true
	pub := h.prepared().PublicationID
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	if len(h.inputs) != 1 || h.inputs[0] != "storage://master/"+masterKey {
		t.Fatalf("inputs %v", h.inputs)
	}
	if got := h.mockState().Videos[h.publication(pub).RemoteID].SHA256; got != sha(masterBytes) {
		t.Fatalf("destination received %s", got)
	}
}

func TestUploadPricesItsUsage(t *testing.T) {
	h := newHarness(t)
	big := strings.Repeat("x", 1<<20)
	h.storages[domain.RoleMaster].objects[masterKey] = big
	h.must(h.store.Versions().PutFile(h.ctx(), "CLIP-001", "v1", domain.VersionFile{
		Purpose: domain.FileWorkingMaster,
		File:    domain.MediaFile{Storage: domain.RoleMaster, Key: masterKey, SHA256: sha(big), SizeBytes: int64(len(big))},
	}))
	h.cfg.Budget.PerUnitMinor = map[string]float64{"publish.upload": 2048}
	pub := h.prepared().PublicationID
	res, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage[0].EstimatedMinor != 2 || res.Usage[0].Unit != "GiB" {
		t.Fatalf("usage %+v", res.Usage[0])
	}
}

func TestDestinationWithoutAPrivateState(t *testing.T) {
	caps := connector.PublishingCapabilities{Update: true, Delete: true, Reconcile: true, Visibilities: []domain.Visibility{domain.VisibilityPublic}}

	t.Run("approved upload goes directly to the target", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("open", func(s *scripted) { s.caps = &caps })
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "open", "visibility": "public", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		s, err := mock.ReadState(h.stateDir, tenantID, "open")
		h.must(err)
		if got := s.Videos[h.publication(pub).RemoteID].Visibility; got != domain.VisibilityPublic {
			t.Fatalf("visibility %s", got)
		}
	})

	t.Run("no approval is refused", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Publishing.Defaults.AllowPrivateUploadBeforeApproval = true
		h.addScripted("open", func(s *scripted) { s.caps = &caps })
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "open"}).PublicationID
		_, err := h.runErr("publish.upload", map[string]any{"publicationId": pub})
		if !permanent(err) || !strings.Contains(err.Error(), "no private state") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("withdrawal deletes instead", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("open", func(s *scripted) { s.caps = &caps })
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "open", "visibility": "public", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		s, err := mock.ReadState(h.stateDir, tenantID, "open")
		h.must(err)
		if !s.Videos[h.publication(pub).RemoteID].Deleted {
			t.Fatal("must be deleted")
		}
	})
}

func TestScheduledPublication(t *testing.T) {
	h := newHarness(t)
	pub := h.prepared().PublicationID
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	at := time.Now().Add(400 * time.Millisecond).UTC().Format(time.RFC3339Nano)

	final := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public", "scheduledAt": at})
	if final.Status != "scheduled" {
		t.Fatalf("finalize %+v", final)
	}
	record := h.publication(pub)
	if record.Status != domain.PubScheduled || record.PublishedAt != nil {
		t.Fatalf("record %+v", record)
	}
	if hasEvent(h.store.Events(), event.PublicationCompleted) != 0 {
		t.Fatal("not completed yet")
	}
	if a := h.asset("CLIP-001"); a.Status == domain.AssetPublished {
		t.Fatal("not published yet")
	}
	if v, _ := h.store.Versions().Get(h.ctx(), "CLIP-001", "v1"); v.Immutable {
		t.Fatal("not confirmed yet (D35)")
	}

	// Before the date, a refresh changes nothing.
	out := run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
	if out.Publications[0].Status != "scheduled" {
		t.Fatalf("refresh %+v", out)
	}

	time.Sleep(450 * time.Millisecond)
	out = run[refreshOutput](h, "publish.refresh", map[string]any{"assetId": "CLIP-001"})
	if out.Publications[0].Status != "published" || !out.Publications[0].Changed {
		t.Fatalf("refresh %+v", out)
	}
	record = h.publication(pub)
	if !record.IsPublic() || record.PublishedAt == nil || record.URL == "" {
		t.Fatalf("record %+v", record)
	}
	if hasEvent(h.store.Events(), event.PublicationCompleted) != 1 {
		t.Fatal("completed must fire once")
	}
	if a := h.asset("CLIP-001"); a.Status != domain.AssetPublished || a.Visibility != domain.VisibilityPublic {
		t.Fatalf("asset %s %s", a.Status, a.Visibility)
	}
	if v, _ := h.store.Versions().Get(h.ctx(), "CLIP-001", "v1"); !v.Immutable {
		t.Fatal("immutable after the confirmed publication (D35)")
	}
}

func TestSchedulingNeedsItsPreconditions(t *testing.T) {
	h := newHarness(t)
	pub := h.prepared().PublicationID
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	if _, err := h.runErr("publish.finalize", map[string]any{"publicationId": pub, "scheduledAt": "tomorrow"}); !permanent(err) || !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("got %v", err)
	}

	h = newHarness(t)
	h.addScripted("plain", func(s *scripted) {
		caps := connector.PublishingCapabilities{Visibilities: []domain.Visibility{domain.VisibilityPrivate, domain.VisibilityPublic}}
		s.caps = &caps
	})
	pub = run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "plain", "approvalId": "APR-001"}).PublicationID
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	_, err := h.runErr("publish.finalize", map[string]any{"publicationId": pub, "scheduledAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if !permanent(err) || !errs.IsCode(err, errs.CodeCapabilityUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func TestFinalizeNeedsAnUpload(t *testing.T) {
	h := newHarness(t)
	pub := h.prepared().PublicationID
	if _, err := h.runErr("publish.finalize", map[string]any{"publicationId": pub}); !permanent(err) || !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
}

// D30: public captions never carry a sensitive segment.
func TestCaptionsExcludeSensitiveSegments(t *testing.T) {
	h := newHarness(t)
	pub := h.prepared().PublicationID
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	out := run[finalizeOutput](h, "publish.finalize", map[string]any{
		"publicationId": pub, "visibility": "public", "captions": map[string]any{"language": "en"},
	})
	if out.Captions != stepSent {
		t.Fatalf("finalize %+v", out)
	}
	caption := h.mockState().Videos[h.publication(pub).RemoteID].Captions["en"]
	if caption.Format != "vtt" || !strings.HasPrefix(caption.Text, "WEBVTT") || !strings.Contains(caption.Text, publicText) {
		t.Fatalf("caption %+v", caption)
	}
	if strings.Contains(caption.Text, "Maria") || strings.Contains(caption.Text, "diagnosis") {
		t.Fatalf("sensitive text reached the destination: %s", caption.Text)
	}
	if !strings.Contains(caption.Text, "00:00:20.000 --> 00:00:30.000") {
		t.Fatalf("timing lost: %s", caption.Text)
	}
}

func TestCaptionsExcludeAMarkOnTheSource(t *testing.T) {
	h := newHarness(t)
	// Source 125 is clip 25, inside the third segment. The mark came after derivation.
	h.must(h.store.Transcripts().MarkSensitive(h.ctx(), domain.SensitiveSegment{
		AssetID: "ORI-001", Version: "v1", StartSeconds: 125, EndSeconds: 126,
	}))
	pub := h.prepared().PublicationID
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public", "captions": map[string]any{}})
	caption := h.mockState().Videos[h.publication(pub).RemoteID].Captions["en"]
	if strings.Contains(caption.Text, "exhale") {
		t.Fatalf("a source's mark must reach the clip's captions: %s", caption.Text)
	}
}

func TestCaptionsOnlyWithAnApprovedPublicVideo(t *testing.T) {
	h := newHarness(t)
	h.cfg.Publishing.Defaults.AllowPrivateUploadBeforeApproval = true
	pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest}).PublicationID
	run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "captions": map[string]any{"language": "en"}})
	if out.Captions != stepSkipped {
		t.Fatalf("finalize %+v", out)
	}
	if got := h.mockState().Videos[h.publication(pub).RemoteID].Captions; len(got) != 0 {
		t.Fatalf("captions reached a private video: %+v", got)
	}

	h2 := newHarness(t)
	pub = run[prepareOutput](h2, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"}).PublicationID
	run[uploadOutput](h2, "publish.upload", map[string]any{"publicationId": pub})
	out = run[finalizeOutput](h2, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "unlisted", "captions": map[string]any{"language": "en"}})
	if out.Captions != stepSkipped {
		t.Fatalf("D30 says public, and unlisted is not: %+v", out)
	}
}

func TestCaptionOutcomes(t *testing.T) {
	t.Run("unreadable marks send nothing and do not stop the publication", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		h.store.SensitiveErr = func(id.Ref, string) error { return errors.New("busy") }
		out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public", "captions": map[string]any{}})
		if out.Captions != stepFailed || out.Status != "published" {
			t.Fatalf("finalize %+v", out)
		}
		if got := h.mockState().Videos[h.publication(pub).RemoteID].Captions; len(got) != 0 {
			t.Fatalf("captions %+v", got)
		}
	})

	t.Run("srt format", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public", "captions": map[string]any{"format": "srt", "language": "pt-BR"}})
		caption := h.mockState().Videos[h.publication(pub).RemoteID].Captions["pt-BR"]
		if caption.Format != "srt" || !strings.Contains(caption.Text, "00:00:00,000 --> 00:00:10,000") || strings.Contains(caption.Text, "WEBVTT") {
			t.Fatalf("caption %+v", caption)
		}
	})

	t.Run("no transcript is a skip", func(t *testing.T) {
		h := newHarness(t)
		h.must(h.store.Assets().Create(h.ctx(), domain.Asset{ID: "CLIP-002", ULID: id.New(), TenantID: tenantID, Kind: domain.KindClip, Title: "No words", Status: domain.AssetApproved, CurrentVersion: "v1", MasterAvailable: true}))
		h.must(h.store.Versions().Create(h.ctx(), domain.AssetVersion{AssetID: "CLIP-002", Version: "v1", Files: []domain.VersionFile{{Purpose: domain.FileWorkingMaster, File: domain.MediaFile{Storage: domain.RoleMaster, Key: masterKey, SHA256: sha(masterBytes)}}}}))
		h.addApproval("APR-002", domain.ApprovalPublication, domain.ApprovalApproved, "CLIP-002", "v1")
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-002", "destination": mockDest, "visibility": "public", "approvalId": "APR-002"}, runOpts{key: "k2"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public", "captions": map[string]any{}})
		if out.Captions != stepSkipped {
			t.Fatalf("finalize %+v", out)
		}
	})

	t.Run("destination without captions is a skip", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("plain", func(s *scripted) {
			caps := connector.PublishingCapabilities{Visibilities: []domain.Visibility{domain.VisibilityPrivate, domain.VisibilityPublic}}
			s.caps = &caps
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "plain", "visibility": "public", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "captions": map[string]any{}})
		if out.Captions != stepSkipped || out.Thumbnail != stepSkipped || out.Status != "published" {
			t.Fatalf("finalize %+v", out)
		}
	})

	t.Run("a transient destination error retries and a refusal is noted", func(t *testing.T) {
		h := newHarness(t)
		failure := errs.New(errs.CodeUnavailable, "try later")
		h.addScripted("flaky", func(s *scripted) {
			s.captions = func(context.Context, string, string, string, ioReader) error { return failure }
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "flaky", "visibility": "public", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		_, err := h.runErr("publish.finalize", map[string]any{"publicationId": pub, "captions": map[string]any{}})
		if !node.IsTransient(err) {
			t.Fatalf("got %v", err)
		}
		failure = errs.New(errs.CodeForbidden, "not allowed")
		out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "captions": map[string]any{}})
		if out.Captions != stepFailed || out.Status != "published" {
			t.Fatalf("finalize %+v", out)
		}
	})
}

func TestRenderCaptions(t *testing.T) {
	segs := []domain.TranscriptSegment{
		{Start: 3661.5, End: 3662.25, Text: "  two\nlines --> arrow "},
		{Start: 5, End: 6, Text: "   "},
	}
	vtt, err := renderCaptions("vtt", segs)
	if err != nil {
		t.Fatal(err)
	}
	want := "WEBVTT\n\n1\n01:01:01.500 --> 01:01:02.250\ntwo lines -> arrow\n\n"
	if vtt != want {
		t.Fatalf("got %q, want %q", vtt, want)
	}
	if out, err := renderCaptions("vtt", segs[1:]); err != nil || out != "" {
		t.Fatalf("all blank: %q, %v", out, err)
	}
	if _, err := renderCaptions("ass", segs); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("got %v", err)
	}
}

func TestThumbnails(t *testing.T) {
	const thumbKey = "clips/CLIP-001/v1/thumbnail.jpg"

	t.Run("the asset's thumbnail by default", func(t *testing.T) {
		h := newHarness(t)
		h.must(h.store.Versions().PutFile(h.ctx(), "CLIP-001", "v1", domain.VersionFile{
			Purpose: domain.FileThumbnail, File: domain.MediaFile{Storage: domain.RoleProxy, Key: thumbKey},
		}))
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
		if out.Thumbnail != stepSent {
			t.Fatalf("finalize %+v", out)
		}
		if got := h.mockState().Videos[h.publication(pub).RemoteID].ThumbnailSHA256; got != sha("jpeg") {
			t.Fatalf("thumbnail %s", got)
		}
	})

	t.Run("a parameter wins", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public", "thumbnail": "storage://proxy/" + thumbKey})
		if out.Thumbnail != stepSent {
			t.Fatalf("finalize %+v", out)
		}
	})

	t.Run("no thumbnail is a skip", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
		if out.Thumbnail != stepSkipped || out.Status != "published" {
			t.Fatalf("finalize %+v", out)
		}
	})

	t.Run("archive is refused and the publication goes on", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public", "thumbnail": "storage://archive/" + masterKey})
		if out.Thumbnail != stepFailed || out.Status != "published" {
			t.Fatalf("finalize %+v", out)
		}
		if len(h.reads[domain.RoleArchive]) != 0 {
			t.Fatal("archive read")
		}
	})

	t.Run("a refused thumbnail does not stop the publication", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("picky", func(s *scripted) {
			s.thumb = func(context.Context, string, ioReader, string) error {
				return errs.New(errs.CodeForbidden, "channel not verified")
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "picky", "visibility": "public", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "thumbnail": "storage://proxy/" + thumbKey})
		if out.Thumbnail != stepFailed || out.Status != "published" {
			t.Fatalf("finalize %+v", out)
		}
	})

	t.Run("a transient thumbnail error retries", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("flaky", func(s *scripted) {
			s.thumb = func(context.Context, string, ioReader, string) error {
				return errs.New(errs.CodeUnavailable, "try later")
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "flaky", "visibility": "public", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		_, err := h.runErr("publish.finalize", map[string]any{"publicationId": pub, "thumbnail": "storage://proxy/" + thumbKey})
		if !node.IsTransient(err) {
			t.Fatalf("got %v", err)
		}
		if h.publication(pub).Status == domain.PubPublished {
			t.Fatal("must not be published")
		}
	})
}

func TestWithdrawModes(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		h := newHarness(t)
		pub := h.published()
		run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true, "mode": "delete"})
		record := h.publication(pub)
		if record.Status != domain.PubWithdrawn || !strings.Contains(record.StatusReason, "delete") {
			t.Fatalf("record %+v", record)
		}
		if !h.mockState().Videos[record.RemoteID].Deleted {
			t.Fatal("must be deleted")
		}
	})

	t.Run("video already gone counts as done", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("gone", func(s *scripted) {
			s.setVis = func(context.Context, string, domain.Visibility) (connector.RemoteVideo, error) {
				return connector.RemoteVideo{}, errs.NotFound("video")
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "gone", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		if got := h.publication(pub).Status; got != domain.PubWithdrawn {
			t.Fatalf("status %s", got)
		}
	})

	t.Run("destination that cannot set visibility deletes instead", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("rigid", func(s *scripted) {
			s.setVis = func(context.Context, string, domain.Visibility) (connector.RemoteVideo, error) {
				return connector.RemoteVideo{}, errs.New(errs.CodeCapabilityUnsupported, "no")
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "rigid", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		s, err := mock.ReadState(h.stateDir, tenantID, "rigid")
		h.must(err)
		if !s.Videos[h.publication(pub).RemoteID].Deleted {
			t.Fatal("must be deleted")
		}
	})
}

func TestWithdrawSelection(t *testing.T) {
	two := func(t *testing.T) (*harness, string, string) {
		h := newHarness(t)
		h.addScripted("second", func(*scripted) {})
		one := h.published()
		second := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "second", "visibility": "public", "approvalId": "APR-001"}, runOpts{key: "second-key"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": second})
		run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": second, "visibility": "public"})
		return h, one, second
	}

	t.Run("one destination leaves the other and the asset public", func(t *testing.T) {
		h, one, second := two(t)
		out := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "destinations": []string{mockDest}})
		if len(out.Withdrawn) != 1 || out.Withdrawn[0] != one {
			t.Fatalf("withdraw %+v", out)
		}
		if h.publication(second).Status != domain.PubPublished {
			t.Fatal("the other publication must stay")
		}
		if a := h.asset("CLIP-001"); a.Status != domain.AssetPublished || a.Visibility != domain.VisibilityPublic {
			t.Fatalf("asset %s %s", a.Status, a.Visibility)
		}
		run[syncOutput](h, "catalog.public.sync", map[string]any{})
		if videosIn(t, h.publicDB()) != 1 {
			t.Fatal("the video stays in the library through its other destination")
		}

		run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "destinations": []string{"second"}})
		if a := h.asset("CLIP-001"); a.Status != domain.AssetWithdrawn || a.Visibility != domain.VisibilityPrivate {
			t.Fatalf("asset %s %s", a.Status, a.Visibility)
		}
	})

	t.Run("a failing destination does not hold back the other", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"inject": map[string]int{mock.InjectFailRemove: 1}})
		h.addScripted("second", func(s *scripted) {})
		one := h.published()
		second := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "second", "visibility": "public", "approvalId": "APR-001"}, runOpts{key: "second-key"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": second})
		run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": second, "visibility": "public"})

		_, err := h.runErr("publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		if !node.IsTransient(err) {
			t.Fatalf("got %v", err)
		}
		if h.publication(second).Status != domain.PubWithdrawn {
			t.Fatal("the healthy destination must be done")
		}
		if h.publication(one).Status != domain.PubPublished {
			t.Fatal("the failing one stays")
		}
		if a := h.asset("CLIP-001"); a.Status != domain.AssetPublished {
			t.Fatalf("asset %s", a.Status)
		}

		// The retry finishes the job and reports what was done before.
		out := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true}, runOpts{attempt: 2})
		if len(out.Withdrawn) != 1 || out.Withdrawn[0] != one || len(out.Already) != 1 || out.Already[0] != second {
			t.Fatalf("withdraw %+v", out)
		}
		if a := h.asset("CLIP-001"); a.Status != domain.AssetWithdrawn {
			t.Fatalf("asset %s", a.Status)
		}
	})

	t.Run("needs a selection, never everything by default", func(t *testing.T) {
		h := newHarness(t)
		h.published()
		if _, err := h.runErr("publish.withdraw", map[string]any{"assetId": "CLIP-001"}); !permanent(err) || !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a typo is an error", func(t *testing.T) {
		h := newHarness(t)
		h.published()
		if _, err := h.runErr("publish.withdraw", map[string]any{"assetId": "CLIP-001", "destinations": []string{"mok-main"}}); !permanent(err) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a configured destination with no record is a no-op", func(t *testing.T) {
		h := newHarness(t)
		out := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "destinations": []string{mockDest}})
		if len(out.Withdrawn) != 0 || len(out.Already) != 0 {
			t.Fatalf("withdraw %+v", out)
		}
		if len(h.store.Events()) != 0 {
			t.Fatal("no event")
		}
	})

	t.Run("a removed destination's record can still be withdrawn", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		delete(h.cfg.Publishing.Destinations, mockDest)
		out := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "destinations": []string{mockDest}})
		if len(out.Withdrawn) != 1 || out.Withdrawn[0] != pub {
			t.Fatalf("withdraw %+v", out)
		}
	})

	t.Run("unknown mode", func(t *testing.T) {
		h := newHarness(t)
		ctx, e, in := h.input("publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true}, runOpts{})
		in.Parameters = json.RawMessage(`{"assetId":"CLIP-001","all":true,"mode":"burn"}`)
		if _, err := h.node("publish.withdraw").Execute(ctx, e, in); !permanent(err) || !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("unknown asset", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.runErr("publish.withdraw", map[string]any{"assetId": "CLIP-999", "all": true}); !permanent(err) || !errs.IsCode(err, errs.CodeNotFound) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestWithdrawBeforeAndDuringUpload(t *testing.T) {
	t.Run("a record that never left needs no destination", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		out := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		if len(out.Withdrawn) != 1 || h.publication(pub).Status != domain.PubWithdrawn {
			t.Fatalf("withdraw %+v", out)
		}
		if a := h.asset("CLIP-001"); a.Status != domain.AssetApproved {
			t.Fatalf("an asset never published keeps its state: %s", a.Status)
		}
	})

	t.Run("a lost upload is found first", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"inject": map[string]int{mock.InjectOutcomeUnknown: 1}})
		pub := h.prepared().PublicationID
		if _, err := h.runErr("publish.upload", map[string]any{"publicationId": pub}); !node.IsUnknownOutcome(err) {
			t.Fatalf("got %v", err)
		}
		record := h.publication(pub)
		if record.RemoteID != "" {
			t.Fatalf("record %+v", record)
		}
		run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		remote := mock.RemoteID(record.IdempotencyKey)
		if got := h.mockState().Videos[remote].Visibility; got != domain.VisibilityPrivate {
			t.Fatalf("visibility %s", got)
		}
		if h.publication(pub).Status != domain.PubWithdrawn {
			t.Fatal("status")
		}
	})

	t.Run("a destination that cannot say does not get a false withdrawal", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"reconcile": false, "inject": map[string]int{mock.InjectOutcomeUnknown: 1}})
		pub := h.prepared().PublicationID
		if _, err := h.runErr("publish.upload", map[string]any{"publicationId": pub}); !node.IsUnknownOutcome(err) {
			t.Fatalf("got %v", err)
		}
		if _, err := h.runErr("publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true}); err == nil {
			t.Fatal("must not claim withdrawn while a video may exist")
		}
		if h.publication(pub).Status == domain.PubWithdrawn {
			t.Fatal("status")
		}
	})

	t.Run("a failed upload that never arrived is already gone", func(t *testing.T) {
		h := newHarness(t)
		h.storages[domain.RoleMaster].objects[masterKey] = "corrupted"
		pub := h.prepared().PublicationID
		if _, err := h.runErr("publish.upload", map[string]any{"publicationId": pub}); !permanent(err) {
			t.Fatalf("got %v", err)
		}
		out := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		if len(out.Already) != 1 || h.publication(pub).Status != domain.PubFailed {
			t.Fatalf("withdraw %+v", out)
		}
	})

	t.Run("a rejected video is taken down", func(t *testing.T) {
		h := newHarness(t)
		h.setMockParams(map[string]any{"inject": map[string]int{mock.InjectReject: 1}})
		pub := h.prepared().PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		if len(out.Withdrawn) != 1 || h.publication(pub).Status != domain.PubWithdrawn {
			t.Fatalf("withdraw %+v", out)
		}
	})
}

func TestRefresh(t *testing.T) {
	t.Run("no change emits nothing", func(t *testing.T) {
		h := newHarness(t)
		pub := h.published()
		before := len(h.store.Events())
		out := run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
		if out.Publications[0].Changed || len(h.store.Events()) != before {
			t.Fatalf("refresh %+v", out)
		}
	})

	t.Run("removed at the destination counts as withdrawn", func(t *testing.T) {
		h := newHarness(t)
		pub := h.published()
		conn, err := h.resolver.Publishing(h.ctx(), tenantID, mockDest)
		h.must(err)
		h.must(conn.Delete(h.ctx(), h.publication(pub).RemoteID))
		run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
		record := h.publication(pub)
		if record.Status != domain.PubWithdrawn || record.Visibility != domain.VisibilityPrivate || !strings.Contains(record.StatusReason, "removed") {
			t.Fatalf("record %+v", record)
		}
		if hasEvent(h.store.Events(), event.PublicationWithdrawn) != 1 || hasEvent(h.store.Events(), event.PublicationUpdated) != 1 {
			t.Fatal("the library must hear about it")
		}
		if a := h.asset("CLIP-001"); a.Status != domain.AssetWithdrawn {
			t.Fatalf("asset %s", a.Status)
		}
	})

	t.Run("a live video the destination no longer lists counts as removed", func(t *testing.T) {
		h := newHarness(t)
		gone := false
		h.addScripted("odd", func(s *scripted) {
			s.getStatus = func(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
				if gone {
					return connector.RemoteVideo{}, errs.NotFound("video")
				}
				return s.Publishing.GetStatus(ctx, remoteID)
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "odd", "visibility": "public", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
		gone = true
		run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
		if got := h.publication(pub).Status; got != domain.PubWithdrawn {
			t.Fatalf("status %s", got)
		}
	})

	t.Run("a fresh upload not yet listed is not a withdrawal", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("odd", func(s *scripted) {
			s.getStatus = func(context.Context, string) (connector.RemoteVideo, error) {
				return connector.RemoteVideo{}, errs.NotFound("video")
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "odd", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		out := run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
		if out.Publications[0].Changed || h.publication(pub).Status != domain.PubProcessing {
			t.Fatalf("refresh %+v", out)
		}
	})

	t.Run("a published video blocked later", func(t *testing.T) {
		h := newHarness(t)
		var blocked bool
		h.addScripted("odd", func(s *scripted) {
			s.getStatus = func(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
				if blocked {
					return connector.RemoteVideo{RemoteID: remoteID, Status: connector.RemoteRejected, Reason: "copyright"}, nil
				}
				return s.Publishing.GetStatus(ctx, remoteID)
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "odd", "visibility": "public", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
		blocked = true
		run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
		record := h.publication(pub)
		if record.Status != domain.PubRejectedByDestination || record.StatusReason != "copyright" || record.IsPublic() {
			t.Fatalf("record %+v", record)
		}
		run[syncOutput](h, "catalog.public.sync", map[string]any{})
		if videosIn(t, h.publicDB()) != 0 {
			t.Fatal("a blocked video leaves the library")
		}
	})

	t.Run("visibility changed at the destination", func(t *testing.T) {
		h := newHarness(t)
		pub := h.published()
		conn, err := h.resolver.Publishing(h.ctx(), tenantID, mockDest)
		h.must(err)
		_, err = conn.SetVisibility(h.ctx(), h.publication(pub).RemoteID, domain.VisibilityPrivate)
		h.must(err)
		before := len(h.store.AuditRecords())
		run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
		record := h.publication(pub)
		if record.Status != domain.PubPublished || record.Visibility != domain.VisibilityPrivate {
			t.Fatalf("record %+v", record)
		}
		if a := h.asset("CLIP-001"); a.Visibility != domain.VisibilityPrivate {
			t.Fatalf("asset %s", a.Visibility)
		}
		if len(h.store.AuditRecords()) != before+1 {
			t.Fatal("the observed change must be audited")
		}
	})

	t.Run("a remote failure on a published video leaves it", func(t *testing.T) {
		h := newHarness(t)
		var failing bool
		h.addScripted("odd", func(s *scripted) {
			s.getStatus = func(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
				if failing {
					return connector.RemoteVideo{RemoteID: remoteID, Status: connector.RemoteFailed}, nil
				}
				return s.Publishing.GetStatus(ctx, remoteID)
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "odd", "visibility": "public", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		run[finalizeOutput](h, "publish.finalize", map[string]any{"publicationId": pub, "visibility": "public"})
		failing = true
		run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
		if got := h.publication(pub).Status; got != domain.PubPublished {
			t.Fatalf("status %s", got)
		}
	})

	t.Run("nothing at the destination yet", func(t *testing.T) {
		h := newHarness(t)
		pub := h.prepared().PublicationID
		out := run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
		if out.Publications[0].Changed || out.Publications[0].Status != "pending_approval" {
			t.Fatalf("refresh %+v", out)
		}
	})

	t.Run("a withdrawn record stays withdrawn", func(t *testing.T) {
		h := newHarness(t)
		pub := h.published()
		run[withdrawOutput](h, "publish.withdraw", map[string]any{"assetId": "CLIP-001", "all": true})
		out := run[refreshOutput](h, "publish.refresh", map[string]any{"publicationId": pub})
		if out.Publications[0].Changed {
			t.Fatalf("refresh %+v", out)
		}
	})

	t.Run("a destination error is reported", func(t *testing.T) {
		h := newHarness(t)
		h.addScripted("odd", func(s *scripted) {
			s.getStatus = func(context.Context, string) (connector.RemoteVideo, error) {
				return connector.RemoteVideo{}, errs.New(errs.CodeUnavailable, "down")
			}
		})
		pub := run[prepareOutput](h, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": "odd", "approvalId": "APR-001"}).PublicationID
		run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
		if _, err := h.runErr("publish.refresh", map[string]any{"publicationId": pub}); !node.IsTransient(err) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("needs a reference", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.runErr("publish.refresh", map[string]any{}); !permanent(err) || !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestPublicSync(t *testing.T) {
	t.Run("the file reaches the site without internal names", func(t *testing.T) {
		h := newHarness(t)
		h.published()
		run[syncOutput](h, "catalog.public.sync", map[string]any{})
		raw, err := os.ReadFile(h.publicDB())
		h.must(err)
		for _, forbidden := range []string{mockDest, "CLIP-001", "ORI-001", masterKey, "Maria", "diagnosis", string(tenantID)} {
			if strings.Contains(string(raw), forbidden) {
				t.Errorf("public.db contains %q", forbidden)
			}
		}
		if !strings.Contains(string(raw), mock.BaseURL+"/embed/") {
			t.Error("the connector's public links are missing")
		}
		info, err := os.Stat(h.publicDB())
		h.must(err)
		if info.Mode().Perm() != 0o644 {
			t.Errorf("mode %v", info.Mode().Perm())
		}
	})

	t.Run("no public library still exports and ships nothing", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Catalogs.PublicLibrary = nil
		h.published()
		out := run[syncOutput](h, "catalog.public.sync", map[string]any{})
		if out.Shipped || out.Destination != "" || out.Records != 1 {
			t.Fatalf("sync %+v", out)
		}
		if hasEvent(h.store.Events(), event.PublicLibrarySynced) != 0 {
			t.Fatal("nothing shipped, nothing to announce")
		}
	})

	t.Run("a bad driver writes nothing", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Catalogs.PublicLibrary = &config.CatalogInstance{Driver: "nothing-registered"}
		_, err := h.runErr("catalog.public.sync", map[string]any{})
		if !permanent(err) || !errs.IsCode(err, errs.CodeFailedPrecondition) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("an export failure leaves the old file and can retry", func(t *testing.T) {
		h := newHarness(t)
		h.published()
		run[syncOutput](h, "catalog.public.sync", map[string]any{})
		before, err := os.ReadFile(h.publicDB())
		h.must(err)
		h.exporter = exporterFunc(func(context.Context, publicexport.Request) (publicexport.Result, error) {
			return publicexport.Result{}, errs.New(errs.CodeUnavailable, "disk full")
		})
		if _, err := h.runErr("catalog.public.sync", map[string]any{}); !node.IsTransient(err) {
			t.Fatalf("got %v", err)
		}
		after, err := os.ReadFile(h.publicDB())
		h.must(err)
		if string(before) != string(after) {
			t.Fatal("old file changed")
		}
	})

	t.Run("not wired", func(t *testing.T) {
		reg := node.NewRegistry()
		Register(reg)
		h := newHarness(t)
		n, _ := reg.Lookup("catalog.public.sync", 1)
		_, err := n.Execute(h.ctx(), &env{h: h}, node.Input{Parameters: json.RawMessage(`{}`)})
		if !permanent(err) || !strings.Contains(err.Error(), "not wired") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("computed vectors when the store cannot return its own", func(t *testing.T) {
		h := newHarness(t)
		h.store.NoEmbeddingLister = true
		h.embedder = stubEmbedder{}
		h.published()
		out := run[syncOutput](h, "catalog.public.sync", map[string]any{})
		if out.Embeddings != "computed" {
			t.Fatalf("sync %+v", out)
		}
	})

	t.Run("no embedder means lexical", func(t *testing.T) {
		h := newHarness(t)
		h.store.NoEmbeddingLister = true
		h.published()
		out := run[syncOutput](h, "catalog.public.sync", map[string]any{})
		if out.Embeddings != "none" || out.Records != 1 {
			t.Fatalf("sync %+v", out)
		}
	})
}

type stubEmbedder struct{}

func (stubEmbedder) Info() connector.Info { return connector.Info{Driver: "stub"} }
func (stubEmbedder) Embed(_ context.Context, req connector.EmbedRequest) (connector.EmbedResult, error) {
	out := connector.EmbedResult{Model: "fake-hash-embed@1", Dimensions: 2}
	for range req.Texts {
		out.Vectors = append(out.Vectors, []float32{1, 0})
	}
	return out, nil
}

func TestMediaCMS(t *testing.T) {
	addCatalog := func(h *harness, driver string, conn connector.Catalog) {
		h.registry.RegisterCatalog(connector.Info{Driver: driver, Available: true}, func(context.Context, connector.CatalogParams) (connector.Catalog, error) {
			return conn, nil
		})
	}

	t.Run("no instance configured", func(t *testing.T) {
		h := newHarness(t)
		out := run[mediacmsOutput](h, "mediacms.register", map[string]any{"assetId": "CLIP-001"})
		if out.CatalogSync != "none" {
			t.Fatalf("output %+v", out)
		}
		if h.asset("CLIP-001").CatalogSync != "" {
			t.Fatal("nothing to record")
		}
	})

	t.Run("the skeleton records a failure and blocks nothing", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Catalogs.External = map[string]config.CatalogInstance{"cms": {Driver: mediacmsDriver}}
		before := h.asset("CLIP-001")
		out := run[mediacmsOutput](h, "mediacms.register", map[string]any{"assetId": "CLIP-001"})
		if out.CatalogSync != "failed" || out.ErrorCode != string(errs.CodeNotImplemented) {
			t.Fatalf("output %+v", out)
		}
		after := h.asset("CLIP-001")
		if after.CatalogSync != domain.CatalogSyncFailed {
			t.Fatalf("catalog_sync %s", after.CatalogSync)
		}
		if after.Status != before.Status || after.MasterAvailable != before.MasterAvailable || after.ReadinessSatisfied() != before.ReadinessSatisfied() {
			t.Fatal("a catalog failure must not touch readiness (R11)")
		}
	})

	t.Run("transient errors retry, then record a failure and complete", func(t *testing.T) {
		h := newHarness(t)
		fake := &fakeCatalog{err: errs.New(errs.CodeUnavailable, "down")}
		addCatalog(h, "failing", fake)
		h.cfg.Catalogs.External = map[string]config.CatalogInstance{"cms": {Driver: "failing"}}
		out := run[mediacmsOutput](h, "mediacms.register", map[string]any{"assetId": "CLIP-001", "name": "cms"})
		if out.CatalogSync != "failed" || fake.calls != catalogRegisterTries {
			t.Fatalf("output %+v, calls %d", out, fake.calls)
		}
		if h.asset("CLIP-001").CatalogSync != domain.CatalogSyncFailed {
			t.Fatal("must be recorded")
		}
	})

	t.Run("success", func(t *testing.T) {
		h := newHarness(t)
		fake := &fakeCatalog{ref: connector.CatalogRef{ExternalRef: "cms-1"}}
		addCatalog(h, "working", fake)
		h.cfg.Catalogs.External = map[string]config.CatalogInstance{"cms": {Driver: "working"}}
		out := run[mediacmsOutput](h, "mediacms.register", map[string]any{"assetId": "CLIP-001", "name": "cms"})
		if out.CatalogSync != "synced" || out.ExternalRef != "cms-1" || fake.calls != 1 {
			t.Fatalf("output %+v", out)
		}
		if h.asset("CLIP-001").CatalogSync != domain.CatalogSyncSynced {
			t.Fatal("must be recorded")
		}
	})

	t.Run("not wired still records and completes", func(t *testing.T) {
		reg := node.NewRegistry()
		Register(reg)
		h := newHarness(t)
		h.cfg.Catalogs.External = map[string]config.CatalogInstance{"cms": {Driver: mediacmsDriver}}
		n, _ := reg.Lookup("mediacms.register", 1)
		res, err := n.Execute(h.ctx(), &env{h: h}, node.Input{Parameters: json.RawMessage(`{"assetId":"CLIP-001"}`)})
		if err != nil {
			t.Fatal(err)
		}
		var out mediacmsOutput
		h.must(json.Unmarshal(res.Output, &out))
		if out.CatalogSync != "failed" {
			t.Fatalf("output %+v", out)
		}
	})

	t.Run("unknown asset is still an error", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.runErr("mediacms.register", map[string]any{"assetId": "CLIP-999"}); !permanent(err) {
			t.Fatalf("got %v", err)
		}
	})
}

type fakeCatalog struct {
	calls int
	err   error
	ref   connector.CatalogRef
}

func (f *fakeCatalog) Info() connector.Info { return connector.Info{Driver: "fake"} }
func (f *fakeCatalog) Register(context.Context, connector.CatalogItem) (connector.CatalogRef, error) {
	f.calls++
	return f.ref, f.err
}
func (f *fakeCatalog) Update(context.Context, connector.CatalogItem) (connector.CatalogRef, error) {
	return f.ref, f.err
}
func (f *fakeCatalog) Remove(context.Context, connector.CatalogRef) error { return f.err }

func TestTransitionTable(t *testing.T) {
	for _, s := range domain.PublicationStatuses {
		if !canMove(s, s) {
			t.Errorf("%s must allow repeating itself", s)
		}
	}
	if canMove(domain.PubWithdrawn, domain.PubPublished) {
		t.Error("a withdrawn record is history")
	}
	if canMove(domain.PubRejectedByDestination, domain.PubPublished) {
		t.Error("a rejected record cannot publish")
	}
	if canMove(domain.PubUploading, domain.PubPublished) {
		t.Error("published needs processing first")
	}
	p := domain.Publication{ID: "PUB-001", Status: domain.PubWithdrawn}
	if err := move(&p, domain.PubUploading, time.Now()); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
}

func TestRangeReaderReusesItsStreamAndRepositions(t *testing.T) {
	h := newHarness(t)
	st := h.storages[domain.RoleMaster]
	r := &rangeReader{ctx: h.ctx(), storage: st, key: masterKey, size: int64(len(masterBytes))}
	defer r.close()

	buf := make([]byte, 5)
	if n, err := r.ReadAt(buf, 0); n != 5 || err != nil || string(buf) != masterBytes[:5] {
		t.Fatalf("%d %v %q", n, err, buf)
	}
	if n, err := r.ReadAt(buf, 5); n != 5 || err != nil || string(buf) != masterBytes[5:10] {
		t.Fatalf("%d %v %q", n, err, buf)
	}
	if got := len(h.reads[domain.RoleMaster]); got != 1 {
		t.Fatalf("sequential reads opened %d streams", got)
	}
	if n, err := r.ReadAt(buf, 1); n != 5 || err != nil || string(buf) != masterBytes[1:6] {
		t.Fatalf("%d %v %q", n, err, buf)
	}
	if got := len(h.reads[domain.RoleMaster]); got != 2 {
		t.Fatalf("a backward read must reopen: %d", got)
	}
	big := make([]byte, 100)
	n, err := r.ReadAt(big, int64(len(masterBytes))-3)
	if n != 3 || err == nil {
		t.Fatalf("short read: %d %v", n, err)
	}
	if n, err := r.ReadAt(buf, int64(len(masterBytes))); n != 0 || err == nil {
		t.Fatalf("past the end: %d %v", n, err)
	}
}

func TestStorageVerifyUnsupportedStillUploads(t *testing.T) {
	h := newHarness(t)
	h.storages[domain.RoleMaster].verifyErr = errs.New(errs.CodeCapabilityUnsupported, "no checksums here")
	pub := h.prepared().PublicationID
	out := run[uploadOutput](h, "publish.upload", map[string]any{"publicationId": pub})
	if out.Status != "processing" {
		t.Fatalf("output %+v", out)
	}
	// The destination still checks the hash the record declares.
	if got := h.mockState().Videos[h.publication(pub).RemoteID].SHA256; got != sha(masterBytes) {
		t.Fatalf("hash %s", got)
	}
}
