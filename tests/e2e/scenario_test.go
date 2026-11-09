//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

const (
	jobTimeout = 4 * time.Minute

	fixtureSeconds = 120
	tenant         = "acme"
	publicDB       = "public/acme/public.db"
	tenantDB       = "data/tenants/acme/tenant.db"
	accessLog      = "storage/access.log"
)

// scenario holds what the steps share. Each step reads what earlier steps left, so a
// failed step stops the rest (I286).
type scenario struct {
	stack stack
	tmp   string
	pin   string
	init  initResult

	admin, editor, publisher, viewer *client
	bin                              string

	httpClient *http.Client

	fixture   string
	originals []string
	clip      string
	markText  string
	keepText  string
	// parentClip and grandchild are the clip and clip of a clip of the lineage step.
	parentClip, grandchild string
}

func TestScenario(t *testing.T) {
	var s stack
	switch target := os.Getenv("VIDYOM_E2E_TARGET"); target {
	case "", "native":
		s = newNative(t)
	case "compose":
		s = newCompose(t)
	default:
		t.Fatalf("unknown VIDYOM_E2E_TARGET %q", target)
	}
	tmp, err := os.MkdirTemp("", "vdmclient")
	if err != nil {
		t.Fatal(err)
	}
	sc := &scenario{stack: s, tmp: tmp}
	t.Cleanup(func() {
		if t.Failed() {
			s.Dump(t)
		}
		s.Down(t)
		if os.Getenv("VIDYOM_E2E_KEEP") == "" {
			_ = os.RemoveAll(tmp)
		}
	})

	steps := []struct {
		name string
		run  func(*testing.T)
	}{
		{"01-init-and-start", sc.step01},
		{"02-pairing-and-users", sc.step02},
		{"03-import-original", sc.step03},
		{"04-search", sc.step04},
		{"05-clip-selection-gate", sc.step05},
		{"06-approve-and-render", sc.step06},
		{"07-publication-gate", sc.step07},
		{"08-publish-to-mock", sc.step08},
		{"09-public-export", sc.step09},
		{"10-library-search", sc.step10},
		{"11-withdraw", sc.step11},
		{"12-audit-chain", sc.step12},
		{"13-resilience", sc.step13},
		{"x1-hooks-with-a-real-plugin", sc.stepHooks},
		{"x2-clip-of-a-clip-and-a-late-mark", sc.stepLineageMark},
		{"x3-tenant-isolation", sc.stepIsolation},
		{"x4-dead-log-sink", sc.stepDeadSink},
		{"x5-budget-and-rejection", sc.stepBudget},
		{"x6-failure-paths", sc.stepFailurePaths},
		{"x7-job-control", sc.stepJobControl},
		{"x8-other-workflows", sc.stepOtherWorkflows},
		{"x9-metrics", sc.stepMetrics},
		{"x10-unix-socket", sc.stepUnixSocket},
		{"x11-observability-profile", sc.stepObservabilityProfile},
		{"x12-unauthorized-clients", sc.stepAuthorization},
		{"x13-secrets", sc.stepSecrets},
		{"x14-storage-role-moves", sc.stepSwitchStorage},
	}
	for _, step := range steps {
		if !t.Run(step.name, step.run) {
			t.FailNow()
		}
	}
}

// ---- Step 1 ----

func (sc *scenario) step01(t *testing.T) {
	sc.stack.Up(t)
	sc.init = sc.stack.Init(t)
	if sc.init.PairingCode == "" || sc.init.TLSFingerprint == "" || sc.init.Tenant != tenant {
		t.Fatalf("vidyomd init result: %+v", sc.init)
	}
	sc.pin = sc.init.TLSFingerprint
	sc.httpClient = pinnedClient(sc.pin)

	// D13: no master key configured means a non-zero exit and no listener.
	for _, variant := range []string{"no-provider", "missing-key"} {
		code, output := sc.stack.Refused(t, variant)
		if code == 0 {
			t.Fatalf("%s: the daemon started without a usable master key:\n%s", variant, output)
		}
		if _, err := pinnedClient(sc.pin).Get(sc.stack.HTTPSURL() + "/api/v1/health"); err == nil {
			t.Fatalf("%s: something is listening although the daemon refused to start", variant)
		}
		if !strings.Contains(output, "master key") {
			t.Fatalf("%s: exit %d for another reason:\n%s", variant, code, output)
		}
		t.Logf("%s: exit %d", variant, code)
	}

	sc.stack.StartMockLLM(t)
	sc.stack.StartDaemon(t)
	sc.waitHealthy(t)
	sc.stack.StartAIWorker(t)
	sc.stack.StartWorkers(t)
	waitFor(t, 30*time.Second, "health with the AI worker", func() (bool, string) {
		code, body := httpCall(t, sc.httpClient, http.MethodGet, sc.stack.HTTPSURL()+"/api/v1/health", nil, nil)
		var h struct {
			Status     string `json:"status"`
			Sealing    string `json:"sealing"`
			Components map[string]struct {
				Status string `json:"status"`
			} `json:"components"`
		}
		_ = json.Unmarshal(body, &h)
		return code == http.StatusOK && h.Status == "ok" && h.Sealing == "file" &&
			h.Components["redis"].Status == "ok" && h.Components["ai_worker"].Status == "ok", string(body)
	})
}

func (sc *scenario) waitHealthy(t *testing.T) {
	t.Helper()
	waitFor(t, 30*time.Second, "/api/v1/health", func() (bool, string) {
		resp, err := pinnedClient(sc.pin).Get(sc.stack.HTTPSURL() + "/api/v1/health")
		if err != nil {
			return false, err.Error()
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK, resp.Status
	})
}

// ---- Step 2 ----

func (sc *scenario) newClient(t *testing.T, name string) *client {
	t.Helper()
	return newClient(t, name, sc.tmp, sc.cliBinary(t), sc.stack.HTTPSURL(), sc.pin)
}

func (sc *scenario) cliBinary(t *testing.T) string {
	t.Helper()
	if sc.bin != "" {
		return sc.bin
	}
	sc.bin = filepath.Join(sc.tmp, "vidyom")
	must(t, append(os.Environ(), "CGO_ENABLED=0"), repoRoot(t), "go", "build", "-tags", "vidyom_test_keyring", "-o", sc.bin, "./cmd/vidyom")
	return sc.bin
}

func (sc *scenario) pair(t *testing.T, c *client, code string) {
	t.Helper()
	c.ok(t, "server", "add", "main", "--url", sc.stack.HTTPSURL(), "--fingerprint", sc.pin)
	c.ok(t, "server", "pair", "main", "--code", code)
	c.ok(t, "tenant", "use", tenant)
}

func (sc *scenario) pairUser(t *testing.T, name, email string) *client {
	t.Helper()
	var code api.PairingCodeResponse
	sc.admin.jsonOut(t, &code, "device", "pairing-code", "create", "--user", email)
	if code.Code == "" {
		t.Fatalf("no pairing code for %s", email)
	}
	if code.TLSFingerprint != sc.pin {
		t.Errorf("the pairing code carries pin %q, want %q", code.TLSFingerprint, sc.pin)
	}
	c := sc.newClient(t, name)
	sc.pair(t, c, code.Code)
	return c
}

func (sc *scenario) step02(t *testing.T) {
	sc.admin = sc.newClient(t, "admin")
	sc.pair(t, sc.admin, sc.init.PairingCode)

	// A one-time code does not work twice.
	again := sc.newClient(t, "again")
	again.ok(t, "server", "add", "main", "--url", sc.stack.HTTPSURL(), "--fingerprint", sc.pin)
	if _, _, code := again.try(t, "server", "pair", "main", "--code", sc.init.PairingCode); code == 0 {
		t.Fatal("the bootstrap pairing code worked twice")
	}

	for user, role := range map[string]string{
		"editor@example.com":    "editor",
		"publisher@example.com": "publisher",
		"viewer@example.com":    "viewer",
	} {
		sc.admin.ok(t, "user", "add", user, "--role", role)
	}
	sc.editor = sc.pairUser(t, "editor", "editor@example.com")
	sc.publisher = sc.pairUser(t, "publisher", "publisher@example.com")
	sc.viewer = sc.pairUser(t, "viewer", "viewer@example.com")

	// D32: no Vidyom-Tenant, no tenant-scoped answer. A bad tenant looks like nothing.
	token := sc.admin.credential(t)
	auth := map[string]string{"Authorization": "Bearer " + token}
	assets := sc.stack.HTTPSURL() + "/api/v1/assets"
	status, body := httpCall(t, sc.httpClient, http.MethodGet, assets, auth, nil)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "tenant_required") {
		t.Fatalf("no tenant header: %d %s", status, body)
	}
	status, _ = httpCall(t, sc.httpClient, http.MethodGet, assets, map[string]string{"Authorization": auth["Authorization"], api.HeaderTenant: "nowhere"}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown tenant: %d", status)
	}
	status, _ = httpCall(t, sc.httpClient, http.MethodGet, assets, map[string]string{api.HeaderTenant: tenant}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", status)
	}
	status, _ = httpCall(t, sc.httpClient, http.MethodGet, assets, map[string]string{"Authorization": auth["Authorization"], api.HeaderTenant: tenant}, nil)
	if status != http.StatusOK {
		t.Fatalf("with credential and tenant: %d", status)
	}

	// A device credential works until it is revoked.
	spare := sc.pairUser(t, "spare", "viewer@example.com")
	spare.ok(t, "asset", "list")
	var devices api.List[domain.Device]
	sc.admin.jsonOut(t, &devices, "security", "sessions", "--all")
	var spareID string
	for _, device := range devices.Items {
		if device.Email == "viewer@example.com" {
			spareID = string(device.ID)
		}
	}
	if spareID == "" {
		t.Fatalf("no device for the spare client in %+v", devices.Items)
	}
	sc.admin.ok(t, "security", "revoke", spareID)
	if _, stderr, code := spare.try(t, "asset", "list"); code == 0 {
		t.Fatal("a revoked device credential still works")
	} else {
		t.Logf("revoked: exit %d: %s", code, firstLine(stderr))
	}
}

