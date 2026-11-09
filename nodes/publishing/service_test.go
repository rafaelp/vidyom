package publishingnodes

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/publicexport/fakestore"
)

type fakeWorkflows struct {
	service.Workflows
	workflow string
	req      api.RunWorkflowRequest
	calls    int
}

func (f *fakeWorkflows) Run(_ context.Context, _ service.Scope, workflow string, req api.RunWorkflowRequest) (api.RunWorkflowResponse, error) {
	f.workflow, f.req = workflow, req
	f.calls++
	return api.RunWorkflowResponse{JobID: "JOB-007", Workflow: workflow}, nil
}

func (f *fakeWorkflows) inputs(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(f.req.Inputs, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type fakeStores struct {
	repo.TenantStores
	stores map[domain.TenantID]repo.TenantStore
}

func (f fakeStores) ForTenant(_ context.Context, tenant domain.TenantID) (repo.TenantStore, error) {
	s, ok := f.stores[tenant]
	if !ok {
		return nil, errs.NotFound("tenant")
	}
	return s, nil
}

type serviceFixture struct {
	*harness
	wf    *fakeWorkflows
	svc   service.Publications
	scope service.Scope
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	h := newHarness(t)
	wf := &fakeWorkflows{}
	return &serviceFixture{
		harness: h,
		wf:      wf,
		svc: NewService(ServiceDeps{
			Stores:     fakeStores{stores: map[domain.TenantID]repo.TenantStore{tenantID: h.store, "globex": fakestore.New("globex")}},
			Workflows:  wf,
			Resolver:   h.resolver,
			Config:     tenants{h},
			Connectors: h.registry,
			Now:        func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) },
		}),
		scope: service.Scope{
			Tenant: tenantID, IdempotencyKey: "client-key", TraceID: "trace-1",
			Principal: domain.Principal{Email: "publisher@example.com", Role: domain.RolePublisher},
		},
	}
}

