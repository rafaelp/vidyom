package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/api/uploads"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

type ingestFixture struct {
	w       *world
	h       http.Handler
	root    string
	outside string
}

func newIngestFixture(t *testing.T) *ingestFixture {
	t.Helper()
	w := newWorld(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "ingest")
	outside := filepath.Join(base, "private")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(root, "class.mp4"):     "video",
		filepath.Join(outside, "secret.mp4"): "other",
	} {
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	w.workflows.specs = map[string]api.WorkflowDetail{
		"ingest-original": {WorkflowSummary: api.WorkflowSummary{
			ID: "ingest-original",
			Inputs: map[string]wfdef.InputSpec{
				"file":  {Type: wfdef.InputFile, Required: true},
				"title": {Type: wfdef.InputString},
				"extra": {Type: wfdef.InputObject},
			},
		}},
	}
	return &ingestFixture{
		w: w, root: root, outside: outside,
		h: w.serve(func(d *Deps) { d.IngestRoots = []string{root} }),
	}
}

func (f *ingestFixture) run(inputs string) request {
	return request{
		method: "POST", path: "/api/v1/workflows/ingest-original/runs",
		token: tokenEditor, tenant: tenantA, key: keyFor(inputs),
		body: `{"inputs":` + inputs + `}`,
	}
}

func fileInput(t *testing.T, path string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"file": path})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *ingestFixture) submittedInputs(t *testing.T) map[string]any {
	t.Helper()
	got := f.w.lastCall(t)
	if !strings.HasPrefix(got.Name, "workflows.Run") {
		t.Fatalf("last call = %s", got.Name)
	}
	var inputs map[string]any
	if err := json.Unmarshal(got.Args.(api.RunWorkflowRequest).Inputs, &inputs); err != nil {
		t.Fatal(err)
	}
	return inputs
}

func (f *ingestFixture) notRun(t *testing.T) {
	t.Helper()
	for _, name := range f.w.callNames() {
		if strings.HasPrefix(name, "workflows.Run") {
			t.Fatalf("the workflow ran: %v", f.w.callNames())
		}
	}
}

func TestServerPathInsideRootIsAcceptedAndCanonical(t *testing.T) {
	f := newIngestFixture(t)
	path := filepath.Join(f.root, "class.mp4")
	for _, ref := range []string{"server:" + path, path} { // bare form stays accepted (R20)
		f.w.resetCalls()
		wantStatus(t, do(f.h, f.run(fileInput(t, ref))), http.StatusAccepted)
		if got := f.submittedInputs(t)["file"]; got != "server:"+path {
			t.Fatalf("%q canonicalized to %v", ref, got)
		}
	}
}

// §15.6: `..` and a symlink out of the roots are rejected.
func TestServerPathEscapes(t *testing.T) {
	f := newIngestFixture(t)
	link := filepath.Join(f.root, "link.mp4")
	if err := os.Symlink(filepath.Join(f.outside, "secret.mp4"), link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(f.root, "dir")
	if err := os.Symlink(f.outside, dirLink); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		ref    string
		status int
		code   errs.Code
	}{
		{"absolute outside", "server:" + filepath.Join(f.outside, "secret.mp4"), 403, errs.CodeForbidden},
		{"bare outside", filepath.Join(f.outside, "secret.mp4"), 403, errs.CodeForbidden},
		{"dot dot leaving the root", "server:" + f.root + "/../private/secret.mp4", 403, errs.CodeForbidden},
		{"dot dot staying inside", "server:" + f.root + "/../ingest/class.mp4", 400, errs.CodeInvalidArgument},
		{"symlink to a file outside", "server:" + link, 403, errs.CodeForbidden},
		{"symlink to a directory outside", "server:" + filepath.Join(dirLink, "secret.mp4"), 403, errs.CodeForbidden},
		{"relative", "server:class.mp4", 400, errs.CodeInvalidArgument},
		{"system file", "server:/etc/passwd", 403, errs.CodeForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f.w.resetCalls()
			wantError(t, do(f.h, f.run(fileInput(t, c.ref))), c.status, c.code)
			f.notRun(t)
		})
	}
}

// The forbidden answer must not reveal whether a path outside the roots exists.
func TestNoExistenceOracleOutsideRoots(t *testing.T) {
	f := newIngestFixture(t)
	exists := do(f.h, f.run(fileInput(t, "server:"+filepath.Join(f.outside, "secret.mp4"))))
	missing := do(f.h, f.run(fileInput(t, "server:"+filepath.Join(f.outside, "nothing-here.mp4"))))
	wantError(t, exists, http.StatusForbidden, errs.CodeForbidden)
	wantError(t, missing, http.StatusForbidden, errs.CodeForbidden)

	// Inside the roots, not found is fine to say.
	inside := do(f.h, f.run(fileInput(t, "server:"+filepath.Join(f.root, "nothing-here.mp4"))))
	wantError(t, inside, http.StatusNotFound, errs.CodeNotFound)
}

func TestNoRootsRefusesEveryServerPath(t *testing.T) {
	f := newIngestFixture(t)
	h := f.w.serve() // no IngestRoots
	wantError(t, do(h, f.run(fileInput(t, "server:"+filepath.Join(f.root, "class.mp4")))), http.StatusForbidden, errs.CodeForbidden)
	f.notRun(t)
}

