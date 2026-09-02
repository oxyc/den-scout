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
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// take refills by elapsed time, spends a token, and reports how long the caller must wait for it. The
// token is spent even when the bucket goes negative, so concurrent callers queue in order rather than all
// waking to the same free slot.
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
