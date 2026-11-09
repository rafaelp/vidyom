package persistence

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// assetRepo is the canonical catalog (D24). Files, transcript info, provenance and
// publications are derived on read and ignored on write (I105).
type assetRepo struct {
	conn
	tenant domain.TenantID
}

var (
	_ repo.AssetRepo           = assetRepo{}
	_ repo.AssetAIPolicyWriter = assetRepo{}
)

// assetKey identifies a row after resolving a reference.
type assetKey struct {
	ulid     string
	friendly id.Friendly
}

// resolveAsset finds an asset by friendly ID or ULID in this tenant. Soft-deleted
// assets still resolve: their IDs stay burned and workflows may need them (D17).
func resolveAsset(ctx context.Context, q querier, ref id.Ref) (assetKey, error) {
	column, value, ok := refColumn(ref, id.PrefixOriginal, id.PrefixClip, id.PrefixEdited)
	if !ok {
		return assetKey{}, errs.NotFound("asset")
	}
	var key assetKey
	err := q.QueryRowContext(ctx, `SELECT ulid, friendly_id FROM assets WHERE `+column+` = ?`, value).Scan(&key.ulid, &key.friendly)
	if err != nil {
		return assetKey{}, notFoundIfNone(err, "asset")
	}
	return key, nil
}

// optionalAssetULID resolves a possibly empty reference to a value for a nullable
// foreign key.
func optionalAssetULID(ctx context.Context, q querier, ref string) (any, error) {
	if ref == "" {
		return nil, nil
	}
	key, err := resolveAsset(ctx, q, id.Ref(ref))
	if err != nil {
		return nil, err
	}
	return key.ulid, nil
}

func validateAsset(a domain.Asset) error {
	switch a.Kind {
	case domain.KindOriginal, domain.KindClip, domain.KindEdited:
	default:
		return invalidArg("unknown asset kind %q", a.Kind)
	}
	valid := false
	for _, s := range domain.AssetStatuses {
		if s == a.Status {
			valid = true
			break
		}
	}
	if !valid {
		return invalidArg("unknown asset status %q", a.Status)
	}
	switch a.Visibility {
	case domain.VisibilityPrivate, domain.VisibilityUnlisted, domain.VisibilityPublic:
	default:
		return invalidArg("unknown visibility %q", a.Visibility)
	}
	switch a.CatalogSync {
	case domain.CatalogSyncNone, domain.CatalogSyncPending, domain.CatalogSyncSynced, domain.CatalogSyncFailed:
	default:
		return invalidArg("unknown catalog sync state %q", a.CatalogSync)
	}
	if a.Title == "" {
		return invalidArg("an asset needs a title")
	}
	return nil
}

func (r assetRepo) Create(ctx context.Context, a domain.Asset) error {
	if a.TenantID != "" && a.TenantID != r.tenant {
		return invalidArg("asset for tenant %q written to the database of %q", a.TenantID, r.tenant)
	}
	if a.ID == "" || a.ULID == "" {
		return invalidArg("an asset needs a friendly ID and a ULID")
	}
	if id.Friendly(a.ID).Prefix() != a.Kind.IDPrefix() {
		return invalidArg("friendly ID %s does not match kind %s", a.ID, a.Kind)
	}
	if a.Status == "" {
		a.Status = domain.AssetRegistered
	}
	if a.Visibility == "" {
		a.Visibility = domain.VisibilityPrivate
	}
	if a.CatalogSync == "" {
		a.CatalogSync = domain.CatalogSyncNone
	}
	if a.CurrentVersion == "" {
		a.CurrentVersion = "v1"
	}
	if err := validateAsset(a); err != nil {
		return err
	}
	now := r.now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = a.CreatedAt
	}
	return r.write(ctx, func(q querier) error {
		processing, consent, source, err := r.initialPolicy(ctx, q, a)
		if err != nil {
			return err
		}
		if a.Collection != "" {
			if err := ensureCollection(ctx, q, a.Collection); err != nil {
				return err
			}
		}
		parentRef := string(a.ParentAssetID)
		if parentRef == "" && a.Lineage != nil {
			parentRef = string(a.Lineage.SourceAssetID)
		}
		parent, err := optionalAssetULID(ctx, q, parentRef)
		if err != nil {
			return err
		}
		tags, err := jsonList(a.Tags)
		if err != nil {
			return err
		}
		consentJSON, err := jsonArg(consent)
		if err != nil {
			return err
		}
		lineage, err := jsonArg(a.Lineage)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO assets (
				ulid, friendly_id, kind, category, title, description, tags, collection, visibility,
				ai_processing, ai_consent, ai_policy_source, status, current_version,
				master_available, backup_verified, proxy_available, timeline_validated, catalog_sync,
				duration_seconds, language, parent_ulid, lineage, created_at, updated_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(a.ULID), string(a.ID), string(a.Kind), a.Category, a.Title, a.Description, tags, a.Collection, string(a.Visibility),
			string(processing), consentJSON, string(source), string(a.Status), a.CurrentVersion,
			boolInt(a.MasterAvailable), boolInt(a.BackupVerified), boolInt(a.ProxyAvailable), boolInt(a.TimelineValidated), string(a.CatalogSync),
			a.DurationSeconds, a.Language, parent, lineage, timeText(a.CreatedAt), timeText(a.UpdatedAt), timeArg(a.DeletedAt))
		if err != nil {
			return mapErr(err)
		}
		return replaceLineageSegments(ctx, q, string(a.ULID), a.Lineage)
	})
}

