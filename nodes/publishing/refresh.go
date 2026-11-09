package publishingnodes

import (
	"context"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

type refreshParams struct {
	PublicationID string `json:"publicationId"`
	AssetID       string `json:"assetId"`
}

type refreshed struct {
	PublicationID string `json:"publication_id"`
	Destination   string `json:"destination"`
	Status        string `json:"status"`
	Visibility    string `json:"visibility"`
	URL           string `json:"url,omitempty"`
	RemoteID      string `json:"remote_id,omitempty"`
	Changed       bool   `json:"changed"`
}

type refreshOutput struct {
	Publications []refreshed `json:"publications"`
}

// refreshNode re-reads remote state into the record.
type refreshNode struct{ base }

func (n *refreshNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p refreshParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	store := d.Store()
	var records []domain.Publication
	switch {
	case p.PublicationID != "":
		record, err := getPublication(ctx, store, p.PublicationID)
		if err != nil {
			return node.Result{}, classifyStore(err)
		}
		records = append(records, record)
	case p.AssetID != "":
		asset, err := store.Assets().Get(ctx, id.Ref(p.AssetID))
		if err != nil {
			return node.Result{}, classifyStore(err)
		}
		records, err = publicationsOf(ctx, store, repo.PublicationFilter{AssetID: id.Ref(asset.ID)})
		if err != nil {
			return node.Result{}, classifyStore(err)
		}
	default:
		return node.Result{}, node.Permanent(errs.New(errs.CodeInvalidArgument, "publicationId or assetId is required"))
	}

	out := refreshOutput{Publications: []refreshed{}}
	for _, record := range records {
		conn, err := connFor(ctx, d, record)
		if err != nil {
			return node.Result{}, classify(err)
		}
		updated, changed, err := refreshRecord(ctx, store, conn, d.Tenant(), jobActor(env), nodeEmitter(d), record, n.opts.Now().UTC())
		if err != nil {
			return node.Result{}, classify(err)
		}
		out.Publications = append(out.Publications, refreshed{
			PublicationID: string(updated.ID), Destination: updated.Destination, Status: string(updated.Status),
			Visibility: string(updated.Visibility), URL: updated.URL, RemoteID: updated.RemoteID, Changed: changed,
		})
	}
	return node.JSONResult(out)
}

// connFor opens a record's destination only when there is something to ask it.
func connFor(ctx context.Context, d node.DaemonEnv, r domain.Publication) (connector.Publishing, error) {
	if r.RemoteID == "" || r.Status == domain.PubWithdrawn {
		return nil, nil
	}
	return d.Publishing(ctx, r.Destination)
}

// refreshRecord applies one read of remote state. Shared by the node and
// service.Publications.Sync, which is why it takes an emitter.
func refreshRecord(ctx context.Context, store repo.TenantStore, conn connector.Publishing, tenant domain.TenantID, actor string, emit emitter, record domain.Publication, now time.Time) (domain.Publication, bool, error) {
	if record.RemoteID == "" || record.Status == domain.PubWithdrawn || conn == nil {
		return record, false, nil
	}
	remote, err := conn.GetStatus(ctx, record.RemoteID)
	if errs.IsCode(err, errs.CodeNotFound) {
		// A video that was live and is gone was removed. A fresh upload may just not be
		// listed yet, which must not read as a withdrawal.
		if record.Status != domain.PubPublished && record.Status != domain.PubScheduled {
			return record, false, nil
		}
		remote, err = connector.RemoteVideo{RemoteID: record.RemoteID, Status: connector.RemoteRemoved}, nil
	}
	if err != nil {
		return record, false, err
	}

	next := record
	completed, withdrawn := false, false
	switch remote.Status {
	case connector.RemoteRemoved:
		next.Visibility = domain.VisibilityPrivate
		next.WithdrawnAt = &now
		next.StatusReason = firstOf(remote.Reason, "removed at the destination")
		if err := move(&next, domain.PubWithdrawn, now); err != nil {
			return record, false, err
		}
		withdrawn = true
	case connector.RemoteRejected:
		next.StatusReason = firstOf(remote.Reason, "refused by the destination")
		if err := move(&next, domain.PubRejectedByDestination, now); err != nil {
			return record, false, err
		}
	case connector.RemoteFailed:
		if record.Status != domain.PubPublished {
			next.StatusReason = firstOf(remote.Reason, "failed at the destination")
			if err := move(&next, domain.PubFailed, now); err != nil {
				return record, false, err
			}
		}
	case connector.RemoteReady:
		if remote.URL != "" {
			next.URL = remote.URL
		}
		switch record.Status {
		case domain.PubScheduled:
			// The destination went live on schedule. R43 still applies.
			if remote.URL != "" {
				next.Visibility = remote.Visibility
				if err := move(&next, domain.PubPublished, now); err != nil {
					return record, false, err
				}
				if next.PublishedAt == nil {
					next.PublishedAt = &now
				}
				completed = true
			}
		case domain.PubPublished:
			if remote.Visibility != "" {
				next.Visibility = remote.Visibility
			}
		}
	case connector.RemoteScheduled:
		if remote.URL != "" {
			next.URL = remote.URL
		}
	}

	if !publicationChanged(record, next) {
		return record, false, nil
	}
	next.UpdatedAt = now
	ctx = context.WithoutCancel(ctx)
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Publications().Update(ctx, next); err != nil {
			return err
		}
		if completed {
			if err := tx.Versions().MarkImmutable(ctx, id.Ref(next.AssetID), next.AssetVersion); err != nil {
				return err
			}
		}
		if err := settleAsset(ctx, tx, next.AssetID, completed && next.ApprovalID != "", now); err != nil {
			return err
		}
		if record.Visibility != next.Visibility {
			if err := auditEffect(ctx, tx, tenant, actor, "publication.visibility_observed", domain.PermPublicationRead, next, now); err != nil {
				return err
			}
		}
		draft := event.Draft{Type: event.PublicationUpdated, AssetID: string(next.AssetID), Payload: eventPayload(next)}
		if err := emit(ctx, tx, draft); err != nil {
			return err
		}
		switch {
		case completed:
			draft.Type = event.PublicationCompleted
			return emit(ctx, tx, draft)
		case withdrawn:
			draft.Type = event.PublicationWithdrawn
			return emit(ctx, tx, draft)
		}
		return nil
	})
	if err != nil {
		return record, false, err
	}
	return next, true, nil
}

func publicationChanged(a, b domain.Publication) bool {
	return a.Status != b.Status || a.Visibility != b.Visibility || a.URL != b.URL || a.StatusReason != b.StatusReason
}
