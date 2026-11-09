package apiserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// routeCase says how to call a route and which fake must receive it. A new route in
// the table with no case fails TestEveryRouteIsServed, so nothing goes unserved.
type routeCase struct {
	id     string // the {id} parameter, when the route has one
	body   string
	call   string
	status int
}

var routeCases = map[string]routeCase{
	api.RouteHealth:  {status: 200},
	api.RouteVersion: {status: 200},
	api.RouteMetrics: {call: "observability.Metrics", status: 200},
	api.RouteDoctor:  {call: "doctor.Run", status: 200},

	api.RouteWorkflowsList:     {call: "workflows.List", status: 200},
	api.RouteWorkflowsGet:      {id: "create-clip", call: "workflows.Get", status: 200},
	api.RouteWorkflowsValidate: {body: `{"definition":{"id":"x"}}`, call: "workflows.Validate", status: 200},
	api.RouteWorkflowsRun:      {id: "create-clip", body: `{"inputs":{"prompt":"p"}}`, call: "workflows.Run:create-clip", status: 202},
	api.RouteWorkflowsRunBody:  {body: `{"definition":{"id":"x","version":"1.0.0","nodes":[]}}`, call: "workflows.Run:", status: 202},

	api.RouteJobsList:    {call: "jobs.List", status: 200},
	api.RouteJobsGet:     {id: "JOB-001", call: "jobs.Get", status: 200},
	api.RouteJobsEvents:  {id: "JOB-001", call: "jobs.Subscribe", status: 200},
	api.RouteJobsLogs:    {id: "JOB-001", call: "jobs.Logs", status: 200},
	api.RouteJobsCancel:  {id: "JOB-001", call: "jobs.Cancel", status: 200},
	api.RouteJobsRetry:   {id: "JOB-001", call: "jobs.Retry", status: 200},
	api.RouteJobsPause:   {id: "JOB-001", call: "jobs.Pause", status: 200},
	api.RouteJobsResume:  {id: "JOB-001", call: "jobs.Resume", status: 200},
	api.RouteJobsResolve: {id: "JOB-001", body: `{"action":"fail"}`, call: "jobs.Resolve", status: 200},

	api.RouteUploadsCreate:   {body: `{"filename":"a.mp4","size_bytes":1,"sha256":"` + strings.Repeat("a", 64) + `"}`, status: 201},
	api.RouteUploadsGet:      {id: "UPL-009", status: 404},
	api.RouteUploadsChunk:    {id: "UPL-009", status: 404},
	api.RouteUploadsComplete: {id: "UPL-009", status: 404},
	api.RouteUploadsAbort:    {id: "UPL-009", status: 404},

	api.RouteAssetsList:         {call: "assets.List", status: 200},
	api.RouteAssetsGet:          {id: "ORI-001", call: "assets.Get", status: 200},
	api.RouteAssetsUpdate:       {id: "ORI-001", body: `{"title":"t"}`, call: "assets.Update", status: 200},
	api.RouteAssetsDelete:       {id: "CLIP-001", call: "workflows.Run:delete-asset", status: 202},
	api.RouteAssetsLineage:      {id: "CLIP-001", call: "assets.Lineage", status: 200},
	api.RouteAssetsVersions:     {id: "CLIP-001", call: "assets.Versions", status: 200},
	api.RouteAssetsAIProcessing: {id: "ORI-001", body: `{"ai_processing":"local_only"}`, call: "assets.SetAIProcessing", status: 200},
	api.RouteAssetsConsent:      {id: "ORI-001", call: "assets.Consent", status: 200},
	api.RouteAssetsTranscript:   {id: "ORI-001", call: "assets.Transcript", status: 200},
	api.RouteAssetsProxyLink:    {id: "ORI-001", call: "storages.ProxyLink", status: 200},
	api.RouteAssetsExport:       {id: "ORI-001", body: `{}`, call: "workflows.RunNode:catalog.export_package", status: 202},
	api.RouteSensitiveList:      {id: "ORI-001", call: "assets.ListSensitive", status: 200},
	api.RouteSensitiveAdd:       {id: "ORI-001", body: `{"start_seconds":1,"end_seconds":2}`, call: "assets.AddSensitive", status: 201},
	api.RouteSensitiveRemove:    {id: "ORI-001", body: `{"start_seconds":1,"end_seconds":2}`, call: "assets.RemoveSensitive", status: 200},

	api.RouteCollectionsList:         {call: "assets.Collections", status: 200},
	api.RouteCollectionsAIProcessing: {body: `{"ai_processing":"local_only"}`, call: "assets.SetCollectionAIProcessing", status: 200},

	api.RouteClipsCreate: {body: `{"source_id":"ORI-001","prompt":"p"}`, call: "workflows.CreateClip", status: 202},
	api.RouteSearch:      {call: "search.Search", status: 200},

	api.RouteApprovalsList:    {call: "approvals.List", status: 200},
	api.RouteApprovalsGet:     {id: "APR-001", call: "approvals.Get", status: 200},
	api.RouteApprovalsApprove: {id: "APR-001", body: `{"note":"ok"}`, call: "approvals.Approve", status: 200},
	api.RouteApprovalsReject:  {id: "APR-001", body: `{"reason":"no"}`, call: "approvals.Reject", status: 200},

	api.RoutePublicationsCreate:   {body: `{"asset_id":"CLIP-001","destinations":["mock-main"]}`, call: "publications.Request", status: 202},
	api.RoutePublicationsList:     {call: "publications.List", status: 200},
	api.RoutePublicationsGet:      {id: "PUB-001", call: "publications.Get", status: 200},
	api.RoutePublicationsWithdraw: {id: "PUB-001", call: "publications.WithdrawOne", status: 202},
	api.RoutePublicationsUnpub:    {body: `{"asset_id":"CLIP-001","all":true}`, call: "publications.Withdraw", status: 202},
	api.RoutePublicationsSync:     {id: "PUB-001", call: "publications.Sync", status: 200},
	api.RouteDestinationsList:     {call: "publications.Destinations", status: 200},

	api.RouteNodesList:    {call: "introspection.Nodes", status: 200},
	api.RouteNodesGet:     {call: "introspection.Node", status: 200},
	api.RouteNodesRun:     {body: `{"parameters":{"x":1}}`, call: "workflows.RunNode:video.cut", status: 202},
	api.RouteConnectors:   {call: "connectors.List", status: 200},
	api.RoutePluginsList:  {call: "plugins.List", status: 200},
	api.RoutePluginsGet:   {id: "notifications", call: "plugins.Get", status: 200},
	api.RoutePluginsAdd:   {body: `{"path":"/plugins/x"}`, call: "plugins.Install", status: 201},
	api.RouteHooksList:    {call: "introspection.Hooks", status: 200},
	api.RouteHooksTest:    {id: "validate-source", body: `{}`, call: "introspection.TestHook", status: 200},
	api.RouteEventsList:   {call: "events.List", status: 200},
	api.RouteEventsDeliv:  {call: "events.Deliveries", status: 200},
	api.RouteEventsStream: {call: "events.Subscribe", status: 200},

	api.RouteStoragesList:    {call: "storages.List", status: 200},
	api.RouteStoragesTest:    {call: "storages.Test", status: 200},
	api.RouteStoragesVerify:  {body: `{"asset_id":"ORI-001"}`, call: "storages.Verify", status: 200},
	api.RouteStoragesCopy:    {body: `{"from":"master","to":"proxy","asset_id":"ORI-001"}`, status: 409},
	api.RouteStoragesRestore: {body: `{"asset_id":"ORI-001","to":"master"}`, call: "assets.Versions", status: 200},

	api.RouteSecretsList:   {call: "secrets.List", status: 200},
	api.RouteSecretsPut:    {body: `{"name":"gcs-main","location":"server","value":"fictional"}`, call: "secrets.Put", status: 201},
	api.RouteSecretsDelete: {call: "secrets.Delete", status: 200},
	api.RouteSecretsTest:   {call: "secrets.Test", status: 200},
	api.RouteSecretsRotate: {body: `{"value":"fictional"}`, call: "secrets.Rotate", status: 200},

	api.RoutePairingCodes:  {body: `{}`, call: "devices.CreatePairingCode", status: 201},
	api.RouteDevicesPair:   {body: `{"code":"ABCDE-FGHIJ","device_name":"laptop"}`, call: "devices.Pair", status: 201},
	api.RouteDevicesList:   {call: "devices.List", status: 200},
	api.RouteDevicesRevoke: {id: "CLIENT-001", call: "devices.Revoke", status: 200},
	api.RouteDevicesRotate: {id: "CLIENT-001", call: "devices.Rotate", status: 200},
	api.RouteClientsList:   {call: "devices.List", status: 200},
	api.RouteClientsRevoke: {id: "CLIENT-001", call: "devices.Revoke", status: 200},

	api.RouteTenantsList: {call: "tenants.List", status: 200},
	api.RouteTenantsAdd:  {body: `{"id":"initech"}`, call: "tenants.Create", status: 201},
	api.RouteUsersList:   {call: "users.List", status: 200},
	api.RouteUsersAdd:    {body: `{"email":"maria@example.com","role":"editor"}`, call: "users.Add", status: 201},
	api.RouteUsersGrant:  {id: "maria@example.com", body: `{"permission":"asset.delete"}`, call: "users.Grant", status: 200},
	api.RouteBudgetGet:   {call: "budgets.Status", status: 200},
	api.RouteBudgetPut:   {body: `{"monthly_minor":50000,"currency":"USD"}`, call: "budgets.Set", status: 200},
	api.RouteBudgetOver:  {body: `{"reason":"end-of-month release"}`, call: "budgets.Override", status: 200},
	api.RouteAuditList:   {call: "audit.List", status: 200},
	api.RouteAuditVerify: {call: "audit.Verify", status: 200},
	api.RouteAuditExport: {call: "audit.Export", status: 200},
	api.RouteLogsList:    {call: "observability.Logs", status: 200},
	api.RouteLogsStream:  {call: "observability.SubscribeLogs", status: 200},
	api.RouteObsList:     {call: "observability.Sinks", status: 200},
	api.RouteObsStatus:   {call: "observability.Sinks", status: 200},
	api.RouteObsTest:     {id: "loki", call: "observability.TestSink", status: 200},
	api.RouteObsEnable:   {id: "loki", call: "observability.SetSinkEnabled", status: 200},
	api.RouteObsDisable:  {id: "loki", call: "observability.SetSinkEnabled", status: 200},

	api.RouteWorkerRegister:      {body: `{"worker_id":"w1","pools":["media"],"resources":{"cpu_cores":1,"memory_mb":1,"disk_mb":1,"gpu":false,"max_concurrency":1}}`, call: "worker.Register", status: 200},
	api.RouteWorkerHeartbeat:     {body: `{"worker_id":"w1","running":0}`, call: "worker.Heartbeat", status: 200},
	api.RouteWorkerLease:         {id: taskID, body: `{"worker_id":"w1"}`, call: "worker.Lease", status: 200},
	api.RouteWorkerTaskHeartbeat: {id: taskID, body: `{"lease_id":"l1"}`, call: "worker.TaskHeartbeat", status: 200},
	api.RouteWorkerTaskLogs:      {id: taskID, body: `{"lease_id":"l1","lines":[]}`, call: "worker.TaskLogs", status: 200},
	api.RouteWorkerComplete:      {id: taskID, body: `{"lease_id":"l1","output":{}}`, call: "worker.Complete", status: 200},
	api.RouteWorkerFail:          {id: taskID, body: `{"lease_id":"l1","code":"x","message":"m","retryable":false,"outcome_unknown":false}`, call: "worker.Fail", status: 200},
	api.RouteWorkerConnection:    {id: taskID, body: `{"lease_id":"l1","role":"master"}`, call: "worker.Connection", status: 200},
	api.RouteWorkerCacheKey:      {id: taskID, body: `{"lease_id":"l1"}`, call: "worker.CacheKey", status: 200},
}

