package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/apiclient/apitest"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func uploadFixture(t *testing.T, name string, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*13 + i/509)
	}
	return fileWith(t, name, data), data
}

func runInputs(t *testing.T, call apitest.Call) map[string]any {
	t.Helper()
	var req api.RunWorkflowRequest
	if err := json.Unmarshal(call.Body, &req); err != nil {
		t.Fatal(err)
	}
	var inputs map[string]any
	if err := json.Unmarshal(req.Inputs, &inputs); err != nil {
		t.Fatalf("inputs: %v: %s", err, req.Inputs)
	}
	return inputs
}

// R20: the path of `original import` is a client file.
func TestOriginalImportUploadsThenIngests(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	uploads := h.srv.EnableUploads(4096)
	path, data := uploadFixture(t, "yoga-class.mp4", 4096*3+7)

	out := h.mustRun("original", "import", path, "--type", "yoga_class", "--title", "Hatha Yoga Class", "--workflow", "ingest-original")
	assertContains(t, out, "UPL-001", "JOB-001")

	if !bytes.Equal(uploads.Data("UPL-001"), data) {
		t.Fatal("the server does not hold the file")
	}
	if uploads.Completed() != 1 {
		t.Fatal("the upload must be completed before ingest starts")
	}
	call, ok := h.srv.Last(api.RouteWorkflowsRun)
	if !ok || !strings.HasSuffix(call.Path, "/workflows/ingest-original/runs") {
		t.Fatalf("run = %+v", call)
	}
	inputs := runInputs(t, call)
	if inputs["file"] != "upload:UPL-001" || inputs["title"] != "Hatha Yoga Class" || inputs["category"] != "yoga_class" {
		t.Fatalf("inputs = %v", inputs)
	}

	// Run only after complete, and complete only after the bytes.
	order := []string{}
	for _, c := range h.srv.Calls() {
		order = append(order, c.Route)
	}
	joined := strings.Join(order, " ")
	if strings.Index(joined, api.RouteUploadsComplete) > strings.Index(joined, api.RouteWorkflowsRun) {
		t.Fatalf("order = %v", order)
	}

	entries, _ := os.ReadDir(filepath.Join(h.dir, "uploads"))
	if len(entries) != 0 {
		t.Fatalf("the checkpoint must be gone after the run was accepted: %v", entries)
	}
}

func TestOriginalImportDefaultsWorkflowAndTitle(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.srv.EnableUploads(4096)
	path, _ := uploadFixture(t, "yoga-class.mp4", 100)
	h.mustRun("original", "import", path)
	call, ok := h.srv.Last(api.RouteWorkflowsRun)
	if !ok || !strings.Contains(call.Path, "ingest-original") {
		t.Fatalf("run = %+v", call)
	}
	if inputs := runInputs(t, call); inputs["title"] != "yoga-class" {
		t.Fatalf("the default title comes from the file name: %v", inputs)
	}
}

func TestOriginalImportJSONHasBothRecords(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.srv.EnableUploads(4096)
	path, _ := uploadFixture(t, "class.mp4", 100)
	out := decodeJSON(t, h.mustRun("original", "import", path, "--json"))
	upload, _ := out["upload"].(map[string]any)
	job, _ := out["job"].(map[string]any)
	if upload["id"] != "UPL-001" || job["job_id"] != "JOB-001" {
		t.Fatalf("out = %v", out)
	}
}

// R20: --server-path sends a reference and uploads nothing.
func TestOriginalImportServerPath(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	uploads := h.srv.EnableUploads(4096)
	h.mustRun("original", "import", "/data/class.mp4", "--server-path", "--title", "T")
	call, _ := h.srv.Last(api.RouteWorkflowsRun)
	if inputs := runInputs(t, call); inputs["file"] != "server:/data/class.mp4" {
		t.Fatalf("inputs = %v", inputs)
	}
	if uploads.Created() != 0 {
		t.Fatal("nothing may be uploaded")
	}

	if code := h.run("original", "import", "class.mp4", "--server-path"); code != 2 {
		t.Fatalf("a relative server path: exit %d", code)
	}
}