// initialPolicy applies I104 to a new asset.
func (r assetRepo) initialPolicy(ctx context.Context, q querier, a domain.Asset) (domain.AIProcessing, *domain.AIConsent, repo.AIPolicySource, error) {
	switch a.AIProcessing {
	case domain.AIExternalAllowed:
		if a.AIConsent == nil {
			return "", nil, "", errs.New(errs.CodeFailedPrecondition, "external AI processing needs a consent record (D8)")
		}
		return a.AIProcessing, a.AIConsent, repo.AIPolicyAsset, nil
	case domain.AILocalOnly:
		return a.AIProcessing, nil, repo.AIPolicyAsset, nil
	case "":
		if a.Collection != "" {
			c, found, err := getCollection(ctx, q, r.tenant, a.Collection)
			if err != nil {
				return "", nil, "", err
			}
			if found {
				return c.AIProcessing, c.AIConsent, repo.AIPolicyCollection, nil
			}
		}
		return domain.AILocalOnly, nil, repo.AIPolicyDefault, nil
	default:
		return "", nil, "", invalidArg("unknown AI processing policy %q", a.AIProcessing)
	}
}

func replaceLineageSegments(ctx context.Context, q querier, assetULID string, l *domain.Lineage) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM lineage_segments WHERE asset_ulid = ?`, assetULID); err != nil {
		return mapErr(err)
	}
	if l == nil {
		return nil
	}
	for _, s := range l.Segments {
		source := s.SourceAssetID
		if source == "" {
			source = l.SourceAssetID
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO lineage_segments (asset_ulid, source_asset_id, source_version, start_seconds, end_seconds) VALUES (?, ?, ?, ?, ?)`,
			assetULID, string(source), s.SourceVersion, s.StartSeconds, s.EndSeconds); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

const assetSelect = `SELECT a.rowid, a.friendly_id, a.ulid, a.kind, a.category, a.title, a.description, a.tags, a.collection, a.visibility,
	a.ai_processing, a.ai_consent, a.status, a.current_version,
	a.master_available, a.backup_verified, a.proxy_available, a.timeline_validated, a.catalog_sync,
	a.duration_seconds, a.language, COALESCE((SELECT p.friendly_id FROM assets p WHERE p.ulid = a.parent_ulid), ''), a.lineage,
	a.created_at, a.updated_at, a.deleted_at
	FROM assets a`

func (r assetRepo) scanRow(s scanner) (int64, domain.Asset, error) {
	var (
		a                               domain.Asset
		rowid                           int64
		master, backup, proxy, timeline int
	)
	err := s.Scan(&rowid, &a.ID, &a.ULID, &a.Kind, &a.Category, &a.Title, &a.Description, jsonCol{&a.Tags}, &a.Collection, &a.Visibility,
		&a.AIProcessing, jsonCol{&a.AIConsent}, &a.Status, &a.CurrentVersion,
		&master, &backup, &proxy, &timeline, &a.CatalogSync,
		&a.DurationSeconds, &a.Language, &a.ParentAssetID, jsonCol{&a.Lineage},
		reqTime{&a.CreatedAt}, reqTime{&a.UpdatedAt}, nullTime{&a.DeletedAt})
	a.TenantID = r.tenant
	a.MasterAvailable = master != 0
	a.BackupVerified = backup != 0
	a.ProxyAvailable = proxy != 0
	a.TimelineValidated = timeline != 0
	if len(a.Tags) == 0 {
		a.Tags = nil
	}
	return rowid, a, err
}

