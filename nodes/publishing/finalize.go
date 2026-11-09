package publishingnodes

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/publicexport"
)

type captionParams struct {
	Language string `json:"language"`
	Format   string `json:"format"`
}

type finalizeParams struct {
	PublicationID string            `json:"publicationId"`
	Visibility    domain.Visibility `json:"visibility"`
	Thumbnail     string            `json:"thumbnail"`
	Captions      *captionParams    `json:"captions"`
	ApprovalID    string            `json:"approvalId"`
	ScheduledAt   string            `json:"scheduledAt"`
}

type finalizeOutput struct {
	PublicationID string `json:"publication_id"`
	Status        string `json:"status"`
	URL           string `json:"url,omitempty"`
	Visibility    string `json:"visibility"`
	// Thumbnail and Captions report what happened to each: sent, skipped or failed.
	Thumbnail string `json:"thumbnail,omitempty"`
	Captions  string `json:"captions,omitempty"`
}

// Outcomes of the optional steps.
const (
	stepSent    = "sent"
	stepSkipped = "skipped"
	stepFailed  = "failed"
)

// finalizeNode opens visibility last, so a video is never public before its metadata
// is in place, and writes published only with a URL just returned by the destination
// (R43). Re-running it on a finished record changes nothing.
type finalizeNode struct{ base }

func (n *finalizeNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p finalizeParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	store := d.Store()
	record, err := getPublication(ctx, store, p.PublicationID)
	if err != nil {
		return node.Result{}, classifyStore(err)
	}

	switch record.Status {
	case domain.PubFailed, domain.PubRejectedByDestination, domain.PubWithdrawn:
		detail := record.StatusReason
		if detail == "" {
			detail = "no reason recorded"
		}
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeFailedPrecondition, "publication %s is %s: %s", record.ID, record.Status, detail))
	}
	if record.RemoteID == "" {
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeFailedPrecondition, "publication %s has no upload to finalize", record.ID))
	}

	target := firstVisibility(p.Visibility, record.Visibility, domain.VisibilityPrivate)
	wasPublished := record.Status == domain.PubPublished
	if wasPublished && target == record.Visibility {
		return node.JSONResult(finalizeOutputOf(record))
	}

	// Approval: anything but private needs one, and a change to a published video
	// needs a new one (D7, D37).
	approvalID := firstOf(p.ApprovalID, string(record.ApprovalID))
	if wasPublished && (p.ApprovalID == "" || id.Friendly(p.ApprovalID) == record.ApprovalID) {
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeFailedPrecondition,
			"changing the visibility of published publication %s needs a new publisher approval (D7)", record.ID))
	}
	approved := false
	if approvalID != "" {
		kinds := []domain.ApprovalKind{domain.ApprovalPublication, domain.ApprovalVisibilityChange}
		a, err := checkApproval(ctx, store, approvalID, record.AssetID, record.AssetVersion, kinds...)
		if err != nil {
			return node.Result{}, node.Permanent(err)
		}
		record.ApprovalID = a.ID
		approved = true
	}
	if target != domain.VisibilityPrivate && !approved {
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeFailedPrecondition,
			"visibility %s of publication %s needs an approved publication approval (D37)", target, record.ID))
	}

	var schedule *time.Time
	if p.ScheduledAt != "" {
		at, err := time.Parse(time.RFC3339, p.ScheduledAt)
		if err != nil {
			return node.Result{}, node.Permanent(errs.Wrap(errs.CodeInvalidArgument, err, "scheduledAt must be RFC 3339"))
		}
		if !approved {
			return node.Result{}, node.Permanent(errs.New(errs.CodeFailedPrecondition, "scheduling needs an approved publication approval (D37)"))
		}
		schedule = &at
	}

	conn, err := d.Publishing(ctx, record.Destination)
	if err != nil {
		return node.Result{}, classify(err)
	}
	caps := conn.Capabilities()
	if schedule != nil && !caps.Scheduling {
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeCapabilityUnsupported, "destination %s does not support scheduling", record.Destination))
	}

	remote, err := conn.GetStatus(ctx, record.RemoteID)
	if err != nil {
		return node.Result{}, classify(err)
	}
	switch remote.Status {
	case connector.RemoteProcessing:
		return node.Result{}, node.Transient(errs.Newf(errs.CodeUnavailable, "destination %s is still processing publication %s", record.Destination, record.ID))
	case connector.RemoteRejected, connector.RemoteFailed, connector.RemoteRemoved:
		return n.refused(ctx, d, record, remote)
	}

	out := finalizeOutput{PublicationID: string(record.ID)}
	if !wasPublished {
		if out.Thumbnail, err = n.sendThumbnail(ctx, d, conn, record, p.Thumbnail); err != nil {
			return node.Result{}, err
		}
		if target == domain.VisibilityPublic && approved && p.Captions != nil {
			if out.Captions, err = n.sendCaptions(ctx, d, conn, record, *p.Captions); err != nil {
				return node.Result{}, err
			}
		} else if p.Captions != nil {
			out.Captions = stepSkipped
		}
	}

	if schedule != nil {
		return n.schedule(ctx, d, conn, record, target, *schedule, out)
	}

	if remote.Visibility != target {
		remote, err = conn.SetVisibility(ctx, record.RemoteID, target)
		if err != nil {
			return node.Result{}, classify(err)
		}
	}
	// Ask again: the URL must come from a response about the final state (R43).
	remote, err = conn.GetStatus(ctx, record.RemoteID)
	if err != nil {
		return node.Result{}, classify(err)
	}
	if remote.Status != connector.RemoteReady || remote.URL == "" {
		return node.Result{}, node.Transient(errs.Newf(errs.CodeUnavailable, "destination %s has not returned a URL for publication %s yet", record.Destination, record.ID))
	}
	if remote.Visibility != target {
		return node.Result{}, node.Transient(errs.Newf(errs.CodeUnavailable, "destination %s reports visibility %s, not %s", record.Destination, remote.Visibility, target))
	}

	now := n.opts.Now().UTC()
	record.URL = remote.URL
	record.Visibility = remote.Visibility
	record.StatusReason = ""
	if err := move(&record, domain.PubPublished, now); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if record.PublishedAt == nil {
		record.PublishedAt = &now
	}
	ctx = context.WithoutCancel(ctx)
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Publications().Update(ctx, record); err != nil {
			return err
		}
		if err := tx.Versions().MarkImmutable(ctx, id.Ref(record.AssetID), record.AssetVersion); err != nil {
			return err
		}
		if err := settleAsset(ctx, tx, record.AssetID, approved, now); err != nil {
			return err
		}
		if err := auditEffect(ctx, tx, d.Tenant(), jobActor(env), "publication.visibility_set", record, now); err != nil {
			return err
		}
		eventType := event.PublicationCompleted
		if wasPublished {
			eventType = event.PublicationUpdated
		}
		return d.Emit(ctx, tx, event.Draft{Type: eventType, AssetID: string(record.AssetID), Payload: eventPayload(record)})
	})
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	result := finalizeOutputOf(record)
	result.Thumbnail, result.Captions = out.Thumbnail, out.Captions
	return node.JSONResult(result)
}

