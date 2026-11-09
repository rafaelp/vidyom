package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// §12.1's endpoint list (D45, D54, D59) plus R41 and the D6/D12/D19/D11 block.
var specEndpoints = []string{
	"GET /workflows", "POST /workflows/{id}/runs",
	"GET /jobs/{id}", "GET /jobs/{id}/events", "POST /jobs/{id}/cancel", "POST /jobs/{id}/retry",
	"POST /assets/uploads", "GET /assets/{id}", "GET /assets/{id}/lineage",
	"POST /clips", "POST /approvals/{id}/approve", "POST /approvals/{id}/reject",
	"POST /publications", "GET /nodes", "GET /connectors", "GET /plugins", "GET /health", "GET /metrics",
	"GET /search", "GET /assets", "GET /assets/{id}/versions", "GET /approvals", "GET /approvals/{id}",
	"POST /jobs/{id}/pause", "POST /jobs/{id}/resume", "DELETE /publications/{id}",
	"GET /events", "GET /events/deliveries",
	"GET /secrets", "POST /secrets", "DELETE /secrets/{name}",
	"GET /clients", "POST /clients/{id}/revoke", "GET /audit",
	"GET /tenants", "GET /users", "POST /users", "POST /pairing-codes", "POST /devices/pair",
	"GET /devices", "POST /devices/{id}/revoke", "POST /devices/{id}/rotate",
	"GET /budget", "PUT /budget", "POST /budget/override",
	"PUT /assets/{id}/ai-processing", "POST /jobs/{id}/resolve", "GET /audit/verify",
}

func TestSpecEndpointsPresent(t *testing.T) {
	have := map[string]Route{}
	for _, r := range Routes {
		have[r.Method+" "+r.Path] = r
	}
	for _, want := range specEndpoints {
		if _, ok := have[want]; !ok {
			t.Errorf("§12.1 endpoint missing from the route table: %s", want)
		}
	}
}

func TestRoutesRegisterWithoutConflict(t *testing.T) {
	// ServeMux panics on conflicting patterns, so this proves the table is servable.
	mux := http.NewServeMux()
	for _, r := range Routes {
		mux.HandleFunc(r.Pattern(), func(http.ResponseWriter, *http.Request) {})
	}
	for _, r := range Routes {
		path := strings.NewReplacer("{id}", "ORI-001", "{name}", "n", "{role}", "master", "{type}", "video.cut").Replace(r.Path)
		req := httptest.NewRequest(r.Method, Prefix+path, nil)
		_, pattern := mux.Handler(req)
		if pattern != r.Pattern() {
			t.Errorf("%s %s routed to %q, want %q", r.Method, path, pattern, r.Pattern())
		}
	}
}

func TestEveryRouteIsAuthorized(t *testing.T) {
	for _, r := range Routes {
		switch r.Auth {
		case AuthNone:
			if r.Name != RouteHealth && r.Name != RouteVersion && r.Name != RouteDevicesPair {
				t.Errorf("%s must not be unauthenticated", r.Name)
			}
		case AuthWorker:
			if r.Tenant != TenantNone {
				t.Errorf("%s: worker routes carry the tenant in the lease", r.Name)
			}
		case AuthUser:
			if r.Permission == "" && !r.Dynamic {
				t.Errorf("%s has neither a permission nor dynamic authorization", r.Name)
			}
		default:
			t.Errorf("%s has an unknown auth mode %q", r.Name, r.Auth)
		}
		if r.Tenant == TenantOptional && !r.Dynamic {
			t.Errorf("%s: an optional tenant needs dynamic authorization", r.Name)
		}
		if r.Stream && r.Mutating() {
			t.Errorf("%s: a stream must be a GET", r.Name)
		}
	}
}

func TestTenantFreeRoutesAreOnlyManagement(t *testing.T) {
	allowed := map[string]bool{
		RouteHealth: true, RouteVersion: true, RouteDevicesPair: true,
		RouteTenantsList: true, RouteTenantsAdd: true,
	}
	for _, r := range Routes {
		if r.Tenant == TenantNone && r.Auth != AuthWorker && !allowed[r.Name] {
			t.Errorf("%s is tenant-free, but D32 requires the header on tenant-scoped requests", r.Name)
		}
	}
}

func TestURL(t *testing.T) {
	got := ByName(RouteAssetsLineage).URL("id", "CLIP-001")
	if got != "/api/v1/assets/CLIP-001/lineage" {
		t.Fatalf("URL = %s", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a missing parameter must panic")
		}
	}()
	ByName(RouteAssetsGet).URL()
}

func TestByNameUnknownPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	ByName("nope")
}

func TestNamesListed(t *testing.T) {
	if len(RouteNames()) != len(Routes) {
		t.Fatal("name index out of sync")
	}
}
