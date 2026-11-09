package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/hook"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/internal/workflow"
)

const simpleDoc = `{"id":"simple","version":"1.0.0","nodes":[{"id":"n","type":"test.pure","typeVersion":1,"parameters":{}}]}`

func TestIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	h.add(simpleDoc)
	h.add(`{"id":"other","version":"1.0.0","nodes":[{"id":"n","type":"test.pure","typeVersion":1,"parameters":{}}]}`)
	ctx := context.Background()
	wf := h.engine.Workflows()
	s := scopeOf(editor)
	s.IdempotencyKey = "same-key"
	first, err := wf.Run(ctx, s, "simple", api.RunWorkflowRequest{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := wf.Run(ctx, s, "simple", api.RunWorkflowRequest{IdempotencyKey: "same-key"})
	if err != nil {
		t.Fatal(err)
	}
	if first.JobID != second.JobID {
		t.Fatalf("two jobs for one key: %s %s", first.JobID, second.JobID)
	}
	if _, err := wf.Run(ctx, s, "simple", api.RunWorkflowRequest{IdempotencyKey: "another"}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("header and body differ: %v", err)
	}
	if _, err := wf.Run(ctx, s, "other", api.RunWorkflowRequest{}); !errs.IsCode(err, errs.CodeIdempotencyMismatch) {
		t.Fatalf("same key, other workflow: %v", err)
	}
	h.waitStatus(h.job(first.JobID), domain.JobCompleted)
	if len(h.callsOf("n")) != 1 {
		t.Fatalf("ran %d", len(h.callsOf("n")))
	}
}

func TestRequiredPermissionsAtSubmission(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"deleter","version":"1.0.0","nodes":[{"id":"d","type":"catalog.delete","typeVersion":1,"parameters":{"assetId":"ORI-001"}}]}`)
	h.add(`{"id":"restorer","version":"1.0.0","nodes":[{"id":"g","type":"storage.get","typeVersion":1,"parameters":{"source":"storage://archive/a","allowArchive":true}}]}`)
	h.add(`{"id":"pub","version":"1.0.0","nodes":[{"id":"p","type":"publish.prepare","typeVersion":1,"parameters":{"assetId":"ORI-001","destination":"d"}}]}`)
	h.add(simpleDoc)
	ctx := context.Background()
	wf := h.engine.Workflows()
	for name, c := range map[string]struct {
		p    domain.Principal
		perm domain.Permission
	}{
		"deleter":  {editor, domain.PermAssetDelete},
		"restorer": {publisher, domain.PermStorageArchiveRestore},
		"pub":      {editor, domain.PermPublicationCreate},
	} {
		if _, err := wf.Run(ctx, scopeOf(c.p), name, api.RunWorkflowRequest{}); !errs.IsCode(err, errs.CodeForbidden) || !strings.Contains(err.Error(), string(c.perm)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An individual grant counts (D48).
	granted := editor
	granted.Grants = []domain.Permission{domain.PermAssetDelete}
	resp, err := wf.Run(ctx, scopeOf(granted), "deleter", api.RunWorkflowRequest{})
	if err != nil {
		t.Fatal(err)
	}
	job := h.job(resp.JobID)
	if len(job.Permissions) != 1 || job.Permissions[0] != domain.PermAssetDelete {
		t.Fatalf("the job records what it needs: %v", job.Permissions)
	}
	// Listing shows what the caller may run (D59).
	list, err := wf.List(ctx, scopeOf(editor))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, w := range list {
		names[w.ID] = true
	}
	if !names["simple"] || names["deleter"] || names["restorer"] {
		t.Fatalf("listed %v", names)
	}
	detail, err := wf.Get(ctx, scopeOf(editor), "simple", "")
	if err != nil || detail.SHA256 == "" || len(detail.Definition) == 0 {
		t.Fatalf("detail %+v %v", detail, err)
	}
	if _, err := wf.Get(ctx, scopeOf(editor), "nope", ""); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestInlineDefinitionsAndValidate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wf := h.engine.Workflows()
	s := scopeOf(editor)
	resp, err := wf.Run(ctx, s, "", api.RunWorkflowRequest{Definition: json.RawMessage(simpleDoc)})
	if err != nil {
		t.Fatal(err)
	}
	job := h.waitStatus(h.job(resp.JobID), domain.JobCompleted)
	if job.WorkflowSHA256 != wfdef.SHA256([]byte(simpleDoc)) {
		t.Fatal("the hash must cover the exact bytes submitted")
	}
	for _, doc := range []string{
		`{"id":"x"}`,
		`{"id":"x","version":"1.0.0","nodes":[{"id":"n","type":"nope.nope","typeVersion":1}]}`,
		`{"id":"x","version":"1.0.0","nodes":[{"id":"n","type":"test.pure","typeVersion":1}],"subworkflows":{"y":{"id":"y"}}}`,
	} {
		if _, err := wf.Run(ctx, s, "", api.RunWorkflowRequest{Definition: json.RawMessage(doc)}); !errs.IsCode(err, errs.CodeValidationFailed) {
			t.Errorf("%s: %v", doc, err)
		}
	}
	if _, err := wf.Run(ctx, s, "", api.RunWorkflowRequest{}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("nothing to run: %v", err)
	}
	// Webhook triggers validate, and are refused when run (I22).
	web := `{"id":"hook-in","version":"1.0.0","trigger":{"type":"webhook"},"nodes":[{"id":"n","type":"test.pure","typeVersion":1}]}`
	res, err := wf.Validate(ctx, s, api.ValidateWorkflowRequest{Definition: json.RawMessage(web)})
	if err != nil || !res.Valid || len(res.Issues) != 1 {
		t.Fatalf("validate %+v %v", res, err)
	}
	if _, err := wf.Run(ctx, s, "", api.RunWorkflowRequest{Definition: json.RawMessage(web)}); !errs.IsCode(err, errs.CodeNotImplemented) {
		t.Fatalf("webhook: %v", err)
	}
	res, err = wf.Validate(ctx, s, api.ValidateWorkflowRequest{Definition: json.RawMessage(`{"id":"x"}`)})
	if err != nil || res.Valid {
		t.Fatalf("invalid: %+v %v", res, err)
	}
}

func TestDryRunHasNoEffect(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.tenantCfg = config.Tenant{
		Storages: config.Storages{Roles: map[domain.StorageRole]config.StorageInstance{
			domain.RoleMaster: {Driver: "local", Root: "/m", Usage: config.UsageOperational},
		}},
		Publishing: config.Publishing{Destinations: map[string]config.Destination{"yt": {Driver: "youtube", CredentialsRef: "youtube-oauth"}}},
	}
	if err := h.store.Secrets().Put(ctx, repo.SecretRecord{Meta: domain.SecretMeta{Name: "youtube-oauth", Location: domain.SecretClient}, Ciphertext: []byte("sealed"), KeyVersion: 1}); err != nil {
		t.Fatal(err)
	}
	wf := h.engine.Workflows()
	doc := `{"id":"auto-withdraw","version":"1.0.0","inputs":{"asset":{"type":"string","required":true}},"trigger":{"type":"event","event":"asset.deleted"},"nodes":[
		{"id":"w","type":"publish.withdraw","typeVersion":1,"parameters":{"assetId":"ORI-001","destinations":["yt"]}},
		{"id":"p","type":"storage.put","typeVersion":1,"parameters":{"storage":"archive","source":"server:/x","key":"k"}}]}`
	resp, err := wf.Run(ctx, scopeOf(editor), "", api.RunWorkflowRequest{Definition: json.RawMessage(doc), DryRun: true})
	if err != nil {
		t.Fatalf("a dry run reports, it does not fail: %v", err)
	}
	if resp.JobID != "" || resp.Validation == nil {
		t.Fatalf("response %+v", resp)
	}
	codes := map[string]bool{}
	for _, i := range resp.Validation.Issues {
		codes[i.Code] = true
	}
	if !codes[workflow.IssueTrigger] {
		t.Fatalf("a required input with a trigger: %+v", resp.Validation)
	}
	doc = strings.Replace(doc, `"inputs":{"asset":{"type":"string","required":true}},`, "", 1)
	resp, err = wf.Run(ctx, scopeOf(editor), "", api.RunWorkflowRequest{Definition: json.RawMessage(doc), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	codes = map[string]bool{}
	for _, i := range resp.Validation.Issues {
		codes[i.Code] = true
	}
	for _, want := range []string{workflow.IssuePermission, workflow.IssueConnector, workflow.IssueCredential} {
		if !codes[want] {
			t.Errorf("missing %s in %+v", want, resp.Validation.Issues)
		}
	}
	if resp.Validation.Valid {
		t.Fatalf("a permission problem makes it invalid: %+v", resp.Validation)
	}
	// The R37 warning also comes from plain validate (§15.6).
	res, err := wf.Validate(ctx, scopeOf(editor), api.ValidateWorkflowRequest{Definition: json.RawMessage(doc)})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range res.Issues {
		if i.Code == workflow.IssueCredential && i.Severity == workflow.SeverityWarning && strings.Contains(i.Message, "youtube-oauth") {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues %+v", res.Issues)
	}
	active, _ := h.store.Jobs().ListActive(ctx)
	if len(active) != 0 {
		t.Fatalf("a dry run created a job: %+v", active)
	}
	// Given no inputs, a dry run warns about required ones (§11.3).
	h.add(`{"id":"needs","version":"1.0.0","inputs":{"title":{"type":"string","required":true}},"nodes":[{"id":"n","type":"test.pure","typeVersion":1}]}`)
	resp, err = wf.Run(ctx, scopeOf(editor), "needs", api.RunWorkflowRequest{DryRun: true})
	if err != nil || !resp.Validation.Valid || len(resp.Validation.Issues) == 0 {
		t.Fatalf("response %+v %v", resp.Validation, err)
	}
}

func TestFileInputs(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"takes","version":"1.0.0","inputs":{"file":{"type":"file","required":true}},"nodes":[{"id":"n","type":"test.pure","typeVersion":1,"parameters":{"f":"${inputs.file}"}}]}`)
	ctx := context.Background()
	wf := h.engine.Workflows()
	s := scopeOf(editor)
	root := h.deps.Daemon.IngestRoots[0]
	good := filepath.Join(root, "class.mp4")
	if err := os.WriteFile(good, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(v string) (domain.Job, error) {
		raw, _ := json.Marshal(map[string]string{"file": v})
		resp, err := wf.Run(ctx, s, "takes", api.RunWorkflowRequest{Inputs: raw})
		if err != nil {
			return domain.Job{}, err
		}
		return h.job(resp.JobID), nil
	}
	j, err := run(good)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(j.Inputs), "server:") {
		t.Fatalf("a bare path is canonicalized (R20): %s", j.Inputs)
	}
	for name, ref := range map[string]string{
		"outside the roots": "/etc/passwd",
		"traversal":         "server:" + root + "/../secret",
		"relative":          "class.mp4",
		"scratch":           "scratch://01J00000000000000000000000/n/f",
		"archive":           "storage://archive/originals/x",
	} {
		if _, err := run(ref); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	// A symbolic link leaving the roots is refused too (§15.6).
	link := filepath.Join(root, "link")
	if err := os.Symlink("/etc/hosts", link); err == nil {
		if _, err := run("server:" + link); err == nil {
			t.Error("a link out of the roots must be refused")
		}
	}
	// An upload must be complete, and reaches workers by ULID (I183).
	upl := domain.Upload{ID: "UPL-001", ULID: id.New(), Status: domain.UploadPending, TenantID: "acme"}
	if err := h.store.Uploads().Create(ctx, upl); err != nil {
		t.Fatal(err)
	}
	if _, err := run("upload:UPL-001"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("incomplete upload: %v", err)
	}
	upl.Status = domain.UploadComplete
	if err := h.store.Uploads().Update(ctx, upl); err != nil {
		t.Fatal(err)
	}
	j, err = run("upload:UPL-001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(j.Inputs), "upload:"+string(upl.ULID)) {
		t.Fatalf("inputs %s", j.Inputs)
	}
	if _, err := run("upload:UPL-999"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown upload: %v", err)
	}
	// Unknown and wrongly typed inputs are refused before a job exists.
	if _, err := wf.Run(ctx, s, "takes", api.RunWorkflowRequest{Inputs: json.RawMessage(`{"file":"` + good + `","extra":1}`)}); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Fatalf("unknown input: %v", err)
	}
}