const taskID = "01J0000000000000000000000Z.node_1.1"

func pathFor(rt api.Route, c routeCase) string {
	idValue := c.id
	if idValue == "" {
		idValue = "ORI-001"
	}
	return api.Prefix + strings.NewReplacer(
		"{id}", idValue, "{name}", "gcs-main", "{role}", "master", "{type}", "video.cut",
	).Replace(rt.Path)
}

// tokenFor returns a caller allowed on the route, in tenantA where a tenant applies.
func tokenFor(rt api.Route) string {
	switch {
	case rt.Auth == api.AuthWorker:
		return tokenWorker
	case rt.Permission.SystemLevel():
		return tokenSystem
	default:
		return tokenAdmin
	}
}

func requestFor(rt api.Route, c routeCase) request {
	req := request{method: rt.Method, path: pathFor(rt, c), token: tokenFor(rt), body: c.body}
	if rt.Auth == api.AuthNone {
		req.token = ""
	}
	if rt.Tenant != api.TenantNone {
		req.tenant = tenantA
	}
	if rt.Mutating() {
		req.key = "key-" + rt.Name
	}
	if rt.Name == api.RouteUploadsChunk {
		req.headers = map[string]string{api.HeaderUploadOffset: "0"}
		req.body = "x"
	}
	if rt.Name == api.RouteSearch {
		req.path += "?q=breathing"
	}
	return req
}

