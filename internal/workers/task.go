package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/observability"
	"go.opentelemetry.io/otel/attribute"
)

// failCodeCancelled is what a node aborted by the cancel flag reports (I194).
const failCodeCancelled = "cancelled"

var (
	// errCancelled is the cause when the daemon's cancel flag aborts a node.
	errCancelled = errors.New("the job was cancelled")
	// errLeaseLost is the cause when the daemon says the lease is gone.
	errLeaseLost = errors.New("the lease is gone")
	// errOutage is the cause after DaemonOutageLimit without a heartbeat.
	errOutage = errors.New("the daemon has been unreachable too long")
)

// handle takes one delivery from the stream to its acknowledgment.
func (w *worker) handle(d jobqueue.Delivery, pool node.Pool) {
	if d.Pointer.NodeType == jobqueue.NodeTypeScratchCleanup {
		w.cleanupScratch(d, pool)
		return
	}
	lease, err := w.cfg.Coordinator.Lease(w.execCtx, d.Pointer.TaskID, workerproto.LeaseRequest{WorkerID: w.cfg.WorkerID})
	if err != nil {
		switch errs.CodeOf(err) {
		case errs.CodeGone:
			w.ack(pool, d)
		case errs.CodeConflict:
			w.cfg.Logger.Debug("task left for another worker", "task_id", d.Pointer.TaskID)
		default:
			if w.execCtx.Err() == nil {
				w.cfg.Logger.Warn("lease failed; the entry stays pending", "task_id", d.Pointer.TaskID, "error", errs.Public(err).Message)
			}
		}
		return
	}
	buffer := &lineBuffer{}
	logger := newTaskLogger(w.cfg.Logger.Handler(), buffer,
		slog.String("tenant_id", string(lease.TenantID)),
		slog.String("job_id", string(lease.JobID)),
		slog.String("node_id", lease.NodeID),
		slog.String("node_type", lease.NodeType),
		slog.Int("attempt", lease.Attempt),
		slog.String("trace_id", lease.TraceID),
	)
	internal := w.cfg.Logger.With(
		"tenant_id", string(lease.TenantID), "job_id", string(lease.JobID),
		"node_id", lease.NodeID, "node_type", lease.NodeType, "trace_id", lease.TraceID,
	)
	run := &taskRun{worker: w, delivery: d, pool: pool, lease: lease, logger: logger, internal: internal, buffer: buffer}
	run.execute()
}

// cleanupScratch serves a cleanup pointer: no lease, no result (I181).
func (w *worker) cleanupScratch(d jobqueue.Delivery, pool node.Pool) {
	job, _, _, err := jobqueue.ParseTaskID(d.Pointer.TaskID)
	if err == nil {
		err = RemoveJobScratch(w.cfg.ScratchDir, d.Pointer.TenantID, job)
	}
	if err != nil {
		w.cfg.Logger.Warn("job scratch cleanup failed", "task_id", d.Pointer.TaskID, "error", err)
	}
	w.ack(pool, d)
}

// taskRun is one leased execution.
type taskRun struct {
	worker   *worker
	delivery jobqueue.Delivery
	pool     node.Pool
	lease    workerproto.Lease
	// logger also feeds the job's stream, which clients read. internal does not: only
	// it may carry an internal cause or FFmpeg's stderr (I190).
	logger   *slog.Logger
	internal *slog.Logger
	buffer   *lineBuffer
}

func (t *taskRun) taskID() string { return t.lease.TaskID }