func TestOriginalImportTreatsPathAsClientFileEvenWithAURIShape(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	uploads := h.srv.EnableUploads(4096)
	if code := h.run("original", "import", "server:/data/class.mp4"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "cannot read the file")
	if uploads.Created() != 0 || len(h.srv.Calls(api.RouteWorkflowsRun)) != 0 {
		t.Fatal("it is a missing local file, not a server reference (R20)")
	}
}

func TestUploadChecksTheWorkflowBeforeAnyBytesMove(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	uploads := h.srv.EnableUploads(4096)
	h.srv.Error(api.RouteWorkflowsGet, errs.NotFound("workflow"))
	path, _ := uploadFixture(t, "class.mp4", 100)
	if code := h.run("original", "import", path, "--workflow", "missing"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "workflow not found")
	if uploads.Created() != 0 {
		t.Fatal("no transfer may start against a missing workflow")
	}
}

func TestUploadChecksInputsBeforeAnyBytesMove(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	uploads := h.srv.EnableUploads(4096)
	path, _ := uploadFixture(t, "class.mp4", 100)
	if code := h.run("original", "import", path, "--input", "count=many"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if uploads.Created() != 0 {
		t.Fatal("no transfer may start with bad inputs")
	}
}

func TestAssetUploadWithoutWorkflow(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.srv.EnableUploads(4096)
	path, _ := uploadFixture(t, "class.mp4", 100)
	out := h.mustRun("asset", "upload", path)
	assertContains(t, out, "upload:UPL-001", "SHA-256 verified")
	if len(h.srv.Calls(api.RouteWorkflowsRun)) != 0 {
		t.Fatal("no workflow was asked for")
	}
	entries, _ := os.ReadDir(filepath.Join(h.dir, "uploads"))
	if len(entries) != 0 {
		t.Fatal("the checkpoint must be gone")
	}
	if code := h.run("asset", "upload", path, "--title", "T"); code != 2 {
		t.Fatalf("metadata without a workflow: exit %d", code)
	}
	if code := h.run("asset", "upload", filepath.Join(h.dir, "missing.mp4")); code != 1 {
		t.Fatalf("missing file: exit %d", code)
	}
}

func TestAssetUploadWithWorkflowAndServerFlag(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.srv.EnableUploads(4096)
	path, _ := uploadFixture(t, "class.mp4", 100)
	h.mustRun("server", "add", "production", "--socket", h.socket)
	h.mustRun("asset", "upload", path, "--server", "production", "--tenant", "acme", "--workflow", "ingest-original")
	call, _ := h.srv.Last(api.RouteWorkflowsRun)
	if inputs := runInputs(t, call); inputs["file"] != "upload:UPL-001" {
		t.Fatalf("inputs = %v", inputs)
	}
}

func TestUploadRetriesAreVisibleAndSucceed(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	uploads := h.srv.EnableUploads(4096)
	uploads.FailNextChunk(apitest.DropConnection())
	uploads.FailNextChunk(apitest.Status(errs.CodeUnavailable))
	path, data := uploadFixture(t, "class.mp4", 4096*2)
	h.mustRun("asset", "upload", path)
	if !bytes.Equal(uploads.Data("UPL-001"), data) {
		t.Fatal("the server does not hold the file")
	}
	assertContains(t, h.stderr.String(), "retrying")
}

func TestInterruptedUploadSaysItCanResume(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	uploads := h.srv.EnableUploads(4096)
	ctx, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	uploads.OnChunk(func(int64) { interrupt() })
	path, _ := uploadFixture(t, "class.mp4", 4096*4)
	if code := h.runContext(ctx, "asset", "upload", path); code != 130 {
		t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
	}
	assertContains(t, h.stderr.String(), "run the same command again")
	entries, _ := os.ReadDir(filepath.Join(h.dir, "uploads"))
	if len(entries) != 1 {
		t.Fatalf("the checkpoint must stay: %v", entries)
	}
}

// Real processes: SIGKILL during the transfer, then resume with the same command.
func TestKilledProcessResumesWithoutResendingOrDuplicating(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	const chunk = 64 << 10
	uploads := h.srv.EnableUploads(chunk)
	path, data := uploadFixture(t, "class.mp4", chunk*40)

	killAt := int64(chunk * 10)
	var (
		mu     sync.Mutex
		victim *exec.Cmd
		killed atomic.Bool
	)
	uploads.OnChunk(func(received int64) {
		mu.Lock()
		defer mu.Unlock()
		if victim != nil && received >= killAt && !killed.Load() {
			killed.Store(true)
			_ = victim.Process.Kill()
		}
	})

	command := []string{"original", "import", path, "--title", "Killed", "--json"}
	first := h.helper(command...)
	mu.Lock()
	victim = first
	err := first.Start()
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err == nil {
		t.Fatal("the process finished before it was killed; the file is too small")
	}
	if !killed.Load() {
		t.Fatal("the process died on its own")
	}
	record, ok := uploads.Record("UPL-001")
	if !ok || record.ReceivedBytes < killAt || record.ReceivedBytes >= int64(len(data)) {
		t.Fatalf("server state after the kill: %+v", record)
	}
	if n := len(h.srv.Calls(api.RouteWorkflowsRun)); n != 0 {
		t.Fatalf("the workflow started before the transfer finished: %d", n)
	}

	var stdout, stderr bytes.Buffer
	second := h.helper(command...)
	second.Stdout, second.Stderr = &stdout, &stderr
	if err := second.Run(); err != nil {
		t.Fatalf("resume failed: %v\n%s", err, stderr.String())
	}
	if uploads.Created() != 1 {
		t.Fatalf("created %d uploads; the resume must reuse the first", uploads.Created())
	}
	if !bytes.Equal(uploads.Data("UPL-001"), data) {
		t.Fatal("the server does not hold the file")
	}
	if accepted := uploads.TotalAccepted(); accepted != int64(len(data)) {
		t.Fatalf("accepted %d bytes for a %d byte file: chunks were sent twice", accepted, len(data))
	}
	sum := sha256.Sum256(data)
	if done, _ := uploads.Record("UPL-001"); done.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("wrong digest declared")
	}
	if n := len(h.srv.Calls(api.RouteWorkflowsRun)); n != 1 {
		t.Fatalf("workflow started %d times", n)
	}
	out := decodeJSON(t, stdout.String())
	if job, _ := out["job"].(map[string]any); job["job_id"] != "JOB-001" {
		t.Fatalf("out = %v", out)
	}
	entries, _ := os.ReadDir(filepath.Join(h.dir, "uploads"))
	if len(entries) != 0 {
		t.Fatalf("the checkpoint must be gone: %v", entries)
	}
}

