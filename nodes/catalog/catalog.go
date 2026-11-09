package catalognodes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/event"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/schemas"
)

// HostEnv is an optional capability of the environment: the daemon's settings, for
// what a node needs from the host (the spool, retention).
type HostEnv interface {
	DaemonConfig() config.Daemon
}

type runner func(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error)

type catalogNode struct {
	desc node.Descriptor
	run  runner
}

func (n catalogNode) Descriptor() node.Descriptor { return n.desc }

func (n catalogNode) Execute(ctx context.Context, env node.Env, in node.Input) (node.Result, error) {
	d, err := node.RequireDaemon(env)
	if err != nil {
		return node.Result{}, err
	}
	res, err := n.run(ctx, d, in)
	if err != nil {
		return node.Result{}, classify(err)
	}
	return res, nil
}

// classify marks what a retry can fix. A wrong status or a missing asset cannot.
func classify(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return node.Transient(err)
	}
	switch errs.CodeOf(err) {
	case errs.CodeUnavailable, errs.CodeTimeout, errs.CodeRateLimited:
		return node.Transient(err)
	default:
		return node.Permanent(err)
	}
}

// Register adds every catalog.* node this component owns.
func Register(r node.Registrar) {
	for name, run := range map[string]runner{
		"catalog.register":                register,
		"catalog.get":                     get,
		"catalog.list":                    list,
		"catalog.update_metadata":         updateMetadata,
		"catalog.set_status":              setStatus,
		"catalog.set_flags":               setFlags,
		"catalog.record_file":             recordFile,
		"catalog.record_provenance":       recordProvenance,
		"catalog.next_version":            nextVersion,
		"catalog.mark_ready":              markReady,
		"catalog.check_overlap":           checkOverlap,
		"catalog.resolve_render_source":   resolveRenderSource,
		"catalog.export_package":          exportPackage,
		"catalog.list_expired_renders":    listExpiredRenders,
		"catalog.list_purgeable_archives": listPurgeableArchives,
		"catalog.release_input":           releaseInput,
		"catalog.delete":                  deleteAsset,
		"catalog.get_transcript":          getTranscript,
	} {
		d, err := schemas.DescriptorFor(name, 1)
		if err != nil {
			panic(err)
		}
		r.RegisterNode(catalogNode{desc: d, run: run})
	}
}

// liveAsset resolves an asset, treating a deleted one as gone: its friendly ID stays
// burned, but nothing works on it any more (D17).
func liveAsset(ctx context.Context, repos interface{ Assets() repo.AssetRepo }, ref string) (domain.Asset, error) {
	a, err := repos.Assets().Get(ctx, id.Ref(ref))
	if err != nil {
		return domain.Asset{}, err
	}
	if a.DeletedAt != nil {
		return domain.Asset{}, errs.NotFound("asset")
	}
	return a, nil
}

func invalid(format string, args ...any) error {
	return errs.Newf(errs.CodeInvalidArgument, format, args...)
}

func precondition(format string, args ...any) error {
	return errs.Newf(errs.CodeFailedPrecondition, format, args...)
}

func actor(ctx context.Context, env node.DaemonEnv) string {
	j, err := env.Store().Jobs().GetByULID(ctx, env.JobULID())
	if err != nil || j.SubmittedBy == "" {
		return string(env.JobID())
	}
	return j.SubmittedBy
}

func keyPrefix(a domain.Asset) string { return KeyPrefix(a.Kind, a.ID, a.CurrentVersion) }

// KeyPrefix is the role-relative key prefix of a version (§14.1).
func KeyPrefix(kind domain.AssetKind, asset id.Friendly, version string) string {
	return fmt.Sprintf("%s/%s/%s", kind.StoragePrefix(), asset, version)
}