// ---- Step 3 ----

func (sc *scenario) generateFixture(t *testing.T, name string, seconds int, size string, rate int) string {
	t.Helper()
	path := filepath.Join(sc.tmp, name)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	must(t, os.Environ(), sc.tmp, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%s:rate=%d:duration=%d", size, rate, seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=330:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", path)
	return path
}

func (sc *scenario) step03(t *testing.T) {
	sc.fixture = sc.generateFixture(t, "fixture.mp4", fixtureSeconds, "1280x720", 30)
	size := fileSize(t, sc.fixture)

	// A wrong hash is refused when the upload completes.
	sc.assertWrongHashRefused(t)

	// Kill the client during the transfer, then run the same command again.
	importArgs := []string{"--json", "original", "import", sc.fixture,
		"--type", "yoga_class", "--title", "Hatha Yoga Class", "--speaker", "Maria Example",
		"--workflow", "ingest-original", "--chunk-bytes", strconv.Itoa(1 << 20)}
	uploadID, offset := sc.killDuringUpload(t, importArgs, size)
	t.Logf("killed the client with %s at offset %d of %d", uploadID, offset, size)

	stdout := sc.editor.ok(t, importArgs[0:]...)
	var imported struct {
		Upload *domain.Upload           `json:"upload"`
		Job    *api.RunWorkflowResponse `json:"job"`
	}
	if err := json.Unmarshal([]byte(stdout), &imported); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if imported.Upload == nil || string(imported.Upload.ID) != uploadID {
		t.Fatalf("the second run started a new upload: %+v, want %s", imported.Upload, uploadID)
	}
	if imported.Upload.Status != domain.UploadComplete || imported.Upload.SizeBytes != size {
		t.Fatalf("upload after resume: %+v", imported.Upload)
	}
	if imported.Job == nil || imported.Job.JobID == "" {
		t.Fatalf("no job: %s", stdout)
	}
	jobID := string(imported.Job.JobID)

	// Readiness is a consolidated condition: sample while the job runs, and at every
	// sample ready_for_clipping must mean all the flags hold.
	sawIngesting := false
	waitFor(t, jobTimeout, "ingest-original", func() (bool, string) {
		var job api.JobDetail
		sc.editor.jsonOut(t, &job, "job", "status", jobID)
		if job.AssetID != "" {
			var asset domain.Asset
			sc.editor.jsonOut(t, &asset, "asset", "show", string(job.AssetID))
			if asset.Status == domain.AssetIngesting {
				sawIngesting = true
			}
			if asset.Status == domain.AssetReadyForClipping && !(asset.MasterAvailable && asset.BackupVerified && asset.ProxyAvailable && asset.TimelineValidated && asset.Transcript != nil && asset.Transcript.Indexed) {
				t.Fatalf("ready_for_clipping before the condition holds: %+v", asset)
			}
		}
		failOn(t, job)
		return job.Status == domain.JobCompleted, nodeSummary(job)
	})
	if !sawIngesting {
		t.Error("never saw the asset in ingesting")
	}

	var job api.JobDetail
	sc.editor.jsonOut(t, &job, "job", "status", jobID)
	ori := string(job.AssetID)
	if ori != "ORI-001" {
		t.Fatalf("asset %q, want ORI-001", ori)
	}
	sc.originals = append(sc.originals, ori)
	var asset domain.Asset
	sc.editor.jsonOut(t, &asset, "asset", "show", ori)
	if asset.Status != domain.AssetReadyForClipping || asset.Title != "Hatha Yoga Class" || asset.Category != "yoga_class" || asset.Speaker != "Maria Example" {
		t.Fatalf("asset after ingest: %+v", asset)
	}

	// Raw in archive, master in master, proxy in proxy, each in its own namespace.
	checkFile := func(label string, f *domain.MediaFile, role domain.StorageRole) string {
		t.Helper()
		if f == nil {
			t.Fatalf("%s: no file", label)
		}
		if f.Storage != role || !f.Verified || f.SHA256 == "" {
			t.Fatalf("%s: %+v", label, f)
		}
		rel := filepath.Join("storage", string(role), tenant, f.Key)
		if !sc.stack.Exists(t, rel) {
			t.Fatalf("%s: %s is not on disk", label, rel)
		}
		return rel
	}
	raw := checkFile("source_original", asset.SourceOriginal, domain.RoleArchive)
	master := checkFile("working_master", asset.WorkingMaster, domain.RoleMaster)
	proxy := checkFile("proxy", asset.Proxy, domain.RoleProxy)
	if asset.SourceOriginal.SHA256 != imported.Upload.SHA256 {
		t.Errorf("the raw file's SHA-256 %s differs from the upload's %s", asset.SourceOriginal.SHA256, imported.Upload.SHA256)
	}

	// R10: the same timeline, within max(100 ms, one frame).
	tolerance := max(0.1, 1.0/30)
	reference := ffprobeDuration(t, sc.fixture)
	for label, rel := range map[string]string{"raw": raw, "master": master, "proxy": proxy} {
		got := ffprobeDuration(t, sc.stack.Fetch(t, rel))
		if diff := math.Abs(got - reference); diff > tolerance {
			t.Errorf("%s duration %.3f differs from %.3f by more than %.3f", label, got, reference, tolerance)
		}
	}

	// D56: the ASR audio comes from the master.
	audio := nodeOf(t, job, "audio")
	var params struct {
		Source string `json:"source"`
	}
	mustUnmarshal(t, audio.Parameters, &params)
	if !strings.HasPrefix(params.Source, "storage://master/") {
		t.Errorf("audio.extract read %q, not the master", params.Source)
	}

	// The readiness event comes after backup and index.
	events := sc.events(t, sc.editor)
	readyAt, ok := eventTime(events, "asset.ready_for_clipping", ori)
	if !ok {
		t.Fatalf("no asset.ready_for_clipping for %s", ori)
	}
	for _, earlier := range []string{"storage.backup.verified", "asset.indexed"} {
		at, ok := eventTime(events, earlier, ori)
		if !ok || !at.Before(readyAt) {
			t.Errorf("%s at %v, ready at %v", earlier, at, readyAt)
		}
	}
}

func (sc *scenario) assertWrongHashRefused(t *testing.T) {
	t.Helper()
	token := sc.editor.credential(t)
	headers := func(key string) map[string]string {
		return map[string]string{
			"Authorization": "Bearer " + token, api.HeaderTenant: tenant,
			"Idempotency-Key": key, "Content-Type": "application/json",
		}
	}
	base := sc.stack.HTTPSURL() + "/api/v1"
	body, _ := json.Marshal(map[string]any{"filename": "x.bin", "size_bytes": 4, "sha256": strings.Repeat("0", 64)})
	status, data := httpCall(t, sc.httpClient, http.MethodPost, base+"/assets/uploads", headers("hash-1"), body)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("create upload: %d %s", status, data)
	}
	var upload domain.Upload
	mustUnmarshal(t, data, &upload)
	chunk := headers("hash-2")
	chunk["Upload-Offset"] = "0"
	chunk["Content-Type"] = "application/octet-stream"
	status, data = httpCall(t, sc.httpClient, http.MethodPatch, base+"/uploads/"+string(upload.ID), chunk, []byte("data"))
	if status/100 != 2 {
		t.Fatalf("chunk: %d %s", status, data)
	}
	status, data = httpCall(t, sc.httpClient, http.MethodPost, base+"/uploads/"+string(upload.ID)+"/complete", headers("hash-3"), nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("complete with a wrong hash: %d %s", status, data)
	}
}

