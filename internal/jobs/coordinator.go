package jobs

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/version"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
)

// Coordinator returns what the API's worker routes call (I4).
func (e *Engine) Coordinator() workerproto.Coordinator { return coordinator{e} }

type coordinator struct{ e *Engine }

var _ workerproto.Coordinator = coordinator{}

func gone(what string) error { return errs.Newf(errs.CodeGone, "task is obsolete: %s", what) }

func (c coordinator) Register(ctx context.Context, req workerproto.RegisterRequest) (workerproto.RegisterResponse, error) {
	pools := make([]string, len(req.Pools))
	for i, p := range req.Pools {
		pools[i] = string(p)
	}
	err := c.e.d.Scheduler.Matcher.Register(ctx, domain.Worker{ID: req.WorkerID, Pools: pools, Resources: req.Resources, Version: req.Version})
	if err != nil {
		return workerproto.RegisterResponse{}, err
	}
	return workerproto.RegisterResponse{
		HeartbeatSeconds: c.e.d.HeartbeatSeconds,
		ProtocolVersion:  workerproto.ProtocolVersion,
		DaemonVersion:    version.Version,
	}, nil
}

func (c coordinator) Heartbeat(ctx context.Context, req workerproto.HeartbeatRequest) error {
	return c.e.d.Scheduler.Matcher.Heartbeat(ctx, req.WorkerID, req.Running, req.DiskMB)
}

func (c coordinator) Lease(ctx context.Context, taskID string, req workerproto.LeaseRequest) (workerproto.Lease, error) {
	return c.e.lease(ctx, taskID, req.WorkerID, true)
}

// lease grants exactly one live lease, which is what makes a duplicated pointer
// harmless (I4). matchWorker is false for the daemon's own pool.
func (e *Engine) lease(ctx context.Context, taskID, workerID string, matchWorker bool) (workerproto.Lease, error) {
	jobULID, nodeID, _, err := jobqueue.ParseTaskID(taskID)
	if err != nil {
		return workerproto.Lease{}, gone("unknown task")
	}
	_, store, err := e.tenantOf(ctx, jobULID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return workerproto.Lease{}, gone("its job does not exist")
		}
		return workerproto.Lease{}, err
	}
	unlock := e.locks.lock(jobULID)
	defer unlock()
	job, err := store.Jobs().GetByULID(ctx, jobULID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return workerproto.Lease{}, gone("its job does not exist")
		}
		return workerproto.Lease{}, err
	}
	n, err := store.Jobs().GetNode(ctx, jobULID, nodeID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return workerproto.Lease{}, gone("its node does not exist")
		}
		return workerproto.Lease{}, err
	}
	switch {
	case n.TaskID != taskID:
		return workerproto.Lease{}, gone("a newer attempt exists")
	case job.Status.Terminal():
		return workerproto.Lease{}, gone("its job ended")
	case strings.HasPrefix(job.StatusReason, reasonCancelRequested):
		return workerproto.Lease{}, gone("its job is being cancelled")
	case job.Status == domain.JobPaused, job.Status == domain.JobNeedsAttention:
		// Reconciliation enqueues it again when the job continues.
		return workerproto.Lease{}, gone("its job is on hold")
	}
	now := e.now()
	switch n.Status {
	case domain.NodeQueued:
	case domain.NodeRunning:
		if len(n.Output) > 0 {
			return workerproto.Lease{}, gone("its result is recorded")
		}
		// Exactly one live lease, even for the same worker: two entries for one task can
		// reach one worker, and only the first may run. A worker that lost the answer
		// to its request waits for the lease to expire.
		if n.LeaseExpiresAt != nil && now.Before(*n.LeaseExpiresAt) {
			return workerproto.Lease{}, errs.Newf(errs.CodeConflict, "task is leased by worker %s", n.WorkerID)
		}
		return workerproto.Lease{}, gone("its lease expired")
	default:
		return workerproto.Lease{}, gone("its node is " + string(n.Status))
	}
	desc, ok := e.descriptor(n.NodeType, n.TypeVersion)
	if !ok {
		return workerproto.Lease{}, errs.Newf(errs.CodeInternal, "unknown node type %s", n.NodeType)
	}
	if matchWorker {
		if err := e.d.Scheduler.Matcher.Compatible(workerID, desc.Pool, desc.Requirements); err != nil {
			return workerproto.Lease{}, err
		}
	}
	expires := now.Add(e.d.LeaseTTL)
	n.Status = domain.NodeRunning
	n.WorkerID = workerID
	n.LeaseID = string(id.New())
	n.LeaseExpiresAt = &expires
	n.StartedAt = &now
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().PutNode(ctx, n); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"status": n.Status, "attempt": n.Attempt, "worker_id": workerID})
		_, err := tx.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: job.ID, At: now, Kind: domain.JobEventNode, NodeID: nodeID, Data: data})
		return err
	})
	if err != nil {
		return workerproto.Lease{}, err
	}
	e.notifyJob(job.ULID)
	e.checks.Store(job.ULID, expires)
	return e.leaseOf(ctx, store, job, n)
}