// hydrate fills what is derived on read for the asset's current version (I105).
func (r assetRepo) hydrate(ctx context.Context, q querier, a *domain.Asset) error {
	ulid := string(a.ULID)
	var provenance *domain.Provenance
	err := q.QueryRowContext(ctx, `SELECT provenance FROM asset_versions WHERE asset_ulid = ? AND version = ?`, ulid, a.CurrentVersion).Scan(jsonCol{&provenance})
	if err != nil && !isNoRows(err) {
		return mapErr(err)
	}
	a.Provenance = provenance

	files, err := versionFiles(ctx, q, ulid, a.CurrentVersion)
	if err != nil {
		return err
	}
	hasCaptions := false
	for i := range files {
		f := &files[i]
		if f.Purpose == domain.FileCaptions {
			hasCaptions = true
		}
		if f.Name != "" {
			continue
		}
		switch f.Purpose {
		case domain.FileSourceOriginal:
			a.SourceOriginal = &f.File
		case domain.FileWorkingMaster:
			a.WorkingMaster = &f.File
		case domain.FileProxy:
			a.Proxy = &f.File
		case domain.FileBackup:
			a.Backup = &f.File
		case domain.FileThumbnail:
			a.Thumbnail = &f.File
		}
	}

	var engine, basis string
	err = q.QueryRowContext(ctx, `SELECT engine, timestamp_basis FROM transcripts WHERE asset_ulid = ? AND version = ?`, ulid, a.CurrentVersion).Scan(&engine, &basis)
	switch {
	case err == nil:
		info := &domain.TranscriptInfo{Format: "json", TimestampBasis: basis, Engine: engine}
		if hasCaptions {
			info.Format = "json+srt"
		}
		lexical, err := hasLexical(ctx, q, ulid, a.CurrentVersion)
		if err != nil {
			return err
		}
		model, err := vectorModelFor(ctx, q, ulid, a.CurrentVersion)
		if err != nil {
			return err
		}
		info.EmbeddingModel = model
		info.Indexed = lexical && model != ""
		a.Transcript = info
	case isNoRows(err):
		a.Transcript = nil
	default:
		return mapErr(err)
	}

	pubs, err := queryAll(ctx, q, publicationSelect+` WHERE p.asset_ulid = ? ORDER BY p.rowid`, []any{ulid}, scanPublicationFor(r.tenant))
	if err != nil {
		return err
	}
	a.Publications = pubs
	return nil
}

func (r assetRepo) Get(ctx context.Context, ref id.Ref) (domain.Asset, error) {
	var a domain.Asset
	err := r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, ref)
		if err != nil {
			return err
		}
		_, a, err = r.scanRow(q.QueryRowContext(ctx, assetSelect+` WHERE a.ulid = ?`, key.ulid))
		if err != nil {
			return notFoundIfNone(err, "asset")
		}
		return r.hydrate(ctx, q, &a)
	})
	if err != nil {
		return domain.Asset{}, err
	}
	return a, nil
}

