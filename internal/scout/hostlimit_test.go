package scout

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A burst passes straight through. A household glancing down a few episodes must never wait: the limiter
// exists to bound sustained traffic, not to tax normal use.
func TestHostLimiter_burstIsFree(t *testing.T) {
	l := newHostLimiter(50*time.Millisecond, 4)
	start := time.Now()
	for i := 0; i < 4; i++ {
		l.wait(context.Background(), "indexer.example")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("a burst within the allowance waited %v", elapsed)
	}
}

// Past the burst, requests QUEUE rather than being shed. That distinction is the whole point: a shed
// request became "this release does not exist" for an entire evening, and a request that waits is
// invisible next to an eight-second scrape budget.
func TestHostLimiter_pacesBeyondTheBurst(t *testing.T) {
	l := newHostLimiter(30*time.Millisecond, 2)
	start := time.Now()
	for i := 0; i < 5; i++ {
		l.wait(context.Background(), "indexer.example")
	}
	// 2 free, then 3 paced ⇒ at least ~90ms. Generous upper bound: CI timers are not precise.
	elapsed := time.Since(start)
	if elapsed < 60*time.Millisecond {
		t.Errorf("no pacing applied: %v", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("pacing far slower than configured: %v", elapsed)
	}
}

// Per host, so a slow or throttled indexer cannot delay a healthy one.
func TestHostLimiter_isPerHost(t *testing.T) {
	l := newHostLimiter(time.Second, 1)
	l.wait(context.Background(), "slow.example") // spends slow's only token
	start := time.Now()
	l.wait(context.Background(), "fast.example") // must not be held behind it
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("one host's budget delayed another: %v", elapsed)
	}
}

// A cancelled context ends the wait. The caller's deadline governs; the limiter never outlives it, or a
// queued request would go on holding a scrape budget that has already expired.
func TestHostLimiter_respectsCancellation(t *testing.T) {
	l := newHostLimiter(10*time.Second, 1)
	l.wait(context.Background(), "h") // spend the token
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	l.wait(ctx, "h")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited past the caller's deadline: %v", elapsed)
	}
}

// Concurrent callers queue in order rather than all waking to the same free slot — the token is spent
// even when the bucket goes negative, which is what makes the wait fair.
func TestHostLimiter_concurrentCallersQueue(t *testing.T) {
	l := newHostLimiter(20*time.Millisecond, 1)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.wait(context.Background(), "h") }()
	}
	wg.Wait()
	// 1 free + 3 paced at 20ms ⇒ ~60ms if they queue; ~0 if they all raced through.
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("concurrent callers bypassed the limiter: %v", elapsed)
	}
}

// A nil limiter and an empty host are no-ops, so a test double or an unparsed URL never blocks.
func TestHostLimiter_degradesToNoOp(t *testing.T) {
	var nilLimiter *hostLimiter
	nilLimiter.wait(context.Background(), "h")
	newHostLimiter(time.Hour, 0).wait(context.Background(), "")
}
