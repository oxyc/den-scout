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
//
// Asserted on the SPREAD of delays, not on the wall clock. A wall-clock bound measures only the slowest
// caller, so it passed just as happily when every overflow caller was handed one identical delay — which
// is a synchronised herd, the precise opposite of a queue, and was a real shipped behaviour.
func TestHostLimiter_concurrentCallersQueue(t *testing.T) {
	b := &bucket{tokens: 2, last: time.Now()}
	var delays []time.Duration
	for i := 0; i < 6; i++ {
		delays = append(delays, b.take(20*time.Millisecond, 2))
	}
	if delays[0] != 0 || delays[1] != 0 {
		t.Errorf("the burst should pass free: %v", delays[:2])
	}
	// Each overflow caller waits one interval longer than the one before it, so they leave in order
	// instead of arriving at the host together.
	for i := 2; i < len(delays); i++ {
		want := time.Duration(i-1) * 20 * time.Millisecond
		if delays[i] < want-time.Millisecond || delays[i] > want+time.Millisecond {
			t.Errorf("caller %d waited %v, want ~%v (delays must fan out, not collapse)", i, delays[i], want)
		}
	}
}

// A cancel-heavy caller must not be able to defeat the limiter.
//
// This is the regression that a debt floor plus an unconditional refund produced together: a caller
// clamped by the floor was charged nothing, but cancelling still gave a token back, so every abandoned
// wait MINTED one. Measured on the shipped settings, ~4000 requests passed free in two seconds against a
// budget of twelve. Each half looked reasonable in review; only together were they a hole.
func TestHostLimiter_cancellationCannotMintTokens(t *testing.T) {
	// Repeated WAVES, not one burst: minted tokens are only visible once a later caller spends them, so
	// a single wave of cancels proves nothing and would pass against the very bug this exists to catch.
	l := newHostLimiter(50*time.Millisecond, 4)
	var free int
	var mu sync.Mutex
	for wave := 0; wave < 20; wave++ {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				defer cancel()
				start := time.Now()
				l.wait(ctx, "h")
				if time.Since(start) < 500*time.Microsecond {
					mu.Lock()
					free++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
	// 400 callers over ~20 waves. Only the burst of 4, plus whatever elapsed time genuinely earned,
	// may pass without waiting; the bug this catches let hundreds through.
	if free > 25 {
		t.Errorf("%d of 400 callers passed free under a burst of 4: cancellation is minting tokens", free)
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
	b := &bucket{tokens: 0, last: time.Now()}
	b.take(time.Second, 8) // spends to -1
	delay := b.take(time.Second, 8)
	// Now at -2, so the caller waits two intervals.
	if delay < 1900*time.Millisecond || delay > 2100*time.Millisecond {
		t.Errorf("delay %v does not match a 1s interval at two tokens of debt", delay)
	}
}

// Debt accumulates exactly, one token per caller. It is deliberately NOT floored: a caller that would
// queue past its own deadline is released by its context, not by forgiving the debt, because forgiven
// debt is indistinguishable from a token that was never spent and can be refunded into existence.
func TestBucket_debtIsExact(t *testing.T) {
	b := &bucket{tokens: 0, last: time.Now()}
	for i := 0; i < 100; i++ {
		b.take(time.Hour, 4) // an hour per token, so elapsed-time refill can't blur the count
	}
	if b.tokens > -99.9 || b.tokens < -100.1 {
		t.Errorf("100 callers should owe 100 tokens, bucket holds %.2f", b.tokens)
	}
}

// A refund can only ever undo a spend. Without the cap it is a way to create tokens: refunds outnumber
// spends whenever a caller is released by its context, and an uncapped counter banks the difference.
func TestBucket_refundCannotBankCredit(t *testing.T) {
	b := &bucket{tokens: 4, last: time.Now()}
	for i := 0; i < 10; i++ {
		b.refund(4)
	}
	if b.tokens > 4.001 {
		t.Errorf("refunds banked credit past the burst: %.2f", b.tokens)
	}

	// It still refunds where a token really was spent.
	b = &bucket{tokens: 0, last: time.Now()}
	b.take(time.Hour, 4)
	b.refund(4)
	if b.tokens < -0.001 || b.tokens > 0.001 {
		t.Errorf("a spend followed by its refund should net zero: %.2f", b.tokens)
	}
}

// The clamps in newHostLimiter, exercised. A zero interval makes the refill +Inf and every delay zero —
// a limiter that reads as configured and silently isn't — and burst 0 would pace even the first request.
func TestNewHostLimiter_clampsNonsenseSettings(t *testing.T) {
	l := newHostLimiter(0, 0)
	if l.every <= 0 {
		t.Errorf("a non-positive interval was not clamped: %v", l.every)
	}
	if l.burst < 1 {
		t.Errorf("burst was not clamped to at least one: %d", l.burst)
	}
	// And the clamped limiter genuinely paces: one free, the next waits.
	l = newHostLimiter(-time.Second, -5)
	l.wait(context.Background(), "h")
	if d := l.buckets["h"].take(l.every, l.burst); d <= 0 {
		t.Error("a clamped limiter does not pace at all")
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

// The burst has to cover a household's largest legitimate burst, which is opening a season: one request
// per episode, all at once.
//
// At a burst of 10 a 24-episode show queued the tail fourteen seconds out, against an eight-second
// scrape budget — so those episodes were never asked, counted as indexers that did not answer, and came
// back as a degraded empty list from a perfectly healthy indexer. The sustained rate is what protects
// the indexer; the burst only decides whether normal use waits at all.
func TestIndexerLimiter_burstCoversASeasonOpen(t *testing.T) {
	const season = 24
	b := &bucket{tokens: float64(indexerLimiter.burst), last: time.Now()}
	for i := 0; i < season; i++ {
		if delay := b.take(indexerLimiter.every, indexerLimiter.burst); delay > 0 {
			t.Fatalf("episode %d of a season open waited %v; the tail is never asked within the budget",
				i+1, delay)
		}
	}
}