func TestScratchIsInternal(t *testing.T) {
	f := newIngestFixture(t)
	const scratch = "scratch://01J0000000000000000000000Z/node/file.mp4"
	wantError(t, do(f.h, f.run(fileInput(t, scratch))), http.StatusBadRequest, errs.CodeInvalidArgument)

	// Through an input not declared as a file, too.
	wantError(t, do(f.h, f.run(`{"file":"storage://master/a","title":"`+scratch+`"}`)), http.StatusBadRequest, errs.CodeInvalidArgument)
	wantError(t, do(f.h, f.run(`{"file":"storage://master/a","extra":{"nested":["`+scratch+`"]}}`)), http.StatusBadRequest, errs.CodeInvalidArgument)
	f.notRun(t)
}

func TestReferencesInUntypedInputsAreChecked(t *testing.T) {
	f := newIngestFixture(t)
	// A declared string that would feed a node's source cannot bypass the roots.
	wantError(t, do(f.h, f.run(`{"file":"storage://master/a","title":"server:/etc/passwd"}`)), http.StatusForbidden, errs.CodeForbidden)
	wantError(t, do(f.h, f.run(`{"file":"storage://master/a","undeclared":"server:/etc/passwd"}`)), http.StatusForbidden, errs.CodeForbidden)
	wantError(t, do(f.h, f.run(`{"file":"storage://master/a","extra":{"deep":{"er":["server:/etc/passwd"]}}}`)), http.StatusForbidden, errs.CodeForbidden)
	f.notRun(t)

	// Prose that only resembles a reference is left alone, and numbers survive.
	f.w.resetCalls()
	inputs := `{"file":"storage://master/a","title":"server: the waiter at the restaurant","big":12345678901234567890}`
	wantStatus(t, do(f.h, f.run(inputs)), http.StatusAccepted)
	raw := string(f.w.lastCall(t).Args.(api.RunWorkflowRequest).Inputs)
	if !strings.Contains(raw, "server: the waiter") || !strings.Contains(raw, "12345678901234567890") {
		t.Fatalf("inputs changed: %s", raw)
	}
}

func TestStorageAndImportPassThrough(t *testing.T) {
	f := newIngestFixture(t)
	wantStatus(t, do(f.h, f.run(`{"file":"storage://master/originals/ORI-001/v1/master.mp4"}`)), http.StatusAccepted)
	if got := f.submittedInputs(t)["file"]; got != "storage://master/originals/ORI-001/v1/master.mp4" {
		t.Fatalf("file = %v", got)
	}
	wantStatus(t, do(f.h, f.run(`{"file":"import:youtube/abc"}`)), http.StatusAccepted)
	// A bad key cannot escape the role.
	wantError(t, do(f.h, f.run(`{"file":"storage://master/../archive/x"}`)), http.StatusBadRequest, errs.CodeInvalidArgument)
	// A file input must be a reference.
	wantError(t, do(f.h, f.run(`{"file":{"path":"/x"}}`)), http.StatusBadRequest, errs.CodeInvalidArgument)
	// Inputs must be an object.
	wantError(t, do(f.h, f.run(`["/x"]`)), http.StatusBadRequest, errs.CodeInvalidArgument)
}

