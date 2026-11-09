// Package instagramreels is a skeleton (D25, I11): the driver registers so
// configuration and `vidyom connector list` can name it, reports Available false
// and returns errs.NotImplemented from every operation. No Instagram API is
// called, and vertical reframing from the 4K master is not implemented. Its API
// stays unvalidated (§17.2).
package instagramreels

import (
	"github.com/rafaelp/vidyom/connectors/publishing/skeleton"
	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// Driver is the name used in publishing.json.
const Driver = "instagram-reels"

// Register declares the skeleton.
func Register(r connector.Registrar) {
	skeleton.Register(r, Driver, "skeleton: the Instagram Reels API and vertical reframing are not integrated in this version (D25, I11)")
}
