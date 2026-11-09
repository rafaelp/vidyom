package domain

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
)

func readyAsset(kind AssetKind, status AssetStatus) Asset {
	return Asset{
		Kind: kind, Status: status, MasterAvailable: true, BackupVerified: true,
		ProxyAvailable: true, TimelineValidated: true,
		Transcript: &TranscriptInfo{Indexed: true, EmbeddingModel: "bge-m3@1"},
	}
}

func TestReadinessNeedsEveryCondition(t *testing.T) {
	if !readyAsset(KindOriginal, AssetIngesting).ReadinessSatisfied() {
		t.Fatal("full asset should be ready")
	}
	mutations := map[string]func(*Asset){
		"backup":   func(a *Asset) { a.BackupVerified = false },
		"master":   func(a *Asset) { a.MasterAvailable = false },
		"proxy":    func(a *Asset) { a.ProxyAvailable = false },
		"timeline": func(a *Asset) { a.TimelineValidated = false },
		"no text":  func(a *Asset) { a.Transcript = nil },
		"no index": func(a *Asset) { a.Transcript = &TranscriptInfo{Indexed: false, EmbeddingModel: "m"} },
		"no vecs":  func(a *Asset) { a.Transcript = &TranscriptInfo{Indexed: true} },
	}
	for name, mutate := range mutations {
		a := readyAsset(KindOriginal, AssetIngesting)
		mutate(&a)
		if a.ReadinessSatisfied() {
			t.Errorf("%s: should not be ready", name)
		}
	}
	// External catalog failure must not block readiness (R11).
	a := readyAsset(KindOriginal, AssetIngesting)
	a.CatalogSync = CatalogSyncFailed
	if !a.ReadinessSatisfied() {
		t.Error("catalog_sync=failed must not affect readiness")
	}
}

func TestClipEligible(t *testing.T) {
	cases := []struct {
		asset Asset
		want  bool
	}{
		{readyAsset(KindOriginal, AssetReadyForClipping), true},
		{readyAsset(KindOriginal, AssetIngesting), false},
		{readyAsset(KindOriginal, AssetArchivedOnly), false},
		{readyAsset(KindOriginal, AssetPublished), false},
		{readyAsset(KindClip, AssetReadyForClipping), true},
		{readyAsset(KindClip, AssetEditorialReview), true},
		{readyAsset(KindClip, AssetPublished), true},
		{readyAsset(KindClip, AssetWithdrawn), false},
		{readyAsset(KindClip, AssetRejected), false},
	}
	for _, c := range cases {
		if got := c.asset.ClipEligible(); got != c.want {
			t.Errorf("%s/%s: ClipEligible = %v, want %v", c.asset.Kind, c.asset.Status, got, c.want)
		}
	}
	noMaster := readyAsset(KindOriginal, AssetReadyForClipping)
	noMaster.MasterAvailable = false
	if noMaster.ClipEligible() {
		t.Error("a missing master must never be eligible (§2.5)")
	}
}

func TestAssetTransitions(t *testing.T) {
	allowed := [][2]AssetStatus{
		{AssetRegistered, AssetIngesting},
		{AssetIngesting, AssetReadyForClipping},
		{AssetReadyForClipping, AssetEditorialReview},
		{AssetEditorialReview, AssetApproved},
		{AssetApproved, AssetPublished},
		{AssetPublished, AssetWithdrawn},
		{AssetIngesting, AssetIngesting},
	}
	for _, p := range allowed {
		if !p[0].CanTransition(p[1]) {
			t.Errorf("%s -> %s should be allowed", p[0], p[1])
		}
	}
	denied := [][2]AssetStatus{
		{AssetRegistered, AssetReadyForClipping},
		{AssetIngestFailed, AssetReadyForClipping},
		{AssetEditorialReview, AssetPublished},
		{AssetRejected, AssetPublished},
	}
	for _, p := range denied {
		if p[0].CanTransition(p[1]) {
			t.Errorf("%s -> %s should be denied", p[0], p[1])
		}
	}
	for _, s := range AssetStatuses {
		if _, ok := assetTransitions[s]; !ok {
			t.Errorf("no transitions declared for %s", s)
		}
	}
}

