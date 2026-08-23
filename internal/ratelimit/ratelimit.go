// Package ratelimit provides token-bucket rate limiting keyed by an arbitrary
// string (a client IP or a user id). It wraps golang.org/x/time/rate — the one
// new Phase-5 dependency, justified because a correct, garbage-collected
// per-key limiter is exactly what x/time/rate + a bounded map provides and a
// hand-rolled equivalent buys nothing.
//
// Each key gets its own bucket with a shared refill rate and burst. Buckets are
// evicted after an idle TTL so a request flood from many distinct IPs cannot
// grow the map without bound.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ttl is how long a bucket may sit idle (no requests) before it is evicted on
// the next sweep. It is several burst-refill windows, so a legitimately
// rate-limited client never loses its bucket mid-backoff, while one-shot
// attackers are reclaimed.
const ttl = 10 * time.Minute

// sweepInterval is how often idle buckets are reclaimed.
const sweepInterval = time.Minute

// Limiter hands out per-key token buckets. It is safe for concurrent use.
type Limiter struct {
	rps   rate.Limit
	burst int

	mu      sync.Mutex
	buckets map[string]*entry
	now     func() time.Time // overridable in tests

	stop chan struct{}
}

// entry is one key's bucket plus its last-seen timestamp for eviction.
type entry struct {
	lim  *rate.Limiter
	last time.Time
}

// New builds a Limiter allowing rps requests/second with the given burst,
// and starts a background sweep that reclaims idle buckets. Call Stop to halt
// the sweep goroutine.
func New(rps float64, burst int) *Limiter {
	l := &Limiter{
		rps:     rate.Limit(rps),
		burst:   burst,
		buckets: make(map[string]*entry),
		now:     time.Now,
		stop:    make(chan struct{}),
	}
	go l.sweeper()
	return l
}

// Allow reports whether one request for key is permitted right now, consuming
// a token if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	e, ok := l.buckets[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[key] = e
	}
	e.last = l.now()
	l.mu.Unlock()
	return e.lim.Allow()
}

// RetryAfter is the suggested wait before the next request for key will be
// admitted, derived from a fresh reservation. It returns 0 when a token is
// available now.
func (l *Limiter) RetryAfter(key string) time.Duration {
	l.mu.Lock()
	e, ok := l.buckets[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[key] = e
	}
	e.last = l.now()
	l.mu.Unlock()

	res := e.lim.Reserve()
	if !res.OK() {
		return 0
	}
	d := res.Delay()
	res.Cancel() // advisory only; do not consume the token
	return d
}

// Stop halts the background sweep goroutine. It is idempotent.
func (l *Limiter) Stop() {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
}

// sweeper periodically reclaims buckets that have been idle longer than ttl.
func (l *Limiter) sweeper() {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			l.evictIdle(now)
		}
	}
}

// evictIdle removes buckets unused since now-ttl.
func (l *Limiter) evictIdle(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, e := range l.buckets {
		if now.Sub(e.last) > ttl {
			delete(l.buckets, k)
		}
	}
}

// size reports the number of live buckets; used in tests.
func (l *Limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
