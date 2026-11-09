// Package videovortex is a skeleton (I11): the driver registers, reports Available
// false and returns errs.NotImplemented from every operation. No VideoVortex tool
// is called. Its real API, CLI and license stay unvalidated (§17.2).
package videovortex

import (
	"github.com/rafaelp/vidyom/connectors/catalog/skeleton"
	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// Driver is the name used in catalogs.json.
const Driver = "videovortex"

// Register declares the skeleton.
func Register(r connector.Registrar) {
	skeleton.Register(r, Driver, "skeleton: the VideoVortex API, CLI and license are unvalidated (I11)")
}
