package apiserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func clip(key string) request {
	return request{
		method: "POST", path: "/api/v1/clips", token: tokenEditor, tenant: tenantA, key: key,
		body: `{"source_id":"ORI-001","prompt":"the 90-year-old woman"}`,
	}
}

func countCalls(w *world, name string) int {
	n := 0
	for _, c := range w.callNames() {
		if c == name {
			n++
		}
	}
	return n
}

func TestKeyIsRequiredOnMutations(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for _, rt := range api.Routes {
		if !usesIdempotency(rt) {
			continue
		}
		req := requestFor(rt, routeCases[rt.Name])
		req.key = ""
		wantError(t, do(h, req), http.StatusBadRequest, errs.CodeInvalidArgument)
	}
	for _, name := range w.callNames() {
		if !strings.HasPrefix(name, "audit:") {
			t.Fatalf("a request without a key reached %s", name)
		}
	}
	if records := w.auditRecords(); len(records) != 0 {
		t.Fatalf("a refused request wrote the audit: %+v", records)
	}
}

func TestKeyExceptions(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	// No key needed: reads by nature, idempotent by name, or tenant-free.
	wantStatus(t, do(h, request{method: "POST", path: "/api/v1/workflows/validate", token: tokenViewer, tenant: tenantA, body: `{"definition":{"id":"x"}}`}), http.StatusOK)
	wantStatus(t, do(h, request{method: "POST", path: "/api/v1/secrets/gcs-main/rotate", token: tokenAdmin, tenant: tenantA, body: `{"value":"v"}`}), http.StatusOK)
	wantStatus(t, do(h, request{method: "POST", path: "/api/v1/tenants", token: tokenSystem, body: `{"id":"initech"}`}), http.StatusCreated)
	wantStatus(t, do(h, request{method: "POST", path: "/api/v1/devices/CLIENT-001/rotate", token: tokenAdmin}), http.StatusOK)
	wantStatus(t, do(h, request{method: "POST", path: "/api/v1/worker/heartbeat", token: tokenWorker, body: `{"worker_id":"w","running":0}`}), http.StatusOK)
}

func TestKeyIsIgnoredWhereNotUsed(t *testing.T) {
	// A value that would be refused where keys matter is simply ignored here.
	w := newWorld(t)
	rec := do(w.serve(), request{method: "POST", path: "/api/v1/tenants", token: tokenSystem, key: "has space", body: `{"id":"initech"}`})
	wantStatus(t, rec, http.StatusCreated)
}

func TestInvalidKeys(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for _, bad := range []string{strings.Repeat("k", 256), "has space", "tab\tkey", "ação"} {
		wantError(t, do(h, clip(bad)), http.StatusBadRequest, errs.CodeInvalidArgument)
	}
}

func TestSameKeySameRequestReplays(t *testing.T) {
	w := newWorld(t)
	h := w.serve()

	first := do(h, clip("k1"))
	wantStatus(t, first, http.StatusAccepted)
	if first.Header().Get(api.HeaderIdempotencyReplayed) != "" {
		t.Fatal("the first answer is no replay")
	}
	second := do(h, clip("k1"))
	wantStatus(t, second, http.StatusAccepted)
	if second.Header().Get(api.HeaderIdempotencyReplayed) != "true" {
		t.Fatal("the replay is not marked")
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay differs: %s vs %s", first.Body.String(), second.Body.String())
	}
	if got := second.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if n := countCalls(w, "workflows.CreateClip"); n != 1 {
		t.Fatalf("the service ran %d times", n)
	}
	// A replay is no new mutation: one audit record, not two.
	if records := w.auditRecords(); len(records) != 1 {
		t.Fatalf("%d audit records", len(records))
	}
	if first.Header().Get(api.HeaderRequestID) == second.Header().Get(api.HeaderRequestID) {
		t.Error("a replay is a new request and needs its own request ID")
	}
}

func TestSameKeyDifferentRequestIsRefused(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	wantStatus(t, do(h, clip("k1")), http.StatusAccepted)

	other := clip("k1")
	other.body = `{"source_id":"ORI-002","prompt":"something else"}`
	wantError(t, do(h, other), http.StatusConflict, errs.CodeIdempotencyMismatch)

	// The same key on another route is a different request too.
	otherRoute := request{method: "POST", path: "/api/v1/publications", token: tokenPublisher, tenant: tenantA, key: "k2", body: `{"asset_id":"CLIP-001","destinations":["m"]}`}
	wantStatus(t, do(h, otherRoute), http.StatusAccepted)
	wrong := otherRoute
	wrong.path = "/api/v1/publications/withdraw"
	wantError(t, do(h, wrong), http.StatusConflict, errs.CodeIdempotencyMismatch)

	if n := countCalls(w, "workflows.CreateClip"); n != 1 {
		t.Fatalf("the service ran %d times", n)
	}
}

