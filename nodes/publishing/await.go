package publishingnodes

import (
	"context"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

type awaitParams struct {
	PublicationID  string `json:"publicationId"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

type awaitOutput struct {
	PublicationID string `json:"publication_id"`
	Status        string `json:"status"`
	URL           string `json:"url,omitempty"`
	// Ready is true when the destination finished and publish.finalize can run.
	Ready bool `json:"ready"`
}

// awaitNode polls until the destination finishes (I229). Rejection and failure are
// answers, not errors, so a workflow can route them, and publish.finalize refuses
// them either way. It writes nothing but the record, so class none is safe.
type awaitNode struct{ base }

func (n *awaitNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var params awaitParams
	if err := in.Bind(&params); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	record, err := getPublication(ctx, d.Store(), params.PublicationID)
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	switch record.Status {
	case domain.PubPublished, domain.PubScheduled, domain.PubWithdrawn, domain.PubFailed, domain.PubRejectedByDestination:
		return node.JSONResult(awaitOutputOf(record))
	}
	if record.RemoteID == "" {
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeFailedPrecondition, "publication %s has no upload to wait for", record.ID))
	}
	conn, err := d.Publishing(ctx, record.Destination)
	if err != nil {
		return node.Result{}, classify(err)
	}

	timeout := defaultAwaitTimeout
	if params.TimeoutSeconds > 0 {
		timeout = time.Duration(params.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	wait := n.opts.PollMin
	for {
		remote, err := conn.GetStatus(ctx, record.RemoteID)
		if err != nil {
			return node.Result{}, awaitError(ctx, err)
		}
		if remote.Status != connector.RemoteProcessing {
			return n.finish(ctx, d, record, remote)
		}
		env.Progress(0, "waiting for "+record.Destination+" to finish processing")
		select {
		case <-ctx.Done():
			return node.Result{}, awaitError(ctx, ctx.Err())
		case <-time.After(wait):
		}
		wait = min(wait*2, n.opts.PollMax)
	}
}

// awaitError maps our own timeout to a transient timeout error. The record stays
// processing, so publish.refresh or another attempt can pick it up.
func awaitError(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return node.Transient(errs.New(errs.CodeTimeout, "the destination did not finish processing in time"))
	}
	return classify(err)
}

func awaitOutputOf(r domain.Publication) awaitOutput {
	return awaitOutput{PublicationID: string(r.ID), Status: string(r.Status), URL: r.URL, Ready: r.Status == domain.PubPublished}
}

func (n *awaitNode) finish(ctx context.Context, d node.DaemonEnv, record domain.Publication, remote connector.RemoteVideo) (node.Result, error) {
	now := n.opts.Now().UTC()
	ready := false
	next := statusFromRemote(remote)
	if remote.Status == connector.RemoteReady {
		ready = true
		// A private-first upload that finished is waiting for its approval (I220).
		if record.ApprovalID == "" {
			next = domain.PubPendingApproval
		}
	}
	if remote.URL != "" {
		record.URL = remote.URL
	}
	record.StatusReason = remote.Reason
	if remote.Status == connector.RemoteRemoved && record.StatusReason == "" {
		record.StatusReason = "removed at the destination"
	}
	if err := move(&record, next, now); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	err := d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Publications().Update(ctx, record); err != nil {
			return err
		}
		switch record.Status {
		case domain.PubRejectedByDestination, domain.PubFailed, domain.PubScheduled:
			return d.Emit(ctx, tx, event.Draft{Type: event.PublicationUpdated, AssetID: string(record.AssetID), Payload: eventPayload(record)})
		}
		return nil
	})
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	out := awaitOutputOf(record)
	out.Ready = ready
	return node.JSONResult(out)
}
