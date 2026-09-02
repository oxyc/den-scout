package scout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if !isAddEndpoint(r) {
			return resp(200, `{"data":[]}`), nil // the account holds nothing; only the ADD is abandoned
		}
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

// A torrent the account already holds is resolved from the account, not bought again.
//
// The resolve entry is a six-hour convenience cache, not a record of what the account has — so anything
// not played in the last six hours, which is most of a library, fell through to createtorrent and paid
// an add for a torrent already sitting there. The account listing answers this and was already being
// consulted by Status; it just was not consulted here.
func TestAddBudget_aHeldTorrentIsNotBoughtAgain(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	adds := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case isAddEndpoint(r):
			adds++
			return resp(200, `{"data":{"torrent_id":9}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			return resp(200, `{"data":[{"id":9,"hash":"`+H+`"}]}`), nil
		}
		return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
	}}
	s := &torBoxStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: torboxAPI}
	target := ResolveTarget{InfoHash: H, FileIdx: intp(0)}
	// The sequence /play actually runs: Status first — it finds the torrent in the account and remembers
	// its id — then Resolve. Resolve must use what Status learned rather than buying the torrent again.
	if _, downloading := s.Status(context.Background(), target); downloading {
		t.Fatal("a finished torrent is not downloading")
	}
	link, err := s.Resolve(context.Background(), target)
	if err != nil || link == "" {
		t.Fatalf("a held torrent must resolve: %q %v", link, err)
	}
	if adds != 0 {
		t.Errorf("spent %d adds on a torrent the account already holds", adds)
	}
	if left := globalAddBudget.remaining(budgetAccount(ServiceTorBox, "tok")); left != 50 {
		t.Errorf("allowance dropped to %d for a torrent that needed no add", left)
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
			adds := 0
			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if isAddEndpoint(r) {
					adds++
				}
				return resp(200, `{"data":[]}`), nil // an empty account: nothing held
			}}
			_, err := tc.build(d).Resolve(context.Background(), ResolveTarget{InfoHash: H, NoAdd: true})
			if err == nil {
				t.Fatal("nothing is held, so a NoAdd resolve must fail rather than queue it")
			}
			// Reads are fine and expected — asking the account what it holds is how NoAdd is answered.
			// What must never happen is the queueing call.
			if adds != 0 {
				t.Errorf("made %d adds for a NoAdd target — the store queued the torrent anyway", adds)
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

// isAddEndpoint — the three calls that actually queue a torrent. Reads (mylist, checkcached, requestdl)
// are not adds, and a test that counts every request cannot tell the difference.
func isAddEndpoint(r *http.Request) bool {
	p := r.URL.Path
	return strings.Contains(p, "createtorrent") ||
		strings.Contains(p, "addMagnet") ||
		strings.Contains(p, "directdl")
}

// Scout's own in-flight marker is not the STORE refusing, and must not be recorded or reported as one.
//
// It shipped returning a bare StoreUnavailableError{torbox,…}, which recordRefusal then wrote into the
// per-hash refusal memory: /play and the probe answered 503 naming TorBox for a state TorBox had no part
// in, the backoff outlived its own 90s window (60s refusal re-recorded on each poll), and the read-only
// NoAdd path was blocked by a marker it cannot cause.
func TestAddInFlight_isScoutSideNotTheStores(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	noteAddAttempt(cache, ServiceTorBox, "tok", H)

	err := addInFlight(cache, ServiceTorBox, "tok", H)
	if err == nil {
		t.Fatal("an add in flight must be reported")
	}
	// Its own sentinel, not the budget's: an add in flight means the release IS being fetched, so the
	// route answers 202 "coming", where a spent allowance answers 503 "not now". Sharing errScoutSide
	// made both come out as a refusal, and the client stopped trying other sources for a release scout
	// had queued itself.
	if !errors.Is(err, errAddInFlight) {
		t.Errorf("an add in flight needs its own sentinel, not the budget's: %v", err)
	}
	if errors.Is(err, errScoutSide) {
		t.Error("an add in flight is not a refusal; conflating them loses the 202")
	}
	recordRefusal(cache, ServiceTorBox, "tok", H, err)
	if _, remembered := backedOff(cache, ServiceTorBox, "tok", H); remembered {
		t.Error("scout's in-flight marker was written into the store's refusal memory")
	}
}

// EVERY store that can add needs the in-flight guard. Giving it to TorBox alone did not stop the cancel
// loop — it moved it: ResolvePreferring walks on to the store without a marker, and fifty cancelled
// polls closed THAT account's allowance instead.
func TestAddInFlight_appliesToEveryStoreThatAdds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(doer, Cache) Store
	}{
		{"torbox", func(d doer, c Cache) Store {
			return &torBoxStore{token: "tok", client: d, cache: c, api: torboxAPI}
		}},
		{"realdebrid", func(d doer, c Cache) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}},
		{"premiumize", func(d doer, c Cache) Store {
			return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			adds := 0
			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if !isAddEndpoint(r) {
					return resp(200, `{"data":[]}`), nil
				}
				adds++
				return nil, context.Canceled // sent, then abandoned by the client
			}}
			store := tc.build(d, NewMemoryCache(1<<20))
			for i := 0; i < 30; i++ {
				_, _ = store.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			}
			if adds != 1 {
				t.Errorf("sent %d adds across thirty cancelled polls of one release", adds)
			}
		})
	}
}

// Finding the torrent in the account settles the in-flight marker — that add plainly landed.
//
// Without this the marker outlives the fact it stood in for: the release stays "awaiting the result"
// for its full 90s window while the result is sitting in the account listing, and every poll in between
// is refused for a state that has already resolved itself.
func TestAddInFlight_discoveringTheTorrentSettlesTheMarker(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "mylist") {
			return resp(200, `{"data":[{"id":9,"hash":"`+H+`"}]}`), nil
		}
		return resp(404, "{}"), nil
	}}
	s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}

	// An add went out and was abandoned, so the marker is set.
	noteAddAttempt(cache, ServiceTorBox, "tok", H)
	if err := addInFlight(cache, ServiceTorBox, "tok", H); err == nil {
		t.Fatal("the marker should be set")
	}

	// The account listing then shows the torrent: the add landed, so the marker must go.
	if _, found := s.torrentID(context.Background(), H); !found {
		t.Fatal("the account holds it")
	}
	if err := addInFlight(cache, ServiceTorBox, "tok", H); err != nil {
		t.Errorf("the marker outlived the discovery it stood in for: %v", err)
	}
}

// A remembered torrent id is a claim with a six-hour life, not a fact. When the account no longer has
// the torrent, forget it and buy it again — do not answer dead_link for six hours.
//
// The shortcut that stops re-buying a held torrent also stopped the SELF-HEALING that used to happen
// when a torrent was removed account-side: Resolve took the held path, failed, and re-Put the id with a
// fresh six-hour TTL on every poll, so a client polling kept the stale entry alive indefinitely and
// blacklisted a release that had been playing an hour earlier.
func TestKnownTorrentID_aDeletedTorrentIsForgottenAndReBought(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	gone := true
	adds := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case isAddEndpoint(r):
			adds++
			gone = false // re-bought: the account has it again
			return resp(200, `{"data":{"torrent_id":77}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			return resp(200, `{"data":[]}`), nil
		}
		if gone {
			return resp(404, `{"success":false}`), nil // requestdl on a torrent that is not there
		}
		return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
	}}
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL) // a stale id from before the deletion
	s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}

	link, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)})
	if err != nil || link == "" {
		t.Fatalf("a deleted torrent must be re-bought, not answered dead: %q %v", link, err)
	}
	if adds != 1 {
		t.Errorf("made %d adds; the stale id should have been forgotten and the torrent re-added", adds)
	}
	if raw, _ := cache.Get(torrentIDKey("tok", H)); raw == "42" {
		t.Error("the stale id survived, and every later poll would keep it alive for another six hours")
	}

	// And when the re-add ALSO fails, the stale id must be gone rather than re-stamped with a fresh
	// six-hour TTL. That refresh is what made a deleted torrent unplayable indefinitely: the held path
	// re-Put the id on every poll, so a polling client kept its own poison alive.
	stuck := NewMemoryCache(1 << 20)
	stuck.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
	dead := &torBoxStore{token: "tok", cache: stuck, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "mylist") {
				return resp(200, `{"data":[]}`), nil
			}
			return resp(404, `{"success":false}`), nil // nothing works: the add fails too
		}}}
	_, _ = dead.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)})
	if raw, present := stuck.Get(torrentIDKey("tok", H)); present && raw == "42" {
		t.Error("a failing resolve refreshed the stale id instead of forgetting it")
	}
}

// An add going out clears the torrent-miss marker.
//
// That marker suppresses the account listing for 15s, and the listing is the only thing that can find
// the torrent the add just created — so leaving it kept the next poll on the "nothing is queued" path
// instead of "it is downloading". This line was deleted once during a refactor, with its explanation,
// and nothing failed.
func TestNoteAddAttempt_clearsTheTorrentMissMarker(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentMissKey("tok", H), "1", torrentMissTTL) // Status looked and found nothing
	noteAddAttempt(cache, ServiceTorBox, "tok", H)
	if _, stillMissing := cache.Get(torrentMissKey("tok", H)); stillMissing {
		t.Error("the miss marker outlived the add that disproves it; Status cannot see the new torrent")
	}
}

