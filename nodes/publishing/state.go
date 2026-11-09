package publishingnodes

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// transitions is the table of I220. Repeating a state is always allowed.
var transitions = map[domain.PublicationStatus][]domain.PublicationStatus{
	domain.PubPendingApproval: {
		domain.PubUploading, domain.PubProcessing, domain.PubPublished, domain.PubScheduled,
		domain.PubFailed, domain.PubRejectedByDestination, domain.PubWithdrawn,
	},
	domain.PubUploading: {
		domain.PubProcessing, domain.PubScheduled, domain.PubFailed,
		domain.PubRejectedByDestination, domain.PubWithdrawn,
	},
	domain.PubProcessing: {
		domain.PubPublished, domain.PubScheduled, domain.PubPendingApproval, domain.PubFailed,
		domain.PubRejectedByDestination, domain.PubWithdrawn,
	},
	domain.PubScheduled:             {domain.PubPublished, domain.PubFailed, domain.PubRejectedByDestination, domain.PubWithdrawn},
	domain.PubPublished:             {domain.PubRejectedByDestination, domain.PubWithdrawn},
	domain.PubFailed:                {domain.PubUploading, domain.PubProcessing, domain.PubRejectedByDestination, domain.PubWithdrawn},
	domain.PubRejectedByDestination: {domain.PubWithdrawn},
}

// canMove reports whether the state machine of I220 allows a move.
func canMove(from, to domain.PublicationStatus) bool {
	return from == to || slices.Contains(transitions[from], to)
}

// move changes a record's state, or explains why it cannot.
func move(p *domain.Publication, to domain.PublicationStatus, now time.Time) error {
	if !canMove(p.Status, to) {
		return errs.Newf(errs.CodeFailedPrecondition, "publication %s cannot move from %s to %s", p.ID, p.Status, to)
	}
	p.Status = to
	p.UpdatedAt = now
	return nil
}

// active reports whether a record may still be at, or reach, the destination.
func active(s domain.PublicationStatus) bool {
	switch s {
	case domain.PubPendingApproval, domain.PubUploading, domain.PubProcessing, domain.PubScheduled, domain.PubPublished:
		return true
	default:
		return false
	}
}

// statusFromRemote maps what a destination says to the record's state (D52).
// Ready stays processing: only publish.finalize and refresh write published (R43).
func statusFromRemote(r connector.RemoteVideo) domain.PublicationStatus {
	switch r.Status {
	case connector.RemoteRejected:
		return domain.PubRejectedByDestination
	case connector.RemoteFailed, connector.RemoteRemoved:
		return domain.PubFailed
	case connector.RemoteScheduled:
		return domain.PubScheduled
	default:
		return domain.PubProcessing
	}
}

// retryable reports whether an error is worth another attempt.
func retryable(err error) bool {
	switch errs.CodeOf(err) {
	case errs.CodeUnavailable, errs.CodeTimeout, errs.CodeRateLimited:
		return true
	default:
		return false
	}
}

// classify marks a destination's error for the engine (I223). A cancelled context
// is transient: the effect did not happen, and a new attempt reconciles first.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case connector.IsOutcomeUnknown(err):
		return node.Unknown(err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), retryable(err):
		return node.Transient(err)
	default:
		return node.Permanent(err)
	}
}

// classifyStore marks a repository error: the database having a bad moment is
// transient, and anything else coded is a request that cannot be satisfied.
func classifyStore(err error) error {
	switch errs.CodeOf(err) {
	case errs.CodeInternal, errs.CodeUnavailable, errs.CodeTimeout, errs.CodeRateLimited:
		return node.Transient(err)
	default:
		return node.Permanent(err)
	}
}

// allowPrivateFirst reads the D37 option: the destination's value wins over defaults.
func allowPrivateFirst(cfg config.Publishing, destination string) bool {
	if d, ok := cfg.Destinations[destination]; ok && d.AllowPrivateUploadBeforeApproval != nil {
		return *d.AllowPrivateUploadBeforeApproval
	}
	return cfg.Defaults.AllowPrivateUploadBeforeApproval
}

// checkApproval proves an approval authorizes this asset and version (I221).
func checkApproval(ctx context.Context, tx repo.TenantRepos, approvalID string, asset id.Friendly, version string, kinds ...domain.ApprovalKind) (domain.Approval, error) {
	a, err := tx.Approvals().Get(ctx, id.Ref(approvalID))
	if errs.IsCode(err, errs.CodeNotFound) {
		return domain.Approval{}, errs.Newf(errs.CodeFailedPrecondition, "approval %s does not exist", approvalID)
	}
	if err != nil {
		return domain.Approval{}, err
	}
	if a.Status != domain.ApprovalApproved {
		return domain.Approval{}, errs.Newf(errs.CodeFailedPrecondition, "approval %s is %s, not approved", a.ID, a.Status)
	}
	if !slices.Contains(kinds, a.Kind) {
		return domain.Approval{}, errs.Newf(errs.CodeFailedPrecondition, "approval %s is of kind %s, which does not authorize this", a.ID, a.Kind)
	}
	if a.AssetID != asset {
		return domain.Approval{}, errs.Newf(errs.CodeFailedPrecondition, "approval %s is for another asset", a.ID)
	}
	if a.AssetVersion != "" && a.AssetVersion != version {
		return domain.Approval{}, errs.Newf(errs.CodeFailedPrecondition, "approval %s is for version %s, not %s", a.ID, a.AssetVersion, version)
	}
	return a, nil
}

