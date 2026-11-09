package publishingnodes

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
)

// mediacmsDriver is the driver this node looks for. The connector is a skeleton in
// this version (I11), so a configured instance records catalog_sync = failed.
const mediacmsDriver = "mediacms"

type mediacmsParams struct {
	AssetID string `json:"assetId"`
	// Name picks an instance in catalogs.external; empty picks the first mediacms one.
	Name string `json:"name"`
}

type mediacmsOutput struct {
	CatalogSync string `json:"catalog_sync"`
	ExternalRef string `json:"external_ref,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
}

// mediacmsNode registers an asset with the optional MediaCMS connector (D24). It
// always completes once the asset is found: a failed catalog must block nothing, so
// that guarantee does not depend on a workflow setting continueOnFailure (R11, I235).
type mediacmsNode struct{ base }

func (n *mediacmsNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	// Additional properties are allowed here, so a strict decoder would be wrong.
	var p mediacmsParams
	if err := json.Unmarshal(in.Parameters, &p); err != nil {
		return node.Result{}, node.Permanent(errs.Wrap(errs.CodeInvalidArgument, err, "invalid node parameters"))
	}
	if p.AssetID == "" {
		return node.Result{}, node.Permanent(errs.New(errs.CodeInvalidArgument, "assetId is required"))
	}
	asset, err := d.Store().Assets().Get(ctx, id.Ref(p.AssetID))
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	cfg, err := d.TenantConfig(ctx)
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	name, inst, ok := pickMediaCMS(cfg.Catalogs, p.Name)
	if !ok {
		return node.JSONResult(mediacmsOutput{CatalogSync: string(domain.CatalogSyncNone)})
	}

	ref, regErr := n.register(ctx, d, name, inst, asset)
	if regErr != nil {
		d.Logger().Warn("external catalog registration failed; this blocks nothing (R11)",
			"asset", string(asset.ID), "catalog", name, "code", string(errs.CodeOf(regErr)))
	}
	status := domain.CatalogSyncSynced
	if regErr != nil {
		status = domain.CatalogSyncFailed
	}
	asset.CatalogSync = status
	asset.UpdatedAt = n.opts.Now().UTC()
	if err := d.Store().Assets().Update(context.WithoutCancel(ctx), asset); err != nil {
		return node.Result{}, classifyStore(err)
	}
	out := mediacmsOutput{CatalogSync: string(status), ExternalRef: ref.ExternalRef}
	if regErr != nil {
		out.ErrorCode = string(errs.CodeOf(regErr))
	}
	return node.JSONResult(out)
}

func pickMediaCMS(c config.Catalogs, wanted string) (string, config.CatalogInstance, bool) {
	if wanted != "" {
		inst, ok := c.External[wanted]
		return wanted, inst, ok
	}
	names := make([]string, 0, len(c.External))
	for name, inst := range c.External {
		if inst.Driver == mediacmsDriver {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", config.CatalogInstance{}, false
	}
	sort.Strings(names)
	return names[0], c.External[names[0]], true
}

// register tries a few times on transient errors. It never lets an error stop the
// job, so these retries are the node's own.
func (n *mediacmsNode) register(ctx context.Context, d node.DaemonEnv, name string, inst config.CatalogInstance, asset domain.Asset) (connector.CatalogRef, error) {
	if n.opts.Connectors == nil {
		return connector.CatalogRef{}, errs.New(errs.CodeFailedPrecondition, "mediacms.register is not wired: this daemon has no connector registry")
	}
	factory, err := n.opts.Connectors.Catalog(inst.Driver)
	if err != nil {
		return connector.CatalogRef{}, err
	}
	conn, err := factory(ctx, connector.CatalogParams{Tenant: d.Tenant(), Name: name, Config: inst, Secrets: d.Secrets()})
	if err != nil {
		return connector.CatalogRef{}, err
	}
	var ref connector.CatalogRef
	wait := catalogRegisterBackup
	for attempt := 1; ; attempt++ {
		ref, err = conn.Register(ctx, connector.CatalogItem{Asset: asset})
		if err == nil || !retryable(err) || attempt >= catalogRegisterTries {
			return ref, err
		}
		select {
		case <-ctx.Done():
			return ref, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}