func (r assetRepo) List(ctx context.Context, f repo.AssetFilter, p repo.Page) ([]domain.Asset, string, error) {
	var (
		items []domain.Asset
		next  string
	)
	err := r.read(ctx, func(q querier) error {
		var w where
		if f.Kind != "" {
			w.add("a.kind = ?", string(f.Kind))
		}
		if f.Status != "" {
			w.add("a.status = ?", string(f.Status))
		}
		if f.Collection != "" {
			w.add("a.collection = ?", f.Collection)
		}
		if !f.IncludeDeleted {
			w.add("a.deleted_at IS NULL")
		}
		if f.SourceID != "" {
			key, err := resolveAsset(ctx, q, f.SourceID)
			if err != nil {
				return err
			}
			w.add("a.parent_ulid = ?", key.ulid)
		}
		var err error
		items, next, err = listDesc(ctx, q, assetSelect, "a.rowid", w, p, r.scanRow)
		if err != nil {
			return err
		}
		for i := range items {
			if err := r.hydrate(ctx, q, &items[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return items, next, nil
}

// Update writes the mutable fields. It never writes the AI policy (I104) or anything
// derived on read (I105), and rejects an invalid status transition.
func (r assetRepo) Update(ctx context.Context, a domain.Asset) error {
	if a.TenantID != "" && a.TenantID != r.tenant {
		return invalidArg("asset for tenant %q written to the database of %q", a.TenantID, r.tenant)
	}
	if err := validateAsset(a); err != nil {
		return err
	}
	return r.write(ctx, func(q querier) error {
		ref := id.Ref(a.ULID)
		if ref == "" {
			ref = id.Ref(a.ID)
		}
		key, err := resolveAsset(ctx, q, ref)
		if err != nil {
			return err
		}
		var status domain.AssetStatus
		var collection string
		var policySource repo.AIPolicySource
		if err := q.QueryRowContext(ctx, `SELECT status, collection, ai_policy_source FROM assets WHERE ulid = ?`, key.ulid).Scan(&status, &collection, &policySource); err != nil {
			return mapErr(err)
		}
		if !status.CanTransition(a.Status) {
			return errs.Newf(errs.CodeFailedPrecondition, "asset %s cannot move from %s to %s", key.friendly, status, a.Status)
		}
		parentRef := string(a.ParentAssetID)
		if parentRef == "" && a.Lineage != nil {
			parentRef = string(a.Lineage.SourceAssetID)
		}
		parent, err := optionalAssetULID(ctx, q, parentRef)
		if err != nil {
			return err
		}
		tags, err := jsonList(a.Tags)
		if err != nil {
			return err
		}
		lineage, err := jsonArg(a.Lineage)
		if err != nil {
			return err
		}
		if a.CurrentVersion == "" {
			return invalidArg("an asset needs a current version")
		}
		_, err = q.ExecContext(ctx, `UPDATE assets SET category = ?, title = ?, description = ?, tags = ?, collection = ?, visibility = ?,
				status = ?, current_version = ?, master_available = ?, backup_verified = ?, proxy_available = ?, timeline_validated = ?,
				catalog_sync = ?, duration_seconds = ?, language = ?, parent_ulid = ?, lineage = ?, updated_at = ?
			WHERE ulid = ?`,
			a.Category, a.Title, a.Description, tags, a.Collection, string(a.Visibility),
			string(a.Status), a.CurrentVersion, boolInt(a.MasterAvailable), boolInt(a.BackupVerified), boolInt(a.ProxyAvailable), boolInt(a.TimelineValidated),
			string(a.CatalogSync), a.DurationSeconds, a.Language, parent, lineage, timeText(r.now()), key.ulid)
		if err != nil {
			return mapErr(err)
		}
		if err := replaceLineageSegments(ctx, q, key.ulid, a.Lineage); err != nil {
			return err
		}
		if a.Collection == collection {
			return nil
		}
		// A move between collections re-evaluates a policy that was not decided on the
		// asset itself (I104).
		if a.Collection != "" {
			if err := ensureCollection(ctx, q, a.Collection); err != nil {
				return err
			}
		}
		if policySource == repo.AIPolicyAsset {
			return nil
		}
		processing, consent, source := domain.AILocalOnly, (*domain.AIConsent)(nil), repo.AIPolicyDefault
		if a.Collection != "" {
			c, found, err := getCollection(ctx, q, r.tenant, a.Collection)
			if err != nil {
				return err
			}
			if found {
				processing, consent, source = c.AIProcessing, c.AIConsent, repo.AIPolicyCollection
			}
		}
		return writeAIPolicy(ctx, q, key.ulid, processing, consent, source, r.now())
	})
}

func writeAIPolicy(ctx context.Context, q querier, assetULID string, processing domain.AIProcessing, consent *domain.AIConsent, source repo.AIPolicySource, at time.Time) error {
	consentJSON, err := jsonArg(consent)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE assets SET ai_processing = ?, ai_consent = ?, ai_policy_source = ?, updated_at = ? WHERE ulid = ?`,
		string(processing), consentJSON, string(source), timeText(at), assetULID)
	return mapErr(err)
}

func validatePolicy(processing domain.AIProcessing, consent *domain.AIConsent) error {
	switch processing {
	case domain.AILocalOnly:
		return nil
	case domain.AIExternalAllowed:
		if consent == nil {
			return errs.New(errs.CodeFailedPrecondition, "external AI processing needs a consent record (D8)")
		}
		return nil
	default:
		return invalidArg("unknown AI processing policy %q", processing)
	}
}

// SetAIPolicy changes an asset's effective policy (I104).
func (r assetRepo) SetAIPolicy(ctx context.Context, ref id.Ref, processing domain.AIProcessing, consent *domain.AIConsent, source repo.AIPolicySource) error {
	if err := validatePolicy(processing, consent); err != nil {
		return err
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, ref)
		if err != nil {
			return err
		}
		return writeAIPolicy(ctx, q, key.ulid, processing, consent, source, r.now())
	})
}

// AIPolicySource reports who decided an asset's policy.
func (r assetRepo) AIPolicySource(ctx context.Context, ref id.Ref) (repo.AIPolicySource, error) {
	var source repo.AIPolicySource
	err := r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, ref)
		if err != nil {
			return err
		}
		return mapErr(q.QueryRowContext(ctx, `SELECT ai_policy_source FROM assets WHERE ulid = ?`, key.ulid).Scan(&source))
	})
	return source, err
}

// ApplyCollectionPolicy rewrites members not sourced from an asset-level decision.
func (r assetRepo) ApplyCollectionPolicy(ctx context.Context, collection string, processing domain.AIProcessing, consent *domain.AIConsent) (int64, error) {
	if err := validatePolicy(processing, consent); err != nil {
		return 0, err
	}
	var changed int64
	err := r.write(ctx, func(q querier) error {
		consentJSON, err := jsonArg(consent)
		if err != nil {
			return err
		}
		res, err := q.ExecContext(ctx, `UPDATE assets SET ai_processing = ?, ai_consent = ?, ai_policy_source = ?, updated_at = ?
			WHERE collection = ? AND ai_policy_source != ?`,
			string(processing), consentJSON, string(repo.AIPolicyCollection), timeText(r.now()), collection, string(repo.AIPolicyAsset))
		if err != nil {
			return mapErr(err)
		}
		changed, err = res.RowsAffected()
		return mapErr(err)
	})
	return changed, err
}

func (r assetRepo) AppendHistory(ctx context.Context, h domain.HistoryEntry) error {
	if h.At.IsZero() {
		h.At = r.now()
	}
	if h.Action == "" {
		return invalidArg("a history entry needs an action")
	}
	if len(h.Changes) > 0 && !json.Valid(h.Changes) {
		return invalidArg("history changes must be JSON")
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, id.Ref(h.AssetID))
		if err != nil {
			return err
		}
		changes, err := jsonArg(h.Changes)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO asset_history (asset_ulid, at, actor, action, changes) VALUES (?, ?, ?, ?, ?)`,
			key.ulid, timeText(h.At), h.Actor, h.Action, changes)
		return mapErr(err)
	})
}

func (r assetRepo) History(ctx context.Context, ref id.Ref, p repo.Page) ([]domain.HistoryEntry, string, error) {
	var (
		items []domain.HistoryEntry
		next  string
	)
	err := r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, ref)
		if err != nil {
			return err
		}
		var w where
		w.add("asset_ulid = ?", key.ulid)
		items, next, err = listDesc(ctx, q, `SELECT id, at, actor, action, changes FROM asset_history`, "id", w, p,
			func(s scanner) (int64, domain.HistoryEntry, error) {
				var (
					k int64
					h domain.HistoryEntry
				)
				err := s.Scan(&k, reqTime{&h.At}, &h.Actor, &h.Action, rawCol{&h.Changes})
				h.AssetID = key.friendly
				return k, h, err
			})
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return items, next, nil
}

// SoftDelete sets deleted_at once; a repeated call keeps the first time (D17).
func (r assetRepo) SoftDelete(ctx context.Context, ref id.Ref, at time.Time) error {
	if at.IsZero() {
		at = r.now()
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, ref)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `UPDATE assets SET deleted_at = COALESCE(deleted_at, ?), updated_at = ? WHERE ulid = ?`,
			timeText(at), timeText(r.now()), key.ulid)
		return mapErr(err)
	})
}