// A kill after the run was accepted but before the client saw the answer must not
// start a second job: the key is derived from the upload (I170).
func TestKillBetweenRunAndCheckpointRemovalReusesTheKey(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.srv.EnableUploads(4096)
	path, _ := uploadFixture(t, "class.mp4", 4096*2)

	command := []string{"original", "import", path, "--title", "Killed"}
	first := h.helper(command...)
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	h.srv.Handle(api.RouteWorkflowsRun, func(w http.ResponseWriter, _ *http.Request) {
		<-started
		once.Do(func() {
			_ = first.Process.Kill()
			close(release)
		})
		apitest.WriteJSON(w, 202, sampleRun())
	})
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	close(started)
	_ = first.Wait()
	select {
	case <-release:
	case <-time.After(30 * time.Second):
		t.Fatal("the run was never reached")
	}

	second := h.helper(command...)
	if out, err := second.CombinedOutput(); err != nil {
		t.Fatalf("resume failed: %v\n%s", err, out)
	}
	calls := h.srv.Calls(api.RouteWorkflowsRun)
	if len(calls) != 2 {
		t.Fatalf("runs = %d", len(calls))
	}
	one, two := calls[0].Header.Get(api.HeaderIdempotencyKey), calls[1].Header.Get(api.HeaderIdempotencyKey)
	if one == "" || one != two {
		t.Fatalf("the retried run must carry the same key: %q, %q", one, two)
	}
}