// checkUploadGate applies D37 to a record about to be uploaded.
func checkUploadGate(ctx context.Context, tx repo.TenantRepos, p domain.Publication, cfg config.Publishing) error {
	if p.ApprovalID != "" {
		_, err := checkApproval(ctx, tx, string(p.ApprovalID), p.AssetID, p.AssetVersion, domain.ApprovalPublication)
		return err
	}
	if allowPrivateFirst(cfg, p.Destination) {
		return nil
	}
	return errs.Newf(errs.CodeFailedPrecondition, "publication %s has no approval, and destination %s requires one before the upload (D37)", p.ID, p.Destination)
}

// getPublication resolves a publication parameter.
func getPublication(ctx context.Context, tx repo.TenantRepos, ref string) (domain.Publication, error) {
	if ref == "" {
		return domain.Publication{}, errs.New(errs.CodeInvalidArgument, "publicationId is required")
	}
	return tx.Publications().Get(ctx, id.Ref(ref))
}

// publicationsOf lists all of an asset's records across pages.
func publicationsOf(ctx context.Context, tx repo.TenantRepos, f repo.PublicationFilter) ([]domain.Publication, error) {
	var out []domain.Publication
	page := repo.Page{}
	for {
		items, next, err := tx.Publications().List(ctx, f, page)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
}

// emitter writes an event inside a transaction. A node uses env.Emit; the service,
// which has no node environment, appends to the outbox itself.
type emitter func(ctx context.Context, tx repo.TenantRepos, d event.Draft) error

func nodeEmitter(d node.DaemonEnv) emitter {
	return func(ctx context.Context, tx repo.TenantRepos, draft event.Draft) error {
		return d.Emit(ctx, tx, draft)
	}
}

// eventPayload is what publication events carry: no secret and no transcript (§8.2).
func eventPayload(p domain.Publication) map[string]any {
	out := map[string]any{
		"publication_id": string(p.ID),
		"destination":    p.Destination,
		"asset_version":  p.AssetVersion,
		"status":         string(p.Status),
		"visibility":     string(p.Visibility),
	}
	if p.RemoteID != "" {
		out["remote_id"] = p.RemoteID
	}
	if p.URL != "" {
		out["url"] = p.URL
	}
	if p.ApprovalID != "" {
		out["approval_id"] = string(p.ApprovalID)
	}
	if p.StatusReason != "" {
		out["reason"] = p.StatusReason
	}
	return out
}

// promoteAsset walks an asset to published through the states of §2.5, because
// AssetRepo.Update refuses a jump. Originals keep their state: §2.5 reserves
// published for derivatives. Publication approval is what makes these moves valid.
func promoteAsset(a *domain.Asset) []domain.AssetStatus {
	if a.Kind == domain.KindOriginal {
		return nil
	}
	switch a.Status {
	case domain.AssetReadyForClipping:
		return []domain.AssetStatus{domain.AssetEditorialReview, domain.AssetApproved, domain.AssetPublished}
	case domain.AssetEditorialReview:
		return []domain.AssetStatus{domain.AssetApproved, domain.AssetPublished}
	case domain.AssetApproved, domain.AssetWithdrawn:
		return []domain.AssetStatus{domain.AssetPublished}
	default:
		return nil
	}
}

// settleAsset writes the asset's status and visibility after a record changed, in
// the transaction that changed it. promote is true when an approved publication was
// just confirmed. With nothing active left, a published asset becomes withdrawn.
func settleAsset(ctx context.Context, tx repo.TenantRepos, assetID id.Friendly, promote bool, now time.Time) error {
	asset, err := tx.Assets().Get(ctx, id.Ref(assetID))
	if err != nil {
		return err
	}
	records, err := publicationsOf(ctx, tx, repo.PublicationFilter{AssetID: id.Ref(assetID)})
	if err != nil {
		return err
	}
	visibility := domain.VisibilityPrivate
	anyActive := false
	for _, r := range records {
		anyActive = anyActive || active(r.Status)
		if r.Status == domain.PubPublished {
			visibility = moreOpen(visibility, r.Visibility)
		}
	}
	changed := asset.Visibility != visibility
	asset.Visibility = visibility

	if promote {
		for _, next := range promoteAsset(&asset) {
			asset.Status = next
			asset.UpdatedAt = now
			if err := tx.Assets().Update(ctx, asset); err != nil {
				return err
			}
			changed = false
		}
	}
	if !anyActive && asset.Status == domain.AssetPublished {
		asset.Status = domain.AssetWithdrawn
		changed = true
	}
	if !changed {
		return nil
	}
	asset.UpdatedAt = now
	return tx.Assets().Update(ctx, asset)
}

func moreOpen(a, b domain.Visibility) domain.Visibility {
	rank := func(v domain.Visibility) int {
		switch v {
		case domain.VisibilityPublic:
			return 2
		case domain.VisibilityUnlisted:
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// auditEffect records an external visibility change (D37). It records the fact,
// never content, and a failure propagates so the effect is never unaudited.
func auditEffect(ctx context.Context, tx repo.TenantRepos, tenant domain.TenantID, actor, action string, perm domain.Permission, p domain.Publication, now time.Time) error {
	details, err := json.Marshal(map[string]string{
		"destination": p.Destination,
		"visibility":  string(p.Visibility),
		"approval_id": string(p.ApprovalID),
	})
	if err != nil {
		return err
	}
	_, err = tx.Audit().Append(ctx, domain.AuditRecord{
		ULID:       id.New(),
		TenantID:   tenant,
		At:         now,
		Actor:      actor,
		Action:     action,
		Permission: perm,
		Resource:   string(p.ID),
		Result:     "allowed",
		Details:    details,
	})
	return err
}

func jobActor(env node.Env) string { return "job:" + string(env.JobID()) }
