// Package contract is the root of Vidyom's shared contract packages.
//
// Every component compiles against the sub-packages here and nothing else of
// another component. They must stay free of cgo and of database packages so
// the CLI and the media worker remain static binaries.
//
// Rules for changing them are in docs/plans/00-master-plan.md, section 7:
// additive changes only during the parallel build stage.
package contract