func (t *taskRun) execute() {
	w := t.worker
	n, ok := w.registry.Lookup(t.lease.NodeType, t.lease.TypeVersion)
	if !ok {
		t.fail(workerproto.FailRequest{
			Code:    string(errs.CodeNotImplemented),
			Message: fmt.Sprintf("this worker has no node %s@%d", t.lease.NodeType, t.lease.TypeVersion),
		})
		return
	}
	desc := n.Descriptor()
	if desc.Pool != t.pool {
		t.fail(workerproto.FailRequest{
			Code:    string(errs.CodeInternal),
			Message: fmt.Sprintf("node %s belongs to pool %s, not %s", t.lease.NodeType, desc.Pool, t.pool),
		})
		return
	}
	env, err := NewTaskEnv(w.deps, t.lease, t.logger)
	if err != nil {
		t.reportError(err)
		return
	}
	defer env.Close()
	var nodeEnv node.Env = env
	if t.pool == node.PoolAI {
		nodeEnv = aiEnv{env}
	}

	ctx, cancel := context.WithCancelCause(w.execCtx)
	defer cancel(nil)
	stopHeartbeat := t.heartbeat(ctx, cancel, desc, env)

	started := w.cfg.Now()
	spanCtx, span := observability.StartJobSpan(ctx, t.lease.TraceID, "node "+t.lease.NodeType,
		attribute.String("tenant_id", string(t.lease.TenantID)),
		attribute.String("job_id", string(t.lease.JobID)),
		attribute.String("node_id", t.lease.NodeID),
		attribute.Int("attempt", t.lease.Attempt))
	result, execErr := safeExecute(spanCtx, t.internal, n, nodeEnv, node.Input{
		Parameters:     t.lease.Parameters,
		Attempt:        t.lease.Attempt,
		IdempotencyKey: t.lease.IdempotencyKey,
		LeaseID:        t.lease.LeaseID,
		PersonRetry:    t.lease.PersonRetry,
	})
	observability.EndSpan(span, execErr)
	stopHeartbeat()
	t.flushLogs()
	// Plaintext inputs go now, not after re-delivery, which can last a daemon outage.
	usage := env.Usage()
	env.Close()
	w.cfg.Metrics.Observe(telemetry.NodeDuration, w.cfg.Now().Sub(started).Seconds(), telemetry.Labels{
		"node_type": t.lease.NodeType, "tenant_id": string(t.lease.TenantID),
	})

	cause := context.Cause(ctx)
	if errors.Is(cause, errLeaseLost) {
		t.logger.Info("dropping the result of a task whose lease is gone")
		w.ack(t.pool, t.delivery)
		return
	}
	if execErr != nil {
		w.cfg.Metrics.Add(telemetry.NodeFailures, 1, telemetry.Labels{
			"node_type": t.lease.NodeType, "tenant_id": string(t.lease.TenantID),
		})
		failure := failureFor(execErr, cause)
		t.internal.Error("node failed", "code", failure.Code, "cause", execErr)
		t.logger.Error("node failed", "code", failure.Code, "reason", failure.Message)
		t.flushLogs()
		t.fail(failure)
		return
	}
	t.complete(result, usage)
}

// failureFor maps a node's error to what the daemon needs (D11), using only public
// text, so an internal cause never leaves the worker.
func failureFor(err, cause error) workerproto.FailRequest {
	switch {
	case errors.Is(cause, errCancelled) && isContextError(err):
		return workerproto.FailRequest{Code: failCodeCancelled, Message: "the node was aborted by the job's cancellation"}
	case (errors.Is(cause, errShutdown) || errors.Is(cause, errOutage)) && isContextError(err):
		return workerproto.FailRequest{Code: string(errs.CodeUnavailable), Message: cause.Error(), Retryable: true}
	}
	public := errs.Public(err)
	return workerproto.FailRequest{
		Code:           string(public.Code),
		Message:        public.Message,
		Retryable:      node.IsTransient(err),
		OutcomeUnknown: node.IsUnknownOutcome(err),
	}
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// safeExecute turns a node's panic into a failure, so one bad node cannot take the
// other tasks of this worker down with it.
func safeExecute(ctx context.Context, internal *slog.Logger, n node.Node, env node.Env, in node.Input) (result node.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			internal.Error("node panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = errs.New(errs.CodeInternal, "the node crashed")
		}
	}()
	return n.Execute(ctx, env, in)
}