func (e *Engine) leaseOf(ctx context.Context, store repo.TenantStore, job domain.Job, n domain.NodeExecution) (workerproto.Lease, error) {
	expires := e.now().Add(e.d.LeaseTTL)
	if n.LeaseExpiresAt != nil {
		expires = *n.LeaseExpiresAt
	}
	lease := workerproto.Lease{
		TaskID:         n.TaskID,
		LeaseID:        n.LeaseID,
		ExpiresAt:      expires,
		TenantID:       store.Tenant(),
		JobID:          job.ID,
		JobULID:        job.ULID,
		NodeID:         n.NodeID,
		NodeType:       n.NodeType,
		TypeVersion:    n.TypeVersion,
		Attempt:        n.Attempt,
		Parameters:     n.Parameters,
		IdempotencyKey: n.IdempotencyKey,
		TraceID:        job.TraceID,
		Permissions:    job.Permissions,
		ScratchJobs:    e.ancestors(ctx, store, job),
	}
	if desc, ok := e.descriptor(n.NodeType, n.TypeVersion); ok && desc.Pool == node.PoolAI && e.d.Config != nil {
		// Model names, not secrets (I197).
		if cfg, err := e.d.Config.TenantConfig(ctx, store.Tenant()); err == nil {
			ai := cfg.AI
			lease.AI = &ai
		}
	}
	return lease, nil
}

// ancestors lists the jobs above this one, whose scratch it may read (I182).
func (e *Engine) ancestors(ctx context.Context, store repo.TenantStore, job domain.Job) []id.ULID {
	var out []id.ULID
	parent := job.Trigger.ParentJobID
	for parent != "" && len(out) <= MaxSubworkflowDepth {
		out = append(out, id.ULID(parent))
		j, err := store.Jobs().GetByULID(ctx, id.ULID(parent))
		if err != nil {
			break
		}
		parent = j.Trigger.ParentJobID
	}
	return out
}

// liveTask loads a task for someone holding its lease. A wrong lease is 410: another
// owner has it, and the caller drops what it has (I4). The caller holds the job lock.
func (e *Engine) liveTask(ctx context.Context, store repo.TenantStore, jobULID id.ULID, nodeID, taskID, leaseID string) (domain.Job, domain.NodeExecution, error) {
	job, err := store.Jobs().GetByULID(ctx, jobULID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return domain.Job{}, domain.NodeExecution{}, gone("its job does not exist")
		}
		return domain.Job{}, domain.NodeExecution{}, err
	}
	n, err := store.Jobs().GetNode(ctx, jobULID, nodeID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return domain.Job{}, domain.NodeExecution{}, gone("its node does not exist")
		}
		return domain.Job{}, domain.NodeExecution{}, err
	}
	if n.TaskID != taskID || n.LeaseID != leaseID || leaseID == "" {
		return domain.Job{}, domain.NodeExecution{}, gone("the lease is not current")
	}
	return job, n, nil
}