// assetOutput is catalog.get's output: the asset plus URIs usable as parameters.
func assetOutput(a domain.Asset) map[string]any {
	out := map[string]any{
		"id":               a.ID,
		"ulid":             a.ULID,
		"kind":             a.Kind,
		"status":           a.Status,
		"title":            a.Title,
		"current_version":  a.CurrentVersion,
		"visibility":       a.Visibility,
		"ai_processing":    a.AIProcessing,
		"master_available": a.MasterAvailable,
		"proxy_available":  a.ProxyAvailable,
		"backup_verified":  a.BackupVerified,
		"key_prefix":       keyPrefix(a),
	}
	if a.Category != "" {
		out["category"] = a.Category
	}
	if a.Language != "" {
		out["language"] = a.Language
	}
	if a.DurationSeconds > 0 {
		out["duration_seconds"] = a.DurationSeconds
	}
	if a.Lineage != nil && a.Lineage.RootAssetID != "" {
		out["root_asset_id"] = a.Lineage.RootAssetID
	}
	if a.WorkingMaster != nil {
		out["working_master_uri"] = a.WorkingMaster.URI()
	}
	if a.Proxy != nil {
		out["proxy_uri"] = a.Proxy.URI()
	}
	if a.SourceOriginal != nil {
		out["source_original_uri"] = a.SourceOriginal.URI()
		out["archive_uri"] = a.SourceOriginal.URI()
	} else if a.Backup != nil {
		out["archive_uri"] = a.Backup.URI()
	}
	return out
}

// ---- catalog.register ----

type lineageParam struct {
	Method              domain.LineageMethod `json:"method"`
	SourceAssetID       string               `json:"source_asset_id"`
	SelectionApprovalID id.Friendly          `json:"selection_approval_id"`
	Segments            []domain.Segment     `json:"segments"`
}

// stableULID derives the asset's ULID from the node's idempotency key, so a re-run
// after a lost acknowledgment finds the asset instead of creating a second one.
func stableULID(job id.ULID, key string) id.ULID {
	sum := sha256.Sum256([]byte("catalog.register\x00" + key))
	var u ulid.ULID
	if parsed, err := ulid.ParseStrict(string(job)); err == nil {
		copy(u[:6], parsed[:6])
	}
	copy(u[6:], sum[:10])
	return id.ULID(u.String())
}

