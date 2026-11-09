package connector

import (
	"context"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
)

// CatalogItem is what an optional external catalog receives. The canonical
// catalog stays the tenant database (D24); a connector failure is recorded as
// catalog_sync = failed and blocks nothing (R11).
type CatalogItem struct {
	Asset       domain.Asset `json:"asset"`
	ProxyLink   string       `json:"proxy_link,omitempty"`
	ExternalRef string       `json:"external_ref,omitempty"`
}

// CatalogRef identifies an item at the external catalog.
type CatalogRef struct {
	ExternalRef string `json:"external_ref"`
	URL         string `json:"url,omitempty"`
}

// Catalog is an external catalog connector: MediaCMS, VideoVortex or Rewind in
// the spec, all skeletons in this version (I11).
type Catalog interface {
	Info() Info
	Register(ctx context.Context, item CatalogItem) (CatalogRef, error)
	Update(ctx context.Context, item CatalogItem) (CatalogRef, error)
	Remove(ctx context.Context, ref CatalogRef) error
}

// CatalogParams is what a factory receives.
type CatalogParams struct {
	Tenant  domain.TenantID
	Name    string
	Config  config.CatalogInstance
	Secrets SecretResolver
}

// CatalogFactory opens a catalog connector.
type CatalogFactory func(ctx context.Context, p CatalogParams) (Catalog, error)

// PublicArtifact is an exported public.db ready to ship (D18).
type PublicArtifact struct {
	Tenant        domain.TenantID `json:"tenant"`
	Path          string          `json:"path"`
	SHA256        string          `json:"sha256"`
	SizeBytes     int64           `json:"size_bytes"`
	SchemaVersion int             `json:"schema_version"`
}

// ShipReceipt is the result of shipping.
type ShipReceipt struct {
	Destination string `json:"destination"`
	SHA256      string `json:"sha256"`
}

// PublicCatalog ships the filtered public dataset to where the site reads it
// (R44, D18). The library never contacts the daemon.
type PublicCatalog interface {
	Info() Info
	Ship(ctx context.Context, artifact PublicArtifact) (ShipReceipt, error)
	// Clear removes the tenant's dataset at the destination, idempotently.
	Clear(ctx context.Context, tenant domain.TenantID) error
}

// PublicCatalogFactory opens a public catalog connector.
type PublicCatalogFactory func(ctx context.Context, p CatalogParams) (PublicCatalog, error)
