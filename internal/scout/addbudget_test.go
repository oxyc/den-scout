package scout

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The allowance covers EVERY store that can add, not just the one whose limit is documented.
//
// It was enforced inside torBoxStore.addMagnet, so Real-Debrid and Premiumize adds were uncounted — and
// an unbounded add loop is a bug wherever it points, published limit or not. Per service AND account, so
// a busy Real-Debrid cannot close TorBox's budget.
func TestSpendAdd_isPerServiceAndAccount(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 1)
	defer func() { globalAddBudget = prev }()

	for _, svc := range []DebridService{ServiceTorBox, ServiceRealDebrid, ServicePremiumize} {
		if err := spendAdd(svc, "tok", H); err != nil {
			t.Errorf("%s: first add refused: %v", svc, err)
		}
		if err := spendAdd(svc, "tok", H); err == nil {
			t.Errorf("%s: second add allowed against an allowance of 1", svc)
		}
		// A different account on the same service has its own allowance.
		if err := spendAdd(svc, "other-tok", H); err != nil {
			t.Errorf("%s: another account's allowance was consumed: %v", svc, err)
		}
	}
}

// A refusal names the service, so the app can say which debrid is the problem rather than "no source".
func TestSpendAdd_refusalNamesTheService(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 0)
	defer func() { globalAddBudget = prev }()

	err := spendAdd(ServiceRealDebrid, "tok", H)
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Service != ServiceRealDebrid {
		t.Fatalf("want a realdebrid StoreUnavailableError, got %v", err)
	}
}

// Every store that can add is WIRED to the allowance, not merely able to consult it.
//
// The budget lived inside torBoxStore.addMagnet, so Real-Debrid and Premiumize added freely past it.
// Testing spendAdd alone would not have noticed: this drives each store's Resolve with the allowance
// already spent and asserts no request reaches the service at all.
func TestStores_refuseToAddOnceTheBudgetIsSpent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(doer) Store
	}{
		{"torbox", func(d doer) Store {
			return &torBoxStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: torboxAPI}
		}},
		{"realdebrid", func(d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: realDebridAPI}
		}},
		{"premiumize", func(d doer) Store {
			return &premiumizeStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: premiumizeAPI}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 0) // nothing left this hour
			defer func() { globalAddBudget = prev }()

			reqs := 0
			d := mockDoer{fn: func(*http.Request) (*http.Response, error) {
				reqs++
				return resp(200, `{}`), nil
			}}
			_, err := tc.build(d).Resolve(context.Background(), ResolveTarget{InfoHash: H})
			var unavailable *StoreUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("want a budget refusal, got %v", err)
			}
			if reqs != 0 {
				t.Errorf("made %d requests despite a spent allowance — the store is not wired to it", reqs)
			}
		})
	}
}

// A cancel storm must cost ONE add, not one per poll — and the one it costs must be counted.
//
// Both halves matter and an earlier version got each of them wrong in turn. Refunding cancelled adds
// looked right and was measurably false: the POST has already been written, the debrid counts it, and
// sixty cancelled polls of one release put sixty createtorrent calls on the account while this budget
// still reported a full allowance. The cure is not to stop counting but to stop re-sending: an add whose
// outcome we never learned is remembered, and the next poll finds that memory instead of the add path.
func TestAddBudget_aCancelStormCostsOneAdd(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	sent := 0
	d := mockDoer{fn: func(*http.Request) (*http.Response, error) {
		sent++ // the request went out; the caller just never gets the answer
		return nil, context.Canceled
	}}
	s := &torBoxStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: torboxAPI}
	for i := 0; i < 60; i++ {
		_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	}
	if sent != 1 {
		t.Errorf("sent %d createtorrent requests across sixty cancelled polls of one release", sent)
	}
	if left := globalAddBudget.remaining(budgetAccount(ServiceTorBox, "tok")); left != 49 {
		t.Errorf("remaining = %d, want 49 — the add that WAS sent must be counted", left)
	}
	// Other titles are unaffected: one stuck release must not close the hour.
	if err := spendAdd(ServiceTorBox, "tok", repeat("c", 40)); err != nil {
		t.Errorf("another title was refused: %v", err)
	}
}

// The in-flight marker means "we do not know", so it must not outlive learning. Once createtorrent
// answers — even to refuse — the outcome is known and a legitimate retry has to be able to proceed.
func TestAddBudget_aKnownOutcomeReleasesTheInFlightMarker(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	sent := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "createtorrent") {
			sent++
			return resp(200, `{"data":{"torrent_id":9}}`), nil
		}
		return resp(500, "boom"), nil // the follow-up listing fails, so the resolve is retried
	}}
	s := &torBoxStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: torboxAPI}
	ep := ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(1)}
	_, _ = s.Resolve(context.Background(), ep)
	_, _ = s.Resolve(context.Background(), ep)
	if sent != 2 {
		t.Errorf("sent %d adds; a completed-but-unusable add must not block the retry", sent)
	}
}

