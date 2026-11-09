package cli_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

const fictionalToken = "vdm_01JC0000000000000000000009_fictional-device-credential"

func pairingAnswers(h *harness) {
	h.srv.JSON(api.RouteDevicesPair, 201, api.DeviceCredentialResponse{
		Device: domain.Device{ID: "CLIENT-007", ULID: "01JC0000000000000000000009", Email: "editor@example.com", Status: domain.DeviceActive},
		Token:  fictionalToken,
	})
}

func addRemote(t *testing.T, h *harness) (url, fingerprint string) {
	t.Helper()
	url, fingerprint = h.srv.ServeTLS()
	h.mustRun("server", "add", "production", "--url", url, "--fingerprint", fingerprint)
	return url, fingerprint
}

func TestPairingStoresCredentialOnlyInTheKeyring(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	pairingAnswers(h)

	out := h.mustRun("server", "pair", "production", "--code", "ABCDE-FGHJK-MNPQR-STVWX")
	assertContains(t, out, "CLIENT-007", "editor@example.com", "OS keyring")
	h.assertNoLeak(fictionalToken)

	stored, err := h.keyring.Get("vidyom", "device/production")
	if err != nil || stored != fictionalToken {
		t.Fatalf("keyring: %v", err)
	}
	assertContains(t, h.config(), "CLIENT-007")
	if strings.Contains(h.keyring.dump(), "probe") {
		t.Fatal("the keyring probe must leave nothing behind")
	}

	call, _ := h.srv.Last(api.RouteDevicesPair)
	body := call.JSONBody(t)
	if body["code"] != "ABCDE-FGHJK-MNPQR-STVWX" || body["device_name"] != "workstation" {
		t.Fatalf("body keys wrong: device_name = %v", body["device_name"])
	}
	if call.Header.Get("Authorization") != "" {
		t.Fatal("pairing is the only unauthenticated mutation and carries no credential")
	}

	// The JSON form has no token either.
	h.mustRun("server", "pair", "production", "--code", "ABCDE-FGHJK-MNPQR-STVWX", "--json")
	h.assertNoLeak(fictionalToken)
}

func TestPairedCredentialIsSentOverPinnedTLS(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	pairingAnswers(h)
	h.mustRun("server", "pair", "production", "--code", "CODE")
	h.srv.Token = fictionalToken
	h.srv.JSON(api.RouteJobsList, 200, api.List[domain.Job]{})
	h.mustRun("job", "list", "--tenant", "acme")
	call, _ := h.srv.Last(api.RouteJobsList)
	if call.Header.Get("Authorization") != "Bearer "+fictionalToken {
		t.Fatal("the stored credential must authenticate")
	}
	if call.Header.Get(api.HeaderTenant) != "acme" {
		t.Fatal("tenant header missing")
	}
}

// §15.6: no keyring means an explicit failure and no plaintext. The one-time code
// must survive it.
func TestPairingWithoutKeyringFailsClearlyAndKeepsTheCode(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	pairingAnswers(h)
	h.keyring.setErr = security.ErrKeyringUnavailable
	if code := h.run("server", "pair", "production", "--code", "CODE"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "no usable OS keyring", "plaintext")
	if len(h.srv.Calls(api.RouteDevicesPair)) != 0 {
		t.Fatal("the code must not be spent when the credential could not be stored")
	}
	if strings.Contains(h.config(), "CLIENT-007") {
		t.Fatal("no pairing may be recorded")
	}
}

func TestPairingWhenTheKeyringFailsLateNeverPrintsTheToken(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	pairingAnswers(h)
	h.keyring.failSetAfter = 1 // the probe passes, the real write fails
	if code := h.run("server", "pair", "production", "--code", "CODE"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "CLIENT-007", "revoke")
	h.assertNoLeak(fictionalToken)
}

func TestPairingCodeFromPromptOrStdin(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	pairingAnswers(h)

	h.stdin = "CODE-FROM-STDIN\n"
	h.mustRun("server", "pair", "production")
	call, _ := h.srv.Last(api.RouteDevicesPair)
	if call.JSONBody(t)["code"] != "CODE-FROM-STDIN" {
		t.Fatal("stdin code not used")
	}

	h.terminal = true
	h.password = func() ([]byte, error) { return []byte("CODE-FROM-PROMPT"), nil }
	h.mustRun("server", "pair", "production")
	call, _ = h.srv.Last(api.RouteDevicesPair)
	if call.JSONBody(t)["code"] != "CODE-FROM-PROMPT" {
		t.Fatal("prompt code not used")
	}
}