func firstVisibility(values ...domain.Visibility) domain.Visibility {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func finalizeOutputOf(r domain.Publication) finalizeOutput {
	return finalizeOutput{PublicationID: string(r.ID), Status: string(r.Status), URL: r.URL, Visibility: string(r.Visibility)}
}

// refused records what the destination said and stops the node.
func (n *finalizeNode) refused(ctx context.Context, d node.DaemonEnv, record domain.Publication, remote connector.RemoteVideo) (node.Result, error) {
	now := n.opts.Now().UTC()
	record.StatusReason = firstOf(remote.Reason, "the destination no longer has this video")
	if err := move(&record, statusFromRemote(remote), now); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	ctx = context.WithoutCancel(ctx)
	err := d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Publications().Update(ctx, record); err != nil {
			return err
		}
		return d.Emit(ctx, tx, event.Draft{Type: event.PublicationUpdated, AssetID: string(record.AssetID), Payload: eventPayload(record)})
	})
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	return node.Result{}, node.Permanent(errs.Newf(errs.CodeFailedPrecondition, "publication %s is %s: %s", record.ID, record.Status, record.StatusReason))
}

// schedule asks the destination to publish later (D52). The record stays scheduled
// until publish.refresh sees the video live and writes published.
func (n *finalizeNode) schedule(ctx context.Context, d node.DaemonEnv, conn connector.Publishing, record domain.Publication, target domain.Visibility, at time.Time, out finalizeOutput) (node.Result, error) {
	asset, err := d.Store().Assets().Get(ctx, id.Ref(record.AssetID))
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	remote, err := conn.Update(ctx, record.RemoteID, connector.VideoMetadata{
		Title: record.Title, Description: record.Description, Tags: asset.Tags, Language: asset.Language,
		Visibility: domain.VisibilityPrivate, ScheduledAt: &at,
	})
	if err != nil {
		return node.Result{}, classify(err)
	}
	if remote.Status != connector.RemoteScheduled {
		return node.Result{}, node.Transient(errs.Newf(errs.CodeUnavailable, "destination %s did not confirm the schedule", record.Destination))
	}
	now := n.opts.Now().UTC()
	record.Visibility = target
	record.URL = remote.URL
	if err := move(&record, domain.PubScheduled, now); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	ctx = context.WithoutCancel(ctx)
	err = d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Publications().Update(ctx, record); err != nil {
			return err
		}
		if err := auditEffect(ctx, tx, d.Tenant(), jobActor(d), "publication.visibility_scheduled", record, now); err != nil {
			return err
		}
		return d.Emit(ctx, tx, event.Draft{Type: event.PublicationUpdated, AssetID: string(record.AssetID), Payload: eventPayload(record)})
	})
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	result := finalizeOutputOf(record)
	result.Thumbnail, result.Captions = out.Thumbnail, out.Captions
	return node.JSONResult(result)
}