func register(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		Kind         domain.AssetKind    `json:"kind"`
		Title        string              `json:"title"`
		Category     string              `json:"category"`
		Description  string              `json:"description"`
		Language     string              `json:"language"`
		Collection   string              `json:"collection"`
		Speaker      string              `json:"speaker"`
		Visibility   domain.Visibility   `json:"visibility"`
		AIProcessing domain.AIProcessing `json:"aiProcessing"`
		Lineage      *lineageParam       `json:"lineage"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	store := env.Store()
	ulidKey := stableULID(env.JobULID(), in.IdempotencyKey)
	output := func(a domain.Asset) (node.Result, error) {
		return node.JSONResult(map[string]any{
			"id": a.ID, "ulid": a.ULID, "kind": a.Kind, "version": a.CurrentVersion, "key_prefix": keyPrefix(a),
		})
	}
	if existing, err := store.Assets().Get(ctx, id.Ref(ulidKey)); err == nil {
		return output(existing)
	} else if !errs.IsCode(err, errs.CodeNotFound) {
		return node.Result{}, err
	}

	asset := domain.Asset{
		ULID:           ulidKey,
		TenantID:       env.Tenant(),
		Kind:           p.Kind,
		Category:       p.Category,
		Title:          p.Title,
		Description:    p.Description,
		Language:       p.Language,
		Collection:     p.Collection,
		Speaker:        p.Speaker,
		Visibility:     domain.VisibilityPrivate,
		AIProcessing:   p.AIProcessing,
		Status:         domain.AssetRegistered,
		CurrentVersion: "v1",
		CatalogSync:    domain.CatalogSyncNone,
	}
	if asset.Category == "" {
		asset.Category = "other"
	}
	if p.Visibility != "" && p.Visibility != domain.VisibilityPrivate {
		// Assets are born private (§2.5). Visibility changes come from publishing, after
		// a publisher's approval (D7).
		return node.Result{}, invalid("an asset is born private; %s comes from publishing (§2.5)", p.Visibility)
	}
	if p.AIProcessing == domain.AIExternalAllowed {
		if !env.HasPermission(domain.PermAssetAIConsent) {
			return node.Result{}, errs.Forbidden(string(domain.PermAssetAIConsent))
		}
		asset.AIConsent = &domain.AIConsent{AuthorizedBy: actor(ctx, env), AuthorizedAt: time.Now().UTC(), Scope: "asset"}
	}
	if p.Kind == domain.KindOriginal {
		if p.Lineage != nil {
			return node.Result{}, invalid("an original has no lineage")
		}
	} else {
		if p.Lineage == nil {
			return node.Result{}, invalid("a %s needs a lineage (§2.3)", p.Kind)
		}
		if p.Lineage.Method != domain.LineageComputed {
			// Declared lineage is Future (D50).
			return node.Result{}, errs.NotImplemented("lineage method " + string(p.Lineage.Method))
		}
		lineage, parent, err := buildLineage(ctx, store, p.Lineage)
		if err != nil {
			return node.Result{}, err
		}
		asset.Lineage = lineage
		asset.ParentAssetID = parent.ID
		if asset.Language == "" {
			asset.Language = parent.Language
		}
		if asset.Collection == "" {
			asset.Collection = parent.Collection
		}
		if asset.Speaker == "" {
			asset.Speaker = parent.Speaker
		}
	}
	err := store.InTx(ctx, func(tx repo.TenantRepos) error {
		friendly, err := tx.IDs().Next(ctx, p.Kind.IDPrefix())
		if err != nil {
			return err
		}
		asset.ID = friendly
		if err := tx.Assets().Create(ctx, asset); err != nil {
			return err
		}
		if err := tx.Versions().Create(ctx, domain.AssetVersion{AssetID: friendly, Version: asset.CurrentVersion}); err != nil {
			return err
		}
		if err := env.Emit(ctx, tx, event.Draft{Type: event.AssetCreated, AssetID: string(friendly), Payload: map[string]any{"kind": p.Kind}}); err != nil {
			return err
		}
		if p.Kind == domain.KindClip {
			return env.Emit(ctx, tx, event.Draft{Type: event.ClipCreated, AssetID: string(friendly)})
		}
		return nil
	})
	if err != nil {
		return node.Result{}, err
	}
	return output(asset)
}

// buildLineage fills what the source knows: root, chain and versions (D36, §12.3).
func buildLineage(ctx context.Context, store repo.TenantStore, p *lineageParam) (*domain.Lineage, domain.Asset, error) {
	if p.SourceAssetID == "" {
		return nil, domain.Asset{}, invalid("a lineage needs source_asset_id")
	}
	if len(p.Segments) == 0 {
		return nil, domain.Asset{}, invalid("a lineage needs segments (R27)")
	}
	source, err := store.Assets().Get(ctx, id.Ref(p.SourceAssetID))
	if err != nil {
		return nil, domain.Asset{}, err
	}
	out := &domain.Lineage{
		Method:              p.Method,
		SourceAssetID:       source.ID,
		RootAssetID:         source.ID,
		SelectionApprovalID: p.SelectionApprovalID,
		Chain:               []id.Friendly{source.ID},
	}
	if source.Lineage != nil {
		out.RootAssetID = source.Lineage.RootAssetID
		if out.RootAssetID == "" {
			out.RootAssetID = source.Lineage.SourceAssetID
		}
		out.Chain = append(out.Chain, source.Lineage.Chain...)
	}
	for _, s := range p.Segments {
		if s.EndSeconds <= s.StartSeconds {
			return nil, domain.Asset{}, invalid("a segment must end after it starts")
		}
		if s.SourceAssetID == "" {
			s.SourceAssetID = source.ID
		}
		if s.SourceVersion == "" {
			s.SourceVersion = source.CurrentVersion
		}
		out.Segments = append(out.Segments, s)
	}
	return out, source, nil
}

// ---- catalog.get, catalog.list ----

func get(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		ID                     string             `json:"id"`
		RequiredStatus         domain.AssetStatus `json:"requiredStatus"`
		RequireClipEligible    bool               `json:"requireClipEligible"`
		RequireMasterAvailable bool               `json:"requireMasterAvailable"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	a, err := env.Store().Assets().Get(ctx, id.Ref(p.ID))
	if err != nil {
		return node.Result{}, err
	}
	if a.DeletedAt != nil {
		return node.Result{}, errs.NotFound("asset")
	}
	if p.RequiredStatus != "" && a.Status != p.RequiredStatus {
		return node.Result{}, precondition("asset %s is %s, not %s", a.ID, a.Status, p.RequiredStatus)
	}
	// The invariant of §3.2: a missing master is an error, and nothing here reaches
	// for the archive.
	if p.RequireMasterAvailable && (!a.MasterAvailable || a.WorkingMaster == nil) {
		return node.Result{}, precondition("asset %s has no available master; restoring it is a separate administrative flow (§3.2)", a.ID)
	}
	if p.RequireClipEligible && !a.ClipEligible() {
		return node.Result{}, precondition("asset %s cannot be a clip source: %s (I21)", a.ID, whyNotReady(a))
	}
	return node.JSONResult(assetOutput(a))
}

