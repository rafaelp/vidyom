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

// auditRepo is one hash chain (D23): a tenant's, or the system's with an empty
// tenant (I108). It has no update or delete except the retention prune.
type auditRepo struct {
	conn
	tenant domain.TenantID
}

var (
	_ repo.AuditRepo       = auditRepo{}
	_ repo.AuditMaintainer = auditRepo{}
)

const auditColumns = `seq, ulid, at, actor, method, action, permission, resource, result, request_id, details, prev_hash, hash`

func (r auditRepo) scan(s scanner) (domain.AuditRecord, error) {
	var rec domain.AuditRecord
	err := s.Scan(&rec.Seq, &rec.ULID, reqTime{&rec.At}, &rec.Actor, &rec.Method, &rec.Action, &rec.Permission,
		&rec.Resource, &rec.Result, &rec.RequestID, rawCol{&rec.Details}, &rec.PrevHash, &rec.Hash)
	rec.TenantID = r.tenant
	return rec, err
}

// Append assigns Seq, PrevHash and Hash under the write lock, so concurrent appends
// cannot fork the chain.
func (r auditRepo) Append(ctx context.Context, rec domain.AuditRecord) (domain.AuditRecord, error) {
	if rec.TenantID != "" && rec.TenantID != r.tenant {
		return domain.AuditRecord{}, invalidArg("audit record for tenant %q written to the chain of %q", rec.TenantID, r.tenant)
	}
	rec.TenantID = r.tenant
	if rec.Action == "" {
		return domain.AuditRecord{}, invalidArg("an audit record needs an action")
	}
	if rec.Result == "" {
		return domain.AuditRecord{}, invalidArg("an audit record needs a result")
	}
	if len(rec.Details) > 0 && !json.Valid(rec.Details) {
		return domain.AuditRecord{}, invalidArg("audit details must be JSON")
	}
	if rec.ULID == "" {
		rec.ULID = id.New()
	}
	if rec.At.IsZero() {
		rec.At = r.now()
	}
	rec.At = rec.At.UTC()
	err := r.write(ctx, func(q querier) error {
		prevSeq, prevHash, err := chainHead(ctx, q)
		if err != nil {
			return err
		}
		rec.Seq = prevSeq + 1
		rec.PrevHash = prevHash
		rec.Hash, err = rec.ComputeHash()
		if err != nil {
			return invalidArg("%v", err)
		}
		details, err := jsonArg(rec.Details)
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO audit_log (`+auditColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.Seq, string(rec.ULID), timeText(rec.At), rec.Actor, string(rec.Method), rec.Action, string(rec.Permission),
			rec.Resource, rec.Result, rec.RequestID, details, rec.PrevHash, rec.Hash)
		return mapErr(err)
	})
	if err != nil {
		return domain.AuditRecord{}, err
	}
	return rec, nil
}

// chainHead returns the last sequence and hash of the chain, falling back to the
// prune anchor and then to the genesis value.
func chainHead(ctx context.Context, q querier) (int64, string, error) {
	var seq int64
	var hash string
	err := q.QueryRowContext(ctx, `SELECT seq, hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&seq, &hash)
	if err == nil {
		return seq, hash, nil
	}
	if !isNoRows(err) {
		return 0, "", mapErr(err)
	}
	err = q.QueryRowContext(ctx, `SELECT seq, hash FROM audit_anchor WHERE id = 1`).Scan(&seq, &hash)
	if err == nil {
		return seq, hash, nil
	}
	if !isNoRows(err) {
		return 0, "", mapErr(err)
	}
	return 0, domain.AuditGenesisHash, nil
}

func (r auditRepo) List(ctx context.Context, f repo.AuditFilter, p repo.Page) ([]domain.AuditRecord, string, error) {
	var w where
	if f.Since != nil {
		w.add("at >= ?", timeText(*f.Since))
	}
	if f.Actor != "" {
		w.add("actor = ?", f.Actor)
	}
	if f.Action != "" {
		w.add("action = ?", f.Action)
	}
	if f.Resource != "" {
		w.add("resource = ?", f.Resource)
	}
	return listDesc(ctx, r.q(), `SELECT seq, `+auditColumns+` FROM audit_log`, "seq", w, p, func(s scanner) (int64, domain.AuditRecord, error) {
		var k int64
		var rec domain.AuditRecord
		err := s.Scan(&k, &rec.Seq, &rec.ULID, reqTime{&rec.At}, &rec.Actor, &rec.Method, &rec.Action, &rec.Permission,
			&rec.Resource, &rec.Result, &rec.RequestID, rawCol{&rec.Details}, &rec.PrevHash, &rec.Hash)
		rec.TenantID = r.tenant
		return k, rec, err
	})
}

func (r auditRepo) Scan(ctx context.Context, afterSeq int64, limit int) ([]domain.AuditRecord, error) {
	if limit <= 0 {
		limit = maxPageLimit
	}
	return queryAll(ctx, r.q(), `SELECT `+auditColumns+` FROM audit_log WHERE seq > ? ORDER BY seq LIMIT ?`, []any{afterSeq, limit}, r.scan)
}

func scanAuditExport(s scanner) (repo.AuditExport, error) {
	var e repo.AuditExport
	err := s.Scan(&e.FromSeq, &e.ToSeq, &e.HeadHash, &e.Key, &e.SHA256, reqTime{&e.At})
	return e, err
}

const auditExportColumns = `from_seq, to_seq, head_hash, key, sha256, at`

func (r auditRepo) LastExport(ctx context.Context) (repo.AuditExport, bool, error) {
	e, err := scanAuditExport(r.q().QueryRowContext(ctx, `SELECT `+auditExportColumns+` FROM audit_exports ORDER BY to_seq DESC LIMIT 1`))
	if err != nil {
		if isNoRows(err) {
			return repo.AuditExport{}, false, nil
		}
		return repo.AuditExport{}, false, mapErr(err)
	}
	return e, true, nil
}

func (r auditRepo) RecordExport(ctx context.Context, e repo.AuditExport) error {
	if e.At.IsZero() {
		e.At = r.now()
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO audit_exports (`+auditExportColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
			e.FromSeq, e.ToSeq, e.HeadHash, e.Key, e.SHA256, timeText(e.At))
		return mapErr(err)
	})
}

func (r auditRepo) ListExports(ctx context.Context) ([]repo.AuditExport, error) {
	return queryAll(ctx, r.q(), `SELECT `+auditExportColumns+` FROM audit_exports ORDER BY to_seq`, nil, scanAuditExport)
}

func (r auditRepo) Anchor(ctx context.Context) (repo.AuditAnchor, error) {
	var a repo.AuditAnchor
	err := r.q().QueryRowContext(ctx, `SELECT seq, hash FROM audit_anchor WHERE id = 1`).Scan(&a.Seq, &a.Hash)
	if err != nil {
		if isNoRows(err) {
			return repo.AuditAnchor{}, nil
		}
		return repo.AuditAnchor{}, mapErr(err)
	}
	return a, nil
}

// PruneExportedBefore removes whole exported segments older than t, up to the newest
// export boundary with nothing recent at or below it. The last removed record becomes
// the anchor. Stopping only at a boundary is what lets Verify compare the anchor with an
// export's recorded head (D23, I107). A chain that already disagrees with its export is
// evidence, so it is refused rather than pruned.
func (r auditRepo) PruneExportedBefore(ctx context.Context, t time.Time) (int64, error) {
	var removed int64
	err := r.write(ctx, func(q querier) error {
		var firstRecent *int64
		if err := q.QueryRowContext(ctx, `SELECT MIN(seq) FROM audit_log WHERE at >= ?`, timeText(t)).Scan(&firstRecent); err != nil {
			return mapErr(err)
		}
		query := `SELECT to_seq, head_hash FROM audit_exports WHERE to_seq > COALESCE((SELECT seq FROM audit_anchor WHERE id = 1), 0)`
		var args []any
		if firstRecent != nil {
			query += ` AND to_seq < ?`
			args = append(args, *firstRecent)
		}
		var (
			boundary int64
			head     string
		)
		err := q.QueryRowContext(ctx, query+` ORDER BY to_seq DESC LIMIT 1`, args...).Scan(&boundary, &head)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return mapErr(err)
		}
		var hash string
		err = q.QueryRowContext(ctx, `SELECT hash FROM audit_log WHERE seq = ?`, boundary).Scan(&hash)
		if isNoRows(err) || (err == nil && hash != head) {
			return errs.Newf(errs.CodeFailedPrecondition, "audit record %d differs from what was exported: not pruning", boundary)
		}
		if err != nil {
			return mapErr(err)
		}
		if _, err := q.ExecContext(ctx, `UPDATE audit_prune_guard SET allowed = 1`); err != nil {
			return mapErr(err)
		}
		res, err := q.ExecContext(ctx, `DELETE FROM audit_log WHERE seq <= ?`, boundary)
		if err != nil {
			return mapErr(err)
		}
		removed, err = res.RowsAffected()
		if err != nil {
			return mapErr(err)
		}
		if _, err := q.ExecContext(ctx, `UPDATE audit_prune_guard SET allowed = 0`); err != nil {
			return mapErr(err)
		}
		_, err = q.ExecContext(ctx, `INSERT INTO audit_anchor (id, seq, hash) VALUES (1, ?, ?)
			ON CONFLICT (id) DO UPDATE SET seq = excluded.seq, hash = excluded.hash`, boundary, hash)
		return mapErr(err)
	})
	return removed, err
}