// ClipsOverlapping lists derivatives of a source whose lineage overlaps the segments
// (D47). It works on the source's timeline: segments that do not name a source count
// as this one's.
func (r assetRepo) ClipsOverlapping(ctx context.Context, source id.Ref, segments []domain.Segment) ([]domain.Asset, error) {
	var out []domain.Asset
	err := r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, source)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, s := range segments {
			ulids, err := queryAll(ctx, q, `SELECT DISTINCT l.asset_ulid FROM lineage_segments l JOIN assets a ON a.ulid = l.asset_ulid
				WHERE l.source_asset_id = ? AND l.start_seconds < ? AND l.end_seconds > ? AND a.deleted_at IS NULL`,
				[]any{string(key.friendly), s.EndSeconds, s.StartSeconds},
				func(sc scanner) (string, error) {
					var v string
					err := sc.Scan(&v)
					return v, err
				})
			if err != nil {
				return err
			}
			for _, u := range ulids {
				if seen[u] {
					continue
				}
				seen[u] = true
				_, a, err := r.scanRow(q.QueryRowContext(ctx, assetSelect+` WHERE a.ulid = ?`, u))
				if err != nil {
					return mapErr(err)
				}
				if err := r.hydrate(ctx, q, &a); err != nil {
					return err
				}
				out = append(out, a)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- Versions ----

type versionRepo struct {
	conn
	tenant domain.TenantID
}

var _ repo.VersionRepo = versionRepo{}

func versionFiles(ctx context.Context, q querier, assetULID, version string) ([]domain.VersionFile, error) {
	return queryAll(ctx, q, `SELECT purpose, name, file FROM version_files WHERE asset_ulid = ? AND version = ? ORDER BY purpose, name`,
		[]any{assetULID, version},
		func(s scanner) (domain.VersionFile, error) {
			var f domain.VersionFile
			err := s.Scan(&f.Purpose, &f.Name, jsonCol{&f.File})
			return f, err
		})
}

func (r versionRepo) Create(ctx context.Context, v domain.AssetVersion) error {
	if v.Version == "" {
		return invalidArg("a version needs a name")
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = r.now()
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, id.Ref(v.AssetID))
		if err != nil {
			return err
		}
		provenance, err := jsonArg(v.Provenance)
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO asset_versions (asset_ulid, version, immutable, provenance, created_at) VALUES (?, ?, ?, ?, ?)`,
			key.ulid, v.Version, boolInt(v.Immutable), provenance, timeText(v.CreatedAt)); err != nil {
			return mapErr(err)
		}
		for _, f := range v.Files {
			if err := putFile(ctx, q, key.ulid, v.Version, f); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r versionRepo) Get(ctx context.Context, asset id.Ref, version string) (domain.AssetVersion, error) {
	var out domain.AssetVersion
	err := r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		out, err = readVersion(ctx, q, key, version)
		return err
	})
	if err != nil {
		return domain.AssetVersion{}, err
	}
	return out, nil
}

func readVersion(ctx context.Context, q querier, key assetKey, version string) (domain.AssetVersion, error) {
	v := domain.AssetVersion{AssetID: key.friendly, Version: version}
	var immutable int
	err := q.QueryRowContext(ctx, `SELECT immutable, provenance, created_at FROM asset_versions WHERE asset_ulid = ? AND version = ?`, key.ulid, version).
		Scan(&immutable, jsonCol{&v.Provenance}, reqTime{&v.CreatedAt})
	if err != nil {
		return domain.AssetVersion{}, notFoundIfNone(err, "asset version")
	}
	v.Immutable = immutable != 0
	v.Files, err = versionFiles(ctx, q, key.ulid, version)
	if err != nil {
		return domain.AssetVersion{}, err
	}
	return v, nil
}

func (r versionRepo) List(ctx context.Context, asset id.Ref) ([]domain.AssetVersion, error) {
	var out []domain.AssetVersion
	err := r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		names, err := queryAll(ctx, q, `SELECT version FROM asset_versions WHERE asset_ulid = ? ORDER BY rowid`, []any{key.ulid},
			func(s scanner) (string, error) {
				var v string
				err := s.Scan(&v)
				return v, err
			})
		if err != nil {
			return err
		}
		for _, name := range names {
			v, err := readVersion(ctx, q, key, name)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func putFile(ctx context.Context, q querier, assetULID, version string, f domain.VersionFile) error {
	if f.Purpose == "" {
		return invalidArg("a version file needs a purpose")
	}
	file, err := json.Marshal(f.File)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO version_files (asset_ulid, version, purpose, name, file) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (asset_ulid, version, purpose, name) DO UPDATE SET file = excluded.file`,
		assetULID, version, string(f.Purpose), f.Name, string(file))
	return mapErr(err)
}

// PutFile records a file, replacing the same purpose and name. An immutable version
// refuses it: after publication, a new render needs a new version (D35).
func (r versionRepo) PutFile(ctx context.Context, asset id.Ref, version string, f domain.VersionFile) error {
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		var immutable int
		err = q.QueryRowContext(ctx, `SELECT immutable FROM asset_versions WHERE asset_ulid = ? AND version = ?`, key.ulid, version).Scan(&immutable)
		if err != nil {
			return notFoundIfNone(err, "asset version")
		}
		if immutable != 0 {
			return errs.Newf(errs.CodeFailedPrecondition, "version %s of %s was published and is immutable: create a new version (D35)", version, key.friendly)
		}
		return putFile(ctx, q, key.ulid, version, f)
	})
}