func TestEveryRouteIsServed(t *testing.T) {
	for _, rt := range api.Routes {
		t.Run(rt.Name, func(t *testing.T) {
			c, ok := routeCases[rt.Name]
			if !ok {
				t.Fatalf("route %s has no test case; add one and make sure it is served", rt.Name)
			}
			w := newWorld(t)
			rec := do(w.serve(), requestFor(rt, c))
			if rec.Code != c.status {
				t.Fatalf("%s %s = %d, want %d: %s", rt.Method, rt.Path, rec.Code, c.status, rec.Body.String())
			}
			if rec.Header().Get(api.HeaderRequestID) == "" {
				t.Error("no request ID")
			}
			if c.call == "" {
				return
			}
			found := false
			for _, name := range w.callNames() {
				if name == c.call {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s never reached %s; calls: %v", rt.Name, c.call, w.callNames())
			}
		})
	}
	if len(routeCases) != len(api.Routes) {
		t.Errorf("%d cases for %d routes", len(routeCases), len(api.Routes))
	}
}

func TestListsAreNeverNull(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for _, name := range []string{
		api.RouteWorkflowsList, api.RouteJobsList, api.RouteAssetsList, api.RouteApprovalsList,
		api.RoutePublicationsList, api.RouteNodesList, api.RoutePluginsList, api.RouteConnectors,
		api.RouteEventsList, api.RouteSecretsList, api.RouteDevicesList, api.RouteTenantsList,
		api.RouteUsersList, api.RouteAuditList, api.RouteLogsList, api.RouteObsList, api.RouteStoragesList,
	} {
		rt := api.ByName(name)
		rec := do(h, requestFor(rt, routeCases[name]))
		wantStatus(t, rec, http.StatusOK)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(body["items"]) != "[]" {
			t.Errorf("%s: items = %s", name, body["items"])
		}
	}
}

func TestUnauthenticatedIsRefusedEverywhere(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for _, rt := range api.Routes {
		if rt.Auth == api.AuthNone {
			continue
		}
		req := requestFor(rt, routeCases[rt.Name])
		req.token = ""
		rec := do(h, req)
		wantError(t, rec, http.StatusUnauthorized, errs.CodeUnauthenticated)
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate", rt.Name)
		}
	}
	if calls := w.callNames(); len(calls) != 0 {
		t.Fatalf("unauthenticated requests reached: %v", calls)
	}
}

func TestBadCredentialsAndWrongKinds(t *testing.T) {
	w := newWorld(t)
	h := w.serve()

	rec := do(h, request{method: "GET", path: "/api/v1/assets", token: tokenRevoked, tenant: tenantA})
	wantError(t, rec, http.StatusUnauthorized, errs.CodeUnauthenticated)

	rec = do(h, request{method: "GET", path: "/api/v1/assets", tenant: tenantA, headers: map[string]string{"Authorization": "Basic abc"}})
	wantError(t, rec, http.StatusUnauthorized, errs.CodeUnauthenticated)

	// A worker token is no user credential, and the reverse.
	rec = do(h, request{method: "GET", path: "/api/v1/assets", token: tokenWorker, tenant: tenantA})
	wantError(t, rec, http.StatusUnauthorized, errs.CodeUnauthenticated)
	rec = do(h, request{method: "POST", path: "/api/v1/worker/heartbeat", token: tokenAdmin, body: `{"worker_id":"w","running":0}`})
	wantError(t, rec, http.StatusUnauthorized, errs.CodeUnauthenticated)

	if calls := w.callNames(); len(calls) != 0 {
		t.Fatalf("refused requests reached: %v", calls)
	}
}

func TestTenantHeaderIsRequired(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for _, rt := range api.Routes {
		if rt.Tenant != api.TenantRequired {
			continue
		}
		req := requestFor(rt, routeCases[rt.Name])
		req.tenant = ""
		wantError(t, do(h, req), http.StatusBadRequest, errs.CodeTenantRequired)
	}
	if calls := w.callNames(); len(calls) != 0 {
		t.Fatalf("requests without a tenant reached: %v", calls)
	}
}

