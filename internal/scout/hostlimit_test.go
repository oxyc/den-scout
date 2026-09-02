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
	l := newHostLimiter(20*time.Millisecond, 2)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.wait(context.Background(), "h") }()
	}
	wg.Wait()
	// Two pass free and the rest are paced, so this takes at least one interval — where a limiter the
	// callers raced through would take none. The total is bounded by the debt floor rather than by the
	// caller count, which is that floor's whole purpose and has its own test.
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("concurrent callers bypassed the limiter: %v", elapsed)
	}
}

// The refill arithmetic itself, asserted exactly rather than through the clock.
//
// Every timing test here runs in one burst with no waiting, so deleting the refill line entirely leaves
// them all passing — a limiter that never refills is only ever SLOWER, and the bounds are one-sided.
// This drives the bucket directly instead.
func TestBucket_refillsByElapsedTime(t *testing.T) {
	b := &bucket{tokens: 0, last: time.Now().Add(-500 * time.Millisecond)}
	// 500ms elapsed at 100ms per token = 5 tokens, so the next take is free and 4 remain.
	if delay := b.take(100*time.Millisecond, 10); delay != 0 {
		t.Errorf("half a second should have refilled five tokens, waited %v", delay)
	}
	if b.tokens < 3.9 || b.tokens > 4.1 {
		t.Errorf("refill arithmetic: %.2f tokens, want ~4", b.tokens)
	}

	// The cap is applied before the spend, so an idle bucket cannot bank more than one burst.
	idle := &bucket{tokens: 0, last: time.Now().Add(-time.Hour)}
	idle.take(time.Millisecond, 3)
	if idle.tokens > 2.001 {
		t.Errorf("an idle bucket banked past its burst: %.2f", idle.tokens)
	}
}

// The configured rate, asserted as a rate. The timing tests accept a wide band; this pins the maths.
func TestBucket_delayMatchesTheConfiguredRate(t *testing.T) {
	// Burst 8 so the debt floor does not bind — that behaviour has its own test.
	b := &bucket{tokens: 0, last: time.Now()}
	b.take(time.Second, 8) // spends to -1
	delay := b.take(time.Second, 8)
	// Now at -2, so the caller waits two intervals.
	if delay < 1900*time.Millisecond || delay > 2100*time.Millisecond {
		t.Errorf("delay %v does not match a 1s interval at two tokens of debt", delay)
	}
}

// Debt is floored, or a stampede queues the tail arbitrarily far past the caller's own deadline.
func TestBucket_debtIsFloored(t *testing.T) {
	b := &bucket{tokens: 0, last: time.Now()}
	var worst time.Duration
	for i := 0; i < 100; i++ {
		if d := b.take(10*time.Millisecond, 4); d > worst {
			worst = d
		}
	}
	if b.tokens < -4.001 {
		t.Errorf("debt ran past the floor: %.2f", b.tokens)
	}
	if worst > 50*time.Millisecond {
		t.Errorf("a stampede queued the tail %v out", worst)
	}
}

// An abandoned wait returns its token. A request that was never sent must not charge the next caller —
// the debt decays at a rate comparable to the scrape budget, so it carries across requests.
func TestHostLimiter_cancellationRefundsTheToken(t *testing.T) {
	l := newHostLimiter(10*time.Second, 1)
	l.wait(context.Background(), "h") // spend the only token
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	l.wait(ctx, "h") // abandoned — must give the token back

	l.mu.Lock()
	b := l.buckets["h"]
	l.mu.Unlock()
	b.mu.Lock()
	tokens := b.tokens
	b.mu.Unlock()
	if tokens < -0.001 {
		t.Errorf("an abandoned wait kept its token: %.2f", tokens)
	}
}

// A nil limiter and an empty host are no-ops, so a test double or an unparsed URL never blocks.
func TestHostLimiter_degradesToNoOp(t *testing.T) {
	var nilLimiter *hostLimiter
	nilLimiter.wait(context.Background(), "h")
	newHostLimiter(time.Hour, 0).wait(context.Background(), "")
}
