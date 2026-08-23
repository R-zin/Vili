package ratelimit

import (
	"testing"
	"time"
)

func TestAllowBurstThenRefuse(t *testing.T) {
	l := New(1, 3) // 1 rps, burst 3
	defer l.Stop()

	for i := 0; i < 3; i++ {
		if !l.Allow("ip") {
			t.Fatalf("request %d within burst should be allowed", i+1)
		}
	}
	if l.Allow("ip") {
		t.Fatal("fourth request should exceed the burst and be refused")
	}
	// A different key has its own full bucket.
	if !l.Allow("other") {
		t.Fatal("a different key should have an independent bucket")
	}
}

func TestRefill(t *testing.T) {
	l := New(100, 1) // refills fast
	defer l.Stop()
	if !l.Allow("k") {
		t.Fatal("first request should be allowed")
	}
	if l.Allow("k") {
		t.Fatal("second immediate request should be refused (burst 1)")
	}
	time.Sleep(20 * time.Millisecond) // ~2 tokens refill at 100 rps
	if !l.Allow("k") {
		t.Fatal("request after refill window should be allowed")
	}
}

func TestRetryAfter(t *testing.T) {
	l := New(1, 1) // 1 rps, burst 1
	defer l.Stop()
	if !l.Allow("k") {
		t.Fatal("first request allowed")
	}
	// Exhausted: an immediate retry needs roughly up to 1/rps.
	d := l.RetryAfter("k")
	if d <= 0 {
		t.Fatalf("expected a positive RetryAfter for an exhausted bucket, got %s", d)
	}
	if d > 2*time.Second {
		t.Fatalf("RetryAfter unreasonably large: %s", d)
	}
}

func TestEvictIdle(t *testing.T) {
	l := New(1, 1)
	defer l.Stop()
	base := time.Now()
	l.now = func() time.Time { return base }

	l.Allow("stale")
	l.Allow("fresh")
	// Advance the fresh key's last-seen but let the stale one age out.
	l.buckets["fresh"].last = base.Add(ttl + time.Minute)
	l.buckets["stale"].last = base

	l.evictIdle(base.Add(ttl + 2*time.Minute))

	if l.size() != 1 {
		t.Fatalf("expected 1 live bucket after eviction, got %d", l.size())
	}
}

func TestStopIdempotent(t *testing.T) {
	l := New(1, 1)
	l.Stop()
	l.Stop() // must not panic
}