func (r versionRepo) MarkImmutable(ctx context.Context, asset id.Ref, version string) error {
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		res, err := q.ExecContext(ctx, `UPDATE asset_versions SET immutable = 1 WHERE asset_ulid = ? AND version = ?`, key.ulid, version)
		return requireAffected(res, err, "asset version")
	})
}

func (r versionRepo) SetProvenance(ctx context.Context, asset id.Ref, version string, p domain.Provenance) error {
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		provenance, err := jsonArg(p)
		if err != nil {
			return err
		}
		res, err := q.ExecContext(ctx, `UPDATE asset_versions SET provenance = ? WHERE asset_ulid = ? AND version = ?`, provenance, key.ulid, version)
		return requireAffected(res, err, "asset version")
	})
}

// ---- Collections ----

type collectionRepo struct {
	conn
	tenant domain.TenantID
}

var _ repo.CollectionRepo = collectionRepo{}

// ensureCollection records a collection an asset names, local_only until an admin
// says otherwise (D8).
func ensureCollection(ctx context.Context, q querier, name string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO collections (name, ai_processing) VALUES (?, 'local_only') ON CONFLICT (name) DO NOTHING`, name)
	return mapErr(err)
}

func scanCollection(tenant domain.TenantID) func(scanner) (domain.Collection, error) {
	return func(s scanner) (domain.Collection, error) {
		c := domain.Collection{TenantID: tenant}
		err := s.Scan(&c.Name, &c.AIProcessing, jsonCol{&c.AIConsent})
		return c, err
	}
}

func getCollection(ctx context.Context, q querier, tenant domain.TenantID, name string) (domain.Collection, bool, error) {
	c, err := scanCollection(tenant)(q.QueryRowContext(ctx, `SELECT name, ai_processing, ai_consent FROM collections WHERE name = ?`, name))
	if err != nil {
		if isNoRows(err) {
			return domain.Collection{}, false, nil
		}
		return domain.Collection{}, false, mapErr(err)
	}
	return c, true, nil
}

func (r collectionRepo) Get(ctx context.Context, name string) (domain.Collection, error) {
	c, found, err := getCollection(ctx, r.q(), r.tenant, name)
	if err != nil {
		return domain.Collection{}, err
	}
	if !found {
		return domain.Collection{}, errs.NotFound("collection")
	}
	return c, nil
}

func (r collectionRepo) List(ctx context.Context) ([]domain.Collection, error) {
	return queryAll(ctx, r.q(), `SELECT name, ai_processing, ai_consent FROM collections ORDER BY name`, nil, scanCollection(r.tenant))
}

func (r collectionRepo) Upsert(ctx context.Context, c domain.Collection) error {
	if c.Name == "" {
		return invalidArg("a collection needs a name")
	}
	if c.AIProcessing == "" {
		c.AIProcessing = domain.AILocalOnly
	}
	if err := validatePolicy(c.AIProcessing, c.AIConsent); err != nil {
		return err
	}
	consent, err := jsonArg(c.AIConsent)
	if err != nil {
		return err
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO collections (name, ai_processing, ai_consent) VALUES (?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET ai_processing = excluded.ai_processing, ai_consent = excluded.ai_consent`,
			c.Name, string(c.AIProcessing), consent)
		return mapErr(err)
	})
}

