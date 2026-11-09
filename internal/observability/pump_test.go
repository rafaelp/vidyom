package observability

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fastTuning() Tuning {
	return Tuning{
		QueueSize:      8,
		BatchSize:      4,
		FlushInterval:  5 * time.Millisecond,
		WriteTimeout:   50 * time.Millisecond,
		OpenTimeout:    50 * time.Millisecond,
		OpenRetry:      5 * time.Millisecond,
		BackoffBase:    time.Millisecond,
		BackoffMax:     2 * time.Millisecond,
		MaxAttempts:    3,
		DrainTimeout:   100 * time.Millisecond,
		FailureEvery:   time.Hour,
		HealthInterval: 5 * time.Millisecond,
		RingSize:       16,
	}
}

type intSink struct {
	mu      sync.Mutex
	got     []int
	writes  int
	failing atomic.Bool
	panics  atomic.Bool
	block   chan struct{}
}

func (s *intSink) write(ctx context.Context, batch []int) error {
	if s.panics.Load() {
		panic("boom")
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.failing.Load() {
		return errors.New("sink is down, password=hunter2")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, batch...)
	s.writes++
	return nil
}

func (s *intSink) snapshot() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.got...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPumpDeliversInOrderAndFlushes(t *testing.T) {
	s := &intSink{}
	flushed := atomic.Int32{}
	tuning := fastTuning()
	tuning.QueueSize = 100
	p := newPump(pumpFuncs[int]{write: s.write, flush: func(context.Context) error { flushed.Add(1); return nil }}, tuning, nil, nil)
	p.start()
	for i := range 50 {
		p.push(i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.flushCtx(ctx); err != nil {
		t.Fatal(err)
	}
	got := s.snapshot()
	if len(got) != 50 {
		t.Fatalf("got %d items", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("out of order at %d: %v", i, got)
		}
	}
	if flushed.Load() == 0 {
		t.Error("flush did not reach the sink")
	}
	if err := p.stop(ctx); err != nil {
		t.Fatal(err)
	}
	st := p.stats()
	if st.Written != 50 || st.Backlog != 0 || st.Dropped != 0 || !st.Opened || !st.Stopped {
		t.Errorf("stats: %+v", st)
	}
}

func TestPumpDropsOldestWhenFull(t *testing.T) {
	s := &intSink{block: make(chan struct{})}
	tuning := fastTuning()
	tuning.WriteTimeout = time.Hour
	p := newPump(pumpFuncs[int]{write: s.write}, tuning, nil, nil)
	p.start()
	p.push(0)
	eventually(t, "the first write to start", func() bool { return p.inflight.Load() == 1 })

	start := time.Now()
	for i := 1; i <= 100; i++ {
		p.push(i) // never blocks, though the sink is hung
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("push blocked for %v", elapsed)
	}
	st := p.stats()
	if st.Dropped != 100-8 {
		t.Errorf("dropped %d, want %d", st.Dropped, 100-8)
	}
	close(s.block)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.flushCtx(ctx); err != nil {
		t.Fatal(err)
	}
	got := s.snapshot()
	if len(got) != 9 || got[0] != 0 || got[1] != 93 || got[8] != 100 {
		t.Errorf("expected the first in flight and the newest 8, got %v", got)
	}
	_ = p.stop(ctx)
}

func TestPumpRetriesThenDropsAndRecovers(t *testing.T) {
	s := &intSink{}
	s.failing.Store(true)
	var failures, recoveries atomic.Int32
	var lastMessage atomic.Value
	p := newPump(pumpFuncs[int]{write: s.write}, fastTuning(), nil, func(m string) string { return "sanitized:" + m })
	p.onFailure = func(m string) { failures.Add(1); lastMessage.Store(m) }
	p.onRecover = func() { recoveries.Add(1) }
	p.start()
	p.push(1)
	eventually(t, "the batch to be dropped after its attempts", func() bool { return p.stats().Dropped == 1 })
	st := p.stats()
	if st.Failures != 3 {
		t.Errorf("failures %d, want 3 attempts", st.Failures)
	}
	if !st.Failing || st.LastError == "" {
		t.Errorf("stats should show failure: %+v", st)
	}
	if failures.Load() != 1 {
		t.Errorf("failure record count %d, want 1 per transition", failures.Load())
	}
	if lastMessage.Load() != "sanitized:sink is down, password=hunter2" || st.LastError != lastMessage.Load() {
		t.Errorf("error must pass through sanitize: %v %q", lastMessage.Load(), st.LastError)
	}

	s.failing.Store(false)
	p.push(2)
	eventually(t, "recovery", func() bool { return recoveries.Load() == 1 })
	if got := s.snapshot(); len(got) != 1 || got[0] != 2 {
		t.Errorf("got %v", got)
	}
	if p.stats().Failing {
		t.Error("should no longer be failing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.stop(ctx)
}

func TestPumpBufferedItemsReachARecoveredSink(t *testing.T) {
	s := &intSink{}
	s.failing.Store(true)
	tuning := fastTuning()
	tuning.MaxAttempts = 1000 // survive the outage
	tuning.QueueSize = 100
	p := newPump(pumpFuncs[int]{write: s.write}, tuning, nil, nil)
	p.start()
	for i := range 10 {
		p.push(i)
	}
	eventually(t, "a failed attempt", func() bool { return p.stats().Failures > 0 })
	if len(s.snapshot()) != 0 {
		t.Fatal("nothing should have been written")
	}
	s.failing.Store(false)
	eventually(t, "the buffered items", func() bool { return len(s.snapshot()) == 10 })
	if p.stats().Dropped != 0 {
		t.Errorf("nothing should have been dropped: %+v", p.stats())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.stop(ctx)
}

func TestPumpSurvivesPanic(t *testing.T) {
	s := &intSink{}
	s.panics.Store(true)
	p := newPump(pumpFuncs[int]{write: s.write}, fastTuning(), nil, nil)
	p.start()
	p.push(1)
	eventually(t, "the panic to count as failure", func() bool { return p.stats().Failures > 0 })
	s.panics.Store(false)
	p.push(2)
	eventually(t, "the sink to work again", func() bool { return len(s.snapshot()) >= 1 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.stop(ctx); err != nil {
		t.Error(err)
	}
}

func TestPumpOpenFailureIsRetriedAndQueueKeeps(t *testing.T) {
	s := &intSink{}
	var opens atomic.Int32
	p := newPump(pumpFuncs[int]{
		write: s.write,
		open: func(context.Context) error {
			if opens.Add(1) < 3 {
				return errors.New("no route")
			}
			return nil
		},
	}, fastTuning(), nil, nil)
	p.start()
	p.push(1)
	eventually(t, "the third open", func() bool { return opens.Load() >= 3 })
	eventually(t, "the queued item", func() bool { return len(s.snapshot()) == 1 })
	if !p.stats().Opened {
		t.Error("should be open")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.stop(ctx)
}

func TestPumpHungOpenDoesNotBlockAndStopGivesUp(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	tuning := fastTuning()
	tuning.OpenTimeout = time.Hour
	p := newPump(pumpFuncs[int]{
		write: func(context.Context, []int) error { return nil },
		open: func(context.Context) error {
			<-release // ignores its context
			return nil
		},
	}, tuning, nil, nil)
	p.start()
	start := time.Now()
	p.push(1)
	if time.Since(start) > time.Second {
		t.Error("push blocked")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("stop should give up at its deadline, got %v", err)
	}
	if err := p.flushCtx(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("flush should give up too, got %v", err)
	}
}

func TestPumpStopDrainsAndClosesOnce(t *testing.T) {
	s := &intSink{}
	var closes atomic.Int32
	tuning := fastTuning()
	tuning.FlushInterval = time.Hour // only the drain can move these
	tuning.QueueSize = 100
	p := newPump(pumpFuncs[int]{write: s.write, close: func(context.Context) error { closes.Add(1); return nil }}, tuning, nil, nil)
	p.start()
	for i := range 3 {
		p.push(i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.snapshot()) != 3 {
		t.Errorf("drain lost items: %v", s.snapshot())
	}
	if closes.Load() != 1 {
		t.Errorf("close ran %d times", closes.Load())
	}
	if err := p.stop(ctx); err != nil {
		t.Errorf("second stop: %v", err)
	}
	p.push(9)
	if p.stats().Dropped != 1 {
		t.Error("a push after stop must count as dropped")
	}
}

func TestPumpDirectWrite(t *testing.T) {
	s := &intSink{}
	p := newPump(pumpFuncs[int]{write: s.write}, fastTuning(), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.direct(ctx, []int{1}); err == nil {
		t.Error("a sink that is not open must refuse")
	}
	p.start()
	eventually(t, "open", func() bool { return p.opened.Load() })
	if err := p.direct(ctx, []int{7}); err != nil {
		t.Fatal(err)
	}
	if got := s.snapshot(); len(got) != 1 || got[0] != 7 {
		t.Errorf("got %v", got)
	}
	_ = p.stop(ctx)
}

func TestPumpStuckIsReported(t *testing.T) {
	s := &intSink{block: make(chan struct{})}
	defer close(s.block)
	tuning := fastTuning()
	tuning.WriteTimeout = time.Millisecond
	release := make(chan struct{})
	p := newPump(pumpFuncs[int]{write: func(context.Context, []int) error { <-release; return nil }}, tuning, nil, nil)
	p.start()
	p.push(1)
	eventually(t, "the write to be seen as stuck", func() bool { return p.stats().Stuck })
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.stop(ctx)
}
