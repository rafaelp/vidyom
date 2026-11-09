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

// jobRepo holds job state, the source of truth (D4), and checkpoints (D11).
type jobRepo struct {
	conn
	tenant domain.TenantID
}

var _ repo.JobRepo = jobRepo{}

func (r jobRepo) Create(ctx context.Context, j domain.Job) error {
	if j.TenantID != "" && j.TenantID != r.tenant {
		return invalidArg("job for tenant %q written to the database of %q", j.TenantID, r.tenant)
	}
	if j.ID == "" || j.ULID == "" {
		return invalidArg("a job needs a friendly ID and a ULID")
	}
	if id.Friendly(j.ID).Prefix() != id.PrefixJob {
		return invalidArg("%s is not a job ID", j.ID)
	}
	if j.WorkflowID == "" {
		return invalidArg("a job needs a workflow")
	}
	if j.Status == "" {
		j.Status = domain.JobQueued
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = r.now()
	}
	trigger, err := json.Marshal(j.Trigger)
	if err != nil {
		return err
	}
	permissions, err := jsonList(j.Permissions)
	if err != nil {
		return err
	}
	attention, err := jsonArg(j.Attention)
	if err != nil {
		return err
	}
	jobErr, err := jsonArg(j.Error)
	if err != nil {
		return err
	}
	snapshot, _ := jsonArg(j.Snapshot)
	inputs, _ := jsonArg(j.Inputs)
	outputs, _ := jsonArg(j.Outputs)
	return r.write(ctx, func(q querier) error {
		asset, err := optionalAssetULID(ctx, q, string(j.AssetID))
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO jobs (ulid, friendly_id, workflow_id, workflow_version, workflow_sha256, snapshot, status, status_reason,
				inputs, outputs, trigger_info, parent_job_id, asset_ulid, submitted_by, permissions, idempotency_key, trace_id, progress_percent,
				attention, error, created_at, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(j.ULID), string(j.ID), j.WorkflowID, j.WorkflowVersion, j.WorkflowSHA256, snapshot, string(j.Status), j.StatusReason,
			inputs, outputs, string(trigger), j.Trigger.ParentJobID, asset, j.SubmittedBy, permissions, j.IdempotencyKey, j.TraceID, j.ProgressPercent,
			attention, jobErr, timeText(j.CreatedAt), timeArg(j.StartedAt), timeArg(j.FinishedAt))
		return mapErr(err)
	})
}

func jobSelect(withSnapshot bool) string {
	snapshot := "NULL"
	if withSnapshot {
		snapshot = "j.snapshot"
	}
	return `SELECT j.rowid, j.friendly_id, j.ulid, j.workflow_id, j.workflow_version, j.workflow_sha256, ` + snapshot + `, j.status, j.status_reason,
		j.inputs, j.outputs, j.trigger_info, COALESCE(a.friendly_id, ''), j.submitted_by, j.permissions, j.idempotency_key, j.trace_id, j.progress_percent,
		j.attention, j.error, j.created_at, j.started_at, j.finished_at
		FROM jobs j LEFT JOIN assets a ON a.ulid = j.asset_ulid`
}

func (r jobRepo) scanRow(s scanner) (int64, domain.Job, error) {
	var (
		j     domain.Job
		rowid int64
	)
	err := s.Scan(&rowid, &j.ID, &j.ULID, &j.WorkflowID, &j.WorkflowVersion, &j.WorkflowSHA256, rawCol{&j.Snapshot}, &j.Status, &j.StatusReason,
		rawCol{&j.Inputs}, rawCol{&j.Outputs}, jsonCol{&j.Trigger}, &j.AssetID, &j.SubmittedBy, jsonCol{&j.Permissions}, &j.IdempotencyKey, &j.TraceID, &j.ProgressPercent,
		jsonCol{&j.Attention}, jsonCol{&j.Error}, reqTime{&j.CreatedAt}, nullTime{&j.StartedAt}, nullTime{&j.FinishedAt})
	j.TenantID = r.tenant
	if len(j.Permissions) == 0 {
		j.Permissions = nil
	}
	return rowid, j, err
}

func (r jobRepo) scanJob(s scanner) (domain.Job, error) {
	_, j, err := r.scanRow(s)
	return j, err
}

func (r jobRepo) Get(ctx context.Context, ref id.Ref) (domain.Job, error) {
	column, value, ok := refColumn(ref, id.PrefixJob)
	if !ok {
		return domain.Job{}, errs.NotFound("job")
	}
	j, err := r.scanJob(r.q().QueryRowContext(ctx, jobSelect(true)+` WHERE j.`+column+` = ?`, value))
	if err != nil {
		return domain.Job{}, notFoundIfNone(err, "job")
	}
	return j, nil
}