// ---- Transcripts ----

type transcriptRepo struct{ conn }

var _ repo.TranscriptRepo = transcriptRepo{}

func (r transcriptRepo) Save(ctx context.Context, t domain.Transcript) error {
	if t.Version == "" {
		return invalidArg("a transcript needs a version")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = r.now()
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, id.Ref(t.AssetID))
		if err != nil {
			return err
		}
		t.AssetID = key.friendly
		doc, err := json.Marshal(t)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO transcripts (asset_ulid, version, engine, timestamp_basis, document, created_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (asset_ulid, version) DO UPDATE SET engine = excluded.engine, timestamp_basis = excluded.timestamp_basis,
				document = excluded.document, created_at = excluded.created_at`,
			key.ulid, t.Version, t.Engine, t.TimestampBasis, string(doc), timeText(t.CreatedAt))
		return mapErr(err)
	})
}

func (r transcriptRepo) Get(ctx context.Context, asset id.Ref, version string) (domain.Transcript, error) {
	var t domain.Transcript
	err := r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		err = q.QueryRowContext(ctx, `SELECT document FROM transcripts WHERE asset_ulid = ? AND version = ?`, key.ulid, version).Scan(jsonCol{&t})
		return notFoundIfNone(err, "transcript")
	})
	if err != nil {
		return domain.Transcript{}, err
	}
	return t, nil
}

func (r transcriptRepo) SaveAnalysis(ctx context.Context, a domain.Analysis) error {
	if a.Version == "" {
		return invalidArg("an analysis needs a version")
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, id.Ref(a.AssetID))
		if err != nil {
			return err
		}
		a.AssetID = key.friendly
		doc, err := json.Marshal(a)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO analyses (asset_ulid, version, document) VALUES (?, ?, ?)
			ON CONFLICT (asset_ulid, version) DO UPDATE SET document = excluded.document`, key.ulid, a.Version, string(doc))
		return mapErr(err)
	})
}