func TestTenantIsolation(t *testing.T) {
	h := newHarness(t)
	asset := h.newAsset(domain.KindOriginal)
	h.add(strings.Replace(gateDoc, "KIND", "clip_selection", 1))
	j := h.run("gate", map[string]any{"asset": asset.ID})
	a := h.pendingApproval(j)
	ctx := context.Background()
	foreign := service.Scope{Tenant: "globex", Principal: domain.Principal{Email: "x@example.com", Method: domain.AuthDevice, Role: domain.RoleAdmin}}
	if _, err := h.engine.Jobs().Get(ctx, foreign, id.Ref(j.ID)); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("job: %v", err)
	}
	if _, err := h.engine.Jobs().Get(ctx, foreign, id.Ref(j.ULID)); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("job by ULID: %v", err)
	}
	if _, err := h.engine.Jobs().Cancel(ctx, foreign, id.Ref(j.ID), api.JobActionRequest{}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := h.engine.Approvals().Approve(ctx, foreign, id.Ref(a.ID), api.ApproveRequest{}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("approve: %v", err)
	}
	if _, err := h.engine.Workflows().CreateClip(ctx, foreign, api.CreateClipRequest{SourceID: string(asset.ID), Prompt: "x"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("clip of a foreign source: %v", err)
	}
	if _, err := h.engine.Jobs().Get(ctx, service.Scope{Principal: admin}, id.Ref(j.ID)); !errs.IsCode(err, errs.CodeTenantRequired) {
		t.Fatalf("no tenant: %v", err)
	}
	// Each tenant numbers its own (D46).
	other, _ := h.stores.ForTenant(ctx, "globex")
	h.add(simpleDoc)
	resp, err := h.engine.Workflows().Run(ctx, foreign, "simple", api.RunWorkflowRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.JobID != "JOB-001" {
		t.Fatalf("job %s", resp.JobID)
	}
	h.waitFor("the foreign job", func() bool {
		got, err := other.Jobs().Get(ctx, id.Ref(resp.JobID))
		return err == nil && got.Status == domain.JobCompleted
	})
}

func TestRunNode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wf := h.engine.Workflows()
	asset := h.newAsset(domain.KindOriginal)
	if err := h.store.Versions().PutFile(ctx, id.Ref(asset.ID), "v1", domain.VersionFile{Purpose: domain.FileWorkingMaster, File: domain.MediaFile{Storage: domain.RoleMaster, Key: "originals/ORI-001/v1/master.mp4"}}); err != nil {
		t.Fatal(err)
	}
	h.output("ai.transcribe", map[string]any{"transcript": map[string]any{"language": "en"}})
	// `vidyom node run ai.transcribe --asset ORI-001` (§11.3).
	resp, err := wf.RunNode(ctx, scopeOf(editor), "ai.transcribe", api.RunNodeRequest{AssetID: string(asset.ID)})
	if err != nil {
		t.Fatal(err)
	}
	j := h.waitStatus(h.job(resp.JobID), domain.JobCompleted)
	if j.AssetID != asset.ID {
		t.Fatalf("asset %q", j.AssetID)
	}
	var params map[string]any
	_ = json.Unmarshal(h.callsOf("run")[0].Params, &params)
	if params["assetId"] != string(asset.ID) || params["source"] != "storage://master/originals/ORI-001/v1/master.mp4" {
		t.Fatalf("params %v", params)
	}
	// D53: rendering has no way around the gate.
	if _, err := wf.RunNode(ctx, scopeOf(admin), "video.cut", api.RunNodeRequest{Parameters: json.RawMessage(`{"source":"storage://master/a","segments":[{"start_seconds":1,"end_seconds":2}]}`)}); !errs.IsCode(err, errs.CodeValidationFailed) {
		t.Fatalf("video.cut: %v", err)
	}
	for _, typ := range []string{"flow.if", "approval.request", "workflow.execute", "event.emit"} {
		if _, err := wf.RunNode(ctx, scopeOf(admin), typ, api.RunNodeRequest{}); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: %v", typ, err)
		}
	}
	if _, err := wf.RunNode(ctx, scopeOf(admin), "nope.nope", api.RunNodeRequest{}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := wf.RunNode(ctx, scopeOf(editor), "catalog.delete", api.RunNodeRequest{AssetID: string(asset.ID)}); !errs.IsCode(err, errs.CodeForbidden) {
		t.Fatalf("permission: %v", err)
	}
	if _, err := wf.RunNode(ctx, scopeOf(editor), "ai.transcribe", api.RunNodeRequest{AssetID: "ORI-999"}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown asset: %v", err)
	}
}

func TestJobReadsAndLogs(t *testing.T) {
	h := newHarness(t)
	h.add(`{"id":"logs","version":"1.0.0","nodes":[{"id":"p","type":"video.probe","typeVersion":1,"parameters":{"source":"storage://master/a"}}]}`)
	j := h.waitStatus(h.run("logs", nil), domain.JobCompleted)
	ctx := context.Background()
	if _, err := h.store.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: j.ID, Kind: domain.JobEventLog, Level: "info", Message: "hello"}); err != nil {
		t.Fatal(err)
	}
	jobs := h.engine.Jobs()
	logs, err := jobs.Logs(ctx, scopeOf(viewer), id.Ref(j.ID), api.PageQuery{})
	if err != nil || len(logs.Items) != 1 || logs.Items[0].Message != "hello" {
		t.Fatalf("logs %+v %v", logs, err)
	}
	detail, err := jobs.Get(ctx, scopeOf(viewer), id.Ref(j.ID))
	if err != nil || len(detail.Nodes) != 1 {
		t.Fatalf("detail %+v %v", detail, err)
	}
	list, err := jobs.List(ctx, scopeOf(viewer), api.JobQuery{Status: domain.JobCompleted, Workflow: "logs"})
	if err != nil || len(list.Items) != 1 || list.Items[0].Snapshot != nil {
		t.Fatalf("list %+v %v", list, err)
	}
	empty, err := jobs.List(ctx, scopeOf(viewer), api.JobQuery{Status: domain.JobFailed})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty %+v %v", empty, err)
	}
	// A waiting node's internal state does not reach the API.
	asset := h.newAsset(domain.KindOriginal)
	h.add(strings.Replace(gateDoc, "KIND", "clip_selection", 1))
	waiting := h.run("gate", map[string]any{"asset": asset.ID})
	h.pendingApproval(waiting)
	detail, err = jobs.Get(ctx, scopeOf(viewer), id.Ref(waiting.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range detail.Nodes {
		if n.NodeID == "ask" && len(n.Output) != 0 {
			t.Fatalf("output %s", n.Output)
		}
	}
}

func TestIntrospection(t *testing.T) {
	h := newHarness(t)
	h.tenantCfg = config.Tenant{Hooks: config.Hooks{Workflow: wfdef.HookSet{Before: []wfdef.HookDecl{decl("audit-all")}}}}
	h.add(hookedDoc)
	ctx := context.Background()
	in := h.engine.Introspection()
	s := scopeOf(admin)
	nodes, err := in.Nodes(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]node.Descriptor{}
	for _, d := range nodes {
		seen[d.Type] = d
	}
	if _, ok := seen["storage.put"]; !ok {
		t.Fatal("worker-pool nodes come from the catalog")
	}
	if _, ok := seen["flow.if"]; !ok {
		t.Fatal("registered nodes are listed")
	}
	d, err := in.Node(ctx, s, "video.cut")
	if err != nil || d.Pool != node.PoolMedia {
		t.Fatalf("node %+v %v", d, err)
	}
	if _, err := in.Node(ctx, s, "nope.nope"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	hooksList, err := in.Hooks(ctx, s, "hooked")
	if err != nil {
		t.Fatal(err)
	}
	levels := map[string]bool{}
	for _, info := range hooksList {
		levels[info.Level+":"+info.ID] = true
	}
	if !levels["global:audit-all"] || !levels["workflow:w-before"] || !levels["node:n-after"] {
		t.Fatalf("hooks %v", levels)
	}
	if _, err := in.Hooks(ctx, s, "nope"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown workflow: %v", err)
	}
	h.hookFn = func(inv hook.Invocation) (hook.Result, error) {
		return hook.Result{Allow: false, Reason: "not today"}, nil
	}
	out, err := in.TestHook(ctx, s, "w-before", api.TestHookRequest{AssetID: "ORI-001"})
	if err != nil || out["allow"] != false || out["reason"] != "not today" {
		t.Fatalf("test hook %v %v", out, err)
	}
	if _, err := in.TestHook(ctx, s, "nope", api.TestHookRequest{}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("unknown hook: %v", err)
	}
}

type fakeEmbedder struct{ connector.Embedder }

type fakeLLM struct {
	connector.LLM
	endpoint config.LLMEndpoint
}

// TestDaemonEnv proves all of node.DaemonEnv, including config.AllowedEndpoints for
// LLM (D8), through a node that uses it.
func TestDaemonEnv(t *testing.T) {
	h := newHarness(t, harnessOpts{noEngine: true})
	h.stopWorker()
	reg := connector.NewRegistry()
	reg.RegisterEmbedder(connector.Info{Driver: DefaultEmbedderDriver, Available: true}, func(_ context.Context, p connector.AIParams) (connector.Embedder, error) {
		if p.Tenant != "acme" || p.AI.EmbeddingModel != "bge-m3" {
			t.Errorf("params %+v", p)
		}
		return fakeEmbedder{}, nil
	})
	reg.RegisterLLM(connector.Info{Driver: DefaultLLMDriver, Available: true}, func(_ context.Context, ep config.LLMEndpoint, _ connector.SecretResolver) (connector.LLM, error) {
		return fakeLLM{endpoint: ep}, nil
	})
	h.deps.Connectors = reg
	h.deps.SecretsFor = func(domain.TenantID) connector.SecretResolver { return fixedSecrets{"key": "value"} }
	h.tenantCfg = config.Tenant{AI: config.AI{EmbeddingModel: "bge-m3", LLMEndpoints: []config.LLMEndpoint{
		{Name: "cloud", BaseURL: "https://llm.example.internal", Class: config.EndpointExternal},
		{Name: "lan", BaseURL: "http://llm.lan", Class: config.EndpointLocal},
	}}}
	h.startEngine()
	checked := make(chan error, 1)
	asset := h.newAsset(domain.KindOriginal)
	h.on("test.daemon", func(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
		checked <- func() error {
			d, err := node.RequireDaemon(env)
			if err != nil {
				return err
			}
			if d.Tenant() != "acme" || d.JobID() == "" || d.NodeID() != "n" || d.TraceID() == "" || d.Logger() == nil {
				return errs.New(errs.CodeInternal, "identity")
			}
			if d.Store().Tenant() != "acme" {
				return errs.New(errs.CodeInternal, "store")
			}
			if _, err := d.TenantConfig(ctx); err != nil {
				return err
			}
			if _, err := d.Embedder(ctx); err != nil {
				return err
			}
			if v, err := d.Secrets().ResolveSecret(ctx, "key"); err != nil || string(v) != "value" {
				return errs.New(errs.CodeInternal, "secrets")
			}
			local, err := d.LLM(ctx, domain.Asset{ID: asset.ID, AIProcessing: domain.AILocalOnly})
			if err != nil {
				return err
			}
			if local.(fakeLLM).endpoint.Name != "lan" {
				return errs.New(errs.CodeInternal, "local_only reached "+local.(fakeLLM).endpoint.Name)
			}
			external, err := d.LLM(ctx, domain.Asset{ID: asset.ID, AIProcessing: domain.AIExternalAllowed})
			if err != nil || external.(fakeLLM).endpoint.Name != "lan" {
				return errs.New(errs.CodeInternal, "local first")
			}
			if err := d.Emit(ctx, nil, eventDraft()); err != nil {
				return err
			}
			if _, err := d.Publishing(ctx, "d"); !errs.IsCode(err, errs.CodeUnavailable) {
				return errs.New(errs.CodeInternal, "publishing must say it is not wired")
			}
			if _, err := d.Storage(ctx, domain.RoleMaster); !errs.IsCode(err, errs.CodeUnavailable) {
				return errs.New(errs.CodeInternal, "storage must say it is not wired")
			}
			// Scratch stays inside the job, and other jobs' files are refused.
			path, err := d.Scratch().Path("a/out.bin")
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				return err
			}
			uri, err := domain.ParseInputURI(d.Scratch().URI("a/out.bin"))
			if err != nil {
				return err
			}
			back, err := d.Scratch().Resolve(uri)
			if err != nil || back != path {
				return errs.New(errs.CodeInternal, "scratch round trip")
			}
			uri.JobULID = string(id.New())
			if _, err := d.Scratch().Resolve(uri); !errs.IsCode(err, errs.CodeForbidden) {
				return errs.New(errs.CodeInternal, "another job's scratch")
			}
			if _, err := d.Scratch().Path("../escape"); err == nil {
				return errs.New(errs.CodeInternal, "traversal")
			}
			input, err := d.OpenInput(ctx, d.Scratch().URI("a/out.bin"))
			if err != nil || input.Size != 1 {
				return errs.New(errs.CodeInternal, "open scratch")
			}
			if _, err := d.OpenInput(ctx, "storage://archive/x", domain.RoleArchive); !errs.IsCode(err, errs.CodeForbiddenStorageRole) {
				return errs.New(errs.CodeInternal, "forbidden role")
			}
			if _, err := d.OpenInput(ctx, "import:youtube/abc"); !errs.IsCode(err, errs.CodeNotImplemented) {
				return errs.New(errs.CodeInternal, "import is a documented gap")
			}
			if d.HasPermission(domain.PermStorageArchiveRestore) {
				return errs.New(errs.CodeInternal, "permission out of nowhere")
			}
			d.Progress(50, "half way")
			return nil
		}()
		return node.JSONResult(map[string]any{})
	})
	h.add(`{"id":"env","version":"1.0.0","nodes":[{"id":"n","type":"test.daemon","typeVersion":1,"parameters":{}}]}`)
	j := h.waitStatus(h.runAs(editor, "env", nil), domain.JobCompleted)
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
	if h.job(j.ID).TraceID == "" {
		t.Fatal("trace")
	}
	// No endpoint for a policy means failed_precondition, which the AI nodes handle (I28).
	h.mu.Lock()
	h.tenantCfg.AI.LLMEndpoints = []config.LLMEndpoint{{Name: "cloud", Class: config.EndpointExternal}}
	h.mu.Unlock()
	env := &nodeEnv{e: h.engine, store: h.store, job: j, nodeID: "n"}
	if _, err := env.LLM(context.Background(), domain.Asset{ID: asset.ID, AIProcessing: domain.AILocalOnly}); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("local_only with only external endpoints: %v", err)
	}
}
