// Package version carries the build version, set at link time by the Makefile:
//
//	-ldflags "-X github.com/rafaelp/vidyom/internal/contract/version.Version=v0.1.0"
//
// Releases use Semantic Versioning, 0.x until stable (D20).
package version

// Version is the build version.
var Version = "0.0.0-dev"

// APIVersion is the daemon API version (D54).
const APIVersion = "v1"
