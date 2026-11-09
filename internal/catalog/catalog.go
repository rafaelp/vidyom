package catalog

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
	"github.com/rafaelp/vidyom/internal/contract/service"
	"github.com/rafaelp/vidyom/internal/security/audit"
)

// Options configures the service.
type Options struct {
	// Clock is used for timestamps. Nil means time.Now.
	Clock func() time.Time
}

// Service implements service.Assets.
type Service struct {
	stores repo.TenantStores
	clock  func() time.Time
}

var _ service.Assets = (*Service)(nil)

// New returns the service.
func New(stores repo.TenantStores, opts Options) *Service {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Service{stores: stores, clock: opts.Clock}
}

func (s *Service) store(ctx context.Context, scope service.Scope) (repo.TenantStore, error) {
	if scope.Tenant == "" {
		return nil, errs.New(errs.CodeTenantRequired, "this operation needs a tenant")
	}
	return s.stores.ForTenant(ctx, scope.Tenant)
}

// live reads an asset the API may show. A soft-deleted asset is not found here, while
// staying visible to workflows through the repository (D17, I117).
func live(ctx context.Context, r repo.AssetRepo, ref id.Ref) (domain.Asset, error) {
	a, err := r.Get(ctx, ref)
	if err != nil {
		return domain.Asset{}, err
	}
	if a.DeletedAt != nil {
		return domain.Asset{}, errs.NotFound("asset")
	}
	return a, nil
}

func (s *Service) List(ctx context.Context, scope service.Scope, q api.AssetQuery) (api.List[domain.Asset], error) {
	store, err := s.store(ctx, scope)
	if err != nil {
		return api.List[domain.Asset]{}, err
	}
	items, next, err := store.Assets().List(ctx, repo.AssetFilter{
		Kind:       q.Kind,
		Status:     q.Status,
		SourceID:   id.Ref(q.SourceID),
		Collection: q.Collection,
	}, repo.Page{Limit: q.Limit, Cursor: q.Cursor})
	if err != nil {
		return api.List[domain.Asset]{}, err
	}
	if items == nil {
		items = []domain.Asset{}
	}
	return api.List[domain.Asset]{Items: items, NextCursor: next}, nil
}

func (s *Service) Get(ctx context.Context, scope service.Scope, ref id.Ref) (domain.Asset, error) {
	store, err := s.store(ctx, scope)
	if err != nil {
		return domain.Asset{}, err
	}
	return live(ctx, store.Assets(), ref)
}

// entry builds an audit entry for the request.
func entry(scope service.Scope, action string, perm domain.Permission, resource string, details map[string]any) security.AuditEntry {
	return security.AuditEntry{
		TenantID:   scope.Tenant,
		Principal:  scope.Principal,
		Action:     action,
		Permission: perm,
		Resource:   resource,
		RequestID:  scope.RequestID,
		Details:    details,
	}
}

// mutation commits a change with its history entry and audit record.
func (s *Service) mutation(ctx context.Context, tx repo.TenantRepos, scope service.Scope, assetID id.Friendly, action string, perm domain.Permission, changes any, details map[string]any, at time.Time) error {
	var raw json.RawMessage
	if changes != nil {
		data, err := json.Marshal(changes)
		if err != nil {
			return err
		}
		raw = data
	}
	err := tx.Assets().AppendHistory(ctx, domain.HistoryEntry{AssetID: assetID, At: at, Actor: scope.Principal.Actor(), Action: action, Changes: raw})
	if err != nil {
		return err
	}
	return audit.AppendTx(ctx, tx.Audit(), entry(scope, action, perm, string(assetID), details), at)
}