// Only a definitive "no such torrent" forgets a remembered id. Everything else says nothing about what
// the account holds, and re-buying on it costs an add per poll.
//
// Measured before this: ten polls of ONE already-downloaded release cost ten adds, because a cancelled
// poll erased the id, the re-add re-created the torrent, the next Status found it and cleared the
// in-flight marker meant to stop the loop, and round it went.
func TestKnownTorrentID_onlyAConfirmedAbsenceForgetsTheID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		respond func() (*http.Response, error)
	}{
		{"cancelled poll", func() (*http.Response, error) { return nil, context.Canceled }},
		{"throttled", func() (*http.Response, error) { return resp(429, `{"error":"RATE"}`), nil }},
		{"server fault", func() (*http.Response, error) { return resp(500, `{}`), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			adds := 0
			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if isAddEndpoint(r) {
					adds++
					return resp(200, `{"data":{"torrent_id":77}}`), nil
				}
				if strings.Contains(r.URL.Path, "mylist") {
					return resp(200, `{"data":[]}`), nil
				}
				return tc.respond() // requestdl on the held torrent
			}}
			cache := NewMemoryCache(1 << 20)
			cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
			s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}

			for i := 0; i < 10; i++ {
				_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)})
			}
			if adds != 0 {
				t.Errorf("%d adds for a held torrent after a %s — this says nothing about what the account has", adds, tc.name)
			}
			if raw, _ := cache.Get(torrentIDKey("tok", H)); raw != "42" {
				t.Errorf("the id was forgotten on a %s: %q", tc.name, raw)
			}
		})
	}
}

// A pack that does not contain the requested episode is a PERMANENT fact about a torrent the account
// has. Re-buying the pack to re-learn it spends an add and returns the identical refusal.
func TestKnownTorrentID_aPackMismatchDoesNotReBuyThePack(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	adds := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if isAddEndpoint(r) {
			adds++
			return resp(200, `{"data":{"torrent_id":77}}`), nil
		}
		// The pack holds S01E01–E02 and nothing else.
		return resp(200, `{"data":{"files":[{"id":0,"name":"Show.S01E01.mkv","size":10},{"id":1,"name":"Show.S01E02.mkv","size":20}]}}`), nil
	}}
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
	s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}

	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(9)})
	if !errors.Is(err, errEpisodeNotInTorrent) {
		t.Fatalf("want errEpisodeNotInTorrent, got %v", err)
	}
	if adds != 0 {
		t.Errorf("spent %d adds re-buying a pack to re-learn what it does not contain", adds)
	}
}

// Every cache key that carries account state is scoped by the token.
//
// The comments say what happens otherwise — "would let one user's cached torrent_id be used with another
// user's token → wrong/other-account content" — and the cache is process-global, so it is reachable.
// Stripping keyHash(token) from any of these left the whole suite green.
func TestCacheKeys_areScopedToTheAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(token string) string
	}{
		{"resolve", func(tok string) string { return resolveKey(tok, H) }},
		{"torrentID", func(tok string) string { return torrentIDKey(tok, H) }},
		{"torrentMiss", func(tok string) string { return torrentMissKey(tok, H) }},
		{"refused", func(tok string) string { return refusedKey(ServiceTorBox, tok, H) }},
		{"addAttempt", func(tok string) string { return addAttemptKey(ServiceTorBox, tok, H) }},
		{"budget", func(tok string) string { return budgetAccount(ServiceTorBox, tok) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mine, theirs := tc.key("my-token"), tc.key("their-token")
			if mine == theirs {
				t.Fatalf("two accounts share the key %q — one user's state would be served to another", mine)
			}
			if strings.Contains(mine, "my-token") {
				t.Errorf("the raw token is in the key: %q", mine)
			}
			if !strings.Contains(mine, keyHash("my-token")) {
				t.Errorf("the key is not derived from the account: %q", mine)
			}
		})
	}
}

// A store with no Status endpoint must remember that it QUEUED a release, or every poll re-adds it.
//
// TorBox does not need this — it keeps a torrent id and answers Status, so a poll during the download
// short-circuits long before Resolve. Real-Debrid and Premiumize have neither, and a just-added torrent
// that is not downloadable yet comes back as a dead link rather than a refusal, so nothing stopped the
// loop. Thirty polls of one release put thirty duplicate torrents on the account and spent thirty of the
// hourly allowance in a minute: the loop TorBox's guards closed, relocated.
func TestQueued_aStoreWithoutStatusDoesNotReAddOnEveryPoll(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(doer, Cache) Store
	}{
		{"realdebrid", func(d doer, c Cache) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}},
		{"premiumize", func(d doer, c Cache) Store {
			return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			adds := 0
			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if isAddEndpoint(r) {
					adds++
					// Accepted, but nothing playable yet — the normal state of a fresh download.
					return resp(200, `{"id":"t1","status":"success","content":[]}`), nil
				}
				return resp(200, `{"files":[],"links":[]}`), nil
			}}
			store := tc.build(d, NewMemoryCache(1<<20))
			for i := 0; i < 30; i++ {
				_, _ = store.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			}
			// The measure is the CHARGE, not the call. Premiumize's directdl is both the question and the
			// purchase — it is the only thing that can notice the transfer finished, so it has to keep
			// being called; what must not repeat is paying for it. Real-Debrid can separate the two, and
			// does: one addMagnet, then info against the torrent it remembers.
			if left := globalAddBudget.remaining(budgetAccount(store.Service(), "tok")); left != 49 {
				t.Errorf("allowance dropped to %d; one queued release must cost one add", left)
			}
			if store.Service() == ServiceRealDebrid && adds != 1 {
				t.Errorf("%d addMagnet calls across thirty polls — each one a duplicate torrent", adds)
			}
		})
	}
}

// A release that RESOLVED must be playable again immediately.
//
// The queued marker reused the in-flight key with a 20-minute TTL and nothing cleared it on success, so
// a store with no held-torrent fast path answered 202 "downloading" for a release it had just served.
// One infohash is a whole season pack, so playing S01E01 blocked every other episode of the show; a
// replay, a resume, or an expired redirect hit the same wall.
func TestQueued_aResolvedReleaseIsNotBlockedByItsOwnMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		make func(doer, Cache) Store
	}{
		{"realdebrid", "",
			func(d doer, c Cache) Store {
				return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
			}},
		{"premiumize", `{"status":"success","content":[{"path":"movie.mkv","link":"https://pm/final","size":999}]}`,
			func(d doer, c Cache) Store {
				return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if tc.body != "" {
					return resp(200, tc.body), nil
				}
				switch { // Real-Debrid needs its full add → info → select → unrestrict sequence
				case strings.Contains(r.URL.Path, "addMagnet"):
					return resp(201, `{"id":"t1"}`), nil
				case strings.Contains(r.URL.Path, "/torrents/info/"):
					return resp(200, `{"files":[{"id":1,"path":"/movie.mkv","bytes":999}],"links":["https://rd/restricted"]}`), nil
				case strings.Contains(r.URL.Path, "selectFiles"):
					return resp(204, ""), nil
				case strings.Contains(r.URL.Path, "unrestrict/link"):
					return resp(200, `{"download":"https://rd/dl.mkv"}`), nil
				}
				return resp(404, "{}"), nil
			}}
			store := tc.make(d, NewMemoryCache(1<<20))

			first, err := store.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			if err != nil || first == "" {
				t.Fatalf("first resolve: %q %v", first, err)
			}
			second, err := store.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			if err != nil || second == "" {
				t.Fatalf("a release that just played came back as %q %v — its own marker blocked it", second, err)
			}
		})
	}
}

// A permanent refusal after the add must not be laundered into a 20-minute wait: the client has to be
// able to give up on this release and try the next.
func TestQueued_aPermanentRefusalClearsTheMarker(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	cache := NewMemoryCache(1 << 20)
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if isAddEndpoint(r) {
			return resp(201, `{"id":"t1"}`), nil
		}
		// A filename RD will not serve — no amount of waiting changes it.
		return resp(200, `{"files":[{"id":1,"path":"/Movie.2024.WEB-DL.x265.mkv","bytes":999}],"links":[]}`), nil
	}}
	s := &realDebridStore{token: "tok", client: d, cache: cache, api: realDebridAPI}
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err == nil {
		t.Fatal("a blocked filename must fail")
	}
	if err := addInFlight(cache, ServiceRealDebrid, "tok", H); err != nil {
		t.Error("a permanent refusal was left looking like a download in progress")
	}
}

// Real-Debrid remembers the torrent it bought, so a release is paid for once however often it is asked
// for — including when it can never be served.
//
// RD's addMagnet creates a NEW torrent every call, and RD has no Status endpoint for the wait in
// between, so the guard has to be a memory of the purchase. It must survive both a success (the next
// episode of the same pack) and a permanent refusal (a pack that lacks the episode), which is what
// every earlier version of this got wrong in one direction or the other.
func TestRealDebrid_buysAReleaseOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		info string
		want string // "" when no link is expected
	}{
		{"a pack that lacks the episode", `{"files":[{"id":1,"path":"/Show.S01E01.mkv","bytes":9}],"links":[]}`, ""},
		{"a release that plays", `{"files":[{"id":1,"path":"/movie.mkv","bytes":9}],"links":["https://rd/r"]}`, "https://rd/dl.mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			adds := 0
			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(r.URL.Path, "addMagnet"):
					adds++
					return resp(201, `{"id":"t1"}`), nil
				case strings.Contains(r.URL.Path, "/torrents/info/"):
					return resp(200, tc.info), nil
				case strings.Contains(r.URL.Path, "selectFiles"):
					return resp(204, ""), nil
				case strings.Contains(r.URL.Path, "unrestrict/link"):
					return resp(200, `{"download":"https://rd/dl.mkv"}`), nil
				}
				return resp(404, "{}"), nil
			}}
			s := &realDebridStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: realDebridAPI}
			target := ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(9)}
			if tc.want != "" {
				target = ResolveTarget{InfoHash: H}
			}
			var last string
			for i := 0; i < 10; i++ {
				link, _ := s.Resolve(context.Background(), target)
				last = link
			}
			if adds != 1 {
				t.Errorf("%d adds across ten polls — RD makes a new torrent every time", adds)
			}
			if last != tc.want {
				t.Errorf("last resolve = %q, want %q", last, tc.want)
			}
		})
	}
}

