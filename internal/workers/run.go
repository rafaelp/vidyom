package workers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/contract/telemetry"
	"github.com/rafaelp/vidyom/internal/contract/workerproto"
	"github.com/rafaelp/vidyom/internal/workers/cache"
)

// reportGrace is how long results may still be delivered once shutdown's grace ends.
const reportGrace = 10 * time.Second

// errShutdown is the cause when the worker stops while a node runs.
var errShutdown = errors.New("the worker is shutting down")

// worker is one running process.
type worker struct {
	cfg      Config
	registry node.Registry
	deps     TaskDeps

	slots chan struct{}

	mu      sync.Mutex
	running map[string]id.ULID // task ID to job

	tasks sync.WaitGroup

	execCtx    context.Context
	execCancel context.CancelCauseFunc
	reportCtx  context.Context
	reportStop context.CancelFunc

	heartbeatEvery time.Duration
}

// Run registers the worker and serves its pools until ctx ends, then drains: running
// nodes get ShutdownGrace to finish before they are cancelled and reported as
// retryable. It returns an error only for what no retry can fix at startup.
func Run(ctx context.Context, cfg Config, registry node.Registry) error {
	if err := cfg.applyDefaults(); err != nil {
		return err
	}
	if slices.Contains(cfg.Pools, node.PoolMedia) {
		warnings, err := cfg.FFmpeg.Check(ctx)
		if err != nil {
			return fmt.Errorf("workers: the media pool needs FFmpeg: %w", err)
		}
		for _, w := range warnings {
			cfg.Logger.Warn(w)
		}
	}
	for _, dir := range []string{cfg.ScratchDir, cfg.CacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("workers: %w", err)
		}
	}
	tenantCache, err := cache.New(cache.Options{
		Root:       cfg.CacheDir,
		MaxBytes:   cfg.CacheMaxBytes,
		IdleExpiry: cfg.CacheIdle,
		Now:        cfg.Now,
		Logger:     cfg.Logger,
	})
	if err != nil {
		return fmt.Errorf("workers: %w", err)
	}
	w := &worker{
		cfg:      cfg,
		registry: registry,
		slots:    make(chan struct{}, cfg.Resources.MaxConcurrency),
		running:  map[string]id.ULID{},
		deps: TaskDeps{
			Services:    cfg.Coordinator,
			Connectors:  cfg.Connectors,
			Cache:       tenantCache,
			ScratchDir:  cfg.ScratchDir,
			SpoolDir:    cfg.SpoolDir,
			IngestRoots: cfg.IngestRoots,
			AIWorker:    cfg.AIWorker,
			Transcriber: cfg.Transcriber,
			Embedder:    cfg.Embedder,
			Metrics:     cfg.Metrics,
			Now:         cfg.Now,
		},
	}
	return w.run(ctx, tenantCache)
}

