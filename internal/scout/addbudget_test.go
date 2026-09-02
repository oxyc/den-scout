package scout

import (
	"sync"
	"testing"
	"time"
)

// The allowance is spent, then refused — not spent, then apologised for.
func TestAddBudget_refusesPastTheLimit(t *testing.T) {
	b := newAddBudget(time.Hour, 3)
	for i := 0; i < 3; i++ {
		if !b.take("acct") {
			t.Fatalf("refused add %d of an allowance of 3", i+1)
		}
	}
	if b.take("acct") {
		t.Error("a fourth add was allowed against an allowance of 3")
	}
	if got := b.remaining("acct"); got != 0 {
		t.Errorf("remaining = %d, want 0", got)
	}
}

// Per ACCOUNT. One install's spending must not refuse another's, and the token is the identity.
func TestAddBudget_isPerAccount(t *testing.T) {
	b := newAddBudget(time.Hour, 1)
	if !b.take("a") || !b.take("b") {
		t.Fatal("two accounts each have their own first add")
	}
	if b.take("a") {
		t.Error("account a's allowance was not its own")
	}
	if got := b.remaining("b"); got != 0 {
		t.Errorf("b remaining = %d, want 0", got)
	}
}

// Rolling, not a fixed hourly bucket: the ceiling this mirrors is rolling, so spending the whole
// allowance at 10:59 and again at 11:01 is exactly the burst that trips the real limit.
func TestAddBudget_windowRollsRatherThanResetting(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	b := newAddBudget(time.Hour, 2)
	b.now = func() time.Time { return now }

	// Staggered, so the two adds age out at different times — that is what makes the window a rolling
	// one rather than a bucket that empties all at once.
	if !b.take("acct") {
		t.Fatal("the first add is within the allowance")
	}
	now = now.Add(30 * time.Minute)
	if !b.take("acct") {
		t.Fatal("the second add is within the allowance")
	}
	// 10:59 — both are still inside the hour.
	now = now.Add(29 * time.Minute)
	if b.take("acct") {
		t.Error("an add while the allowance is still spent must be refused")
	}
	// 11:01 — the 10:00 add has aged out; the 10:30 one has not, so exactly one slot is free.
	now = now.Add(2 * time.Minute)
	if !b.take("acct") {
		t.Error("the oldest add aged out; one slot should be free")
	}
	if b.take("acct") {
		t.Error("only one add had aged out, but two slots were granted")
	}
}

// A refusal spends nothing, or a client that keeps asking would hold its own budget shut forever.
func TestAddBudget_aRefusalCostsNothing(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	b := newAddBudget(time.Hour, 1)
	b.now = func() time.Time { return now }

	b.take("acct")
	for i := 0; i < 50; i++ { // a poll loop, hammering while refused
		now = now.Add(time.Second)
		b.take("acct")
	}
	// The single spent add is ~50s old; once it ages out the next caller is served, and the 50 refusals
	// must not have pushed the window forward.
	now = now.Add(time.Hour)
	if !b.take("acct") {
		t.Error("refusals extended the window — a polling client would never recover")
	}
}

// Concurrent callers must not be able to overspend: the whole point is a hard ceiling under load, and
// load is exactly when the paths that spend adds all fire at once.
func TestAddBudget_concurrentCallersCannotOverspend(t *testing.T) {
	b := newAddBudget(time.Hour, 10)
	var granted int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.take("acct") {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 10 {
		t.Errorf("%d adds granted against an allowance of 10", granted)
	}
}

// A nil budget is a no-op, so a store built without one is never silently blocked.
func TestAddBudget_nilAllowsEverything(t *testing.T) {
	var b *addBudget
	if !b.take("acct") {
		t.Error("a nil budget must not refuse")
	}
	if b.remaining("acct") != -1 {
		t.Error("a nil budget has no remaining count to report")
	}
}
