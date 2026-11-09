// Package authentication resolves who is calling (D12, R19, I17): per-device
// credentials issued through one-time pairing codes, Unix socket peer identity mapped
// through localUsers, and worker tokens. Only hashes are stored. Scope rules and
// formats are in docs/decisions/core.md, I112.
package authentication
