package publishingnodes

import (
	"context"
	"errors"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/publicexport"
)

type syncOutput struct {
	Records     int    `json:"records"`
	SHA256      string `json:"sha256"`
	Destination string `json:"destination"`
	// Shipped is false when the tenant has no public library configured.
	Shipped      bool   `json:"shipped"`
	Withheld     int    `json:"withheld"`
	TextWithheld int    `json:"text_withheld"`
	Embeddings   string `json:"embeddings"`
}

// syncNode rebuilds this tenant's public.db and ships it (D18). It always rebuilds
// the whole file, so withdrawals disappear (R44). The assetId parameter names what
// triggered the sync and changes nothing about what is written.
type syncNode struct{ base }

func (n *syncNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if n.opts.Exporter == nil {
		return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition, "catalog.public.sync is not wired: this daemon has no exporter"))
	}
	cfg, err := d.TenantConfig(ctx)
	if err != nil {
		return node.Result{}, classifyStore(err)
	}

	// Open the destination first: a bad configuration should not write a file.
	var shipper connector.PublicCatalog
	if inst := cfg.Catalogs.PublicLibrary; inst != nil {
		shipper, err = n.openShipper(ctx, d, *inst)
		if err != nil {
			return node.Result{}, classify(err)
		}
	}

	drivers := map[string]string{}
	for name, dest := range cfg.Publishing.Destinations {
		drivers[name] = dest.Driver
	}
	req := publicexport.Request{
		Tenant:  d.Tenant(),
		Store:   d.Store(),
		Drivers: drivers,
		Links:   publicLinks(d),
	}
	if _, stored := d.Store().Search().(repo.EmbeddingLister); !stored {
		embedder, err := d.Embedder(ctx)
		switch {
		case err == nil:
			req.Embedder = embedder
		default:
			d.Logger().Warn("public.db will have no vectors: no embedder", "code", string(errs.CodeOf(err)))
		}
	}

	res, err := n.opts.Exporter.Export(ctx, req)
	if err != nil {
		return node.Result{}, classifyExport(err)
	}
	for _, w := range res.Withheld {
		d.Logger().Warn("public.db withheld a record", "asset", string(w.Asset), "publication", string(w.Publication), "reason", w.Reason)
	}

	out := syncOutput{
		Records:      res.Records,
		SHA256:       res.Artifact.SHA256,
		Withheld:     len(res.Withheld),
		TextWithheld: res.TextWithheld,
		Embeddings:   string(res.EmbeddingSource),
	}
	if shipper != nil {
		receipt, err := shipper.Ship(ctx, res.Artifact)
		if err != nil {
			return node.Result{}, classify(err)
		}
		out.Destination = receipt.Destination
		out.Shipped = true
		err = d.Emit(ctx, nil, event.Draft{Type: event.PublicLibrarySynced, Payload: map[string]any{
			"records": res.Records, "sha256": res.Artifact.SHA256, "destination": receipt.Destination,
		}})
		if err != nil {
			return node.Result{}, classifyStore(err)
		}
	}
	return node.JSONResult(out)
}

func (n *syncNode) openShipper(ctx context.Context, d node.DaemonEnv, inst config.CatalogInstance) (connector.PublicCatalog, error) {
	if n.opts.Connectors == nil {
		return nil, errs.New(errs.CodeFailedPrecondition, "catalog.public.sync is not wired: this daemon has no connector registry")
	}
	factory, err := n.opts.Connectors.PublicCatalog(inst.Driver)
	if err != nil {
		return nil, errs.Wrap(errs.CodeFailedPrecondition, err, "the public library's driver is not registered: "+inst.Driver)
	}
	return factory(ctx, connector.CatalogParams{
		Tenant: d.Tenant(), Name: "publicLibrary", Config: inst, Secrets: d.Secrets(),
	})
}

// publicLinks asks each record's connector for its public links. Failure to open a
// connector means no embed, never a failed export.
func publicLinks(d node.DaemonEnv) func(context.Context, domain.Publication, string) connector.PublicLinks {
	return func(ctx context.Context, p domain.Publication, _ string) connector.PublicLinks {
		conn, err := d.Publishing(ctx, p.Destination)
		if err != nil {
			return connector.PublicLinks{}
		}
		linker, ok := conn.(connector.PublicLinker)
		if !ok {
			return connector.PublicLinks{}
		}
		return linker.PublicLinks(connector.RemoteVideo{
			RemoteID: p.RemoteID, URL: p.URL, Status: connector.RemoteReady, Visibility: p.Visibility,
		})
	}
}

// classifyExport keeps a broken setup permanent; anything else is worth a retry,
// because the previous file is still in place.
func classifyExport(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return node.Transient(err)
	case errs.IsCode(err, errs.CodeFailedPrecondition), errs.IsCode(err, errs.CodeInvalidArgument):
		return node.Permanent(err)
	default:
		return node.Transient(err)
	}
}