func (f *ingestFixture) completedUpload(t *testing.T, s service.Scope, content string) domain.Upload {
	t.Helper()
	ctx := context.Background()
	u, err := f.w.uploadsSvc.Create(ctx, s, api.CreateUploadRequest{
		Filename: "class.mp4", SizeBytes: int64(len(content)), SHA256: sumHex(content),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.w.uploadsSvc.AppendChunk(ctx, s, idRef(string(u.ID)), uploads.Chunk{Body: strings.NewReader(content)}); err != nil {
		t.Fatal(err)
	}
	u, err = f.w.uploadsSvc.Complete(ctx, s, idRef(string(u.ID)))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUploadReferences(t *testing.T) {
	f := newIngestFixture(t)
	scope := service.Scope{Tenant: tenantA, Principal: domain.Principal{Email: "editor@example.com"}}
	done := f.completedUpload(t, scope, "video")

	// Rewritten to the ULID, which names the spool file a worker can open (I131, I183).
	wantStatus(t, do(f.h, f.run(fileInput(t, "upload:"+string(done.ID)))), http.StatusAccepted)
	if got := f.submittedInputs(t)["file"]; got != "upload:"+string(done.ULID) {
		t.Fatalf("file = %v", got)
	}

	// Not complete: a job must not start.
	pending, err := f.w.uploadsSvc.Create(context.Background(), scope, api.CreateUploadRequest{Filename: "b.mp4", SizeBytes: 5, SHA256: sumHex("video")})
	if err != nil {
		t.Fatal(err)
	}
	f.w.resetCalls()
	wantError(t, do(f.h, f.run(fileInput(t, "upload:"+string(pending.ID)))), http.StatusConflict, errs.CodeFailedPrecondition)
	f.notRun(t)

	// Another tenant's upload does not resolve here.
	foreign := f.completedUpload(t, service.Scope{Tenant: tenantB}, "other")
	wantError(t, do(f.h, f.run(fileInput(t, "upload:"+string(foreign.ULID)))), http.StatusNotFound, errs.CodeNotFound)
	f.notRun(t)

	// A friendly ID of another prefix is no upload reference.
	wantError(t, do(f.h, f.run(fileInput(t, "upload:ORI-001"))), http.StatusBadRequest, errs.CodeInvalidArgument)
}

func TestInlineDefinitionsAreScanned(t *testing.T) {
	f := newIngestFixture(t)
	inline := func(def string) request {
		return request{method: "POST", path: "/api/v1/workflows/runs", token: tokenEditor, tenant: tenantA, key: keyFor(def), body: `{"definition":` + def + `}`}
	}
	bad := []string{
		`{"id":"x","version":"1.0.0","nodes":[{"id":"a","type":"storage.get","typeVersion":1,"parameters":{"source":"server:/etc/passwd"}}]}`,
		`{"id":"x","version":"1.0.0","nodes":[{"id":"a","type":"storage.get","typeVersion":1,"parameters":{"nested":{"source":"scratch://01J0000000000000000000000Z/n/f"}}}]}`,
		`{"id":"x","version":"1.0.0","inputs":{"file":{"type":"file","default":"server:/etc/passwd"}},"nodes":[{"id":"a","type":"flow.wait","typeVersion":1}]}`,
		`{"id":"x","version":"1.0.0","inputs":{"o":{"type":"object","default":{"p":"server:/etc/passwd"}}},"nodes":[{"id":"a","type":"flow.wait","typeVersion":1}]}`,
	}
	for _, def := range bad {
		rec := do(f.h, inline(def))
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", def, rec.Code)
		}
		f.notRun(t)
	}

	// A reference through ${...} is left to the engine and the worker's second layer.
	good := `{"id":"x","version":"1.0.0","inputs":{"file":{"type":"file"}},"nodes":[{"id":"a","type":"storage.get","typeVersion":1,"parameters":{"source":"${inputs.file}"}}]}`
	wantStatus(t, do(f.h, inline(good)), http.StatusAccepted)
	got := f.w.lastCall(t).Args.(api.RunWorkflowRequest)
	if string(got.Definition) != good {
		t.Fatalf("the definition was rewritten; its hash must cover the client's bytes: %s", got.Definition)
	}

	// Its declared file inputs follow the same rule.
	withInput := func(ref string) request {
		r := inline(good)
		r.body = `{"definition":` + good + `,"inputs":` + fileInput(t, ref) + `}`
		r.key = keyFor(ref)
		return r
	}
	f.w.resetCalls()
	wantError(t, do(f.h, withInput("server:/etc/passwd")), http.StatusForbidden, errs.CodeForbidden)
	f.notRun(t)
	wantStatus(t, do(f.h, withInput(filepath.Join(f.root, "class.mp4"))), http.StatusAccepted)

	// Not a workflow at all.
	rec := do(f.h, request{method: "POST", path: "/api/v1/workflows/runs", token: tokenEditor, tenant: tenantA, key: "k9", body: `{"definition":"text"}`})
	wantError(t, rec, http.StatusUnprocessableEntity, errs.CodeValidationFailed)
}

func TestNodeRunParameters(t *testing.T) {
	f := newIngestFixture(t)
	run := func(params string) request {
		return request{method: "POST", path: "/api/v1/nodes/storage.get/runs", token: tokenEditor, tenant: tenantA, key: keyFor(params), body: `{"parameters":` + params + `}`}
	}
	wantError(t, do(f.h, run(`{"source":"/etc/passwd"}`)), http.StatusForbidden, errs.CodeForbidden)
	wantError(t, do(f.h, run(`{"source":"scratch://01J0000000000000000000000Z/n/f"}`)), http.StatusBadRequest, errs.CodeInvalidArgument)

	path := filepath.Join(f.root, "class.mp4")
	wantStatus(t, do(f.h, run(`{"source":"`+path+`","allowArchive":false}`)), http.StatusAccepted)
	got := f.w.lastCall(t).Args.(api.RunNodeRequest)
	if !strings.Contains(string(got.Parameters), "server:"+path) {
		t.Fatalf("parameters = %s", got.Parameters)
	}
}

func TestWorkflowLookupErrorStopsTheRun(t *testing.T) {
	f := newIngestFixture(t)
	f.w.err = errs.NotFound("workflow")
	rec := do(f.h, f.run(`{"file":"storage://master/a"}`))
	wantError(t, rec, http.StatusNotFound, errs.CodeNotFound)
}

func TestLexicalRootCheckHandlesPrefixNames(t *testing.T) {
	s := &server{d: Deps{IngestRoots: []string{"/data/ingest"}}}
	cases := map[string]bool{
		"/data/ingest/a.mp4":      true,
		"/data/ingest":            true,
		"/data/ingest-other/a":    false,
		"/data/ingest/../other/a": false,
		"/data":                   false,
	}
	for path, want := range cases {
		if got := s.lexicallyInsideRoots(path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}
