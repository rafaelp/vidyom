package publishingnodes

import (
	"context"
	"slices"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

type prepareParams struct {
	AssetID     string            `json:"assetId"`
	Destination string            `json:"destination"`
	Version     string            `json:"version"`
	Visibility  domain.Visibility `json:"visibility"`
	ApprovalID  string            `json:"approvalId"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Tags        []string          `json:"tags"`
}

type prepareOutput struct {
	PublicationID    string `json:"publication_id"`
	SourceURI        string `json:"source_uri"`
	SHA256           string `json:"sha256"`
	AssetVersion     string `json:"asset_version"`
	AlreadyPublished bool   `json:"already_published"`
}

// prepareNode creates the record and picks the file. It is where approval timing
// is enforced first (D37, I221).
type prepareNode struct{ base }

func (n *prepareNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, node.Permanent(err)
	}
	var p prepareParams
	if err := in.Bind(&p); err != nil {
		return node.Result{}, node.Permanent(err)
	}
	if p.AssetID == "" || p.Destination == "" {
		return node.Result{}, node.Permanent(errs.New(errs.CodeInvalidArgument, "assetId and destination are required"))
	}

	cfg, err := d.TenantConfig(ctx)
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	destination, ok := cfg.Publishing.Destinations[p.Destination]
	if !ok {
		return node.Result{}, node.Permanent(errs.Newf(errs.CodeInvalidArgument, "destination %q is not configured", p.Destination))
	}
	conn, err := d.Publishing(ctx, p.Destination)
	if err != nil {
		return node.Result{}, classify(err)
	}
	if info := conn.Info(); !info.Available {
		return node.Result{}, node.Permanent(errs.NotImplemented("destination " + p.Destination + " (" + info.Reason + ")"))
	}

	key := in.IdempotencyKey
	if key == "" {
		key = node.IdempotencyKey(env.Tenant(), env.JobULID(), env.NodeID())
	}
	now := n.opts.Now().UTC()
	var out prepareOutput
	err = d.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		var err error
		out, err = n.prepare(ctx, d, tx, p, destination, cfg.Publishing, key, now)
		return err
	})
	if err != nil {
		return node.Result{}, classifyStore(err)
	}
	return node.JSONResult(out)
}

func (n *prepareNode) prepare(ctx context.Context, d node.DaemonEnv, tx repo.TenantRepos, p prepareParams, destination config.Destination, cfg config.Publishing, key string, now time.Time) (prepareOutput, error) {
	asset, err := tx.Assets().Get(ctx, id.Ref(p.AssetID))
	if err != nil {
		return prepareOutput{}, err
	}
	if asset.DeletedAt != nil {
		return prepareOutput{}, errs.NotFound("asset")
	}
	version := p.Version
	if version == "" {
		version = asset.CurrentVersion
	}

	// A repeat of this very node: same key, same record, no second event.
	if existing, found, err := tx.Publications().FindByIdempotencyKey(ctx, key); err != nil {
		return prepareOutput{}, err
	} else if found {
		return outputFor(tx, ctx, existing)
	}

	if err := publishableStatus(asset); err != nil {
		return prepareOutput{}, err
	}
	ver, err := tx.Versions().Get(ctx, id.Ref(asset.ID), version)
	if err != nil {
		return prepareOutput{}, err
	}
	master, published, err := pickSource(asset, ver, p.Destination)
	if err != nil {
		return prepareOutput{}, err
	}
	sum := master.SHA256
	if published != nil {
		sum = published.SHA256
	}

	// D37: an approval now, or a private upload where the option allows it.
	visibility := p.Visibility
	if visibility == "" {
		visibility = destination.Visibility
	}
	if visibility == "" {
		visibility = domain.VisibilityPrivate
	}
	approval := id.Friendly("")
	if p.ApprovalID != "" {
		a, err := checkApproval(ctx, tx, p.ApprovalID, asset.ID, version, domain.ApprovalPublication)
		if err != nil {
			return prepareOutput{}, err
		}
		approval = a.ID
	} else if allowPrivateFirst(cfg, p.Destination) {
		visibility = domain.VisibilityPrivate
	} else {
		return prepareOutput{}, errs.Newf(errs.CodeFailedPrecondition,
			"publishing to %s requires an approval before the upload (D37): pass approvalId from a publication approval", p.Destination)
	}

	if err := mergeTags(ctx, tx, asset, p.Tags, d, now); err != nil {
		return prepareOutput{}, err
	}

	// Another job's active record for the same version and destination is adopted,
	// so a second request is not a second upload (I222).
	records, err := publicationsOf(ctx, tx, repo.PublicationFilter{AssetID: id.Ref(asset.ID), Destination: p.Destination})
	if err != nil {
		return prepareOutput{}, err
	}
	for _, r := range records {
		if r.AssetVersion != version || !active(r.Status) {
			continue
		}
		if r.ApprovalID == "" && approval != "" {
			r.ApprovalID = approval
			if r.Status != domain.PubPublished {
				r.Visibility = visibility
			}
			r.UpdatedAt = now
			if err := tx.Publications().Update(ctx, r); err != nil {
				return prepareOutput{}, err
			}
		}
		out, err := outputFor(tx, ctx, r)
		if err != nil {
			return prepareOutput{}, err
		}
		// A different target on a published video goes on to publish.finalize, which
		// requires a new approval for the change (D7).
		out.AlreadyPublished = r.Status == domain.PubPublished && r.Visibility == visibility
		return out, nil
	}

	friendly, err := tx.IDs().Next(ctx, id.PrefixPublication)
	if err != nil {
		return prepareOutput{}, err
	}
	record := domain.Publication{
		ID:                  friendly,
		ULID:                id.New(),
		TenantID:            d.Tenant(),
		AssetID:             asset.ID,
		Destination:         p.Destination,
		AssetVersion:        version,
		PublishedFileSHA256: sum,
		PublishedFile:       published,
		Status:              domain.PubPendingApproval,
		Visibility:          visibility,
		ApprovalID:          approval,
		Title:               firstOf(p.Title, asset.Title),
		Description:         firstOf(p.Description, asset.Description),
		IdempotencyKey:      key,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := tx.Publications().Create(ctx, record); err != nil {
		return prepareOutput{}, err
	}
	if err := d.Emit(ctx, tx, event.Draft{Type: event.PublicationCreated, AssetID: string(asset.ID), Payload: eventPayload(record)}); err != nil {
		return prepareOutput{}, err
	}
	return outputFor(tx, ctx, record)
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// publishableStatus refuses assets that cannot go out. The gate itself is the
// approval: a derivative in review or ready for clipping may go out with one, or
// privately where D37 allows.
func publishableStatus(a domain.Asset) error {
	switch a.Status {
	case domain.AssetApproved, domain.AssetPublished, domain.AssetWithdrawn,
		domain.AssetEditorialReview, domain.AssetReadyForClipping:
		return nil
	default:
		return errs.Newf(errs.CodeFailedPrecondition, "an asset in state %s cannot be published", a.Status)
	}
}

// pickSource returns the master, and a different published file when one exists
// (R4). A file identical to the master is not a second file. Everything must be in
// role master: publishing never reads a backup (§7.3).
func pickSource(a domain.Asset, v domain.AssetVersion, destination string) (domain.MediaFile, *domain.MediaFile, error) {
	var master *domain.MediaFile
	var named, general *domain.MediaFile
	for i := range v.Files {
		f := &v.Files[i]
		switch {
		case f.Purpose == domain.FileWorkingMaster && f.Name == "":
			master = &f.File
		case f.Purpose == domain.FilePublishedFile && f.Name == destination:
			named = &f.File
		case f.Purpose == domain.FilePublishedFile && f.Name == "":
			general = &f.File
		}
	}
	if master == nil {
		return domain.MediaFile{}, nil, errs.Newf(errs.CodeFailedPrecondition, "version %s of %s has no working master", v.Version, a.ID)
	}
	if v.Version == a.CurrentVersion && !a.MasterAvailable {
		return domain.MediaFile{}, nil, errs.Newf(errs.CodeFailedPrecondition, "the master of %s is not available", a.ID)
	}
	if master.Storage != domain.RoleMaster {
		return domain.MediaFile{}, nil, errs.Newf(errs.CodeForbiddenStorageRole, "publishing only reads from the master role, not %s", master.Storage)
	}
	if master.SHA256 == "" {
		return domain.MediaFile{}, nil, errs.Newf(errs.CodeFailedPrecondition, "the master of %s has no recorded checksum", a.ID)
	}
	published := named
	if published == nil {
		published = general
	}
	if published == nil || published.SHA256 == master.SHA256 {
		return *master, nil, nil
	}
	if published.Storage != domain.RoleMaster {
		return domain.MediaFile{}, nil, errs.Newf(errs.CodeForbiddenStorageRole, "a published file must be in the master role, not %s", published.Storage)
	}
	if published.SHA256 == "" {
		return domain.MediaFile{}, nil, errs.Newf(errs.CodeFailedPrecondition, "the published file of %s has no recorded checksum", a.ID)
	}
	file := *published
	return *master, &file, nil
}

// mergeTags adds requested tags to the asset, with a history entry, because a
// publication has no tags of its own (I222).
func mergeTags(ctx context.Context, tx repo.TenantRepos, asset domain.Asset, tags []string, env node.Env, now time.Time) error {
	merged := slices.Clone(asset.Tags)
	for _, t := range tags {
		if t != "" && !slices.Contains(merged, t) {
			merged = append(merged, t)
		}
	}
	if len(merged) == len(asset.Tags) {
		return nil
	}
	asset.Tags = merged
	asset.UpdatedAt = now
	if err := tx.Assets().Update(ctx, asset); err != nil {
		return err
	}
	return tx.Assets().AppendHistory(ctx, domain.HistoryEntry{
		AssetID: asset.ID,
		At:      now,
		Actor:   jobActor(env),
		Action:  "publish.prepare.tags",
	})
}

// outputFor builds the node's output from a record.
func outputFor(tx repo.TenantRepos, ctx context.Context, r domain.Publication) (prepareOutput, error) {
	file, err := sourceOf(ctx, tx, r)
	if err != nil {
		return prepareOutput{}, err
	}
	return prepareOutput{
		PublicationID:    string(r.ID),
		SourceURI:        file.URI(),
		SHA256:           r.PublishedFileSHA256,
		AssetVersion:     r.AssetVersion,
		AlreadyPublished: r.Status == domain.PubPublished,
	}, nil
}

// sourceOf finds the file a record sends: its own published file, or the version's
// master, whose hash must still be the recorded one.
func sourceOf(ctx context.Context, tx repo.TenantRepos, r domain.Publication) (domain.MediaFile, error) {
	if r.PublishedFile != nil {
		return *r.PublishedFile, nil
	}
	v, err := tx.Versions().Get(ctx, id.Ref(r.AssetID), r.AssetVersion)
	if err != nil {
		return domain.MediaFile{}, err
	}
	for _, f := range v.Files {
		if f.Purpose == domain.FileWorkingMaster && f.Name == "" {
			if f.File.SHA256 != r.PublishedFileSHA256 {
				return domain.MediaFile{}, errs.Newf(errs.CodeFailedPrecondition, "the master of %s changed after publication %s was prepared", r.AssetID, r.ID)
			}
			return f.File, nil
		}
	}
	return domain.MediaFile{}, errs.Newf(errs.CodeFailedPrecondition, "version %s of %s has no working master", r.AssetVersion, r.AssetID)
}
