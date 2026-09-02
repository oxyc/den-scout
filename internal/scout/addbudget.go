package scout

import (
	"log"
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

// refund returns the most recent charge when the add did not happen. Without it a cancelled request is
// indistinguishable from a torrent that was queued, and the client cancels constantly — fifty cancelled
// polls of ONE release closed the whole hour, measured, after which every other title was refused too.
// The same lesson the host limiter learned: a charge for a request that was never made is debt the next
// caller pays.
func (b *addBudget) refund(account string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	spent := b.spent[account]
	if len(spent) == 0 {
		return
	}
	b.spent[account] = spent[:len(spent)-1]
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

// snapshot reports the remaining allowance per account, for /health. Only accounts that have spent
// something appear, so a fresh process reports nothing rather than a list of zeros.
func (b *addBudget) snapshot() map[string]int {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	accounts := make([]string, 0, len(b.spent))
	for acct := range b.spent {
		accounts = append(accounts, acct)
	}
	b.mu.Unlock()
	out := make(map[string]int, len(accounts))
	for _, acct := range accounts {
		if left := b.remaining(acct); left < b.limit {
			// The account key is service:hash(token) — the hash keeps the token out of a response that is
			// not otherwise authenticated, while still telling two accounts apart.
			out[acct] = left
		}
	}
	return out
}

// One per process, keyed by service+account, so every store built for every request shares the count.
//
// In memory, deliberately but not costlessly: a redeploy or crash-loop resets the count while the real
// hourly ceiling keeps counting, so a restart hands back an allowance the service has not. The gap
// between this limit and the real 60 is what absorbs that, which is one more reason not to close it.
// A budget held on the store would be worse still — the stores are rebuilt per request.
var globalAddBudget = newAddBudget(addBudgetWindow, addBudgetLimit)

// spendAdd charges one add against a service's account, or refuses. Every store calls this immediately
// before the request that queues a torrent, so there is one place the allowance is enforced rather than
// one per store — and no way to add without passing through it.
//
// Per service AND account: TorBox's ceiling is TorBox's. Counting them together would let a busy
// Real-Debrid close TorBox's budget, and a service with no published limit would still be worth bounding
// — an unbounded add loop is a bug wherever it points.
func spendAdd(svc DebridService, token, infoHash string) error {
	if globalAddBudget.take(budgetAccount(svc, token)) {
		return nil
	}
	log.Printf("scout: %s add budget spent for the hour, refusing %s", svc, shortHash(infoHash))
	return &StoreUnavailableError{svc, "hourly add budget spent"}
}

// refundAdd gives the charge back when the request that would have queued the torrent never completed —
// a cancelled client, an expired deadline. Charging before the request is right (an add that succeeds
// but whose response is lost must still count), but only if an add that demonstrably did not happen is
// given back.
func refundAdd(svc DebridService, token string, err error) {
	if isCancellation(err) {
		globalAddBudget.refund(budgetAccount(svc, token))
	}
}

func budgetAccount(svc DebridService, token string) string {
	return string(svc) + ":" + keyHash(token)
}
