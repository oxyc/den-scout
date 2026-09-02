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