func whyNotReady(a domain.Asset) string {
	var missing []string
	if !a.BackupVerified {
		missing = append(missing, "backup not verified")
	}
	if !a.MasterAvailable {
		missing = append(missing, "master not available")
	}
	if !a.ProxyAvailable {
		missing = append(missing, "proxy not available")
	}
	if !a.TimelineValidated {
		missing = append(missing, "timeline not validated")
	}
	if a.Transcript == nil {
		missing = append(missing, "no transcript")
	} else {
		if !a.Transcript.Indexed {
			missing = append(missing, "text and vector indexes incomplete")
		}
		if a.Transcript.EmbeddingModel == "" {
			missing = append(missing, "no embedding model")
		}
	}
	if a.DeletedAt != nil {
		missing = append(missing, "deleted")
	}
	if len(missing) == 0 {
		return "status " + string(a.Status)
	}
	return strings.Join(missing, ", ")
}

func list(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		Kind   domain.AssetKind   `json:"kind"`
		Status domain.AssetStatus `json:"status"`
		Limit  int                `json:"limit"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	if p.Limit <= 0 || p.Limit > 1000 {
		p.Limit = 1000
	}
	items := []map[string]any{}
	cursor := ""
	for len(items) < p.Limit {
		page, next, err := env.Store().Assets().List(ctx, repo.AssetFilter{Kind: p.Kind, Status: p.Status}, repo.Page{Limit: p.Limit - len(items), Cursor: cursor})
		if err != nil {
			return node.Result{}, err
		}
		for _, a := range page {
			items = append(items, assetOutput(a))
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return node.JSONResult(map[string]any{"items": items})
}

// ---- metadata, status and flags ----

func updateMetadata(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID     string    `json:"assetId"`
		Title       *string   `json:"title"`
		Description *string   `json:"description"`
		Tags        *[]string `json:"tags"`
		Category    *string   `json:"category"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	who := actor(ctx, env)
	var friendly id.Friendly
	err := env.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		a, err := liveAsset(ctx, tx, p.AssetID)
		if err != nil {
			return err
		}
		friendly = a.ID
		changes := map[string]any{}
		if p.Title != nil && *p.Title != a.Title {
			a.Title = *p.Title
			changes["title"] = *p.Title
		}
		if p.Description != nil && *p.Description != a.Description {
			a.Description = *p.Description
			changes["description"] = *p.Description
		}
		if p.Tags != nil {
			a.Tags = *p.Tags
			changes["tags"] = *p.Tags
		}
		if p.Category != nil && *p.Category != a.Category {
			a.Category = *p.Category
			changes["category"] = *p.Category
		}
		if len(changes) == 0 {
			return nil
		}
		if err := tx.Assets().Update(ctx, a); err != nil {
			return err
		}
		return appendHistory(ctx, tx, a.ID, who, "metadata_changed", changes)
	})
	if err != nil {
		return node.Result{}, err
	}
	return node.JSONResult(map[string]any{"asset_id": friendly})
}