func TestUnauthenticatedLearnsNothingAboutTenants(t *testing.T) {
	w := newWorld(t)
	rec := do(w.serve(), request{method: "GET", path: "/api/v1/assets"})
	wantError(t, rec, http.StatusUnauthorized, errs.CodeUnauthenticated)
}

func TestMalformedTenantHeader(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for _, bad := range []string{"../evil", "ACME", "a", "acme/x", "acme name"} {
		rec := do(h, request{method: "GET", path: "/api/v1/assets", token: tokenAdmin, headers: map[string]string{api.HeaderTenant: bad}})
		wantError(t, rec, http.StatusBadRequest, errs.CodeInvalidArgument)
	}
}

func TestForeignTenantLooksMissing(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for _, rt := range api.Routes {
		if rt.Tenant != api.TenantRequired {
			continue
		}
		req := requestFor(rt, routeCases[rt.Name])
		req.tenant = tenantB // admin of acme is no member of globex
		foreign := do(h, req)
		wantError(t, foreign, http.StatusNotFound, errs.CodeNotFound)

		req.tenant = "does-not-exist"
		missing := do(h, req)
		wantError(t, missing, http.StatusNotFound, errs.CodeNotFound)

		var a, b errs.Body
		_ = json.Unmarshal(foreign.Body.Bytes(), &a)
		_ = json.Unmarshal(missing.Body.Bytes(), &b)
		if a.Error.Message != b.Error.Message {
			t.Fatalf("%s: foreign and missing tenants differ: %q vs %q", rt.Name, a.Error.Message, b.Error.Message)
		}
	}
	if calls := w.callNames(); len(calls) != 0 {
		t.Fatalf("cross-tenant requests reached: %v", calls)
	}
}

func TestSystemAdminNeedsAMembership(t *testing.T) {
	w := newWorld(t)
	rec := do(w.serve(), request{method: "GET", path: "/api/v1/assets", token: tokenSystem, tenant: tenantA})
	wantError(t, rec, http.StatusNotFound, errs.CodeNotFound)
}

func TestRoutePermissionsAreEnforced(t *testing.T) {
	// viewer@ is a viewer of acme: below every route that needs more, and nothing
	// in this table is decided per resource.
	w := newWorld(t)
	h := w.serve()
	for _, rt := range api.Routes {
		if rt.Permission == "" || serviceDecidesPermission[rt.Name] || domain.RoleViewer.Has(rt.Permission) {
			continue
		}
		t.Run(rt.Name, func(t *testing.T) {
			w.resetCalls()
			req := requestFor(rt, routeCases[rt.Name])
			req.token = tokenViewer
			if rt.Tenant != api.TenantNone {
				req.tenant = tenantA
			}
			body := wantError(t, do(h, req), http.StatusForbidden, errs.CodeForbidden)
			if body.Details["permission"] != string(rt.Permission) {
				t.Errorf("details = %v", body.Details)
			}
			for _, name := range w.callNames() {
				if !strings.HasPrefix(name, "audit:") {
					t.Fatalf("a denied request reached %s", name)
				}
			}
			records := w.auditRecords()
			if rt.Mutating() && (len(records) != 1 || records[0].Result != resultDenied) {
				t.Fatalf("a denied mutation must be audited: %+v", records)
			}
			if !rt.Mutating() && len(records) != 0 {
				t.Fatalf("a denied read wrote the audit: %+v", records)
			}
		})
	}
}

// The authorization rows of §15.6.
func TestUnauthorizedClientCannotDoPrivilegedThings(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for name, token := range map[string]string{
		api.RoutePluginsAdd:         tokenEditor,
		api.RouteStoragesRestore:    tokenPublisher,
		api.RouteAssetsDelete:       tokenPublisher,
		api.RoutePublicationsCreate: tokenEditor,
		api.RouteSecretsPut:         tokenPublisher,
		api.RouteTenantsAdd:         tokenAdmin,
	} {
		rt := api.ByName(name)
		req := requestFor(rt, routeCases[name])
		req.token = token
		wantError(t, do(h, req), http.StatusForbidden, errs.CodeForbidden)
	}
	for _, c := range w.callNames() {
		if !strings.HasPrefix(c, "audit:") {
			t.Fatalf("a denied request reached %s", c)
		}
	}
}

func TestOptionalTenantRoutes(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	pairing := api.ByName(api.RoutePairingCodes)
	post := func(token string, tenant domain.TenantID) request {
		return request{method: pairing.Method, path: api.Prefix + pairing.Path, token: token, tenant: tenant, body: `{}`}
	}

	// A system admin needs no tenant and no membership.
	wantStatus(t, do(h, post(tokenSystem, "")), http.StatusCreated)
	if got := w.lastCall(t).Scope.Tenant; got != "" {
		t.Fatalf("tenant = %q", got)
	}

	// A tenant admin acts inside the tenant, and is told how when no tenant is sent.
	wantStatus(t, do(h, post(tokenAdmin, tenantA)), http.StatusCreated)
	if got := w.lastCall(t).Scope; got.Tenant != tenantA || got.Principal.Role != domain.RoleAdmin {
		t.Fatalf("scope = %+v", got)
	}
	wantError(t, do(h, post(tokenAdmin, "")), http.StatusForbidden, errs.CodeForbidden)

	// Below admin, and outside the tenant.
	wantError(t, do(h, post(tokenEditor, tenantA)), http.StatusForbidden, errs.CodeForbidden)
	wantError(t, do(h, post(tokenAdmin, tenantB)), http.StatusNotFound, errs.CodeNotFound)
}

func TestAnyoneMayAskTheServiceToRevokeTheirOwnDevice(t *testing.T) {
	// Scope is decided by service.Devices (I112), so a viewer gets through to it.
	w := newWorld(t)
	h := w.serve()
	for _, path := range []string{"/api/v1/devices/CLIENT-002/revoke", "/api/v1/clients/CLIENT-002/revoke"} {
		w.resetCalls()
		wantStatus(t, do(h, request{method: "POST", path: path, token: tokenViewer}), http.StatusOK)
		if got := w.lastCall(t); got.Name != "devices.Revoke" || got.Args != any(idRef("CLIENT-002")) {
			t.Fatalf("call = %+v", got)
		}
	}
}

