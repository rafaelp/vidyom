package apiserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/api/uploads"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/version"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
	"github.com/rafaelp/vidyom/schemas"
)

func keyFor(s string) string { return "k-" + sumHex(s)[:16] }

func sumHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---- Health and version ----

func TestHealth(t *testing.T) {
	w := newWorld(t)
	var mu sync.Mutex
	redisErr := error(nil)
	aiErr := error(nil)
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	calls := 0
	h := w.serve(func(d *Deps) {
		d.Sealing = "file"
		d.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		d.HealthChecks = []HealthCheck{
			{Name: "redis", Critical: true, Check: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				calls++
				return redisErr
			}},
			{Name: "ai_worker", Check: func(context.Context) error { mu.Lock(); defer mu.Unlock(); return aiErr }},
		}
	})
	get := func() (*httptest.ResponseRecorder, api.HealthResponse) {
		rec := do(h, request{method: "GET", path: "/api/v1/health"})
		var body api.HealthResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return rec, body
	}
	advance := func() { mu.Lock(); now = now.Add(healthCacheFor + time.Second); mu.Unlock() }

	rec, body := get()
	wantStatus(t, rec, http.StatusOK)
	if body.Status != "ok" || body.Sealing != "file" || body.Version != version.Version || body.Components["redis"].Status != "ok" {
		t.Fatalf("body = %+v", body)
	}

	// Cached: an unauthenticated route cannot be used to hammer Redis.
	get()
	get()
	mu.Lock()
	if calls != 1 {
		t.Errorf("checks ran %d times", calls)
	}
	mu.Unlock()

	// A failure of something optional is degraded, not down.
	advance()
	mu.Lock()
	aiErr = errors.New("connection refused at 10.0.0.5")
	mu.Unlock()
	rec, body = get()
	wantStatus(t, rec, http.StatusOK)
	if body.Status != "degraded" || body.Components["ai_worker"].Status != "degraded" {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatal("details on an unauthenticated route")
	}

	// A critical failure is down, with 503.
	advance()
	mu.Lock()
	redisErr = errors.New("down")
	mu.Unlock()
	rec, body = get()
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if body.Status != "down" {
		t.Fatalf("body = %+v", body)
	}
}

func TestHealthAndVersionNeedNoCredential(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	wantStatus(t, do(h, request{method: "GET", path: "/api/v1/health"}), http.StatusOK)
	rec := do(h, request{method: "GET", path: "/api/v1/version"})
	wantStatus(t, rec, http.StatusOK)
	var body api.VersionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.APIVersion != version.APIVersion {
		t.Fatalf("body = %+v", body)
	}
}

// ---- Metrics ----

func TestMetricsAccess(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	get := func(token string, tenant domain.TenantID) *httptest.ResponseRecorder {
		return do(h, request{method: "GET", path: "/api/v1/metrics", token: token, tenant: tenant})
	}

	// No tenant: system admin, everything.
	rec := get(tokenSystem, "")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `tenant_id="globex"`) || !strings.Contains(rec.Body.String(), "vidyom_queue_depth") {
		t.Fatalf("body = %s", rec.Body.String())
	}

	// No tenant, not a system admin: refused.
	wantError(t, get(tokenAdmin, ""), http.StatusForbidden, errs.CodeForbidden)

	// A tenant admin sees that tenant only (I128, D6).
	rec = get(tokenAdmin, tenantA)
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !strings.Contains(body, `vidyom_jobs{state="running",tenant_id="acme"} 2`) {
		t.Fatalf("own samples missing: %s", body)
	}
	for _, leaked := range []string{"globex", "other_tenant_id", "vidyom_queue_depth"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("%s leaked to a tenant: %s", leaked, body)
		}
	}
	if !strings.Contains(body, "# TYPE vidyom_jobs gauge") {
		t.Fatalf("comments dropped: %s", body)
	}

	// Below admin, and outside the tenant.
	wantError(t, get(tokenViewer, tenantA), http.StatusForbidden, errs.CodeForbidden)
	wantError(t, get(tokenAdmin, tenantB), http.StatusNotFound, errs.CodeNotFound)
}