func TestKillBeforeCreateIsSaved(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	uploads := h.srv.EnableUploads(4096)
	path, data := uploadFixture(t, "class.mp4", 4096*2)

	// The create is answered only after the client process is gone, so it never learns
	// the ID. The next run must find the same upload through the saved key.
	create := h.srv.Handler(api.RouteUploadsCreate)
	first := h.helper("asset", "upload", path)
	var once sync.Once
	killed := make(chan struct{})
	started := make(chan struct{})
	h.srv.Handle(api.RouteUploadsCreate, func(w http.ResponseWriter, r *http.Request) {
		<-started
		once.Do(func() {
			_ = first.Process.Kill()
			close(killed)
		})
		create(w, r)
	})
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	close(started)
	_ = first.Wait()
	<-killed

	second := h.helper("asset", "upload", path)
	if out, err := second.CombinedOutput(); err != nil {
		t.Fatalf("resume failed: %v\n%s", err, out)
	}
	if uploads.Created() != 1 {
		t.Fatalf("created %d uploads", uploads.Created())
	}
	if !bytes.Equal(uploads.Data("UPL-001"), data) {
		t.Fatal("the server does not hold the file")
	}
}

// helper builds a real CLI process: this test binary, re-executed.
func (h *harness) helper(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(),
		"VIDYOM_CLI_HELPER=1",
		"VIDYOM_CONFIG_DIR="+h.dir,
		"VIDYOM_SOCKET="+h.socket,
	)
	return cmd
}

func TestWorkflowInputCoercion(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()

	h.mustRun("workflow", "run", "ingest-original",
		"--input", "title=A, with a comma",
		"--input", "count=3.5", "--input", "flag=true",
		"--input", `list=[1,"a"]`, "--input", `object={"k":"v"}`,
		"--input", "file=upload:UPL-001", "--input", "undeclared=7")
	call, _ := h.srv.Last(api.RouteWorkflowsRun)
	inputs := runInputs(t, call)
	if inputs["title"] != "A, with a comma" || inputs["count"] != 3.5 || inputs["flag"] != true ||
		inputs["file"] != "upload:UPL-001" || inputs["undeclared"] != "7" {
		t.Fatalf("inputs = %v", inputs)
	}
	if list, _ := inputs["list"].([]any); len(list) != 2 {
		t.Fatalf("list = %v", inputs["list"])
	}
	if object, _ := inputs["object"].(map[string]any); object["k"] != "v" {
		t.Fatalf("object = %v", inputs["object"])
	}

	h.srv.Reset()
	bad := []string{
		"file=scratch://01JC0000000000000000000000/n/f", // internal, never from a client
		"file=relative/path.mp4",
		"count=many",
		"flag=perhaps",
		"list=notjson",
		`list={"a":1}`,
		`object=[1]`,
		"novalue",
		"=value",
	}
	for _, input := range bad {
		if code := h.run("workflow", "run", "ingest-original", "--input", input); code != 2 {
			t.Errorf("%q: exit %d", input, code)
		}
	}
	if len(h.srv.Calls(api.RouteWorkflowsRun)) != 0 {
		t.Fatal("no run may start")
	}
}

