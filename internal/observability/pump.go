package observability

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Tuning holds the numbers of I273. A zero field takes its default.
type Tuning struct {
	QueueSize      int
	BatchSize      int
	FlushInterval  time.Duration
	WriteTimeout   time.Duration
	OpenTimeout    time.Duration
	OpenRetry      time.Duration
	BackoffBase    time.Duration
	BackoffMax     time.Duration
	MaxAttempts    int
	DrainTimeout   time.Duration
	FailureEvery   time.Duration
	HealthInterval time.Duration
	RingSize       int
}

func (t Tuning) withDefaults() Tuning {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	if t.QueueSize <= 0 {
		t.QueueSize = 10000
	}
	if t.BatchSize <= 0 {
		t.BatchSize = 256
	}
	if t.MaxAttempts <= 0 {
		t.MaxAttempts = 5
	}
	if t.RingSize <= 0 {
		t.RingSize = 4096
	}
	def(&t.FlushInterval, time.Second)
	def(&t.WriteTimeout, 10*time.Second)
	def(&t.OpenTimeout, 10*time.Second)
	def(&t.OpenRetry, 30*time.Second)
	def(&t.BackoffBase, 500*time.Millisecond)
	def(&t.BackoffMax, 30*time.Second)
	def(&t.DrainTimeout, 5*time.Second)
	def(&t.FailureEvery, time.Minute)
	def(&t.HealthInterval, 5*time.Second)
	return t
}

// pumpFuncs is what one sink instance does. open runs on the pump's goroutine, so a
// sink that hangs while opening delays only itself.
type pumpFuncs[T any] struct {
	open  func(ctx context.Context) error
	write func(ctx context.Context, batch []T) error
	flush func(ctx context.Context) error
	close func(ctx context.Context) error
}

// pumpStats is a snapshot of one sink's health.
type pumpStats struct {
	Opened      bool
	Stopped     bool
	Backlog     int64
	Dropped     int64
	Failures    int64
	Written     int64
	Failing     bool
	LastError   string
	LastErrorAt time.Time
	LastSuccess time.Time
	Delay       time.Duration
	Stuck       bool
}

type pumpEntry[T any] struct {
	item T
	at   time.Time
}

// pump is the isolation unit of §7.4 and §10.1: one bounded queue and one goroutine
// per sink instance. push never blocks. Nothing a sink does, including hanging
// forever or panicking, reaches the caller or another sink.
type pump[T any] struct {
	funcs    pumpFuncs[T]
	tuning   Tuning
	now      func() time.Time
	sanitize func(string) string
	// onFailure runs on the transition to failing and then at most once per
	// Tuning.FailureEvery while failing. onRecover runs when it works again.
	onFailure func(message string)
	onRecover func()

	mu      sync.Mutex
	buf     []pumpEntry[T]
	head    int
	size    int
	stopped bool

	wake    chan struct{}
	flushed chan struct{}
	stopCh  chan struct{}
	done    chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	writeMu sync.Mutex // serializes the pump with a direct test write

	opened      atomic.Bool
	flushing    atomic.Bool
	dropped     atomic.Int64
	failures    atomic.Int64
	written     atomic.Int64
	inflight    atomic.Int64
	writeStart  atomic.Int64 // unix nanos of the write in progress, or 0
	lastDelayNS atomic.Int64

	statMu        sync.Mutex
	failing       bool
	lastError     string
	lastErrorAt   time.Time
	lastSuccess   time.Time
	lastFailEvent time.Time
}