func (r transcriptRepo) Analysis(ctx context.Context, asset id.Ref, version string) (domain.Analysis, error) {
	var a domain.Analysis
	err := r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		err = q.QueryRowContext(ctx, `SELECT document FROM analyses WHERE asset_ulid = ? AND version = ?`, key.ulid, version).Scan(jsonCol{&a})
		return notFoundIfNone(err, "analysis")
	})
	if err != nil {
		return domain.Analysis{}, err
	}
	return a, nil
}

func (r transcriptRepo) MarkSensitive(ctx context.Context, s domain.SensitiveSegment) error {
	if s.Version == "" {
		return invalidArg("a sensitive segment needs a version")
	}
	if s.StartSeconds < 0 || s.EndSeconds <= s.StartSeconds {
		return invalidArg("a sensitive segment needs 0 <= start < end")
	}
	if s.MarkedAt.IsZero() {
		s.MarkedAt = r.now()
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, id.Ref(s.AssetID))
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO sensitive_segments (asset_ulid, version, start_seconds, end_seconds, reason, marked_by, marked_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (asset_ulid, version, start_seconds, end_seconds) DO UPDATE SET reason = excluded.reason, marked_by = excluded.marked_by, marked_at = excluded.marked_at`,
			key.ulid, s.Version, s.StartSeconds, s.EndSeconds, s.Reason, s.MarkedBy, timeText(s.MarkedAt))
		return mapErr(err)
	})
}

func (r transcriptRepo) UnmarkSensitive(ctx context.Context, asset id.Ref, version string, start, end float64) error {
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		res, err := q.ExecContext(ctx, `DELETE FROM sensitive_segments WHERE asset_ulid = ? AND version = ? AND start_seconds = ? AND end_seconds = ?`,
			key.ulid, version, start, end)
		return requireAffected(res, err, "sensitive segment")
	})
}

func (r transcriptRepo) Sensitive(ctx context.Context, asset id.Ref, version string) ([]domain.SensitiveSegment, error) {
	var out []domain.SensitiveSegment
	err := r.read(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, asset)
		if err != nil {
			return err
		}
		out, err = queryAll(ctx, q, `SELECT version, start_seconds, end_seconds, reason, marked_by, marked_at FROM sensitive_segments
			WHERE asset_ulid = ? AND version = ? ORDER BY start_seconds, end_seconds`, []any{key.ulid, version},
			func(s scanner) (domain.SensitiveSegment, error) {
				v := domain.SensitiveSegment{AssetID: key.friendly}
				err := s.Scan(&v.Version, &v.StartSeconds, &v.EndSeconds, &v.Reason, &v.MarkedBy, reqTime{&v.MarkedAt})
				return v, err
			})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