func TestDevicesListNeedsNoTenant(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	wantStatus(t, do(h, request{method: "GET", path: "/api/v1/devices", token: tokenViewer}), http.StatusOK)
	if got := w.lastCall(t).Scope; got.Tenant != "" || got.Principal.Email != "viewer@example.com" {
		t.Fatalf("scope = %+v", got)
	}
}

func TestTenantRoutesIgnoreTheHeaderWhenFree(t *testing.T) {
	w := newWorld(t)
	rec := do(w.serve(), request{method: "GET", path: "/api/v1/tenants", token: tokenViewer, tenant: "not-a-member"})
	wantStatus(t, rec, http.StatusOK)
}

func TestPeerIdentityAuthenticates(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	uid := 1000
	r := newRequest(t, "GET", "/api/v1/assets")
	r = r.WithContext(WithConnInfo(r.Context(), ConnInfo{PeerUID: &uid, Transport: "unix"}))
	r.Header.Set(api.HeaderTenant, string(tenantA))
	rec := serveRequest(h, r)
	wantStatus(t, rec, http.StatusOK)
	if got := w.lastCall(t).Scope.Principal; got.Method != domain.AuthPeer || got.Email != "admin@example.com" {
		t.Fatalf("principal = %+v", got)
	}

	other := 2000
	r = newRequest(t, "GET", "/api/v1/assets")
	r = r.WithContext(WithConnInfo(r.Context(), ConnInfo{PeerUID: &other, Transport: "unix"}))
	r.Header.Set(api.HeaderTenant, string(tenantA))
	wantError(t, serveRequest(h, r), http.StatusUnauthorized, errs.CodeUnauthenticated)
}

func TestPathParameters(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	get := func(path string) request {
		return request{method: "GET", path: path, token: tokenAdmin, tenant: tenantA}
	}
	// A valid ID of another resource is simply not here.
	wantError(t, do(h, get("/api/v1/jobs/ORI-001")), http.StatusNotFound, errs.CodeNotFound)
	wantError(t, do(h, get("/api/v1/assets/JOB-001")), http.StatusNotFound, errs.CodeNotFound)
	// Malformed.
	wantError(t, do(h, get("/api/v1/jobs/hello")), http.StatusBadRequest, errs.CodeInvalidArgument)
	wantError(t, do(h, get("/api/v1/assets/ORI-1")), http.StatusBadRequest, errs.CodeInvalidArgument)
	// ULIDs always parse; the service resolves them.
	wantStatus(t, do(h, get("/api/v1/jobs/01J0000000000000000000000Z")), http.StatusOK)
	// A role that does not exist.
	rec := do(h, request{method: "POST", path: "/api/v1/storages/cold/test", token: tokenAdmin, tenant: tenantA, key: "k"})
	wantError(t, rec, http.StatusBadRequest, errs.CodeInvalidArgument)
	if strings.Contains(strings.Join(w.callNames(), ","), "storages.Test") {
		t.Fatal("an unknown role reached the service")
	}
}

func TestUnknownPathsAndMethods(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	wantError(t, do(h, request{method: "GET", path: "/api/v1/nope"}), http.StatusNotFound, errs.CodeNotFound)
	wantError(t, do(h, request{method: "GET", path: "/"}), http.StatusNotFound, errs.CodeNotFound)
	rec := do(h, request{method: "PUT", path: "/api/v1/health"})
	wantError(t, rec, http.StatusMethodNotAllowed, errs.CodeInvalidArgument)
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q", allow)
	}
}

func TestMissingServicesAnswerNotImplemented(t *testing.T) {
	w := newWorld(t)
	h := NewHandler(Deps{Authenticator: authn{w}, Authorizer: authz{w}, Auditor: auditor{w}, TenantStores: w.stores, Logger: w.deps().Logger})
	for _, rt := range api.Routes {
		if rt.Name == api.RouteHealth || rt.Name == api.RouteVersion {
			continue
		}
		rec := do(h, requestFor(rt, routeCases[rt.Name]))
		wantError(t, rec, http.StatusNotImplemented, errs.CodeNotImplemented)
	}
}

func TestMissingIdentityFailsClosed(t *testing.T) {
	h := NewHandler(Deps{Logger: newWorld(t).deps().Logger})
	rec := do(h, request{method: "GET", path: "/api/v1/assets", token: tokenAdmin, tenant: tenantA})
	wantError(t, rec, http.StatusServiceUnavailable, errs.CodeUnavailable)
	rec = do(h, request{method: "POST", path: "/api/v1/worker/heartbeat", token: tokenWorker, body: "{}"})
	wantError(t, rec, http.StatusServiceUnavailable, errs.CodeUnavailable)
}

func TestErrorModel(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	get := request{method: "GET", path: "/api/v1/assets/ORI-001", token: tokenAdmin, tenant: tenantA}

	for code, status := range map[errs.Code]int{
		errs.CodeNotFound:           404,
		errs.CodeConflict:           409,
		errs.CodeFailedPrecondition: 409,
		errs.CodeForbidden:          403,
		errs.CodeBudgetExceeded:     402,
		errs.CodeGone:               410,
		errs.CodeValidationFailed:   422,
		errs.CodeNotImplemented:     501,
		errs.CodeUnavailable:        503,
		errs.CodeSealed:             503,
		errs.CodeTimeout:            504,
	} {
		w.err = errs.New(code, "stated reason")
		body := wantError(t, do(h, get), status, code)
		if body.Message != "stated reason" {
			t.Errorf("message = %q", body.Message)
		}
	}

	// An internal cause never reaches the client, and is in the log with the request ID.
	w.err = errs.Wrap(errs.CodeInternal, errString("secret-looking path /etc/x"), "the public part")
	rec := do(h, get)
	body := wantError(t, rec, http.StatusInternalServerError, errs.CodeInternal)
	if strings.Contains(rec.Body.String(), "secret-looking") || body.Message != "the public part" {
		t.Fatalf("cause leaked: %s", rec.Body.String())
	}

	w.err = errString("an uncoded failure with internal text")
	rec = do(h, get)
	wantError(t, rec, http.StatusInternalServerError, errs.CodeInternal)
	if strings.Contains(rec.Body.String(), "uncoded failure") {
		t.Fatalf("uncoded error leaked: %s", rec.Body.String())
	}
	if !strings.Contains(w.logText(), rec.Header().Get(api.HeaderRequestID)) {
		t.Fatal("the failure is not findable in the log by request ID")
	}
}