func (w *worker) run(ctx context.Context, tenantCache *cache.Cache) error {
	w.execCtx, w.execCancel = context.WithCancelCause(context.WithoutCancel(ctx))
	w.reportCtx, w.reportStop = context.WithCancel(context.WithoutCancel(ctx))
	defer w.reportStop()
	defer w.execCancel(nil)

	resp, err := w.register(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	w.heartbeatEvery = time.Duration(resp.HeartbeatSeconds) * time.Second
	if w.heartbeatEvery <= 0 {
		w.heartbeatEvery = workerproto.DefaultHeartbeatSeconds * time.Second
	}
	if w.cfg.HeartbeatInterval > 0 {
		w.heartbeatEvery = w.cfg.HeartbeatInterval
	}
	w.publishResources()
	log := w.cfg.Logger.With("worker_id", w.cfg.WorkerID)
	log.Info("worker registered", "pools", w.cfg.Pools, "max_concurrency", w.cfg.Resources.MaxConcurrency, "version", w.cfg.Version)

	var background sync.WaitGroup
	spawn := func(fn func()) {
		background.Add(1)
		go func() {
			defer background.Done()
			fn()
		}()
	}
	spawn(func() { w.heartbeatLoop(ctx) })
	spawn(func() { w.housekeeping(ctx, tenantCache) })
	for _, pool := range w.cfg.Pools {
		spawn(func() { w.consume(ctx, pool) })
		spawn(func() { w.claim(ctx, pool) })
	}
	<-ctx.Done()
	background.Wait()
	log.Info("worker draining", "running", w.runningCount())
	w.drain()
	log.Info("worker stopped")
	return nil
}

// drain waits for running nodes, then cancels them, then gives their reports a
// moment to reach the daemon.
func (w *worker) drain() {
	done := make(chan struct{})
	go func() {
		w.tasks.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(w.cfg.ShutdownGrace):
	}
	w.execCancel(errShutdown)
	select {
	case <-done:
		return
	case <-time.After(reportGrace):
	}
	w.reportStop()
	select {
	case <-done:
	case <-time.After(reportGrace):
		// A node is ignoring its context. Exiting ends it, and its lease expires.
		w.cfg.Logger.Error("worker stopping with a node still running", "running", w.runningCount())
	}
}

// register retries while the daemon is unreachable: Compose starts everything at
// once. A refused token or another protocol version fails at once.
func (w *worker) register(ctx context.Context) (workerproto.RegisterResponse, error) {
	backoff := time.Second
	for {
		resp, err := w.cfg.Coordinator.Register(ctx, workerproto.RegisterRequest{
			WorkerID:  w.cfg.WorkerID,
			Pools:     w.cfg.Pools,
			Resources: w.cfg.Resources,
			Version:   w.cfg.Version,
		})
		if err == nil {
			if resp.ProtocolVersion != 0 && resp.ProtocolVersion != workerproto.ProtocolVersion {
				return resp, fmt.Errorf("workers: the daemon speaks worker protocol %d, this worker speaks %d", resp.ProtocolVersion, workerproto.ProtocolVersion)
			}
			return resp, nil
		}
		if ctx.Err() != nil {
			return resp, ctx.Err()
		}
		switch errs.CodeOf(err) {
		case errs.CodeUnauthenticated, errs.CodeForbidden:
			return resp, fmt.Errorf("workers: the daemon refused this worker: %w", err)
		}
		w.cfg.Logger.Warn("waiting for the daemon", "error", errs.Public(err).Message)
		if !sleep(ctx, backoff) {
			return resp, ctx.Err()
		}
		backoff = min(backoff*2, w.cfg.RetryMax)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *worker) publishResources() {
	r := w.cfg.Resources
	labels := telemetry.Labels{"worker": w.cfg.WorkerID}
	w.cfg.Metrics.Set(telemetry.WorkerCPU, float64(r.CPUCores), labels)
	w.cfg.Metrics.Set(telemetry.WorkerMemory, float64(r.MemoryMB<<20), labels)
	gpu := 0.0
	if r.GPU {
		gpu = 1
	}
	w.cfg.Metrics.Set(telemetry.WorkerGPU, gpu, labels)
}

// heartbeatLoop keeps the worker registered. An outage only logs: running tasks are
// unaffected, and the daemon re-learns this worker on its next heartbeat.
func (w *worker) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(w.heartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		disk := freeDiskMB(w.cfg.ScratchDir)
		w.cfg.Metrics.Set(telemetry.WorkerDisk, float64(disk<<20), telemetry.Labels{"worker": w.cfg.WorkerID})
		err := w.cfg.Coordinator.Heartbeat(ctx, workerproto.HeartbeatRequest{
			WorkerID: w.cfg.WorkerID,
			Running:  w.runningCount(),
			DiskMB:   disk,
		})
		if err != nil && ctx.Err() == nil {
			w.cfg.Logger.Warn("worker heartbeat failed", "error", errs.Public(err).Message)
		}
	}
}

// housekeeping sweeps the cache and reaps idle scratch.
func (w *worker) housekeeping(ctx context.Context, tenantCache *cache.Cache) {
	ticker := time.NewTicker(w.cfg.Housekeeping)
	defer ticker.Stop()
	for {
		w.sweep(tenantCache)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *worker) sweep(tenantCache *cache.Cache) {
	if err := tenantCache.Sweep(); err != nil {
		w.cfg.Logger.Warn("cache sweep failed", "error", err)
	}
	removed, err := reapIdleScratch(w.cfg.ScratchDir, w.cfg.ScratchMaxIdle, w.cfg.Now(), w.jobRunning)
	if err != nil {
		w.cfg.Logger.Warn("scratch reap failed", "error", err)
	}
	if removed > 0 {
		w.cfg.Logger.Info("removed idle job scratch", "jobs", removed)
	}
}

func (w *worker) jobRunning(job id.ULID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, running := range w.running {
		if running == job {
			return true
		}
	}
	return false
}

func (w *worker) runningCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.running)
}

