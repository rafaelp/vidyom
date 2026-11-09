package apiserver

import (
	"net"
	"sync"
	"time"
)

// ipLimiter is a token bucket per remote IP, held in memory (I127).
type ipLimiter struct {
	burst   float64
	perSec  float64
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

const maxLimiterEntries = 10_000

func newIPLimiter(perWindow int, window time.Duration, now func() time.Time) *ipLimiter {
	return &ipLimiter{
		burst:   float64(perWindow),
		perSec:  float64(perWindow) / window.Seconds(),
		now:     now,
		buckets: map[string]*bucket{},
	}
}

// allow takes one token, or says how long until one exists.
func (l *ipLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.buckets) >= maxLimiterEntries {
		l.prune(now)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSec)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.perSec * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// prune forgets buckets that are full again, then everything if that is not enough,
// which at worst resets the limit under an address flood.
func (l *ipLimiter) prune(now time.Time) {
	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.perSec >= l.burst {
			delete(l.buckets, key)
		}
	}
	if len(l.buckets) >= maxLimiterEntries {
		l.buckets = map[string]*bucket{}
	}
}

// remoteKey names the caller for the limiter: the IP, or the transport when there
// is no address, as on a Unix socket.
func (c *call) remoteKey() string {
	if c.conn.Transport == "unix" {
		return "unix"
	}
	if c.conn.RemoteAddr != nil {
		if host, _, err := net.SplitHostPort(c.conn.RemoteAddr.String()); err == nil {
			return host
		}
	}
	if host, _, err := net.SplitHostPort(c.r.RemoteAddr); err == nil {
		return host
	}
	return c.r.RemoteAddr
}