// Only RD's own 404 forgets the remembered torrent. An empty file list is a torrent whose metadata has
// not resolved yet — forgetting on it re-bought the release on the very next poll.
func TestRealDebrid_forgetsOnlyOnAConfirmedAbsence(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	adds, gone := 0, false
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "addMagnet"):
			adds++
			return resp(201, `{"id":"t1"}`), nil
		case strings.Contains(r.URL.Path, "/torrents/info/"):
			if gone {
				return resp(404, `{}`), nil
			}
			return resp(200, `{"files":[],"links":[]}`), nil // queued, metadata not in yet
		}
		return resp(404, "{}"), nil
	}}
	cache := NewMemoryCache(1 << 20)
	s := &realDebridStore{token: "tok", client: d, cache: cache, api: realDebridAPI}
	for i := 0; i < 5; i++ {
		_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	}
	if adds != 1 {
		t.Errorf("%d adds while the torrent was merely not ready", adds)
	}
	if _, held := s.knownTorrent(ResolveTarget{InfoHash: H}); !held {
		t.Error("a not-ready torrent was forgotten")
	}
	// A THROTTLE on the info call is not an absence either — it describes this attempt, not the account.
	throttled := NewMemoryCache(1 << 20)
	tAdds := 0
	ts := &realDebridStore{token: "tok", cache: throttled, api: realDebridAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "addMagnet") {
				tAdds++
				return resp(201, `{"id":"t1"}`), nil
			}
			return resp(503, `{}`), nil // RD is faulting, and says nothing about what it holds
		}}}
	for i := 0; i < 5; i++ {
		_, _ = ts.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	}
	if tAdds != 1 {
		t.Errorf("%d adds while RD was merely throttling its own info endpoint", tAdds)
	}
	if _, held := ts.knownTorrent(ResolveTarget{InfoHash: H}); !held {
		t.Error("a throttle forgot the torrent, so the next poll buys it again")
	}

	// Now RD really does not have it: forget, and let the next attempt buy it again.
	gone = true
	_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if _, held := s.knownTorrent(ResolveTarget{InfoHash: H}); held {
		t.Error("a 404 must forget the id, or the release is stuck behind a ghost for six hours")
	}
}

// Premiumize marks a release as queued ONLY when directdl had nothing to serve. directdl IS the fetch,
// so marking every call blocked releases that would have played instantly — the second episode of a pack
// could not be resolved for twenty minutes.
func TestPremiumize_marksOnlyAnEmptyAnswer(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	cache := NewMemoryCache(1 << 20)
	served := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return resp(200, `{"status":"success","content":[{"path":"m.mkv","link":"https://pm/l","size":9}]}`), nil
		}}}
	if _, err := served.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err != nil {
		t.Fatalf("a release premiumize can serve must resolve: %v", err)
	}
	if alreadyQueued(cache, "tok", H) {
		t.Error("a release that was SERVED was marked as queued; the next request would be refused")
	}
	if _, err := served.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err != nil {
		t.Errorf("the same release could not be resolved twice: %v", err)
	}

	// An empty answer means a transfer was queued: remember it, so the next poll does not pay again —
	// but keep asking, because directdl is the only thing that can notice it finished.
	before := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "pending"))
	calls := 0
	done := false
	queued := &premiumizeStore{token: "pending", cache: NewMemoryCache(1 << 20), api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			calls++
			if done {
				return resp(200, `{"status":"success","content":[{"path":"m.mkv","link":"https://pm/done","size":9}]}`), nil
			}
			return resp(200, `{"status":"success","content":[]}`), nil
		}}}
	for i := 0; i < 10; i++ {
		_, err := queued.Resolve(context.Background(), ResolveTarget{InfoHash: H})
		if !errors.Is(err, errAddInFlight) {
			t.Fatalf("a queued transfer must read as coming, not dead: %v", err)
		}
	}
	if spent := before - globalAddBudget.remaining(budgetAccount(ServicePremiumize, "pending")); spent != 1 {
		t.Errorf("paid for the same transfer %d times", spent)
	}
	if calls < 10 {
		t.Errorf("only asked %d times: nothing else can notice the transfer finished", calls)
	}

	// And when it does finish, the very next poll serves it.
	done = true
	link, err := queued.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if err != nil || link != "https://pm/done" {
		t.Errorf("a completed transfer must be served: %q %v", link, err)
	}
	if alreadyQueued(queued.cache, "pending", H) {
		t.Error("the marker outlived the transfer it described")
	}
}

// A remembered Real-Debrid torrent is per FILE, and its link is the one for the file selected.
//
// One infohash is a whole season pack. Reusing a torrent id across episodes re-selected files on a
// torrent RD had already started and then served links[0] regardless — S01E01's link for a request for
// S01E02. Silently the wrong episode, which is worse than the add it saved.
func TestRealDebrid_servesTheEpisodeThatWasAskedFor(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	// A pack RD has already fully selected: two files, two links, in file order.
	pack := `{"files":[{"id":1,"path":"/Show.S01E01.mkv","bytes":9,"selected":1},
	                   {"id":2,"path":"/Show.S01E02.mkv","bytes":9,"selected":1}],
	          "links":["https://rd/E01","https://rd/E02"]}`
	var unrestricted string
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "addMagnet"):
			return resp(201, `{"id":"t1"}`), nil
		case strings.Contains(r.URL.Path, "/torrents/info/"):
			return resp(200, pack), nil
		case strings.Contains(r.URL.Path, "selectFiles"):
			return resp(204, ""), nil
		case strings.Contains(r.URL.Path, "unrestrict/link"):
			b, _ := io.ReadAll(r.Body)
			unrestricted = string(b)
			return resp(200, `{"download":"https://rd/dl"}`), nil
		}
		return resp(404, "{}"), nil
	}}
	s := &realDebridStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: realDebridAPI}

	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(1)}); err != nil {
		t.Fatalf("E01: %v", err)
	}
	if !strings.Contains(unrestricted, "E01") {
		t.Errorf("E01 unrestricted %q", unrestricted)
	}
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)}); err != nil {
		t.Fatalf("E02: %v", err)
	}
	if !strings.Contains(unrestricted, "E02") {
		t.Errorf("E02 unrestricted %q — the pack's first link was served for the second episode", unrestricted)
	}
}

// rdLinkFor maps a file id onto RD's links, which describe the SELECTED files in file order.
func TestRDLinkFor(t *testing.T) {
	info := &rdInfo{Links: []string{"L-b", "L-d"}}
	info.Files = append(info.Files,
		struct {
			ID       int    `json:"id"`
			Path     string `json:"path"`
			Bytes    int    `json:"bytes"`
			Selected int    `json:"selected"`
		}{ID: 1, Path: "a", Selected: 0},
		struct {
			ID       int    `json:"id"`
			Path     string `json:"path"`
			Bytes    int    `json:"bytes"`
			Selected int    `json:"selected"`
		}{ID: 2, Path: "b", Selected: 1},
		struct {
			ID       int    `json:"id"`
			Path     string `json:"path"`
			Bytes    int    `json:"bytes"`
			Selected int    `json:"selected"`
		}{ID: 3, Path: "c", Selected: 0},
		struct {
			ID       int    `json:"id"`
			Path     string `json:"path"`
			Bytes    int    `json:"bytes"`
			Selected int    `json:"selected"`
		}{ID: 4, Path: "d", Selected: 1},
	)
	if link, ok := rdLinkFor(info, 2); !ok || link != "L-b" {
		t.Errorf("first selected file → first link: %q %v", link, ok)
	}
	if link, ok := rdLinkFor(info, 4); !ok || link != "L-d" {
		t.Errorf("second selected file → second link: %q %v", link, ok)
	}
	// An unselected file has no link of its own; guessing one is what served the wrong episode.
	if _, ok := rdLinkFor(info, 3); ok {
		t.Error("an unselected file must not be given someone else's link")
	}
	// A leaner response with no selection flags and exactly one link has only one possible answer.
	lean := &rdInfo{Links: []string{"only"}}
	lean.Files = append(lean.Files, struct {
		ID       int    `json:"id"`
		Path     string `json:"path"`
		Bytes    int    `json:"bytes"`
		Selected int    `json:"selected"`
	}{ID: 7, Path: "x"})
	if link, ok := rdLinkFor(lean, 7); !ok || link != "only" {
		t.Errorf("single-link fallback: %q %v", link, ok)
	}
}

// The two keys HEAD added carry account state and must be scoped to the token like every other one.
func TestCacheKeys_theNewOnesAreScopedToo(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  func(token string) string
	}{
		{"rdTorrent", func(tok string) string { return rdTorrentKey(tok, H, ResolveTarget{InfoHash: H}) }},
		{"pmQueued", func(tok string) string { return pmQueuedKey(tok, H) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mine, theirs := tc.key("my-token"), tc.key("their-token")
			if mine == theirs {
				t.Fatalf("two accounts share the key %q", mine)
			}
			if strings.Contains(mine, "my-token") || !strings.Contains(mine, keyHash("my-token")) {
				t.Errorf("key not derived from the account without exposing it: %q", mine)
			}
		})
	}
	// And the RD key separates episodes of one pack, or a season pack serves one episode for all of them.
	e1 := rdTorrentKey("tok", H, ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(1)})
	e2 := rdTorrentKey("tok", H, ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if e1 == e2 {
		t.Error("two episodes of one pack share a remembered torrent")
	}
}

