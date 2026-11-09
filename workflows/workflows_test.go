package workflows

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/internal/workflow"
)

// required are the files of the master plan's section 10.
var required = []string{
	"ingest-original", "ingest-derived", "create-clip", "publish-video", "publish-one",
	"unpublish-video", "delete-asset", "restore-archived", "reindex-asset",
	"refresh-publication", "verify-storage", "bulk-create-clips",
	"update-public-library", "public-library-on-withdrawn", "public-library-on-updated",
	"expire-unapproved-renders",
}

func load(t *testing.T) *workflow.Registry {
	t.Helper()
	v := workflow.NewValidator(nil)
	r := workflow.NewRegistry(v)
	if err := r.LoadFS(FS, "workflows"); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEveryFileIsValidWithoutWarnings(t *testing.T) {
	v := workflow.NewValidator(nil)
	r := workflow.NewRegistry(v)
	if err := r.LoadFS(FS, "workflows"); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
	for _, e := range r.List() {
		res := v.Validate(e.Raw)
		for _, i := range res.Issues {
			t.Errorf("%s: %s: %s", e.Definition.ID, i.Code, i.Message)
		}
	}
}

func TestRequiredFilesExist(t *testing.T) {
	r := load(t)
	for _, id := range required {
		if _, ok := r.Get(id, ""); !ok {
			t.Errorf("missing workflow %s", id)
		}
	}
}

func TestCreateClipGateCannotBeSkipped(t *testing.T) {
	r := load(t)
	e, _ := r.Get("create-clip", "")
	var doc map[string]any
	if err := json.Unmarshal(e.Raw, &doc); err != nil {
		t.Fatal(err)
	}
	// Removing the gate, however it is done, is refused (D53).
	v := workflow.NewValidator(nil)
	workflow.NewRegistry(v)
	nodes := doc["nodes"].([]any)
	var kept []any
	for _, n := range nodes {
		if n.(map[string]any)["id"] != "approve_selection" {
			kept = append(kept, n)
		}
	}
	doc["nodes"] = kept
	conn := doc["connections"].(map[string]any)
	delete(conn, "approve_selection")
	conn["overlap"] = []any{"if_approved"}
	conn["preview"] = []any{"if_approved"}
	broken, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	res := v.Validate(broken)
	if res.OK() {
		t.Fatal("create-clip without its gate must be invalid")
	}
	found := false
	for _, i := range res.Issues {
		if i.Code == workflow.IssueClipSelectionGate || i.Code == workflow.IssueReference {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues %+v", res.Issues)
	}
	def := e.Definition
	inputs := make([]string, 0, len(def.Inputs))
	for name := range def.Inputs {
		inputs = append(inputs, name)
	}
	for _, name := range inputs {
		if strings.Contains(strings.ToLower(name), "approv") || strings.Contains(strings.ToLower(name), "skip") {
			t.Errorf("create-clip must offer no option to skip the gate, found input %q", name)
		}
	}
}

func TestPermissions(t *testing.T) {
	r := load(t)
	want := map[string][]domain.Permission{
		"create-clip":           nil, // an editor runs it; the gates authorize the rest (D7)
		"publish-video":         nil,
		"publish-one":           {domain.PermPublicationCreate},
		"publish-private-first": {domain.PermPublicationCreate},
		"unpublish-video":       {domain.PermPublicationWithdraw},
		"delete-asset":          {domain.PermAssetDelete},
		"restore-archived":      {domain.PermStorageArchiveRestore},
		"ingest-original":       nil,
	}
	for id, perms := range want {
		def, ok := r.LookupWorkflow(id, "")
		if !ok {
			t.Fatalf("missing %s", id)
		}
		got := workflow.RequiredPermissions(def, r)
		if len(got) != len(perms) {
			t.Errorf("%s: required %v, want %v", id, got, perms)
			continue
		}
		for i := range got {
			if got[i] != perms[i] {
				t.Errorf("%s: required %v, want %v", id, got, perms)
			}
		}
	}
}

func TestIngestOriginalCoversR26(t *testing.T) {
	r := load(t)
	def, _ := r.LookupWorkflow("ingest-original", "")
	g, problems := workflow.BuildGraph(def)
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	for _, before := range []string{"backup", "store_master", "store_proxy", "index", "timeline"} {
		if !g.IsAncestor(before, "ready") {
			t.Errorf("%s must be required by mark_ready (R11, R26)", before)
		}
	}
	for _, n := range def.Nodes {
		if n.Type == "mediacms.register" {
			t.Errorf("the default file must not call MediaCMS (D24)")
		}
	}
	// ASR audio comes from the stored master (D56).
	audio := nodeSpec(def, "audio")
	if !strings.Contains(string(audio.Parameters), "${nodes.store_master.output.uri}") {
		t.Errorf("audio.extract source: %s", audio.Parameters)
	}
	backup := nodeSpec(def, "backup")
	if !strings.Contains(string(backup.Parameters), "original.${nodes.probe.output.ext}") {
		t.Errorf("the raw key must keep the original extension (R26): %s", backup.Parameters)
	}
	if def.Settings.AssetStatusOnFailure != "ingest_failed" {
		t.Errorf("settings: %+v", def.Settings)
	}
}

func nodeSpec(def *wfdef.Definition, id string) *wfdef.NodeSpec {
	for i := range def.Nodes {
		if def.Nodes[i].ID == id {
			return &def.Nodes[i]
		}
	}
	return nil
}

func TestTriggers(t *testing.T) {
	r := load(t)
	events := map[string]string{}
	for _, e := range r.Triggered(wfdef.TriggerEvent) {
		events[e.Definition.ID] = e.Definition.Trigger.Event
	}
	for id, ev := range map[string]string{
		"update-public-library":       "publication.completed",
		"public-library-on-withdrawn": "publication.withdrawn",
		"public-library-on-updated":   "publication.updated",
	} {
		if events[id] != ev {
			t.Errorf("%s triggers on %q", id, events[id])
		}
	}
	scheduled := map[string]bool{}
	for _, e := range r.Triggered(wfdef.TriggerSchedule) {
		scheduled[e.Definition.ID] = true
	}
	if !scheduled["expire-unapproved-renders"] || !scheduled["verify-storage"] {
		t.Errorf("scheduled: %v", scheduled)
	}
}
