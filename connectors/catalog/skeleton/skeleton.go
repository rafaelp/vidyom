// Package skeleton is the shared body of external catalog connectors that exist
// only as declarations in this version: MediaCMS, VideoVortex and Rewind (I11). A
// skeleton compiles and registers, reports Available false with its reason, and
// answers every operation with errs.NotImplemented. Nothing here talks to a
// service. The canonical catalog stays the tenant database (D24).
package skeleton

import (
	"context"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Version is what a skeleton reports as its driver version.
const Version = "0.0.0-skeleton"

// Register declares driver as an unavailable catalog connector.
func Register(r connector.Registrar, driver, reason string) {
	info := connector.Info{Driver: driver, Version: Version, Available: false, Reason: reason}
	r.RegisterCatalog(info, func(context.Context, connector.CatalogParams) (connector.Catalog, error) {
		info := info
		info.Family = connector.FamilyCatalog
		return Catalog{info: info}, nil
	})
}

// Catalog refuses everything. It opens without error so that callers can record
// catalog_sync = failed with the reason (R11).
type Catalog struct{ info connector.Info }

var _ connector.Catalog = Catalog{}

func (c Catalog) gap() error { return errs.NotImplemented(c.info.Driver + " catalog connector") }

func (c Catalog) Info() connector.Info { return c.info }

func (c Catalog) Register(context.Context, connector.CatalogItem) (connector.CatalogRef, error) {
	return connector.CatalogRef{}, c.gap()
}

func (c Catalog) Update(context.Context, connector.CatalogItem) (connector.CatalogRef, error) {
	return connector.CatalogRef{}, c.gap()
}

func (c Catalog) Remove(context.Context, connector.CatalogRef) error { return c.gap() }