func TestJobTransitions(t *testing.T) {
	for _, s := range JobStatuses {
		if _, ok := jobTransitions[s]; !ok {
			t.Errorf("no transitions declared for %s", s)
		}
	}
	if !JobRunning.CanTransition(JobNeedsAttention) {
		t.Error("running must reach needs_attention (D11)")
	}
	if JobNeedsAttention.CanTransition(JobCompleted) {
		t.Error("needs_attention leaves only through resolve")
	}
	if JobCompleted.CanTransition(JobRunning) || JobCancelled.CanTransition(JobQueued) {
		t.Error("completed and cancelled are final")
	}
	if !JobFailed.CanTransition(JobQueued) {
		t.Error("job retry requeues a failed job")
	}
	if JobNeedsAttention.Terminal() {
		t.Error("needs_attention is not terminal (§8.2)")
	}
}

func TestRoleBundles(t *testing.T) {
	cases := []struct {
		role Role
		perm Permission
		want bool
	}{
		{RoleViewer, PermAssetRead, true},
		{RoleViewer, PermApprovalDecideClipSelection, false},
		{RoleEditor, PermApprovalDecideClipSelection, true},
		{RoleEditor, PermApprovalDecidePublication, false},
		{RolePublisher, PermApprovalDecidePublication, true},
		{RolePublisher, PermApprovalDecideClipSelection, true},
		{RolePublisher, PermPublicationWithdraw, true},
		{RolePublisher, PermAssetDelete, false},
		{RoleAdmin, PermAssetDelete, true},
		{RoleAdmin, PermStorageArchiveRestore, true},
		{RoleAdmin, PermPluginInstall, true},
		{RoleAdmin, PermTenantCreate, false},
		{Role("nope"), PermAssetRead, false},
	}
	for _, c := range cases {
		if got := c.role.Has(c.perm); got != c.want {
			t.Errorf("%s.Has(%s) = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
	if !RoleAdmin.AtLeast(RoleEditor) || RoleViewer.AtLeast(RoleEditor) || Role("x").AtLeast(Role("x")) {
		t.Error("AtLeast is wrong")
	}
	if len(PermissionsOf(RoleAdmin)) <= len(PermissionsOf(RoleViewer)) {
		t.Error("bundles must accumulate")
	}
}

func TestPrincipalCan(t *testing.T) {
	editor := Principal{Role: RoleEditor, Grants: []Permission{PermAssetDelete}}
	if !editor.Can(PermAssetDelete) {
		t.Error("individual grant must apply (D48)")
	}
	if editor.Can(PermPluginInstall) {
		t.Error("no grant, no permission")
	}
	sys := Principal{SystemAdmin: true}
	if !sys.Can(PermTenantCreate) {
		t.Error("system admin creates tenants")
	}
	if sys.Can(PermAssetRead) {
		t.Error("system admin needs a membership for tenant data")
	}
}

func TestApprovalRules(t *testing.T) {
	viewer := Principal{Role: RoleViewer, Email: "v@example.com"}
	editor := Principal{Role: RoleEditor, Email: "e@example.com"}
	publisher := Principal{Role: RolePublisher, Email: "p@example.com"}
	admin := Principal{Role: RoleAdmin, Email: "a@example.com"}

	check := func(p Principal, k ApprovalKind, approvers []string, want bool) {
		t.Helper()
		if got := MayDecide(p, k, approvers); got != want {
			t.Errorf("MayDecide(%s, %s, %v) = %v, want %v", p.Role, k, approvers, got, want)
		}
	}
	check(viewer, ApprovalClipSelection, nil, false)
	check(editor, ApprovalClipSelection, nil, true)
	check(editor, ApprovalPublication, nil, false)
	check(publisher, ApprovalPublication, nil, true)
	check(admin, ApprovalPublication, nil, true)
	check(editor, ApprovalVisibilityChange, nil, false)
	check(publisher, ApprovalMetadataChange, nil, true)

	// Tightening works: selection limited to publishers.
	check(editor, ApprovalClipSelection, []string{"publisher"}, false)
	check(publisher, ApprovalClipSelection, []string{"publisher"}, true)
	check(admin, ApprovalClipSelection, []string{"publisher"}, true)
	// A listed email still needs the floor permission: no loosening.
	check(viewer, ApprovalClipSelection, []string{"v@example.com"}, false)
	check(editor, ApprovalPublication, []string{"e@example.com"}, false)
	check(editor, ApprovalClipSelection, []string{"e@example.com"}, true)

	if err := ValidateApprovers(ApprovalPublication, []string{"editor"}); err == nil {
		t.Error("editor approvers would loosen publication")
	}
	if err := ValidateApprovers(ApprovalClipSelection, []string{"viewer"}); err == nil {
		t.Error("viewer approvers would loosen selection")
	}
	if err := ValidateApprovers(ApprovalClipSelection, []string{"editor", "x@example.com"}); err != nil {
		t.Error(err)
	}
	if err := ValidateApprovers(ApprovalOther, nil); err == nil {
		t.Error("kind other needs approvers")
	}
}

func TestPublicationValidate(t *testing.T) {
	p := Publication{Status: PubPublished, AssetVersion: "v1"}
	if p.Validate() == nil {
		t.Error("published without URL must fail (R43)")
	}
	p.RemoteID, p.URL = "r", "https://media.example.internal/watch/r"
	if err := p.Validate(); err != nil {
		t.Error(err)
	}
	if (Publication{Status: PubProcessing}).Validate() == nil {
		t.Error("the asset version is required")
	}
	if (Publication{Status: PubPublished, Visibility: VisibilityPrivate}).IsPublic() {
		t.Error("private is not public")
	}
}

func TestTenantValidate(t *testing.T) {
	for _, ok := range []TenantID{"acme", "a1", "my-tenant-2"} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []TenantID{"", "a", "ACME", "../etc", "a/b", "1abc", "a.b", "acme\n"} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestInputURIRoundTrip(t *testing.T) {
	job := string(id.New())
	for _, s := range []string{
		"server:/data/class.mp4",
		"upload:UPL-001",
		"storage://master/originals/ORI-001/v1/master.mp4",
		"import:youtube-main/abc123",
		"scratch://" + job + "/compress/master.mp4",
	} {
		u, err := ParseInputURI(s)
		if err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		if u.String() != s {
			t.Errorf("round trip %s -> %s", s, u.String())
		}
	}
	u, err := ParseInputURI("/data/class.mp4")
	if err != nil || u.Kind != URIServer || u.String() != "server:/data/class.mp4" {
		t.Errorf("a bare path must become server: (R20), got %+v, %v", u, err)
	}
}

func TestInputURIRejects(t *testing.T) {
	for _, s := range []string{
		"", "relative/path.mp4", "server:relative",
		"upload:", "upload:ORI-001", "upload:nonsense",
		"storage://", "storage://cold/key", "storage://master", "storage://master/", "storage://master/../x", "storage://master/a//b", "storage://master//abs",
		"import:", "import:only", "import:/x",
		"scratch://x/y/z", "scratch://" + string(id.New()) + "/../x", "scratch://" + string(id.New()) + "/node/../../etc/passwd",
	} {
		if _, err := ParseInputURI(s); err == nil {
			t.Errorf("%q should be rejected", s)
		}
	}
	if _, err := ParseUserInputURI("scratch://" + string(id.New()) + "/n/f.mp4"); err == nil {
		t.Error("clients cannot supply scratch references")
	}
}

func TestArchiveGuards(t *testing.T) {
	archive, _ := ParseInputURI("storage://archive/originals/ORI-001/v1/original.mov")
	master, _ := ParseInputURI("storage://master/originals/ORI-001/v1/master.mp4")
	proxy, _ := ParseInputURI("storage://proxy/originals/ORI-001/v1/proxy.mp4")
	scratch, _ := ParseInputURI("scratch://" + string(id.New()) + "/n/f.mp4")

	if err := archive.ForbidRole(RoleArchive); !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
		t.Errorf("archive must be refused, got %v", err)
	}
	if err := master.ForbidRole(RoleArchive); err != nil {
		t.Error(err)
	}
	if err := master.RequireRole(RoleMaster, false); err != nil {
		t.Error(err)
	}
	if err := proxy.RequireRole(RoleMaster, true); err == nil {
		t.Error("video.cut must not take a proxy")
	}
	if err := archive.RequireRole(RoleMaster, true); err == nil {
		t.Error("video.cut must not take archive")
	}
	if err := scratch.RequireRole(RoleMaster, true); err != nil {
		t.Error(err)
	}
	if err := scratch.RequireRole(RoleMaster, false); err == nil {
		t.Error("scratch only when allowed")
	}
}

func TestResolveServerPath(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ingest")
	outside := filepath.Join(dir, "secret")
	for _, d := range []string{root, outside, filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	good := filepath.Join(root, "sub", "class.mp4")
	secret := filepath.Join(outside, "key.pem")
	for _, f := range []string{good, secret} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "escape.mp4")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	roots := []string{root}

	if _, err := ResolveServerPath(roots, good); err != nil {
		t.Errorf("good path: %v", err)
	}
	if _, err := ResolveServerPath(roots, secret); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("outside the root must be forbidden, got %v", err)
	}
	if _, err := ResolveServerPath(roots, link); !errs.IsCode(err, errs.CodeForbidden) {
		t.Errorf("symlink leaving the root must be forbidden, got %v", err)
	}
	if _, err := ResolveServerPath(roots, root+"/sub/../../secret/key.pem"); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("'..' must be rejected, got %v", err)
	}
	if _, err := ResolveServerPath(roots, filepath.Join(root, "missing.mp4")); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("missing file, got %v", err)
	}
	var coded *errs.Error
	if _, err := ResolveServerPath(nil, good); !errors.As(err, &coded) {
		t.Errorf("no roots means nothing is allowed, got %v", err)
	}
}