// Premiumize's "coming" has a deadline, and a release it already holds costs nothing.
//
// Two failures either side of the same line. Refreshing the marker on every poll made 202 an absorbing
// state: a dead magnet answered "downloading 0%" forever and the client could never fall through. And
// charging for every directdl billed an add per play of a release the account already had — directdl is
// a purchase for what Premiumize lacks and a plain read for what it holds, and only the answer says
// which.
func TestPremiumize_pendingExpiresAndHeldReleasesAreFree(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()
	acct := budgetAccount(ServicePremiumize, "tok")

	// A release Premiumize holds: served, and net zero against the allowance however often it is played.
	cache := NewMemoryCache(1 << 20)
	held := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return resp(200, `{"status":"success","content":[{"path":"m.mkv","link":"https://pm/l","size":9}]}`), nil
		}}}
	for i := 0; i < 24; i++ { // a season of a pack the account already has
		if _, err := held.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err != nil {
			t.Fatalf("a held release must serve: %v", err)
		}
	}
	if left := globalAddBudget.remaining(acct); left != 50 {
		t.Errorf("allowance %d after 24 plays of a release the account already holds", left)
	}

	// A transfer that never produces anything: "coming" until the deadline, then dead so the client can
	// try something else.
	pending := NewMemoryCache(1 << 20)
	stuck := &premiumizeStore{token: "tok", cache: pending, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return resp(200, `{"status":"success","content":[]}`), nil
		}}}
	if _, err := stuck.Resolve(context.Background(), ResolveTarget{InfoHash: H}); !errors.Is(err, errAddInFlight) {
		t.Fatalf("a freshly queued transfer is coming: %v", err)
	}
	// Backdate the stamp past the give-up window — the marker records WHEN, so it can age.
	pending.Put(pmQueuedKey("tok", H), strconv.FormatInt(time.Now().Add(-pendingGiveUp-time.Minute).Unix(), 10), queuedTTL)
	_, err := stuck.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if errors.Is(err, errAddInFlight) {
		t.Error("a transfer that never arrived is still reported as coming; the client cannot move on")
	}
	if err == nil {
		t.Error("it produced nothing, so it cannot be a success either")
	}
}

// A refused key is the SERVICE declining this account, not a verdict on the release — so it backs off
// instead of paying for another attempt every poll.
func TestStoreRefusedUs_coversAnExpiredKey(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusInternalServerError} {
		if !storeRefusedUs(code) {
			t.Errorf("http %d is the service refusing us, not a dead release", code)
		}
	}
	for _, code := range []int{http.StatusOK, http.StatusNotFound} {
		if storeRefusedUs(code) {
			t.Errorf("http %d is an answer about the release, not a refusal", code)
		}
	}

	// End to end: a stale token must cost one add and one backoff, not one per poll.
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	calls := 0
	cache := NewMemoryCache(1 << 20)
	s := &premiumizeStore{token: "stale", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			calls++
			return resp(403, `{"status":"error","message":"invalid apikey"}`), nil
		}}}
	for i := 0; i < 10; i++ {
		_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	}
	if calls != 1 {
		t.Errorf("asked a service with a bad key %d times", calls)
	}
	if left := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "stale")); left < 49 {
		t.Errorf("a stale key drained the allowance to %d", left)
	}
}

// A pack Premiumize ALREADY HOLDS costs nothing, even when it cannot serve the episode asked for.
//
// The refund sat behind the file-selection checks, so the two failures a season pack actually produces —
// the pack does not contain the requested episode, or the entry carries no link — returned before it.
// Every poll charged another add while queueing nothing: twenty polls, twenty adds, the hourly allowance
// gone in under two minutes, on the branch season packs always take. The movie branch was fine, and the
// movie branch was all that was tested.
func TestPremiumize_aHeldPackCostsNothingEvenWhenItCannotServeTheEpisode(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	// The account holds this pack; it just does not contain S01E09.
	d := mockDoer{fn: func(*http.Request) (*http.Response, error) {
		return resp(200, `{"status":"success","content":[
			{"path":"Show.S01E01.mkv","link":"https://pm/1","size":9},
			{"path":"Show.S01E02.mkv","link":"https://pm/2","size":9}]}`), nil
	}}
	s := &premiumizeStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: premiumizeAPI}
	for i := 0; i < 20; i++ {
		if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(9)}); err == nil {
			t.Fatal("the pack does not hold that episode; it must not resolve")
		}
	}
	if left := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "tok")); left != 50 {
		t.Errorf("allowance %d after twenty polls of a pack the account already holds", left)
	}
}

// A key the service refuses is an ACCOUNT condition, so it backs the account off — not one release at a
// time while every other release pays for the same 403.
//
// Keyed per infohash, sixty distinct releases made fifty calls and emptied the hourly allowance, and
// replacing the key did not restore service because the budget stayed spent for the rest of the hour.
func TestStoreRefusal_aRejectedKeyBacksOffTheWholeAccount(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	calls := 0
	cache := NewMemoryCache(1 << 20)
	s := &premiumizeStore{token: "stale", client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
		calls++
		return resp(403, `{"status":"error","message":"invalid apikey"}`), nil
	}}, cache: cache, api: premiumizeAPI}

	for i := 0; i < 60; i++ { // sixty DIFFERENT releases, as a browse would produce
		_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: fmt.Sprintf("%040x", i)})
	}
	if calls != 1 {
		t.Errorf("asked a service that had rejected the key %d times", calls)
	}
	if left := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "stale")); left < 49 {
		t.Errorf("a rejected key drained the allowance to %d; replacing it would not restore service", left)
	}
	// A per-release refusal must still be per-release, or one dead torrent silences the account.
	perRelease := NewMemoryCache(1 << 20)
	recordRefusal(perRelease, ServiceTorBox, "tok", H, &StoreUnavailableError{Service: ServiceTorBox, Reason: "createtorrent http 429"})
	if _, off := backedOff(perRelease, ServiceTorBox, "tok", repeat("c", 40)); off {
		t.Error("a 429 about one release backed off the whole account")
	}
}

// The give-up deadline sits strictly inside the marker's life, or it can never be reached: the marker
// expires first, the next poll queues the same doomed transfer, and "coming" becomes absorbing again.
func TestPremiumize_giveUpHappensBeforeTheMarkerExpires(t *testing.T) {
	if pendingGiveUp >= queuedTTL {
		t.Fatalf("pendingGiveUp (%s) must be shorter than queuedTTL (%s), or the deadline is unreachable",
			pendingGiveUp, queuedTTL)
	}
	cache := NewMemoryCache(1 << 20)
	// Stamped a moment ago: still believable.
	cache.Put(pmQueuedKey("tok", H), strconv.FormatInt(time.Now().Unix(), 10), queuedTTL)
	if pendingTooLong(cache, "tok", H) {
		t.Error("a transfer queued seconds ago has not had its chance yet")
	}
	// Stamped just inside the marker's life but past the deadline: no longer believable, and reachable.
	old := time.Now().Add(-(queuedTTL - time.Minute))
	cache.Put(pmQueuedKey("tok", H), strconv.FormatInt(old.Unix(), 10), queuedTTL)
	if !pendingTooLong(cache, "tok", H) {
		t.Error("a transfer still marked but long past its deadline is being reported as coming")
	}
}

// Giving up on a stuck transfer is REMEMBERED, not merely reached.
//
// Without that, the marker expired at queuedTTL and the next poll queued the same doomed transfer for
// another ten-minute window — and the client keeps polling a release it was told was dead, so it would.
func TestPremiumize_theGiveUpVerdictIsRemembered(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	cache := NewMemoryCache(1 << 20)
	s := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return resp(200, `{"status":"success","content":[]}`), nil
		}}}
	// Queued long ago and still producing nothing.
	old := time.Now().Add(-(queuedTTL - time.Minute))
	cache.Put(pmQueuedKey("tok", H), strconv.FormatInt(old.Unix(), 10), queuedTTL)

	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err == nil {
		t.Fatal("a transfer past its deadline is not a success")
	}

	// Remembered — but in the QUEUE marker, never as a store refusal. Filing it as a refusal made every
	// following poll a 503 naming Premiumize, which tells the viewer their debrid is refusing and stops
	// the client trying other sources — for a release scout itself condemned, on a healthy store. The
	// whole point of the dead link is that the client moves on.
	if _, refused := backedOff(cache, ServicePremiumize, "tok", H); refused {
		t.Error("the give-up was filed as a store refusal; the client can no longer fall through")
	}
	for i := 0; i < 5; i++ {
		_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
		var unavailable *StoreUnavailableError
		if errors.As(err, &unavailable) {
			t.Fatalf("poll %d blamed the store for a verdict scout reached: %v", i, err)
		}
		if err == nil {
			t.Fatalf("poll %d resolved a transfer that produced nothing", i)
		}
	}
	// And it is not re-queued: the marker keeps suppressing the charge for its full life.
	if left := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "tok")); left != 50 {
		t.Errorf("allowance %d — a condemned transfer was queued again", left)
	}
}

