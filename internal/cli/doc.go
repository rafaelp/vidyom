// Package cli implements the vidyom command line: the commands of §11 over the API
// client in internal/apiclient. It is free of cgo and of database packages. Every
// command supports --json; viewing a job never changes it.
//
// Owner: component cli (docs/plans/cli.md). Decisions: docs/decisions/cli.md.
package cli