// A real failure still counts. The request reached the service, so the add may well have happened —
// refunding on anything but a cancellation would make the ceiling meaningless.
func TestAddBudget_aRealFailureStillSpends(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 5)
	defer func() { globalAddBudget = prev }()

	d := mockDoer{fn: func(*http.Request) (*http.Response, error) { return resp(400, `{"error":"NOPE"}`), nil }}
	s := &torBoxStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: torboxAPI}
	_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if left := globalAddBudget.remaining(budgetAccount(ServiceTorBox, "tok")); left != 4 {
		t.Errorf("remaining = %d, want 4 — a delivered request must count", left)
	}
}

// A NoAdd target can never queue a torrent, whatever the store would otherwise do.
//
// The probe path already chose carefully — it only resolves releases a cache check reported as held —
// and still POSTed createtorrent, because TorBox's checkcached says what TORBOX has, not what this
// ACCOUNT has. A caller cannot promise this by choosing well; only the store can enforce it.
func TestNoAdd_neverReachesAnAddEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(doer) Store
	}{
		{"torbox", func(d doer) Store {
			return &torBoxStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: torboxAPI}
		}},
		{"realdebrid", func(d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: realDebridAPI}
		}},
		{"premiumize", func(d doer) Store {
			return &premiumizeStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: premiumizeAPI}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqs := 0
			d := mockDoer{fn: func(*http.Request) (*http.Response, error) {
				reqs++
				return resp(200, `{"data":{"torrent_id":1}}`), nil
			}}
			_, err := tc.build(d).Resolve(context.Background(), ResolveTarget{InfoHash: H, NoAdd: true})
			if err == nil {
				t.Fatal("nothing is held, so a NoAdd resolve must fail rather than queue it")
			}
			if reqs != 0 {
				t.Errorf("made %d requests for a NoAdd target — the store queued the torrent anyway", reqs)
			}
		})
	}
}

// refundAdd must discriminate. Its whole job is telling "the request was never sent" from "the response
// never arrived", and an earlier version refunded both — putting sixty real adds on the account while
// reporting a full allowance. The prior test for this drove a 400 RESPONSE, so err was nil and refundAdd
// was never reached: it passed against an unconditional refund.
func TestRefundAdd_onlyForARequestThatWasNeverSent(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 10)
	defer func() { globalAddBudget = prev }()
	acct := budgetAccount(ServiceTorBox, "tok")

	globalAddBudget.take(acct)
	refundAdd(ServiceTorBox, "tok", context.Canceled)
	if left := globalAddBudget.remaining(acct); left != 9 {
		t.Errorf("a cancelled add was refunded (remaining %d) — it had already reached the service", left)
	}
	refundAdd(ServiceTorBox, "tok", context.DeadlineExceeded)
	if left := globalAddBudget.remaining(acct); left != 9 {
		t.Errorf("an expired deadline was refunded (remaining %d) — same reason", left)
	}
	refundAdd(ServiceTorBox, "tok", errors.New("connection reset"))
	if left := globalAddBudget.remaining(acct); left != 9 {
		t.Errorf("a transport error was refunded (remaining %d); it may still have been delivered", left)
	}
	refundAdd(ServiceTorBox, "tok", errRequestNotSent)
	if left := globalAddBudget.remaining(acct); left != 10 {
		t.Errorf("a request that was never built must be refunded, remaining %d", left)
	}
}

// Scout's own ceiling is not the debrid's refusal, and must not be remembered or reported as one.
func TestSpendAdd_ourCeilingIsNotTheStoresRefusal(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 0)
	defer func() { globalAddBudget = prev }()

	cache := NewMemoryCache(1 << 20)
	err := spendAdd(ServiceTorBox, "tok", H)
	recordRefusal(cache, ServiceTorBox, "tok", H, err)
	if _, remembered := backedOff(cache, ServiceTorBox, "tok", H); remembered {
		t.Error("scout's own budget was written into the store's refusal memory")
	}
	// It still reads as unavailable to the caller — just not as TorBox's doing.
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("a budget refusal must still be a refusal: %v", err)
	}
	if !strings.Contains(unavailable.Reason, "scout") {
		t.Errorf("the reason must name scout as the refuser: %q", unavailable.Reason)
	}
}

// /health reports the allowance without naming accounts. The route is unauthenticated, and a stable
// service:hash(token) key would make it a confirmation oracle for a guessed token.
func TestHealth_reportsTheBudgetWithoutNamingAccounts(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 10)
	defer func() { globalAddBudget = prev }()
	globalAddBudget.take(budgetAccount(ServiceTorBox, "secret-token"))

	rec := httptest.NewRecorder()
	NewHandler(Deps{Cache: NewMemoryCache(1 << 20)}).ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "addBudgetRemaining") {
		t.Errorf("a spent allowance must be visible: %s", body)
	}
	if strings.Contains(body, keyHash("secret-token")) || strings.Contains(body, "torbox") {
		t.Errorf("/health discloses which account is spending: %s", body)
	}
}