func (r jobRepo) GetByULID(ctx context.Context, job id.ULID) (domain.Job, error) {
	j, err := r.scanJob(r.q().QueryRowContext(ctx, jobSelect(true)+` WHERE j.ulid = ?`, string(job)))
	if err != nil {
		return domain.Job{}, notFoundIfNone(err, "job")
	}
	return j, nil
}

func (r jobRepo) FindByIdempotencyKey(ctx context.Context, key string) (domain.Job, bool, error) {
	if key == "" {
		return domain.Job{}, false, nil
	}
	j, err := r.scanJob(r.q().QueryRowContext(ctx, jobSelect(true)+` WHERE j.idempotency_key = ?`, key))
	if err != nil {
		if isNoRows(err) {
			return domain.Job{}, false, nil
		}
		return domain.Job{}, false, mapErr(err)
	}
	return j, true, nil
}

// List omits the snapshot, which Get and ListActive return (I102).
func (r jobRepo) List(ctx context.Context, f repo.JobFilter, p repo.Page) ([]domain.Job, string, error) {
	var (
		items []domain.Job
		next  string
	)
	err := r.read(ctx, func(q querier) error {
		var w where
		if f.Status != "" {
			w.add("j.status = ?", string(f.Status))
		}
		if f.WorkflowID != "" {
			w.add("j.workflow_id = ?", f.WorkflowID)
		}
		if f.AssetID != "" {
			key, err := resolveAsset(ctx, q, f.AssetID)
			if err != nil {
				return err
			}
			w.add("j.asset_ulid = ?", key.ulid)
		}
		if f.ParentJob != "" {
			parent, err := resolveJob(ctx, q, f.ParentJob)
			if err != nil {
				return err
			}
			w.add("j.parent_job_id IN (?, ?)", string(parent.friendly), parent.ulid)
		}
		var err error
		items, next, err = listDesc(ctx, q, jobSelect(false), "j.rowid", w, p, r.scanRow)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return items, next, nil
}

type jobKey struct {
	ulid     string
	friendly id.Friendly
}

func resolveJob(ctx context.Context, q querier, ref id.Ref) (jobKey, error) {
	column, value, ok := refColumn(ref, id.PrefixJob)
	if !ok {
		return jobKey{}, errs.NotFound("job")
	}
	var key jobKey
	err := q.QueryRowContext(ctx, `SELECT ulid, friendly_id FROM jobs WHERE `+column+` = ?`, value).Scan(&key.ulid, &key.friendly)
	if err != nil {
		return jobKey{}, notFoundIfNone(err, "job")
	}
	return key, nil
}

// Update writes the job's state. The snapshot, workflow identity and trigger are
// immutable (§6.5), and an invalid status transition is refused.
func (r jobRepo) Update(ctx context.Context, j domain.Job) error {
	if j.TenantID != "" && j.TenantID != r.tenant {
		return invalidArg("job for tenant %q written to the database of %q", j.TenantID, r.tenant)
	}
	attention, err := jsonArg(j.Attention)
	if err != nil {
		return err
	}
	jobErr, err := jsonArg(j.Error)
	if err != nil {
		return err
	}
	inputs, _ := jsonArg(j.Inputs)
	outputs, _ := jsonArg(j.Outputs)
	return r.write(ctx, func(q querier) error {
		ref := id.Ref(j.ULID)
		if ref == "" {
			ref = id.Ref(j.ID)
		}
		key, err := resolveJob(ctx, q, ref)
		if err != nil {
			return err
		}
		var status domain.JobStatus
		if err := q.QueryRowContext(ctx, `SELECT status FROM jobs WHERE ulid = ?`, key.ulid).Scan(&status); err != nil {
			return mapErr(err)
		}
		if status != j.Status && !status.CanTransition(j.Status) {
			return errs.Newf(errs.CodeFailedPrecondition, "job %s cannot move from %s to %s", key.friendly, status, j.Status)
		}
		asset, err := optionalAssetULID(ctx, q, string(j.AssetID))
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `UPDATE jobs SET status = ?, status_reason = ?, inputs = ?, outputs = ?, asset_ulid = ?, progress_percent = ?,
				attention = ?, error = ?, started_at = ?, finished_at = ?
			WHERE ulid = ?`,
			string(j.Status), j.StatusReason, inputs, outputs, asset, j.ProgressPercent, attention, jobErr, timeArg(j.StartedAt), timeArg(j.FinishedAt), key.ulid)
		return mapErr(err)
	})
}

// ListActive returns non-terminal jobs with their snapshots, for reconciliation (D4).
func (r jobRepo) ListActive(ctx context.Context) ([]domain.Job, error) {
	return queryAll(ctx, r.q(), jobSelect(true)+` WHERE j.status NOT IN (?, ?, ?) ORDER BY j.rowid`,
		[]any{string(domain.JobCompleted), string(domain.JobFailed), string(domain.JobCancelled)}, r.scanJob)
}

const nodeColumns = `job_ulid, node_id, node_type, type_version, status, attempt, parameters, output, error, idempotency_key, task_id, worker_id, lease_id, lease_expires_at, progress_percent, started_at, finished_at`

func scanNode(s scanner) (domain.NodeExecution, error) {
	var n domain.NodeExecution
	err := s.Scan(&n.JobULID, &n.NodeID, &n.NodeType, &n.TypeVersion, &n.Status, &n.Attempt, rawCol{&n.Parameters}, rawCol{&n.Output}, jsonCol{&n.Error},
		&n.IdempotencyKey, &n.TaskID, &n.WorkerID, &n.LeaseID, nullTime{&n.LeaseExpiresAt}, &n.ProgressPercent, nullTime{&n.StartedAt}, nullTime{&n.FinishedAt})
	return n, err
}

// PutNode upserts one node's durable record. A completed record is the checkpoint
// (D11). Output above the cap is refused (I35).
func (r jobRepo) PutNode(ctx context.Context, n domain.NodeExecution) error {
	if n.JobULID == "" || n.NodeID == "" {
		return invalidArg("a node execution needs a job and a node ID")
	}
	if len(n.Output) > domain.MaxNodeOutputBytes {
		return errs.Newf(errs.CodeOutputTooLarge, "node %s output is %d bytes; the limit is %d", n.NodeID, len(n.Output), domain.MaxNodeOutputBytes)
	}
	parameters, _ := jsonArg(n.Parameters)
	output, _ := jsonArg(n.Output)
	nodeErr, err := jsonArg(n.Error)
	if err != nil {
		return err
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO node_executions (`+nodeColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (job_ulid, node_id) DO UPDATE SET node_type = excluded.node_type, type_version = excluded.type_version, status = excluded.status,
				attempt = excluded.attempt, parameters = excluded.parameters, output = excluded.output, error = excluded.error,
				idempotency_key = excluded.idempotency_key, task_id = excluded.task_id, worker_id = excluded.worker_id, lease_id = excluded.lease_id,
				lease_expires_at = excluded.lease_expires_at, progress_percent = excluded.progress_percent, started_at = excluded.started_at, finished_at = excluded.finished_at`,
			string(n.JobULID), n.NodeID, n.NodeType, n.TypeVersion, string(n.Status), n.Attempt, parameters, output, nodeErr,
			n.IdempotencyKey, n.TaskID, n.WorkerID, n.LeaseID, timeArg(n.LeaseExpiresAt), n.ProgressPercent, timeArg(n.StartedAt), timeArg(n.FinishedAt))
		if err != nil && errs.IsCode(mapErr(err), errs.CodeFailedPrecondition) {
			return errs.NotFound("job")
		}
		return mapErr(err)
	})
}