func TestPanicBecomesTheErrorBody(t *testing.T) {
	w := newWorld(t)
	w.obs.metrics = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	rec := do(w.serve(), request{method: "GET", path: "/api/v1/metrics", token: tokenSystem})
	wantError(t, rec, http.StatusInternalServerError, errs.CodeInternal)
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatal("panic text leaked")
	}
}

func TestStandardHeaders(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	rec := do(h, request{method: "GET", path: "/api/v1/version"})
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	first := rec.Header().Get(api.HeaderRequestID)
	second := do(h, request{method: "GET", path: "/api/v1/version"}).Header().Get(api.HeaderRequestID)
	if first == "" || first == second {
		t.Errorf("request IDs: %q, %q", first, second)
	}
	// A client's request ID is ignored (I124).
	rec = do(h, request{method: "GET", path: "/api/v1/version", headers: map[string]string{api.HeaderRequestID: "client-chosen"}})
	if rec.Header().Get(api.HeaderRequestID) == "client-chosen" {
		t.Error("client-chosen request ID accepted")
	}
}

func TestTraceParentPropagates(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	const parent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	rec := do(h, request{method: "GET", path: "/api/v1/assets", token: tokenAdmin, tenant: tenantA, headers: map[string]string{api.HeaderTraceParent: parent}})
	wantStatus(t, rec, http.StatusOK)
	if got := w.lastCall(t).Scope.TraceID; got != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("trace ID = %q", got)
	}
	if got := rec.Header().Get(api.HeaderTraceParent); !strings.HasPrefix(got, "00-0af7651916cd43dd8448eb211c80319c-") {
		t.Fatalf("traceparent = %q", got)
	}

	// Without one, a trace ID still exists, in valid form.
	w.resetCalls()
	rec = do(h, request{method: "GET", path: "/api/v1/assets", token: tokenAdmin, tenant: tenantA})
	got := w.lastCall(t).Scope.TraceID
	if len(got) != 32 || !strings.Contains(rec.Header().Get(api.HeaderTraceParent), got) {
		t.Fatalf("generated trace ID = %q, header %q", got, rec.Header().Get(api.HeaderTraceParent))
	}

	// A malformed one is not trusted.
	w.resetCalls()
	do(h, request{method: "GET", path: "/api/v1/assets", token: tokenAdmin, tenant: tenantA, headers: map[string]string{api.HeaderTraceParent: "garbage"}})
	if got := w.lastCall(t).Scope.TraceID; len(got) != 32 {
		t.Fatalf("trace ID from garbage = %q", got)
	}
}