func (c coordinator) TaskHeartbeat(ctx context.Context, taskID string, req workerproto.TaskHeartbeatRequest) (workerproto.TaskHeartbeatResponse, error) {
	e := c.e
	jobULID, nodeID, _, err := jobqueue.ParseTaskID(taskID)
	if err != nil {
		return workerproto.TaskHeartbeatResponse{}, gone("unknown task")
	}
	_, store, err := e.tenantOf(ctx, jobULID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return workerproto.TaskHeartbeatResponse{}, gone("its job does not exist")
		}
		return workerproto.TaskHeartbeatResponse{}, err
	}
	unlock := e.locks.lock(jobULID)
	defer unlock()
	job, n, err := e.liveTask(ctx, store, jobULID, nodeID, taskID, req.LeaseID)
	if err != nil {
		return workerproto.TaskHeartbeatResponse{}, err
	}
	if n.Status != domain.NodeRunning {
		return workerproto.TaskHeartbeatResponse{}, gone("its node is " + string(n.Status))
	}
	now := e.now()
	expires := now.Add(e.d.LeaseTTL)
	n.LeaseExpiresAt = &expires
	writeEvent := false
	if req.Percent > 0 && req.Percent-n.ProgressPercent >= 1 {
		n.ProgressPercent = req.Percent
		writeEvent = true
	}
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		if err := tx.Jobs().PutNode(ctx, n); err != nil {
			return err
		}
		if !writeEvent {
			return nil
		}
		data, _ := json.Marshal(map[string]any{"percent": req.Percent})
		_, err := tx.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: job.ID, At: now, Kind: domain.JobEventProgress, NodeID: nodeID, Message: req.Message, Data: data})
		return err
	})
	if err != nil {
		return workerproto.TaskHeartbeatResponse{}, err
	}
	if writeEvent {
		e.notifyJob(jobULID)
	}
	e.checks.Store(jobULID, expires)
	return workerproto.TaskHeartbeatResponse{
		ExpiresAt: expires,
		Cancel:    strings.HasPrefix(job.StatusReason, reasonCancelRequested) || job.Status.Terminal(),
	}, nil
}

const maxLogLinesPerRequest = 500

