// Package peertube is a skeleton (D25, I11): the driver registers so configuration
// and `vidyom connector list` can name it, reports Available false and returns
// errs.NotImplemented from every operation. No PeerTube instance is called.
// PeerTube comes after Vimeo on the roadmap, and its API stays unvalidated (§17.2).
package peertube

import (
	"github.com/rafaelp/vidyom/connectors/publishing/skeleton"
	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// Driver is the name used in publishing.json.
const Driver = "peertube"

// Register declares the skeleton.
func Register(r connector.Registrar) {
	skeleton.Register(r, Driver, "skeleton: the PeerTube API is not integrated in this version (D25, I11)")
}
