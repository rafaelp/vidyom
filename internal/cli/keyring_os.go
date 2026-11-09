//go:build !vidyom_test_keyring

package cli

import (
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/security/keyring"
)

// defaultKeyring is the OS keyring. There is no fallback (§9.1): when it cannot be
// used, its operations return security.ErrKeyringUnavailable.
func defaultKeyring(func(string) string, func(string)) security.Keyring { return keyring.New() }