func (c coordinator) TaskLogs(ctx context.Context, taskID string, req workerproto.TaskLogsRequest) error {
	e := c.e
	jobULID, nodeID, _, err := jobqueue.ParseTaskID(taskID)
	if err != nil {
		return gone("unknown task")
	}
	tenant, store, err := e.tenantOf(ctx, jobULID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return gone("its job does not exist")
		}
		return err
	}
	unlock := e.locks.lock(jobULID)
	job, _, err := e.liveTask(ctx, store, jobULID, nodeID, taskID, req.LeaseID)
	unlock()
	if err != nil {
		return err
	}
	lines := req.Lines
	if len(lines) > maxLogLinesPerRequest {
		lines = lines[len(lines)-maxLogLinesPerRequest:]
	}
	err = store.InTx(ctx, func(tx repo.TenantRepos) error {
		for _, l := range lines {
			at := l.At
			if at.IsZero() {
				at = e.now()
			}
			if _, err := tx.Jobs().AppendEvent(ctx, domain.JobEvent{JobID: job.ID, At: at, Kind: domain.JobEventLog, NodeID: nodeID, Level: l.Level, Message: l.Message}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, l := range lines {
		e.d.Logger.Info(l.Message, "tenant_id", tenant, "job_id", job.ID, "node_id", nodeID, "trace_id", job.TraceID, "worker_level", l.Level)
	}
	e.notifyJob(jobULID)
	return nil
}

func (c coordinator) Complete(ctx context.Context, taskID string, req workerproto.CompleteRequest) error {
	e := c.e
	jobULID, nodeID, _, err := jobqueue.ParseTaskID(taskID)
	if err != nil {
		return gone("unknown task")
	}
	tenant, store, err := e.tenantOf(ctx, jobULID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return gone("its job does not exist")
		}
		return err
	}
	return e.completeTask(ctx, tenant, store, jobULID, nodeID, taskID, req.LeaseID, node.Result{Output: req.Output, Usage: req.Usage, Tools: req.Tools})
}

// completeTask records a task's result: idempotent on task and lease, so a worker may
// deliver it again until acknowledged, and only then ack the stream (I4).
func (e *Engine) completeTask(ctx context.Context, tenant domain.TenantID, store repo.TenantStore, jobULID id.ULID, nodeID, taskID, leaseID string, res node.Result) error {
	r, unlock, err := e.taskRun(ctx, store, jobULID, nodeID, taskID, leaseID)
	if err != nil {
		return err
	}
	defer unlock()
	n := r.nodes[nodeID]
	if n.Status == domain.NodeCompleted || (n.Status == domain.NodeRunning && len(n.Output) > 0) {
		return nil
	}
	if n.Status != domain.NodeRunning {
		return gone("its node is " + string(n.Status))
	}
	e.recordUsage(ctx, tenant, r.job, nodeID, res.Usage)
	if err := r.applyResult(nodeID, res, nil); err != nil {
		return err
	}
	e.Kick(tenant, jobULID)
	return nil
}

func (e *Engine) recordUsage(ctx context.Context, tenant domain.TenantID, job domain.Job, nodeID string, usage []domain.UsageRecord) {
	if len(usage) == 0 || e.d.Budgets == nil {
		return
	}
	for i := range usage {
		usage[i].TenantID = tenant
		usage[i].JobID = string(job.ID)
		usage[i].NodeID = nodeID
		if usage[i].At.IsZero() {
			usage[i].At = e.now()
		}
	}
	if err := e.d.Budgets.Record(ctx, tenant, usage); err != nil {
		// The work happened: losing a cost estimate must not undo it.
		e.d.Logger.Warn("cannot record usage", "tenant_id", tenant, "job_id", job.ID, "node_id", nodeID, "error", err)
	}
}

// taskRun loads a job for applying a task's result, holding its lock.
func (e *Engine) taskRun(ctx context.Context, store repo.TenantStore, jobULID id.ULID, nodeID, taskID, leaseID string) (*jobRun, func(), error) {
	unlock := e.locks.lock(jobULID)
	job, _, err := e.liveTask(ctx, store, jobULID, nodeID, taskID, leaseID)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	rt, err := e.runtime(job)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	r := &jobRun{e: e, ctx: ctx, store: store, job: job, rt: rt, decoded: map[string]any{}}
	if err := r.loadNodes(); err != nil {
		unlock()
		return nil, nil, err
	}
	return r, unlock, nil
}

func (c coordinator) Fail(ctx context.Context, taskID string, req workerproto.FailRequest) error {
	e := c.e
	jobULID, nodeID, _, err := jobqueue.ParseTaskID(taskID)
	if err != nil {
		return gone("unknown task")
	}
	tenant, store, err := e.tenantOf(ctx, jobULID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return gone("its job does not exist")
		}
		return err
	}
	return e.failTask(ctx, tenant, store, jobULID, nodeID, taskID, req.LeaseID, failure{
		Code: req.Code, Message: req.Message, Retryable: req.Retryable, Unknown: req.OutcomeUnknown,
	})
}

func (e *Engine) failTask(ctx context.Context, tenant domain.TenantID, store repo.TenantStore, jobULID id.ULID, nodeID, taskID, leaseID string, f failure) error {
	r, unlock, err := e.taskRun(ctx, store, jobULID, nodeID, taskID, leaseID)
	if err != nil {
		return err
	}
	defer unlock()
	n := r.nodes[nodeID]
	if n.Status != domain.NodeRunning || len(n.Output) > 0 {
		// Already answered: a repeat of a delivered failure, or a retry scheduled.
		return nil
	}
	if err := r.onFailure(nodeID, f); err != nil {
		return err
	}
	e.Kick(tenant, jobULID)
	return nil
}

func (c coordinator) Connection(ctx context.Context, taskID string, req workerproto.ConnectionRequest) (workerproto.ConnectionResponse, error) {
	e := c.e
	jobULID, nodeID, _, err := jobqueue.ParseTaskID(taskID)
	if err != nil {
		return workerproto.ConnectionResponse{}, gone("unknown task")
	}
	tenant, store, err := e.tenantOf(ctx, jobULID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return workerproto.ConnectionResponse{}, gone("its job does not exist")
		}
		return workerproto.ConnectionResponse{}, err
	}
	if err := e.requireRunning(ctx, store, jobULID, nodeID, taskID, req.LeaseID); err != nil {
		return workerproto.ConnectionResponse{}, err
	}
	if e.d.Config == nil {
		return workerproto.ConnectionResponse{}, errs.New(errs.CodeUnavailable, "tenant configuration is not wired in this daemon")
	}
	cfg, err := e.d.Config.TenantConfig(ctx, tenant)
	if err != nil {
		return workerproto.ConnectionResponse{}, err
	}
	inst, ok := cfg.Storages.Roles[req.Role]
	if !ok {
		return workerproto.ConnectionResponse{}, errs.Newf(errs.CodeFailedPrecondition, "storage role %s is not configured", req.Role)
	}
	resp := workerproto.ConnectionResponse{Instance: inst}
	if inst.CredentialsRef != "" {
		if e.d.SecretsFor == nil {
			return workerproto.ConnectionResponse{}, errs.New(errs.CodeUnavailable, "the vault is not wired in this daemon")
		}
		secret, err := e.d.SecretsFor(tenant).ResolveSecret(ctx, inst.CredentialsRef)
		if err != nil {
			return workerproto.ConnectionResponse{}, err
		}
		resp.Credentials = secret
	}
	return resp, nil
}