func TestServiceRequestRunsPublishVideoWithItsInputs(t *testing.T) {
	f := newServiceFixture(t)
	resp, err := f.svc.Request(f.ctx(), f.scope, api.CreatePublicationRequest{
		AssetID: "CLIP-001", Destinations: []string{mockDest}, Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.JobID != "JOB-007" || f.wf.workflow != "publish-video" || f.wf.req.IdempotencyKey != "client-key" {
		t.Fatalf("%+v, workflow %s, request %+v", resp, f.wf.workflow, f.wf.req)
	}
	in := f.wf.inputs(t)
	if in["assetId"] != "CLIP-001" || in["visibility"] != "public" {
		t.Fatalf("inputs %v", in)
	}
	if got := in["destinations"].([]any); len(got) != 1 || got[0] != mockDest {
		t.Fatalf("inputs %v", in)
	}
}

func TestServiceRequestPassesAFriendlyIDForAULID(t *testing.T) {
	f := newServiceFixture(t)
	asset := f.asset("CLIP-001")
	if _, err := f.svc.Request(f.ctx(), f.scope, api.CreatePublicationRequest{AssetID: string(asset.ULID), Destinations: []string{mockDest}}); err != nil {
		t.Fatal(err)
	}
	in := f.wf.inputs(t)
	if in["assetId"] != "CLIP-001" {
		t.Fatalf("inputs %v", in)
	}
	if _, set := in["visibility"]; set {
		t.Fatalf("no visibility requested, none passed: %v", in)
	}
}

func TestServiceRequestRefusals(t *testing.T) {
	f := newServiceFixture(t)
	f.cfg.Publishing.Destinations["vimeo-main"] = config.Destination{Driver: "vimeo"}
	cases := map[string]struct {
		req  api.CreatePublicationRequest
		code errs.Code
	}{
		"no asset":        {api.CreatePublicationRequest{Destinations: []string{mockDest}}, errs.CodeInvalidArgument},
		"no destinations": {api.CreatePublicationRequest{AssetID: "CLIP-001"}, errs.CodeInvalidArgument},
		"bad visibility":  {api.CreatePublicationRequest{AssetID: "CLIP-001", Destinations: []string{mockDest}, Visibility: "open"}, errs.CodeInvalidArgument},
		"unknown asset":   {api.CreatePublicationRequest{AssetID: "CLIP-999", Destinations: []string{mockDest}}, errs.CodeNotFound},
		"unknown dest":    {api.CreatePublicationRequest{AssetID: "CLIP-001", Destinations: []string{"nowhere"}}, errs.CodeInvalidArgument},
		"skeleton dest":   {api.CreatePublicationRequest{AssetID: "CLIP-001", Destinations: []string{"vimeo-main"}}, errs.CodeNotImplemented},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := f.svc.Request(f.ctx(), f.scope, tc.req); !errs.IsCode(err, tc.code) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if f.wf.calls != 0 {
		t.Fatalf("a refused request must start nothing: %d", f.wf.calls)
	}
}

func TestServiceWithdraw(t *testing.T) {
	f := newServiceFixture(t)

	if _, err := f.svc.Withdraw(f.ctx(), f.scope, api.WithdrawRequest{AssetID: "CLIP-001", All: true}); err != nil {
		t.Fatal(err)
	}
	if f.wf.workflow != "unpublish-video" {
		t.Fatalf("workflow %s", f.wf.workflow)
	}
	if in := f.wf.inputs(t); in["assetId"] != "CLIP-001" || in["all"] != true {
		t.Fatalf("inputs %v", in)
	}

	if _, err := f.svc.Withdraw(f.ctx(), f.scope, api.WithdrawRequest{AssetID: "CLIP-001", Destinations: []string{mockDest}}); err != nil {
		t.Fatal(err)
	}
	in := f.wf.inputs(t)
	if _, set := in["all"]; set {
		t.Fatalf("inputs %v", in)
	}

	calls := f.wf.calls
	for name, req := range map[string]api.WithdrawRequest{
		"no asset":     {All: true},
		"no selection": {AssetID: "CLIP-001"},
	} {
		if _, err := f.svc.Withdraw(f.ctx(), f.scope, req); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := f.svc.Withdraw(f.ctx(), f.scope, api.WithdrawRequest{AssetID: "CLIP-999", All: true}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Errorf("got %v", err)
	}
	if f.wf.calls != calls {
		t.Fatal("a refused request must start nothing")
	}
}

func TestServiceWithdrawOneTargetsTheRecordsDestination(t *testing.T) {
	f := newServiceFixture(t)
	pub := f.published()
	if _, err := f.svc.WithdrawOne(f.ctx(), f.scope, id.Ref(pub)); err != nil {
		t.Fatal(err)
	}
	in := f.wf.inputs(t)
	if in["assetId"] != "CLIP-001" || in["destinations"].([]any)[0] != mockDest {
		t.Fatalf("inputs %v", in)
	}
	if _, err := f.svc.WithdrawOne(f.ctx(), f.scope, "PUB-999"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestServiceReadsAreTenantScoped(t *testing.T) {
	f := newServiceFixture(t)
	pub := f.published()

	got, err := f.svc.Get(f.ctx(), f.scope, id.Ref(pub))
	if err != nil || string(got.ID) != pub {
		t.Fatalf("%+v, %v", got, err)
	}
	list, err := f.svc.List(f.ctx(), f.scope, api.PublicationQuery{AssetID: "CLIP-001", Status: domain.PubPublished})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("%+v, %v", list, err)
	}
	list, err = f.svc.List(f.ctx(), f.scope, api.PublicationQuery{Status: domain.PubWithdrawn})
	if err != nil || list.Items == nil || len(list.Items) != 0 {
		t.Fatalf("an empty list must encode as an array: %+v, %v", list, err)
	}

	foreign := f.scope
	foreign.Tenant = "globex"
	if _, err := f.svc.Get(f.ctx(), foreign, id.Ref(pub)); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("cross-tenant access must answer 404: %v", err)
	}
	if list, err := f.svc.List(f.ctx(), foreign, api.PublicationQuery{}); err != nil || len(list.Items) != 0 {
		t.Fatalf("%+v, %v", list, err)
	}
	if _, err := f.svc.Sync(f.ctx(), foreign, id.Ref(pub)); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
	unknown := f.scope
	unknown.Tenant = "nobody"
	if _, err := f.svc.Request(f.ctx(), unknown, api.CreatePublicationRequest{AssetID: "CLIP-001", Destinations: []string{mockDest}}); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestServiceSyncAppliesTheSameRulesAndEmits(t *testing.T) {
	f := newServiceFixture(t)
	pub := f.published()
	conn, err := f.resolver.Publishing(f.ctx(), tenantID, mockDest)
	f.must(err)
	f.must(conn.Delete(f.ctx(), f.publication(pub).RemoteID))

	got, err := f.svc.Sync(f.ctx(), f.scope, id.Ref(pub))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.PubWithdrawn {
		t.Fatalf("record %+v", got)
	}
	events := f.store.EventsOfType(event.PublicationWithdrawn)
	if len(events) != 1 || events[0].SchemaVersion != event.SchemaVersion || events[0].TraceID != "trace-1" ||
		events[0].TenantID != tenantID || events[0].AssetID != "CLIP-001" || events[0].EventID == "" {
		t.Fatalf("events %+v", events)
	}

	// With nothing to ask, a sync is a plain read.
	before := len(f.store.Events())
	again, err := f.svc.Sync(f.ctx(), f.scope, id.Ref(pub))
	if err != nil || again.Status != domain.PubWithdrawn || len(f.store.Events()) != before {
		t.Fatalf("%+v, %v", again, err)
	}

	prep := run[prepareOutput](f.harness, "publish.prepare", map[string]any{"assetId": "CLIP-001", "destination": mockDest, "approvalId": "APR-001"}, runOpts{key: "fresh"})
	unsent, err := f.svc.Sync(f.ctx(), f.scope, id.Ref(prep.PublicationID))
	if err != nil || unsent.Status != domain.PubPendingApproval {
		t.Fatalf("%+v, %v", unsent, err)
	}
}

func TestServiceDestinations(t *testing.T) {
	f := newServiceFixture(t)
	dest := f.cfg.Publishing.Destinations[mockDest]
	dest.Visibility = domain.VisibilityPrivate
	dest.CredentialsRef = "mock-oauth"
	f.cfg.Publishing.Destinations[mockDest] = dest
	f.cfg.Publishing.Destinations["vimeo-main"] = config.Destination{Driver: "vimeo"}
	f.cfg.Publishing.Destinations["ghost"] = config.Destination{Driver: "nothing-registered"}

	list, err := f.svc.Destinations(f.ctx(), f.scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Name != "ghost" || list[1].Name != mockDest || list[2].Name != "vimeo-main" {
		t.Fatalf("list %+v", list)
	}
	if list[0].Available || list[0].Reason == "" {
		t.Fatalf("unknown driver: %+v", list[0])
	}
	if !list[1].Available || list[1].Driver != "mock" || list[1].Visibility != domain.VisibilityPrivate {
		t.Fatalf("mock: %+v", list[1])
	}
	if list[2].Available || list[2].Reason == "" {
		t.Fatalf("skeleton: %+v", list[2])
	}
	raw, _ := json.Marshal(list)
	if string(raw) == "" || containsString(raw, "mock-oauth") {
		t.Fatalf("no credential reference in the answer: %s", raw)
	}
}

func containsString(raw []byte, s string) bool {
	for i := 0; i+len(s) <= len(raw); i++ {
		if string(raw[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}

var _ connector.Publishing = (*scripted)(nil)
