package skeleton_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rafaelp/vidyom/connectors/catalog/mediacms"
	"github.com/rafaelp/vidyom/connectors/catalog/rewind"
	"github.com/rafaelp/vidyom/connectors/catalog/videovortex"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func TestSkeletonsDeclareThemselvesUnavailable(t *testing.T) {
	cases := []struct {
		driver   string
		register func(connector.Registrar)
	}{
		{mediacms.Driver, mediacms.Register},
		{videovortex.Driver, videovortex.Register},
		{rewind.Driver, rewind.Register},
	}
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			reg := connector.NewRegistry()
			tc.register(reg)

			listed := reg.List()
			if len(listed) != 1 || listed[0].Family != connector.FamilyCatalog || listed[0].Available {
				t.Fatalf("registry lists %+v", listed)
			}
			if !strings.Contains(listed[0].Reason, "skeleton") {
				t.Fatalf("reason must state the gap, got %q", listed[0].Reason)
			}

			factory, err := reg.Catalog(tc.driver)
			if err != nil {
				t.Fatal(err)
			}
			c, err := factory(ctx, connector.CatalogParams{Tenant: "acme", Name: "x"})
			if err != nil {
				t.Fatalf("a skeleton must open so callers can record the failure: %v", err)
			}
			if c.Info().Available || c.Info().Driver != tc.driver {
				t.Fatalf("Info says %+v", c.Info())
			}
			if _, err := c.Register(ctx, connector.CatalogItem{}); !errs.IsCode(err, errs.CodeNotImplemented) {
				t.Errorf("Register: got %v", err)
			}
			if _, err := c.Update(ctx, connector.CatalogItem{}); !errs.IsCode(err, errs.CodeNotImplemented) {
				t.Errorf("Update: got %v", err)
			}
			if err := c.Remove(ctx, connector.CatalogRef{}); !errs.IsCode(err, errs.CodeNotImplemented) {
				t.Errorf("Remove: got %v", err)
			}
		})
	}
}