func TestFailuresAreNotRemembered(t *testing.T) {
	w := newWorld(t)
	h := w.serve()

	w.err = errs.New(errs.CodeUnavailable, "try later")
	wantError(t, do(h, clip("k1")), http.StatusServiceUnavailable, errs.CodeUnavailable)
	if n := w.stores.Store(tenantA).Records(); n != 0 {
		t.Fatalf("%d records after a failure", n)
	}

	w.err = nil
	rec := do(h, clip("k1"))
	wantStatus(t, rec, http.StatusAccepted)
	if rec.Header().Get(api.HeaderIdempotencyReplayed) != "" {
		t.Fatal("the retry after a failure must really run")
	}
	if n := countCalls(w, "workflows.CreateClip"); n != 2 {
		t.Fatalf("the service ran %d times", n)
	}
}

func TestKeysAreNamespacedByUserAndTenant(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	wantStatus(t, do(h, clip("shared")), http.StatusAccepted)

	other := clip("shared")
	other.token = tokenPublisher
	rec := do(h, other)
	wantStatus(t, rec, http.StatusAccepted)
	if rec.Header().Get(api.HeaderIdempotencyReplayed) != "" {
		t.Fatal("one user's key replayed another user's answer")
	}

	// viewer@ is admin of globex: the same key in another tenant is independent.
	globex := clip("shared")
	globex.token = tokenViewer
	globex.tenant = tenantB
	wantStatus(t, do(h, globex), http.StatusAccepted)
	if n := w.stores.Store(tenantB).Records(); n != 1 {
		t.Fatalf("globex has %d records", n)
	}
	if n := w.stores.Store(tenantA).Records(); n != 2 {
		t.Fatalf("acme has %d records", n)
	}
}

func TestConcurrentDuplicatesRunOnce(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	const n = 16
	results := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = do(h, clip("same-key"))
		}()
	}
	wg.Wait()
	replays := 0
	for _, rec := range results {
		wantStatus(t, rec, http.StatusAccepted)
		if rec.Header().Get(api.HeaderIdempotencyReplayed) == "true" {
			replays++
		}
	}
	if runs := countCalls(w, "workflows.CreateClip"); runs != 1 {
		t.Fatalf("the service ran %d times", runs)
	}
	if replays != n-1 {
		t.Fatalf("%d replays", replays)
	}
}

func TestRunRequestKeyInBody(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	run := func(key, body string) request {
		return request{method: "POST", path: "/api/v1/workflows/create-clip/runs", token: tokenEditor, tenant: tenantA, key: key, body: body}
	}

	// The body field alone is enough (I15), and reaches the service either way.
	wantStatus(t, do(h, run("", `{"idempotency_key":"from-body","inputs":{}}`)), http.StatusAccepted)
	got := w.lastCall(t)
	if got.Scope.IdempotencyKey != "from-body" || got.Args.(api.RunWorkflowRequest).IdempotencyKey != "from-body" {
		t.Fatalf("call = %+v", got)
	}

	wantStatus(t, do(h, run("from-header", `{"inputs":{}}`)), http.StatusAccepted)
	got = w.lastCall(t)
	if got.Args.(api.RunWorkflowRequest).IdempotencyKey != "from-header" {
		t.Fatalf("call = %+v", got)
	}

	// Both, equal: fine. Both, different: refused.
	wantStatus(t, do(h, run("same", `{"idempotency_key":"same"}`)), http.StatusAccepted)
	wantError(t, do(h, run("one", `{"idempotency_key":"other"}`)), http.StatusBadRequest, errs.CodeInvalidArgument)
}

func TestRepositoriesCarryTheScope(t *testing.T) {
	w := newWorld(t)
	wantStatus(t, do(w.serve(), clip("k")), http.StatusAccepted)
	s := w.lastCall(t).Scope
	if s.Tenant != tenantA || s.IdempotencyKey != "k" || s.RequestID == "" || s.TraceID == "" {
		t.Fatalf("scope = %+v", s)
	}
	if s.Principal.Email != "editor@example.com" || s.Principal.Role != domain.RoleEditor || s.Principal.TenantID != tenantA {
		t.Fatalf("principal = %+v", s.Principal)
	}
}

// ---- Audit ----

func TestAuditIsWrittenBeforeTheHandler(t *testing.T) {
	w := newWorld(t)
	wantStatus(t, do(w.serve(), clip("k")), http.StatusAccepted)
	calls := w.callNames()
	if len(calls) != 2 || calls[0] != "audit:allowed" || calls[1] != "workflows.CreateClip" {
		t.Fatalf("order = %v", calls)
	}
	records := w.auditRecords()
	got := records[0]
	if got.Action != api.RouteClipsCreate || got.Permission != domain.PermWorkflowRun || got.TenantID != tenantA || got.Result != resultAllowed {
		t.Fatalf("record = %+v", got)
	}
	if got.Principal.Email != "editor@example.com" || got.RequestID == "" {
		t.Fatalf("record = %+v", got)
	}
}