// sendThumbnail uploads the parameter's file, else the asset's. Only a transient
// destination error is returned; a missing thumbnail must not stop a publication.
func (n *finalizeNode) sendThumbnail(ctx context.Context, d node.DaemonEnv, conn connector.Publishing, record domain.Publication, param string) (string, error) {
	if !conn.Capabilities().Thumbnails {
		return stepSkipped, nil
	}
	ref := param
	if ref == "" {
		asset, err := d.Store().Assets().Get(ctx, id.Ref(record.AssetID))
		if err != nil {
			return "", classifyStore(err)
		}
		if asset.Thumbnail == nil {
			return stepSkipped, nil
		}
		ref = asset.Thumbnail.URI()
	}
	image, contentType, err := openThumbnail(ctx, d, ref)
	if err != nil {
		d.Logger().Warn("publishing without a thumbnail", "publication", string(record.ID), "code", string(errs.CodeOf(err)))
		return stepFailed, nil
	}
	defer image.Close()
	if err := conn.UploadThumbnail(ctx, record.RemoteID, image, contentType); err != nil {
		if retryable(err) || ctx.Err() != nil {
			return "", classify(err)
		}
		d.Logger().Warn("the destination refused the thumbnail", "publication", string(record.ID), "code", string(errs.CodeOf(err)))
		return stepFailed, nil
	}
	return stepSent, nil
}

func openThumbnail(ctx context.Context, d node.DaemonEnv, ref string) (io.ReadCloser, string, error) {
	u, err := domain.ParseInputURI(ref)
	if err != nil {
		return nil, "", errs.Wrap(errs.CodeInvalidArgument, err, "invalid thumbnail reference")
	}
	if err := u.ForbidRole(domain.RoleArchive); err != nil {
		return nil, "", err
	}
	if u.Kind == domain.URIStorage {
		st, err := d.Storage(ctx, u.Role)
		if err != nil {
			return nil, "", err
		}
		image, info, err := st.Get(ctx, u.Key)
		if err != nil {
			return nil, "", err
		}
		return image, firstOf(info.ContentType, thumbnailType(u.Key)), nil
	}
	local, err := d.OpenInput(ctx, ref, domain.RoleArchive)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(local.Path)
	if err != nil {
		return nil, "", errs.Wrap(errs.CodeUnavailable, err, "cannot open the thumbnail")
	}
	return f, thumbnailType(local.Path), nil
}

func thumbnailType(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".png") {
		return "image/png"
	}
	return "image/jpeg"
}

// sendCaptions renders captions from the stored transcript with sensitive segments
// removed and uploads them (D30, I228). If the marks cannot be read, no captions go
// out at all.
func (n *finalizeNode) sendCaptions(ctx context.Context, d node.DaemonEnv, conn connector.Publishing, record domain.Publication, p captionParams) (string, error) {
	if !conn.Capabilities().Captions {
		return stepSkipped, nil
	}
	store := d.Store()
	transcript, err := store.Transcripts().Get(ctx, id.Ref(record.AssetID), record.AssetVersion)
	if errs.IsCode(err, errs.CodeNotFound) {
		return stepSkipped, nil
	}
	if err != nil {
		return "", classifyStore(err)
	}
	asset, err := store.Assets().Get(ctx, id.Ref(record.AssetID))
	if err != nil {
		return "", classifyStore(err)
	}
	marks, err := publicexport.SensitiveIntervals(ctx, store, asset, record.AssetVersion)
	if err != nil {
		d.Logger().Warn("publishing without captions: sensitive marks could not be read", "publication", string(record.ID), "code", string(errs.CodeOf(err)))
		return stepFailed, nil
	}
	format := firstOf(p.Format, "vtt")
	language := firstOf(p.Language, transcript.Language, asset.Language)
	text, err := renderCaptions(format, publicexport.ReleasedSegments(transcript, marks))
	if err != nil {
		return "", node.Permanent(err)
	}
	if text == "" {
		return stepSkipped, nil
	}
	if err := conn.UploadCaptions(ctx, record.RemoteID, language, format, strings.NewReader(text)); err != nil {
		if retryable(err) || ctx.Err() != nil {
			return "", classify(err)
		}
		d.Logger().Warn("the destination refused the captions", "publication", string(record.ID), "code", string(errs.CodeOf(err)))
		return stepFailed, nil
	}
	return stepSent, nil
}