// emitPublicUpdates writes publication.updated for each public publication of an asset,
// so public-library-on-updated refreshes public.db (§8.2, D30).
func emitPublicUpdates(ctx context.Context, tx repo.TenantRepos, scope service.Scope, a domain.Asset, reason string, at time.Time) error {
	for _, p := range a.Publications {
		if !p.IsPublic() {
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"publication_id": p.ID,
			"destination":    p.Destination,
			"visibility":     p.Visibility,
			"reason":         reason,
		})
		if err != nil {
			return err
		}
		_, err = tx.Outbox().Append(ctx, event.Envelope{
			EventID:       id.New(),
			Type:          event.PublicationUpdated,
			SchemaVersion: event.SchemaVersion,
			OccurredAt:    at,
			TenantID:      scope.Tenant,
			AssetID:       string(a.ID),
			TraceID:       scope.TraceID,
			Payload:       payload,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func hasPublicPublication(a domain.Asset) bool {
	return slices.ContainsFunc(a.Publications, domain.Publication.IsPublic)
}

// Update edits metadata; it never creates a media version (D35). On a public video the
// caller needs the publisher-level permission, and the edit counts as that approval
// (D7, I117). Moving a public video between collections changes no public metadata.
func (s *Service) Update(ctx context.Context, scope service.Scope, ref id.Ref, req api.UpdateAssetRequest) (domain.Asset, error) {
	store, err := s.store(ctx, scope)
	if err != nil {
		return domain.Asset{}, err
	}
	now := s.clock().UTC()
	var out domain.Asset
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		a, err := live(ctx, tx.Assets(), ref)
		if err != nil {
			return err
		}
		changes := map[string]any{}
		publicEdit := false
		if req.Title != nil && *req.Title != a.Title {
			a.Title, publicEdit = *req.Title, true
			changes["title"] = a.Title
		}
		if req.Description != nil && *req.Description != a.Description {
			a.Description, publicEdit = *req.Description, true
			changes["description"] = a.Description
		}
		if req.Tags != nil && !slices.Equal(*req.Tags, a.Tags) {
			a.Tags, publicEdit = *req.Tags, true
			changes["tags"] = a.Tags
		}
		if req.Category != nil && *req.Category != a.Category {
			a.Category, publicEdit = *req.Category, true
			changes["category"] = a.Category
		}
		if req.Collection != nil && *req.Collection != a.Collection {
			a.Collection = *req.Collection
			changes["collection"] = a.Collection
		}
		if len(changes) == 0 {
			out = a
			return nil
		}
		perm := domain.PermAssetWrite
		isPublic := hasPublicPublication(a)
		if publicEdit && isPublic {
			perm = domain.PermApprovalDecidePublication
			if !scope.Principal.Can(perm) {
				return errs.Newf(errs.CodeForbidden, "%s is public: changing its public metadata needs a publisher (D7)", a.ID).With("permission", string(perm))
			}
		}
		if err := tx.Assets().Update(ctx, a); err != nil {
			return err
		}
		fields := make([]string, 0, len(changes))
		for k := range changes {
			fields = append(fields, k)
		}
		slices.Sort(fields)
		// The audit names which fields changed. The values stay in the asset's history.
		if err := s.mutation(ctx, tx, scope, a.ID, "metadata.update", perm, changes, map[string]any{"fields": fields}, now); err != nil {
			return err
		}
		if publicEdit && isPublic {
			if err := emitPublicUpdates(ctx, tx, scope, a, "metadata_changed", now); err != nil {
				return err
			}
		}
		out, err = tx.Assets().Get(ctx, ref)
		return err
	})
	if err != nil {
		return domain.Asset{}, err
	}
	return out, nil
}

// Lineage returns where an asset comes from and what was derived from it (§2.3).
func (s *Service) Lineage(ctx context.Context, scope service.Scope, ref id.Ref) (api.LineageResponse, error) {
	store, err := s.store(ctx, scope)
	if err != nil {
		return api.LineageResponse{}, err
	}
	a, err := live(ctx, store.Assets(), ref)
	if err != nil {
		return api.LineageResponse{}, err
	}
	out := api.LineageResponse{AssetID: a.ID, Lineage: a.Lineage}
	page := repo.Page{Limit: 500}
	for {
		items, next, err := store.Assets().List(ctx, repo.AssetFilter{SourceID: id.Ref(a.ULID)}, page)
		if err != nil {
			return api.LineageResponse{}, err
		}
		for _, child := range items {
			out.Children = append(out.Children, child.ID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	slices.Sort(out.Children)
	return out, nil
}

func (s *Service) Versions(ctx context.Context, scope service.Scope, ref id.Ref) ([]domain.AssetVersion, error) {
	store, err := s.store(ctx, scope)
	if err != nil {
		return nil, err
	}
	if _, err := live(ctx, store.Assets(), ref); err != nil {
		return nil, err
	}
	versions, err := store.Versions().List(ctx, ref)
	if err != nil {
		return nil, err
	}
	if versions == nil {
		versions = []domain.AssetVersion{}
	}
	return versions, nil
}

func validProcessing(p domain.AIProcessing) error {
	if p != domain.AILocalOnly && p != domain.AIExternalAllowed {
		return errs.Newf(errs.CodeInvalidArgument, "ai_processing must be local_only or external_allowed, not %q", p)
	}
	return nil
}

// policyWriter finds the optional capability that writes the AI policy (I104).
func policyWriter(tx repo.TenantRepos) (repo.AssetAIPolicyWriter, error) {
	w, ok := tx.Assets().(repo.AssetAIPolicyWriter)
	if !ok {
		return nil, errs.New(errs.CodeInternal, "this store cannot record an AI policy")
	}
	return w, nil
}

// SetAIProcessing decides the external AI policy for one asset and writes the consent
// record: who and when (D8). An asset-level decision wins over later collection
// changes. Returning to local_only clears the record; the history and audit keep it.
func (s *Service) SetAIProcessing(ctx context.Context, scope service.Scope, ref id.Ref, req api.SetAIProcessingRequest) (domain.Asset, error) {
	if err := validProcessing(req.AIProcessing); err != nil {
		return domain.Asset{}, err
	}
	store, err := s.store(ctx, scope)
	if err != nil {
		return domain.Asset{}, err
	}
	now := s.clock().UTC()
	var out domain.Asset
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		a, err := live(ctx, tx.Assets(), ref)
		if err != nil {
			return err
		}
		w, err := policyWriter(tx)
		if err != nil {
			return err
		}
		var consent *domain.AIConsent
		if req.AIProcessing == domain.AIExternalAllowed {
			consent = &domain.AIConsent{AuthorizedBy: scope.Principal.Actor(), AuthorizedAt: now, Scope: "asset"}
		}
		if err := w.SetAIPolicy(ctx, ref, req.AIProcessing, consent, repo.AIPolicyAsset); err != nil {
			return err
		}
		change := map[string]any{"ai_processing": req.AIProcessing, "previous": a.AIProcessing}
		if err := s.mutation(ctx, tx, scope, a.ID, "ai_processing.set", domain.PermAssetAIConsent, change, change, now); err != nil {
			return err
		}
		out, err = tx.Assets().Get(ctx, ref)
		return err
	})
	if err != nil {
		return domain.Asset{}, err
	}
	return out, nil
}

func (s *Service) Consent(ctx context.Context, scope service.Scope, ref id.Ref) (api.ConsentResponse, error) {
	store, err := s.store(ctx, scope)
	if err != nil {
		return api.ConsentResponse{}, err
	}
	a, err := live(ctx, store.Assets(), ref)
	if err != nil {
		return api.ConsentResponse{}, err
	}
	return api.ConsentResponse{AIProcessing: a.AIProcessing, Consent: a.AIConsent}, nil
}

// versionOf picks a version, defaulting to the asset's current one.
func versionOf(a domain.Asset, version string) string {
	if version != "" {
		return version
	}
	return a.CurrentVersion
}

// Transcript returns the whole text to callers holding asset.sensitive, and to everyone
// else a copy with sensitive segments redacted, including what its sources mark (D30,
// I118).
func (s *Service) Transcript(ctx context.Context, scope service.Scope, ref id.Ref, version string) (domain.Transcript, error) {
	store, err := s.store(ctx, scope)
	if err != nil {
		return domain.Transcript{}, err
	}
	a, err := live(ctx, store.Assets(), ref)
	if err != nil {
		return domain.Transcript{}, err
	}
	version = versionOf(a, version)
	t, err := store.Transcripts().Get(ctx, ref, version)
	if err != nil {
		return domain.Transcript{}, err
	}
	if scope.Principal.Can(domain.PermAssetSensitive) {
		return t, nil
	}
	sensitive, err := effectiveMarks(ctx, store, a, version, 0)
	if err != nil {
		// What is hidden is unknown, so nothing is released to this reader.
		return domain.Transcript{}, err
	}
	return t.Redacted(sensitive), nil
}

func (s *Service) ListSensitive(ctx context.Context, scope service.Scope, ref id.Ref, version string) ([]domain.SensitiveSegment, error) {
	store, err := s.store(ctx, scope)
	if err != nil {
		return nil, err
	}
	a, err := live(ctx, store.Assets(), ref)
	if err != nil {
		return nil, err
	}
	segments, err := store.Transcripts().Sensitive(ctx, ref, versionOf(a, version))
	if err != nil {
		return nil, err
	}
	if segments == nil {
		segments = []domain.SensitiveSegment{}
	}
	return segments, nil
}

// segmentDetails describes an interval for history and audit, without its text.
func segmentDetails(version string, start, end float64) map[string]any {
	return map[string]any{"version": version, "start_seconds": start, "end_seconds": end}
}

// AddSensitive marks an interval. The export of a public asset, or of a public clip cut
// from this one, must drop it, so the mark also asks for a public library refresh (D30).
func (s *Service) AddSensitive(ctx context.Context, scope service.Scope, ref id.Ref, req api.SensitiveSegmentRequest) error {
	store, err := s.store(ctx, scope)
	if err != nil {
		return err
	}
	now := s.clock().UTC()
	return store.InTx(ctx, func(tx repo.TenantRepos) error {
		a, err := live(ctx, tx.Assets(), ref)
		if err != nil {
			return err
		}
		version := versionOf(a, req.Version)
		err = tx.Transcripts().MarkSensitive(ctx, domain.SensitiveSegment{
			AssetID:      a.ID,
			Version:      version,
			StartSeconds: req.StartSeconds,
			EndSeconds:   req.EndSeconds,
			Reason:       req.Reason,
			MarkedBy:     scope.Principal.Actor(),
			MarkedAt:     now,
		})
		if err != nil {
			return err
		}
		details := segmentDetails(version, req.StartSeconds, req.EndSeconds)
		if err := s.mutation(ctx, tx, scope, a.ID, "sensitive.mark", domain.PermAssetSensitive, details, details, now); err != nil {
			return err
		}
		return refreshPublicExports(ctx, tx, scope, a, "sensitive_segments_changed", now)
	})
}

// RemoveSensitive unmarks the exact interval (I118).
func (s *Service) RemoveSensitive(ctx context.Context, scope service.Scope, ref id.Ref, req api.SensitiveSegmentRequest) error {
	store, err := s.store(ctx, scope)
	if err != nil {
		return err
	}
	now := s.clock().UTC()
	return store.InTx(ctx, func(tx repo.TenantRepos) error {
		a, err := live(ctx, tx.Assets(), ref)
		if err != nil {
			return err
		}
		version := versionOf(a, req.Version)
		if err := tx.Transcripts().UnmarkSensitive(ctx, ref, version, req.StartSeconds, req.EndSeconds); err != nil {
			return err
		}
		details := segmentDetails(version, req.StartSeconds, req.EndSeconds)
		if err := s.mutation(ctx, tx, scope, a.ID, "sensitive.unmark", domain.PermAssetSensitive, details, details, now); err != nil {
			return err
		}
		return refreshPublicExports(ctx, tx, scope, a, "sensitive_segments_changed", now)
	})
}

func (s *Service) Collections(ctx context.Context, scope service.Scope) ([]domain.Collection, error) {
	store, err := s.store(ctx, scope)
	if err != nil {
		return nil, err
	}
	list, err := store.Collections().List(ctx)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []domain.Collection{}
	}
	return list, nil
}

// SetCollectionAIProcessing decides the policy for a collection and rewrites its members,
// except those with a decision of their own (D8, I104). A new name creates the
// collection, so an admin can decide before the first asset arrives.
func (s *Service) SetCollectionAIProcessing(ctx context.Context, scope service.Scope, name string, req api.SetAIProcessingRequest) (domain.Collection, error) {
	if err := validProcessing(req.AIProcessing); err != nil {
		return domain.Collection{}, err
	}
	if name == "" {
		return domain.Collection{}, errs.New(errs.CodeInvalidArgument, "a collection needs a name")
	}
	store, err := s.store(ctx, scope)
	if err != nil {
		return domain.Collection{}, err
	}
	now := s.clock().UTC()
	var out domain.Collection
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		var consent *domain.AIConsent
		if req.AIProcessing == domain.AIExternalAllowed {
			consent = &domain.AIConsent{AuthorizedBy: scope.Principal.Actor(), AuthorizedAt: now, Scope: "collection"}
		}
		if err := tx.Collections().Upsert(ctx, domain.Collection{Name: name, AIProcessing: req.AIProcessing, AIConsent: consent}); err != nil {
			return err
		}
		w, err := policyWriter(tx)
		if err != nil {
			return err
		}
		changed, err := w.ApplyCollectionPolicy(ctx, name, req.AIProcessing, consent)
		if err != nil {
			return err
		}
		details := map[string]any{"ai_processing": req.AIProcessing, "assets_changed": changed}
		if err := audit.AppendTx(ctx, tx.Audit(), entry(scope, "ai_processing.set_collection", domain.PermAssetAIConsent, "collection:"+name, details), now); err != nil {
			return err
		}
		out, err = tx.Collections().Get(ctx, name)
		return err
	})
	if err != nil {
		return domain.Collection{}, err
	}
	return out, nil
}