func TestMetricsHandlerSeesTheTenantHeader(t *testing.T) {
	// The observability manager also filters by this header (I2xx of extensibility).
	w := newWorld(t)
	var seen string
	w.obs.metrics = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get(api.HeaderTenant)
	})
	wantStatus(t, do(w.serve(), request{method: "GET", path: "/api/v1/metrics", token: tokenAdmin, tenant: tenantA}), http.StatusOK)
	if seen != string(tenantA) {
		t.Fatalf("header = %q", seen)
	}
}

// ---- Metrics of authentication ----

type countingMetrics struct {
	mu     sync.Mutex
	counts map[string]float64
}

func (m *countingMetrics) Add(name string, delta float64, _ telemetry.Labels) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[name] += delta
}
func (m *countingMetrics) Set(string, float64, telemetry.Labels)     {}
func (m *countingMetrics) Observe(string, float64, telemetry.Labels) {}

func TestRefusalsAreCounted(t *testing.T) {
	w := newWorld(t)
	m := &countingMetrics{counts: map[string]float64{}}
	h := w.serve(func(d *Deps) { d.Metrics = m })
	do(h, request{method: "GET", path: "/api/v1/assets", tenant: tenantA})
	if m.counts[telemetry.AuthFailures] != 1 {
		t.Errorf("auth failures = %v", m.counts[telemetry.AuthFailures])
	}
	do(h, request{method: "POST", path: "/api/v1/pairing-codes", token: tokenAdmin, body: `{}`})
	if m.counts[telemetry.AuthzDenials] != 1 {
		t.Errorf("denials = %v", m.counts[telemetry.AuthzDenials])
	}
}

// ---- Uploads through HTTP, against the real upload service ----