func appendHistory(ctx context.Context, tx repo.TenantRepos, asset id.Friendly, who, action string, changes map[string]any) error {
	raw, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	return tx.Assets().AppendHistory(ctx, domain.HistoryEntry{AssetID: asset, At: time.Now().UTC(), Actor: who, Action: action, Changes: raw})
}

func setStatus(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID string             `json:"assetId"`
		Status  domain.AssetStatus `json:"status"`
		Reason  string             `json:"reason"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	known := false
	for _, s := range domain.AssetStatuses {
		if s == p.Status {
			known = true
		}
	}
	if !known {
		return node.Result{}, invalid("unknown asset status %q", p.Status)
	}
	who := actor(ctx, env)
	var friendly id.Friendly
	err := env.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		a, err := liveAsset(ctx, tx, p.AssetID)
		if err != nil {
			return err
		}
		friendly = a.ID
		if a.Status == p.Status {
			return nil
		}
		if !a.Status.CanTransition(p.Status) {
			return precondition("asset %s cannot move from %s to %s (§2.5)", a.ID, a.Status, p.Status)
		}
		from := a.Status
		a.Status = p.Status
		if err := tx.Assets().Update(ctx, a); err != nil {
			return err
		}
		return appendHistory(ctx, tx, a.ID, who, "status_changed", map[string]any{"from": from, "to": p.Status, "reason": p.Reason})
	})
	if err != nil {
		return node.Result{}, err
	}
	return node.JSONResult(map[string]any{"asset_id": friendly, "status": p.Status})
}

func setFlags(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID           string              `json:"assetId"`
		MasterAvailable   *bool               `json:"masterAvailable"`
		BackupVerified    *bool               `json:"backupVerified"`
		ProxyAvailable    *bool               `json:"proxyAvailable"`
		TimelineValidated *bool               `json:"timelineValidated"`
		CatalogSync       *domain.CatalogSync `json:"catalogSync"`
		DurationSeconds   *float64            `json:"durationSeconds"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	var friendly id.Friendly
	err := env.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		a, err := liveAsset(ctx, tx, p.AssetID)
		if err != nil {
			return err
		}
		friendly = a.ID
		if p.MasterAvailable != nil {
			a.MasterAvailable = *p.MasterAvailable
		}
		if p.BackupVerified != nil {
			a.BackupVerified = *p.BackupVerified
		}
		if p.ProxyAvailable != nil {
			a.ProxyAvailable = *p.ProxyAvailable
		}
		if p.TimelineValidated != nil {
			a.TimelineValidated = *p.TimelineValidated
		}
		if p.CatalogSync != nil {
			a.CatalogSync = *p.CatalogSync
		}
		if p.DurationSeconds != nil {
			a.DurationSeconds = *p.DurationSeconds
		}
		return tx.Assets().Update(ctx, a)
	})
	if err != nil {
		return node.Result{}, err
	}
	return node.JSONResult(map[string]any{"asset_id": friendly})
}

// ---- files, provenance, versions ----

// purposeRole says which role a file of a purpose must sit in. A working master in
// archive would be the first step towards clipping from it (§3.2).
var purposeRole = map[domain.FilePurpose]domain.StorageRole{
	domain.FileSourceOriginal: domain.RoleArchive,
	domain.FileBackup:         domain.RoleArchive,
	domain.FileWorkingMaster:  domain.RoleMaster,
	domain.FilePublishedFile:  domain.RoleMaster,
	domain.FileProxy:          domain.RoleProxy,
}

