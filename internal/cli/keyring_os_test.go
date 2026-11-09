//go:build !vidyom_test_keyring

package cli

import (
	"fmt"
	"strings"
	"testing"
)

// The default build must not contain the test keyring, whatever the environment says.
func TestDefaultBuildIgnoresTheTestKeyringVariable(t *testing.T) {
	kr := defaultKeyring(func(string) string { return "/tmp/plaintext-keyring.json" }, func(string) {
		t.Error("the default build must not warn about a test keyring")
	})
	if name := fmt.Sprintf("%T", kr); strings.Contains(name, "file") || strings.Contains(name, "unavailable") {
		t.Fatalf("keyring type = %s", name)
	}
}
