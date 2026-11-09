// Package keylock is an in-process lock per string key, waiting with a context.
// One daemon owns a data directory (I137), so a process-local lock is enough for
// idempotency keys and upload chunks.
package keylock

import (
	"context"
	"sync"
)

// Locks is a set of locks by key. Its zero value is not usable; call New.
type Locks struct {
	mu   sync.Mutex
	held map[string]*entry
}

type entry struct {
	ch   chan struct{}
	refs int
}

// New returns an empty set.
func New() *Locks { return &Locks{held: map[string]*entry{}} }

// Lock takes the key's lock, or returns ctx's error when ctx ends first. The
// returned function releases it. Entries exist only while held or awaited.
func (l *Locks) Lock(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	e, ok := l.held[key]
	if !ok {
		e = &entry{ch: make(chan struct{}, 1)}
		l.held[key] = e
	}
	e.refs++
	l.mu.Unlock()

	select {
	case e.ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-e.ch
				l.release(key, e)
			})
		}, nil
	case <-ctx.Done():
		l.release(key, e)
		return nil, ctx.Err()
	}
}

func (l *Locks) release(key string, e *entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(l.held, key)
	}
}