// heartbeat extends the lease and carries progress until stopped. The returned
// function stops it and waits.
func (t *taskRun) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, desc node.Descriptor, env *TaskEnv) func() {
	w := t.worker
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(w.heartbeatEvery)
		defer ticker.Stop()
		lastSuccess := w.cfg.Now()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			t.flushLogs()
			progress := env.lastProgress()
			resp, err := w.cfg.Coordinator.TaskHeartbeat(ctx, t.taskID(), workerproto.TaskHeartbeatRequest{
				LeaseID: t.lease.LeaseID,
				Percent: progress.percent,
				Message: progress.message,
			})
			if err != nil {
				switch {
				case ctx.Err() != nil:
					return
				case errs.IsCode(err, errs.CodeGone):
					t.logger.Warn("the daemon says this lease is gone; aborting the node")
					cancel(errLeaseLost)
					return
				}
				if w.cfg.Now().Sub(lastSuccess) > w.cfg.DaemonOutageLimit {
					t.logger.Error("aborting the node: the daemon has been unreachable too long")
					cancel(errOutage)
					return
				}
				t.logger.Warn("task heartbeat failed; continuing", "error", errs.Public(err).Message)
				continue
			}
			lastSuccess = w.cfg.Now()
			if resp.Cancel && desc.SupportsAbort {
				// A node that cannot abort finishes, and the engine stops at the next
				// checkpoint (D11).
				t.logger.Info("aborting the node on the job's cancellation")
				cancel(errCancelled)
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func (t *taskRun) flushLogs() {
	for {
		lines := t.buffer.take()
		if len(lines) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(t.worker.reportCtx, 10*time.Second)
		err := t.worker.cfg.Coordinator.TaskLogs(ctx, t.taskID(), workerproto.TaskLogsRequest{LeaseID: t.lease.LeaseID, Lines: lines})
		cancel()
		if err != nil {
			return // Best effort: the worker's own log has them too.
		}
	}
}

func (t *taskRun) complete(result node.Result, usage []domain.UsageRecord) {
	switch {
	case result.Suspend != nil:
		t.fail(workerproto.FailRequest{Code: string(errs.CodeInternal), Message: "a worker-pool node cannot suspend"})
		return
	case len(result.Output) > domain.MaxNodeOutputBytes:
		t.fail(workerproto.FailRequest{Code: string(errs.CodeOutputTooLarge), Message: "the node's output exceeds the 32 MiB cap"})
		return
	}
	output := result.Output
	if len(output) == 0 {
		output = json.RawMessage(`{}`)
	}
	req := workerproto.CompleteRequest{
		LeaseID: t.lease.LeaseID,
		Output:  output,
		Usage:   append(result.Usage, usage...),
		Tools:   result.Tools,
	}
	t.deliver("complete", func(ctx context.Context) error {
		return t.worker.cfg.Coordinator.Complete(ctx, t.taskID(), req)
	})
}

func (t *taskRun) fail(req workerproto.FailRequest) {
	req.LeaseID = t.lease.LeaseID
	t.deliver("fail", func(ctx context.Context) error {
		return t.worker.cfg.Coordinator.Fail(ctx, t.taskID(), req)
	})
}

func (t *taskRun) reportError(err error) {
	t.fail(failureFor(err, nil))
}

// deliver repeats a report until the daemon has committed it, and only then acks
// (I4). Gone means someone else owns the task: ack and drop. Nothing else may ack,
// so a 404, which the engine never answers for a task of its own, is a misrouted
// request worth another try. After DaemonOutageLimit the report is abandoned and the
// entry stays pending, so a poisoned task cannot hold a slot forever: the lease then
// expires, and the daemon decides.
func (t *taskRun) deliver(what string, call func(ctx context.Context) error) {
	w := t.worker
	deadline := w.cfg.Now().Add(w.cfg.DaemonOutageLimit)
	backoff := time.Second
	for {
		err := call(w.reportCtx)
		switch {
		case err == nil:
			w.ack(t.pool, t.delivery)
			return
		case w.reportCtx.Err() != nil:
			t.internal.Warn("worker stopping before the daemon acknowledged a result", "report", what)
			return
		}
		switch errs.CodeOf(err) {
		case errs.CodeGone:
			t.internal.Info("the daemon no longer wants this result", "report", what, "error", errs.Public(err).Message)
			w.ack(t.pool, t.delivery)
			return
		case errs.CodeInvalidArgument, errs.CodeConflict, errs.CodeFailedPrecondition, errs.CodeOutputTooLarge, errs.CodeValidationFailed:
			// No retry can change the answer. The entry stays pending, and the daemon's
			// reconciliation decides.
			t.internal.Error("the daemon refused a result", "report", what, "error", errs.Public(err).Message)
			return
		}
		if w.cfg.Now().After(deadline) {
			t.internal.Error("giving up on delivering a result; the entry stays pending", "report", what, "error", errs.Public(err).Message)
			return
		}
		t.internal.Warn("result not delivered yet", "report", what, "error", errs.Public(err).Message)
		if !sleep(w.reportCtx, backoff) {
			return
		}
		backoff = min(backoff*2, w.cfg.RetryMax)
	}
}