func TestAuditHashCoversEveryField(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 123456789, time.UTC)
	base := AuditRecord{
		Seq: 1, ULID: "01JABCDEFGHJKMNPQRSTVWXYZ0", TenantID: "acme", At: at, Actor: "a@example.com",
		Method: AuthDevice, Action: "asset.update", Permission: PermAssetWrite, Resource: "ORI-001",
		Result: "allowed", RequestID: "r1", Details: []byte(`{"k":"v"}`), PrevHash: AuditGenesisHash,
	}
	want, err := base.ComputeHash()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := base.ComputeHash(); again != want {
		t.Fatal("the hash must be deterministic")
	}
	changes := map[string]func(*AuditRecord){
		"seq":        func(r *AuditRecord) { r.Seq = 2 },
		"ulid":       func(r *AuditRecord) { r.ULID = "01JABCDEFGHJKMNPQRSTVWXYZ1" },
		"tenant":     func(r *AuditRecord) { r.TenantID = "other" },
		"at":         func(r *AuditRecord) { r.At = at.Add(time.Nanosecond) },
		"actor":      func(r *AuditRecord) { r.Actor = "b@example.com" },
		"method":     func(r *AuditRecord) { r.Method = AuthPeer },
		"action":     func(r *AuditRecord) { r.Action = "asset.delete" },
		"permission": func(r *AuditRecord) { r.Permission = PermAssetDelete },
		"resource":   func(r *AuditRecord) { r.Resource = "ORI-002" },
		"result":     func(r *AuditRecord) { r.Result = "denied" },
		"request":    func(r *AuditRecord) { r.RequestID = "r2" },
		"details":    func(r *AuditRecord) { r.Details = []byte(`{"k":"w"}`) },
		"prev":       func(r *AuditRecord) { r.PrevHash = strings.Repeat("1", 64) },
	}
	for name, change := range changes {
		r := base
		change(&r)
		got, err := r.ComputeHash()
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
	// A hash that went through JSON still verifies: an offline check of an export.
	base.Hash = want
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var back AuditRecord
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if got, _ := back.ComputeHash(); got != want {
		t.Fatal("an exported record must verify offline")
	}
}

func TestTranscriptRedactedKeepsTimeline(t *testing.T) {
	tr := Transcript{Segments: []TranscriptSegment{
		{Start: 0, End: 10, Text: "public", Words: []Word{{Start: 0, End: 1, Text: "public"}}},
		{Start: 10, End: 20, Text: "name of a minor", Words: []Word{{Start: 10, End: 11, Text: "name"}}},
		{Start: 20, End: 30, Text: "public again"},
	}}
	got := tr.Redacted([]SensitiveSegment{{StartSeconds: 12, EndSeconds: 15}})
	if got.Segments[1].Text != RedactedText || got.Segments[1].Words != nil {
		t.Fatalf("segment not redacted: %+v", got.Segments[1])
	}
	if got.Segments[0].Text != "public" || got.Segments[2].Text != "public again" {
		t.Fatalf("neighbors must stay: %+v", got.Segments)
	}
	if got.Segments[1].Start != 10 || got.Segments[1].End != 20 {
		t.Fatal("the timeline must stay intact")
	}
	if tr.Segments[1].Text != "name of a minor" {
		t.Fatal("the input must not change")
	}
}