func recordFile(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID string             `json:"assetId"`
		Version string             `json:"version"`
		Purpose domain.FilePurpose `json:"purpose"`
		Name    string             `json:"name"`
		Object  struct {
			Storage   domain.StorageRole `json:"storage"`
			Key       string             `json:"key"`
			SHA256    string             `json:"sha256"`
			SizeBytes int64              `json:"size_bytes"`
			Verified  bool               `json:"verified"`
			Profile   string             `json:"profile"`
			Location  *domain.Location   `json:"location"`
		} `json:"object"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	if want, ok := purposeRole[p.Purpose]; ok && p.Object.Storage != want {
		return node.Result{}, errs.Newf(errs.CodeForbiddenStorageRole, "a %s file belongs in role %s, not %s", p.Purpose, want, p.Object.Storage)
	}
	if err := domain.ValidateStorageKey(p.Object.Key); err != nil {
		return node.Result{}, invalid("%v", err)
	}
	store := env.Store()
	a, err := liveAsset(ctx, store, p.AssetID)
	if err != nil {
		return node.Result{}, err
	}
	version := p.Version
	if version == "" {
		version = a.CurrentVersion
	}
	file := domain.VersionFile{Purpose: p.Purpose, Name: p.Name, File: domain.MediaFile{
		Storage: p.Object.Storage, Key: p.Object.Key, SHA256: p.Object.SHA256, SizeBytes: p.Object.SizeBytes,
		Profile: p.Object.Profile, Verified: p.Object.Verified, Location: p.Object.Location, RecordedAt: time.Now().UTC(),
	}}
	if err := store.Versions().PutFile(ctx, id.Ref(a.ID), version, file); err != nil {
		return node.Result{}, err
	}
	return node.JSONResult(map[string]any{"asset_id": a.ID, "version": version, "purpose": p.Purpose})
}

// toolsData is what the engine stores on a node's job event when a node reports tool
// versions (node.Result.Tools).
type toolsData struct {
	Tools map[string]string `json:"tools"`
}

func recordProvenance(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID  string            `json:"assetId"`
		Version  string            `json:"version"`
		Inputs   map[string]string `json:"inputs"`
		Outputs  map[string]string `json:"outputs"`
		Profiles []string          `json:"profiles"`
		Tools    map[string]string `json:"tools"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	store := env.Store()
	a, err := liveAsset(ctx, store, p.AssetID)
	if err != nil {
		return node.Result{}, err
	}
	job, err := store.Jobs().GetByULID(ctx, env.JobULID())
	if err != nil {
		return node.Result{}, err
	}
	nodes, err := store.Jobs().ListNodes(ctx, env.JobULID())
	if err != nil {
		return node.Result{}, err
	}
	types := map[string]int{}
	for _, n := range nodes {
		types[n.NodeType] = n.TypeVersion
	}
	tools := map[string]string{}
	events, err := store.Jobs().ListEvents(ctx, id.Ref(job.ID), 0, 0)
	if err != nil {
		return node.Result{}, err
	}
	for _, e := range events {
		if e.Kind != domain.JobEventNode || len(e.Data) == 0 {
			continue
		}
		var d toolsData
		if json.Unmarshal(e.Data, &d) == nil {
			for k, v := range d.Tools {
				tools[k] = v
			}
		}
	}
	for k, v := range p.Tools {
		tools[k] = v
	}
	version := p.Version
	if version == "" {
		version = a.CurrentVersion
	}
	prov := domain.Provenance{
		JobID:          job.ID,
		Workflow:       job.WorkflowID + "@" + job.WorkflowVersion,
		WorkflowSHA256: job.WorkflowSHA256,
		NodeTypes:      types,
		Profiles:       p.Profiles,
		Tools:          tools,
		InputSHA256:    p.Inputs,
		OutputSHA256:   p.Outputs,
	}
	if err := store.Versions().SetProvenance(ctx, id.Ref(a.ID), version, prov); err != nil {
		return node.Result{}, err
	}
	return node.JSONResult(map[string]any{"asset_id": a.ID})
}