func (e *Engine) requireRunning(ctx context.Context, store repo.TenantStore, jobULID id.ULID, nodeID, taskID, leaseID string) error {
	unlock := e.locks.lock(jobULID)
	defer unlock()
	_, n, err := e.liveTask(ctx, store, jobULID, nodeID, taskID, leaseID)
	if err != nil {
		return err
	}
	if n.Status != domain.NodeRunning || (n.LeaseExpiresAt != nil && !e.now().Before(*n.LeaseExpiresAt)) {
		return gone("there is no live lease")
	}
	return nil
}

func (c coordinator) CacheKey(ctx context.Context, taskID string, req workerproto.CacheKeyRequest) (workerproto.CacheKeyResponse, error) {
	e := c.e
	jobULID, nodeID, _, err := jobqueue.ParseTaskID(taskID)
	if err != nil {
		return workerproto.CacheKeyResponse{}, gone("unknown task")
	}
	tenant, store, err := e.tenantOf(ctx, jobULID)
	if err != nil {
		if errs.IsCode(err, errs.CodeNotFound) {
			return workerproto.CacheKeyResponse{}, gone("its job does not exist")
		}
		return workerproto.CacheKeyResponse{}, err
	}
	if err := e.requireRunning(ctx, store, jobULID, nodeID, taskID, req.LeaseID); err != nil {
		return workerproto.CacheKeyResponse{}, err
	}
	if e.d.Keys == nil {
		return workerproto.CacheKeyResponse{}, errs.New(errs.CodeUnavailable, "derived keys are not wired in this daemon")
	}
	key, err := e.d.Keys.CacheKey(ctx, tenant)
	if err != nil {
		return workerproto.CacheKeyResponse{}, err
	}
	return workerproto.CacheKeyResponse{Key: key}, nil
}

// reportProgress is the daemon pool's way of saying what a worker's heartbeat says.
func (e *Engine) reportProgress(taskID, leaseID string, percent float64, message string) {
	ctx := e.lifeContext()
	if _, err := e.Coordinator().TaskHeartbeat(ctx, taskID, workerproto.TaskHeartbeatRequest{LeaseID: leaseID, Percent: percent, Message: message}); err != nil && !errs.IsCode(err, errs.CodeGone) {
		e.d.Logger.Debug("progress not recorded", "task", taskID, "error", err)
	}
}
