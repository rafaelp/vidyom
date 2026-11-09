package catalognodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// ---- catalog.list_expired_renders ----

func retentionDays(env node.DaemonEnv, given int) int {
	if given > 0 {
		return given
	}
	if h, ok := env.(HostEnv); ok {
		if d := h.DaemonConfig().Retention.UnapprovedRenderDays; d > 0 {
			return d
		}
	}
	return config.DefaultUnapprovedRenderDays
}

func listAll(ctx context.Context, store repo.TenantStore, f repo.AssetFilter, visit func(domain.Asset) bool) error {
	cursor := ""
	for {
		page, next, err := store.Assets().List(ctx, f, repo.Page{Limit: 200, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, a := range page {
			if !visit(a) {
				return nil
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

func listExpiredRenders(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		OlderThanDays int `json:"olderThanDays"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays(env, p.OlderThanDays))
	items := []map[string]any{}
	// A render that was never approved is one still in review, or one a person rejected
	// (D16). Originals are permanent, and approved ones are not expired.
	for _, kind := range []domain.AssetKind{domain.KindClip, domain.KindEdited} {
		for _, status := range []domain.AssetStatus{domain.AssetRejected, domain.AssetEditorialReview} {
			err := listAll(ctx, env.Store(), repo.AssetFilter{Kind: kind, Status: status}, func(a domain.Asset) bool {
				if a.CreatedAt.After(cutoff) || a.WorkingMaster == nil || !a.MasterAvailable {
					return true
				}
				items = append(items, map[string]any{
					"asset_id": a.ID, "version": a.CurrentVersion, "master_uri": a.WorkingMaster.URI(),
					"status": a.Status, "created_at": a.CreatedAt.Format(time.RFC3339),
				})
				return true
			})
			if err != nil {
				return node.Result{}, err
			}
		}
	}
	return node.JSONResult(map[string]any{"items": items})
}

// ---- catalog.release_input ----

func releaseInput(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		Source string `json:"source"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	u, err := domain.ParseInputURI(p.Source)
	if err != nil {
		return node.Result{}, invalid("%v", err)
	}
	// Only a spool copy is ours to delete: a server: path is someone's file (R9).
	if u.Kind != domain.URIUpload {
		return node.JSONResult(map[string]bool{"released": false})
	}
	h, ok := env.(HostEnv)
	if !ok {
		return node.JSONResult(map[string]bool{"released": false})
	}
	upload, err := env.Store().Uploads().Get(ctx, id.Ref(u.UploadRef))
	if errs.IsCode(err, errs.CodeNotFound) {
		return node.JSONResult(map[string]bool{"released": true})
	}
	if err != nil {
		return node.Result{}, err
	}
	dir := h.DaemonConfig().SpoolDir
	if dir == "" {
		return node.JSONResult(map[string]bool{"released": false})
	}
	path := filepath.Join(dir, string(env.Tenant()), string(upload.ULID))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return node.Result{}, errs.Wrap(errs.CodeUnavailable, err, "cannot remove the upload's spool copy")
	}
	if err := env.Store().Uploads().Delete(ctx, id.Ref(upload.ULID)); err != nil && !errs.IsCode(err, errs.CodeNotFound) {
		return node.Result{}, err
	}
	return node.JSONResult(map[string]bool{"released": true})
}

// ---- catalog.delete ----

// archiveMinDays is the archive's minimum storage duration (D17, §7.2): the instance's
// parameter when it declares one, 365 days for an ARCHIVE class, otherwise none.
func archiveMinDays(inst config.StorageInstance) int {
	var p struct {
		MinStorageDays *int `json:"minStorageDays"`
	}
	if len(inst.Parameters) > 0 && json.Unmarshal(inst.Parameters, &p) == nil && p.MinStorageDays != nil {
		return *p.MinStorageDays
	}
	if strings.EqualFold(inst.StorageClass, "ARCHIVE") {
		return 365
	}
	return 0
}

// archiveFiles lists an asset's archive objects across versions.
func archiveFiles(ctx context.Context, store repo.TenantStore, asset id.Friendly) ([]domain.MediaFile, error) {
	versions, err := store.Versions().List(ctx, id.Ref(asset))
	if err != nil {
		return nil, err
	}
	var out []domain.MediaFile
	for _, v := range versions {
		for _, f := range v.Files {
			if f.File.Storage == domain.RoleArchive {
				out = append(out, f.File)
			}
		}
	}
	return out, nil
}

func deleteAsset(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID string `json:"assetId"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	if !env.HasPermission(domain.PermAssetDelete) {
		return node.Result{}, errs.Forbidden(string(domain.PermAssetDelete))
	}
	store := env.Store()
	a, err := store.Assets().Get(ctx, id.Ref(p.AssetID))
	if err != nil {
		return node.Result{}, err
	}
	pubs, _, err := store.Publications().List(ctx, repo.PublicationFilter{AssetID: id.Ref(a.ID)}, repo.Page{Limit: 1000})
	if err != nil {
		return node.Result{}, err
	}
	for _, pub := range pubs {
		switch pub.Status {
		case domain.PubWithdrawn, domain.PubFailed, domain.PubRejectedByDestination:
		default:
			return node.Result{}, precondition("publication %s of %s is %s; withdraw it first (R16)", pub.ID, a.ID, pub.Status)
		}
	}
	// Operational copies go now. The archive copy stays, inaccessible, until its
	// minimum storage duration ends (D17). Missing is success, so a re-run is safe.
	versions, err := store.Versions().List(ctx, id.Ref(a.ID))
	if err != nil {
		return node.Result{}, err
	}
	for _, v := range versions {
		for _, f := range v.Files {
			if f.File.Storage == domain.RoleArchive {
				continue
			}
			st, err := env.Storage(ctx, f.File.Storage)
			if err != nil {
				return node.Result{}, err
			}
			if err := st.Delete(ctx, f.File.Key); err != nil && !errs.IsCode(err, errs.CodeNotFound) {
				return node.Result{}, err
			}
		}
	}
	if err := store.Search().DeleteAsset(ctx, id.Ref(a.ID)); err != nil && !errs.IsCode(err, errs.CodeNotFound) {
		return node.Result{}, err
	}

	cfg, err := env.TenantConfig(ctx)
	if err != nil {
		return node.Result{}, err
	}
	archive := cfg.Storages.Roles[domain.RoleArchive]
	minDays := archiveMinDays(archive)
	files, err := archiveFiles(ctx, store, a.ID)
	if err != nil {
		return node.Result{}, err
	}
	now := time.Now().UTC()
	purgeAfter := now
	var estimate float64
	unit := cfg.Budget.PerUnitMinor[EarlyDeletionOperation]
	for _, f := range files {
		ends := f.RecordedAt.AddDate(0, 0, minDays)
		if ends.After(purgeAfter) {
			purgeAfter = ends
		}
		if remaining := ends.Sub(now).Hours() / 24; remaining > 0 && unit > 0 {
			estimate += float64(f.SizeBytes) / (1 << 30) * remaining * unit
		}
	}
	who := actor(ctx, env)
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		current, err := tx.Assets().Get(ctx, id.Ref(a.ID))
		if err != nil {
			return err
		}
		if current.DeletedAt != nil {
			return nil
		}
		if err := tx.Assets().SoftDelete(ctx, id.Ref(a.ID), now); err != nil {
			return err
		}
		if err := appendHistory(ctx, tx, a.ID, who, "deleted", map[string]any{"archive_purge_after": purgeAfter.Format(time.RFC3339)}); err != nil {
			return err
		}
		details, err := json.Marshal(map[string]any{"kind": a.Kind, "archive_purge_after": purgeAfter.Format(time.RFC3339)})
		if err != nil {
			return err
		}
		if _, err := tx.Audit().Append(ctx, domain.AuditRecord{
			ULID: id.New(), TenantID: env.Tenant(), At: now, Actor: who, Method: domain.AuthSystem,
			Action: "asset.deleted", Permission: domain.PermAssetDelete, Resource: string(a.ID), Result: "allowed", Details: details,
		}); err != nil {
			return err
		}
		return env.Emit(ctx, tx, event.Draft{Type: event.AssetDeleted, AssetID: string(a.ID), Payload: map[string]any{"kind": a.Kind}})
	})
	if err != nil {
		return node.Result{}, err
	}
	return node.JSONResult(map[string]any{
		"asset_id":                      a.ID,
		"archive_purge_after":           purgeAfter.Format(time.RFC3339),
		"early_deletion_estimate_minor": int64(estimate + 0.5),
	})
}

// EarlyDeletionOperation is the cost table entry for deleting an archive object before
// its minimum duration, per GiB and day (D17, D19). Absent means no estimate.
const EarlyDeletionOperation = "storage.archive.early_deletion"

func listPurgeableArchives(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		Limit int `json:"limit"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	if p.Limit <= 0 || p.Limit > 1000 {
		p.Limit = 200
	}
	cfg, err := env.TenantConfig(ctx)
	if err != nil {
		return node.Result{}, err
	}
	minDays := archiveMinDays(cfg.Storages.Roles[domain.RoleArchive])
	st, err := env.Storage(ctx, domain.RoleArchive)
	if err != nil {
		return node.Result{}, err
	}
	store := env.Store()
	now := time.Now().UTC()
	items := []map[string]any{}
	var walkErr error
	err = listAll(ctx, store, repo.AssetFilter{IncludeDeleted: true}, func(a domain.Asset) bool {
		if a.DeletedAt == nil {
			return true
		}
		files, err := archiveFiles(ctx, store, a.ID)
		if err != nil {
			walkErr = err
			return false
		}
		for _, f := range files {
			if f.RecordedAt.AddDate(0, 0, minDays).After(now) {
				continue
			}
			exists, err := st.Exists(ctx, f.Key)
			if err != nil {
				walkErr = err
				return false
			}
			if !exists {
				continue
			}
			items = append(items, map[string]any{"asset_id": a.ID, "archive_uri": f.URI()})
			if len(items) >= p.Limit {
				return false
			}
		}
		return true
	})
	if err != nil {
		return node.Result{}, err
	}
	if walkErr != nil {
		return node.Result{}, walkErr
	}
	return node.JSONResult(map[string]any{"items": items})
}

// ---- catalog.export_package ----

const redacted = "[redacted]"

func overlapsSensitive(start, end float64, marks []domain.SensitiveSegment) bool {
	for _, m := range marks {
		if start < m.EndSeconds && m.StartSeconds < end {
			return true
		}
	}
	return false
}

// redactTranscript replaces marked text, keeping timings (D30: redacted in exports).
// Words carry the text too, so they are dropped from a marked segment.
func redactTranscript(t domain.Transcript, marks []domain.SensitiveSegment) domain.Transcript {
	out := t
	out.Segments = make([]domain.TranscriptSegment, len(t.Segments))
	for i, s := range t.Segments {
		if overlapsSensitive(s.Start, s.End, marks) {
			s.Text = redacted
			s.Words = nil
		}
		out.Segments[i] = s
	}
	return out
}

func srtTime(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	ms := int64(seconds*1000 + 0.5)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

func renderSRT(t domain.Transcript) []byte {
	var b strings.Builder
	for i, s := range t.Segments {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTime(s.Start), srtTime(s.End), s.Text)
	}
	return []byte(b.String())
}

func exportPackage(ctx context.Context, env node.DaemonEnv, in node.Input) (node.Result, error) {
	var p struct {
		AssetID         string             `json:"assetId"`
		DestinationRole domain.StorageRole `json:"destinationRole"`
	}
	if err := in.Bind(&p); err != nil {
		return node.Result{}, err
	}
	if p.DestinationRole == "" {
		p.DestinationRole = domain.RoleMaster
	}
	if p.DestinationRole == domain.RoleArchive {
		return node.Result{}, errs.Newf(errs.CodeForbiddenStorageRole, "an export package does not go to the archive")
	}
	store := env.Store()
	a, err := store.Assets().Get(ctx, id.Ref(p.AssetID))
	if err != nil {
		return node.Result{}, err
	}
	if a.DeletedAt != nil {
		return node.Result{}, errs.NotFound("asset")
	}
	st, err := env.Storage(ctx, p.DestinationRole)
	if err != nil {
		return node.Result{}, err
	}
	prefix := fmt.Sprintf("exports/%s/", a.ID)
	put := func(name, contentType string, data []byte) error {
		_, err := st.Put(ctx, prefix+name, bytes.NewReader(data), connector.PutOptions{
			SizeBytes: int64(len(data)), ContentType: contentType, IdempotencyKey: in.IdempotencyKey + "/" + name,
		})
		return err
	}
	putJSON := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return put(name, "application/json", data)
	}
	if err := putJSON("metadata.json", a); err != nil {
		return node.Result{}, err
	}
	marks, err := store.Transcripts().Sensitive(ctx, id.Ref(a.ID), a.CurrentVersion)
	if err != nil && !errs.IsCode(err, errs.CodeNotFound) {
		return node.Result{}, err
	}
	if t, err := store.Transcripts().Get(ctx, id.Ref(a.ID), a.CurrentVersion); err == nil {
		safe := redactTranscript(t, marks)
		if err := putJSON("transcript.json", safe); err != nil {
			return node.Result{}, err
		}
		if err := put("transcript.srt", "application/x-subrip", renderSRT(safe)); err != nil {
			return node.Result{}, err
		}
	} else if !errs.IsCode(err, errs.CodeNotFound) {
		return node.Result{}, err
	}
	if an, err := store.Transcripts().Analysis(ctx, id.Ref(a.ID), a.CurrentVersion); err == nil {
		if overlaps := summaryUnsafe(an, marks); !overlaps {
			if err := put("summary.md", "text/markdown", []byte("# "+a.Title+"\n\n"+an.Summary+"\n")); err != nil {
				return node.Result{}, err
			}
		}
		chapters := []domain.Chapter{}
		for _, c := range an.Chapters {
			if overlapsSensitive(c.StartSeconds, c.EndSeconds, marks) {
				c.Summary = redacted
			}
			chapters = append(chapters, c)
		}
		if err := putJSON("chapters.json", chapters); err != nil {
			return node.Result{}, err
		}
	} else if !errs.IsCode(err, errs.CodeNotFound) {
		return node.Result{}, err
	}
	if err := putJSON("lineage.json", map[string]any{"lineage": a.Lineage, "provenance": a.Provenance}); err != nil {
		return node.Result{}, err
	}
	versions, err := store.Versions().List(ctx, id.Ref(a.ID))
	if err != nil {
		return node.Result{}, err
	}
	if err := putJSON("versions.json", versions); err != nil {
		return node.Result{}, err
	}
	pubs, _, err := store.Publications().List(ctx, repo.PublicationFilter{AssetID: id.Ref(a.ID)}, repo.Page{Limit: 1000})
	if err != nil {
		return node.Result{}, err
	}
	if err := putJSON("publications.json", pubs); err != nil {
		return node.Result{}, err
	}
	if a.Thumbnail != nil {
		if err := copyThumbnail(ctx, env, *a.Thumbnail, put); err != nil {
			return node.Result{}, err
		}
	}
	uri := domain.InputURI{Kind: domain.URIStorage, Role: p.DestinationRole, Key: prefix + "metadata.json"}
	return node.JSONResult(map[string]any{"uri": uri.String()})
}

// summaryUnsafe is conservative: a summary is free text derived from the whole
// transcript, so an asset with any marked segment exports no summary (D30).
func summaryUnsafe(_ domain.Analysis, marks []domain.SensitiveSegment) bool { return len(marks) > 0 }

func copyThumbnail(ctx context.Context, env node.DaemonEnv, file domain.MediaFile, put func(string, string, []byte) error) error {
	st, err := env.Storage(ctx, file.Storage)
	if err != nil {
		return err
	}
	r, _, err := st.Get(ctx, file.Key)
	if errs.IsCode(err, errs.CodeNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot read the thumbnail")
	}
	return put("thumbnail.jpg", "image/jpeg", buf.Bytes())
}