func (w *worker) acquire(ctx context.Context) bool {
	select {
	case w.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *worker) release() { <-w.slots }

// consume reads new entries one at a time, each holding a slot, so a busy worker
// leaves work in the stream for others.
func (w *worker) consume(ctx context.Context, pool node.Pool) {
	backoff := time.Second
	for {
		if !w.acquire(ctx) {
			return
		}
		deliveries, err := w.cfg.Queue.Read(ctx, jobqueue.ReadOptions{
			Pool:     pool,
			Consumer: w.cfg.WorkerID,
			Count:    1,
			Block:    w.cfg.ReadBlock,
		})
		if err != nil {
			w.release()
			if ctx.Err() != nil {
				return
			}
			w.cfg.Logger.Warn("queue read failed", "pool", pool, "error", err)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, w.cfg.RetryMax)
			continue
		}
		backoff = time.Second
		if len(deliveries) == 0 {
			w.release()
			if ctx.Err() != nil {
				return
			}
			continue
		}
		w.start(deliveries[0], pool)
		for _, extra := range deliveries[1:] {
			if !w.acquire(ctx) {
				return
			}
			w.start(extra, pool)
		}
	}
}

// claimBatch bounds one claim. It is a batch so that the entries of long tasks, which
// come back every ClaimMinIdle and are skipped here, cannot hide a dead consumer's
// entry behind them.
const claimBatch = 16

// claim takes over entries of dead consumers. An entry of a task that runs elsewhere
// costs one refused lease; one that runs here is skipped.
func (w *worker) claim(ctx context.Context, pool node.Pool) {
	ticker := time.NewTicker(w.cfg.ClaimInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if w.runningCount() >= cap(w.slots) {
			continue
		}
		deliveries, err := w.cfg.Queue.ClaimStale(ctx, pool, w.cfg.WorkerID, w.cfg.ClaimMinIdle, claimBatch)
		if err != nil {
			if ctx.Err() == nil {
				w.cfg.Logger.Warn("queue claim failed", "pool", pool, "error", err)
			}
			continue
		}
		for _, d := range deliveries {
			if w.isRunning(d.Pointer.TaskID) {
				continue
			}
			// A blocking acquire: the consumer holds a slot during each blocking read, and
			// channel waiters are served in order, so this gets the next free slot.
			if !w.acquire(ctx) {
				return
			}
			w.start(d, pool)
		}
	}
}

func (w *worker) isRunning(taskID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.running[taskID]
	return ok
}

// start runs a delivery in its own goroutine, holding the slot already acquired.
func (w *worker) start(d jobqueue.Delivery, pool node.Pool) {
	job, _, _, err := jobqueue.ParseTaskID(d.Pointer.TaskID)
	if err != nil {
		w.release()
		w.cfg.Logger.Warn("dropping a pointer with an invalid task ID", "pool", pool)
		w.ack(pool, d)
		return
	}
	w.mu.Lock()
	if _, already := w.running[d.Pointer.TaskID]; already {
		w.mu.Unlock()
		w.release()
		return
	}
	w.running[d.Pointer.TaskID] = job
	w.mu.Unlock()
	w.tasks.Add(1)
	go func() {
		defer w.tasks.Done()
		defer w.release()
		defer func() {
			w.mu.Lock()
			delete(w.running, d.Pointer.TaskID)
			w.mu.Unlock()
		}()
		w.handle(d, pool)
	}()
}

// ack acknowledges an entry, retrying briefly. A lost ack is harmless: the entry
// comes back, and its lease answers 410.
func (w *worker) ack(pool node.Pool, d jobqueue.Delivery) {
	for attempt := range 3 {
		err := w.cfg.Queue.Ack(w.reportCtx, pool, d.StreamID, d.Pointer.TaskID)
		if err == nil {
			return
		}
		if w.reportCtx.Err() != nil {
			return
		}
		w.cfg.Logger.Warn("queue ack failed", "task_id", d.Pointer.TaskID, "attempt", attempt+1, "error", err)
		if !sleep(w.reportCtx, time.Second) {
			return
		}
	}
}