func TestRemoteWithoutPairingFailsClearly(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	if code := h.run("job", "list", "--tenant", "acme"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "not paired", "vidyom server pair production")
	if len(h.srv.Calls()) != 0 {
		t.Fatal("nothing may be sent without a credential")
	}
}

func TestRemoteWithoutKeyringFailsClearly(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	h.keyring.getErr = security.ErrKeyringUnavailable
	if code := h.run("job", "list", "--tenant", "acme"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "no usable OS keyring")
}

func TestUnixNeedsNoKeyringUntilPaired(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.keyring.getErr = errors.New("the keyring must not be touched")
	h.keyring.setErr = h.keyring.getErr
	h.srv.JSON(api.RouteJobsList, 200, api.List[domain.Job]{})
	h.mustRun("job", "list")
	call, _ := h.srv.Last(api.RouteJobsList)
	if call.Header.Get("Authorization") != "" {
		t.Fatal("an unpaired socket client relies on its peer identity")
	}
}

func TestUnixPairedSendsCredential(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	pairingAnswers(h)
	h.mustRun("server", "pair", "test", "--code", "CODE")
	h.srv.JSON(api.RouteJobsList, 200, api.List[domain.Job]{})
	h.mustRun("job", "list")
	call, _ := h.srv.Last(api.RouteJobsList)
	if call.Header.Get("Authorization") != "Bearer "+fictionalToken {
		t.Fatal("a paired socket client sends its credential")
	}
}

func TestServerAddValidation(t *testing.T) {
	h := newHarness(t)
	hex := strings.Repeat("ab", 32)
	bad := [][]string{
		{"server", "add", "x"},
		{"server", "add", "x", "--url", "https://a.example.internal", "--socket", "/s"},
		{"server", "add", "bad name", "--socket", "/s"},
		{"server", "add", "x", "--socket", "/s", "--fingerprint", hex},
		{"server", "add", "x", "--url", "https://a.example.internal", "--remote-socket", "/s"},
		{"server", "add", "x", "--url", "https://a.example.internal", "--fingerprint", hex, "--ca-file", "/c"},
		{"server", "add", "x", "--url", "https://a.example.internal", "--client-cert", "/c"},
	}
	for _, command := range bad {
		if code := h.run(command...); code != 2 {
			t.Errorf("%v: exit %d\n%s", command, code, h.stderr.String())
		}
	}
	refused := [][]string{
		{"server", "add", "x", "--url", "http://a.example.internal"},
		{"server", "add", "x", "--url", "https://user:pass@a.example.internal"},
		{"server", "add", "x", "--url", "https://a.example.internal/prefix"},
		{"server", "add", "x", "--url", "https://a.example.internal", "--fingerprint", "zz"},
		{"server", "add", "x", "--ssh", "-oProxyCommand=evil"},
		{"server", "add", "x", "--url", "https://a.example.internal", "--ca-file", "/nonexistent/ca.pem"},
	}
	for _, command := range refused {
		if code := h.run(command...); code != 1 {
			t.Errorf("%v: exit %d", command, code)
		}
	}
	if _, err := os.Stat(filepath.Join(h.dir, "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused server must leave no configuration")
	}
}

