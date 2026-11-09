package publishingnodes

import (
	"context"
	"errors"
	"slices"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// Withdrawal modes (§15.8).
const (
	modePrivate = "private"
	modeDelete  = "delete"
)

type withdrawParams struct {
	AssetID      string   `json:"assetId"`
	Destinations []string `json:"destinations"`
	All          bool     `json:"all"`
	Mode         string   `json:"mode"`
}

type withdrawOutput struct {
	Withdrawn []string `json:"withdrawn"`
	Already   []string `json:"already"`
}

// withdrawNode takes publications down. It needs no approval (D17), repeating it is
// a no-op (§15.8), and a failing destination does not hold the others back (I236).
type withdrawNode struct{ base }

func (n *withdrawNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p withdrawParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if p.AssetID == "" {
		return node.Result{}, node.Permanent(errs.New(errs.CodeInvalidArgument, "assetId is required"))
	}
	if !p.All && len(p.Destinations) == 0 {
		return node.Result{}, node.Permanent(errs.New(errs.CodeInvalidArgument, "withdrawal needs destinations or all"))
	}
	mode := firstOf(p.Mode, modePrivate)
	if mode != modePrivate && mode != modeDelete {
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeInvalidArgument, "unknown withdrawal mode %q", mode))
	}

	store := d.Store()
	asset, err := store.Assets().Get(ctx, id.Ref(p.AssetID))
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	records, err := publicationsOf(ctx, store, repo.PublicationFilter{AssetID: id.Ref(asset.ID)})
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	if !p.All {
		cfg, err := d.TenantConfig(ctx)
		if err != nil {
			return node.Result{}, classifyStore(err)
		}
		for _, name := range p.Destinations {
			_, configured := cfg.Publishing.Destinations[name]
			hasRecord := slices.ContainsFunc(records, func(r domain.Publication) bool { return r.Destination == name })
			if !configured && !hasRecord {
				return node.Result{}, node.Permanent(errs.Newf(errs.CodeInvalidArgument, "destination %q is not configured and has no publication", name))
			}
		}
		records = slices.DeleteFunc(records, func(r domain.Publication) bool {
			return !slices.Contains(p.Destinations, r.Destination)
		})
	}

	out := withdrawOutput{Withdrawn: []string{}, Already: []string{}}
	var failures []error
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return node.Result{}, classify(err)
		}
		already, err := n.withdrawOne(ctx, d, record, mode)
		switch {
		case err != nil:
			failures = append(failures, err)
		case already:
			out.Already = append(out.Already, string(record.ID))
		default:
			out.Withdrawn = append(out.Withdrawn, string(record.ID))
		}
	}
	if len(failures) > 0 {
		joined := errors.Join(failures...)
		for _, f := range failures {
			if !retryable(f) && !errors.Is(f, context.Canceled) && !connector.IsOutcomeUnknown(f) {
				return node.Result{}, node.Permanent(joined)
			}
		}
		return node.Result{}, node.Transient(joined)
	}
	return node.JSONResult(out)
}

// withdrawOne reports already = true when nothing was left to do.
func (n *withdrawNode) withdrawOne(ctx context.Context, d node.DaemonEnv, record domain.Publication, mode string) (bool, error) {
	unlock, ok := n.locks.tryLock(string(record.ULID))
	if !ok {
		return false, errs.Newf(errs.CodeUnavailable, "another task is working on publication %s", record.ID)
	}
	defer unlock()

	// Re-read under the lock: a finishing upload may just have recorded its remote ID.
	record, err := d.Store().Publications().Get(ctx, id.Ref(record.ID))
	if err != nil {
		return false, err
	}
	if record.Status == domain.PubWithdrawn {
		return true, nil
	}
	mayBeRemote := record.Status == domain.PubUploading || record.Status == domain.PubFailed
	if record.RemoteID == "" && !mayBeRemote && record.Status != domain.PubPendingApproval {
		return true, nil
	}

	if record.RemoteID != "" || mayBeRemote {
		conn, err := d.Publishing(ctx, record.Destination)
		if err != nil {
			return false, err
		}
		if record.RemoteID == "" {
			// A video may exist though no ID was recorded: ask before saying it is gone.
			remote, err := conn.Reconcile(ctx, record.IdempotencyKey)
			if err != nil {
				return false, err
			}
			if remote != nil {
				record.RemoteID = remote.RemoteID
			}
		}
		if record.RemoteID == "" && record.Status == domain.PubFailed {
			return true, nil // it never reached the destination
		}
		if record.RemoteID != "" {
			if err := removeAtDestination(ctx, conn, record.RemoteID, mode); err != nil {
				return false, err
			}
		}
	}

	now := n.opts.Now().UTC()
	if err := move(&record, domain.PubWithdrawn, now); err != nil {
		return false, err
	}
	record.Visibility = domain.VisibilityPrivate
	record.WithdrawnAt = &now
	record.StatusReason = "withdrawn, mode " + mode
	ctx = context.WithoutCancel(ctx)
	err = d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Publications().Update(ctx, record); err != nil {
			return err
		}
		if err := settleAsset(ctx, tx, record.AssetID, false, now); err != nil {
			return err
		}
		if err := auditEffect(ctx, tx, d.Tenant(), jobActor(d), "publication.withdrawn", domain.PermPublicationWithdraw, record, now); err != nil {
			return err
		}
		return d.Emit(ctx, tx, event.Draft{Type: event.PublicationWithdrawn, AssetID: string(record.AssetID), Payload: eventPayload(record)})
	})
	if err != nil {
		return false, err
	}
	return false, nil
}

// removeAtDestination applies the mode, using the other one when the destination
// cannot do it. A video already gone counts as done.
func removeAtDestination(ctx context.Context, conn connector.Publishing, remoteID, mode string) error {
	caps := conn.Capabilities()
	private := len(caps.Visibilities) == 0 || slices.Contains(caps.Visibilities, domain.VisibilityPrivate)
	switch {
	case mode == modePrivate && !private && caps.Delete:
		mode = modeDelete
	case mode == modeDelete && !caps.Delete && private:
		mode = modePrivate
	}
	var err error
	if mode == modeDelete {
		err = conn.Delete(ctx, remoteID)
	} else {
		_, err = conn.SetVisibility(ctx, remoteID, domain.VisibilityPrivate)
		if errs.IsCode(err, errs.CodeCapabilityUnsupported) && caps.Delete {
			err = conn.Delete(ctx, remoteID)
		}
	}
	if errs.IsCode(err, errs.CodeNotFound) {
		return nil
	}
	return err
}