func TestAccessLogHasNoQueryOrCredential(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	do(h, request{method: "GET", path: "/api/v1/search?q=private+words", token: tokenAdmin, tenant: tenantA})
	logs := w.logText()
	if strings.Contains(logs, "private") {
		t.Errorf("the query reached the log: %s", logs)
	}
	if strings.Contains(logs, tokenAdmin) {
		t.Errorf("the credential reached the log: %s", logs)
	}
	for _, want := range []string{`"route":"search"`, `"tenant_id":"acme"`, `"actor":"admin@example.com"`, `"device_id":"CLIENT-001"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log lacks %s: %s", want, logs)
		}
	}
}

func TestQueriesReachTheServices(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	get := func(path string) {
		t.Helper()
		wantStatus(t, do(h, request{method: "GET", path: path, token: tokenAdmin, tenant: tenantA}), http.StatusOK)
	}
	get("/api/v1/jobs?status=running&workflow=create-clip&asset_id=ORI-001&limit=5&cursor=c")
	if got := w.lastCall(t).Args.(api.JobQuery); got.Status != domain.JobRunning || got.Workflow != "create-clip" || got.AssetID != "ORI-001" || got.Limit != 5 || got.Cursor != "c" {
		t.Fatalf("job query = %+v", got)
	}
	get("/api/v1/assets?kind=clip&status=approved&source_id=ORI-001&collection=yoga")
	if got := w.lastCall(t).Args.(api.AssetQuery); got.Kind != "clip" || got.Status != "approved" || got.SourceID != "ORI-001" || got.Collection != "yoga" {
		t.Fatalf("asset query = %+v", got)
	}
	get("/api/v1/approvals?status=pending&kind=publication&asset_id=CLIP-001")
	if got := w.lastCall(t).Args.(api.ApprovalQuery); got.Status != "pending" || got.Kind != "publication" || got.AssetID != "CLIP-001" {
		t.Fatalf("approval query = %+v", got)
	}
	get("/api/v1/publications?asset_id=CLIP-001&destination=mock-main&status=published")
	if got := w.lastCall(t).Args.(api.PublicationQuery); got.AssetID != "CLIP-001" || got.Destination != "mock-main" || got.Status != "published" {
		t.Fatalf("publication query = %+v", got)
	}
	get("/api/v1/events?since=2026-11-08T00:00:00Z&type=clip.created")
	if got := w.lastCall(t).Args.(api.EventQuery); got.Since != "2026-11-08T00:00:00Z" || got.Type != "clip.created" {
		t.Fatalf("event query = %+v", got)
	}
	get("/api/v1/events/deliveries?status=failed")
	if got := w.lastCall(t).Args.(api.DeliveryQuery); got.Status != "failed" {
		t.Fatalf("delivery query = %+v", got)
	}
	get("/api/v1/audit?since=2026-11-08T00:00:00Z&actor=a&action=b&resource=c")
	if got := w.lastCall(t).Args.(api.AuditQuery); got.Actor != "a" || got.Action != "b" || got.Resource != "c" {
		t.Fatalf("audit query = %+v", got)
	}
	get("/api/v1/logs?since=2026-11-08T00:00:00Z&level=error&job_id=JOB-001")
	if got := w.lastCall(t).Args.(api.LogQuery); got.Level != "error" || got.JobID != "JOB-001" {
		t.Fatalf("log query = %+v", got)
	}
	get("/api/v1/search?q=breathing&asset_id=ORI-001&mode=lexical&limit=3")
	if got := w.lastCall(t).Args.(api.SearchQuery); got.Q != "breathing" || got.AssetID != "ORI-001" || got.Mode != "lexical" || got.Limit != 3 {
		t.Fatalf("search query = %+v", got)
	}
	get("/api/v1/doctor?check=security")
	if got := w.lastCall(t).Args; got != any("security") {
		t.Fatalf("doctor check = %v", got)
	}
	get("/api/v1/hooks?workflow=create-clip")
	if got := w.lastCall(t).Args; got != any("create-clip") {
		t.Fatalf("hooks workflow = %v", got)
	}
	get("/api/v1/assets/ORI-001/transcript?version=v2")
	if got := w.lastCall(t).Args; got != any("v2") {
		t.Fatalf("transcript version = %v", got)
	}
	get("/api/v1/workflows/create-clip?version=1.2.0")
	if got := w.lastCall(t).Args; got != any("create-clip@1.2.0") {
		t.Fatalf("workflow version = %v", got)
	}
}

func TestBadQueries(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	for _, path := range []string{
		"/api/v1/jobs?limit=0",
		"/api/v1/jobs?limit=abc",
		"/api/v1/jobs?limit=100000",
		"/api/v1/search",
		"/api/v1/search?q=x&mode=magic",
		"/api/v1/doctor?check=everything",
	} {
		rec := do(h, request{method: "GET", path: path, token: tokenAdmin, tenant: tenantA})
		wantError(t, rec, http.StatusBadRequest, errs.CodeInvalidArgument)
	}
}

func TestBodiesAreDecodedStrictly(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	post := func(body string) request {
		return request{method: "POST", path: "/api/v1/approvals/APR-001/approve", token: tokenPublisher, tenant: tenantA, key: keyFor(body), body: body}
	}
	// startSeconds would otherwise silently approve something else.
	rec := do(h, post(`{"startSeconds":1}`))
	wantError(t, rec, http.StatusBadRequest, errs.CodeInvalidArgument)
	if !strings.Contains(rec.Body.String(), "startSeconds") {
		t.Errorf("the message should name the field: %s", rec.Body.String())
	}
	wantError(t, do(h, post(`{"note":"a"} {"note":"b"}`)), http.StatusBadRequest, errs.CodeInvalidArgument)
	wantError(t, do(h, post(`{"note":`)), http.StatusBadRequest, errs.CodeInvalidArgument)
	wantError(t, do(h, post(`{"note":1}`)), http.StatusBadRequest, errs.CodeInvalidArgument)

	// Required bodies.
	rec = do(h, request{method: "POST", path: "/api/v1/clips", token: tokenEditor, tenant: tenantA, key: "k"})
	wantError(t, rec, http.StatusBadRequest, errs.CodeInvalidArgument)
}

func TestBadJSONNeverEchoesValues(t *testing.T) {
	w := newWorld(t)
	rec := do(w.serve(), request{method: "POST", path: "/api/v1/secrets", token: tokenAdmin, tenant: tenantA, body: `{"name":"x","location":"server","value":123456789}`})
	wantError(t, rec, http.StatusBadRequest, errs.CodeInvalidArgument)
	if strings.Contains(rec.Body.String(), "123456789") {
		t.Fatalf("a value reached the error: %s", rec.Body.String())
	}
}

func TestBodyLimits(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	huge := `{"note":"` + strings.Repeat("x", defaultBodyLimit) + `"}`
	rec := do(h, request{method: "POST", path: "/api/v1/approvals/APR-001/approve", token: tokenPublisher, tenant: tenantA, key: "k", body: huge})
	wantError(t, rec, http.StatusRequestEntityTooLarge, errs.CodePayloadTooLarge)

	rec = do(h, request{method: "POST", path: "/api/v1/devices/pair", body: `{"code":"` + strings.Repeat("x", pairBodyLimit) + `"}`})
	wantError(t, rec, http.StatusRequestEntityTooLarge, errs.CodePayloadTooLarge)
}

func TestSecretsAreMetadataOnly(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	const value = "fictional-secret-value-123"

	// A client-location secret must never reach the server (§9.1).
	rec := do(h, request{method: "POST", path: "/api/v1/secrets", token: tokenAdmin, tenant: tenantA, body: `{"name":"x","location":"client","value":"` + value + `"}`})
	wantError(t, rec, http.StatusBadRequest, errs.CodeInvalidArgument)

	rec = do(h, request{method: "POST", path: "/api/v1/secrets", token: tokenAdmin, tenant: tenantA, body: `{"name":"x","location":"server","value":"` + value + `"}`})
	wantStatus(t, rec, http.StatusCreated)
	if strings.Contains(rec.Body.String(), value) {
		t.Fatal("the response holds the value")
	}
	if strings.Contains(w.logText(), value) {
		t.Fatal("the log holds the value")
	}
	for _, record := range w.auditRecords() {
		if strings.Contains(string(mustJSON(t, record)), value) {
			t.Fatal("the audit holds the value")
		}
	}
	if n := w.stores.Store(tenantA).Records(); n != 0 {
		t.Fatalf("%d idempotency records for a request holding a secret", n)
	}

	wantError(t, do(h, request{method: "POST", path: "/api/v1/secrets", token: tokenAdmin, tenant: tenantA, body: `{"name":"../x","location":"server","value":"v"}`}), http.StatusBadRequest, errs.CodeInvalidArgument)
	wantError(t, do(h, request{method: "POST", path: "/api/v1/secrets", token: tokenAdmin, tenant: tenantA, body: `{"name":"x","location":"server"}`}), http.StatusBadRequest, errs.CodeInvalidArgument)
}

func TestRequestValidationInHandlers(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	cases := []request{
		{method: "POST", path: "/api/v1/workflows/validate", token: tokenAdmin, tenant: tenantA, body: `{}`},
		{method: "POST", path: "/api/v1/workflows/runs", token: tokenAdmin, tenant: tenantA, key: "k1", body: `{}`},
		{method: "POST", path: "/api/v1/workflows/create-clip/runs", token: tokenAdmin, tenant: tenantA, key: "k2", body: `{"definition":{"id":"x"}}`},
		{method: "POST", path: "/api/v1/tenants", token: tokenSystem, body: `{"id":"Not A Slug"}`},
		{method: "POST", path: "/api/v1/users", token: tokenAdmin, tenant: tenantA, key: "k3", body: `{"email":"a@example.com","role":"owner"}`},
		{method: "POST", path: "/api/v1/users/a@example.com/grants", token: tokenAdmin, tenant: tenantA, key: "k4", body: `{"permission":"system.admin"}`},
		{method: "POST", path: "/api/v1/budget/override", token: tokenAdmin, tenant: tenantA, key: "k5", body: `{"reason":"  "}`},
		{method: "POST", path: "/api/v1/devices/pair", body: `{}`},
	}
	for _, req := range cases {
		wantError(t, do(h, req), http.StatusBadRequest, errs.CodeInvalidArgument)
	}
}

func TestDryRunAnswers200(t *testing.T) {
	w := newWorld(t)
	rec := do(w.serve(), request{method: "POST", path: "/api/v1/workflows/ingest-original/runs", token: tokenEditor, tenant: tenantA, key: "k", body: `{"dry_run":true}`})
	wantStatus(t, rec, http.StatusOK)
}

func TestPairingCodeCarriesTheFingerprint(t *testing.T) {
	w := newWorld(t)
	h := w.serve(func(d *Deps) { d.TLSFingerprint = "sha256:abcdef" })
	rec := do(h, request{method: "POST", path: "/api/v1/pairing-codes", token: tokenSystem, body: `{}`})
	wantStatus(t, rec, http.StatusCreated)
	var body api.PairingCodeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.TLSFingerprint != "sha256:abcdef" || body.Code == "" {
		t.Fatalf("body = %+v", body)
	}
}

func TestPairingIsRateLimited(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	pair := request{method: "POST", path: "/api/v1/devices/pair", body: `{"code":"ABCDE"}`}
	for i := 0; i < 10; i++ {
		wantStatus(t, do(h, pair), http.StatusCreated)
	}
	rec := do(h, pair)
	wantError(t, rec, http.StatusTooManyRequests, errs.CodeRateLimited)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}

	// Another address is unaffected; the bucket is per IP.
	r := newRequest(t, "POST", "/api/v1/devices/pair")
	r.Body = http.NoBody
	r.RemoteAddr = "192.0.2.77:1234"
	rec = serveRequest(h, withBody(r, `{"code":"ABCDE"}`))
	wantStatus(t, rec, http.StatusCreated)
}

func TestWorkerRoutes(t *testing.T) {
	w := newWorld(t)
	h := w.serve()
	post := func(path, body string) request {
		return request{method: "POST", path: path, token: tokenWorker, body: body}
	}

	// Lease errors keep their codes: 410 means ack and drop, 409 means leave it.
	lease := "/api/v1/worker/tasks/" + taskID + "/lease"
	w.err = errs.New(errs.CodeGone, "obsolete")
	wantError(t, do(h, post(lease, `{"worker_id":"w"}`)), http.StatusGone, errs.CodeGone)
	w.err = errs.New(errs.CodeConflict, "incompatible")
	wantError(t, do(h, post(lease, `{"worker_id":"w"}`)), http.StatusConflict, errs.CodeConflict)
	w.err = nil

	// Credentials arrive for the worker, and never reach the log.
	rec := do(h, post("/api/v1/worker/tasks/"+taskID+"/connection", `{"lease_id":"l","role":"master"}`))
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("credentials must not be cached")
	}
	if strings.Contains(w.logText(), "fictional-credential") {
		t.Fatal("a credential reached the log")
	}

	// Worker routes need no idempotency key and write no audit (I123).
	if len(w.auditRecords()) != 0 {
		t.Fatalf("worker routes audited: %+v", w.auditRecords())
	}
	if got := w.logText(); !strings.Contains(got, `"actor":"worker:worker-1"`) {
		t.Errorf("the log does not name the worker: %s", got)
	}

	// The output cap of I35, at the door.
	huge := `{"lease_id":"l","output":"` + strings.Repeat("x", domain.MaxNodeOutputBytes) + `"}`
	rec = do(h, post("/api/v1/worker/tasks/"+taskID+"/complete", huge))
	wantError(t, rec, http.StatusRequestEntityTooLarge, errs.CodeOutputTooLarge)

	tooBig := strings.Repeat("x", completeBodyLimit+1)
	rec = do(h, post("/api/v1/worker/tasks/"+taskID+"/complete", tooBig))
	wantError(t, rec, http.StatusRequestEntityTooLarge, errs.CodePayloadTooLarge)
}
