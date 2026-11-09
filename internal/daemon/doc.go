// Package daemon runs vidyomd: configuration, fail-closed listeners (§9.3, I19,
// I133–I137), the master key check (D13), lifecycle, service.Doctor and the logic
// of `vidyomd init`.
//
// Every component arrives through Deps, as contract types. cmd/vidyomd/wire.go
// assembles them; this package imports no other component's implementation.
package daemon