func newPump[T any](funcs pumpFuncs[T], tuning Tuning, now func() time.Time, sanitize func(string) string) *pump[T] {
	tuning = tuning.withDefaults()
	if now == nil {
		now = time.Now
	}
	if sanitize == nil {
		sanitize = func(s string) string { return s }
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &pump[T]{
		funcs:    funcs,
		tuning:   tuning,
		now:      now,
		sanitize: sanitize,
		buf:      make([]pumpEntry[T], tuning.QueueSize),
		wake:     make(chan struct{}, 1),
		flushed:  make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
}

func (p *pump[T]) start() { go p.run() }

// push queues an item. A full queue drops the oldest entry (I273). It never blocks.
func (p *pump[T]) push(item T) {
	entry := pumpEntry[T]{item: item, at: p.now()}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		p.dropped.Add(1)
		return
	}
	if p.size == len(p.buf) {
		p.buf[p.head] = pumpEntry[T]{}
		p.head = (p.head + 1) % len(p.buf)
		p.size--
		p.dropped.Add(1)
	}
	p.buf[(p.head+p.size)%len(p.buf)] = entry
	p.size++
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *pump[T]) take(max int) []pumpEntry[T] {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := min(p.size, max)
	if n == 0 {
		return nil
	}
	out := make([]pumpEntry[T], n)
	for i := range n {
		idx := (p.head + i) % len(p.buf)
		out[i] = p.buf[idx]
		p.buf[idx] = pumpEntry[T]{}
	}
	p.head = (p.head + n) % len(p.buf)
	p.size -= n
	p.inflight.Store(int64(n)) // under the lock, so a flush never sees a taken batch as gone
	return out
}

// idle reports that nothing is queued or being delivered.
func (p *pump[T]) idle() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size == 0 && p.inflight.Load() == 0
}

func (p *pump[T]) backlog() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size
}

func (p *pump[T]) isStopping() bool {
	select {
	case <-p.stopCh:
		return true
	default:
		return false
	}
}

func (p *pump[T]) run() {
	defer close(p.done)
	if !p.openLoop() {
		p.drop(p.take(len(p.buf)))
		p.inflight.Store(0)
		return
	}
	for {
		batch := p.nextBatch()
		if batch == nil {
			break
		}
		p.deliver(batch)
	}
	if p.funcs.flush != nil {
		_ = p.call(p.tuning.WriteTimeout, p.funcs.flush)
	}
	if p.funcs.close != nil {
		_ = p.call(p.tuning.WriteTimeout, p.funcs.close)
	}
}

func (p *pump[T]) openLoop() bool {
	if p.funcs.open == nil {
		p.opened.Store(true)
		return true
	}
	for {
		err := p.call(p.tuning.OpenTimeout, p.funcs.open)
		if err == nil {
			p.opened.Store(true)
			return true
		}
		p.recordFailure(fmt.Errorf("open: %w", err))
		select {
		case <-time.After(p.tuning.OpenRetry):
		case <-p.stopCh:
			return false
		}
	}
}

// nextBatch waits for entries, then gives a batch time to fill unless a flush or
// shutdown wants them now. It returns nil only when stopping with an empty queue.
func (p *pump[T]) nextBatch() []pumpEntry[T] {
	for p.backlog() == 0 {
		if p.isStopping() {
			return nil
		}
		select {
		case <-p.wake:
		case <-p.stopCh:
		case <-p.flushed:
		}
	}
	timer := time.NewTimer(p.tuning.FlushInterval)
	defer timer.Stop()
wait:
	for p.backlog() < p.tuning.BatchSize && !p.flushing.Load() && !p.isStopping() {
		select {
		case <-timer.C:
			break wait
		case <-p.wake:
		case <-p.flushed:
		case <-p.stopCh:
		}
	}
	return p.take(p.tuning.BatchSize)
}

func (p *pump[T]) deliver(batch []pumpEntry[T]) {
	items := make([]T, len(batch))
	for i := range batch {
		items[i] = batch[i].item
	}
	backoff := p.tuning.BackoffBase
	defer p.inflight.Store(0)
	for attempt := 1; ; attempt++ {
		err := p.writeBatch(items)
		if err == nil {
			p.written.Add(int64(len(batch)))
			p.lastDelayNS.Store(int64(p.now().Sub(batch[0].at)))
			p.recordSuccess()
			return
		}
		p.recordFailure(err)
		if attempt >= p.tuning.MaxAttempts || p.isStopping() {
			p.drop(batch)
			return
		}
		select {
		case <-time.After(backoff):
		case <-p.stopCh:
			p.drop(batch)
			return
		}
		backoff = min(backoff*2, p.tuning.BackoffMax)
	}
}

func (p *pump[T]) drop(batch []pumpEntry[T]) { p.dropped.Add(int64(len(batch))) }