func TestUploadProtocolOverHTTP(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(w.serve())
	defer srv.Close()
	data := []byte(strings.Repeat("vidyom-", 500))

	send := func(method, path string, headers map[string]string, body string) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tokenEditor)
		req.Header.Set(api.HeaderTenant, string(tenantA))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			raw = append(raw, buf[:n]...)
			if err != nil {
				break
			}
		}
		return resp, raw
	}
	key := func(k string) map[string]string { return map[string]string{api.HeaderIdempotencyKey: k} }

	// Create, under an idempotency key: a retry gets the same upload.
	body, _ := json.Marshal(api.CreateUploadRequest{Filename: "class.mp4", SizeBytes: int64(len(data)), SHA256: sumHex(string(data))})
	resp, raw := send("POST", "/api/v1/assets/uploads", key("create-1"), string(body))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, raw)
	}
	var created domain.Upload
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if created.ChunkBytes <= 0 || created.Status != domain.UploadPending {
		t.Fatalf("created = %+v", created)
	}
	resp, raw2 := send("POST", "/api/v1/assets/uploads", key("create-1"), string(body))
	if resp.Header.Get(api.HeaderIdempotencyReplayed) != "true" || string(raw2) != string(raw) {
		t.Fatalf("retry created another upload: %s", raw2)
	}

	path := "/api/v1/uploads/" + string(created.ID)

	// Missing or malformed offset.
	resp, _ = send("PATCH", path, key("c1"), "abc")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no offset: %d", resp.StatusCode)
	}

	// A chunk with its checksum returns the new offset in the header.
	chunk := string(data[:1000])
	sum := sha256.Sum256([]byte(chunk))
	headers := map[string]string{
		api.HeaderUploadOffset:   "0",
		api.HeaderUploadChecksum: "sha256 " + base64.StdEncoding.EncodeToString(sum[:]),
	}
	resp, raw = send("PATCH", path, headers, chunk)
	if resp.StatusCode != http.StatusOK || resp.Header.Get(api.HeaderUploadOffset) != "1000" {
		t.Fatalf("chunk: %d %s %v", resp.StatusCode, raw, resp.Header)
	}

	// Wrong offset: 409 with the current one, in the header and in the details.
	resp, raw = send("PATCH", path, map[string]string{api.HeaderUploadOffset: "0"}, chunk)
	if resp.StatusCode != http.StatusConflict || resp.Header.Get(api.HeaderUploadOffset) != "1000" {
		t.Fatalf("wrong offset: %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), `"offset":1000`) {
		t.Fatalf("details: %s", raw)
	}

	// Bad checksums.
	resp, _ = send("PATCH", path, map[string]string{api.HeaderUploadOffset: "1000", api.HeaderUploadChecksum: "md5 abc"}, "x")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown algorithm: %d", resp.StatusCode)
	}
	other := sha256.Sum256([]byte("other"))
	resp, raw = send("PATCH", path, map[string]string{
		api.HeaderUploadOffset:   "1000",
		api.HeaderUploadChecksum: "sha256 " + base64.StdEncoding.EncodeToString(other[:]),
	}, "x")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: %d %s", resp.StatusCode, raw)
	}

	// GET reports the offset.
	resp, raw = send("GET", path, nil, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get(api.HeaderUploadOffset) != "1000" {
		t.Fatalf("get: %d %s", resp.StatusCode, raw)
	}

	// Completing early refuses; the rest, then complete.
	resp, _ = send("POST", path+"/complete", key("complete-1"), "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("early complete: %d", resp.StatusCode)
	}
	resp, raw = send("PATCH", path, map[string]string{api.HeaderUploadOffset: "1000"}, string(data[1000:]))
	if resp.StatusCode != http.StatusOK || resp.Header.Get(api.HeaderUploadOffset) != strconv.Itoa(len(data)) {
		t.Fatalf("last chunk: %d %s", resp.StatusCode, raw)
	}
	resp, raw = send("POST", path+"/complete", key("complete-2"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete: %d %s", resp.StatusCode, raw)
	}
	var done domain.Upload
	if err := json.Unmarshal(raw, &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != domain.UploadComplete {
		t.Fatalf("status %s", done.Status)
	}
	onDisk, err := os.ReadFile(uploads.Path(w.spool, tenantA, created.ULID))
	if err != nil || string(onDisk) != string(data) {
		t.Fatalf("spool: %v", err)
	}

	// Chunks write no audit; create, complete and the refused complete do (I123).
	actions := map[string]int{}
	for _, r := range w.auditRecords() {
		actions[r.Action]++
	}
	if actions[api.RouteUploadsChunk] != 0 {
		t.Fatalf("chunks audited: %v", actions)
	}
	if actions[api.RouteUploadsCreate] != 1 || actions[api.RouteUploadsComplete] == 0 {
		t.Fatalf("audit: %v", actions)
	}

	// Abort removes everything.
	resp, _ = send("DELETE", path, key("abort-1"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("abort: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(w.spool, string(tenantA), string(created.ULID))); !os.IsNotExist(err) {
		t.Fatal("file kept after abort")
	}
}

func TestUploadsAreTenantScopedOverHTTP(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	rec := do(h, request{
		method: "POST", path: "/api/v1/assets/uploads", token: tokenEditor, tenant: tenantA, key: "k",
		body: `{"filename":"a.mp4","size_bytes":3,"sha256":"` + sumHex("abc") + `"}`,
	})
	wantStatus(t, rec, http.StatusCreated)
	var created domain.Upload
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	// viewer@ is admin of globex; the same ID there is someone else's or nothing.
	rec = do(h, request{method: "GET", path: "/api/v1/uploads/" + string(created.ULID), token: tokenViewer, tenant: tenantB})
	wantError(t, rec, http.StatusNotFound, errs.CodeNotFound)
}

// ---- Routes backed by jobs ----

func TestAssetDeleteRunsTheWorkflow(t *testing.T) {
	w := newWorld(t)
	rec := do(w.serve(), request{method: "DELETE", path: "/api/v1/assets/CLIP-001", token: tokenAdmin, tenant: tenantA, key: "k"})
	wantStatus(t, rec, http.StatusAccepted)
	got := w.lastCall(t)
	if got.Name != "workflows.Run:delete-asset" {
		t.Fatalf("call = %s", got.Name)
	}
	req := got.Args.(api.RunWorkflowRequest)
	if string(req.Inputs) != `{"asset_id":"CLIP-001"}` || req.IdempotencyKey != "k" {
		t.Fatalf("request = %+v", req)
	}
}

func TestAssetExportRunsTheNode(t *testing.T) {
	w := newWorld(t)
	rec := do(w.serve(), request{method: "POST", path: "/api/v1/assets/ORI-001/export", token: tokenViewer, tenant: tenantA, key: "k"})
	wantStatus(t, rec, http.StatusAccepted)
	got := w.lastCall(t)
	if got.Name != "workflows.RunNode:catalog.export_package" {
		t.Fatalf("call = %s", got.Name)
	}
	if req := got.Args.(api.RunNodeRequest); string(req.Parameters) != `{"assetId":"ORI-001"}` {
		t.Fatalf("request = %+v", req)
	}
}

func assetWithFiles(w *world) {
	w.assets.asset = domain.Asset{ID: "ORI-001", CurrentVersion: "v2"}
	w.assets.versions = []domain.AssetVersion{
		{Version: "v1", Files: []domain.VersionFile{
			{File: domain.MediaFile{Storage: domain.RoleMaster, Key: "originals/ORI-001/v1/master.mp4", SizeBytes: 1 << 40}},
		}},
		{Version: "v2", Files: []domain.VersionFile{
			{File: domain.MediaFile{Storage: domain.RoleMaster, Key: "originals/ORI-001/v2/master.mp4", SizeBytes: 100}},
			{Name: "alt", File: domain.MediaFile{Storage: domain.RoleMaster, Key: "originals/ORI-001/v2/alt.mp4", SizeBytes: 50}},
			{File: domain.MediaFile{Storage: domain.RoleArchive, Key: "originals/ORI-001/v2/original.mov", SizeBytes: 3 << 30}},
		}},
	}
}

func TestStorageCopy(t *testing.T) {
	w := newWorld(t)
	assetWithFiles(w)
	h := w.serve()
	post := func(body string) *httptest.ResponseRecorder {
		return do(h, request{method: "POST", path: "/api/v1/storages/copy", token: tokenAdmin, tenant: tenantA, key: keyFor(body), body: body})
	}

	// Reading archive is for restore-archived only (§3.3).
	wantError(t, post(`{"from":"archive","to":"master","asset_id":"ORI-001"}`), http.StatusForbidden, errs.CodeForbiddenStorageRole)
	wantError(t, post(`{"from":"master","to":"another-storage","asset_id":"ORI-001"}`), http.StatusBadRequest, errs.CodeInvalidArgument)
	wantError(t, post(`{"from":"master","to":"master","asset_id":"ORI-001"}`), http.StatusBadRequest, errs.CodeInvalidArgument)
	wantError(t, post(`{"from":"master","to":"proxy","asset_id":"JOB-001"}`), http.StatusBadRequest, errs.CodeInvalidArgument)
	wantError(t, post(`{"from":"proxy","to":"master","asset_id":"ORI-001"}`), http.StatusConflict, errs.CodeFailedPrecondition)

	rec := post(`{"from":"master","to":"proxy","asset_id":"ORI-001"}`)
	wantStatus(t, rec, http.StatusAccepted)
	got := w.lastCall(t)
	if got.Name != "workflows.Run:" {
		t.Fatalf("call = %s", got.Name)
	}
	definition := got.Args.(api.RunWorkflowRequest).Definition
	if err := schemas.Validate("workflow.v1.json", definition); err != nil {
		t.Fatalf("the generated workflow fails the schema: %v\n%s", err, definition)
	}
	def, err := wfdef.Parse(definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Nodes) != 2 {
		t.Fatalf("%d nodes, want the two master files of v2", len(def.Nodes))
	}
	var params map[string]string
	if err := json.Unmarshal(def.Nodes[0].Parameters, &params); err != nil {
		t.Fatal(err)
	}
	if params["source"] != "storage://master/originals/ORI-001/v2/master.mp4" || params["storage"] != "proxy" || params["key"] != "originals/ORI-001/v2/master.mp4" {
		t.Fatalf("parameters = %v", params)
	}
	catalog := def.Nodes[0]
	if err := schemas.Validate("node-catalog.v1.json#/nodes/"+catalogIndex(t, "storage.copy")+"/parameters", catalog.Parameters); err != nil {
		t.Fatalf("parameters fail the catalog: %v", err)
	}
}

// catalogIndex finds a node's position in the catalog, for its schema reference.
func catalogIndex(t *testing.T, nodeType string) string {
	t.Helper()
	data, err := schemas.Read("node-catalog.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Nodes []struct {
			Type string `json:"type"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	for i, n := range catalog.Nodes {
		if n.Type == nodeType {
			return strconv.Itoa(i)
		}
	}
	t.Fatalf("%s is not in the catalog", nodeType)
	return ""
}

func TestStorageRestoreIsTwoPhase(t *testing.T) {
	w := newWorld(t)
	assetWithFiles(w)
	h := w.serve()
	post := func(key, body string) *httptest.ResponseRecorder {
		return do(h, request{method: "POST", path: "/api/v1/storages/restore", token: tokenAdmin, tenant: tenantA, key: key, body: body})
	}

	// Without confirm: the cost, and nothing started (§3.3).
	rec := post("k1", `{"asset_id":"ORI-001","to":"master"}`)
	wantStatus(t, rec, http.StatusOK)
	var preview api.StorageRestorePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.ConfirmRequired || preview.ArchiveBytes != 3<<30 || preview.AssetID != "ORI-001" {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.EstimateMinor == nil || *preview.EstimateMinor != 6 || preview.Currency != "USD" {
		t.Fatalf("estimate: %+v", preview)
	}
	for _, name := range w.callNames() {
		if strings.HasPrefix(name, "workflows.Run") {
			t.Fatal("a preview started a job")
		}
	}

	// With confirm: the workflow.
	rec = post("k2", `{"asset_id":"ORI-001","to":"master","confirm":true}`)
	wantStatus(t, rec, http.StatusAccepted)
	got := w.lastCall(t)
	if got.Name != "workflows.Run:restore-archived" {
		t.Fatalf("call = %s", got.Name)
	}
	if string(got.Args.(api.RunWorkflowRequest).Inputs) != `{"asset_id":"ORI-001","to":"master"}` {
		t.Fatalf("inputs = %s", got.Args.(api.RunWorkflowRequest).Inputs)
	}

	wantError(t, post("k3", `{"asset_id":"ORI-001","to":"archive"}`), http.StatusBadRequest, errs.CodeInvalidArgument)

	// No cost table: the bytes, and a plain statement that there is no estimate.
	h = w.serve(func(d *Deps) { d.TenantConfig = nil })
	rec = do(h, request{method: "POST", path: "/api/v1/storages/restore", token: tokenAdmin, tenant: tenantA, key: "k4", body: `{"asset_id":"ORI-001","to":"master"}`})
	wantStatus(t, rec, http.StatusOK)
	var plain api.StorageRestorePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.EstimateMinor != nil || plain.Note == "" {
		t.Fatalf("preview = %+v", plain)
	}
}

// The input names this package sends must exist in the engine's workflow files. The
// files are read, not imported, and the check waits until they land.
func TestWorkflowInputNamesMatchTheFiles(t *testing.T) {
	for file, inputs := range map[string][]string{
		"delete-asset.json":     {"asset_id"},
		"restore-archived.json": {"asset_id", "to"},
	} {
		data, err := os.ReadFile(filepath.Join("..", "..", "workflows", file))
		if err != nil {
			t.Logf("%s has not landed: %v", file, err)
			continue
		}
		def, err := wfdef.Parse(data)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		for _, name := range inputs {
			if _, ok := def.Inputs[name]; !ok {
				t.Errorf("%s does not declare input %q", file, name)
			}
		}
	}
}
