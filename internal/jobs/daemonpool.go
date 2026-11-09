package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
)

// daemonPool consumes pool daemon. These nodes need the database or the vault (I7),
// and they go through Redis like the others, so reconciliation has one model.
func (e *Engine) daemonPool(ctx context.Context) {
	slots := make(chan struct{}, e.d.DaemonConcurrency)
	claimEvery := time.NewTicker(e.d.LeaseTTL)
	defer claimEvery.Stop()
	handle := func(deliveries []jobqueue.Delivery) {
		for _, d := range deliveries {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			d := d
			e.background(func() {
				defer func() { <-slots }()
				e.runDaemonTask(ctx, d)
			})
		}
	}
	// Whatever this consumer read before a crash is still pending in Redis.
	if pending, err := e.d.Queue.ClaimStale(ctx, node.PoolDaemon, daemonConsumer, 0, 256); err == nil {
		handle(pending)
	}
	for ctx.Err() == nil {
		free := cap(slots) - len(slots)
		if free == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		got, err := e.d.Queue.Read(ctx, jobqueue.ReadOptions{Pool: node.PoolDaemon, Consumer: daemonConsumer, Count: free, Block: 250 * time.Millisecond})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.d.Logger.Warn("daemon pool cannot read the queue", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		handle(got)
		select {
		case <-claimEvery.C:
			if stale, err := e.d.Queue.ClaimStale(ctx, node.PoolDaemon, daemonConsumer, 2*e.d.LeaseTTL, free); err == nil {
				handle(stale)
			}
		default:
		}
	}
}

func (e *Engine) ack(ctx context.Context, d jobqueue.Delivery) {
	if err := e.d.Queue.Ack(ctx, node.PoolDaemon, d.StreamID, d.Pointer.TaskID); err != nil && ctx.Err() == nil {
		e.d.Logger.Warn("cannot ack a task", "task", d.Pointer.TaskID, "error", err)
	}
}

// runDaemonTask is what vidyom-worker does for a task, in-process.
func (e *Engine) runDaemonTask(ctx context.Context, d jobqueue.Delivery) {
	taskID := d.Pointer.TaskID
	lease, err := e.lease(ctx, taskID, daemonConsumer, false)
	switch {
	case err == nil:
	case errs.IsCode(err, errs.CodeGone):
		e.ack(ctx, d)
		return
	default:
		// Conflict or an outage: it stays unacknowledged, and ClaimStale brings it back.
		if ctx.Err() == nil {
			e.d.Logger.Debug("daemon pool did not lease a task", "task", taskID, "error", err)
		}
		return
	}
	store, err := e.d.Tenants.ForTenant(ctx, lease.TenantID)
	if err != nil {
		e.d.Logger.Warn("daemon pool cannot open the tenant", "tenant_id", lease.TenantID, "error", err)
		return
	}
	job, err := store.Jobs().GetByULID(ctx, lease.JobULID)
	if err != nil {
		e.d.Logger.Warn("daemon pool cannot load the job", "job", lease.JobULID, "error", err)
		return
	}
	impl, ok := e.d.Nodes.Lookup(lease.NodeType, lease.TypeVersion)
	if !ok {
		if err := e.failTask(ctx, lease.TenantID, store, lease.JobULID, lease.NodeID, taskID, lease.LeaseID, failure{
			Code: string(errs.CodeInternal), Message: "node type " + lease.NodeType + " is not registered in this daemon",
		}); err == nil {
			e.ack(ctx, d)
		}
		return
	}
	desc := impl.Descriptor()
	env := &nodeEnv{e: e, store: store, job: job, nodeID: lease.NodeID, taskID: taskID, leaseID: lease.LeaseID, ancestor: lease.ScratchJobs}
	in := node.Input{Parameters: lease.Parameters, Attempt: lease.Attempt, IdempotencyKey: lease.IdempotencyKey, LeaseID: lease.LeaseID}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.running.Store(taskID, cancel)
	defer e.running.Delete(taskID)
	stopBeat := e.beat(runCtx, taskID, lease.LeaseID, desc.SupportsAbort, cancel)
	defer stopBeat()

	res, execErr := e.executeDaemon(runCtx, impl, desc, env, in)
	switch {
	case execErr == nil:
		err = e.completeTask(ctx, lease.TenantID, store, lease.JobULID, lease.NodeID, taskID, lease.LeaseID, res)
	case errors.Is(execErr, errAttention):
		err = e.attentionTask(ctx, lease, store, execErr)
	default:
		f := failureOf(execErr)
		if runCtx.Err() != nil && ctx.Err() == nil {
			f = failure{Code: CodeCancelled, Message: "cancelled while running"}
		}
		err = e.failTask(ctx, lease.TenantID, store, lease.JobULID, lease.NodeID, taskID, lease.LeaseID, f)
	}
	if err == nil || errs.IsCode(err, errs.CodeGone) {
		e.ack(ctx, d)
		return
	}
	if ctx.Err() == nil {
		e.d.Logger.Warn("cannot record a task's result", "task", taskID, "error", err)
	}
}

var errAttention = errors.New("the outcome needs a person")

// executeDaemon asks a reconcilable node what happened before any retry (D11). Only
// the daemon pool can: the worker protocol has no reconcile call (I148).
func (e *Engine) executeDaemon(ctx context.Context, impl node.Node, desc node.Descriptor, env *nodeEnv, in node.Input) (node.Result, error) {
	if desc.SideEffect == node.EffectReconcilable && in.Attempt > 1 {
		rec, ok := impl.(node.Reconciler)
		if !ok {
			return node.Result{}, attentionError("node " + desc.Type + " declares reconcilable but cannot reconcile")
		}
		outcome, res, err := rec.Reconcile(ctx, env, in)
		switch {
		case err != nil:
			return node.Result{}, attentionError("reconciling failed: " + errs.Public(err).Message)
		case outcome == node.ReconcileDone:
			return res, nil
		case outcome == node.ReconcileNotStarted:
		default:
			return node.Result{}, attentionError("the destination cannot say whether the effect happened")
		}
	}
	return safeExecute(ctx, impl, env, in)
}

type attentionErr struct{ reason string }

func (a attentionErr) Error() string        { return a.reason }
func (a attentionErr) Is(target error) bool { return target == errAttention }

func attentionError(reason string) error { return attentionErr{reason} }

func (e *Engine) attentionTask(ctx context.Context, lease workerproto.Lease, store repo.TenantStore, cause error) error {
	r, unlock, err := e.taskRun(ctx, store, lease.JobULID, lease.NodeID, lease.TaskID, lease.LeaseID)
	if err != nil {
		return err
	}
	defer unlock()
	if r.nodes[lease.NodeID].Status != domain.NodeRunning {
		return nil
	}
	if err := r.toAttention(lease.NodeID, failure{Code: CodeNeedsAttention, Message: cause.Error(), Unknown: true}); err != nil {
		return err
	}
	e.Kick(lease.TenantID, lease.JobULID)
	return nil
}

// beat extends a daemon-pool lease and watches for a cooperative cancel (D11).
func (e *Engine) beat(ctx context.Context, taskID, leaseID string, abortable bool, cancel context.CancelFunc) func() {
	interval := e.d.LeaseTTL / 3
	if interval <= 0 {
		interval = time.Second
	}
	stop := make(chan struct{})
	e.background(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			resp, err := e.Coordinator().TaskHeartbeat(ctx, taskID, workerproto.TaskHeartbeatRequest{LeaseID: leaseID})
			if err != nil {
				if errs.IsCode(err, errs.CodeGone) {
					cancel()
					return
				}
				continue
			}
			if resp.Cancel && abortable {
				cancel()
				return
			}
		}
	})
	return func() { close(stop) }
}
