// Package vimeo is a skeleton (D25, I11): the driver registers so configuration
// and `vidyom connector list` can name it, reports Available false and returns
// errs.NotImplemented from every operation. No Vimeo API is called. Vimeo comes
// after YouTube on the roadmap, and its API stays unvalidated (§17.2).
package vimeo

import (
	"github.com/rafaelp/vidyom/connectors/publishing/skeleton"
	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// Driver is the name used in publishing.json.
const Driver = "vimeo"

// Register declares the skeleton.
func Register(r connector.Registrar) {
	skeleton.Register(r, Driver, "skeleton: the Vimeo API is not integrated in this version (D25, I11)")
}
