// Package keyring is the client machine's OS keyring (§9.1): Keychain on macOS,
// Credential Manager on Windows, Secret Service (GNOME Keyring, KWallet) on Linux.
// There is no fallback. A client without a usable keyring gets
// security.ErrKeyringUnavailable, and no code path writes a credential to a file
// (§15.6).
//
// The package is cgo-free and imports no database code, because the CLI links it.
package keyring