// killDuringUpload starts the import and kills the client once bytes have reached the
// server. It finds the upload through the client's own checkpoint file.
func (sc *scenario) killDuringUpload(t *testing.T, args []string, size int64) (string, int64) {
	t.Helper()
	cmd := exec.Command(sc.editor.bin, args...)
	cmd.Env = sc.editor.env()
	cmd.Dir = sc.editor.dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()

	token := sc.editor.credential(t)
	headers := map[string]string{"Authorization": "Bearer " + token, api.HeaderTenant: tenant}
	checkpoints := filepath.Join(sc.editor.dir, "uploads")
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			done <- err
			t.Fatalf("the client finished before it could be killed: %v; make the fixture larger", err)
		default:
		}
		id := uploadIDFromCheckpoints(checkpoints)
		if id != "" {
			status, data := httpCall(t, sc.httpClient, http.MethodGet, sc.stack.HTTPSURL()+"/api/v1/uploads/"+id, headers, nil)
			if status == http.StatusOK {
				var upload domain.Upload
				mustUnmarshal(t, data, &upload)
				if upload.ReceivedBytes > 0 && upload.ReceivedBytes < size {
					if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
						t.Fatal(err)
					}
					return id, upload.ReceivedBytes
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the upload never made progress")
	return "", 0
}

var uploadRef = regexp.MustCompile(`"upload_id"\s*:\s*"(UPL-[0-9]+)"`)

func uploadIDFromCheckpoints(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if match := uploadRef.FindSubmatch(data); match != nil {
			return string(match[1])
		}
	}
	return ""
}

// ---- Step 4 ----

func (sc *scenario) step04(t *testing.T) {
	// A second original, from a server path, so asset scoping has something to exclude.
	small := sc.generateFixture(t, "small.mp4", 40, "640x360", 25)
	serverPath := sc.stack.PlaceIngest(t, small)
	if _, stderr, code := sc.editor.try(t, "original", "import", "/etc/hosts", "--server-path", "--title", "Escape"); code == 0 {
		t.Fatal("a server path outside ingestRoots was accepted")
	} else {
		t.Logf("outside ingestRoots: %s", firstLine(stderr))
	}
	var second api.RunWorkflowResponse
	sc.editor.jsonOut(t, &second, "original", "import", serverPath, "--server-path", "--title", "Second class", "--type", "yoga_class")
	if second.JobID == "" {
		t.Fatal("no job for the second original")
	}
	sc.waitJob(t, sc.editor, string(second.JobID), domain.JobCompleted)
	sc.originals = append(sc.originals, "ORI-002")

	var all api.SearchResponse
	sc.viewer.jsonOut(t, &all, "original", "search", "conscious breathing")
	if len(all.Items) == 0 {
		t.Fatal("no hits")
	}
	seen := map[string]bool{}
	for _, hit := range all.Items {
		seen[string(hit.AssetID)] = true
		if hit.EndSeconds <= hit.StartSeconds {
			t.Errorf("a hit with no time span: %+v", hit)
		}
	}
	if !seen["ORI-001"] || !seen["ORI-002"] {
		t.Errorf("the tenant-wide search did not find both originals: %v", seen)
	}
	if all.Mode != "hybrid" || all.EmbeddingModel != "fake-hash-embed@1" {
		t.Errorf("mode %q, model %q", all.Mode, all.EmbeddingModel)
	}

	var scoped api.SearchResponse
	sc.viewer.jsonOut(t, &scoped, "original", "search", "conscious breathing", "--asset", "ORI-001")
	if len(scoped.Items) == 0 {
		t.Fatal("no hits scoped to ORI-001")
	}
	for _, hit := range scoped.Items {
		if hit.AssetID != "ORI-001" {
			t.Errorf("the asset filter let through %+v", hit)
		}
	}

	// Each index on its own: FTS5, then sqlite-vec.
	for _, mode := range []string{"lexical", "semantic"} {
		var result api.SearchResponse
		sc.viewer.jsonOut(t, &result, "original", "search", "conscious breathing", "--asset", "ORI-001", "--mode", mode)
		if result.Mode != mode || len(result.Items) == 0 {
			t.Errorf("mode %s: %+v", mode, result)
		}
	}
}

// ---- Step 5 ----

func (sc *scenario) step05(t *testing.T) {
	var started api.RunWorkflowResponse
	sc.editor.jsonOut(t, &started, "clip", "create", "--source", "ORI-001", "--prompt", "conscious breathing", "--destination", "mock-main")
	if started.JobID == "" {
		t.Fatal("clip create started no job")
	}
	jobID := string(started.JobID)

	// A client that follows the job and goes away changes nothing (§5.5, D51).
	attach := exec.Command(sc.editor.bin, "job", "attach", jobID)
	attach.Env = sc.editor.env()
	if err := attach.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := attach.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = attach.Wait()

	sc.waitJob(t, sc.editor, jobID, domain.JobWaitingApproval)
	var approval domain.Approval
	sc.editor.jsonOut(t, &approval, "approval", "show", "APR-001")
	if approval.Kind != domain.ApprovalClipSelection || approval.Status != domain.ApprovalPending || approval.AssetID != "ORI-001" {
		t.Fatalf("APR-001: %+v", approval)
	}
	var evidence struct {
		Excerpt    string           `json:"excerpt"`
		Confidence float64          `json:"confidence"`
		Segments   []domain.Segment `json:"segments"`
	}
	mustUnmarshal(t, approval.Evidence, &evidence)
	if evidence.Excerpt == "" || evidence.Confidence <= 0 || len(evidence.Segments) == 0 || evidence.Segments[0].EndSeconds <= evidence.Segments[0].StartSeconds {
		t.Fatalf("APR-001 evidence: %s", approval.Evidence)
	}
	if !strings.Contains(strings.ToLower(evidence.Excerpt), "conscious breathing") {
		t.Errorf("the excerpt does not hold the request: %q", evidence.Excerpt)
	}
	// §3.2 step 6: the reviewer can obtain something playable, over the API.
	if !strings.HasPrefix(approval.Preview, "storage://proxy/") {
		t.Errorf("the preview is %q, which no reviewer can open", approval.Preview)
	}
	output := filepath.Join(sc.tmp, "preview.mp4")
	sc.viewer.ok(t, "clip", "preview", "APR-001", "--output", output)
	want := evidence.Segments[0].EndSeconds - evidence.Segments[0].StartSeconds
	if got := ffprobeDuration(t, output); math.Abs(got-want) > 0.5 {
		t.Errorf("preview lasts %.3f, the candidate %.3f", got, want)
	}
}

// ---- Step 6 ----

