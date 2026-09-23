package scout

import (
	"errors"
	"fmt"
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
	// defaultPlayReserve is how many of the window's adds a prefetch may not spend (PLAY_RESERVE_ADDS). The
	// budget used not to care WHY an add was made, so a season download running in the background could
	// spend the whole hour and the viewer who then pressed Play was refused. A prefetch — the binge
	// read-ahead, a queued download — stops this many short of the ceiling; a viewer waiting spends down to
	// zero. Small and fixed rather than a share of the allowance: a season pass should still fetch the whole
	// season, it just must not take the last few adds a Play needs.
	defaultPlayReserve = 5
)

// addBudget is a rolling-window counter, per account. Rolling rather than a fixed hourly bucket because
// the ceiling it mirrors is rolling: spending the whole allowance at 10:59 and again at 11:01 is exactly
// the burst that trips the real limit, and a fixed bucket permits it.
type addBudget struct {
	mu     sync.Mutex
	spent  map[string][]time.Time
	window time.Duration
	limit  int
	// reserve is how many adds of the window only a viewer waiting may spend; a prefetch is refused once
	// the account's remaining allowance is at or below it.
	reserve int
	now     func() time.Time // injectable, so the tests do not sleep for an hour
}

func newAddBudget(window time.Duration, limit int) *addBudget {
	return &addBudget{
		spent:   map[string][]time.Time{},
		window:  window,
		limit:   limit,
		reserve: defaultPlayReserve,
		now:     time.Now,
	}
}

// SetPlayReserve sets how many adds of the hour a prefetch may not spend. Called once at startup, from
// PLAY_RESERVE_ADDS.
func SetPlayReserve(n int) {
	globalAddBudget.mu.Lock()
	defer globalAddBudget.mu.Unlock()
	globalAddBudget.reserve = n
}

// playReserve is the reserve in force, for /metrics.
func (b *addBudget) playReserve() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reserve
}

// ceiling is how many adds the window may hold for this intent: the whole limit for a viewer waiting, the
// limit less the reserve for a prefetch.
func (b *addBudget) ceiling(prefetch bool) int {
	if prefetch {
		return b.limit - b.reserve
	}
	return b.limit
}

// take records an add against the account and reports whether it is allowed. A refusal spends nothing,
// so the window drains normally and the next caller after it expires is served. A prefetch is refused
// while the reserve is all that is left; a viewer waiting spends down to zero.
func (b *addBudget) take(account string, prefetch bool) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	cutoff := now.Add(-b.window)
	b.dropDrainedLocked(cutoff)
	kept := b.spent[account][:0]
	for _, t := range b.spent[account] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= b.ceiling(prefetch) {
		b.spent[account] = kept
		return false
	}
	b.spent[account] = append(kept, now)
	return true
}

// dropDrainedLocked forgets accounts whose window has emptied. Entries were only ever rewritten, never
// deleted, so the map grew one key per distinct service+token ever seen and never shrank — and the token
// comes from the install's own config URL on an unauthenticated route, so the set of keys is not bounded
// by anything this process controls. `pruneMintedLocked` is the same idea two files over. Caller holds
// b.mu.
func (b *addBudget) dropDrainedLocked(cutoff time.Time) {
	for acct, spent := range b.spent {
		if len(spent) == 0 {
			delete(b.spent, acct)
			continue
		}
		// The slice is append-only in time order, so the last entry is the newest.
		if !spent[len(spent)-1].After(cutoff) {
			delete(b.spent, acct)
		}
	}
}

// refund returns the most recent charge. Only for an add that was never sent — see refundAdd; refunding
// one that merely lost its response hands back an allowance the debrid has already spent.
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

// freesIn is how long until the account may add again for this intent: zero while it has allowance left,
// else until the charge holding it at its ceiling drains. What a spent budget's (or a held reserve's)
// Retry-After says.
func (b *addBudget) freesIn(account string, prefetch bool) time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	cutoff := now.Add(-b.window)
	var live []time.Time
	for _, t := range b.spent[account] {
		if t.After(cutoff) {
			live = append(live, t)
		}
	}
	ceiling := b.ceiling(prefetch)
	if ceiling <= 0 {
		return b.window // no allowance ever frees; a window is the honest "not soon"
	}
	if len(live) < ceiling {
		return 0
	}
	// Append-only in time order: the charge that must drain for one to free is the one `ceiling` from the newest.
	return live[len(live)-ceiling].Add(b.window).Sub(now)
}