func TestAuditFailureStopsTheMutation(t *testing.T) {
	w := newWorld(t)
	w.auditErr = errString("disk full")
	rec := do(w.serve(), clip("k"))
	wantError(t, rec, http.StatusServiceUnavailable, errs.CodeUnavailable)
	if n := countCalls(w, "workflows.CreateClip"); n != 0 {
		t.Fatal("a mutation ran with the audit down")
	}
	if strings.Contains(rec.Body.String(), "disk full") {
		t.Fatalf("internal text leaked: %s", rec.Body.String())
	}
	if n := w.stores.Store(tenantA).Records(); n != 0 {
		t.Fatalf("%d idempotency records", n)
	}
}

func TestMissingAuditorStopsTheMutation(t *testing.T) {
	w := newWorld(t)
	h := w.serve(func(d *Deps) { d.Auditor = nil })
	wantError(t, do(h, clip("k")), http.StatusServiceUnavailable, errs.CodeUnavailable)
	if n := countCalls(w, "workflows.CreateClip"); n != 0 {
		t.Fatal("a mutation ran with no auditor")
	}
}

func TestFailedMutationAddsARecord(t *testing.T) {
	w := newWorld(t)
	w.err = errs.New(errs.CodeFailedPrecondition, "not ready")
	wantError(t, do(w.serve(), clip("k")), http.StatusConflict, errs.CodeFailedPrecondition)
	records := w.auditRecords()
	if len(records) != 2 || records[0].Result != resultAllowed || records[1].Result != resultFailed {
		t.Fatalf("records = %+v", records)
	}
	if records[1].Details["error_code"] != string(errs.CodeFailedPrecondition) {
		t.Fatalf("details = %+v", records[1].Details)
	}
}

func TestAuditResourceAndContents(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	const note = "a private note about a participant"
	body, _ := json.Marshal(map[string]string{"note": note})
	rec := do(h, request{method: "POST", path: "/api/v1/approvals/APR-007/approve", token: tokenEditor, tenant: tenantA, key: "k", body: string(body)})
	wantStatus(t, rec, http.StatusOK)
	records := w.auditRecords()
	if len(records) != 1 || records[0].Resource != "APR-007" {
		t.Fatalf("records = %+v", records)
	}
	if strings.Contains(string(mustJSON(t, records)), note) {
		t.Fatal("a body reached the audit")
	}
}

func TestReadsAndTenantFreeAudit(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	wantStatus(t, do(h, request{method: "GET", path: "/api/v1/assets", token: tokenViewer, tenant: tenantA}), http.StatusOK)
	if records := w.auditRecords(); len(records) != 0 {
		t.Fatalf("a read wrote the audit: %+v", records)
	}

	// A tenant-free mutation is recorded with an empty tenant: the system chain.
	wantStatus(t, do(h, request{method: "POST", path: "/api/v1/tenants", token: tokenSystem, body: `{"id":"initech"}`}), http.StatusCreated)
	records := w.auditRecords()
	if len(records) != 1 || records[0].TenantID != "" || records[0].Action != api.RouteTenantsAdd {
		t.Fatalf("records = %+v", records)
	}

	// Validation reads only.
	w.resetCalls()
	wantStatus(t, do(h, request{method: "POST", path: "/api/v1/workflows/validate", token: tokenViewer, tenant: tenantA, body: `{"definition":{"id":"x"}}`}), http.StatusOK)
	if records := w.auditRecords(); len(records) != 0 {
		t.Fatalf("validation wrote the audit: %+v", records)
	}

	// Pairing has no identity to record, and must not let anyone grow the chain.
	wantStatus(t, do(h, request{method: "POST", path: "/api/v1/devices/pair", body: `{"code":"ABCDE"}`}), http.StatusCreated)
	if records := w.auditRecords(); len(records) != 0 {
		t.Fatalf("pairing wrote the audit: %+v", records)
	}
}

func TestDeniedMutationIsAuditedAndNotExecuted(t *testing.T) {
	w := newWorld(t)
	rec := do(w.serve(), request{method: "POST", path: "/api/v1/plugins", token: tokenEditor, tenant: tenantA, key: "k", body: `{"path":"/x"}`})
	wantError(t, rec, http.StatusForbidden, errs.CodeForbidden)
	records := w.auditRecords()
	if len(records) != 1 || records[0].Result != resultDenied || records[0].Permission != domain.PermPluginInstall {
		t.Fatalf("records = %+v", records)
	}
	if n := countCalls(w, "plugins.Install"); n != 0 {
		t.Fatal("a denied request ran")
	}
}