func (sc *scenario) step06(t *testing.T) {
	// A viewer is refused, and refused as forbidden.
	sc.viewer.refused(t, "forbidden", "approval", "approve", "APR-001")

	// The editor widens the selection to most of the recording, so the clip has several
	// embedding windows and a sensitive mark on one leaves the others (step 9).
	sc.editor.ok(t, "approval", "approve", "APR-001", "--start", "0", "--end", "110")

	var approval domain.Approval
	sc.editor.jsonOut(t, &approval, "approval", "show", "APR-001")
	if approval.Status != domain.ApprovalApproved || approval.DecidedBy != "editor@example.com" {
		t.Fatalf("APR-001 after approval: %+v", approval)
	}
	waitFor(t, jobTimeout, "CLIP-001", func() (bool, string) {
		var assets api.List[domain.Asset]
		sc.editor.jsonOut(t, &assets, "clip", "list")
		for _, asset := range assets.Items {
			if asset.ID == "CLIP-001" && asset.MasterAvailable && asset.ProxyAvailable && asset.BackupVerified {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d clips", len(assets.Items))
	})
	sc.clip = "CLIP-001"

	var clip domain.Asset
	sc.editor.jsonOut(t, &clip, "asset", "show", sc.clip)
	if clip.Lineage == nil || clip.Lineage.SourceAssetID != "ORI-001" || clip.Lineage.RootAssetID != "ORI-001" || clip.Lineage.SelectionApprovalID != "APR-001" || len(clip.Lineage.Segments) != 1 {
		t.Fatalf("lineage: %+v", clip.Lineage)
	}
	if seg := clip.Lineage.Segments[0]; seg.StartSeconds != 0 || seg.EndSeconds != 110 {
		t.Errorf("lineage segment %+v, want the adjusted 0 to 110", seg)
	}
	if clip.Speaker != "Maria Example" {
		t.Errorf("the clip's speaker is %q, its source's is Maria Example", clip.Speaker)
	}
	for label, f := range map[string]struct {
		file *domain.MediaFile
		role domain.StorageRole
	}{
		"master": {clip.WorkingMaster, domain.RoleMaster},
		"backup": {clip.Backup, domain.RoleArchive},
		"proxy":  {clip.Proxy, domain.RoleProxy},
	} {
		if f.file == nil || f.file.Storage != f.role || !f.file.Verified {
			t.Fatalf("%s of the clip: %+v", label, f.file)
		}
		if !sc.stack.Exists(t, filepath.Join("storage", string(f.role), tenant, f.file.Key)) {
			t.Errorf("%s of the clip is not on disk: %s", label, f.file.Key)
		}
	}
	if got := ffprobeDuration(t, sc.stack.Fetch(t, filepath.Join("storage", "master", tenant, clip.WorkingMaster.Key))); math.Abs(got-110) > 0.2 {
		t.Errorf("the clip's master lasts %.3f, the selection 110", got)
	}

	// Render read only from master: no read of archive anywhere in the access log, and
	// the master was read (R13, R27).
	entries := sc.accessEntries(t)
	masterReads := 0
	for _, e := range entries {
		if e.Role == "archive" && e.Class == "read" {
			t.Errorf("read from archive: %+v", e)
		}
		if e.Role == "master" && e.Class == "read" && strings.Contains(e.Key, "ORI-001") {
			masterReads++
		}
	}
	if masterReads == 0 {
		t.Error("the access log shows no read of the master, so it proves nothing")
	}
	t.Logf("access log: %d entries, %d reads of the master of ORI-001, none of archive", len(entries), masterReads)

	// Once decided, the candidate no longer takes room in proxy.
	waitFor(t, 30*time.Second, "the preview's removal", func() (bool, string) {
		return !sc.stack.Exists(t, "storage/proxy/"+tenant+"/"+strings.TrimPrefix(approval.Preview, "storage://proxy/")), approval.Preview
	})
}

type accessEntry struct {
	Tenant string `json:"tenant"`
	Role   string `json:"role"`
	Op     string `json:"op"`
	Class  string `json:"class"`
	Key    string `json:"key"`
}

func (sc *scenario) accessEntries(t *testing.T) []accessEntry {
	t.Helper()
	data, err := os.ReadFile(sc.stack.Fetch(t, accessLog))
	if err != nil {
		t.Fatal(err)
	}
	var out []accessEntry
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var e accessEntry
		mustUnmarshal(t, line, &e)
		out = append(out, e)
	}
	return out
}

// ---- Step 7 ----

func (sc *scenario) step07(t *testing.T) {
	var approvals api.List[domain.Approval]
	waitFor(t, jobTimeout, "APR-002", func() (bool, string) {
		sc.editor.jsonOut(t, &approvals, "approval", "list", "--status", "pending")
		for _, a := range approvals.Items {
			if a.ID == "APR-002" {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%+v", approvals.Items)
	})

	// A sensitive mark on the last part of the clip, before anyone approves publication.
	// One sentence goes, the rest stays, and step 9 checks both.
	var transcript domain.Transcript
	sc.editor.jsonOut(t, &transcript, "asset", "transcript", sc.clip)
	if len(transcript.Segments) < 4 {
		t.Fatalf("the clip's transcript has %d segments", len(transcript.Segments))
	}
	last := transcript.Segments[len(transcript.Segments)-2]
	first := transcript.Segments[1]
	sc.markText = strings.TrimSpace(last.Text)
	sc.keepText = strings.TrimSpace(first.Text)
	sc.editor.ok(t, "asset", "sensitive", "add", sc.clip,
		"--start", strconv.FormatFloat(last.Start, 'f', 3, 64),
		"--end", strconv.FormatFloat(last.End, 'f', 3, 64),
		"--reason", "names")
	t.Logf("marked sensitive: %q", sc.markText)

	var approval struct {
		domain.Approval
		Notices []string `json:"notices"`
	}
	sc.editor.jsonOut(t, &approval, "approval", "show", "APR-002")
	if approval.Kind != domain.ApprovalPublication || string(approval.AssetID) != sc.clip {
		t.Fatalf("APR-002: %+v", approval)
	}
	if len(approval.Notices) == 0 || !strings.Contains(approval.Notices[0], "tenant's responsibility") {
		t.Errorf("no D29 notice on the publication approval: %+v", approval.Notices)
	}

	// D7's two levels: an editor approves the selection, never a publication.
	t.Logf("editor refused: %s", sc.editor.refused(t, "forbidden", "approval", "approve", "APR-002"))
	var still domain.Approval
	sc.publisher.jsonOut(t, &still, "approval", "show", "APR-002")
	if still.Status != domain.ApprovalPending {
		t.Fatalf("APR-002 after the refusal: %s", still.Status)
	}
	sc.publisher.ok(t, "approval", "approve", "APR-002")
	sc.publisher.jsonOut(t, &still, "approval", "show", "APR-002")
	if still.Status != domain.ApprovalApproved || still.DecidedBy != "publisher@example.com" {
		t.Fatalf("APR-002 after the publisher: %+v", still)
	}
}

// ---- Step 8 ----

type mockState struct {
	Videos map[string]struct {
		RemoteID   string `json:"remote_id"`
		Visibility string `json:"visibility"`
		Captions   map[string]struct {
			Format string `json:"format"`
			Text   string `json:"text"`
		} `json:"captions"`
	} `json:"videos"`
	Uploads int `json:"uploads"`
	Repeats int `json:"repeats"`
}

func (sc *scenario) mock(t *testing.T, destination string) mockState {
	t.Helper()
	data, err := os.ReadFile(sc.stack.Fetch(t, filepath.Join("mock", tenant, destination+".json")))
	if err != nil {
		t.Fatal(err)
	}
	var state mockState
	mustUnmarshal(t, data, &state)
	return state
}

// mockIfAny reads a destination's state, empty until its first upload.
func (sc *scenario) mockIfAny(t *testing.T, destination string) mockState {
	t.Helper()
	if !sc.stack.Exists(t, filepath.Join("mock", tenant, destination+".json")) {
		return mockState{}
	}
	return sc.mock(t, destination)
}

func (sc *scenario) publications(t *testing.T, c *client, asset string) []domain.Publication {
	t.Helper()
	var list api.List[domain.Publication]
	c.jsonOut(t, &list, "publish", "status", asset)
	return list.Items
}

func (sc *scenario) waitPublished(t *testing.T, destination string) domain.Publication {
	t.Helper()
	var found domain.Publication
	waitFor(t, jobTimeout, destination+" published", func() (bool, string) {
		for _, pub := range sc.publications(t, sc.publisher, sc.clip) {
			if pub.Destination == destination && pub.Status == domain.PubPublished {
				found = pub
				return true, ""
			}
		}
		return false, fmt.Sprintf("%+v", sc.publications(t, sc.publisher, sc.clip))
	})
	return found
}

func (sc *scenario) step08(t *testing.T) {
	pub := sc.waitPublished(t, "mock-main")
	if pub.ID != "PUB-001" || pub.RemoteID == "" || !strings.HasPrefix(pub.URL, "https://media.example.internal/watch/") {
		t.Fatalf("PUB-001 published without its evidence: %+v", pub)
	}
	state := sc.mock(t, "mock-main")
	if state.Uploads != 1 {
		t.Fatalf("uploads to mock-main: %d", state.Uploads)
	}
	if video, ok := state.Videos[pub.RemoteID]; !ok || video.Visibility != "public" {
		t.Fatalf("the destination does not have %s as public: %+v", pub.RemoteID, state.Videos)
	}

	// The same key means the same job, with nothing new at the destination.
	const key = "e2e-retry-key"
	var first, again api.RunWorkflowResponse
	sc.publisher.jsonOut(t, &first, "publish", sc.clip, "--to", "mock-main", "--idempotency-key", key)
	sc.publisher.jsonOut(t, &again, "publish", sc.clip, "--to", "mock-main", "--idempotency-key", key)
	if first.JobID == "" || first.JobID != again.JobID {
		t.Fatalf("the same key gave jobs %s and %s", first.JobID, again.JobID)
	}
	// That publication passes its own gate and adopts the record it already has.
	sc.approvePending(t, sc.publisher)
	sc.waitJob(t, sc.publisher, string(first.JobID), domain.JobCompleted)
	if state := sc.mock(t, "mock-main"); state.Uploads != 1 {
		t.Fatalf("a repeated publication uploaded again: %d uploads", state.Uploads)
	}
	if pubs := sc.publications(t, sc.publisher, sc.clip); len(pubs) != 1 {
		t.Fatalf("a repeated publication made a new record: %+v", pubs)
	}

	// An upload whose answer was lost is reconciled, not repeated (§15.5, D11).
	var flaky api.RunWorkflowResponse
	sc.publisher.jsonOut(t, &flaky, "publish", sc.clip, "--to", "mock-flaky")
	sc.approvePending(t, sc.publisher)
	pubFlaky := sc.waitPublished(t, "mock-flaky")
	if state := sc.mock(t, "mock-flaky"); state.Uploads != 1 {
		t.Fatalf("a lost answer made %d uploads", state.Uploads)
	}
	t.Logf("%s reconciled after an unknown outcome, one upload", pubFlaky.ID)
}

// approvePending approves whatever publication gate is waiting.
func (sc *scenario) approvePending(t *testing.T, c *client) string {
	t.Helper()
	var approved string
	waitFor(t, jobTimeout, "a pending publication approval", func() (bool, string) {
		var approvals api.List[domain.Approval]
		c.jsonOut(t, &approvals, "approval", "list", "--status", "pending", "--kind", "publication")
		if len(approvals.Items) == 0 {
			return false, "none"
		}
		approved = string(approvals.Items[0].ID)
		return true, ""
	})
	c.ok(t, "approval", "approve", approved)
	return approved
}

// ---- Step 9 ----

var ulidShaped = regexp.MustCompile(`[0-9A-HJKMNP-TV-Z]{26}`)

func (sc *scenario) step09(t *testing.T) {
	// The event, not a person, started the export.
	var job *domain.Job
	waitFor(t, jobTimeout, "update-public-library", func() (bool, string) {
		var jobs api.List[domain.Job]
		sc.admin.jsonOut(t, &jobs, "job", "list", "--workflow", "update-public-library")
		for i := range jobs.Items {
			if jobs.Items[i].Status == domain.JobCompleted {
				job = &jobs.Items[i]
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d jobs", len(jobs.Items))
	})
	if job.Trigger.Type != "event" {
		t.Errorf("update-public-library was started by %+v", job.Trigger)
	}
	events := sc.events(t, sc.admin)
	if _, ok := eventTime(events, "publication.completed", sc.clip); !ok {
		t.Error("no publication.completed")
	}
	if !hasEvent(events, "public_library.synced") {
		t.Error("no public_library.synced")
	}
	// A sync takes the vectors stored at indexing, instead of embedding again (I232).
	var detail api.JobDetail
	sc.admin.jsonOut(t, &detail, "job", "status", string(job.ID))
	for _, n := range detail.Nodes {
		if n.NodeType != "catalog.public.sync" {
			continue
		}
		var out struct {
			Embeddings string `json:"embeddings"`
		}
		mustUnmarshal(t, n.Output, &out)
		if out.Embeddings != "stored" {
			t.Errorf("catalog.public.sync took its vectors from %q", out.Embeddings)
		}
	}
	sc.checkPublicFile(t)
}

func (sc *scenario) checkPublicFile(t *testing.T) {
	t.Helper()
	waitFor(t, time.Minute, "the export to include both destinations", func() (bool, string) {
		count := strings.TrimSpace(sc.stack.SQL(t, publicDB, "select count(*) from destinations;"))
		return count == "2", count
	})
	local := sc.stack.Fetch(t, publicDB)
	raw, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}

	var clip, ori domain.Asset
	sc.admin.jsonOut(t, &clip, "asset", "show", sc.clip)
	sc.admin.jsonOut(t, &ori, "asset", "show", "ORI-001")
	forbidden := map[string]string{
		"storage URI":          "storage://",
		"master key":           clip.WorkingMaster.Key,
		"proxy key":            clip.Proxy.Key,
		"archive key":          clip.Backup.Key,
		"the clip's ULID":      string(clip.ULID),
		"the original's ULID":  string(ori.ULID),
		"the friendly ID":      "CLIP-001",
		"the source's ID":      "ORI-001",
		"the private title":    "Hatha Yoga Class",
		"the sensitive text":   sc.markText,
		"a local path":         "/storage/",
		"a job ID":             "JOB-0",
		"an approval ID":       "APR-0",
		"the destination name": "mock-main",
	}
	for label, value := range forbidden {
		if value != "" && bytes.Contains(raw, []byte(value)) {
			t.Errorf("public.db holds %s: %q", label, value)
		}
	}
	if match := ulidShaped.Find(raw); match != nil {
		t.Errorf("public.db holds something shaped like a ULID: %s", match)
	}
	if !bytes.Contains(raw, []byte(sc.keepText)) {
		t.Errorf("public.db lost the text that is not sensitive: %q", sc.keepText)
	}

	if got := strings.TrimSpace(sc.stack.SQL(t, publicDB, "select count(*) from videos;")); got != "1" {
		t.Errorf("videos in public.db: %s", got)
	}
	row := strings.TrimSpace(sc.stack.SQL(t, publicDB, "select kind, speaker from videos;"))
	if row != "clip|Maria Example" {
		t.Errorf("video row: %q", row)
	}
	embeddings, _ := strconv.Atoi(strings.TrimSpace(sc.stack.SQL(t, publicDB, "select count(*) from embeddings;")))
	if embeddings == 0 {
		t.Error("no embeddings exported; the mark should have removed only the windows it touches")
	}
	t.Logf("public.db: %d bytes, %d embeddings", len(raw), embeddings)

	// Public captions passed through the same filter.
	state := sc.mock(t, "mock-main")
	captions := 0
	for _, video := range state.Videos {
		for _, caption := range video.Captions {
			captions++
			if strings.Contains(caption.Text, sc.markText) {
				t.Errorf("the sensitive text reached the destination's captions: %q", caption.Text)
			}
		}
	}
	if captions == 0 {
		t.Error("no captions reached the destination, so this checks nothing")
	}
}

// ---- Step 10 ----

type librarySearch struct {
	Query    string `json:"query"`
	Mode     string `json:"mode"`
	Match    string `json:"match"`
	Total    int    `json:"total"`
	Semantic struct {
		Available bool   `json:"available"`
		Model     string `json:"model"`
		TestOnly  bool   `json:"test_only"`
		Reason    string `json:"reason"`
	} `json:"semantic"`
	Results []struct {
		PublicID string `json:"public_id"`
		Title    string `json:"title"`
		Kind     string `json:"kind"`
		URL      string `json:"url"`
		Moments  []struct {
			StartSeconds float64 `json:"start_seconds"`
		} `json:"moments"`
	} `json:"results"`
}

func (sc *scenario) librarySearch(t *testing.T, query string) (int, librarySearch) {
	t.Helper()
	code, body, err := plainGet(sc.stack.LibraryURL() + "/api/search?q=" + url.QueryEscape(query))
	if err != nil {
		t.Fatalf("library search: %v", err)
	}
	var out librarySearch
	if code == http.StatusOK {
		mustUnmarshal(t, body, &out)
	}
	return code, out
}

func (sc *scenario) step10(t *testing.T) {
	sc.stack.StartLibrary(t)
	check := func() {
		t.Helper()
		code, result := sc.librarySearch(t, "conscious breathing")
		if code != http.StatusOK || len(result.Results) != 1 {
			t.Fatalf("library search: %d %+v", code, result)
		}
		hit := result.Results[0]
		if hit.Kind != "clip" || !strings.Contains(strings.ToLower(hit.Title), "conscious breathing") || len(hit.Moments) == 0 {
			t.Fatalf("library hit: %+v", hit)
		}
		if result.Mode != "hybrid" || !result.Semantic.Available || result.Semantic.Model != "fake-hash-embed@1" || !result.Semantic.TestOnly {
			t.Fatalf("library mode %q, semantic %+v", result.Mode, result.Semantic)
		}
	}
	check()

	// Speaker search, which had no data before (§13.2).
	if code, result := sc.librarySearch(t, "Maria Example"); code != http.StatusOK || len(result.Results) != 1 {
		t.Errorf("search by speaker: %d %+v", code, result)
	}

	// The sensitive sentence is not findable in public.
	code, marked := sc.librarySearch(t, sc.markText)
	if code != http.StatusOK {
		t.Fatalf("search for the marked text: %d", code)
	}
	for _, r := range marked.Results {
		for _, m := range r.Moments {
			_ = m
		}
	}

	// D34, R44: with the daemon dead, the library answers just the same.
	sc.stack.KillDaemon(t)
	check()
	status, _, err := plainGet(sc.stack.LibraryURL() + "/")
	if err != nil || status != http.StatusOK {
		t.Fatalf("the library's home page with no daemon: %d %v", status, err)
	}
	sc.stack.StartDaemon(t)
	sc.waitHealthy(t)
}

// ---- Step 11 ----

func (sc *scenario) step11(t *testing.T) {
	var first api.RunWorkflowResponse
	sc.publisher.jsonOut(t, &first, "unpublish", sc.clip, "--all")
	sc.waitJob(t, sc.publisher, string(first.JobID), domain.JobCompleted)

	for _, pub := range sc.publications(t, sc.publisher, sc.clip) {
		if pub.Status != domain.PubWithdrawn || pub.WithdrawnAt == nil {
			t.Errorf("after withdrawal: %+v", pub)
		}
	}
	countWithdrawn := func() int {
		count := 0
		for _, e := range sc.events(t, sc.admin) {
			if e.Type == "publication.withdrawn" && e.AssetID == sc.clip {
				count++
			}
		}
		return count
	}
	if got := countWithdrawn(); got != 2 {
		t.Errorf("publication.withdrawn: %d, two destinations", got)
	}

	// The record leaves the public file, and the site stops serving it, with no restart.
	waitFor(t, time.Minute, "the record to leave public.db", func() (bool, string) {
		count := strings.TrimSpace(sc.stack.SQL(t, publicDB, "select count(*) from videos;"))
		return count == "0", count
	})
	raw, err := os.ReadFile(sc.stack.Fetch(t, publicDB))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(sc.keepText)) {
		t.Error("the withdrawn clip's transcript is still in public.db")
	}
	waitFor(t, 30*time.Second, "the library to drop it", func() (bool, string) {
		code, result := sc.librarySearch(t, "conscious breathing")
		return code == http.StatusOK && len(result.Results) == 0, fmt.Sprintf("%d %+v", code, result)
	})

	// Twice is a no-op: no new event, nothing at the destination.
	before := sc.mock(t, "mock-main")
	var second api.RunWorkflowResponse
	sc.publisher.jsonOut(t, &second, "unpublish", sc.clip, "--all")
	sc.waitJob(t, sc.publisher, string(second.JobID), domain.JobCompleted)
	if got := countWithdrawn(); got != 2 {
		t.Errorf("the second withdrawal emitted events: %d", got)
	}
	if after := sc.mock(t, "mock-main"); after.Uploads != before.Uploads {
		t.Errorf("the second withdrawal changed the destination: %+v", after)
	}
}

// ---- Step 12 ----

func (sc *scenario) step12(t *testing.T) {
	var verification domain.AuditVerification
	sc.admin.jsonOut(t, &verification, "security", "audit", "--verify")
	if !verification.OK || verification.Records == 0 {
		t.Fatalf("verify: %+v", verification)
	}
	t.Logf("chain intact: %d records", verification.Records)

	// As an attacker would: remove the triggers, then edit (I107).
	sc.stack.SQL(t, tenantDB, "DROP TRIGGER audit_log_no_update; UPDATE audit_log SET actor = 'somebody-else@example.com' WHERE seq = 3;")
	stdout, stderr, code := sc.admin.try(t, "security", "audit", "--verify")
	if code == 0 {
		t.Fatalf("a tampered chain verified: %s", stdout)
	}
	t.Logf("tampered: exit %d: %s %s", code, firstLine(stdout), firstLine(stderr))
}

// ---- Step 13 ----

func (sc *scenario) step13(t *testing.T) {
	t.Run("kill-the-daemon-during-a-transcode", sc.killDuringTranscode)
	t.Run("flushall-with-a-job-queued", sc.flushWithQueuedJob)
	t.Run("flushall-under-live-workers", sc.flushUnderLiveWorkers)
}

func (sc *scenario) killDuringTranscode(t *testing.T) {
	heavy := sc.generateFixture(t, "heavy.mp4", 240, "1920x1080", 30)
	var started struct {
		Job *api.RunWorkflowResponse `json:"job"`
	}
	sc.editor.jsonOut(t, &started, "original", "import", heavy, "--title", "Long class", "--type", "yoga_class")
	jobID := string(started.Job.JobID)

	var before api.JobDetail
	waitFor(t, jobTimeout, "the transcode to be running", func() (bool, string) {
		sc.editor.jsonOut(t, &before, "job", "status", jobID)
		failOn(t, before)
		for _, n := range before.Nodes {
			if n.NodeID == "compress" && n.Status == domain.NodeRunning {
				return true, ""
			}
		}
		return false, nodeSummary(before)
	})
	t.Logf("killing vidyomd with: %s", nodeSummary(before))
	completedBefore := map[string]int{}
	for _, n := range before.Nodes {
		if n.Status == domain.NodeCompleted {
			completedBefore[n.NodeID] = n.Attempt
		}
	}
	sc.stack.KillDaemon(t)
	time.Sleep(2 * time.Second)
	sc.stack.StartDaemon(t)
	sc.waitHealthy(t)

	after := sc.waitJob(t, sc.editor, jobID, domain.JobCompleted)
	for _, n := range after.Nodes {
		if n.Status != domain.NodeCompleted && n.Status != domain.NodeSkipped {
			t.Errorf("node %s ended %s", n.NodeID, n.Status)
		}
		if attempt, ok := completedBefore[n.NodeID]; ok && n.Attempt != attempt {
			t.Errorf("node %s was checkpointed at attempt %d and ran again: %d", n.NodeID, attempt, n.Attempt)
		}
		if n.NodeID == "compress" && n.Attempt != 1 {
			t.Errorf("the transcode survived the daemon's death in a worker, yet ran %d times", n.Attempt)
		}
	}

	// No file was written twice under one key: each node ran to completion once.
	var asset domain.Asset
	sc.editor.jsonOut(t, &asset, "asset", "show", string(after.AssetID))
	if asset.Status != domain.AssetReadyForClipping {
		t.Errorf("asset after the crash: %s", asset.Status)
	}
	writes := map[string]int{}
	for _, e := range sc.accessEntries(t) {
		if e.Class == "write" && strings.Contains(e.Key, string(after.AssetID)) {
			writes[e.Role+"/"+e.Key]++
		}
	}
	for key, count := range writes {
		if count != 1 {
			t.Errorf("%s written %d times", key, count)
		}
	}
	t.Logf("resumed from checkpoint: %d nodes, %d distinct writes", len(after.Nodes), len(writes))
}

func (sc *scenario) flushWithQueuedJob(t *testing.T) {
	sc.stack.StopWorkers(t)
	small := sc.generateFixture(t, "small.mp4", 40, "640x360", 25)
	var started struct {
		Job *api.RunWorkflowResponse `json:"job"`
	}
	sc.editor.jsonOut(t, &started, "original", "import", small, "--title", "Queued class", "--type", "yoga_class", "--idempotency-key", "flush-queued")
	jobID := string(started.Job.JobID)
	waitFor(t, time.Minute, "a node to be queued", func() (bool, string) {
		var job api.JobDetail
		sc.editor.jsonOut(t, &job, "job", "status", jobID)
		for _, n := range job.Nodes {
			if n.Status == domain.NodeQueued {
				return true, ""
			}
		}
		return false, nodeSummary(job)
	})
	if got := strings.TrimSpace(sc.stack.Redis(t, "FLUSHALL")); got != "OK" {
		t.Fatalf("FLUSHALL: %s", got)
	}
	if got := strings.TrimSpace(sc.stack.Redis(t, "DBSIZE")); got != "0" {
		t.Logf("DBSIZE right after FLUSHALL: %s", got)
	}
	sc.stack.StartWorkers(t)
	done := sc.waitJob(t, sc.editor, jobID, domain.JobCompleted)
	for _, n := range done.Nodes {
		if n.Status == domain.NodeCompleted && n.Attempt != 1 {
			t.Errorf("node %s needed %d attempts after a lost pointer", n.NodeID, n.Attempt)
		}
	}
}

// flushUnderLiveWorkers empties Redis with workers reading: their consumer groups vanish
// under them, and the next job must still run.
func (sc *scenario) flushUnderLiveWorkers(t *testing.T) {
	sc.stack.Redis(t, "FLUSHALL")
	small := sc.generateFixture(t, "small.mp4", 40, "640x360", 25)
	var started struct {
		Job *api.RunWorkflowResponse `json:"job"`
	}
	sc.editor.jsonOut(t, &started, "original", "import", small, "--title", "After flush", "--type", "yoga_class", "--idempotency-key", "flush-live")
	sc.waitJob(t, sc.editor, string(started.Job.JobID), domain.JobCompleted)
}

// ---- Extended: §15.7 hooks with the sample plugin ----

func (sc *scenario) stepHooks(t *testing.T) {
	built := filepath.Join(sc.tmp, "sample-notifier")
	if err := os.MkdirAll(filepath.Join(built, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	sc.buildPlugin(t, built)
	repo := repoRoot(t)
	manifest, err := os.ReadFile(filepath.Join(repo, "plugins", "sample-notifier", "vidyom-plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(built, "vidyom-plugin.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	onServer := sc.stack.InstallPlugin(t, built)
	sc.admin.ok(t, "plugin", "install", onServer)
	var record domain.PluginRecord
	sc.admin.jsonOut(t, &record, "plugin", "inspect", "sample-notifier")
	t.Logf("installed %s, isolation %q", record.SHA256[:12], record.Isolation)

	definition := func(id string) string {
		return `{
  "id": "` + id + `", "name": "Hook check", "version": "1.0.0",
  "inputs": {"title": {"type": "string"}},
  "hooks": {
    "before": [{"id": "check", "plugin": "sample-notifier", "action": "check-source", "onFailure": "abort"}],
    "finish": [{"id": "finish", "plugin": "sample-notifier", "action": "record-finish", "onFailure": "continue"}]
  },
  "nodes": [{"id": "wait", "type": "flow.wait", "typeVersion": 1, "parameters": {"seconds": 1}}],
  "connections": {}
}`
	}
	file := filepath.Join(sc.tmp, "hook-check.json")
	if err := os.WriteFile(file, []byte(definition("hook-check")), 0o644); err != nil {
		t.Fatal(err)
	}

	// Nothing is granted by installing it: the tenant's grant comes next.
	var refused api.RunWorkflowResponse
	sc.admin.jsonOut(t, &refused, "workflow", "run", file, "--input", "title=Ungranted", "--idempotency-key", "hook-ungranted")
	ended := sc.waitTerminal(t, sc.admin, string(refused.JobID))
	if ended.Status != domain.JobFailed {
		t.Fatalf("a plugin with no grant let a job pass: %s", nodeSummary(ended))
	}

	sc.stack.WriteTenantConfig(t, tenant, "plugins.json", `{
  "plugins": {
    "sample-notifier": {
      "enabled": true,
      "permissions": { "filesystem": ["job-temp"], "events": ["publication.completed"] }
    }
  }
}`)
	time.Sleep(time.Second)

	// §15.7: a before hook rejects an invalid source, and the error is audited.
	var blocked api.RunWorkflowResponse
	sc.admin.jsonOut(t, &blocked, "workflow", "run", file, "--idempotency-key", "hook-blocked")
	failed := sc.waitTerminal(t, sc.admin, string(blocked.JobID))
	if failed.Status != domain.JobFailed {
		t.Fatalf("a before hook did not stop the job: %s", nodeSummary(failed))
	}
	if failed.Error == nil || failed.Error.Code != "hook_blocked" {
		t.Fatalf("job error: %+v", failed.Error)
	}
	var audit api.List[domain.AuditRecord]
	sc.admin.jsonOut(t, &audit, "security", "audit", "--action", "hook.blocked", "--all")
	if len(audit.Items) == 0 {
		t.Error("the blocked hook left no audit record")
	}

	// A good source passes, and finish runs on both outcomes.
	var passed api.RunWorkflowResponse
	sc.admin.jsonOut(t, &passed, "workflow", "run", file, "--input", "title=Hatha Yoga Class", "--idempotency-key", "hook-passed")
	sc.waitJob(t, sc.admin, string(passed.JobID), domain.JobCompleted)

	stats := sc.pluginStats(t)
	if stats["finished"] < 2 {
		t.Errorf("finish hook runs recorded by the plugin: %v, want a success and a failure", stats)
	}
}

func (sc *scenario) buildPlugin(t *testing.T, dir string) {
	t.Helper()
	env := append(os.Environ(), "CGO_ENABLED=0")
	if sc.stack.Name() == "compose" {
		env = append(env, "GOOS=linux", "GOARCH="+dockerArch(t))
	}
	must(t, env, repoRoot(t), "go", "build", "-trimpath", "-o", filepath.Join(dir, "bin", "sample-notifier"), "./plugins/sample-notifier")
}

// pluginStats asks the plugin itself, through a global hook declared for the purpose.
func (sc *scenario) pluginStats(t *testing.T) map[string]int {
	t.Helper()
	sc.stack.WriteTenantConfig(t, tenant, "hooks.json", `{
  "workflow": { "finish": [{ "id": "stats", "plugin": "sample-notifier", "action": "stats", "onFailure": "continue" }] },
  "node": {}
}`)
	time.Sleep(time.Second)
	var result struct {
		Allow      bool           `json:"allow"`
		Parameters map[string]int `json:"parameters"`
	}
	sc.admin.jsonOut(t, &result, "hook", "test", "stats")
	return result.Parameters
}

// ---- Extended: D30 through a clip of a clip ----

func (sc *scenario) stepLineageMark(t *testing.T) {
	// Clip of the original.
	var parent api.RunWorkflowResponse
	sc.editor.jsonOut(t, &parent, "clip", "create", "--source", "ORI-001", "--prompt", "conscious breathing")
	sc.waitJob(t, sc.editor, string(parent.JobID), domain.JobWaitingApproval)
	selection := sc.pendingApproval(t, sc.editor, string(parent.JobID))
	sc.editor.ok(t, "approval", "approve", selection, "--start", "0", "--end", "100")
	done := sc.waitJob(t, sc.editor, string(parent.JobID), domain.JobCompleted)
	var outputs struct {
		ClipID string `json:"clip_id"`
	}
	mustUnmarshal(t, done.Outputs, &outputs)
	clipOfOriginal := outputs.ClipID
	if clipOfOriginal == "" {
		t.Fatalf("create-clip outputs: %s", done.Outputs)
	}
	sc.parentClip = clipOfOriginal

	// Clip of that clip, published.
	var child api.RunWorkflowResponse
	sc.editor.jsonOut(t, &child, "clip", "create", "--source", clipOfOriginal, "--prompt", "conscious breathing", "--destination", "mock-main")
	sc.waitJob(t, sc.editor, string(child.JobID), domain.JobWaitingApproval)
	selection = sc.pendingApproval(t, sc.editor, string(child.JobID))
	sc.editor.ok(t, "approval", "approve", selection, "--start", "0", "--end", "90")
	waitFor(t, jobTimeout, "the publication gate of the clip of a clip", func() (bool, string) {
		var approvals api.List[domain.Approval]
		sc.publisher.jsonOut(t, &approvals, "approval", "list", "--status", "pending", "--kind", "publication")
		return len(approvals.Items) > 0, fmt.Sprintf("%+v", approvals.Items)
	})
	sc.approvePending(t, sc.publisher)
	finished := sc.waitJob(t, sc.editor, string(child.JobID), domain.JobCompleted)
	mustUnmarshal(t, finished.Outputs, &outputs)
	grandchild := outputs.ClipID
	if grandchild == "" {
		t.Fatalf("create-clip outputs: %s", finished.Outputs)
	}
	sc.grandchild = grandchild

	var transcript domain.Transcript
	sc.editor.jsonOut(t, &transcript, "asset", "transcript", grandchild)
	if len(transcript.Segments) < 3 {
		t.Fatalf("transcript of %s: %d segments", grandchild, len(transcript.Segments))
	}
	target := strings.TrimSpace(transcript.Segments[1].Text)
	waitFor(t, jobTimeout, "the clip of a clip in public.db", func() (bool, string) {
		count := strings.TrimSpace(sc.stack.SQL(t, publicDB, "select count(*) from videos;"))
		return count == "1", count
	})
	waitFor(t, time.Minute, "the text in public.db", func() (bool, string) {
		raw, err := os.ReadFile(sc.stack.Fetch(t, publicDB))
		return err == nil && bytes.Contains(raw, []byte(target)), target
	})

	// A mark on the grandparent, added after both cuts.
	var source domain.Transcript
	sc.editor.jsonOut(t, &source, "asset", "transcript", "ORI-001")
	var segment *domain.TranscriptSegment
	for i := range source.Segments {
		if strings.TrimSpace(source.Segments[i].Text) == target {
			segment = &source.Segments[i]
		}
	}
	if segment == nil {
		t.Fatalf("%q is not in the original's transcript", target)
	}
	sc.editor.ok(t, "asset", "sensitive", "add", "ORI-001",
		"--start", strconv.FormatFloat(segment.Start, 'f', 3, 64),
		"--end", strconv.FormatFloat(segment.End, 'f', 3, 64),
		"--reason", "health")

	// D30: it leaves public.db, without anyone republishing.
	waitFor(t, time.Minute, "the grandparent's mark to reach public.db", func() (bool, string) {
		raw, err := os.ReadFile(sc.stack.Fetch(t, publicDB))
		return err == nil && !bytes.Contains(raw, []byte(target)), target
	})
	if code, result := sc.librarySearch(t, target); code == http.StatusOK {
		t.Logf("library search for the marked sentence: %d results, match %q", len(result.Results), result.Match)
	}
	// A viewer reads the clip's transcript redacted, through the same lineage.
	var redacted domain.Transcript
	sc.viewer.jsonOut(t, &redacted, "asset", "transcript", grandchild)
	raw, _ := json.Marshal(redacted)
	if bytes.Contains(raw, []byte(target)) {
		t.Errorf("a viewer reads the grandparent's sensitive text on %s", grandchild)
	}
	// The plugin subscribed to publication.completed got that event (§8.2).
	var deliveries api.List[struct {
		Subscriber string `json:"subscriber"`
		Status     string `json:"status"`
	}]
	sc.admin.jsonOut(t, &deliveries, "event", "deliveries", "--all")
	delivered := false
	for _, d := range deliveries.Items {
		if strings.HasPrefix(d.Subscriber, "plugin:sample-notifier") && d.Status == "delivered" {
			delivered = true
		}
	}
	if !delivered {
		t.Errorf("no delivery to the plugin: %+v", deliveries.Items)
	}
}

func (sc *scenario) pendingApproval(t *testing.T, c *client, job string) string {
	t.Helper()
	var approvals api.List[domain.Approval]
	c.jsonOut(t, &approvals, "approval", "list", "--status", "pending")
	for _, a := range approvals.Items {
		if string(a.JobID) == job {
			return string(a.ID)
		}
	}
	t.Fatalf("no pending approval of %s in %+v", job, approvals.Items)
	return ""
}

// ---- Extended: §15.6 tenant isolation ----

func (sc *scenario) stepIsolation(t *testing.T) {
	sc.admin.ok(t, "tenant", "create", "globex")
	sc.admin.refused(t, "not_found", "--tenant", "globex", "asset", "show", "ORI-001")
	t.Logf("not a member: %s", sc.editor.refused(t, "not_found", "--tenant", "globex", "asset", "list"))
	var results api.SearchResponse
	sc.admin.jsonOut(t, &results, "--tenant", "globex", "original", "search", "conscious breathing")
	if len(results.Items) != 0 {
		t.Fatalf("search in globex found acme's text: %+v", results.Items)
	}
}

// ---- Extended: §15.7, a dead sink ----

func (sc *scenario) stepDeadSink(t *testing.T) {
	// Loki on a port nobody listens on. Jobs keep running, and the failure shows.
	port := freePort(t)
	sc.stack.WriteTenantConfig(t, tenant, "observability.json", fmt.Sprintf(`{
  "observability": {
    "logs": { "sinks": [{ "id": "loki-dead", "driver": "loki", "enabled": true, "parameters": { "url": "http://127.0.0.1:%d" } }] }
  }
}`, port))
	time.Sleep(2 * time.Second)
	small := sc.generateFixture(t, "small.mp4", 40, "640x360", 25)
	var started struct {
		Job *api.RunWorkflowResponse `json:"job"`
	}
	sc.editor.jsonOut(t, &started, "original", "import", small, "--title", "Dead sink class", "--type", "yoga_class", "--idempotency-key", "dead-sink")
	sc.waitJob(t, sc.editor, string(started.Job.JobID), domain.JobCompleted)
	waitFor(t, time.Minute, "the dead sink's failure to show", func() (bool, string) {
		var sinks api.List[api.ObservabilitySink]
		sc.admin.jsonOut(t, &sinks, "observability", "list")
		for _, sink := range sinks.Items {
			if sink.ID == "loki-dead" {
				return sink.Status != "ok" && sink.LastError != "", fmt.Sprintf("%+v", sink)
			}
		}
		return false, fmt.Sprintf("no loki-dead in %+v", sinks.Items)
	})
}

// ---- Shared helpers ----

type eventRow struct {
	ID         string    `json:"event_id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	AssetID    string    `json:"asset_id"`
	JobID      string    `json:"job_id"`
}

func (sc *scenario) events(t *testing.T, c *client) []eventRow {
	t.Helper()
	var list api.List[eventRow]
	c.jsonOut(t, &list, "event", "list", "--all")
	return list.Items
}

func eventTime(events []eventRow, kind, asset string) (time.Time, bool) {
	for _, e := range events {
		if e.Type == kind && e.AssetID == asset {
			return e.OccurredAt, true
		}
	}
	return time.Time{}, false
}

func hasEvent(events []eventRow, kind string) bool {
	return slices.ContainsFunc(events, func(e eventRow) bool { return e.Type == kind })
}

func (sc *scenario) waitJob(t *testing.T, c *client, id string, want domain.JobStatus) api.JobDetail {
	t.Helper()
	var job api.JobDetail
	waitFor(t, jobTimeout, id+" to be "+string(want), func() (bool, string) {
		c.jsonOut(t, &job, "job", "status", id)
		if job.Status == want {
			return true, ""
		}
		failOn(t, job)
		return false, nodeSummary(job)
	})
	return job
}

func (sc *scenario) waitTerminal(t *testing.T, c *client, id string) api.JobDetail {
	t.Helper()
	var job api.JobDetail
	waitFor(t, jobTimeout, id+" to end", func() (bool, string) {
		c.jsonOut(t, &job, "job", "status", id)
		return job.Status.Terminal(), nodeSummary(job)
	})
	return job
}

func failOn(t *testing.T, job api.JobDetail) {
	t.Helper()
	switch job.Status {
	case domain.JobFailed, domain.JobCancelled, domain.JobNeedsAttention:
		detail, _ := json.Marshal(job.Error)
		t.Fatalf("job %s (%s) ended %s: %s\n%s", job.ID, job.WorkflowID, job.Status, detail, nodeSummary(job))
	}
}

func nodeSummary(job api.JobDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s:", job.ID, job.Status)
	for _, n := range job.Nodes {
		if n.Status == domain.NodeCompleted || n.Status == domain.NodePending {
			continue
		}
		fmt.Fprintf(&b, " %s=%s#%d", n.NodeID, n.Status, n.Attempt)
		if n.Error != nil {
			fmt.Fprintf(&b, "(%s: %s)", n.Error.Code, n.Error.Message)
		}
	}
	return b.String()
}

func nodeOf(t *testing.T, job api.JobDetail, id string) domain.NodeExecution {
	t.Helper()
	for _, n := range job.Nodes {
		if n.NodeID == id {
			return n
		}
	}
	t.Fatalf("no node %s in %s", id, job.ID)
	return domain.NodeExecution{}
}

func mustUnmarshal(t *testing.T, data []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode %q: %v", truncate(string(data), 500), err)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func ffprobeDuration(t *testing.T, path string) float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	stdout, stderr, code, err := run(ctx, os.Environ(), "", "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path)
	if err != nil || code != 0 {
		t.Fatalf("ffprobe %s: %v %s", path, err, stderr)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(stdout), 64)
	if err != nil {
		t.Fatalf("ffprobe %s: %q", path, stdout)
	}
	return value
}

func plainGet(target string) (int, []byte, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, buf.Bytes(), nil
}
