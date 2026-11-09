package catalog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/persistence"
	"github.com/rafaelp/vidyom/internal/security/audit"
)

var ctx = context.Background()

func principal(role domain.Role) domain.Principal {
	return domain.Principal{Email: string(role) + "@example.com", Method: domain.AuthDevice, TenantID: "acme", Role: role}
}

func scope(role domain.Role) service.Scope {
	return service.Scope{Tenant: "acme", Principal: principal(role), RequestID: "req-1", TraceID: "trace-1"}
}

type fixture struct {
	stores  *persistence.Stores
	store   repo.TenantStore
	svc     *Service
	auditor *audit.Auditor
}

func setup(t *testing.T) *fixture {
	t.Helper()
	stores, err := persistence.Open(ctx, persistence.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stores.Close() })
	store, err := stores.Tenants.Create(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{stores, store, New(stores.Tenants, Options{}), audit.New(stores.Tenants, audit.Options{System: stores.System})}
}

func (f *fixture) asset(t *testing.T, kind domain.AssetKind) domain.Asset {
	t.Helper()
	friendly, err := f.store.IDs().Next(ctx, kind.IDPrefix())
	if err != nil {
		t.Fatal(err)
	}
	a := domain.Asset{ID: friendly, ULID: id.New(), Kind: kind, Title: "Hatha Yoga Class", Tags: []string{"yoga"}}
	if err := f.store.Assets().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Versions().Create(ctx, domain.AssetVersion{AssetID: friendly, Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Assets().Get(ctx, id.Ref(friendly))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// publish makes a public, published publication.
func (f *fixture) publish(t *testing.T, a domain.Asset) domain.Publication {
	t.Helper()
	friendly, err := f.store.IDs().Next(ctx, id.PrefixPublication)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p := domain.Publication{ID: friendly, ULID: id.New(), AssetID: a.ID, Destination: "mock-main", AssetVersion: "v1", Status: domain.PubPublished,
		Visibility: domain.VisibilityPublic, RemoteID: "r", URL: "https://media.example.internal/watch/r", PublishedAt: &now}
	if err := f.store.Publications().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fixture) auditActions(t *testing.T) []string {
	t.Helper()
	records, err := f.store.Audit().Scan(ctx, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.Action
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func TestGetAndList(t *testing.T) {
	f := setup(t)
	a := f.asset(t, domain.KindOriginal)
	c := f.asset(t, domain.KindClip)
	c.ParentAssetID = a.ID
	if err := f.store.Assets().Update(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Get(ctx, scope(domain.RoleViewer), id.Ref(a.ID))
	if err != nil || got.ID != a.ID {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := f.svc.Get(ctx, scope(domain.RoleViewer), "ORI-404"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := f.svc.Get(ctx, service.Scope{}, id.Ref(a.ID)); !errs.IsCode(err, errs.CodeTenantRequired) {
		t.Fatalf("no tenant: %v", err)
	}
	all, err := f.svc.List(ctx, scope(domain.RoleViewer), api.AssetQuery{})
	if err != nil || len(all.Items) != 2 {
		t.Fatalf("list = %+v, %v", all, err)
	}
	clips, err := f.svc.List(ctx, scope(domain.RoleViewer), api.AssetQuery{Kind: domain.KindClip})
	if err != nil || len(clips.Items) != 1 {
		t.Fatalf("clips = %+v, %v", clips, err)
	}
	children, err := f.svc.List(ctx, scope(domain.RoleViewer), api.AssetQuery{SourceID: string(a.ID)})
	if err != nil || len(children.Items) != 1 || children.Items[0].ID != c.ID {
		t.Fatalf("children = %+v, %v", children, err)
	}
	empty, err := f.svc.List(ctx, scope(domain.RoleViewer), api.AssetQuery{Collection: "none"})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
	paged, err := f.svc.List(ctx, scope(domain.RoleViewer), api.AssetQuery{PageQuery: api.PageQuery{Limit: 1}})
	if err != nil || len(paged.Items) != 1 || paged.NextCursor == "" {
		t.Fatalf("paged = %+v, %v", paged, err)
	}

	// D17: a deleted asset is gone from the API.
	if err := f.store.Assets().SoftDelete(ctx, id.Ref(c.ID), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(ctx, scope(domain.RoleViewer), id.Ref(c.ID)); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	all, _ = f.svc.List(ctx, scope(domain.RoleViewer), api.AssetQuery{})
	if len(all.Items) != 1 {
		t.Fatalf("list after delete = %+v", all)
	}
}

// D35: editing metadata never creates a media version, and is recorded in the history.
func TestUpdateMetadata(t *testing.T) {
	f := setup(t)
	a := f.asset(t, domain.KindOriginal)
	got, err := f.svc.Update(ctx, scope(domain.RoleEditor), id.Ref(a.ID), api.UpdateAssetRequest{Title: ptr("New title"), Tags: ptr([]string{"yoga", "breathing"}), Category: ptr("yoga_class"), Description: ptr("d")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "New title" || len(got.Tags) != 2 || got.Category != "yoga_class" || got.Description != "d" {
		t.Fatalf("asset = %+v", got)
	}
	versions, err := f.svc.Versions(ctx, scope(domain.RoleViewer), id.Ref(a.ID))
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions = %+v, %v", versions, err)
	}
	history, _, err := f.store.Assets().History(ctx, id.Ref(a.ID), repo.Page{})
	if err != nil || len(history) != 1 || history[0].Action != "metadata.update" || history[0].Actor != "editor@example.com" || !strings.Contains(string(history[0].Changes), "New title") {
		t.Fatalf("history = %+v, %v", history, err)
	}
	// The audit names fields and permission, not the values.
	records, err := f.store.Audit().Scan(ctx, 0, 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	r := records[0]
	if r.Permission != domain.PermAssetWrite || r.Resource != string(a.ID) || r.RequestID != "req-1" || !strings.Contains(string(r.Details), "title") || strings.Contains(string(r.Details), "New title") {
		t.Fatalf("record = %+v", r)
	}
	// No change, no history.
	if _, err := f.svc.Update(ctx, scope(domain.RoleEditor), id.Ref(a.ID), api.UpdateAssetRequest{Title: ptr("New title")}); err != nil {
		t.Fatal(err)
	}
	history, _, _ = f.store.Assets().History(ctx, id.Ref(a.ID), repo.Page{})
	if len(history) != 1 {
		t.Fatalf("history = %+v", history)
	}
	if _, err := f.svc.Update(ctx, scope(domain.RoleEditor), id.Ref(a.ID), api.UpdateAssetRequest{Title: ptr("")}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("empty title: %v", err)
	}
	if _, err := f.svc.Update(ctx, scope(domain.RoleEditor), "ORI-404", api.UpdateAssetRequest{Title: ptr("x")}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

// D7: changing public metadata of a public video needs a publisher.
func TestPublicMetadataNeedsPublisher(t *testing.T) {
	f := setup(t)
	a := f.asset(t, domain.KindClip)
	p := f.publish(t, a)
	_, err := f.svc.Update(ctx, scope(domain.RoleEditor), id.Ref(a.ID), api.UpdateAssetRequest{Title: ptr("Changed")})
	if !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("editor: %v", err)
	}
	unchanged, _ := f.svc.Get(ctx, scope(domain.RoleEditor), id.Ref(a.ID))
	if unchanged.Title == "Changed" {
		t.Fatal("a refused edit must not apply")
	}
	if events, _ := f.store.Outbox().Stream(ctx, 0, 10); len(events) != 0 {
		t.Fatalf("a refused edit must not emit: %+v", events)
	}
	// An editor can still move it between collections: that is not public metadata.
	moved, err := f.svc.Update(ctx, scope(domain.RoleEditor), id.Ref(a.ID), api.UpdateAssetRequest{Collection: ptr("yoga")})
	if err != nil || moved.Collection != "yoga" {
		t.Fatalf("collection = %+v, %v", moved, err)
	}
	if events, _ := f.store.Outbox().Stream(ctx, 0, 10); len(events) != 0 {
		t.Fatalf("a collection move must not refresh the library: %+v", events)
	}

	// A publisher edits; the permission that authorized it is recorded and the public
	// library hears about it in the same transaction.
	got, err := f.svc.Update(ctx, scope(domain.RolePublisher), id.Ref(a.ID), api.UpdateAssetRequest{Title: ptr("Changed")})
	if err != nil || got.Title != "Changed" {
		t.Fatalf("publisher = %+v, %v", got, err)
	}
	records, _, err := f.store.Audit().List(ctx, repo.AuditFilter{}, repo.Page{Limit: 1})
	if err != nil || records[0].Permission != domain.PermApprovalDecidePublication {
		t.Fatalf("records = %+v, %v", records, err)
	}
	events, err := f.store.Outbox().Stream(ctx, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	e := events[0].Event
	if e.Type != event.PublicationUpdated || e.AssetID != string(a.ID) || e.TenantID != "acme" || e.TraceID != "trace-1" || strings.Contains(string(e.Payload), "Changed") {
		t.Fatalf("event = %+v", e)
	}

	// Before publication, or after withdrawal, an editor edits freely.
	draft := f.asset(t, domain.KindClip)
	if _, err := f.svc.Update(ctx, scope(domain.RoleEditor), id.Ref(draft.ID), api.UpdateAssetRequest{Title: ptr("Draft")}); err != nil {
		t.Fatal(err)
	}
	withdrawnAt := time.Now()
	p.Status, p.Visibility, p.WithdrawnAt = domain.PubWithdrawn, domain.VisibilityPrivate, &withdrawnAt
	if err := f.store.Publications().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Update(ctx, scope(domain.RoleEditor), id.Ref(a.ID), api.UpdateAssetRequest{Title: ptr("After withdrawal")}); err != nil {
		t.Fatalf("after withdrawal: %v", err)
	}
}

func TestLineage(t *testing.T) {
	f := setup(t)
	ori := f.asset(t, domain.KindOriginal)
	make := func() domain.Asset {
		friendly, err := f.store.IDs().Next(ctx, id.PrefixClip)
		if err != nil {
			t.Fatal(err)
		}
		c := domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindClip, Title: "c", Lineage: &domain.Lineage{
			Method: domain.LineageComputed, SourceAssetID: ori.ID, RootAssetID: ori.ID, SelectionApprovalID: "APR-001",
			Segments: []domain.Segment{{SourceAssetID: ori.ID, SourceVersion: "v1", StartSeconds: 1.5, EndSeconds: 2.5}},
			Chain:    []id.Friendly{ori.ID},
		}}
		if err := f.store.Assets().Create(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	one := make()
	two := make()
	gone := make()
	if err := f.store.Assets().SoftDelete(ctx, id.Ref(gone.ID), time.Now()); err != nil {
		t.Fatal(err)
	}
	parent, err := f.svc.Lineage(ctx, scope(domain.RoleViewer), id.Ref(ori.ID))
	if err != nil {
		t.Fatal(err)
	}
	if parent.AssetID != ori.ID || parent.Lineage != nil || len(parent.Children) != 2 || parent.Children[0] != one.ID || parent.Children[1] != two.ID {
		t.Fatalf("parent = %+v", parent)
	}
	child, err := f.svc.Lineage(ctx, scope(domain.RoleViewer), id.Ref(one.ID))
	if err != nil {
		t.Fatal(err)
	}
	if child.Lineage == nil || child.Lineage.SelectionApprovalID != "APR-001" || child.Lineage.Segments[0].StartSeconds != 1.5 || len(child.Children) != 0 {
		t.Fatalf("child = %+v", child)
	}
	if _, err := f.svc.Lineage(ctx, scope(domain.RoleViewer), "ORI-404"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

// D8: local_only by default; an admin's decision writes who and when, in the database
// and the audit.
func TestConsentRecord(t *testing.T) {
	f := setup(t)
	a := f.asset(t, domain.KindOriginal)
	if a.AIProcessing != domain.AILocalOnly || a.AIConsent != nil {
		t.Fatalf("default = %+v", a)
	}
	c, err := f.svc.Consent(ctx, scope(domain.RoleViewer), id.Ref(a.ID))
	if err != nil || c.AIProcessing != domain.AILocalOnly || c.Consent != nil {
		t.Fatalf("consent = %+v, %v", c, err)
	}
	if _, err := f.svc.SetAIProcessing(ctx, scope(domain.RoleAdmin), id.Ref(a.ID), api.SetAIProcessingRequest{AIProcessing: "open"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad value: %v", err)
	}
	got, err := f.svc.SetAIProcessing(ctx, scope(domain.RoleAdmin), id.Ref(a.ID), api.SetAIProcessingRequest{AIProcessing: domain.AIExternalAllowed})
	if err != nil {
		t.Fatal(err)
	}
	if got.AIProcessing != domain.AIExternalAllowed || got.AIConsent == nil || got.AIConsent.AuthorizedBy != "admin@example.com" || got.AIConsent.Scope != "asset" || got.AIConsent.AuthorizedAt.IsZero() {
		t.Fatalf("asset = %+v", got)
	}
	c, err = f.svc.Consent(ctx, scope(domain.RoleViewer), id.Ref(a.ID))
	if err != nil || c.Consent == nil || c.Consent.AuthorizedBy != "admin@example.com" {
		t.Fatalf("consent = %+v, %v", c, err)
	}
	// It survives an unrelated edit, even a stale one.
	stale := got
	stale.AIProcessing = domain.AILocalOnly
	stale.AIConsent = nil
	stale.Title = "Stale write"
	if err := f.store.Assets().Update(ctx, stale); err != nil {
		t.Fatal(err)
	}
	after, _ := f.svc.Get(ctx, scope(domain.RoleViewer), id.Ref(a.ID))
	if after.AIProcessing != domain.AIExternalAllowed || after.Title != "Stale write" {
		t.Fatalf("after = %+v", after)
	}
	back, err := f.svc.SetAIProcessing(ctx, scope(domain.RoleAdmin), id.Ref(a.ID), api.SetAIProcessingRequest{AIProcessing: domain.AILocalOnly})
	if err != nil || back.AIProcessing != domain.AILocalOnly || back.AIConsent != nil {
		t.Fatalf("back = %+v, %v", back, err)
	}
	// History and audit keep both decisions.
	history, _, err := f.store.Assets().History(ctx, id.Ref(a.ID), repo.Page{})
	if err != nil || len(history) != 2 || !strings.Contains(string(history[0].Changes), "local_only") {
		t.Fatalf("history = %+v, %v", history, err)
	}
	records, _, err := f.store.Audit().List(ctx, repo.AuditFilter{Action: "ai_processing.set"}, repo.Page{})
	if err != nil || len(records) != 2 || records[0].Permission != domain.PermAssetAIConsent || records[1].Actor != "admin@example.com" {
		t.Fatalf("records = %+v, %v", records, err)
	}
	if _, err := f.svc.SetAIProcessing(ctx, scope(domain.RoleAdmin), "ORI-404", api.SetAIProcessingRequest{AIProcessing: domain.AILocalOnly}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestCollectionPolicy(t *testing.T) {
	f := setup(t)
	create := func(collection string) domain.Asset {
		friendly, err := f.store.IDs().Next(ctx, id.PrefixOriginal)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.Assets().Create(ctx, domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindOriginal, Title: "x", Collection: collection}); err != nil {
			t.Fatal(err)
		}
		a, _ := f.store.Assets().Get(ctx, id.Ref(friendly))
		return a
	}
	member := create("yoga")
	explicit := create("yoga")
	outside := create("talks")
	if _, err := f.svc.SetAIProcessing(ctx, scope(domain.RoleAdmin), id.Ref(explicit.ID), api.SetAIProcessingRequest{AIProcessing: domain.AILocalOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetCollectionAIProcessing(ctx, scope(domain.RoleAdmin), "", api.SetAIProcessingRequest{AIProcessing: domain.AILocalOnly}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("no name: %v", err)
	}
	if _, err := f.svc.SetCollectionAIProcessing(ctx, scope(domain.RoleAdmin), "yoga", api.SetAIProcessingRequest{AIProcessing: "open"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("bad value: %v", err)
	}
	c, err := f.svc.SetCollectionAIProcessing(ctx, scope(domain.RoleAdmin), "yoga", api.SetAIProcessingRequest{AIProcessing: domain.AIExternalAllowed})
	if err != nil {
		t.Fatal(err)
	}
	if c.AIProcessing != domain.AIExternalAllowed || c.AIConsent == nil || c.AIConsent.Scope != "collection" || c.AIConsent.AuthorizedBy != "admin@example.com" {
		t.Fatalf("collection = %+v", c)
	}
	got, _ := f.svc.Get(ctx, scope(domain.RoleViewer), id.Ref(member.ID))
	if got.AIProcessing != domain.AIExternalAllowed || got.AIConsent == nil || got.AIConsent.Scope != "collection" {
		t.Fatalf("member = %+v", got)
	}
	got, _ = f.svc.Get(ctx, scope(domain.RoleViewer), id.Ref(explicit.ID))
	if got.AIProcessing != domain.AILocalOnly {
		t.Fatalf("an asset-level decision must win: %+v", got)
	}
	got, _ = f.svc.Get(ctx, scope(domain.RoleViewer), id.Ref(outside.ID))
	if got.AIProcessing != domain.AILocalOnly {
		t.Fatalf("outside = %+v", got)
	}
	// New assets inherit.
	later := create("yoga")
	if later.AIProcessing != domain.AIExternalAllowed {
		t.Fatalf("later = %+v", later)
	}
	list, err := f.svc.Collections(ctx, scope(domain.RoleViewer))
	if err != nil || len(list) != 2 {
		t.Fatalf("collections = %+v, %v", list, err)
	}
	// Turning it off again applies at once (D8).
	if _, err := f.svc.SetCollectionAIProcessing(ctx, scope(domain.RoleAdmin), "yoga", api.SetAIProcessingRequest{AIProcessing: domain.AILocalOnly}); err != nil {
		t.Fatal(err)
	}
	got, _ = f.svc.Get(ctx, scope(domain.RoleViewer), id.Ref(member.ID))
	if got.AIProcessing != domain.AILocalOnly || got.AIConsent != nil {
		t.Fatalf("member = %+v", got)
	}
	records, _, err := f.store.Audit().List(ctx, repo.AuditFilter{Action: "ai_processing.set_collection"}, repo.Page{})
	if err != nil || len(records) != 2 || records[0].Resource != "collection:yoga" {
		t.Fatalf("records = %+v, %v", records, err)
	}
	// A new collection can be decided before its first asset.
	fresh, err := f.svc.SetCollectionAIProcessing(ctx, scope(domain.RoleAdmin), "fresh", api.SetAIProcessingRequest{AIProcessing: domain.AIExternalAllowed})
	if err != nil || fresh.Name != "fresh" {
		t.Fatalf("fresh = %+v, %v", fresh, err)
	}
}

func saveTranscript(t *testing.T, f *fixture, a domain.Asset, version string) {
	t.Helper()
	err := f.store.Transcripts().Save(ctx, domain.Transcript{
		AssetID: a.ID, Version: version, Language: "pt", TimestampBasis: "working_master", Engine: "fake",
		Segments: []domain.TranscriptSegment{
			{Start: 0, End: 10, Text: "public words", Words: []domain.Word{{Start: 0, End: 1, Text: "public"}}},
			{Start: 10, End: 20, Text: "the name of a minor", Words: []domain.Word{{Start: 10, End: 11, Text: "the"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// D30 and I118.
func TestSensitiveSegments(t *testing.T) {
	f := setup(t)
	a := f.asset(t, domain.KindOriginal)
	saveTranscript(t, f, a, "v1")

	empty, err := f.svc.ListSensitive(ctx, scope(domain.RoleViewer), id.Ref(a.ID), "")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
	req := api.SensitiveSegmentRequest{StartSeconds: 12, EndSeconds: 15, Reason: "minor"}
	if err := f.svc.AddSensitive(ctx, scope(domain.RoleEditor), id.Ref(a.ID), req); err != nil {
		t.Fatal(err)
	}
	segments, err := f.svc.ListSensitive(ctx, scope(domain.RoleViewer), id.Ref(a.ID), "v1")
	if err != nil || len(segments) != 1 || segments[0].MarkedBy != "editor@example.com" || segments[0].Version != "v1" || segments[0].Reason != "minor" {
		t.Fatalf("segments = %+v, %v", segments, err)
	}
	if err := f.svc.AddSensitive(ctx, scope(domain.RoleEditor), id.Ref(a.ID), api.SensitiveSegmentRequest{StartSeconds: 5, EndSeconds: 5}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("empty interval: %v", err)
	}
	if err := f.svc.AddSensitive(ctx, scope(domain.RoleEditor), "ORI-404", req); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}

	// A viewer reads a redacted transcript, an editor the whole text.
	viewer, err := f.svc.Transcript(ctx, scope(domain.RoleViewer), id.Ref(a.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if viewer.Segments[1].Text != domain.RedactedText || viewer.Segments[1].Words != nil || viewer.Segments[0].Text != "public words" {
		t.Fatalf("viewer = %+v", viewer.Segments)
	}
	blob, _ := json.Marshal(viewer)
	if strings.Contains(string(blob), "minor") {
		t.Fatalf("viewer sees sensitive text: %s", blob)
	}
	editor, err := f.svc.Transcript(ctx, scope(domain.RoleEditor), id.Ref(a.ID), "v1")
	if err != nil || editor.Segments[1].Text != "the name of a minor" {
		t.Fatalf("editor = %+v, %v", editor.Segments, err)
	}
	// A grant of asset.sensitive gives one viewer the whole text (D48).
	granted := scope(domain.RoleViewer)
	granted.Principal.Grants = []domain.Permission{domain.PermAssetSensitive}
	whole, err := f.svc.Transcript(ctx, granted, id.Ref(a.ID), "")
	if err != nil || whole.Segments[1].Text != "the name of a minor" {
		t.Fatalf("granted = %+v, %v", whole.Segments, err)
	}
	if _, err := f.svc.Transcript(ctx, scope(domain.RoleViewer), id.Ref(a.ID), "v9"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("missing version: %v", err)
	}

	// History and audit hold the interval, never the transcript text or the reason.
	records, err := f.store.Audit().Scan(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if strings.Contains(string(r.Details), "minor") || strings.Contains(string(r.Details), "words") {
			t.Fatalf("the audit holds content: %s", r.Details)
		}
	}
	if len(records) != 1 || records[0].Action != "sensitive.mark" || records[0].Permission != domain.PermAssetSensitive {
		t.Fatalf("records = %+v", records)
	}

	if err := f.svc.RemoveSensitive(ctx, scope(domain.RoleEditor), id.Ref(a.ID), api.SensitiveSegmentRequest{StartSeconds: 12, EndSeconds: 99}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("no exact match: %v", err)
	}
	if err := f.svc.RemoveSensitive(ctx, scope(domain.RoleEditor), id.Ref(a.ID), api.SensitiveSegmentRequest{StartSeconds: 12, EndSeconds: 15}); err != nil {
		t.Fatal(err)
	}
	viewer, _ = f.svc.Transcript(ctx, scope(domain.RoleViewer), id.Ref(a.ID), "")
	if viewer.Segments[1].Text != "the name of a minor" {
		t.Fatalf("after unmark = %+v", viewer.Segments)
	}
	history, _, err := f.store.Assets().History(ctx, id.Ref(a.ID), repo.Page{})
	if err != nil || len(history) != 2 || history[0].Action != "sensitive.unmark" {
		t.Fatalf("history = %+v, %v", history, err)
	}
	// Marks apply per version.
	v2 := a
	if err := f.store.Versions().Create(ctx, domain.AssetVersion{AssetID: a.ID, Version: "v2"}); err != nil {
		t.Fatal(err)
	}
	v2.CurrentVersion = "v2"
	if err := f.store.Assets().Update(ctx, v2); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddSensitive(ctx, scope(domain.RoleEditor), id.Ref(a.ID), req); err != nil {
		t.Fatal(err)
	}
	onV2, _ := f.svc.ListSensitive(ctx, scope(domain.RoleViewer), id.Ref(a.ID), "")
	onV1, _ := f.svc.ListSensitive(ctx, scope(domain.RoleViewer), id.Ref(a.ID), "v1")
	if len(onV2) != 1 || onV2[0].Version != "v2" || len(onV1) != 0 {
		t.Fatalf("v2 = %+v, v1 = %+v", onV2, onV1)
	}
}

// D30: marking a segment on a public video refreshes public.db, in the same transaction.
func TestSensitiveMarkOnPublicVideoRefreshesTheLibrary(t *testing.T) {
	f := setup(t)
	a := f.asset(t, domain.KindClip)
	saveTranscript(t, f, a, "v1")
	req := api.SensitiveSegmentRequest{StartSeconds: 12, EndSeconds: 15}
	if err := f.svc.AddSensitive(ctx, scope(domain.RoleEditor), id.Ref(a.ID), req); err != nil {
		t.Fatal(err)
	}
	if events, _ := f.store.Outbox().Stream(ctx, 0, 10); len(events) != 0 {
		t.Fatalf("a private asset has nothing to refresh: %+v", events)
	}
	f.publish(t, a)
	req.StartSeconds = 13
	if err := f.svc.AddSensitive(ctx, scope(domain.RoleEditor), id.Ref(a.ID), req); err != nil {
		t.Fatal(err)
	}
	events, err := f.store.Outbox().Stream(ctx, 0, 10)
	if err != nil || len(events) != 1 || events[0].Event.Type != event.PublicationUpdated || !strings.Contains(string(events[0].Event.Payload), "sensitive_segments_changed") {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if err := f.svc.RemoveSensitive(ctx, scope(domain.RoleEditor), id.Ref(a.ID), req); err != nil {
		t.Fatal(err)
	}
	events, _ = f.store.Outbox().Stream(ctx, 0, 10)
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
}

// clipOf makes a derivative of a source, cut from the given interval.
func (f *fixture) clipOf(t *testing.T, source domain.Asset, start, end float64) domain.Asset {
	t.Helper()
	friendly, err := f.store.IDs().Next(ctx, id.PrefixClip)
	if err != nil {
		t.Fatal(err)
	}
	c := domain.Asset{ID: friendly, ULID: id.New(), Kind: domain.KindClip, Title: "c", Lineage: &domain.Lineage{
		Method: domain.LineageComputed, SourceAssetID: source.ID, RootAssetID: source.ID,
		Segments: []domain.Segment{{SourceAssetID: source.ID, SourceVersion: "v1", StartSeconds: start, EndSeconds: end}},
	}}
	if err := f.store.Assets().Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Versions().Create(ctx, domain.AssetVersion{AssetID: friendly, Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Assets().Get(ctx, id.Ref(friendly))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// D30: a mark on a source reaches the public clips cut from it, however deep, in the same
// transaction as the mark.
func TestSourceMarkRefreshesPublicDerivatives(t *testing.T) {
	f := setup(t)
	ori := f.asset(t, domain.KindOriginal)
	saveTranscript(t, f, ori, "v1")
	clip := f.clipOf(t, ori, 0, 30)
	grandchild := f.clipOf(t, clip, 0, 10)
	f.clipOf(t, ori, 30, 60) // derived too, but with nothing public to refresh
	f.publish(t, clip)
	f.publish(t, grandchild)
	req := api.SensitiveSegmentRequest{StartSeconds: 12, EndSeconds: 15}
	if err := f.svc.AddSensitive(ctx, scope(domain.RoleEditor), id.Ref(ori.ID), req); err != nil {
		t.Fatal(err)
	}
	events, err := f.store.Outbox().Stream(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range events {
		if e.Event.Type != event.PublicationUpdated {
			t.Fatalf("event = %+v", e.Event)
		}
		got[e.Event.AssetID] = true
	}
	if len(events) != 2 || !got[string(clip.ID)] || !got[string(grandchild.ID)] {
		t.Fatalf("events = %+v", events)
	}
	if err := f.svc.RemoveSensitive(ctx, scope(domain.RoleEditor), id.Ref(ori.ID), req); err != nil {
		t.Fatal(err)
	}
	if events, _ = f.store.Outbox().Stream(ctx, 0, 10); len(events) != 4 {
		t.Fatalf("unmarking must refresh too: %+v", events)
	}
}

// I118: a viewer of a clip does not read what its source marks, even when marked later.
func TestTranscriptRedactsInheritedMarks(t *testing.T) {
	f := setup(t)
	ori := f.asset(t, domain.KindOriginal)
	clip := f.clipOf(t, ori, 100, 120) // the clip's second 5 is the source's second 105
	grandchild := f.clipOf(t, clip, 0, 20)
	transcript := func(a domain.Asset) {
		err := f.store.Transcripts().Save(ctx, domain.Transcript{AssetID: a.ID, Version: "v1", Engine: "fake", Segments: []domain.TranscriptSegment{
			{Start: 0, End: 4, Text: "fine"}, {Start: 4, End: 8, Text: "the name of a minor"}, {Start: 8, End: 20, Text: "fine again"},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	transcript(clip)
	transcript(grandchild)
	if err := f.svc.AddSensitive(ctx, scope(domain.RoleEditor), id.Ref(ori.ID), api.SensitiveSegmentRequest{StartSeconds: 105, EndSeconds: 106}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []domain.Asset{clip, grandchild} {
		got, err := f.svc.Transcript(ctx, scope(domain.RoleViewer), id.Ref(a.ID), "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Segments[1].Text != domain.RedactedText || got.Segments[0].Text != "fine" || got.Segments[2].Text != "fine again" {
			t.Fatalf("%s = %+v", a.ID, got.Segments)
		}
		whole, err := f.svc.Transcript(ctx, scope(domain.RoleEditor), id.Ref(a.ID), "")
		if err != nil || whole.Segments[1].Text != "the name of a minor" {
			t.Fatalf("%s for an editor = %+v, %v", a.ID, whole.Segments, err)
		}
	}
	// A mark outside the cut changes nothing.
	far := f.clipOf(t, ori, 0, 20)
	transcript(far)
	got, err := f.svc.Transcript(ctx, scope(domain.RoleViewer), id.Ref(far.ID), "")
	if err != nil || got.Segments[1].Text != "the name of a minor" {
		t.Fatalf("far = %+v, %v", got.Segments, err)
	}
	// An older version cannot place a source's mark, so a viewer gets nothing.
	if err := f.store.Transcripts().Save(ctx, domain.Transcript{AssetID: clip.ID, Version: "v0", Engine: "fake", Segments: []domain.TranscriptSegment{{Start: 0, End: 1, Text: "old"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Transcript(ctx, scope(domain.RoleViewer), id.Ref(clip.ID), "v0"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("older version: %v", err)
	}
	if old, err := f.svc.Transcript(ctx, scope(domain.RoleEditor), id.Ref(clip.ID), "v0"); err != nil || old.Segments[0].Text != "old" {
		t.Fatalf("older version for an editor = %+v, %v", old, err)
	}
}

type fixedSearch struct{ hits []domain.SearchHit }

func (s fixedSearch) Search(context.Context, service.Scope, api.SearchQuery) (api.SearchResponse, error) {
	return api.SearchResponse{Items: s.hits, Mode: "hybrid", EmbeddingModel: "m@1"}, nil
}

// I118: viewers cannot read through search what a transcript read redacts.
func TestSearchGuard(t *testing.T) {
	f := setup(t)
	ori := f.asset(t, domain.KindOriginal)
	clip := f.clipOf(t, ori, 100, 120)
	if err := f.store.Transcripts().MarkSensitive(ctx, domain.SensitiveSegment{AssetID: ori.ID, Version: "v1", StartSeconds: 10, EndSeconds: 20}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Transcripts().MarkSensitive(ctx, domain.SensitiveSegment{AssetID: ori.ID, Version: "v1", StartSeconds: 105, EndSeconds: 106}); err != nil {
		t.Fatal(err)
	}
	hits := []domain.SearchHit{
		{AssetID: ori.ID, Version: "v1", StartSeconds: 0, EndSeconds: 5, Text: "fine"},
		{AssetID: ori.ID, Version: "v1", StartSeconds: 15, EndSeconds: 30, Text: "marked"},
		{AssetID: clip.ID, Version: "v1", StartSeconds: 0, EndSeconds: 3, Text: "fine in the clip"},
		{AssetID: clip.ID, Version: "v1", StartSeconds: 4, EndSeconds: 8, Text: "inherited mark"},
	}
	guard := NewSearchGuard(fixedSearch{hits}, f.stores.Tenants)

	viewer, err := guard.Search(ctx, scope(domain.RoleViewer), api.SearchQuery{Q: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(viewer.Items) != 2 || viewer.Items[0].Text != "fine" || viewer.Items[1].Text != "fine in the clip" || viewer.Mode != "hybrid" {
		t.Fatalf("viewer = %+v", viewer)
	}
	editor, err := guard.Search(ctx, scope(domain.RoleEditor), api.SearchQuery{Q: "x"})
	if err != nil || len(editor.Items) != 4 {
		t.Fatalf("editor = %+v, %v", editor, err)
	}
	if _, err := guard.Search(ctx, service.Scope{Tenant: "unknown"}, api.SearchQuery{Q: "x"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
}

// Every mutation here is audited, and the chain verifies afterwards (§12.1, D23).
func TestEveryMutationIsAudited(t *testing.T) {
	f := setup(t)
	a := f.asset(t, domain.KindClip)
	saveTranscript(t, f, a, "v1")
	admin, editor := scope(domain.RoleAdmin), scope(domain.RoleEditor)
	if _, err := f.svc.Update(ctx, editor, id.Ref(a.ID), api.UpdateAssetRequest{Title: ptr("x")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetAIProcessing(ctx, admin, id.Ref(a.ID), api.SetAIProcessingRequest{AIProcessing: domain.AIExternalAllowed}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetCollectionAIProcessing(ctx, admin, "c", api.SetAIProcessingRequest{AIProcessing: domain.AIExternalAllowed}); err != nil {
		t.Fatal(err)
	}
	req := api.SensitiveSegmentRequest{StartSeconds: 1, EndSeconds: 2}
	if err := f.svc.AddSensitive(ctx, editor, id.Ref(a.ID), req); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RemoveSensitive(ctx, editor, id.Ref(a.ID), req); err != nil {
		t.Fatal(err)
	}
	got := f.auditActions(t)
	want := []string{"metadata.update", "ai_processing.set", "ai_processing.set_collection", "sensitive.mark", "sensitive.unmark"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %v", got)
	}
	v, err := f.auditor.Verify(ctx, "acme")
	if err != nil || !v.OK || v.Records != 5 {
		t.Fatalf("verify = %+v, %v", v, err)
	}
}
