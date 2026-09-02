package scout

import (
	"context"
	"sync"
	"time"
)

// Outbound pacing, per upstream host.
//
// Nothing bounded how fast scout asked an indexer. Retry-on-shed made that worse by design: a host under
// load answers 502, and the answer was to ask again. Torrentio's own 502s were ultimately a cold-CDN
// problem rather than a rate limit, but the shape was there waiting — several surfaces can each issue
// lookups at once, and the only cap in the whole system guarded the poster grid, whose comment names the
// risk outright ("ban risk").
//
// A token bucket rather than a fixed delay: a household's normal use is bursty and small — open a series,
// glance at three episodes — and should never wait. Sustained traffic is what needs a ceiling. So a burst
// up to `burst` passes immediately, and beyond that requests queue at `every` rather than being shed or
// failed. Queueing is the point: a shed request became "this release does not exist" for an entire
// evening, and a request that waits 300 ms is invisible next to an 8-second scrape budget.
type hostLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	every   time.Duration
	burst   int
}

type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newHostLimiter(every time.Duration, burst int) *hostLimiter {
	// A non-positive interval makes the refill +Inf and every delay zero — a limiter that looks configured
	// and silently isn't. Fail loud in the only direction that is safe here: pace something.
	if every <= 0 {
		every = time.Second
	}
	if burst < 1 {
		burst = 1
	}
	return &hostLimiter{buckets: map[string]*bucket{}, every: every, burst: burst}
}

// wait blocks until this host may be asked again, or the context ends. It never returns an error of its
// own: a cancelled context is the caller's deadline, not a refusal, and the caller already handles that.
func (l *hostLimiter) wait(ctx context.Context, host string) {
	if l == nil || host == "" {
		return
	}
	l.mu.Lock()
	b := l.buckets[host]
	if b == nil {
		b = &bucket{tokens: float64(l.burst), last: time.Now()}
		l.buckets[host] = b
	}
	l.mu.Unlock()

	delay := b.take(l.every, l.burst)
	if delay <= 0 {
		return
	}
	// Not "skip the wait when it would exceed the caller's deadline". That was tried and is a bypass: every
	// caller carries a deadline, so a client with short ones sails past the limiter entirely — the test
	// for cancellation-minting caught it immediately, with 400 of 400 callers passing free. Queueing past
	// a deadline is a real cost, but the burst is where it gets solved, not here.
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		// Give the token back. The caller abandons the request, so nothing was asked of the host — and a
		// token spent on a request never sent is debt the NEXT caller pays, at a rate comparable to the
		// scrape budget itself.
		b.refund(l.burst)
	case <-timer.C:
	}
}

// take refills by elapsed time, spends a token, and reports how long the caller must wait for it. The
// token is spent even when the bucket goes negative, so concurrent callers queue in order rather than all
// waking to the same free slot.
//
// Debt is NOT floored. A floor was tried and had to come out: a clamped caller is charged nothing, but a
// clamped caller that then cancels still refunds — so under a cancel-heavy load the floor MANUFACTURED
// tokens and the limiter stopped limiting (measured: ~4000 free passes in two seconds against a budget
// of twelve). It was also solving a problem that is not this function's to solve. The floor existed to
// stop a caller queueing past its own deadline; `wait` already returns the moment the caller's context
// ends, which is the same protection with none of the accounting damage — and without collapsing every
// saturated caller onto one identical delay, which turned the queue into a synchronised herd.
func (b *bucket) take(every time.Duration, burst int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() / every.Seconds()
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = now
	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens * float64(every))
}

// refund returns the token a caller spent but never used, capped at the burst so it can only ever undo a
// spend — never bank credit the bucket did not earn by elapsed time.
func (b *bucket) refund(burst int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens < float64(burst) {
		b.tokens++
	}
}
