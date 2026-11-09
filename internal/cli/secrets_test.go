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

const fictionalSecret = "fictional-secret-value-7f3a"

func TestSecretAddReadsStdinAndNeverPrintsTheValue(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.stdin = fictionalSecret + "\n"
	h.mustRun("secret", "add", "gcs-main", "--location", "server")

	call, _ := h.srv.Last(api.RouteSecretsPut)
	var req api.PutSecretRequest
	if err := json.Unmarshal(call.Body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Value != fictionalSecret || req.Location != domain.SecretServer || req.Name != "gcs-main" {
		t.Fatalf("name %s, location %s; a single trailing newline must be trimmed", req.Name, req.Location)
	}
	h.assertNoLeak(fictionalSecret)
}

func TestSecretAddOnATerminalUsesTheNoEchoPrompt(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.terminal = true
	h.stdin = "this line must not be read"
	calls := 0
	h.password = func() ([]byte, error) {
		calls++
		return []byte(fictionalSecret), nil
	}
	h.mustRun("secret", "add", "gcs-main", "--location", "server")
	if calls != 1 {
		t.Fatalf("no-echo reads = %d", calls)
	}
	assertContains(t, h.stderr.String(), "not echoed")
	call, _ := h.srv.Last(api.RouteSecretsPut)
	if call.JSONBody(t)["value"] != fictionalSecret {
		t.Fatal("the value from the prompt must reach the vault")
	}
	h.assertNoLeak(fictionalSecret)
}

func TestSecretValueIsNeverAnArgument(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()

	cases := [][]string{
		{"secret", "add", "gcs-main", fictionalSecret, "--location", "server"},
		{"secret", "add", "gcs-main", "--location", "server", "--value", fictionalSecret},
		{"secret", "add", "gcs-main", "--location", "server", "--value=" + fictionalSecret},
		{"secret", "add", "gcs-main", "--location", "server", "-v=" + fictionalSecret},
		{"secret", "rotate", "gcs-main", fictionalSecret},
		{"secret", "rotate", "gcs-main", "--value=" + fictionalSecret},
		{"secret", "add", "gcs-main", "--location", "delegated", "--ttl", fictionalSecret},
	}
	for _, command := range cases {
		h.stdin = "from-stdin"
		if code := h.run(command...); code != 2 {
			t.Errorf("%v: exit %d", command, code)
		}
		h.assertNoLeak(fictionalSecret)
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("nothing may be sent")
	}
}

func TestSecretAddRules(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()

	// A location is a choice that matters for autonomous jobs (R37): no default.
	h.stdin = "x"
	if code := h.run("secret", "add", "gcs-main"); code != 2 {
		t.Errorf("no location: exit %d", code)
	}
	h.stdin = "x"
	if code := h.run("secret", "add", "gcs-main", "--location", "elsewhere"); code != 2 {
		t.Errorf("bad location: exit %d", code)
	}
	h.stdin = "x"
	if code := h.run("secret", "add", "vimeo-oauth", "--location", "delegated"); code != 2 {
		t.Errorf("delegated without a TTL: exit %d", code)
	}
	h.stdin = "x"
	if code := h.run("secret", "add", "gcs-main", "--location", "client", "--ttl", "1h"); code != 2 {
		t.Errorf("client with a TTL: exit %d", code)
	}
	h.stdin = ""
	if code := h.run("secret", "add", "gcs-main", "--location", "server"); code != 1 {
		t.Errorf("empty value: exit %d", code)
	}
	assertContains(t, h.stderr.String(), "empty")
	h.stdin = strings.Repeat("x", 1<<20+1)
	if code := h.run("secret", "add", "gcs-main", "--location", "server"); code != 1 {
		t.Errorf("oversized value: exit %d", code)
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatal("nothing may be sent")
	}

	h.stdin = "x"
	h.mustRun("secret", "add", "vimeo-oauth", "--location", "delegated", "--ttl", "2h")
	call, _ := h.srv.Last(api.RouteSecretsPut)
	if body := call.JSONBody(t); body["location"] != "delegated" || body["ttl_seconds"] != 7200.0 {
		t.Fatalf("keys = %v", body["ttl_seconds"])
	}
}

func TestSecretKeepsInnerNewlines(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.stdin = "line one\nline two\r\n"
	h.mustRun("secret", "add", "pem", "--location", "server")
	call, _ := h.srv.Last(api.RouteSecretsPut)
	if call.JSONBody(t)["value"] != "line one\nline two" {
		t.Fatal("only the final line break may be trimmed")
	}
}

func TestSecretRotateReadsStdin(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.stdin = fictionalSecret
	h.mustRun("secret", "rotate", "gcs-main")
	call, _ := h.srv.Last(api.RouteSecretsRotate)
	if call.JSONBody(t)["value"] != fictionalSecret {
		t.Fatal("rotate must send the new value")
	}
	h.assertNoLeak(fictionalSecret)
}

func TestSecretAPIErrorDoesNotEchoTheValue(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.srv.Error(api.RouteSecretsPut, errs.New(errs.CodeForbidden, "permission secret.write is required"))
	h.stdin = fictionalSecret
	if code := h.run("secret", "add", "gcs-main", "--location", "server"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	h.assertNoLeak(fictionalSecret)
}

func TestClientSecretStaysOnThisMachine(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.stdin = fictionalSecret
	h.mustRun("secret", "add", "youtube-oauth", "--location", "client")

	if len(h.srv.Calls(api.RouteSecretsPut)) != 0 {
		t.Fatal("a client secret must never reach the API")
	}
	stored, err := h.keyring.Get("vidyom", "secret/test/acme/youtube-oauth")
	if err != nil || stored != fictionalSecret {
		t.Fatalf("the keyring must hold it: %v", err)
	}
	h.assertNoLeak(fictionalSecret)
	assertContains(t, h.config(), "youtube-oauth")

	// list merges it, test and rotate act locally.
	out := h.mustRun("secret", "list")
	assertContains(t, out, "youtube-oauth", "client", "gcs-main", "server")

	h.srv.Reset()
	assertContains(t, h.mustRun("secret", "test", "youtube-oauth"), "yes")
	h.stdin = "fictional-rotated"
	h.mustRun("secret", "rotate", "youtube-oauth")
	if stored, _ := h.keyring.Get("vidyom", "secret/test/acme/youtube-oauth"); stored != "fictional-rotated" {
		t.Fatal("rotate must replace the local value")
	}
	if len(h.srv.Mutations()) != 0 {
		t.Fatalf("local operations must not call the API: %v", h.srv.Mutations())
	}

	h.mustRun("secret", "delete", "youtube-oauth")
	if _, err := h.keyring.Get("vidyom", "secret/test/acme/youtube-oauth"); !errors.Is(err, security.ErrKeyringNotFound) {
		t.Fatalf("delete must remove it: %v", err)
	}
	if strings.Contains(h.config(), "youtube-oauth") {
		t.Fatal("delete must remove the name")
	}
	if len(h.srv.Calls(api.RouteSecretsDelete)) != 0 {
		t.Fatal("deleting a local secret must not call the API")
	}

	// With the local one gone, the same name means the server's.
	h.mustRun("secret", "delete", "youtube-oauth")
	if len(h.srv.Calls(api.RouteSecretsDelete)) != 1 {
		t.Fatal("now it must call the API")
	}
}

func TestClientSecretFailsClearlyWithoutKeyring(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.keyring.setErr = security.ErrKeyringUnavailable
	h.stdin = fictionalSecret
	if code := h.run("secret", "add", "youtube-oauth", "--location", "client"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	assertContains(t, h.stderr.String(), "no usable OS keyring")
	h.assertNoLeak(fictionalSecret)
	if strings.Contains(h.config(), "youtube-oauth") {
		t.Fatal("no name may be recorded for a value that was not stored")
	}
}

func TestSecretsNeverInTheCheckpointOrConfigFiles(t *testing.T) {
	h := newHarness(t)
	h.useTenant("acme")
	h.installSamples()
	h.stdin = fictionalSecret
	h.mustRun("secret", "add", "x", "--location", "client")
	err := filepath.Walk(h.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(raw), fictionalSecret) {
			t.Errorf("the secret reached %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