func TestServerAddListUseRemove(t *testing.T) {
	h := newHarness(t)
	hex := strings.Repeat("ab", 32)
	colons := strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	h.mustRun("server", "add", "production", "--url", "https://media.example.internal/", "--fingerprint", "SHA256:"+colons)
	h.mustRun("server", "add", "other", "--socket", "/run/other.sock")
	h.mustRun("server", "add", "ops-host", "--ssh", "operator@media.example.internal")

	var cfg struct {
		Current string `json:"current_server"`
		Servers map[string]struct {
			Kind, URL, Fingerprint, RemoteSocket string
		} `json:"servers"`
	}
	if err := json.Unmarshal([]byte(h.config()), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Current != "production" {
		t.Errorf("the first server becomes current: %q", cfg.Current)
	}
	assertContains(t, h.config(), hex, `"url": "https://media.example.internal"`, `"remote_socket": "/run/vidyom/vidyomd.sock"`)

	out := decodeJSON(t, h.mustRun("server", "list", "--json"))
	if items, _ := out["items"].([]any); len(items) != 3 {
		t.Fatalf("items = %v", out)
	}

	h.mustRun("server", "use", "other")
	assertContains(t, h.config(), `"current_server": "other"`)
	if code := h.run("server", "use", "missing"); code != 1 {
		t.Errorf("unknown server: exit %d", code)
	}

	if code := h.run("server", "add", "other", "--socket", "/run/new.sock"); code != 1 {
		t.Errorf("an existing name needs --force: exit %d", code)
	}

	h.keyring.entries["vidyom/device/production"] = fictionalToken
	h.mustRun("server", "remove", "other")
	assertContains(t, h.mustRun("server", "list"), "production")
	if strings.Contains(h.config(), "other") {
		t.Error("removed server still in the config")
	}
}

func TestForceWithANewEndpointDropsTheCredential(t *testing.T) {
	h := newHarness(t)
	url, fp := addRemote(t, h)
	pairingAnswers(h)
	h.mustRun("server", "pair", "production", "--code", "CODE")

	// The same endpoint keeps the pairing.
	h.mustRun("server", "add", "production", "--url", url, "--fingerprint", fp, "--force")
	if _, err := h.keyring.Get("vidyom", "device/production"); err != nil {
		t.Fatalf("same endpoint must keep the credential: %v", err)
	}
	assertContains(t, h.config(), "CLIENT-007")

	// A credential never follows a name to another place (I163).
	h.mustRun("server", "add", "production", "--url", "https://other.example.internal", "--force")
	if _, err := h.keyring.Get("vidyom", "device/production"); !errors.Is(err, security.ErrKeyringNotFound) {
		t.Fatalf("a new endpoint must drop the credential: %v", err)
	}
	if strings.Contains(h.config(), "CLIENT-007") {
		t.Fatal("the pairing must be forgotten")
	}
}

func TestServerRemoveDeletesCredential(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	pairingAnswers(h)
	h.mustRun("server", "pair", "production", "--code", "CODE")
	h.mustRun("server", "remove", "production")
	if _, err := h.keyring.Get("vidyom", "device/production"); !errors.Is(err, security.ErrKeyringNotFound) {
		t.Fatalf("keyring: %v", err)
	}
}

func TestConfigFileIsPrivateAndHoldsNoToken(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	pairingAnswers(h)
	h.mustRun("server", "pair", "production", "--code", "CODE")
	info, err := os.Stat(filepath.Join(h.dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	if strings.Contains(h.config(), fictionalToken) {
		t.Fatal("the token reached the config")
	}
}

func TestSelfSignedWithoutFingerprintIsRefusedWithAdvice(t *testing.T) {
	h := newHarness(t)
	url, _ := h.srv.ServeTLS()
	h.mustRun("server", "add", "production", "--url", url)
	if code := h.run("daemon", "health"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "not trusted", "--fingerprint")
}

func TestPinMismatchIsReportedAsPossibleInterception(t *testing.T) {
	h := newHarness(t)
	url, _ := h.srv.ServeTLS()
	h.mustRun("server", "add", "production", "--url", url, "--fingerprint", strings.Repeat("cd", 32))
	h.keyring.entries["vidyom/device/production"] = fictionalToken
	if code := h.run("job", "list", "--tenant", "acme"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "pinned fingerprint", "interception")
	if len(h.srv.Calls()) != 0 {
		t.Fatal("no request may cross a mismatched pin")
	}
}

func TestDeviceRotateOwnStoresAndHidesToken(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	pairingAnswers(h)
	h.mustRun("server", "pair", "production", "--code", "CODE")
	h.srv.Token = fictionalToken
	h.srv.JSON(api.RouteDevicesRotate, 200, api.DeviceCredentialResponse{Device: domain.Device{ID: "CLIENT-007"}, Token: "vdm_rotated_own"})

	h.mustRun("device", "rotate") // defaults to this device
	call, _ := h.srv.Last(api.RouteDevicesRotate)
	if !strings.Contains(call.Path, "CLIENT-007") {
		t.Fatalf("path = %s", call.Path)
	}
	if stored, _ := h.keyring.Get("vidyom", "device/production"); stored != "vdm_rotated_own" {
		t.Fatal("the new credential must replace the old one")
	}
	h.assertNoLeak("vdm_rotated_own")
	assertContains(t, h.stdout.String(), "OS keyring")
}

func TestDeviceRotateOtherDeviceDiscardsToken(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	out := h.mustRun("device", "rotate", "CLIENT-002")
	assertContains(t, out, "must pair again")
	h.assertNoLeak("vdm_rotated_token")
	h.mustRun("device", "rotate", "CLIENT-002", "--json")
	h.assertNoLeak("vdm_rotated_token")

	out = h.mustRun("device", "rotate", "CLIENT-002", "--reveal")
	assertContains(t, out, "vdm_rotated_token")
	assertContains(t, h.stderr.String(), "warning")
}

func TestDeviceRotateWithNoDeviceIsAUsageError(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	if code := h.run("device", "rotate"); code != 2 {
		t.Fatalf("exit %d", code)
	}
}

func TestDeviceRevokeOwnForgetsCredential(t *testing.T) {
	h := newHarness(t)
	addRemote(t, h)
	pairingAnswers(h)
	h.mustRun("server", "pair", "production", "--code", "CODE")
	h.srv.Token = fictionalToken
	h.srv.JSON(api.RouteDevicesRevoke, 200, domain.Device{ID: "CLIENT-007", Status: domain.DeviceRevoked})
	h.mustRun("device", "revoke", "CLIENT-007")
	if _, err := h.keyring.Get("vidyom", "device/production"); !errors.Is(err, security.ErrKeyringNotFound) {
		t.Fatalf("keyring: %v", err)
	}
	if strings.Contains(h.config(), "CLIENT-007") {
		t.Fatal("the config must forget the device")
	}

	other := newHarness(t)
	other.useTenant("acme")
	other.installSamples()
	other.keyring.entries["vidyom/device/test"] = fictionalToken
	other.mustRun("security", "revoke", "CLIENT-002")
	if _, err := other.keyring.Get("vidyom", "device/test"); err != nil {
		t.Fatal("revoking another device must keep this one's credential")
	}
}

func TestPairingCodeShowsFingerprint(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	out := h.mustRun("device", "pairing-code", "create", "--ttl", "15m")
	assertContains(t, out, "ABCDE-FGHJK", strings.Repeat("ab", 32), "--fingerprint")
	if code := h.run("device", "pairing-code", "create", "--ttl", "soon"); code != 2 {
		t.Fatalf("bad TTL: exit %d", code)
	}
}

func TestBudgetAmounts(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	assertContains(t, h.mustRun("budget", "show"), "500.00 USD", "12.00 USD", "2.4%")

	h.mustRun("budget", "set", "--monthly", "12.5", "--currency", "usd")
	call, _ := h.srv.Last(api.RouteBudgetPut)
	if body := call.JSONBody(t); body["monthly_minor"] != 1250.0 || body["currency"] != "USD" {
		t.Fatalf("body = %v", body)
	}
	h.mustRun("budget", "set", "--monthly", "500", "--currency", "JPY")
	call, _ = h.srv.Last(api.RouteBudgetPut)
	if body := call.JSONBody(t); body["monthly_minor"] != 500.0 {
		t.Fatalf("JPY has no minor unit: %v", body)
	}
	for _, flags := range [][]string{
		{"--monthly", "1.5", "--currency", "JPY"},
		{"--monthly", "1.555", "--currency", "USD"},
		{"--monthly", "-1", "--currency", "USD"},
		{"--monthly", "abc", "--currency", "USD"},
		{"--monthly", "500"},
	} {
		if code := h.run(append([]string{"budget", "set"}, flags...)...); code != 2 {
			t.Errorf("%v: exit %d", flags, code)
		}
	}
	if code := h.run("budget", "override"); code != 2 {
		t.Errorf("override needs a reason: exit %d", code)
	}
}

// The SSH transport goes through the system client, faked here by this test binary.
func TestSSHServerThroughFakeClient(t *testing.T) {
	h := newHarness(t)
	h.srv.JSON(api.RouteJobsList, 200, api.List[domain.Job]{})
	t.Setenv("VIDYOM_CLI_FAKE_SSH", "1")
	h.mustRun("server", "add", "ops-host", "--ssh", "operator@media.example.internal", "--remote-socket", h.socket)
	if code := h.run("job", "list", "--tenant", "acme"); code != 1 {
		t.Fatalf("pairing is mandatory over SSH (D12): exit %d", code)
	}
	assertContains(t, h.stderr.String(), "not paired")
	h.keyring.entries["vidyom/device/ops-host"] = fictionalToken
	h.mustRun("job", "list", "--tenant", "acme")
	call, _ := h.srv.Last(api.RouteJobsList)
	if call.Header.Get("Authorization") != "Bearer "+fictionalToken {
		t.Fatal("the credential must cross the tunnel")
	}
}

func TestEverySecurityErrorHasAReadableForm(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	for code, want := range map[errs.Code]string{
		errs.CodeUnauthenticated: "vidyom server pair test",
		errs.CodeSealed:          "sealed",
		errs.CodeBudgetExceeded:  "budget override",
	} {
		h.srv.Error(api.RouteJobsList, errs.New(code, "refused"))
		if got := h.run("job", "list"); got != 1 {
			t.Errorf("%s: exit %d", code, got)
		}
		assertContains(t, h.stderr.String(), want)
	}
}