// Whether a refusal is about the ACCOUNT is decided by the status code, not by matching digits in prose.
//
// The reason string embeds the service's own words — including up to 200 bytes of a non-JSON body — so
// substring-matching "401"/"403" classified a rate-limit detail ("retry in 403 ms") and a Cloudflare Ray
// ID as a rejected key, and silenced a healthy account for a minute while logging that the key was bad.
// Deriving a fact from prose when the fact is available at the call site is what isPermanentFailure was
// deleted for.
func TestRefusalIsAboutTheAccount_readsTheStatusNotTheProse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		wantAc bool
	}{
		{"a rejected key", &StoreUnavailableError{Service: ServiceTorBox, Status: 403,
			Reason: "createtorrent http 403 (FORBIDDEN)"}, true},
		{"an expired key", &StoreUnavailableError{Service: ServiceTorBox, Status: 401,
			Reason: "createtorrent http 401"}, true},
		{"a rate limit whose DETAIL contains 403", &StoreUnavailableError{Service: ServiceTorBox, Status: 429,
			Reason: "createtorrent http 429 (RATE_LIMIT: slow down, retry in 403 ms)"}, false},
		{"a 502 body carrying a Ray ID with 401 in it", &StoreUnavailableError{Service: ServiceTorBox, Status: 502,
			Reason: "createtorrent http 502 (Cloudflare Ray ID: 8a1b401c9d2e4f01)"}, false},
		{"a queue depth that looks like a status", &StoreUnavailableError{Service: ServiceTorBox, Status: 429,
			Reason: "createtorrent http 429 (queued behind 4031 jobs)"}, false},
	} {
		if got := refusalIsAboutTheAccount(tc.err); got != tc.wantAc {
			t.Errorf("%s: account-level = %v, want %v", tc.name, got, tc.wantAc)
		}
	}

	// End to end: a 429 whose text contains 403 must back off ONE release, not the account.
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServiceTorBox, "tok", H, &StoreUnavailableError{Service: ServiceTorBox, Status: 429,
		Reason: "createtorrent http 429 (retry in 403 ms)"})
	if _, off := backedOff(cache, ServiceTorBox, "tok", repeat("c", 40)); off {
		t.Error("a rate limit about one release silenced the whole account")
	}
}

// The add-path guards must not block a read.
//
// A release the account already holds needs no add, so a backoff — which describes the cost of adding,
// and which is now account-wide for a rejected key — must not stand in its way. TorBox has ordered it
// this way all along; Real-Debrid and Premiumize checked the backoff first, so one 401 on an unrelated
// release blocked every release the account held, read-only probes included.
func TestResolve_addGuardsDoNotBlockAReadPath(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	cache := NewMemoryCache(1 << 20)
	// A PER-RELEASE add backoff on some other release: a throttle, not a rejected key. This is the kind a
	// read cannot have caused and must not be blocked by. (An account-level 401/403 is different, and
	// does gate reads — asking again with a rejected key is pointless whatever is being asked for.)
	recordRefusal(cache, ServiceRealDebrid, "tok", repeat("f", 40),
		&StoreUnavailableError{Service: ServiceRealDebrid, Status: 429, Reason: "addmagnet http 429"})

	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/torrents/info/"):
			return resp(200, `{"files":[{"id":1,"path":"/movie.mkv","bytes":9,"selected":1}],"links":["https://rd/r"]}`), nil
		case strings.Contains(r.URL.Path, "selectFiles"):
			return resp(204, ""), nil
		case strings.Contains(r.URL.Path, "unrestrict/link"):
			return resp(200, `{"download":"https://rd/dl.mkv"}`), nil
		}
		return resp(404, "{}"), nil
	}}
	s := &realDebridStore{token: "tok", client: d, cache: cache, api: realDebridAPI}
	target := ResolveTarget{InfoHash: H}
	s.rememberTorrent(target, "t1") // this one is already bought

	link, err := s.Resolve(context.Background(), target)
	if err != nil || link != "https://rd/dl.mkv" {
		t.Errorf("a release the account already holds was blocked by an add-path backoff: %q %v", link, err)
	}

	// A NoAdd caller is likewise answered before the backoff, on a release that is NOT held.
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: repeat("b", 40), NoAdd: true}); !errors.Is(err, errWouldAdd) {
		t.Errorf("a read-only resolve was blocked by an add-path guard: %v", err)
	}

	// Premiumize orders it the same way. It has no held-torrent path, so NoAdd is the read here — and it
	// must be answered before an account backoff it cannot have caused.
	pmCache := NewMemoryCache(1 << 20)
	recordRefusal(pmCache, ServicePremiumize, "tok", repeat("f", 40),
		&StoreUnavailableError{Service: ServicePremiumize, Status: 429, Reason: "directdl http 429"})
	pm := &premiumizeStore{token: "tok", cache: pmCache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			t.Error("a NoAdd resolve must not reach the service at all")
			return resp(200, `{}`), nil
		}}}
	if _, err := pm.Resolve(context.Background(), ResolveTarget{InfoHash: H, NoAdd: true}); !errors.Is(err, errWouldAdd) {
		t.Errorf("premiumize blocked a read-only resolve with an add-path backoff: %v", err)
	}
}

// A transfer already queued is never PAID FOR again, however long it goes on failing.
//
// The bound that matters is the charge, not the call: directdl is both the purchase and the only way to
// notice the transfer finished, so it has to keep being made — an earlier version suppressed the call
// instead and made a late-completing transfer permanently unplayable.
func TestPremiumize_aQueuedTransferIsNeverChargedTwice(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	calls := 0
	cache := NewMemoryCache(1 << 20)
	s := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			calls++
			return resp(200, `{"status":"success","content":[]}`), nil
		}}}
	for i := 0; i < 30; i++ {
		if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err == nil {
			t.Fatalf("poll %d resolved a transfer that produced nothing", i)
		}
	}
	if calls != 30 {
		t.Errorf("asked %d times; the call is what notices a transfer finishing and must keep happening", calls)
	}
	// One charge for the one transfer that was genuinely queued, and none for the twenty-nine repeats.
	if left := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "tok")); left != 49 {
		t.Errorf("allowance %d — a queued transfer was paid for more than once", left)
	}
}

// A rejected key DOES gate a read: asking again with a key the service refuses is pointless whatever is
// being asked for, and without this the read path recorded the refusal and asked again on the next poll.
func TestResolve_aRejectedKeyGatesEvenAReadPath(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServiceRealDebrid, "tok", repeat("f", 40),
		&StoreUnavailableError{Service: ServiceRealDebrid, Status: 403, Reason: "addmagnet http 403"})
	s := &realDebridStore{token: "tok", cache: cache, api: realDebridAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			t.Error("a rejected key must stop the request before it is made")
			return resp(200, `{}`), nil
		}}}
	target := ResolveTarget{InfoHash: H}
	s.rememberTorrent(target, "t1") // even a release the account holds
	if _, err := s.Resolve(context.Background(), target); err == nil {
		t.Error("a rejected key cannot serve anything")
	}
}

// Real-Debrid's READ path classifies a refusal as a refusal.
//
// Promoting the held-torrent path ahead of the backoff copied TorBox's ordering without the
// classification that makes it safe: RD mapped every non-2xx to a dead link, so an expired key or a
// throttle — on a release RD demonstrably holds — reached the app as "this release does not exist", with
// no backoff recorded and the account backoff shadowed by the very reordering.
func TestRealDebrid_theReadPathReportsARefusalAsARefusal(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	calls := 0
	cache := NewMemoryCache(1 << 20)
	s := &realDebridStore{token: "tok", cache: cache, api: realDebridAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			calls++
			return resp(403, `{"error":"bad_token"}`), nil
		}}}
	target := ResolveTarget{InfoHash: H}
	s.rememberTorrent(target, "t1") // the account holds this one

	_, err := s.Resolve(context.Background(), target)
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("a rejected key is the service refusing, not a dead release: %v", err)
	}
	// Remembered, so the poll loop stops — and account-wide, because a 403 is about the key.
	for i := 0; i < 9; i++ {
		_, _ = s.Resolve(context.Background(), target)
	}
	if calls != 1 {
		t.Errorf("asked a service that had rejected the key %d times", calls)
	}
	if _, off := backedOff(cache, ServiceRealDebrid, "tok", repeat("c", 40)); !off {
		t.Error("a rejected key must back off the account, not one release")
	}
}

// A transfer Premiumize LATER completes must still be discoverable.
//
// Answering a condemned transfer without asking looked like a saving and was not: directdl is the only
// thing that can observe the transfer finishing, so short-circuiting it made a 4K remux slower than the
// give-up window permanently unplayable until the marker expired. The call is already unbilled once the
// transfer is queued, so suppressing it bought nothing.
func TestPremiumize_aSlowTransferIsStillDiscoveredWhenItCompletes(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	done := false
	calls := 0
	cache := NewMemoryCache(1 << 20)
	s := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			calls++
			if done {
				return resp(200, `{"status":"success","content":[{"path":"m.mkv","link":"https://pm/late","size":9}]}`), nil
			}
			return resp(200, `{"status":"success","content":[]}`), nil
		}}}
	// Queued longer ago than the give-up window: it reads as dead, but is still being asked about.
	cache.Put(pmQueuedKey("tok", H), strconv.FormatInt(time.Now().Add(-(pendingGiveUp+time.Minute)).Unix(), 10), queuedTTL)
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err == nil {
		t.Fatal("past its deadline it is not a success")
	}
	if calls != 1 {
		t.Fatalf("a condemned transfer must still be asked about: %d calls", calls)
	}

	// It finishes. The very next poll serves it.
	done = true
	link, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if err != nil || link != "https://pm/late" {
		t.Errorf("a transfer that completed after the deadline was never discovered: %q %v", link, err)
	}
	// And it cost nothing extra: the transfer was already paid for.
	if left := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "tok")); left != 50 {
		t.Errorf("allowance %d — an already-queued transfer was charged again", left)
	}
}