func nextVersion(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID string `json:"assetId"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	var out map[string]any
	err := env.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		a, err := liveAsset(ctx, tx, p.AssetID)
		if err != nil {
			return err
		}
		current, err := tx.Versions().Get(ctx, id.Ref(a.ID), a.CurrentVersion)
		if err != nil {
			return err
		}
		if !current.Immutable {
			out = map[string]any{"asset_id": a.ID, "version": a.CurrentVersion, "created": false}
			return nil
		}
		next, err := bumpVersion(a.CurrentVersion)
		if err != nil {
			return err
		}
		if err := tx.Versions().Create(ctx, domain.AssetVersion{AssetID: a.ID, Version: next}); err != nil {
			return err
		}
		a.CurrentVersion = next
		if err := tx.Assets().Update(ctx, a); err != nil {
			return err
		}
		out = map[string]any{"asset_id": a.ID, "version": next, "created": true}
		return nil
	})
	if err != nil {
		return node.Result{}, err
	}
	return node.JSONResult(out)
}

func bumpVersion(v string) (string, error) {
	var n int
	if _, err := fmt.Sscanf(v, "v%d", &n); err != nil || n < 1 {
		return "", errs.Newf(errs.CodeInternal, "unexpected version %q", v)
	}
	return fmt.Sprintf("v%d", n+1), nil
}

func markReady(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID string `json:"assetId"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	var friendly id.Friendly
	err := env.Store().InTx(ctx, func(tx repo.TenantRepos) error {
		a, err := liveAsset(ctx, tx, p.AssetID)
		if err != nil {
			return err
		}
		friendly = a.ID
		if !a.ReadinessSatisfied() {
			return precondition("asset %s is not ready for clipping: %s (R11)", a.ID, whyNotReady(a))
		}
		if a.Status == domain.AssetReadyForClipping {
			return nil
		}
		if !a.Status.CanTransition(domain.AssetReadyForClipping) {
			return precondition("asset %s cannot move from %s to ready_for_clipping (§2.5)", a.ID, a.Status)
		}
		a.Status = domain.AssetReadyForClipping
		if err := tx.Assets().Update(ctx, a); err != nil {
			return err
		}
		return env.Emit(ctx, tx, event.Draft{Type: event.AssetReadyForClipping, AssetID: string(a.ID)})
	})
	if err != nil {
		return node.Result{}, err
	}
	return node.JSONResult(map[string]any{"asset_id": friendly, "status": domain.AssetReadyForClipping})
}

// ---- clips ----

// SubstantialOverlap is the fraction of a segment that counts as overlapping an
// existing clip for the D47 warning.
const SubstantialOverlap = 0.5

