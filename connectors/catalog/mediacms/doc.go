// Package mediacms is a skeleton (D24, I11): the driver registers, reports
// Available false and returns errs.NotImplemented from every operation. No MediaCMS
// instance is called. Its API, external proxies and double transcoding stay
// unvalidated (§17.2). With one configured, node mediacms.register records
// catalog_sync = failed, which blocks nothing (R11).
package mediacms

import (
	"github.com/rafaelp/vidyom/connectors/catalog/skeleton"
	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// Driver is the name used in catalogs.json.
const Driver = "mediacms"

// Register declares the skeleton.
func Register(r connector.Registrar) {
	skeleton.Register(r, Driver, "skeleton: the MediaCMS API, external proxies and double transcoding are unvalidated (D24, I11)")
}