// The READ path records only a SERVICE refusal — not a torrent that is merely not ready, and not a
// cancelled poll.
//
// requestDownload flattens every transport failure into a DeadLinkError on purpose, to keep the token
// out of the logs, which destroys the classification isCancellation needs. Recorded broadly, a cancelled
// poll and a still-downloading torrent were both filed as TorBox refusing: the probe route then told the
// viewer their debrid was refusing and stopped the client trying other sources, and a read-only probe
// created a backoff its own contract says it cannot cause.
func TestTorBox_theReadPathRecordsOnlyARealRefusal(t *testing.T) {
	held := func(cache Cache, d doer) *torBoxStore {
		s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}
		cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
		return s
	}
	body := func(status int, payload string) mockDoer {
		return mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "requestdl") {
				return resp(status, payload), nil
			}
			return resp(200, `{"data":{"files":[{"id":0,"name":"m.mkv","size":9}]}}`), nil
		}}
	}
	for _, tc := range []struct {
		name       string
		doer       mockDoer
		wantRecord bool
	}{
		{"still downloading", body(200, `{"success":false}`), false},
		{"a cancelled poll", mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "requestdl") {
				return nil, context.Canceled
			}
			return resp(200, `{"data":{"files":[{"id":0,"name":"m.mkv","size":9}]}}`), nil
		}}, false},
		{"the service refusing", body(429, `{"error":"RATE"}`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewMemoryCache(1 << 20)
			s := held(cache, tc.doer)
			_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)})
			_, recorded := backedOff(cache, ServiceTorBox, "tok", H)
			if recorded != tc.wantRecord {
				t.Errorf("recorded=%v, want %v — this decides whether the app blames the debrid", recorded, tc.wantRecord)
			}
		})
	}
}

// TorBox gets the same account-level gate as the other two: a rejected key makes every request
// pointless, reads included.
func TestTorBox_aRejectedKeyGatesTheReadPathToo(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServiceTorBox, "tok", repeat("f", 40),
		&StoreUnavailableError{Service: ServiceTorBox, Status: 403, Reason: "createtorrent http 403"})
	cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			t.Error("a rejected key must stop the request before it is made")
			return resp(200, `{}`), nil
		}}}
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)}); err == nil {
		t.Error("a rejected key cannot serve even a held torrent")
	}
}

// Premiumize's account gate, which nothing pinned: the existing ordering test seeds a 429, which is a
// per-release refusal and never reaches this branch.
func TestPremiumize_aRejectedKeyGatesEvenAReadPath(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServicePremiumize, "tok", repeat("f", 40),
		&StoreUnavailableError{Service: ServicePremiumize, Status: 401, Reason: "directdl http 401"})
	s := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			t.Error("a rejected key must stop the request before it is made")
			return resp(200, `{}`), nil
		}}}
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err == nil {
		t.Error("a rejected key cannot serve anything")
	}
}

// Premiumize answers an application-level refusal as HTTP 200 `{"status":"error",…}` — an unsupported
// magnet, an account limit, no space. Only a SUCCESSFUL answer carrying no content means a transfer was
// queued. Collapsing the two charged an add for a transfer that does not exist, stamped the queue marker
// for its full twenty minutes so a later legitimate attempt was suppressed as "already queued", and made
// /play answer 202 "downloading, 0%" for ten minutes — a spinner the client cannot fall through — for a
// magnet Premiumize had refused outright.
func TestPremiumize_aRefusedMagnetIsNotAQueuedTransfer(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	cache := NewMemoryCache(1 << 20)
	s := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return resp(200, `{"status":"error","message":"Invalid src"}`), nil
		}}}
	var err error
	for i := 0; i < 5; i++ {
		_, err = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	}
	if errors.Is(err, errAddInFlight) {
		t.Errorf("a refused magnet was reported as a transfer on its way: %v", err)
	}
	var dead *DeadLinkError
	if !errors.As(err, &dead) {
		t.Fatalf("got %v, want a dead link the client can move past", err)
	}
	// Its own words, so "Invalid src" and "not enough space" stay tellable apart.
	if !strings.Contains(dead.Error(), "Invalid src") {
		t.Errorf("the service's own explanation was discarded: %q", dead.Error())
	}
	if alreadyQueued(cache, "tok", H) {
		t.Error("a magnet Premiumize refused was marked as queued, blocking any retry for twenty minutes")
	}
	if left := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "tok")); left != 50 {
		t.Errorf("allowance %d — refusals were charged as adds", left)
	}
}

// Real-Debrid's read path records ONLY a service refusal, the same narrowing TorBox's has. `info` also
// hands back raw transport errors, a bare `info http 400`, and `bad info` for a non-JSON 200 (a
// Cloudflare interstitial). Filing those as refusals made a TCP reset answer 503 store_unavailable
// naming realdebrid — which stops the client trying other sources — for a release RD demonstrably holds,
// and because this path runs on every poll and re-stamps the record, it never lapsed.
func TestRealDebrid_onlyAServiceRefusalIsRecordedOnTheReadPath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		doer       mockDoer
		wantRecord bool
	}{
		{"a transport blip", mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("read tcp 10.0.0.2:443: connection reset by peer")
		}}, false},
		{"a 400", mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return resp(400, `{}`), nil
		}}, false},
		{"an interstitial", mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return resp(200, `<html>just a moment</html>`), nil
		}}, false},
		{"the service refusing", mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return resp(403, `{"error":"bad_token"}`), nil
		}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewMemoryCache(1 << 20)
			s := &realDebridStore{token: "tok", cache: cache, api: realDebridAPI, client: tc.doer}
			target := ResolveTarget{InfoHash: H}
			s.rememberTorrent(target, "t1")
			if _, err := s.Resolve(context.Background(), target); err == nil {
				t.Fatal("nothing here resolves")
			}
			if _, recorded := backedOff(cache, ServiceRealDebrid, "tok", H); recorded != tc.wantRecord {
				t.Errorf("recorded=%v, want %v — this decides whether the app blames the debrid",
					recorded, tc.wantRecord)
			}
		})
	}
}

// The account gate has to sit ABOVE the warm resolve entry, not below it. Any pack played in the last
// six hours takes that fast path and returns without reaching a gate placed after it — the normal state
// during a binge — so ten polls made ten requestdl calls against a key TorBox had already rejected.
func TestTorBox_theAccountGateCoversTheWarmFastPath(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServiceTorBox, "tok", repeat("f", 40),
		&StoreUnavailableError{Service: ServiceTorBox, Status: 403, Reason: "createtorrent http 403"})
	cache.Put(resolveKey("tok", H),
		`{"torrentId":42,"files":[{"Index":0,"Name":"Show.S01E01.mkv","SizeBytes":900}]}`, resolveCacheTTL)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			t.Error("a rejected key must stop the request before it is made, warm entry or not")
			return resp(200, `{}`), nil
		}}}
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)}); err == nil {
		t.Error("a rejected key cannot serve even a pack played minutes ago")
	}
}

// The read path keeps the service's own words, the same rule the add path follows: an account at its
// active-download limit and a torrent id TorBox cannot serve are both a bare 400, and only the text says
// which. Discarding it left the read path unable to tell a genuine refusal from a dead link, so it
// recorded nothing and re-asked on every poll.
func TestTorBox_theReadPathKeepsTheServicesOwnWords(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "requestdl") {
				return resp(400, `{"error":"DOWNLOAD_SERVER_ERROR","detail":"active download limit reached"}`), nil
			}
			return resp(200, `{"data":{"files":[{"id":0,"name":"m.mkv","size":9}]}}`), nil
		}}}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0), NoAdd: true})
	if err == nil || !strings.Contains(err.Error(), "active download limit reached") {
		t.Errorf("got %v — the answer that says WHY was thrown away one layer down", err)
	}
}

// A cached file list that names its episodes is proof about the pack, not a hint: if the requested one
// is not among them, adding the torrent again only re-learns the same thing, on an account with an
// hourly ceiling on adds.
func TestTorBox_aProvenWrongPackIsNotReAdded(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	cache.Put(resolveKey("tok", H),
		`{"torrentId":42,"files":[{"Index":0,"Name":"Show.S01E01.mkv","SizeBytes":900},`+
			`{"Index":1,"Name":"Show.S01E02.mkv","SizeBytes":950}]}`, resolveCacheTTL)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			t.Errorf("the cached list already proves this pack is wrong; %s was asked anyway", r.URL.Path)
			return resp(200, `{}`), nil
		}}}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(9)})
	if !errors.Is(err, errEpisodeNotInTorrent) {
		t.Errorf("got %v, want errEpisodeNotInTorrent", err)
	}
}

// Every answer Premiumize gives that is not a 2xx is one that queued nothing, so the charge goes back.
// Keeping it was the expensive mistake: the allowance is a rolling in-memory window with no reset but a
// restart, so one bad magnet polled every two seconds spent all fifty in about a hundred seconds — after
// which spendAdd refused EVERY Premiumize resolve for the rest of the hour, including releases the
// account already holds that directdl would have served instantly. A refused magnet is a nuisance; an
// hour of 503 scout_busy on a working store is an outage.
func TestPremiumize_anAnsweredFailureIsNotACharge(t *testing.T) {
	for _, status := range []int{400, 402, 404, 429, 500} {
		t.Run(fmt.Sprintf("http %d", status), func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			cache := NewMemoryCache(1 << 20)
			s := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
				client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
					return resp(status, `{"status":"error","message":"nope"}`), nil
				}}}
			for i := 0; i < 60; i++ {
				_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			}
			if left := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "tok")); left != 50 {
				t.Fatalf("allowance %d — a poll loop on an answered failure spent the hour's adds", left)
			}
			// The allowance still being there is only half of it: what it buys is that ANOTHER release —
			// one the account holds, which directdl serves as a read — still resolves, instead of being
			// refused by scout's own bookkeeping for the rest of the hour. A different infohash, because
			// a 429/5xx correctly backs THIS release off for a minute; the damage being pinned here is
			// the account-wide one.
			held := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
				client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
					return resp(200, `{"status":"success","content":[{"path":"m.mkv","link":"https://pm/ok"}]}`), nil
				}}}
			other := ResolveTarget{InfoHash: repeat("b", 40)}
			if link, err := held.Resolve(context.Background(), other); link != "https://pm/ok" {
				t.Errorf("a held release answered %q %v — scout refused what Premiumize would have served", link, err)
			}
		})
	}
}

