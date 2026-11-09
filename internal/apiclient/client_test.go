package apiclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/apiclient"
	"github.com/rafaelp/vidyom/internal/apiclient/apitest"
	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

const testToken = "vdm_test_credential"

func newUnixClient(t *testing.T, srv *apitest.Server, tenant string) *apiclient.Client {
	t.Helper()
	c, err := apiclient.New(apiclient.Options{
		Endpoint: apiclient.Endpoint{Kind: apiclient.KindUnix, Socket: srv.ServeUnix()},
		Tenant:   tenant,
		Sleep:    func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func token(value string, err error) apiclient.TokenFunc {
	return func() (string, error) { return value, err }
}

func TestRequestFollowsRouteTable(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteJobsGet, http.StatusOK, api.JobDetail{Job: domain.Job{ID: "JOB-001", Status: domain.JobRunning}})
	c := newUnixClient(t, srv, "acme")

	job, err := apiclient.Invoke[api.JobDetail](context.Background(), c, apiclient.Call{
		Route: api.RouteJobsGet, Path: apiclient.PathParam("id", "JOB-001"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "JOB-001" {
		t.Fatalf("job = %+v", job)
	}
	call, _ := srv.Last(api.RouteJobsGet)
	if call.Path != "/api/v1/jobs/JOB-001" {
		t.Errorf("path = %s", call.Path)
	}
	if got := call.Header.Get(api.HeaderTenant); got != "acme" {
		t.Errorf("tenant header = %q", got)
	}
	if call.Header.Get(api.HeaderIdempotencyKey) != "" {
		t.Error("a read must not carry an idempotency key")
	}
}

func TestTenantRequiredFailsBeforeAnyRequest(t *testing.T) {
	srv := apitest.New(t)
	c := newUnixClient(t, srv, "")
	err := c.Do(context.Background(), apiclient.Call{Route: api.RouteAssetsList}, nil)
	if !errs.IsCode(err, errs.CodeTenantRequired) {
		t.Fatalf("err = %v", err)
	}
	if len(srv.Calls()) != 0 {
		t.Fatal("no request may leave without the required tenant")
	}
}

func TestTenantFreeRouteNeverCarriesTenant(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteHealth, http.StatusOK, api.HealthResponse{Status: "ok"})
	c := newUnixClient(t, srv, "acme")
	if err := c.Do(context.Background(), apiclient.Call{Route: api.RouteHealth}, nil); err != nil {
		t.Fatal(err)
	}
	call, _ := srv.Last(api.RouteHealth)
	if call.Header.Get(api.HeaderTenant) != "" {
		t.Error("tenant-free route carried the header")
	}
}

func TestOptionalTenantSentWhenKnown(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteDoctor, http.StatusOK, api.DoctorResponse{})
	c := newUnixClient(t, srv, "acme")
	if err := c.Do(context.Background(), apiclient.Call{Route: api.RouteDoctor}, nil); err != nil {
		t.Fatal(err)
	}
	call, _ := srv.Last(api.RouteDoctor)
	if call.Header.Get(api.HeaderTenant) != "acme" {
		t.Error("optional tenant not sent")
	}

	empty := newUnixClient(t, srv, "")
	if err := empty.Do(context.Background(), apiclient.Call{Route: api.RouteDoctor}, nil); err != nil {
		t.Fatal(err)
	}
	call, _ = srv.Last(api.RouteDoctor)
	if call.Header.Get(api.HeaderTenant) != "" {
		t.Error("optional tenant sent when unknown")
	}
}

func TestMutationCarriesIdempotencyKey(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteJobsCancel, http.StatusOK, domain.Job{ID: "JOB-001"})
	c := newUnixClient(t, srv, "acme")
	for _, key := range []string{"", "fixed-key"} {
		err := c.Do(context.Background(), apiclient.Call{
			Route: api.RouteJobsCancel, Path: apiclient.PathParam("id", "JOB-001"),
			Body: api.JobActionRequest{}, IdempotencyKey: key,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		call, _ := srv.Last(api.RouteJobsCancel)
		got := call.Header.Get(api.HeaderIdempotencyKey)
		if got == "" || (key != "" && got != key) {
			t.Errorf("key = %q, want %q", got, key)
		}
	}
}

func TestPathValuesAreEscaped(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteUsersGrant, http.StatusOK, domain.Membership{})
	c := newUnixClient(t, srv, "acme")
	err := c.Do(context.Background(), apiclient.Call{
		Route: api.RouteUsersGrant, Path: apiclient.PathParam("id", "a/../b@example.com"),
		Body: api.GrantRequest{Permission: domain.PermAssetRead},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Last(api.RouteUsersGrant); !ok {
		t.Fatal("an escaped value must stay inside the path segment and reach the same route")
	}
}

func TestErrorBodyDecoded(t *testing.T) {
	srv := apitest.New(t)
	srv.Error(api.RouteAssetsGet, errs.NotFound("asset").With("extra", "x"))
	c := newUnixClient(t, srv, "acme")
	err := c.Do(context.Background(), apiclient.Call{Route: api.RouteAssetsGet, Path: apiclient.PathParam("id", "ORI-001")}, nil)
	var coded *errs.Error
	if !errors.As(err, &coded) {
		t.Fatalf("err = %T %v", err, err)
	}
	if coded.Code != errs.CodeNotFound || coded.RequestID != "req-test" || coded.Details["extra"] != "x" {
		t.Fatalf("coded = %+v", coded)
	}
}

func TestNonAPIErrorBodyIsNotEchoed(t *testing.T) {
	srv := apitest.New(t)
	srv.Handle(api.RouteHealth, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>secret proxy page token=abc</html>"))
	})
	c := newUnixClient(t, srv, "")
	err := c.Do(context.Background(), apiclient.Call{Route: api.RouteHealth}, nil)
	if !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "token=abc") {
		t.Fatalf("raw body echoed: %v", err)
	}
}

func TestItemsAcceptsBothListForms(t *testing.T) {
	srv := apitest.New(t)
	c := newUnixClient(t, srv, "acme")

	srv.JSON(api.RouteTenantsList, http.StatusOK, api.List[api.TenantInfo]{Items: []api.TenantInfo{{Tenant: domain.Tenant{ID: "acme"}}}, NextCursor: "n"})
	list, err := apiclient.Items[api.TenantInfo](context.Background(), c, apiclient.Call{Route: api.RouteTenantsList})
	if err != nil || len(list.Items) != 1 || list.NextCursor != "n" {
		t.Fatalf("list = %+v, err = %v", list, err)
	}

	srv.JSON(api.RouteTenantsList, http.StatusOK, []api.TenantInfo{{Tenant: domain.Tenant{ID: "acme"}}})
	list, err = apiclient.Items[api.TenantInfo](context.Background(), c, apiclient.Call{Route: api.RouteTenantsList})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("bare array: list = %+v, err = %v", list, err)
	}
}

func TestAllFollowsCursors(t *testing.T) {
	srv := apitest.New(t)
	srv.Handle(api.RouteJobsList, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "":
			apitest.WriteJSON(w, 200, api.List[domain.Job]{Items: []domain.Job{{ID: "JOB-001"}}, NextCursor: "c2"})
		case "c2":
			apitest.WriteJSON(w, 200, api.List[domain.Job]{Items: []domain.Job{{ID: "JOB-002"}}})
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	})
	c := newUnixClient(t, srv, "acme")
	list, err := apiclient.All[domain.Job](context.Background(), c, apiclient.Call{
		Route: api.RouteJobsList, Query: apiclient.Query(api.JobQuery{Status: domain.JobRunning}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.NextCursor != "" {
		t.Fatalf("list = %+v", list)
	}
	for _, call := range srv.Calls(api.RouteJobsList) {
		if call.Query.Get("status") != "running" {
			t.Errorf("filter lost on a later page: %v", call.Query)
		}
	}
}

func TestQueryEncoderUsesContractTags(t *testing.T) {
	q := apiclient.Query(api.AssetQuery{
		PageQuery: api.PageQuery{Limit: 5, Cursor: "c"},
		Kind:      domain.KindClip, SourceID: "ORI-001",
	})
	want := map[string]string{"limit": "5", "cursor": "c", "kind": "clip", "source_id": "ORI-001"}
	if len(q) != len(want) {
		t.Fatalf("q = %v", q)
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
}

// ---- Credentials ----

func httpsClient(t *testing.T, url, fingerprint string, tok apiclient.TokenFunc) *apiclient.Client {
	t.Helper()
	c, err := apiclient.New(apiclient.Options{
		Endpoint: apiclient.Endpoint{Kind: apiclient.KindHTTPS, URL: url, Fingerprint: fingerprint},
		Tenant:   "acme",
		Token:    tok,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestHTTPSSendsCredentialOnUserRoutesOnly(t *testing.T) {
	srv := apitest.New(t)
	srv.Token = testToken
	srv.JSON(api.RouteAssetsList, 200, api.List[domain.Asset]{})
	srv.JSON(api.RouteHealth, 200, api.HealthResponse{Status: "ok"})
	url, fp := srv.ServeTLS()
	c := httpsClient(t, url, fp, token(testToken, nil))

	if err := c.Do(context.Background(), apiclient.Call{Route: api.RouteAssetsList}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), apiclient.Call{Route: api.RouteHealth}, nil); err != nil {
		t.Fatal(err)
	}
	call, _ := srv.Last(api.RouteHealth)
	if call.Header.Get("Authorization") != "" {
		t.Error("the credential was sent on an unauthenticated route")
	}
}

func TestHTTPSWithoutCredentialFailsClearly(t *testing.T) {
	srv := apitest.New(t)
	url, fp := srv.ServeTLS()

	cases := []struct {
		name string
		tok  apiclient.TokenFunc
		want string
	}{
		{"no provider", nil, "not paired"},
		{"not paired", token("", security.ErrKeyringNotFound), "not paired"},
		{"no keyring", token("", security.ErrKeyringUnavailable), "no usable OS keyring"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := httpsClient(t, url, fp, tc.tok)
			err := c.Do(context.Background(), apiclient.Call{Route: api.RouteAssetsList}, nil)
			if !errs.IsCode(err, errs.CodeUnauthenticated) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
			if len(srv.Calls()) != 0 {
				t.Fatal("nothing may be sent without a credential")
			}
		})
	}
}

func TestUnixFallsBackToPeerIdentity(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteAssetsList, 200, api.List[domain.Asset]{})
	socket := srv.ServeUnix()

	cases := []struct {
		name string
		tok  apiclient.TokenFunc
		want string
	}{
		{"no provider", nil, ""},
		{"no keyring", token("", security.ErrKeyringUnavailable), ""},
		{"not paired", token("", security.ErrKeyringNotFound), ""},
		{"paired", token(testToken, nil), "Bearer " + testToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := apiclient.New(apiclient.Options{
				Endpoint: apiclient.Endpoint{Kind: apiclient.KindUnix, Socket: socket},
				Tenant:   "acme", Token: tc.tok,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := c.Do(context.Background(), apiclient.Call{Route: api.RouteAssetsList}, nil); err != nil {
				t.Fatal(err)
			}
			call, _ := srv.Last(api.RouteAssetsList)
			if got := call.Header.Get("Authorization"); got != tc.want {
				t.Fatalf("Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- TLS ----

func TestPinMatchSucceeds(t *testing.T) {
	srv := apitest.New(t)
	srv.JSON(api.RouteHealth, 200, api.HealthResponse{Status: "ok"})
	url, fp := srv.ServeTLS()
	c := httpsClient(t, url, fp, nil)
	if err := c.Do(context.Background(), apiclient.Call{Route: api.RouteHealth}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPinMismatchFailsAndSendsNothing(t *testing.T) {
	srv := apitest.New(t)
	url, _ := srv.ServeTLS()
	wrong := strings.Repeat("ab", 32)
	c := httpsClient(t, url, wrong, token(testToken, nil))
	err := c.Do(context.Background(), apiclient.Call{Route: api.RouteAssetsList}, nil)
	var coded *errs.Error
	if !errors.As(err, &coded) {
		t.Fatalf("err = %v", err)
	}
	if coded.Details["reason"] != "tls_fingerprint_mismatch" {
		t.Fatalf("details = %v", coded.Details)
	}
	if apiclient.IsTransient(err) {
		t.Fatal("a pin mismatch must never be retried")
	}
	if len(srv.Calls()) != 0 {
		t.Fatal("no request may cross a mismatched pin")
	}
}

func TestSelfSignedWithoutPinIsRefused(t *testing.T) {
	srv := apitest.New(t)
	url, _ := srv.ServeTLS()
	c := httpsClient(t, url, "", token(testToken, nil))
	err := c.Do(context.Background(), apiclient.Call{Route: api.RouteAssetsList}, nil)
	var coded *errs.Error
	if !errors.As(err, &coded) || coded.Details["reason"] != "tls_untrusted" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "--fingerprint") {
		t.Fatalf("the error must say what to do: %v", err)
	}
	if len(srv.Calls()) != 0 {
		t.Fatal("no request may reach an untrusted server")
	}
}

func TestTLS12ServerIsRefused(t *testing.T) {
	srv := apitest.New(t)
	url, fp := srv.ServeTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
	c := httpsClient(t, url, fp, nil)
	err := c.Do(context.Background(), apiclient.Call{Route: api.RouteHealth}, nil)
	var coded *errs.Error
	if !errors.As(err, &coded) || coded.Details["reason"] != "tls_version" {
		t.Fatalf("TLS 1.2 must be refused with its own reason: %v", err)
	}
	if len(srv.Calls()) != 0 {
		t.Fatal("no request may cross TLS 1.2")
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var reached bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer other.Close()

	srv := apitest.New(t)
	srv.Handle(api.RouteAssetsList, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	})
	url, fp := srv.ServeTLS()
	c := httpsClient(t, url, fp, token(testToken, nil))
	err := c.Do(context.Background(), apiclient.Call{Route: api.RouteAssetsList}, nil)
	if !errs.IsCode(err, errs.CodeFailedPrecondition) || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("a redirect must surface as a clear error: %v", err)
	}
	if apiclient.IsTransient(err) {
		t.Fatal("a redirect must not be retried")
	}
	if reached {
		t.Fatal("the redirect was followed, which could leak the credential")
	}
}

func TestValidateHTTPSURL(t *testing.T) {
	good := []string{"https://media.example.internal", "https://media.example.internal:8443", "https://127.0.0.1:8443/"}
	for _, u := range good {
		if err := apiclient.ValidateHTTPSURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{
		"http://media.example.internal", "ftp://x", "media.example.internal", "https://",
		"https://user:pass@media.example.internal", "https://media.example.internal/prefix",
		"https://media.example.internal/?x=1",
	}
	for _, u := range bad {
		if err := apiclient.ValidateHTTPSURL(u); err == nil {
			t.Errorf("%s: expected an error", u)
		}
	}
	if _, err := apiclient.New(apiclient.Options{Endpoint: apiclient.Endpoint{Kind: apiclient.KindHTTPS, URL: "http://x"}}); err == nil {
		t.Error("New must refuse http://")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	colon := strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	for _, input := range []string{hex, strings.ToUpper(hex), colon, "sha256:" + hex, "SHA256:" + colon, " " + hex + " "} {
		got, err := apiclient.NormalizeFingerprint(input)
		if err != nil || got != hex {
			t.Errorf("%q: got %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "abc", strings.Repeat("zz", 32), hex + "ab"} {
		if _, err := apiclient.NormalizeFingerprint(input); err == nil {
			t.Errorf("%q: expected an error", input)
		}
	}
}

func TestDialFailureIsClear(t *testing.T) {
	c, err := apiclient.New(apiclient.Options{Endpoint: apiclient.Endpoint{Kind: apiclient.KindUnix, Socket: "/nonexistent/vidyomd.sock"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Do(context.Background(), apiclient.Call{Route: api.RouteHealth}, nil)
	if !errs.IsCode(err, errs.CodeUnavailable) || !strings.Contains(err.Error(), "/nonexistent/vidyomd.sock") {
		t.Fatalf("err = %v", err)
	}
}

func TestTransportErrorsDoNotRepeatTheQuery(t *testing.T) {
	c, err := apiclient.New(apiclient.Options{Endpoint: apiclient.Endpoint{Kind: apiclient.KindUnix, Socket: "/nonexistent/vidyomd.sock"}, Tenant: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Do(context.Background(), apiclient.Call{Route: api.RouteSearch, Query: apiclient.Query(api.SearchQuery{Q: "private words"})}, nil)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelledContextIsReported(t *testing.T) {
	srv := apitest.New(t)
	release := make(chan struct{})
	defer close(release)
	srv.Handle(api.RouteHealth, func(http.ResponseWriter, *http.Request) { <-release })
	c := newUnixClient(t, srv, "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.Do(ctx, apiclient.Call{Route: api.RouteHealth}, nil)
	if !errors.Is(err, context.DeadlineExceeded) && !errs.IsCode(err, errs.CodeTimeout) {
		t.Fatalf("err = %v", err)
	}
}