// lowest reports the smallest remaining allowance across all accounts, and how many accounts have spent
// anything. For /health, which is the point: an operator needs to know the ceiling is being approached,
// and a monitor needs one number to alert on.
//
// Deliberately NOT keyed by account. /health is unauthenticated — every other route is protected by an
// unguessable config segment — and `service:hash(token)` is stable, so publishing it turns the endpoint
// into a confirmation oracle for a guessed token and discloses which services this install uses. The
// aggregate answers the operational question without answering that one.
func (b *addBudget) lowest() (left, accounts int) {
	if b == nil {
		return -1, 0
	}
	b.mu.Lock()
	keys := make([]string, 0, len(b.spent))
	for acct := range b.spent {
		keys = append(keys, acct)
	}
	b.mu.Unlock()
	left = b.limit
	for _, acct := range keys {
		if r := b.remaining(acct); r < b.limit {
			accounts++
			if r < left {
				left = r
			}
		}
	}
	return left, accounts
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
//
// `prefetch` is the caller's intent (ResolveTarget.Prefetch): a prefetch is refused while only the reserve
// is left, with errPlayReserve so the route can say so apart from a spent budget.
func spendAdd(svc DebridService, token, infoHash string, prefetch bool) error {
	if globalAddBudget.take(budgetAccount(svc, token), prefetch) {
		return nil
	}
	if prefetch {
		metrics.addRefusedPrefetch.Add(1)
		logLimited("add-reserve:"+string(svc), "%s add allowance is down to the reserve kept for Play, refusing prefetch %s",
			svc, shortHash(infoHash))
		return fmt.Errorf("%w: %w: %w", errScoutSide, errPlayReserve,
			&StoreUnavailableError{Service: svc, Reason: "the rest of scout's hourly add budget is kept for Play"})
	}
	metrics.addRefusedPlay.Add(1)
	logLimited("add-budget:"+string(svc), "%s add budget spent for the hour, refusing %s", svc, shortHash(infoHash))
	// Wrapped so the refusal memory can tell scout's own ceiling from the service's — see errOurBudget.
	return fmt.Errorf("%w: %w", errScoutSide,
		&StoreUnavailableError{Service: svc, Reason: "scout's own hourly add budget for this account is spent"})
}

// refundAdd gives the charge back when the request was never sent at all.
//
// It used to refund on cancellation, which was wrong and measurably so: a cancelled add has already been
// written to the wire, and the debrid counts it. Sixty cancelled polls of one release put sixty
// createtorrent calls on the account while this budget still reported a full allowance. "The response
// never arrived" and "the request never happened" are not the same fact, and only the second is a
// refund — the recurring mistake in this codebase is treating one as the other.
//
// The problem the refund was reaching for is real (a cancelling client must not be able to lock itself
// out), but the answer is to stop re-adding, not to stop counting. See addAttemptKey.
func refundAdd(svc DebridService, token string, err error) {
	if errors.Is(err, errRequestNotSent) {
		globalAddBudget.refund(budgetAccount(svc, token))
	}
}

// refundUnusedAdd gives a charge back when the request turned out not to be an add at all.
//
// Premiumize's directdl is the only caller: it is a purchase for a release the account lacks and a plain
// read for one it holds, and which of those happened is only knowable from the answer. Charging first
// keeps the budget able to GATE the call; refunding here keeps a library of held releases from costing
// an add per play.
func refundUnusedAdd(svc DebridService, token string) {
	globalAddBudget.refund(budgetAccount(svc, token))
}

// errPlayReserve marks a prefetch refused because what is left of the hour is kept for a viewer waiting.
// Not a spent budget and not the service refusing: /play answers it as "reserved_for_play", and the client
// retries after Retry-After without holding it against the release.
var errPlayReserve = errors.New("prefetch refused: the rest of the add allowance is reserved for Play")

// errRequestNotSent marks the one case where nothing reached the service: the request could not even be
// constructed. Everything past that point may have been received.
var errRequestNotSent = errors.New("request was not sent")

func budgetAccount(svc DebridService, token string) string {
	return string(svc) + ":" + keyHash(token)
}