// A blip listing a pack's files must never fall through to the indexer's positional index. listFiles
// answers nil for a transport error, a non-200 and an unreadable body alike, and that nil reached
// selectFileID, where the raw fileIdx is used as a TorBox file id — serving S01E01 for a request for
// S01E02 with a 302 and no error. The cache-read path already refuses on the same evidence.
func TestTorBox_aFailedFileListNeverGuessesTheEpisode(t *testing.T) {
	list := func(fn func() (*http.Response, error)) mockDoer {
		return mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "mylist") {
				return fn()
			}
			return resp(200, `{"success":true,"data":"https://tb/link"}`), nil
		}}
	}
	for _, tc := range []struct {
		name string
		doer mockDoer
	}{
		{"a transport blip", list(func() (*http.Response, error) { return nil, fmt.Errorf("connection reset") })},
		{"a 500", list(func() (*http.Response, error) { return resp(500, `{}`), nil })},
		{"an unreadable body", list(func() (*http.Response, error) { return resp(200, `<html>`), nil })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewMemoryCache(1 << 20)
			cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
			s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI, client: tc.doer}
			// The indexer says file 0; the pack's own names would have said file 1.
			link, err := s.Resolve(context.Background(),
				ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2), FileIdx: intp(0)})
			if link != "" {
				t.Fatalf("served %q blind — this is how the wrong episode plays", link)
			}
			var unavailable *StoreUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("got %v, want the store saying it could not answer", err)
			}
			// Status still has to be able to report the download, so the id must survive the refusal.
			if _, ok := cache.Get(torrentIDKey("tok", H)); !ok {
				t.Error("the torrent id was dropped, so a fetch in progress now reads as dead")
			}
		})
	}
}

// The one TorBox URL carrying the token as a query parameter must not gain a second route into the log
// through an upstream error page that quotes the request URI back.
func TestTorBox_theServicesWordsCannotCarryTheToken(t *testing.T) {
	const token = "supersecrettoken"
	cache := NewMemoryCache(1 << 20)
	// Seeded under the store's OWN token: the id key is account-scoped, so seeding it under any other
	// name leaves the account holding nothing and the request is never made.
	cache.Put(torrentIDKey(token, H), "42", resolveCacheTTL)
	reached := false
	s := &torBoxStore{token: token, cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "requestdl") {
				reached = true
				return resp(400, `Bad Request on `+r.URL.String()), nil
			}
			return resp(200, `{"data":{"files":[{"id":0,"name":"m.mkv","size":9}]}}`), nil
		}}}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0), NoAdd: true})
	if !reached {
		t.Fatal("requestdl was never called, so this asserts nothing")
	}
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("the token reached an error that gets logged: %v", err)
	}
}

// An empty needle would splice the replacement between every character, turning a short error from an
// unconfigured store into an unreadable one.
func TestRedactToken_anEmptyTokenLeavesTextAlone(t *testing.T) {
	if got := redactToken("http 400 (nope)", ""); got != "http 400 (nope)" {
		t.Errorf("got %q", got)
	}
	if got := redactToken("http 400 (tok=abc)", "abc"); got != "http 400 (tok=<token>)" {
		t.Errorf("got %q", got)
	}
}

// The series path must heal from a deleted torrent exactly as the movie path does. Refusing to guess an
// episode fires BEFORE requestDownload, whose 404 was the only producer of errTorrentGone and so the
// only thing that could forget the id and re-add — so a pack deleted account-side answered 503
// store_unavailable on every poll, forever: no backoff to lapse, the six-hour id refreshed each time,
// and the tiered cache writing it through to disk so a restart did not clear it either. The distinction
// that fixes it is that mylist ANSWERING "no such torrent" is not the same as mylist not answering.
func TestTorBox_aDeletedPackIsReBoughtForASeriesToo(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	gone := true
	adds := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case isAddEndpoint(r):
			adds++
			gone = false // re-bought: the account has it again
			return resp(200, `{"data":{"torrent_id":77}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			if gone {
				// TorBox's own words for an id it no longer holds — a 200, not an error.
				return resp(200, `{"success":false,"data":null}`), nil
			}
			return resp(200, `{"data":{"files":[{"id":0,"name":"Show.S01E01.mkv","size":900},`+
				`{"id":1,"name":"Show.S01E02.mkv","size":950}]}}`), nil
		}
		return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
	}}
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL) // a stale id from before the deletion
	s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}

	link, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if err != nil || link == "" {
		t.Fatalf("a deleted pack must be re-bought, not answered unavailable forever: %q %v", link, err)
	}
	if adds != 1 {
		t.Errorf("made %d adds; the stale id should have been forgotten and the pack re-added", adds)
	}
	if raw, _ := cache.Get(torrentIDKey("tok", H)); raw == "42" {
		t.Error("the stale id survived, and every later poll would keep it alive for another six hours")
	}
}

// The two answers must stay apart in the other direction too: a mylist that does not answer is NOT
// grounds to forget the id and buy the torrent again, or a blip costs an add against the hourly ceiling
// and Status loses the id it needs to report the download in progress.
func TestTorBox_aBlipListingFilesDoesNotReBuyThePack(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	adds := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case isAddEndpoint(r):
			adds++
			return resp(200, `{"data":{"torrent_id":77}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			return resp(500, `{}`), nil
		}
		return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
	}}
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
	s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}

	for i := 0; i < 10; i++ {
		if link, _ := s.Resolve(context.Background(),
			ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)}); link != "" {
			t.Fatalf("poll %d served %q without ever seeing the pack's file names", i, link)
		}
	}
	if adds != 0 {
		t.Errorf("made %d adds; a blip listing files says nothing about what the account holds", adds)
	}
	if raw, _ := cache.Get(torrentIDKey("tok", H)); raw != "42" {
		t.Errorf("the id is %q — dropped on a blip, so Status can no longer report the download", raw)
	}
}

// Premiumize's error text is not merely logged: the refused branch PERSISTS it into the refusal cache,
// from where the probe route prints it verbatim on every later poll. The apikey rides in directdl's form
// body, so an upstream error page quoting the request back would put it there to stay.
func TestPremiumize_theServicesWordsCannotCarryTheToken(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	const token = "pmsecrettoken"
	reached := false
	cache := NewMemoryCache(1 << 20)
	s := &premiumizeStore{token: token, cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			reached = true
			body, _ := io.ReadAll(r.Body)
			return resp(500, `Gateway error handling `+string(body)), nil
		}}}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if !reached {
		t.Fatal("directdl was never called, so this asserts nothing")
	}
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("the token reached an error that gets logged: %v", err)
	}
	// And it must not have been written into the refusal memory either, which outlives the request.
	if reason, ok := backedOff(cache, ServicePremiumize, token, H); ok && strings.Contains(reason, token) {
		t.Errorf("the token was persisted into the refusal cache: %s", reason)
	}
}

// mylist replies with arrays as well as objects — that is why listFiles carries an []entry fallback at
// all — and `data:[]` is the array form's only way to say "I hold nothing for this id", exactly as
// `data:null` is the object form's. Read as "no answer" it kept the stale id, re-stamped its six hours
// on every poll and never re-added, wedging the series path at a permanent 503 while movies healed.
func TestTorBox_anEmptyListingIsTheAccountAnswering(t *testing.T) {
	// A BARE top-level `[]` is deliberately absent: it is not the documented envelope at all, so it stays
	// "no answer" rather than being read as a claim about what the account holds.
	for _, payload := range []string{`{"data":[]}`, `{"success":false,"data":null}`} {
		t.Run(payload, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			gone := true
			adds := 0
			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				switch {
				case isAddEndpoint(r):
					adds++
					gone = false
					return resp(200, `{"data":{"torrent_id":77}}`), nil
				case strings.Contains(r.URL.Path, "mylist"):
					if gone {
						return resp(200, payload), nil
					}
					return resp(200, `{"data":[{"files":[{"id":0,"name":"Show.S01E01.mkv","size":900},`+
						`{"id":1,"name":"Show.S01E02.mkv","size":950}]}]}`), nil
				}
				return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
			}}
			cache := NewMemoryCache(1 << 20)
			cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
			s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}

			link, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
			if err != nil || link == "" {
				t.Fatalf("a pack the account no longer holds must be re-bought: %q %v", link, err)
			}
			if adds != 1 {
				t.Errorf("made %d adds; the stale id should have been forgotten once and the pack re-added", adds)
			}
			if raw, _ := cache.Get(torrentIDKey("tok", H)); raw == "42" {
				t.Error("the stale id survived, and every later poll would keep it alive for another six hours")
			}
		})
	}
}

