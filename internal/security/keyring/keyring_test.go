package keyring

import (
	"errors"
	"os"
	"testing"

	gokeyring "github.com/zalando/go-keyring"

	"github.com/rafaelp/vidyom/internal/contract/security"
)

func TestRoundTripWithMockedOS(t *testing.T) {
	gokeyring.MockInit()
	k := New()
	if _, err := k.Get("vidyom", "production"); !errors.Is(err, security.ErrKeyringNotFound) {
		t.Fatalf("missing entry: %v", err)
	}
	if err := k.Set("vidyom", "production", "vdm_fictional"); err != nil {
		t.Fatal(err)
	}
	got, err := k.Get("vidyom", "production")
	if err != nil || got != "vdm_fictional" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if err := k.Delete("vidyom", "production"); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete("vidyom", "production"); !errors.Is(err, security.ErrKeyringNotFound) {
		t.Fatalf("delete of a missing entry: %v", err)
	}
}

// §15.6: a client without a keyring fails explicitly, and there is no plaintext path.
func TestUnavailableKeyringFailsClearly(t *testing.T) {
	gokeyring.MockInitWithError(errors.New("no Secret Service on this host"))
	k := New()
	if _, err := k.Get("vidyom", "production"); !errors.Is(err, security.ErrKeyringUnavailable) {
		t.Fatalf("get: %v", err)
	}
	if err := k.Set("vidyom", "production", "vdm_fictional"); !errors.Is(err, security.ErrKeyringUnavailable) {
		t.Fatalf("set: %v", err)
	}
	if err := k.Delete("vidyom", "production"); !errors.Is(err, security.ErrKeyringUnavailable) {
		t.Fatalf("delete: %v", err)
	}
}

func TestTooLargeIsUnavailable(t *testing.T) {
	err := translate(gokeyring.ErrSetDataTooBig)
	if !errors.Is(err, security.ErrKeyringUnavailable) {
		t.Fatalf("%v", err)
	}
}

// The real OS path runs only when asked, so a plain `go test` never touches the
// maintainer's keychain.
func TestRealKeyring(t *testing.T) {
	if os.Getenv("VIDYOM_TEST_REAL_KEYRING") != "1" {
		t.Skip("set VIDYOM_TEST_REAL_KEYRING=1 to use the real OS keyring")
	}
	k := osKeyring{}
	const service, account = "vidyom-test", "keyring-test"
	t.Cleanup(func() { _ = k.Delete(service, account) })
	if err := k.Set(service, account, "fictional-value"); err != nil {
		t.Fatal(err)
	}
	got, err := k.Get(service, account)
	if err != nil || got != "fictional-value" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if err := k.Delete(service, account); err != nil {
		t.Fatal(err)
	}
}