func TestFileInputAcceptsTheFourForms(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	cases := map[string]string{
		"/data/class.mp4":                  "server:/data/class.mp4", // compatibility, R20
		"server:/data/class.mp4":           "server:/data/class.mp4",
		"upload:UPL-001":                   "upload:UPL-001",
		"storage://master/originals/x.mp4": "storage://master/originals/x.mp4",
		"import:youtube-main/abc":          "import:youtube-main/abc",
	}
	for input, want := range cases {
		h.mustRun("workflow", "run", "ingest-original", "--input", "file="+input)
		call, _ := h.srv.Last(api.RouteWorkflowsRun)
		if got := runInputs(t, call)["file"]; got != want {
			t.Errorf("%s: got %v, want %s", input, got, want)
		}
	}
}

func TestWorkflowFileFromDisk(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	definition := `{"id":"local-flow","inputs":{"count":{"type":"number"}}}`
	path := fileWith(t, "flow.json", []byte(definition))
	h.mustRun("workflow", "run", path, "--input", "count=3")
	call, _ := h.srv.Last(api.RouteWorkflowsRunBody)
	var req api.RunWorkflowRequest
	if err := json.Unmarshal(call.Body, &req); err != nil {
		t.Fatal(err)
	}
	if string(req.Definition) != definition {
		t.Fatalf("definition = %s", req.Definition)
	}
	if len(h.srv.Calls(api.RouteWorkflowsGet)) != 0 {
		t.Fatal("a file needs no lookup")
	}

	broken := fileWith(t, "broken.json", []byte("{not json"))
	if code := h.run("workflow", "run", broken); code != 1 {
		t.Fatalf("broken file: exit %d", code)
	}
	if code := h.run("workflow", "validate", filepath.Join(h.dir, "absent.json")); code != 1 {
		t.Fatalf("missing file: exit %d", code)
	}
}

func TestClipCreateWithAnotherWorkflowRunsIt(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.mustRun("clip", "create", "--source", "ORI-001", "--prompt", "story", "--workflow", "my-clip", "--mode", "short", "--target-duration", "60", "--destination", "a,b")
	if len(h.srv.Calls(api.RouteClipsCreate)) != 0 {
		t.Fatal("POST /clips only runs create-clip")
	}
	call, ok := h.srv.Last(api.RouteWorkflowsRun)
	if !ok || !strings.Contains(call.Path, "my-clip") {
		t.Fatalf("run = %+v", call)
	}
	inputs := runInputs(t, call)
	if inputs["source_id"] != "ORI-001" || inputs["prompt"] != "story" || inputs["mode"] != "short" || inputs["target_duration_seconds"] != 60.0 {
		t.Fatalf("inputs = %v", inputs)
	}
	if code := h.run("clip", "create", "--source", "ORI-001"); code != 2 {
		t.Fatalf("no prompt: exit %d", code)
	}
	h.mustRun("clip", "create", "--source", "ORI-001", "--prompt", "story", "--mode", "best_moments")
	clip, _ := h.srv.Last(api.RouteClipsCreate)
	if clip.JSONBody(t)["mode"] != "best_moments" {
		t.Fatalf("body = %s", clip.Body)
	}
}

func TestNodeRunParameters(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.mustRun("node", "run", "ai.transcribe", "--params-json", `{"language":"pt","beam":5}`, "--param", "language=en")
	call, _ := h.srv.Last(api.RouteNodesRun)
	parameters, _ := call.JSONBody(t)["parameters"].(map[string]any)
	if parameters["language"] != "en" || parameters["beam"] != 5.0 {
		t.Fatalf("parameters = %v", parameters)
	}
	h.mustRun("node", "run", "flow.noop")
	call, _ = h.srv.Last(api.RouteNodesRun)
	if params, ok := call.JSONBody(t)["parameters"].(map[string]any); !ok || len(params) != 0 {
		t.Fatalf("parameters must be an object: %s", call.Body)
	}
	for _, flags := range [][]string{{"--params-json", "[1]"}, {"--param", "novalue"}, {"--asset", "bad"}} {
		if code := h.run(append([]string{"node", "run", "x"}, flags...)...); code != 2 {
			t.Errorf("%v: exit %d", flags, code)
		}
	}
}
