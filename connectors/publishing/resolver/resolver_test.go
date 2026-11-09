package resolver_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rafaelp/vidyom/connectors/publishing/mock"
	"github.com/rafaelp/vidyom/connectors/publishing/resolver"
	"github.com/rafaelp/vidyom/connectors/publishing/vimeo"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

type tenants map[domain.TenantID]config.Publishing

func (t tenants) TenantConfig(_ context.Context, tenant domain.TenantID) (config.Tenant, error) {
	p, ok := t[tenant]
	if !ok {
		return config.Tenant{}, errs.NotFound("tenant")
	}
	return config.Tenant{Publishing: p}, nil
}

func registry() *connector.Registry {
	reg := connector.NewRegistry()
	mock.Register(reg)
	vimeo.Register(reg)
	return reg
}

func mockDestination(t *testing.T, dir string) config.Destination {
	t.Helper()
	params, _ := json.Marshal(map[string]string{"stateDir": dir})
	return config.Destination{Driver: mock.Driver, Parameters: params}
}

func TestResolvesAndKeepsAConnectorUntilItsConfigurationChanges(t *testing.T) {
	ctx := context.Background()
	cfg := tenants{"acme": {Destinations: map[string]config.Destination{
		"mock-main": mockDestination(t, t.TempDir()),
	}}}
	r := resolver.New(registry(), cfg, nil)

	first, err := r.Publishing(ctx, "acme", "mock-main")
	if err != nil {
		t.Fatal(err)
	}
	if first.Info().Driver != mock.Driver {
		t.Fatalf("got %+v", first.Info())
	}
	again, err := r.Publishing(ctx, "acme", "mock-main")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatal("the connector should stay open")
	}

	cfg["acme"].Destinations["mock-main"] = mockDestination(t, t.TempDir())
	changed, err := r.Publishing(ctx, "acme", "mock-main")
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("a changed configuration must open a new connector")
	}
}

func TestTenantsAreKeptApart(t *testing.T) {
	ctx := context.Background()
	cfg := tenants{
		"acme":   {Destinations: map[string]config.Destination{"mock-main": mockDestination(t, t.TempDir())}},
		"globex": {Destinations: map[string]config.Destination{}},
	}
	r := resolver.New(registry(), cfg, nil)
	if _, err := r.Publishing(ctx, "globex", "mock-main"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := r.Publishing(ctx, "nobody", "mock-main"); !errs.IsCode(err, errs.CodeNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownDriverAndSkeleton(t *testing.T) {
	ctx := context.Background()
	cfg := tenants{"acme": {Destinations: map[string]config.Destination{
		"odd":        {Driver: "nothing-registered"},
		"vimeo-main": {Driver: vimeo.Driver},
	}}}
	r := resolver.New(registry(), cfg, nil)
	if _, err := r.Publishing(ctx, "acme", "odd"); !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("got %v", err)
	}
	skeleton, err := r.Publishing(ctx, "acme", "vimeo-main")
	if err != nil {
		t.Fatal(err)
	}
	if skeleton.Info().Available {
		t.Fatal("a skeleton must say it is unavailable")
	}
}
