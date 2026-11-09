// Package rewind is a skeleton (D26, I11): the driver registers, reports Available
// false and returns errs.NotImplemented from every operation. Rewind is an optional
// tool for editors planned for Phase 3.1, and its real interface is unvalidated.
package rewind

import (
	"github.com/rafaelp/vidyom/connectors/catalog/skeleton"
	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// Driver is the name used in catalogs.json.
const Driver = "rewind"

// Register declares the skeleton.
func Register(r connector.Registrar) {
	skeleton.Register(r, Driver, "skeleton: Rewind's real interface is unvalidated; planned for Phase 3.1 (D26, I11)")
}
