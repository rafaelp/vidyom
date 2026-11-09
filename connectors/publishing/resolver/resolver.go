// Package resolver implements connector.PublishingResolver: it turns a tenant and a
// destination's logical name into an open connector, over the driver registry and
// the tenant's publishing.json. Workflows name destinations, never drivers or
// credentials (§7.1).
package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
)

// SecretsFor returns the secret resolver of one tenant.
type SecretsFor func(tenant domain.TenantID) connector.SecretResolver

// New returns a resolver. A connector stays open per tenant and destination, so its
// token cache and session state are reused, until that destination's configuration
// changes.
func New(registry *connector.Registry, configs config.TenantSource, secrets SecretsFor) *Resolver {
	return &Resolver{registry: registry, configs: configs, secrets: secrets, open: map[key]entry{}}
}

// Resolver is the implementation.
type Resolver struct {
	registry *connector.Registry
	configs  config.TenantSource
	secrets  SecretsFor

	metrics telemetry.Metrics

	mu   sync.Mutex
	open map[key]entry
}

var _ connector.PublishingResolver = (*Resolver)(nil)

// WithMetrics records connector calls (§10.3). Without it nothing is recorded.
func (r *Resolver) WithMetrics(m telemetry.Metrics) *Resolver {
	r.metrics = m
	return r
}

type key struct {
	tenant      domain.TenantID
	destination string
}

type entry struct {
	config []byte
	conn   connector.Publishing
}

// Publishing returns the tenant's destination by logical name. An unknown name
// answers not_found, like any other cross-tenant or missing reference.
func (r *Resolver) Publishing(ctx context.Context, tenant domain.TenantID, destination string) (connector.Publishing, error) {
	cfg, err := r.configs.TenantConfig(ctx, tenant)
	if err != nil {
		return nil, err
	}
	dest, ok := cfg.Publishing.Destinations[destination]
	if !ok {
		return nil, errs.Newf(errs.CodeNotFound, "destination %q is not configured", destination)
	}
	fingerprint, err := json.Marshal(dest)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot read the destination's configuration")
	}

	k := key{tenant, destination}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.open[k]; ok && bytes.Equal(e.config, fingerprint) {
		return e.conn, nil
	}
	factory, err := r.registry.Publishing(dest.Driver)
	if err != nil {
		return nil, errs.Wrap(errs.CodeFailedPrecondition, err, "destination "+destination+" names an unknown driver: "+dest.Driver)
	}
	var secrets connector.SecretResolver
	if r.secrets != nil {
		secrets = r.secrets(tenant)
	}
	conn, err := factory(ctx, connector.PublishingParams{Tenant: tenant, Destination: destination, Config: dest, Secrets: secrets})
	if err != nil {
		return nil, err
	}
	if r.metrics != nil {
		conn = meteredConnector{conn, r.metrics, string(tenant)}
	}
	r.open[k] = entry{fingerprint, conn}
	return conn, nil
}
