package publishingnodes

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

// Workflows the service starts (§3.3). Their input names are defined by the files in
// workflows/.
const (
	workflowPublish   = "publish-video"
	workflowUnpublish = "unpublish-video"
)

// ServiceDeps is what NewService needs. Everything is required except Now.
type ServiceDeps struct {
	Stores     repo.TenantStores
	Workflows  service.Workflows
	Resolver   connector.PublishingResolver
	Config     config.TenantSource
	Connectors service.Connectors
	Now        func() time.Time
}

// NewService returns service.Publications. Route permissions and the audit of each
// request are the API layer's; the service validates what depends on the resource.
func NewService(deps ServiceDeps) service.Publications {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &publications{deps}
}

type publications struct{ ServiceDeps }

var _ service.Publications = (*publications)(nil)

// Request starts publish-video. The approval gate lives in that workflow.
func (s *publications) Request(ctx context.Context, sc service.Scope, req api.CreatePublicationRequest) (api.RunWorkflowResponse, error) {
	if req.AssetID == "" {
		return api.RunWorkflowResponse{}, errs.New(errs.CodeInvalidArgument, "asset_id is required")
	}
	if len(req.Destinations) == 0 {
		return api.RunWorkflowResponse{}, errs.New(errs.CodeInvalidArgument, "destinations must name at least one destination")
	}
	switch req.Visibility {
	case "", domain.VisibilityPrivate, domain.VisibilityUnlisted, domain.VisibilityPublic:
	default:
		return api.RunWorkflowResponse{}, errs.Newf(errs.CodeInvalidArgument, "unknown visibility %q", req.Visibility)
	}
	store, err := s.Stores.ForTenant(ctx, sc.Tenant)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	asset, err := store.Assets().Get(ctx, id.Ref(req.AssetID))
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	cfg, err := s.Config.TenantConfig(ctx, sc.Tenant)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	available := s.availability()
	for _, name := range req.Destinations {
		dest, ok := cfg.Publishing.Destinations[name]
		if !ok {
			return api.RunWorkflowResponse{}, errs.Newf(errs.CodeInvalidArgument, "destination %q is not configured", name)
		}
		// Refuse a skeleton now rather than after a person has approved the gate.
		if info, known := available[dest.Driver]; known && !info.Available {
			return api.RunWorkflowResponse{}, errs.NotImplemented("destination " + name + " (" + info.Reason + ")")
		}
	}
	inputs := map[string]any{"assetId": string(asset.ID), "destinations": req.Destinations}
	if req.Visibility != "" {
		inputs["visibility"] = string(req.Visibility)
	}
	return s.run(ctx, sc, workflowPublish, inputs)
}

// List reads the tenant's records.
func (s *publications) List(ctx context.Context, sc service.Scope, q api.PublicationQuery) (api.List[domain.Publication], error) {
	store, err := s.Stores.ForTenant(ctx, sc.Tenant)
	if err != nil {
		return api.List[domain.Publication]{}, err
	}
	items, next, err := store.Publications().List(ctx, repo.PublicationFilter{
		AssetID: id.Ref(q.AssetID), Destination: q.Destination, Status: q.Status,
	}, repo.Page{Limit: q.Limit, Cursor: q.Cursor})
	if err != nil {
		return api.List[domain.Publication]{}, err
	}
	if items == nil {
		items = []domain.Publication{}
	}
	return api.List[domain.Publication]{Items: items, NextCursor: next}, nil
}

// Get reads one record. A foreign ID answers not_found: stores are per tenant.
func (s *publications) Get(ctx context.Context, sc service.Scope, ref id.Ref) (domain.Publication, error) {
	store, err := s.Stores.ForTenant(ctx, sc.Tenant)
	if err != nil {
		return domain.Publication{}, err
	}
	return store.Publications().Get(ctx, ref)
}

