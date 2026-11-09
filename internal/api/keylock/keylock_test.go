package keylock

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSerializesPerKey(t *testing.T) {
	locks := New()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		running int
		peak    int
	)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := locks.Lock(context.Background(), "k")
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			mu.Lock()
			running++
			if running > peak {
				peak = running
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("%d holders at once", peak)
	}
	if len(locks.held) != 0 {
		t.Fatalf("%d entries left behind", len(locks.held))
	}
}

func TestContextEndsTheWait(t *testing.T) {
	locks := New()
	unlock, err := locks.Lock(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := locks.Lock(ctx, "k"); err == nil {
		t.Fatal("expected the wait to end with the context")
	}
	unlock()
	unlock() // a second call is harmless
	if len(locks.held) != 0 {
		t.Fatalf("%d entries left behind", len(locks.held))
	}
}

func TestDifferentKeysDoNotBlock(t *testing.T) {
	locks := New()
	a, err := locks.Lock(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer a()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	b, err := locks.Lock(ctx, "b")
	if err != nil {
		t.Fatalf("key b blocked by key a: %v", err)
	}
	b()
}