func checkOverlap(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		SourceID string           `json:"sourceId"`
		Segments []domain.Segment `json:"segments"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	store := env.Store()
	clips, err := store.Assets().ClipsOverlapping(ctx, id.Ref(p.SourceID), p.Segments)
	if err != nil {
		return node.Result{}, err
	}
	var requested float64
	for _, s := range p.Segments {
		requested += s.EndSeconds - s.StartSeconds
	}
	overlaps := []map[string]any{}
	for _, c := range clips {
		if c.Lineage == nil {
			continue
		}
		var shared, existing float64
		for _, have := range c.Lineage.Segments {
			existing += have.EndSeconds - have.StartSeconds
			for _, want := range p.Segments {
				start, end := max(have.StartSeconds, want.StartSeconds), min(have.EndSeconds, want.EndSeconds)
				if end > start {
					shared += end - start
				}
			}
		}
		shorter := requested
		if existing > 0 && existing < shorter {
			shorter = existing
		}
		if shorter <= 0 || shared/shorter < SubstantialOverlap {
			continue
		}
		overlaps = append(overlaps, map[string]any{
			"clip_id": c.ID, "title": c.Title, "status": c.Status,
			"overlap_seconds": shared, "overlap_fraction": shared / shorter,
		})
	}
	return node.JSONResult(map[string]any{"warning": len(overlaps) > 0, "overlaps": overlaps})
}

// masterOfVersion finds the working master of one version of an asset.
func masterOfVersion(ctx context.Context, store repo.TenantStore, a domain.Asset, version string) (domain.MediaFile, bool) {
	v, err := store.Versions().Get(ctx, id.Ref(a.ID), version)
	if err != nil {
		return domain.MediaFile{}, false
	}
	for _, f := range v.Files {
		if f.Purpose == domain.FileWorkingMaster && f.Name == "" {
			return f.File, true
		}
	}
	return domain.MediaFile{}, false
}

// mapToParent converts one segment on a clip's timeline to its parent's. A clip's
// timeline is its intervals laid end to end, so this works only when the segment falls
// inside a single interval, which is what "the edit is continuous" means (D36).
func mapToParent(s domain.Segment, lineage []domain.Segment) (domain.Segment, bool) {
	offset := 0.0
	for _, in := range lineage {
		length := in.EndSeconds - in.StartSeconds
		if s.StartSeconds >= offset && s.EndSeconds <= offset+length+1e-9 {
			return domain.Segment{
				SourceAssetID: in.SourceAssetID,
				SourceVersion: in.SourceVersion,
				StartSeconds:  in.StartSeconds + (s.StartSeconds - offset),
				EndSeconds:    in.StartSeconds + (s.EndSeconds - offset),
			}, true
		}
		offset += length
	}
	return domain.Segment{}, false
}

func resolveRenderSource(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		SourceID string           `json:"sourceId"`
		Segments []domain.Segment `json:"segments"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	store := env.Store()
	source, err := store.Assets().Get(ctx, id.Ref(p.SourceID))
	if err != nil {
		return node.Result{}, err
	}
	if source.DeletedAt != nil {
		return node.Result{}, errs.NotFound("asset")
	}
	if !source.MasterAvailable || source.WorkingMaster == nil {
		return node.Result{}, precondition("asset %s has no available master; restoring it is a separate administrative flow (§3.2)", source.ID)
	}
	segments := make([]domain.Segment, len(p.Segments))
	for i, s := range p.Segments {
		s.SourceAssetID, s.SourceVersion = source.ID, source.CurrentVersion
		segments[i] = s
	}
	master := *source.WorkingMaster
	current := source
	chain := []id.Friendly{}
	root := source.ID
	if source.Lineage != nil && source.Lineage.RootAssetID != "" {
		root = source.Lineage.RootAssetID
	}
	// Walk towards the root while every segment stays inside one interval and the
	// parent still has its master, so a clip of a clip loses no generation (D36).
	for current.Lineage != nil && len(current.Lineage.Segments) > 0 {
		mapped := make([]domain.Segment, len(segments))
		ok := true
		for i, s := range segments {
			m, inside := mapToParent(s, current.Lineage.Segments)
			if !inside {
				ok = false
				break
			}
			mapped[i] = m
		}
		if !ok {
			break
		}
		parent, err := store.Assets().Get(ctx, id.Ref(current.Lineage.SourceAssetID))
		if err != nil || parent.DeletedAt != nil || !parent.MasterAvailable {
			break
		}
		file, found := masterOfVersion(ctx, store, parent, mapped[0].SourceVersion)
		if !found {
			break
		}
		segments, current, master = mapped, parent, file
		chain = append(chain, parent.ID)
	}
	return node.JSONResult(map[string]any{
		"source_uri":    master.URI(),
		"segments":      segments,
		"root_asset_id": root,
		"chain":         chain,
	})
}

func getTranscript(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID string `json:"assetId"`
		Version string `json:"version"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	store := env.Store()
	a, err := liveAsset(ctx, store, p.AssetID)
	if err != nil {
		return node.Result{}, err
	}
	version := p.Version
	if version == "" {
		version = a.CurrentVersion
	}
	t, err := store.Transcripts().Get(ctx, id.Ref(a.ID), version)
	if err != nil {
		return node.Result{}, err
	}
	if t.Segments == nil {
		t.Segments = []domain.TranscriptSegment{}
	}
	out := map[string]any{"asset_id": a.ID, "version": version, "transcript": t}
	if an, err := store.Transcripts().Analysis(ctx, id.Ref(a.ID), version); err == nil {
		out["analysis"] = an
	} else if !errs.IsCode(err, errs.CodeNotFound) {
		return node.Result{}, err
	}
	return node.JSONResult(out)
}