// Withdraw starts unpublish-video by asset (D17).
func (s *publications) Withdraw(ctx context.Context, sc service.Scope, req api.WithdrawRequest) (api.RunWorkflowResponse, error) {
	if req.AssetID == "" {
		return api.RunWorkflowResponse{}, errs.New(errs.CodeInvalidArgument, "asset_id is required")
	}
	if !req.All && len(req.Destinations) == 0 {
		return api.RunWorkflowResponse{}, errs.New(errs.CodeInvalidArgument, "name destinations or set all: withdrawal never defaults to everything")
	}
	store, err := s.Stores.ForTenant(ctx, sc.Tenant)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	asset, err := store.Assets().Get(ctx, id.Ref(req.AssetID))
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	inputs := map[string]any{"assetId": string(asset.ID)}
	if req.All {
		inputs["all"] = true
	} else {
		inputs["destinations"] = req.Destinations
	}
	return s.run(ctx, sc, workflowUnpublish, inputs)
}

// WithdrawOne starts unpublish-video for one record's destination.
func (s *publications) WithdrawOne(ctx context.Context, sc service.Scope, ref id.Ref) (api.RunWorkflowResponse, error) {
	record, err := s.Get(ctx, sc, ref)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	return s.run(ctx, sc, workflowUnpublish, map[string]any{
		"assetId": string(record.AssetID), "destinations": []string{record.Destination},
	})
}

// Sync re-reads a record's remote state now, with the same rules as publish.refresh.
func (s *publications) Sync(ctx context.Context, sc service.Scope, ref id.Ref) (domain.Publication, error) {
	store, err := s.Stores.ForTenant(ctx, sc.Tenant)
	if err != nil {
		return domain.Publication{}, err
	}
	record, err := store.Publications().Get(ctx, ref)
	if err != nil {
		return domain.Publication{}, err
	}
	if record.RemoteID == "" || record.Status == domain.PubWithdrawn {
		return record, nil
	}
	conn, err := s.Resolver.Publishing(ctx, sc.Tenant, record.Destination)
	if err != nil {
		return domain.Publication{}, err
	}
	actor := sc.Principal.Email
	if actor == "" {
		actor = string(sc.Principal.DeviceID)
	}
	updated, _, err := refreshRecord(ctx, store, conn, sc.Tenant, actor, s.emitter(sc), record, s.Now().UTC())
	return updated, err
}

// Destinations joins tenant configuration with what the registry knows about each
// driver, so a skeleton shows up with its reason and no secret ever does.
func (s *publications) Destinations(ctx context.Context, sc service.Scope) ([]api.DestinationInfo, error) {
	cfg, err := s.Config.TenantConfig(ctx, sc.Tenant)
	if err != nil {
		return nil, err
	}
	available := s.availability()
	out := make([]api.DestinationInfo, 0, len(cfg.Publishing.Destinations))
	for name, dest := range cfg.Publishing.Destinations {
		info := api.DestinationInfo{Name: name, Driver: dest.Driver, Visibility: dest.Visibility}
		if known, ok := available[dest.Driver]; ok {
			info.Available = known.Available
			info.Reason = known.Reason
		} else {
			info.Reason = "no driver named " + dest.Driver + " is registered"
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *publications) availability() map[string]connector.Info {
	out := map[string]connector.Info{}
	if s.Connectors == nil {
		return out
	}
	for _, info := range s.Connectors.List() {
		if info.Family == connector.FamilyPublishing {
			out[info.Driver] = info
		}
	}
	return out
}

func (s *publications) run(ctx context.Context, sc service.Scope, workflow string, inputs map[string]any) (api.RunWorkflowResponse, error) {
	raw, err := json.Marshal(inputs)
	if err != nil {
		return api.RunWorkflowResponse{}, err
	}
	return s.Workflows.Run(ctx, sc, workflow, api.RunWorkflowRequest{Inputs: raw, IdempotencyKey: sc.IdempotencyKey})
}

// emitter writes to the outbox directly: the service has no node environment.
func (s *publications) emitter(sc service.Scope) emitter {
	return func(ctx context.Context, tx repo.TenantRepos, d event.Draft) error {
		payload, err := json.Marshal(d.Payload)
		if err != nil {
			return err
		}
		_, err = tx.Outbox().Append(ctx, event.Envelope{
			EventID:       id.New(),
			Type:          d.Type,
			SchemaVersion: event.SchemaVersion,
			OccurredAt:    s.Now().UTC(),
			TenantID:      sc.Tenant,
			AssetID:       d.AssetID,
			TraceID:       sc.TraceID,
			Payload:       payload,
		})
		return err
	}
}
