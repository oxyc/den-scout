package scout

import (
	"sync"
	"time"
)

// A local ceiling on debrid ADDS, per account.
//
// TorBox allows 60 torrent-adds an hour, and exhausting it has broken playback for a whole evening. Every
// guard in this package until now was a guard against a KNOWN way of spending them — don't probe an
// uncached release, don't re-add while backed off, don't resolve through a store that does not hold it.
// Each one closed a real hole, and each one was found only after it had already cost an evening.
//
// This is the other kind of control: it does not care WHY an add is being made. It counts them, and when
// the hour's allowance is gone it refuses, so an unforeseen path — a new caller, a client looping on an
// error, a bug not yet written — costs a refusal instead of the evening. The backoff in Resolve is the
// post-hoc version of this: it reacts after TorBox has already said no, which is one incident too late.
//
// Sized under the real ceiling, not at it. The gap absorbs adds this process cannot see: another client
// on the same account, a retry TorBox counted and we did not, an add whose response never arrived.
const (
	addBudgetWindow = time.Hour
	addBudgetLimit  = 50
)

// addBudget is a rolling-window counter, per account. Rolling rather than a fixed hourly bucket because
// the ceiling it mirrors is rolling: spending the whole allowance at 10:59 and again at 11:01 is exactly
// the burst that trips the real limit, and a fixed bucket permits it.
type addBudget struct {
	mu     sync.Mutex
	spent  map[string][]time.Time
	window time.Duration
	limit  int
	now    func() time.Time // injectable, so the tests do not sleep for an hour
}

func newAddBudget(window time.Duration, limit int) *addBudget {
	return &addBudget{
		spent:  map[string][]time.Time{},
		window: window,
		limit:  limit,
		now:    time.Now,
	}
}

// take records an add against the account and reports whether it is allowed. A refusal spends nothing,
// so the window drains normally and the next caller after it expires is served.
func (b *addBudget) take(account string) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	cutoff := now.Add(-b.window)
	kept := b.spent[account][:0]
	for _, t := range b.spent[account] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= b.limit {
		b.spent[account] = kept
		return false
	}
	b.spent[account] = append(kept, now)
	return true
}

// remaining is for logging and /health — how many adds the account may still make this window.
func (b *addBudget) remaining(account string) int {
	if b == nil {
		return -1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := b.now().Add(-b.window)
	live := 0
	for _, t := range b.spent[account] {
		if t.After(cutoff) {
			live++
		}
	}
	if left := b.limit - live; left > 0 {
		return left
	}
	return 0
}

// One per process, keyed by account, so every store built for every request shares the count. A budget
// held on the store would reset on each request, which is every request — the stores are rebuilt per
// call from the install's config.
var globalAddBudget = newAddBudget(addBudgetWindow, addBudgetLimit)