func (p *pump[T]) writeBatch(items []T) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.writeStart.Store(p.now().UnixNano())
	defer p.writeStart.Store(0)
	return p.call(p.tuning.WriteTimeout, func(ctx context.Context) error { return p.funcs.write(ctx, items) })
}

// call runs fn with a timeout and turns a panic into an error.
func (p *pump[T]) call(timeout time.Duration, fn func(ctx context.Context) error) (err error) {
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("sink panicked: %v", r)
		}
	}()
	return fn(ctx)
}

func (p *pump[T]) recordFailure(err error) {
	p.failures.Add(1)
	message := p.sanitize(err.Error())
	now := p.now()
	p.statMu.Lock()
	transition := !p.failing
	p.failing = true
	p.lastError = message
	p.lastErrorAt = now
	notify := transition || now.Sub(p.lastFailEvent) >= p.tuning.FailureEvery
	if notify {
		p.lastFailEvent = now
	}
	p.statMu.Unlock()
	if notify && p.onFailure != nil {
		p.onFailure(message)
	}
}

func (p *pump[T]) recordSuccess() {
	p.statMu.Lock()
	wasFailing := p.failing
	p.failing = false
	p.lastSuccess = p.now()
	p.statMu.Unlock()
	if wasFailing && p.onRecover != nil {
		p.onRecover()
	}
}

// stats returns a snapshot.
func (p *pump[T]) stats() pumpStats {
	p.mu.Lock()
	backlog := int64(p.size)
	stopped := p.stopped
	p.mu.Unlock()
	p.statMu.Lock()
	defer p.statMu.Unlock()
	stuck := false
	if start := p.writeStart.Load(); start != 0 {
		stuck = p.now().UnixNano()-start > int64(2*p.tuning.WriteTimeout)
	}
	return pumpStats{
		Opened:      p.opened.Load(),
		Stopped:     stopped,
		Backlog:     backlog + p.inflight.Load(),
		Dropped:     p.dropped.Load(),
		Failures:    p.failures.Load(),
		Written:     p.written.Load(),
		Failing:     p.failing,
		LastError:   p.lastError,
		LastErrorAt: p.lastErrorAt,
		LastSuccess: p.lastSuccess,
		Delay:       time.Duration(p.lastDelayNS.Load()),
		Stuck:       stuck,
	}
}

// flushCtx waits until the queue is drained and written, then flushes the sink. It
// gives up when ctx ends, which a hung sink will make it do.
func (p *pump[T]) flushCtx(ctx context.Context) error {
	p.flushing.Store(true)
	defer p.flushing.Store(false)
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case p.flushed <- struct{}{}:
		default:
		}
		if p.idle() {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			return nil
		case <-tick.C:
		}
	}
	if p.funcs.flush == nil || !p.opened.Load() {
		return nil
	}
	if !p.lockWrite(ctx) {
		return ctx.Err()
	}
	defer p.writeMu.Unlock()
	return p.call(p.tuning.WriteTimeout, p.funcs.flush)
}

func (p *pump[T]) lockWrite(ctx context.Context) bool {
	for !p.writeMu.TryLock() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Millisecond):
		}
	}
	return true
}

// direct writes items now, bypassing the queue, serialized with the pump's writes.
// It is what `observability test` uses.
func (p *pump[T]) direct(ctx context.Context, items []T) error {
	if !p.opened.Load() {
		return fmt.Errorf("sink is not open: %s", p.stats().LastError)
	}
	if !p.lockWrite(ctx) {
		return ctx.Err()
	}
	defer p.writeMu.Unlock()
	return p.call(p.tuning.WriteTimeout, func(c context.Context) error {
		c, cancel := context.WithCancel(c)
		defer cancel()
		defer context.AfterFunc(ctx, cancel)()
		return p.funcs.write(c, items)
	})
}

// stop asks the goroutine to drain and close, waiting at most until ctx ends. A sink
// that ignores its context is abandoned and its goroutine stays until it returns.
func (p *pump[T]) stop(ctx context.Context) error {
	p.mu.Lock()
	already := p.stopped
	p.stopped = true
	p.mu.Unlock()
	if !already {
		close(p.stopCh)
	}
	select {
	case <-p.done:
		p.cancel()
		return nil
	case <-ctx.Done():
		p.cancel()
		return ctx.Err()
	}
}