func (r jobRepo) GetNode(ctx context.Context, job id.ULID, nodeID string) (domain.NodeExecution, error) {
	n, err := scanNode(r.q().QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM node_executions WHERE job_ulid = ? AND node_id = ?`, string(job), nodeID))
	if err != nil {
		return domain.NodeExecution{}, notFoundIfNone(err, "node execution")
	}
	return n, nil
}

func (r jobRepo) GetNodeByTask(ctx context.Context, taskID string) (domain.NodeExecution, error) {
	if taskID == "" {
		return domain.NodeExecution{}, errs.NotFound("task")
	}
	n, err := scanNode(r.q().QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM node_executions WHERE task_id = ?`, taskID))
	if err != nil {
		return domain.NodeExecution{}, notFoundIfNone(err, "task")
	}
	return n, nil
}

func (r jobRepo) ListNodes(ctx context.Context, job id.ULID) ([]domain.NodeExecution, error) {
	return queryAll(ctx, r.q(), `SELECT `+nodeColumns+` FROM node_executions WHERE job_ulid = ? ORDER BY rowid`, []any{string(job)}, scanNode)
}

// AppendEvent assigns the next sequence within the job under the write lock.
func (r jobRepo) AppendEvent(ctx context.Context, e domain.JobEvent) (int64, error) {
	if e.At.IsZero() {
		e.At = r.now()
	}
	if e.Kind == "" {
		return 0, invalidArg("a job event needs a kind")
	}
	data, _ := jsonArg(e.Data)
	var seq int64
	err := r.write(ctx, func(q querier) error {
		key, err := resolveJob(ctx, q, id.Ref(e.JobID))
		if err != nil {
			return err
		}
		return mapErr(q.QueryRowContext(ctx, `INSERT INTO job_events (job_ulid, seq, at, kind, node_id, level, message, data)
			VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM job_events WHERE job_ulid = ?), ?, ?, ?, ?, ?, ?)
			RETURNING seq`,
			key.ulid, key.ulid, timeText(e.At), string(e.Kind), e.NodeID, e.Level, e.Message, data).Scan(&seq))
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

func (r jobRepo) ListEvents(ctx context.Context, job id.Ref, afterSeq int64, limit int) ([]domain.JobEvent, error) {
	if limit <= 0 || limit > maxPageLimit {
		limit = maxPageLimit
	}
	var out []domain.JobEvent
	err := r.read(ctx, func(q querier) error {
		key, err := resolveJob(ctx, q, job)
		if err != nil {
			return err
		}
		out, err = queryAll(ctx, q, `SELECT seq, at, kind, node_id, level, message, data FROM job_events WHERE job_ulid = ? AND seq > ? ORDER BY seq LIMIT ?`,
			[]any{key.ulid, afterSeq, limit},
			func(s scanner) (domain.JobEvent, error) {
				e := domain.JobEvent{JobID: key.friendly}
				err := s.Scan(&e.Seq, reqTime{&e.At}, &e.Kind, &e.NodeID, &e.Level, &e.Message, rawCol{&e.Data})
				return e, err
			})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteFinishedBefore removes terminal jobs with their nodes and events. Provenance
// lives on the asset version and approvals are kept (R21, I105).
func (r jobRepo) DeleteFinishedBefore(ctx context.Context, t time.Time) (int64, error) {
	var removed int64
	err := r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `DELETE FROM jobs WHERE finished_at IS NOT NULL AND finished_at < ? AND status IN (?, ?, ?)`,
			timeText(t), string(domain.JobCompleted), string(domain.JobFailed), string(domain.JobCancelled))
		if err != nil {
			return mapErr(err)
		}
		removed, err = res.RowsAffected()
		return mapErr(err)
	})
	return removed, err
}

// ---- Approvals ----

type approvalRepo struct {
	conn
	tenant domain.TenantID
}

var _ repo.ApprovalRepo = approvalRepo{}

func (r approvalRepo) Create(ctx context.Context, a domain.Approval) error {
	if a.TenantID != "" && a.TenantID != r.tenant {
		return invalidArg("approval for tenant %q written to the database of %q", a.TenantID, r.tenant)
	}
	if a.ID == "" || a.ULID == "" {
		return invalidArg("an approval needs a friendly ID and a ULID")
	}
	if id.Friendly(a.ID).Prefix() != id.PrefixApproval {
		return invalidArg("%s is not an approval ID", a.ID)
	}
	if a.Kind == "" {
		return invalidArg("an approval needs a kind")
	}
	if a.Status == "" {
		a.Status = domain.ApprovalPending
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = r.now()
	}
	approvers, err := jsonList(a.Approvers)
	if err != nil {
		return err
	}
	segments, err := jsonList(a.Segments)
	if err != nil {
		return err
	}
	evidence, _ := jsonArg(a.Evidence)
	return r.write(ctx, func(q querier) error {
		asset, err := optionalAssetULID(ctx, q, string(a.AssetID))
		if err != nil {
			return err
		}
		jobULID := ""
		if a.JobID != "" {
			key, err := resolveJob(ctx, q, id.Ref(a.JobID))
			if err != nil {
				return err
			}
			jobULID = key.ulid
			a.JobID = key.friendly
		}
		_, err = q.ExecContext(ctx, `INSERT INTO approvals (ulid, friendly_id, kind, status, asset_ulid, asset_version, job_id, job_ulid, node_id, approvers, evidence, preview,
				allow_adjust, segments, timeout_seconds, on_timeout, expires_at, decided_by, decided_at, reason, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(a.ULID), string(a.ID), string(a.Kind), string(a.Status), asset, a.AssetVersion, string(a.JobID), jobULID, a.NodeID, approvers, evidence, a.Preview,
			boolInt(a.AllowAdjust), segments, a.TimeoutSeconds, string(a.OnTimeout), timeArg(a.ExpiresAt), a.DecidedBy, timeArg(a.DecidedAt), a.Reason, timeText(a.CreatedAt))
		return mapErr(err)
	})
}

const approvalSelect = `SELECT p.rowid, p.friendly_id, p.ulid, p.kind, p.status, COALESCE(a.friendly_id, ''), p.asset_version, p.job_id, p.node_id, p.approvers, p.evidence, p.preview,
	p.allow_adjust, p.segments, p.timeout_seconds, p.on_timeout, p.expires_at, p.decided_by, p.decided_at, p.reason, p.created_at
	FROM approvals p LEFT JOIN assets a ON a.ulid = p.asset_ulid`

func (r approvalRepo) scanRow(s scanner) (int64, domain.Approval, error) {
	var (
		a           domain.Approval
		rowid       int64
		allowAdjust int
	)
	err := s.Scan(&rowid, &a.ID, &a.ULID, &a.Kind, &a.Status, &a.AssetID, &a.AssetVersion, &a.JobID, &a.NodeID, jsonCol{&a.Approvers}, rawCol{&a.Evidence}, &a.Preview,
		&allowAdjust, jsonCol{&a.Segments}, &a.TimeoutSeconds, &a.OnTimeout, nullTime{&a.ExpiresAt}, &a.DecidedBy, nullTime{&a.DecidedAt}, &a.Reason, reqTime{&a.CreatedAt})
	a.TenantID = r.tenant
	a.AllowAdjust = allowAdjust != 0
	if len(a.Approvers) == 0 {
		a.Approvers = nil
	}
	if len(a.Segments) == 0 {
		a.Segments = nil
	}
	return rowid, a, err
}

func (r approvalRepo) scanApproval(s scanner) (domain.Approval, error) {
	_, a, err := r.scanRow(s)
	return a, err
}

func (r approvalRepo) Get(ctx context.Context, ref id.Ref) (domain.Approval, error) {
	column, value, ok := refColumn(ref, id.PrefixApproval)
	if !ok {
		return domain.Approval{}, errs.NotFound("approval")
	}
	a, err := r.scanApproval(r.q().QueryRowContext(ctx, approvalSelect+` WHERE p.`+column+` = ?`, value))
	if err != nil {
		return domain.Approval{}, notFoundIfNone(err, "approval")
	}
	return a, nil
}

// GetForNode returns the latest approval of a job node, if any.
func (r approvalRepo) GetForNode(ctx context.Context, job id.ULID, nodeID string) (domain.Approval, bool, error) {
	a, err := r.scanApproval(r.q().QueryRowContext(ctx, approvalSelect+` WHERE p.job_ulid = ? AND p.node_id = ? ORDER BY p.rowid DESC LIMIT 1`, string(job), nodeID))
	if err != nil {
		if isNoRows(err) {
			return domain.Approval{}, false, nil
		}
		return domain.Approval{}, false, mapErr(err)
	}
	return a, true, nil
}

func (r approvalRepo) List(ctx context.Context, f repo.ApprovalFilter, p repo.Page) ([]domain.Approval, string, error) {
	var (
		items []domain.Approval
		next  string
	)
	err := r.read(ctx, func(q querier) error {
		var w where
		if f.Status != "" {
			w.add("p.status = ?", string(f.Status))
		}
		if f.Kind != "" {
			w.add("p.kind = ?", string(f.Kind))
		}
		if f.AssetID != "" {
			key, err := resolveAsset(ctx, q, f.AssetID)
			if err != nil {
				return err
			}
			w.add("p.asset_ulid = ?", key.ulid)
		}
		var err error
		items, next, err = listDesc(ctx, q, approvalSelect, "p.rowid", w, p, r.scanRow)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return items, next, nil
}

// Decide applies a decision to a pending record once. The check and the write are
// one statement, so two simultaneous decisions cannot both succeed.
func (r approvalRepo) Decide(ctx context.Context, ref id.Ref, d repo.ApprovalDecision) (domain.Approval, error) {
	switch d.Status {
	case domain.ApprovalApproved, domain.ApprovalRejected, domain.ApprovalExpired:
	default:
		return domain.Approval{}, invalidArg("%q is not a decision", d.Status)
	}
	column, value, ok := refColumn(ref, id.PrefixApproval)
	if !ok {
		return domain.Approval{}, errs.NotFound("approval")
	}
	if d.DecidedAt.IsZero() {
		d.DecidedAt = r.now()
	}
	var out domain.Approval
	err := r.write(ctx, func(q querier) error {
		var segments any
		if d.Segments != nil {
			text, err := jsonList(d.Segments)
			if err != nil {
				return err
			}
			segments = text
		}
		res, err := q.ExecContext(ctx, `UPDATE approvals SET status = ?, decided_by = ?, decided_at = ?, reason = ?, segments = COALESCE(?, segments)
			WHERE `+column+` = ? AND status = ?`,
			string(d.Status), d.DecidedBy, timeText(d.DecidedAt), d.Reason, segments, value, string(domain.ApprovalPending))
		if err != nil {
			return mapErr(err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return mapErr(err)
		}
		out, err = r.scanApproval(q.QueryRowContext(ctx, approvalSelect+` WHERE p.`+column+` = ?`, value))
		if err != nil {
			return notFoundIfNone(err, "approval")
		}
		if n == 0 {
			return errs.Newf(errs.CodeFailedPrecondition, "approval %s was already decided: %s", out.ID, out.Status)
		}
		return nil
	})
	if err != nil {
		return domain.Approval{}, err
	}
	return out, nil
}

func (r approvalRepo) ExpiringBefore(ctx context.Context, t time.Time) ([]domain.Approval, error) {
	return queryAll(ctx, r.q(), approvalSelect+` WHERE p.status = ? AND p.expires_at IS NOT NULL AND p.expires_at <= ? ORDER BY p.expires_at`,
		[]any{string(domain.ApprovalPending), timeText(t)}, r.scanApproval)
}

// ---- Publications ----

type publicationRepo struct {
	conn
	tenant domain.TenantID
}

var _ repo.PublicationRepo = publicationRepo{}

const publicationSelect = `SELECT p.rowid, p.friendly_id, p.ulid, a.friendly_id, p.destination, p.asset_version, p.published_file_sha256, p.published_file, p.remote_id, p.url,
	p.status, p.visibility, p.approval_id, p.title, p.description, p.idempotency_key, p.status_reason, p.created_at, p.updated_at, p.published_at, p.withdrawn_at
	FROM publications p JOIN assets a ON a.ulid = p.asset_ulid`

func scanPublicationRow(tenant domain.TenantID, s scanner) (int64, domain.Publication, error) {
	var (
		p     domain.Publication
		rowid int64
	)
	err := s.Scan(&rowid, &p.ID, &p.ULID, &p.AssetID, &p.Destination, &p.AssetVersion, &p.PublishedFileSHA256, jsonCol{&p.PublishedFile}, &p.RemoteID, &p.URL,
		&p.Status, &p.Visibility, &p.ApprovalID, &p.Title, &p.Description, &p.IdempotencyKey, &p.StatusReason, reqTime{&p.CreatedAt}, reqTime{&p.UpdatedAt}, nullTime{&p.PublishedAt}, nullTime{&p.WithdrawnAt})
	p.TenantID = tenant
	return rowid, p, err
}

func scanPublicationFor(tenant domain.TenantID) func(scanner) (domain.Publication, error) {
	return func(s scanner) (domain.Publication, error) {
		_, p, err := scanPublicationRow(tenant, s)
		return p, err
	}
}

func (r publicationRepo) Create(ctx context.Context, p domain.Publication) error {
	if p.TenantID != "" && p.TenantID != r.tenant {
		return invalidArg("publication for tenant %q written to the database of %q", p.TenantID, r.tenant)
	}
	if p.ID == "" || p.ULID == "" {
		return invalidArg("a publication needs a friendly ID and a ULID")
	}
	if id.Friendly(p.ID).Prefix() != id.PrefixPublication {
		return invalidArg("%s is not a publication ID", p.ID)
	}
	if p.Status == "" {
		p.Status = domain.PubPendingApproval
	}
	if p.Visibility == "" {
		p.Visibility = domain.VisibilityPrivate
	}
	if err := p.Validate(); err != nil {
		return errs.Wrap(errs.CodeFailedPrecondition, err, err.Error())
	}
	now := r.now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	file, err := jsonArg(p.PublishedFile)
	if err != nil {
		return err
	}
	return r.write(ctx, func(q querier) error {
		key, err := resolveAsset(ctx, q, id.Ref(p.AssetID))
		if err != nil {
			return err
		}
		_, err = q.ExecContext(ctx, `INSERT INTO publications (ulid, friendly_id, asset_ulid, destination, asset_version, published_file_sha256, published_file, remote_id, url,
				status, visibility, approval_id, title, description, idempotency_key, status_reason, created_at, updated_at, published_at, withdrawn_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(p.ULID), string(p.ID), key.ulid, p.Destination, p.AssetVersion, p.PublishedFileSHA256, file, p.RemoteID, p.URL,
			string(p.Status), string(p.Visibility), string(p.ApprovalID), p.Title, p.Description, p.IdempotencyKey, p.StatusReason,
			timeText(p.CreatedAt), timeText(p.UpdatedAt), timeArg(p.PublishedAt), timeArg(p.WithdrawnAt))
		if err != nil {
			return mapErr(err)
		}
		return markImmutableIfPublished(ctx, q, key.ulid, p)
	})
}

// markImmutableIfPublished applies D35 whoever writes a published record (I105).
func markImmutableIfPublished(ctx context.Context, q querier, assetULID string, p domain.Publication) error {
	if p.Status != domain.PubPublished {
		return nil
	}
	res, err := q.ExecContext(ctx, `UPDATE asset_versions SET immutable = 1 WHERE asset_ulid = ? AND version = ?`, assetULID, p.AssetVersion)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		// A later Create and PutFile could otherwise replace media already published.
		return errs.Newf(errs.CodeFailedPrecondition, "publication %s names version %s, which does not exist", p.ID, p.AssetVersion)
	}
	return nil
}

func (r publicationRepo) Get(ctx context.Context, ref id.Ref) (domain.Publication, error) {
	column, value, ok := refColumn(ref, id.PrefixPublication)
	if !ok {
		return domain.Publication{}, errs.NotFound("publication")
	}
	p, err := scanPublicationFor(r.tenant)(r.q().QueryRowContext(ctx, publicationSelect+` WHERE p.`+column+` = ?`, value))
	if err != nil {
		return domain.Publication{}, notFoundIfNone(err, "publication")
	}
	return p, nil
}

func (r publicationRepo) FindByIdempotencyKey(ctx context.Context, key string) (domain.Publication, bool, error) {
	if key == "" {
		return domain.Publication{}, false, nil
	}
	p, err := scanPublicationFor(r.tenant)(r.q().QueryRowContext(ctx, publicationSelect+` WHERE p.idempotency_key = ?`, key))
	if err != nil {
		if isNoRows(err) {
			return domain.Publication{}, false, nil
		}
		return domain.Publication{}, false, mapErr(err)
	}
	return p, true, nil
}

func (r publicationRepo) List(ctx context.Context, f repo.PublicationFilter, pg repo.Page) ([]domain.Publication, string, error) {
	var (
		items []domain.Publication
		next  string
	)
	err := r.read(ctx, func(q querier) error {
		var w where
		if f.AssetID != "" {
			key, err := resolveAsset(ctx, q, f.AssetID)
			if err != nil {
				return err
			}
			w.add("p.asset_ulid = ?", key.ulid)
		}
		if f.Destination != "" {
			w.add("p.destination = ?", f.Destination)
		}
		if f.Status != "" {
			w.add("p.status = ?", string(f.Status))
		}
		var err error
		items, next, err = listDesc(ctx, q, publicationSelect, "p.rowid", w, pg, func(s scanner) (int64, domain.Publication, error) {
			return scanPublicationRow(r.tenant, s)
		})
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return items, next, nil
}

func (r publicationRepo) Update(ctx context.Context, p domain.Publication) error {
	if p.TenantID != "" && p.TenantID != r.tenant {
		return invalidArg("publication for tenant %q written to the database of %q", p.TenantID, r.tenant)
	}
	if err := p.Validate(); err != nil {
		return errs.Wrap(errs.CodeFailedPrecondition, err, err.Error())
	}
	file, err := jsonArg(p.PublishedFile)
	if err != nil {
		return err
	}
	ref := id.Ref(p.ULID)
	if ref == "" {
		ref = id.Ref(p.ID)
	}
	column, value, ok := refColumn(ref, id.PrefixPublication)
	if !ok {
		return errs.NotFound("publication")
	}
	return r.write(ctx, func(q querier) error {
		var assetULID string
		if err := q.QueryRowContext(ctx, `SELECT asset_ulid FROM publications WHERE `+column+` = ?`, value).Scan(&assetULID); err != nil {
			return notFoundIfNone(err, "publication")
		}
		_, err := q.ExecContext(ctx, `UPDATE publications SET destination = ?, asset_version = ?, published_file_sha256 = ?, published_file = ?, remote_id = ?, url = ?,
				status = ?, visibility = ?, approval_id = ?, title = ?, description = ?, status_reason = ?, updated_at = ?, published_at = ?, withdrawn_at = ?
			WHERE `+column+` = ?`,
			p.Destination, p.AssetVersion, p.PublishedFileSHA256, file, p.RemoteID, p.URL,
			string(p.Status), string(p.Visibility), string(p.ApprovalID), p.Title, p.Description, p.StatusReason, timeText(r.now()), timeArg(p.PublishedAt), timeArg(p.WithdrawnAt),
			value)
		if err != nil {
			return mapErr(err)
		}
		return markImmutableIfPublished(ctx, q, assetULID, p)
	})
}

// ListPublic is the only source of public.db (D18).
func (r publicationRepo) ListPublic(ctx context.Context) ([]domain.Publication, error) {
	return queryAll(ctx, r.q(), publicationSelect+` WHERE p.status = ? AND p.visibility = ? AND a.deleted_at IS NULL ORDER BY p.rowid`,
		[]any{string(domain.PubPublished), string(domain.VisibilityPublic)}, scanPublicationFor(r.tenant))
}

// ---- Uploads ----

type uploadRepo struct {
	conn
	tenant domain.TenantID
}

var _ repo.UploadRepo = uploadRepo{}

const uploadColumns = `friendly_id, ulid, filename, size_bytes, sha256, chunk_bytes, received_bytes, status, created_by, created_at, expires_at`

func (r uploadRepo) scan(s scanner) (domain.Upload, error) {
	u := domain.Upload{TenantID: r.tenant}
	err := s.Scan(&u.ID, &u.ULID, &u.Filename, &u.SizeBytes, &u.SHA256, &u.ChunkBytes, &u.ReceivedBytes, &u.Status, &u.CreatedBy, reqTime{&u.CreatedAt}, reqTime{&u.ExpiresAt})
	return u, err
}

func (r uploadRepo) Create(ctx context.Context, u domain.Upload) error {
	if u.TenantID != "" && u.TenantID != r.tenant {
		return invalidArg("upload for tenant %q written to the database of %q", u.TenantID, r.tenant)
	}
	if u.ID == "" || u.ULID == "" {
		return invalidArg("an upload needs a friendly ID and a ULID")
	}
	if id.Friendly(u.ID).Prefix() != id.PrefixUpload {
		return invalidArg("%s is not an upload ID", u.ID)
	}
	if u.Status == "" {
		u.Status = domain.UploadPending
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = r.now()
	}
	return r.write(ctx, func(q querier) error {
		_, err := q.ExecContext(ctx, `INSERT INTO uploads (`+uploadColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(u.ID), string(u.ULID), u.Filename, u.SizeBytes, u.SHA256, u.ChunkBytes, u.ReceivedBytes, string(u.Status), u.CreatedBy, timeText(u.CreatedAt), timeText(u.ExpiresAt))
		return mapErr(err)
	})
}

func (r uploadRepo) Get(ctx context.Context, ref id.Ref) (domain.Upload, error) {
	column, value, ok := refColumn(ref, id.PrefixUpload)
	if !ok {
		return domain.Upload{}, errs.NotFound("upload")
	}
	u, err := r.scan(r.q().QueryRowContext(ctx, `SELECT `+uploadColumns+` FROM uploads WHERE `+column+` = ?`, value))
	if err != nil {
		return domain.Upload{}, notFoundIfNone(err, "upload")
	}
	return u, nil
}

func (r uploadRepo) Update(ctx context.Context, u domain.Upload) error {
	ref := id.Ref(u.ULID)
	if ref == "" {
		ref = id.Ref(u.ID)
	}
	column, value, ok := refColumn(ref, id.PrefixUpload)
	if !ok {
		return errs.NotFound("upload")
	}
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `UPDATE uploads SET received_bytes = ?, status = ?, expires_at = ?, chunk_bytes = ? WHERE `+column+` = ?`,
			u.ReceivedBytes, string(u.Status), timeText(u.ExpiresAt), u.ChunkBytes, value)
		return requireAffected(res, err, "upload")
	})
}

func (r uploadRepo) ListExpired(ctx context.Context, t time.Time) ([]domain.Upload, error) {
	return queryAll(ctx, r.q(), `SELECT `+uploadColumns+` FROM uploads WHERE expires_at <= ? AND status = ? ORDER BY expires_at`,
		[]any{timeText(t), string(domain.UploadPending)}, r.scan)
}

func (r uploadRepo) Delete(ctx context.Context, ref id.Ref) error {
	column, value, ok := refColumn(ref, id.PrefixUpload)
	if !ok {
		return errs.NotFound("upload")
	}
	return r.write(ctx, func(q querier) error {
		res, err := q.ExecContext(ctx, `DELETE FROM uploads WHERE `+column+` = ?`, value)
		return requireAffected(res, err, "upload")
	})
}