// A torrent createtorrent returned seconds ago, which mylist then denies, is TorBox disagreeing with
// itself — not the account saying it lacks the torrent. Passed through as `gone` the fresh id was never
// remembered, so the next poll found nothing known and added the same torrent again: one add per poll,
// bounded by nothing but the hourly allowance.
func TestTorBox_aTorrentDeniedRightAfterBuyingItIsNotReBought(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	adds := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case isAddEndpoint(r):
			adds++
			return resp(200, `{"data":{"torrent_id":77}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			return resp(200, `{"success":false,"data":null}`), nil
		}
		return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
	}}
	cache := NewMemoryCache(1 << 20)
	s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}

	target := ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)}
	// The first poll's answer is the one the viewer acts on: a dead link, so the client moves to another
	// release rather than sitting on a service error for one that may be fine elsewhere.
	_, first := s.Resolve(context.Background(), target)
	var dead *DeadLinkError
	if !errors.As(first, &dead) {
		t.Errorf("got %v, want a dead link the client can move past", first)
	}
	// The polls behind it are what the backoff is for: they cost nothing, where before each bought the
	// same torrent again.
	for i := 0; i < 9; i++ {
		_, _ = s.Resolve(context.Background(), target)
	}
	if adds > 1 {
		t.Errorf("made %d adds; a store contradicting itself must not be paid once per poll", adds)
	}
}

// Real-Debrid needs the same bound TorBox's just-added path got, and for a worse loop: `gone` about a
// torrent RD created moments ago erases the memory just written, the next poll finds nothing known and
// adds again — and RD mints a NEW id every time, so the re-add can never converge on one torrent and
// heal. Two minutes of one viewer on one release spent the whole hourly allowance, left fifty duplicate
// torrents on the account, and then answered 503 scout_busy for every RD resolve for the rest of the
// hour. On an RD-only install every /play poll reaches here, since RD has no Status to short-circuit on.
func TestRealDebrid_aTorrentDeniedRightAfterBuyingItIsNotReBought(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	adds := 0
	ids := map[string]bool{}
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "addMagnet"):
			adds++
			id := fmt.Sprintf("t%d", adds) // RD mints a fresh id every time
			ids[id] = true
			return resp(201, fmt.Sprintf(`{"id":%q}`, id)), nil
		case strings.Contains(r.URL.Path, "/torrents/info/"):
			return resp(404, `{}`), nil // RD denies the torrent it just created
		}
		return resp(200, `{}`), nil
	}}
	cache := NewMemoryCache(1 << 20)
	s := &realDebridStore{token: "tok", client: d, cache: cache, api: realDebridAPI}
	target := ResolveTarget{InfoHash: H}

	_, first := s.Resolve(context.Background(), target)
	var dead *DeadLinkError
	if !errors.As(first, &dead) || !strings.Contains(dead.Error(), "denied holding it") {
		t.Errorf("got %v, want a dead link naming the contradiction", first)
	}
	for i := 0; i < 30; i++ {
		_, _ = s.Resolve(context.Background(), target)
	}
	if adds > 1 {
		t.Errorf("made %d adds and left %d duplicate torrents on the account", adds, len(ids))
	}
	if left := globalAddBudget.remaining(budgetAccount(ServiceRealDebrid, "tok")); left < 49 {
		t.Errorf("allowance %d — one viewer on one release spent the hour", left)
	}
}

// The narrow direction of the just-added backoff, which only a comment guarded: it must fire ONLY when
// the store denies a torrent it just created. Widened to any error it would back off every legitimately
// queueing torrent — the ordinary state of a fetch in progress — and answer 503 for a minute on a
// release that is simply not ready yet.
func TestTorBox_theContradictionBackoffDoesNotFireOnAnOrdinaryWait(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case isAddEndpoint(r):
			return resp(200, `{"data":{"torrent_id":77}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			// Queued: the entry exists, its files have not resolved yet.
			return resp(200, `{"data":{"files":[]}}`), nil
		}
		return resp(200, `{"success":false}`), nil // no link yet — still downloading
	}}
	cache := NewMemoryCache(1 << 20)
	s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}

	_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if reason, ok := backedOff(cache, ServiceTorBox, "tok", H); ok {
		t.Errorf("a torrent that is merely still fetching was backed off: %s", reason)
	}
	// And the id is kept, so Status can report the download rather than the release reading as dead.
	if raw, _ := cache.Get(torrentIDKey("tok", H)); raw != "77" {
		t.Errorf("torrent id %q — a queued fetch lost the id Status needs", raw)
	}
}

// mylist is queried by id, so an answer carrying several entries must be matched to the one asked for.
// Taking the first blind picks a file id out of ANOTHER torrent's list — the wrong-episode failure by a
// route the name-match cannot catch, since the names it matches would be the other torrent's.
func TestTorBoxListFiles_matchesTheEntryToTheIdAskedFor(t *testing.T) {
	body := `{"data":[{"id":9,"files":[{"id":0,"name":"OTHER.S09E09.mkv","size":10}]},` +
		`{"id":42,"files":[{"id":7,"name":"Show.S01E02.mkv","size":900}]}]}`
	s := &torBoxStore{token: "tok", api: torboxAPI, client: routed{fallbk: ok(body)}}
	files, err := s.listFiles(t.Context(), 42)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if len(files) != 1 || files[0].Name != "Show.S01E02.mkv" {
		t.Errorf("got %+v — another torrent's files were used to pick ours", files)
	}
	// And when no entry matches, that is not our torrent's file list either: refuse rather than guess.
	s = &torBoxStore{token: "tok", api: torboxAPI, client: routed{fallbk: ok(body)}}
	if _, err := s.listFiles(t.Context(), 5); !errors.Is(err, errNoFileList) {
		t.Errorf("got %v, want errNoFileList when the answer describes other torrents", err)
	}
}

// Two classifications on mylist that the three-way split got wrong in opposite directions.
func TestTorBoxListFiles_classifiesTheRemainingShapes(t *testing.T) {
	// A rejected key found HERE must be able to raise the account-wide backoff. errNoFileList carries
	// Status 0, so routing a 401 to it left the dead key undiscoverable on this endpoint.
	cache := NewMemoryCache(1 << 20)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI, client: routed{fallbk: status(401)}}
	_, err := s.listFiles(t.Context(), 42)
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Status != 401 {
		t.Fatalf("got %v, want a refusal carrying its status", err)
	}
	recordRefusal(cache, ServiceTorBox, "tok", H, err)
	if _, ok := accountBackedOff(cache, ServiceTorBox, "tok"); !ok {
		t.Error("a key TorBox rejected on mylist never reaches the account backoff")
	}

	// A body with no `data` key at all is not TorBox's envelope — a proxy page that happens to be valid
	// JSON, say. Silence, not a claim about the account; read as `gone` it paid an add to re-buy a
	// torrent nobody said was missing.
	s = &torBoxStore{token: "tok", api: torboxAPI, client: routed{fallbk: ok(`{}`)}}
	if _, err := s.listFiles(t.Context(), 42); !errors.Is(err, errNoFileList) {
		t.Errorf("got %v, want errNoFileList for a body that is not the envelope", err)
	}
	// The two shapes that DO say it still must.
	for _, payload := range []string{`{"success":false,"data":null}`, `{"data":null}`} {
		s = &torBoxStore{token: "tok", api: torboxAPI, client: routed{fallbk: ok(payload)}}
		if _, err := s.listFiles(t.Context(), 42); !errors.Is(err, errTorrentGone) {
			t.Errorf("%s: got %v, want errTorrentGone", payload, err)
		}
	}
}

// The same invariant on all three stores: when the service ANSWERS and creates nothing, the charge taken
// before the request goes back. RD had neither of its siblings' bounds on this branch — TorBox records a
// refusal here, Premiumize refunds — so a magnet RD rejects with a 400, or any 2xx scout cannot pull an
// id out of (a proxy page served as 200), spent one add per poll. Fifty real addMagnet calls inside two
// minutes at the client's cadence, and then 503 scout_busy on every RD resolve for the rest of the
// rolling hour, healthy releases included.
func TestRealDebrid_anAnsweredFailureIsNotACharge(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"a rejected magnet", `{"error":"bad magnet","error_code":11}`, 400},
		{"a 2xx with no id", `{"ok":true}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "addMagnet") {
					return resp(tc.status, tc.body), nil
				}
				return resp(200, `{}`), nil
			}}
			cache := NewMemoryCache(1 << 20)
			s := &realDebridStore{token: "tok", client: d, cache: cache, api: realDebridAPI}
			for i := 0; i < 60; i++ {
				_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			}
			if left := globalAddBudget.remaining(budgetAccount(ServiceRealDebrid, "tok")); left != 50 {
				t.Fatalf("allowance %d — one viewer on one release spent the hour", left)
			}
			// What the allowance buys: a DIFFERENT, healthy release still resolves, instead of being
			// refused by scout's own bookkeeping for the rest of the hour.
			healthy := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(r.URL.Path, "addMagnet"):
					return resp(201, `{"id":"t9"}`), nil
				case strings.Contains(r.URL.Path, "unrestrict"):
					return resp(200, `{"download":"https://rd/ok"}`), nil
				case strings.Contains(r.URL.Path, "/torrents/info/"):
					return resp(200, `{"files":[{"id":1,"path":"m.mkv","bytes":9,"selected":1}],"links":["l1"]}`), nil
				}
				return resp(200, `{}`), nil
			}}
			other := &realDebridStore{token: "tok", client: healthy, cache: cache, api: realDebridAPI}
			if link, err := other.Resolve(context.Background(), ResolveTarget{InfoHash: repeat("b", 40)}); link == "" {
				t.Errorf("a healthy release answered %v — scout refused what RD would have served", err)
			}
		})
	}
}

// RD was also the only store discarding the service's own words on this branch, so the log read the same
// for a bad magnet, a locked account and a Cloudflare page.
func TestRealDebrid_theAddKeepsTheServicesOwnWords(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "addMagnet") {
			return resp(400, `{"error":"infringing_file"}`), nil
		}
		return resp(200, `{}`), nil
	}}
	s := &realDebridStore{token: "tok", client: d, cache: NewMemoryCache(1 << 20), api: realDebridAPI}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if err == nil || !strings.Contains(err.Error(), "infringing_file") {
		t.Errorf("got %v — the answer that says WHY was discarded", err)
	}
}

// A key TorBox rejects on mylist must reach the account-wide backoff. listFiles classifies it, but the
// only caller assigned the error and never consulted it for any value but errTorrentGone, so the
// classification died one line after it was made.
func TestTorBox_aRejectedKeyOnMylistReachesTheAccountBackoff(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "mylist") {
				return resp(401, `{"error":"BAD_TOKEN"}`), nil
			}
			return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
		}}}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Status != 401 {
		t.Fatalf("got %v, want the refusal carrying its status", err)
	}
	if _, ok := accountBackedOff(cache, ServiceTorBox, "tok"); !ok {
		t.Error("the dead key was never recorded, so every later request asks with it again")
	}
}
