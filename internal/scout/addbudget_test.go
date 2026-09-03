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

// What the ceiling counts is adds that may HAVE HAPPENED, and the dividing line is whether the service
// answered. A request that reached the wire and was never answered keeps its charge — the torrent may
// well exist, which is why refundAdd will not give it back. An answered non-2xx is the opposite: the
// service said it created nothing, so holding the charge only erodes the allowance. All three stores
// draw the line in the same place; TorBox used to keep both, so a repeatedly polled bad magnet ate its
// hourly allowance where RD and Premiumize were immune.
func TestAddBudget_theChargeFollowsWhetherTheServiceAnswered(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(*http.Request) (*http.Response, error)
		want int
	}{
		{"answered, created nothing", func(*http.Request) (*http.Response, error) {
			return resp(400, `{"error":"NOPE"}`), nil
		}, 5},
		{"never answered", func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("read tcp 10.0.0.2:443: connection reset by peer")
		}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 5)
			defer func() { globalAddBudget = prev }()

			s := &torBoxStore{token: "tok", client: mockDoer{fn: tc.fn},
				cache: NewMemoryCache(1 << 20), api: torboxAPI}
			_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			if left := globalAddBudget.remaining(budgetAccount(ServiceTorBox, "tok")); left != tc.want {
				t.Errorf("remaining = %d, want %d", left, tc.want)
			}
		})
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
	calls := 0
	s := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			calls++
			return resp(200, `{"status":"error","message":"Invalid src"}`), nil
		}}}
	// The FIRST answer is the one the viewer acts on; the polls behind it meet the backoff.
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if errors.Is(err, errAddInFlight) {
		t.Errorf("a refused magnet was reported as a transfer on its way: %v", err)
	}
	var dead *DeadLinkError
	if !errors.As(err, &dead) {
		t.Fatalf("got %v, want a dead link the client can move past", err)
	}
	for i := 0; i < 29; i++ {
		_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	}
	// This is where the loop shows, and where an allowance assertion cannot: HTTP 200 `{"status":"error"}`
	// is how Premiumize reports an unsupported magnet, an account at its limit and "no space", and this
	// branch alone recorded nothing — so thirty polls made thirty real calls while the refund kept the
	// allowance at fifty the whole way.
	if calls > 1 {
		t.Errorf("made %d directdl calls over 30 polls — nothing bounds the loop", calls)
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

			adds := 0
			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "addMagnet") {
					adds++
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
			// The allowance alone cannot see the loop: refunding every attempt keeps it at 50 while sixty
			// polls make sixty real calls upstream. Counting them is what notices that the refund removed
			// the only bound this branch had, which is exactly how that regression shipped.
			if adds > 1 {
				t.Errorf("made %d addMagnet calls over 60 polls — nothing bounds the loop", adds)
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

// A body that could not be READ is not an answer that nothing was created. RD had already sent its
// status line — a 201 means the torrent exists on the account — and a mid-body reset, a TLS truncation
// or the client's context expiring after the headers arrived all land here. Folding it in with "RD
// created nothing" refunded the charge for a torrent that does exist, and with no backoff on that branch
// the budget was the only bound: sixty polls created sixty torrents with the allowance untouched.
func TestRealDebrid_anUnreadableAnswerIsNotProofNothingWasCreated(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	created := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "addMagnet") {
			created++
			// Answered 201 — the torrent exists — then the body dies mid-flight.
			return &http.Response{StatusCode: 201, Body: io.NopCloser(brokenReader{})}, nil
		}
		return resp(200, `{}`), nil
	}}
	cache := NewMemoryCache(1 << 20)
	s := &realDebridStore{token: "tok", client: d, cache: cache, api: realDebridAPI}
	for i := 0; i < 60; i++ {
		_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	}
	if created > 1 {
		t.Errorf("created %d torrents on the account across 60 polls", created)
	}
	// The charge stays: the outcome is unknown, and that is precisely when assuming nothing happened is
	// the expensive assumption.
	if left := globalAddBudget.remaining(budgetAccount(ServiceRealDebrid, "tok")); left != 49 {
		t.Errorf("allowance %d — an add whose outcome is unknown was treated as one that never happened", left)
	}
}

// brokenReader answers with a read error after the status line has already been sent.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, fmt.Errorf("unexpected EOF mid-body") }

// The rejected-key return must not drop the torrent id it was just handed: Status needs it to report a
// download in progress, and without it the next poll knows of no torrent and buys another.
func TestTorBox_theRejectedKeyReturnKeepsTheTorrentId(t *testing.T) {
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
			return resp(403, `{"error":"BAD_TOKEN"}`), nil
		}
		return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
	}}
	cache := NewMemoryCache(1 << 20)
	s := &torBoxStore{token: "tok", client: d, cache: cache, api: torboxAPI}
	_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if raw, _ := cache.Get(torrentIDKey("tok", H)); raw != "77" {
		t.Errorf("torrent id %q — the id createtorrent just handed us was dropped", raw)
	}
	if adds != 1 {
		t.Errorf("made %d adds", adds)
	}
}

// The same bound on Premiumize's answered-failure branch: refunding the charge fixed the quota but left
// a poll loop free to re-ask a magnet Premiumize has already rejected, once every couple of seconds for
// as long as the viewer sits there. Free of quota is not free.
func TestPremiumize_anAnsweredFailureIsAskedOnlyOnce(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	calls := 0
	cache := NewMemoryCache(1 << 20)
	s := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			calls++
			return resp(400, `{"status":"error","message":"Invalid src"}`), nil
		}}}
	target := ResolveTarget{InfoHash: H}
	// The first answer is a dead link, so the client falls through to another release rather than being
	// told the store is unavailable for one the store has a definite opinion about.
	_, first := s.Resolve(context.Background(), target)
	var dead *DeadLinkError
	if !errors.As(first, &dead) {
		t.Errorf("got %v, want a dead link the client can move past", first)
	}
	for i := 0; i < 59; i++ {
		_, _ = s.Resolve(context.Background(), target)
	}
	if calls > 1 {
		t.Errorf("made %d directdl calls over 60 polls — nothing bounds the loop", calls)
	}
	if left := globalAddBudget.remaining(budgetAccount(ServicePremiumize, "tok")); left != 50 {
		t.Errorf("allowance %d — a refused magnet was charged", left)
	}
}

// A cancelled poll is not the store refusing. /play runs on the client's context — a focus change is
// enough — and the add's response BODY is read inside it, so a cancellation lands there on every store.
// Nothing covered that: the existing guard test hands recordRefusal a context.Canceled directly, which
// certifies the helper but not the property that matters (no production path files a cancellation as a
// refusal). A caller that builds its own error satisfies the helper test and breaks the property, which
// is exactly how it shipped.
func TestStores_aCancelledAddBodyIsNotARefusal(t *testing.T) {
	// hangingBody sends the status line, then blocks until the poll's context is cancelled.
	type store struct {
		name string
		svc  DebridService
		make func(Cache, doer) Store
		path string
		head string
	}
	for _, st := range []store{
		{"realdebrid", ServiceRealDebrid, func(c Cache, d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}, "addMagnet", `{"id":"t1"}`},
		{"torbox", ServiceTorBox, func(c Cache, d doer) Store {
			return &torBoxStore{token: "tok", client: d, cache: c, api: torboxAPI}
		}, "createtorrent", `{"data":{"torrent_id":7}}`},
		{"premiumize", ServicePremiumize, func(c Cache, d doer) Store {
			return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
		}, "directdl", `{"status":"success","content":[]}`},
	} {
		t.Run(st.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			cache := NewMemoryCache(1 << 20)
			adds := 0
			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, st.path) {
					adds++
					// The add was accepted; the body then dies because the viewer moved on.
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					return &http.Response{StatusCode: 200, Body: io.NopCloser(cancelledReader{ctx})}, nil
				}
				return resp(200, st.head), nil
			}}
			store := st.make(cache, d)
			for i := 0; i < 30; i++ {
				_, _ = store.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			}
			// Three properties, because "not filed as a refusal" alone is a PROXY: both the TorBox and
			// the Premiumize bug this test was written for passed that assertion while re-sending the add
			// on every poll. What matters is that a cancelled poll cannot cost an add and cannot loop.
			if reason, ok := backedOff(cache, st.svc, "tok", H); ok {
				t.Errorf("a cancelled poll was filed as %s refusing: %s", st.svc, reason)
			}
			if adds > 1 {
				t.Errorf("%s sent the add %d times across 30 polls — the marker that says one is in "+
					"flight was cleared before the body was read", st.svc, adds)
			}
			// The charge stays exactly once: the add was written to the wire, and refunding it is what
			// removed the only bound the loop had.
			if left := globalAddBudget.remaining(budgetAccount(st.svc, "tok")); left != 49 {
				t.Errorf("%s allowance %d, want 49", st.svc, left)
			}
		})
	}
}

// cancelledReader fails the way a body does when the request's context goes away mid-read.
type cancelledReader struct{ ctx context.Context }

func (c cancelledReader) Read([]byte) (int, error) {
	<-c.ctx.Done()
	return 0, c.ctx.Err()
}

// An add whose answer never arrived is what the in-flight marker is for. It must survive, so the next
// poll says "downloading" — honest for an add that went out and whose result we never saw — rather than
// buying the torrent again or blaming the store.
func TestRealDebrid_anUnreadAnswerLeavesTheAddInFlight(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	created := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "addMagnet") {
			created++
			return &http.Response{StatusCode: 201, Body: io.NopCloser(brokenReader{})}, nil
		}
		return resp(200, `{}`), nil
	}}
	cache := NewMemoryCache(1 << 20)
	s := &realDebridStore{token: "tok", client: d, cache: cache, api: realDebridAPI}

	_, first := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if !errors.Is(first, errAddInFlight) {
		t.Errorf("got %v, want the add reported as still in flight", first)
	}
	for i := 0; i < 60; i++ {
		_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	}
	if created > 1 {
		t.Errorf("created %d torrents on the account across 61 polls", created)
	}
	// The charge stays: the add was written to the wire, and assuming otherwise is the expensive guess.
	if left := globalAddBudget.remaining(budgetAccount(ServiceRealDebrid, "tok")); left != 49 {
		t.Errorf("allowance %d — an add that went out was treated as one that never happened", left)
	}
	// And it is not a refusal, so no store gets blamed for a body scout could not read.
	if reason, ok := backedOff(cache, ServiceRealDebrid, "tok", H); ok {
		t.Errorf("filed as RD refusing: %s", reason)
	}
}

// TorBox's createtorrent detail is persisted into the refusal cache, which writes through to disk — so
// it gets the redaction the other three detail sites have. The token rides in a header here rather than
// the URL, so a leak needs the service to echo the header back; this was the one site not following the
// rule, which is reason enough.
func TestTorBox_theAddDetailCannotCarryTheToken(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	const token = "torboxsecrettoken"
	cache := NewMemoryCache(1 << 20)
	s := &torBoxStore{token: token, cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if isAddEndpoint(r) {
				return resp(403, `{"error":"rejected for `+r.Header.Get("authorization")+`"}`), nil
			}
			return resp(200, `{}`), nil
		}}}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("the token reached an error that gets logged: %v", err)
	}
	if reason, ok := backedOff(cache, ServiceTorBox, token, H); ok && strings.Contains(reason, token) {
		t.Errorf("the token was persisted into the refusal cache: %s", reason)
	}
}

// The other side of moving the settle after the body read: an add that DID complete must still clear the
// marker. Left set, every later poll answers "an add is already in flight" for a release that resolved
// perfectly well — 202 downloading forever, with no add ever sent again to heal it.
func TestStores_aCompletedAddClearsTheInFlightMarker(t *testing.T) {
	for _, st := range []struct {
		name string
		svc  DebridService
		make func(Cache, doer) Store
		body func(*http.Request) *http.Response
	}{
		{"realdebrid", ServiceRealDebrid, func(c Cache, d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}, func(r *http.Request) *http.Response {
			switch {
			case strings.Contains(r.URL.Path, "addMagnet"):
				return resp(201, `{"id":"t1"}`)
			case strings.Contains(r.URL.Path, "unrestrict"):
				return resp(200, `{"download":"https://rd/ok"}`)
			case strings.Contains(r.URL.Path, "/torrents/info/"):
				return resp(200, `{"files":[{"id":1,"path":"m.mkv","bytes":9,"selected":1}],"links":["l1"]}`)
			}
			return resp(200, `{}`)
		}},
		{"torbox", ServiceTorBox, func(c Cache, d doer) Store {
			return &torBoxStore{token: "tok", client: d, cache: c, api: torboxAPI}
		}, func(r *http.Request) *http.Response {
			if isAddEndpoint(r) {
				return resp(200, `{"data":{"torrent_id":7}}`)
			}
			return resp(200, `{"success":true,"data":"https://cdn/x"}`)
		}},
		{"premiumize", ServicePremiumize, func(c Cache, d doer) Store {
			return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
		}, func(*http.Request) *http.Response {
			return resp(200, `{"status":"success","content":[{"path":"m.mkv","link":"https://pm/ok"}]}`)
		}},
	} {
		t.Run(st.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			cache := NewMemoryCache(1 << 20)
			d := mockDoer{fn: func(r *http.Request) (*http.Response, error) { return st.body(r), nil }}
			store := st.make(cache, d)
			if link, err := store.Resolve(context.Background(), ResolveTarget{InfoHash: H}); link == "" {
				t.Fatalf("setup: the release did not resolve: %v", err)
			}
			if err := addInFlight(cache, st.svc, "tok", H); err != nil {
				t.Errorf("%s: the marker survived a completed add, so every later poll answers %v",
					st.svc, err)
			}
			// And the release still resolves on the next poll rather than reporting a phantom add.
			if link, err := store.Resolve(context.Background(), ResolveTarget{InfoHash: H}); link == "" {
				t.Errorf("%s: the second poll answered %v", st.svc, err)
			}
		})
	}
}

// A pack named with bare episode numbers — `[Grp] Show - 03 [1080p].mkv`, the standard anime/TV shape —
// is not episode-LABELLED by the only evidence test that can be trusted (a bare number cannot be, since
// "Movie.2019.1080p" is full of digit runs). It used to fall back to the largest video, and because that
// answer is non-nil every caller returned before reading the indexer's fileIdx — a position in this very
// torrent, and correct. So every episode of the pack resolved to the same file, with a 302 and no error.
func TestStores_aBareNumberedPackUsesTheIndexersFileIndex(t *testing.T) {
	files := []TorrentFile{
		{Index: 0, Name: "[Grp] Show - 01 [1080p].mkv", SizeBytes: intp(900)},
		{Index: 1, Name: "[Grp] Show - 02 [1080p].mkv", SizeBytes: intp(4000)}, // the largest: the old answer
		{Index: 2, Name: "[Grp] Show - 03 [1080p].mkv", SizeBytes: intp(950)},
	}
	// S01E03 is at position 2, which is exactly what the indexer said.
	target := ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(3), FileIdx: intp(2)}

	got, err := selectFileID(files, target)
	wantPick(t, got, err, 2, "torbox follows the indexer rather than the biggest file")
	got, err = (&realDebridStore{}).pickFileID(files, target)
	wantPick(t, got, err, 2, "realdebrid follows the indexer")
	got, err = (&premiumizeStore{}).pickIndex(files, target)
	wantPick(t, got, err, 2, "premiumize follows the indexer")

	// With no fileIdx there is nothing better than the largest, and that fallback must survive — a
	// feature plus a sample is the common shape and the big one is right.
	sampled := []TorrentFile{
		{Index: 0, Name: "Show.1080p.mkv", SizeBytes: intp(4000)},
		{Index: 1, Name: "Sample/sample.mkv", SizeBytes: intp(2)},
	}
	blind := ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(3)}
	got, err = selectFileID(sampled, blind)
	wantPick(t, got, err, 0, "torbox still prefers the feature to the sample")
	got, err = (&realDebridStore{}).pickFileID(sampled, blind)
	wantPick(t, got, err, 0, "realdebrid still prefers the feature")
	got, err = (&premiumizeStore{}).pickIndex(sampled, blind)
	wantPick(t, got, err, 0, "premiumize still prefers the feature")
}

// "The outcome is unknown" is a wait, and a wait needs a deadline. The add-attempt marker lives 90s and
// is rewritten by every new attempt, so a release whose add answer is never readable cycled marker →
// expiry → fresh charged add → marker, forever: the client shown 202 "downloading" throughout with no
// path ever answering dead link, and on RD and Premiumize — which have no Status to rediscover the
// torrent with — forty duplicate torrents an hour and a viewer with nothing to do but back out.
func TestStores_anAddWhoseOutcomeIsNeverKnownGivesUp(t *testing.T) {
	for _, svc := range []DebridService{ServiceRealDebrid, ServiceTorBox, ServicePremiumize} {
		t.Run(string(svc), func(t *testing.T) {
			cache := NewMemoryCache(1 << 20)
			// The first failure starts the clock and reads as "still coming".
			first := unknownOutcome(cache, svc, "tok", H, errServiceWentQuiet)
			if !errors.Is(first, errAddInFlight) {
				t.Fatalf("got %v, want the add reported as still in flight", first)
			}
			// Re-asking must not restamp it: a clock every poll resets is not a deadline, it is the
			// absorbing state this exists to end. Wind it back to just inside the window, ask twenty more
			// times, and the stamp must still be the old one.
			aged := time.Now().Add(-addGiveUp + time.Minute)
			cache.Put(unknownOutcomeKey(svc, "tok", H),
				strconv.FormatInt(aged.Unix(), 10), unknownOutcomeTTL)
			for i := 0; i < 20; i++ {
				_ = unknownOutcome(cache, svc, "tok", H, errServiceWentQuiet)
			}
			if raw, _ := cache.Get(unknownOutcomeKey(svc, "tok", H)); raw != strconv.FormatInt(aged.Unix(), 10) {
				t.Errorf("the clock was restamped to %s — twenty polls just bought another ten minutes", raw)
			}
			cache.Put(unknownOutcomeKey(svc, "tok", H),
				strconv.FormatInt(time.Now().Add(-addGiveUp-time.Minute).Unix(), 10), unknownOutcomeTTL)
			var dead *DeadLinkError
			if err := unknownOutcome(cache, svc, "tok", H, errServiceWentQuiet); !errors.As(err, &dead) {
				t.Errorf("got %v, want a dead link so the client can fall through", err)
			}
			// An outcome of any kind ends the run of not knowing, so the next add starts fresh.
			settleAddAttempt(cache, svc, "tok", H)
			if err := unknownOutcome(cache, svc, "tok", H, errServiceWentQuiet); !errors.Is(err, errAddInFlight) {
				t.Errorf("got %v — the give-up outlived the answer that resolved it", err)
			}
		})
	}
}

// An add that went out and was never answered is scout's own uncertainty, not the debrid refusing. All
// three stores set the in-flight marker for exactly that; two of them then recorded the transport
// failure as a refusal as well, and since they consulted backedOff FIRST the refusal pre-empted the
// marker: errAddInFlight became unreachable and the client was told its debrid was refusing — and, per
// this codebase's own account of that answer, stopped trying other sources — for a release scout had an
// add out for. Real-Debrid is the sharp case: it has no Status for handlePlay to rescue the answer with.
func TestStores_anUnansweredAddReadsAsComingNotAsRefusing(t *testing.T) {
	for _, st := range []struct {
		name string
		svc  DebridService
		path string
		make func(Cache, doer) Store
	}{
		{"torbox", ServiceTorBox, "createtorrent", func(c Cache, d doer) Store {
			return &torBoxStore{token: "tok", client: d, cache: c, api: torboxAPI}
		}},
		{"realdebrid", ServiceRealDebrid, "addMagnet", func(c Cache, d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}},
		{"premiumize", ServicePremiumize, "directdl", func(c Cache, d doer) Store {
			return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
		}},
	} {
		t.Run(st.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			adds := 0
			cache := NewMemoryCache(1 << 20)
			s := st.make(cache, mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, st.path) {
					adds++
					return nil, fmt.Errorf("read tcp 10.0.0.2:443: connection reset by peer")
				}
				return resp(200, `{}`), nil
			}})
			target := ResolveTarget{InfoHash: H}
			_, _ = s.Resolve(context.Background(), target)

			// The store said nothing, so nothing may be recorded against it.
			if reason, ok := backedOff(cache, st.svc, "tok", H); ok {
				t.Errorf("an unanswered add was filed as %s refusing: %s", st.svc, reason)
			}
			// And the polls behind it read as "coming", which handlePlay answers 202, rather than 503.
			for i := 0; i < 5; i++ {
				_, err := s.Resolve(context.Background(), target)
				if !errors.Is(err, errAddInFlight) {
					t.Fatalf("poll %d answered %v, want the add reported as still in flight", i, err)
				}
			}
			if adds != 1 {
				t.Errorf("sent the add %d times; the marker exists to stop exactly that", adds)
			}
		})
	}
}

// The account gate the other two stores have. It was redundant on Premiumize until addInFlight was moved
// above backedOff, at which point a live in-flight marker pre-empted a rejected key and answered 202
// "downloading" where TorBox and RD answer 503. A dead key is not a wait.
func TestPremiumize_aRejectedKeyOutranksAnInFlightMarker(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServicePremiumize, "tok", repeat("f", 40),
		&StoreUnavailableError{Service: ServicePremiumize, Status: 401, Reason: "directdl http 401"})
	noteAddAttempt(cache, ServicePremiumize, "tok", H)
	s := &premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			t.Error("a rejected key must stop the request before it is made")
			return resp(200, `{}`), nil
		}}}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if errors.Is(err, errAddInFlight) {
		t.Errorf("a dead key was reported as a transfer on its way: %v", err)
	}
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) {
		t.Errorf("got %v, want the store reported unavailable", err)
	}
}

// The window is per account and the account comes from the install's own config URL, on a route with no
// authentication — so a map that only ever grows is not bounded by anything this process controls.
func TestAddBudget_forgetsAccountsWhoseWindowHasDrained(t *testing.T) {
	now := time.Now()
	b := newAddBudget(time.Hour, 50)
	b.now = func() time.Time { return now }
	for i := 0; i < 5000; i++ {
		b.take(fmt.Sprintf("torbox:%d", i))
	}
	if len(b.spent) != 5000 {
		t.Fatalf("setup: %d accounts", len(b.spent))
	}
	// An hour later every one of those windows has drained; the next caller must not inherit them.
	now = now.Add(2 * time.Hour)
	b.take("torbox:live")
	if len(b.spent) != 1 {
		t.Errorf("map holds %d accounts, want just the live one", len(b.spent))
	}
	// And the live account still accounts correctly.
	if left := b.remaining("torbox:live"); left != 49 {
		t.Errorf("remaining = %d, want 49", left)
	}
}

// An add that keeps failing at the transport must not cycle forever. The marker lives 90 seconds and
// every attempt writes a fresh one, so without a deadline it went marker → expiry → another charged add,
// about forty an hour, until the allowance was gone and every release on the account answered 503
// scout_busy. That is the cycle unknownOutcomeKey exists to end; it was wired only to the body-read path.
func TestStores_aTransportFailedAddGivesUpEventually(t *testing.T) {
	for _, st := range []struct {
		name string
		svc  DebridService
		path string
		make func(Cache, doer) Store
	}{
		{"torbox", ServiceTorBox, "createtorrent", func(c Cache, d doer) Store {
			return &torBoxStore{token: "tok", client: d, cache: c, api: torboxAPI}
		}},
		{"realdebrid", ServiceRealDebrid, "addMagnet", func(c Cache, d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}},
		{"premiumize", ServicePremiumize, "directdl", func(c Cache, d doer) Store {
			return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
		}},
	} {
		t.Run(st.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			adds := 0
			cache := NewMemoryCache(1 << 20)
			s := st.make(cache, mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, st.path) {
					adds++
					return nil, fmt.Errorf("dial tcp 10.0.0.2:443: connect: connection refused")
				}
				return resp(200, `{}`), nil
			}})
			target := ResolveTarget{InfoHash: H}
			_, _ = s.Resolve(context.Background(), target)

			// PRODUCTION must have started the clock. Writing it here instead would test the deadline
			// against a stamp the service never makes — the proxy shape that has hidden a defect in nine
			// of the last ten rounds.
			stamp, ok := cache.Get(unknownOutcomeKey(st.svc, "tok", H))
			if !ok || stamp == "" {
				t.Fatalf("a transport-failed add left no give-up clock, so nothing bounds the retries")
			}
			// While it is running, the polls behind it read as "coming".
			if _, err := s.Resolve(context.Background(), target); !errors.Is(err, errAddInFlight) {
				t.Fatalf("got %v, want the add reported as still in flight", err)
			}
			// Wind the clock the service wrote past its deadline, and lapse the 90s marker with it. Every
			// later poll must answer a dead link the client can move past, and must not buy the torrent
			// again.
			settleAddAttempt(cache, st.svc, "tok", H)
			cache.Put(unknownOutcomeKey(st.svc, "tok", H),
				strconv.FormatInt(time.Now().Add(-addGiveUp-time.Minute).Unix(), 10), unknownOutcomeTTL)

			before := adds
			for i := 0; i < 20; i++ {
				_, err := s.Resolve(context.Background(), target)
				var dead *DeadLinkError
				if !errors.As(err, &dead) {
					t.Fatalf("poll %d answered %v, want a dead link the client can move past", i, err)
				}
			}
			if adds != before {
				t.Errorf("sent %d more adds past the give-up deadline", adds-before)
			}
		})
	}
}

// An add of ours already out outranks any refusal, whichever store said what and in whichever order.
// errAddInFlight wraps a StoreUnavailableError, so the first refusal used to claim the verdict and store
// ORDER decided whether /play answered 202 "downloading" or 503 naming a store that had said nothing.
func TestResolvePreferring_anAddInFlightOutranksARefusal(t *testing.T) {
	refusing := stubStore{svc: ServiceTorBox, err: &StoreUnavailableError{
		Service: ServiceTorBox, Reason: "createtorrent http 429 (backing off)"}}
	fetching := stubStore{svc: ServiceRealDebrid, err: fmt.Errorf("%w: %w", errAddInFlight,
		&StoreUnavailableError{Service: ServiceRealDebrid, Reason: "scout already sent an add"})}

	for _, order := range [][]Store{{refusing, fetching}, {fetching, refusing}} {
		pool := &StorePool{stores: order}
		_, err := pool.ResolvePreferring(context.Background(), ResolveTarget{InfoHash: H}, nil)
		if !errors.Is(err, errAddInFlight) {
			t.Errorf("store order decided the verdict: got %v, want the add reported in flight", err)
		}
	}
}

// stubStore answers with a fixed error, so the pool's own precedence is what is under test.
type stubStore struct {
	svc DebridService
	err error
}

func (s stubStore) Service() DebridService                                 { return s.svc }
func (s stubStore) Resolve(context.Context, ResolveTarget) (string, error) { return "", s.err }
func (s stubStore) Status(context.Context, ResolveTarget) (StoreStatus, bool) {
	return StoreStatus{}, false
}
func (s stubStore) CacheCheck(context.Context, []string) (map[string]bool, error) { return nil, nil }

// A viewer backing out must not condemn a release. /play runs on the client's context and a focus change
// is enough to cancel it, so a cancelled add landed in the same branch as a service that never answered
// — and once the give-up clock was stamped, every later resolve of that release answered a dead link for
// the rest of unknownOutcomeTTL, with no upstream call able to clear it. RD and Premiumize have no
// Status to rediscover the torrent with, so nothing escaped it. A deadline belongs on a service that
// went quiet, not on a client that hung up.
func TestStores_aCancelledAddDoesNotStartTheGiveUpClock(t *testing.T) {
	for _, st := range []struct {
		name string
		svc  DebridService
		path string
		make func(Cache, doer) Store
	}{
		{"torbox", ServiceTorBox, "createtorrent", func(c Cache, d doer) Store {
			return &torBoxStore{token: "tok", client: d, cache: c, api: torboxAPI}
		}},
		{"realdebrid", ServiceRealDebrid, "addMagnet", func(c Cache, d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}},
		{"premiumize", ServicePremiumize, "directdl", func(c Cache, d doer) Store {
			return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
		}},
	} {
		t.Run(st.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			cache := NewMemoryCache(1 << 20)
			for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
				s := st.make(cache, mockDoer{fn: func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, st.path) {
						return nil, fmt.Errorf("Post %q: %w", "https://example/add", cause)
					}
					return resp(200, `{}`), nil
				}})
				_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
				if stamp, ok := cache.Get(unknownOutcomeKey(st.svc, "tok", H)); ok && stamp != "" {
					t.Fatalf("%v started the give-up clock: the release is condemned for %v", cause, addGiveUp)
				}
				settleAddAttempt(cache, st.svc, "tok", H)
			}
			// A service that genuinely went quiet still starts it — the guard must be about the CAUSE,
			// not about switching the deadline off.
			s := st.make(cache, mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, st.path) {
					return nil, fmt.Errorf("read tcp 10.0.0.2:443: connection reset by peer")
				}
				return resp(200, `{}`), nil
			}})
			_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			if stamp, ok := cache.Get(unknownOutcomeKey(st.svc, "tok", H)); !ok || stamp == "" {
				t.Errorf("a service that never answered left no give-up clock")
			}
		})
	}
}

// errServiceWentQuiet stands for the cause these tests mean: the service stopped answering, as opposed
// to the client hanging up. Only the former starts the give-up clock.
var errServiceWentQuiet = errors.New("read tcp 10.0.0.2:443: connection reset by peer")

// The body-read branch is the other half of the same fact, and which one a back-out lands in is decided
// only by whether the response headers arrived first. Guarding the transport branch alone left the same
// release condemned by the same viewer action.
func TestStores_aCancelledBodyReadDoesNotStartTheGiveUpClock(t *testing.T) {
	for _, st := range []struct {
		name string
		svc  DebridService
		path string
		make func(Cache, doer) Store
	}{
		{"torbox", ServiceTorBox, "createtorrent", func(c Cache, d doer) Store {
			return &torBoxStore{token: "tok", client: d, cache: c, api: torboxAPI}
		}},
		{"realdebrid", ServiceRealDebrid, "addMagnet", func(c Cache, d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}},
		{"premiumize", ServicePremiumize, "directdl", func(c Cache, d doer) Store {
			return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
		}},
	} {
		t.Run(st.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			cache := NewMemoryCache(1 << 20)
			// The add is ACCEPTED — 201 on RD, 200 elsewhere — and the body then dies because the viewer
			// moved on. The torrent very likely exists, which is exactly why condemning it is wrong.
			s := st.make(cache, mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, st.path) {
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					return &http.Response{StatusCode: 201, Body: io.NopCloser(cancelledReader{ctx})}, nil
				}
				return resp(200, `{}`), nil
			}})
			_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			if stamp, ok := cache.Get(unknownOutcomeKey(st.svc, "tok", H)); ok && stamp != "" {
				t.Errorf("a back-out mid-body started the give-up clock: the release is condemned for %v", addGiveUp)
			}

			// A body that dies for the SERVICE's own reasons still starts it.
			settleAddAttempt(cache, st.svc, "tok", H)
			s = st.make(cache, mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, st.path) {
					return &http.Response{StatusCode: 201, Body: io.NopCloser(brokenReader{})}, nil
				}
				return resp(200, `{}`), nil
			}})
			_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			if stamp, ok := cache.Get(unknownOutcomeKey(st.svc, "tok", H)); !ok || stamp == "" {
				t.Errorf("a truncated answer left no give-up clock, so nothing bounds the retries")
			}
		})
	}
}

// TorBox reports plenty of errors as HTTP 200 with no torrent id, and a proxy page served as 200 lands
// there too. Keeping the charge walked the whole hourly allowance in one sitting — this branch answers a
// dead link, which is what makes the client fall through to the next candidate, so no poll loop is even
// needed. RD already refunded the same shape.
func TestAddBudget_anEmptyTwoHundredIsNotAnAdd(t *testing.T) {
	for _, st := range []struct {
		name string
		svc  DebridService
		path string
		body string
		make func(Cache, doer) Store
	}{
		{"torbox", ServiceTorBox, "createtorrent", `{"success":false}`, func(c Cache, d doer) Store {
			return &torBoxStore{token: "tok", client: d, cache: c, api: torboxAPI}
		}},
		{"realdebrid", ServiceRealDebrid, "addMagnet", `{"ok":true}`, func(c Cache, d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}},
	} {
		t.Run(st.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			adds := 0
			// Sixty DISTINCT releases: the client falling through candidates, not a poll loop.
			for i := 0; i < 60; i++ {
				cache := NewMemoryCache(1 << 20)
				s := st.make(cache, mockDoer{fn: func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, st.path) {
						adds++
						return resp(200, st.body), nil
					}
					return resp(200, `{}`), nil
				}})
				_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: repeat(fmt.Sprintf("%x", i%16), 40)})
			}
			if left := globalAddBudget.remaining(budgetAccount(st.svc, "tok")); left != 50 {
				t.Errorf("allowance %d after %d empty answers — one bad title walks the hour", left, adds)
			}
		})
	}
}

// Refund what the ANSWER says was not created, and nothing more. `success:false` and an unparseable body
// are the two cases the empty-2xx branch exists for; a body that parses, claims no failure and simply
// carries an id under a name this struct does not know may describe a torrent that WAS created, and
// refunding there would manufacture allowance against a real add.
func TestAddBudget_torBoxRefundsOnlyWhatTheAnswerDenies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int // remaining out of 50
	}{
		{"success:false", `{"success":false}`, 50},
		{"an html proxy page", `<html>502 Bad Gateway</html>`, 50},
		{"an id under a name we do not know", `{"data":{"queued_id":91}}`, 49},
		{"a string id", `{"data":{"torrent_id":"7"}}`, 49},
		{"a one-element array", `{"data":[{"torrent_id":7}]}`, 49},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := globalAddBudget
			globalAddBudget = newAddBudget(time.Hour, 50)
			defer func() { globalAddBudget = prev }()

			s := &torBoxStore{token: "tok", cache: NewMemoryCache(1 << 20), api: torboxAPI,
				client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
					if isAddEndpoint(r) {
						return resp(200, tc.body), nil
					}
					return resp(200, `{}`), nil
				}}}
			_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			if left := globalAddBudget.remaining(budgetAccount(ServiceTorBox, "tok")); left != tc.want {
				t.Errorf("remaining = %d, want %d", left, tc.want)
			}
		})
	}
}

// A miss is only worth remembering when the account list was READ and the hash was not in it. A timeout
// or a cancelled poll is not the account saying no, and remembering it suppressed the one call that can
// rediscover a queued torrent — for fifteen seconds, on /play as well as the probe route.
func TestTorBox_aTimedOutListingIsNotAMiss(t *testing.T) {
	calls := 0
	cache := NewMemoryCache(1 << 20)
	failing := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			calls++
			return nil, context.DeadlineExceeded
		}}}
	if _, ok := failing.Status(context.Background(), ResolveTarget{InfoHash: H}); ok {
		t.Fatal("a timed-out listing cannot report a status")
	}
	if _, missed := cache.Get(torrentMissKey("tok", H)); missed {
		t.Error("a timeout was remembered as the account not holding the torrent")
	}

	// The list recovers, and the very next poll must be allowed to ask again.
	healthy := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			calls++
			if strings.Contains(r.URL.RawQuery, "id=") {
				return resp(200, `{"data":{"progress":0.5,"download_finished":false}}`), nil
			}
			return resp(200, fmt.Sprintf(`{"data":[{"id":77,"hash":%q}]}`, H)), nil
		}}}
	if _, ok := healthy.Status(context.Background(), ResolveTarget{InfoHash: H}); !ok {
		t.Error("the torrent was rediscoverable, but the miss marker suppressed the lookup")
	}

	// An authoritative miss — the list was read and the hash is not in it — IS still remembered.
	other := NewMemoryCache(1 << 20)
	absent := &torBoxStore{token: "tok", cache: other, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			return resp(200, `{"data":[]}`), nil
		}}}
	_, _ = absent.Status(context.Background(), ResolveTarget{InfoHash: H})
	if _, missed := other.Get(torrentMissKey("tok", H)); !missed {
		t.Error("an answered miss must still be remembered, or every poll re-fetches the whole list")
	}
}

// mylist is asked BY id, but it can answer with the whole account list — which is why the array fallback
// exists at all. Taking the first entry blind is the mistake listFiles was hardened against, and here it
// was worse: a finished first entry made a live download read as "no status", which handlePlay answers
// 404 dead_link, so the client blacklists a release that is downloading right now — the one failure
// Status exists to prevent. A downloading first entry is quieter and no better: the viewer watches
// another torrent's percentage, ETA and rate.
func TestTorBoxStatus_reportsOnTheTorrentItAskedAbout(t *testing.T) {
	const ours = 100
	list := func(entries string) doer {
		return mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			return resp(200, `{"data":[`+entries+`]}`), nil
		}}
	}
	finishedOther := `{"id":42,"progress":1,"download_finished":true,"eta":0,"download_speed":0}`
	busyOther := `{"id":42,"progress":0.43,"download_finished":false,"eta":900,"download_speed":1500000}`
	oursBusy := `{"id":100,"progress":0.02,"download_finished":false,"eta":60,"download_speed":10}`

	for _, tc := range []struct{ name, entries string }{
		{"ours behind a finished torrent", finishedOther + "," + oursBusy},
		{"ours behind a busy torrent", busyOther + "," + oursBusy},
		{"ours last of several", finishedOther + "," + busyOther + "," + oursBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewMemoryCache(1 << 20)
			cache.Put(torrentIDKey("tok", H), strconv.Itoa(ours), resolveCacheTTL)
			s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI, client: list(tc.entries)}
			got, ok := s.Status(context.Background(), ResolveTarget{InfoHash: H})
			if !ok {
				t.Fatal("our torrent is in the list and downloading; this must report on it")
			}
			if got.Progress != 0.02 || got.BytesPerSecond == nil || *got.BytesPerSecond != 10 {
				t.Errorf("reported %+v — that is another torrent's progress", got)
			}
		})
	}

	// A list that does not mention our torrent says nothing about it, and must not borrow an answer.
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), strconv.Itoa(ours), resolveCacheTTL)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI, client: list(busyOther)}
	if got, ok := s.Status(context.Background(), ResolveTarget{InfoHash: H}); ok {
		t.Errorf("reported %+v for a torrent the answer never mentions", got)
	}

	// The ordinary single-entry answer to a by-id query need not repeat the id, and still counts.
	cache = NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), strconv.Itoa(ours), resolveCacheTTL)
	s = &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			return resp(200, `{"data":{"progress":0.25,"download_finished":false,"eta":30}}`), nil
		}}}
	got, ok := s.Status(context.Background(), ResolveTarget{InfoHash: H})
	if !ok || got.Progress != 0.25 {
		t.Errorf("an unlabelled single entry is still ours: %+v ok=%v", got, ok)
	}
	// But an object naming a DIFFERENT torrent is not.
	cache = NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), strconv.Itoa(ours), resolveCacheTTL)
	s = &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			return resp(200, `{"data":{"id":42,"progress":0.9,"download_finished":false}}`), nil
		}}}
	if got, ok := s.Status(context.Background(), ResolveTarget{InfoHash: H}); ok {
		t.Errorf("reported %+v from an entry that names torrent 42", got)
	}
}

// A 200 with no `data` key is TorBox's envelope missing, not the account saying it holds nothing —
// listFiles reads the identical body as silence. Calling it a miss wrote a marker that then suppressed
// the only lookup able to rediscover a queued torrent.
func TestTorBox_anEnvelopeWithoutDataIsNotAMiss(t *testing.T) {
	for _, body := range []string{`{}`, `{"success":false}`, `{"success":false,"data":null}`} {
		cache := NewMemoryCache(1 << 20)
		s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
			client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				return resp(200, body), nil
			}}}
		_, _ = s.Status(context.Background(), ResolveTarget{InfoHash: H})
		if _, missed := cache.Get(torrentMissKey("tok", H)); missed {
			t.Errorf("%s was remembered as the account not holding the torrent", body)
		}
	}
	// A real, readable list that lacks the hash still is one.
	cache := NewMemoryCache(1 << 20)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			return resp(200, `{"success":true,"data":[]}`), nil
		}}}
	_, _ = s.Status(context.Background(), ResolveTarget{InfoHash: H})
	if _, missed := cache.Get(torrentMissKey("tok", H)); !missed {
		t.Error("an answered miss must still be remembered")
	}
}

// A refusal is a fact about the account, not the release — and the last calls of RD's chain classified
// nothing. `info`, one call earlier on the same read path, has drawn that line for a long time. So the
// same throttle answered 503 or 404 depending only on which call it landed on: a release RD is holding
// and would serve was reported dead, the client blacklisted it and walked the rest of the list, and
// every candidate hit the same throttled endpoint for the same answer. Nothing was recorded either,
// since resolveExisting only remembers a *StoreUnavailableError.
func TestRealDebrid_everyCallInTheChainClassifiesARefusal(t *testing.T) {
	const infoOK = `{"files":[{"id":1,"path":"Show.S01E01.mkv","bytes":900,"selected":1}],"links":["l1"]}`
	for _, tc := range []struct {
		name   string
		status int
		want   bool // want a StoreUnavailableError rather than a dead link
	}{
		{"selectFiles throttled", 429, true},
		{"selectFiles faulting", 503, true},
		{"selectFiles rejecting the file", 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewMemoryCache(1 << 20)
			s := &realDebridStore{token: "tok", cache: cache, api: realDebridAPI,
				client: routed{routes: map[string]func() (*http.Response, error){
					"addMagnet":     ok(`{"id":"t1"}`),
					"torrents/info": ok(infoOK),
					"selectFiles":   status(tc.status),
				}}}
			_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
			var unavailable *StoreUnavailableError
			if got := errors.As(err, &unavailable); got != tc.want {
				t.Errorf("got %v (unavailable=%v), want unavailable=%v", err, got, tc.want)
			}
		})
	}

	// The last call of the chain, on a release the account demonstrably holds.
	for _, code := range []int{429, 503} {
		cache := NewMemoryCache(1 << 20)
		s := &realDebridStore{token: "tok", cache: cache, api: realDebridAPI,
			client: routed{routes: map[string]func() (*http.Response, error){
				"addMagnet":     ok(`{"id":"t1"}`),
				"torrents/info": ok(infoOK),
				"selectFiles":   ok(`{}`),
				"unrestrict":    status(code),
			}}}
		_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
		var unavailable *StoreUnavailableError
		if !errors.As(err, &unavailable) {
			t.Errorf("unrestrict %d answered %v — a throttle is not a dead release", code, err)
		}
		// And being classified is what gets it remembered, so the next poll costs one call rather than four.
		if _, backed := backedOff(cache, ServiceRealDebrid, "tok", H); !backed {
			t.Errorf("unrestrict %d left no backoff, so nothing slows the retries", code)
		}
	}
}

// Premiumize's HTTP-200 `status:error` branch is the one carrying its real refusals, and it was the only
// detail site splicing the upstream message in unredacted — into text that is logged AND persisted into
// a refusal cache that writes through to disk, from where the probe route prints it back.
func TestPremiumize_theStatusErrorMessageCannotCarryTheToken(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 50)
	defer func() { globalAddBudget = prev }()

	const token = "pm-super-secret-apikey"
	cache := NewMemoryCache(1 << 20)
	s := &premiumizeStore{token: token, cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			return resp(200, `{"status":"error","message":"invalid src for `+string(body)+`"}`), nil
		}}}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("the token reached an error that gets logged: %v", err)
	}
	if reason, ok := backedOff(cache, ServicePremiumize, token, H); ok && strings.Contains(reason, token) {
		t.Errorf("the token was persisted into the refusal cache: %s", reason)
	}
}

// listFiles and Status parse the same mylist payload; they must not disagree about whose entry it is.
// listFiles checked the id only when the array had more than one element, so a single foreign entry
// still handed back its files.
func TestTorBoxListFiles_matchesTheIdAtAnyLength(t *testing.T) {
	foreign := `{"id":42,"files":[{"id":7,"name":"Wanted.S01E01.mkv","size":10}]}`
	ours := `{"id":100,"files":[{"id":3,"name":"Show.S01E01.mkv","size":10}]}`
	for _, tc := range []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"a lone foreign entry, object form", `{"data":` + foreign + `}`, true},
		{"a lone foreign entry, array form", `{"data":[` + foreign + `]}`, true},
		{"ours, object form", `{"data":` + ours + `}`, false},
		{"ours, array form", `{"data":[` + ours + `]}`, false},
		{"ours behind a foreign entry", `{"data":[` + foreign + `,` + ours + `]}`, false},
		// An entry with no id at all is still taken at its word.
		{"an unlabelled entry", `{"data":{"files":[{"id":3,"name":"Show.S01E01.mkv","size":10}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &torBoxStore{token: "tok", api: torboxAPI, client: routed{fallbk: ok(tc.payload)}}
			files, err := s.listFiles(context.Background(), 100)
			if tc.wantErr {
				if err == nil {
					t.Errorf("accepted another torrent's file list: %+v", files)
				}
				return
			}
			if err != nil || len(files) != 1 || files[0].Name != "Show.S01E01.mkv" {
				t.Errorf("got %+v, %v", files, err)
			}
		})
	}
}

// An exact id wins; an unlabelled entry is only a fallback. Taking the first of either meant an id-less
// entry ahead of ours in a multi-entry answer was chosen over the entry that actually names our torrent
// — worse than the blind arr[0] it replaced, because it looks like a check.
func TestTorBoxListFiles_anExactIdBeatsAnUnlabelledEntry(t *testing.T) {
	ours := `{"id":100,"files":[{"id":3,"name":"Show.S01E01.mkv","size":10}]}`
	unlabelled := `{"files":[{"id":7,"name":"OTHER.mkv","size":10}]}`
	zeroID := `{"id":0,"files":[{"id":7,"name":"OTHER.mkv","size":10}]}`
	for _, tc := range []struct{ name, payload, want string }{
		{"unlabelled first", `{"data":[` + unlabelled + `,` + ours + `]}`, "Show.S01E01.mkv"},
		{"id 0 first", `{"data":[` + zeroID + `,` + ours + `]}`, "Show.S01E01.mkv"},
		{"ours first", `{"data":[` + ours + `,` + unlabelled + `]}`, "Show.S01E01.mkv"},
		// With no exact match anywhere, an unlabelled entry is still better than nothing.
		{"only unlabelled", `{"data":[` + unlabelled + `]}`, "OTHER.mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &torBoxStore{token: "tok", api: torboxAPI, client: routed{fallbk: ok(tc.payload)}}
			files, err := s.listFiles(context.Background(), 100)
			if err != nil || len(files) != 1 || files[0].Name != tc.want {
				t.Errorf("got %+v, %v — want %s", files, err, tc.want)
			}
		})
	}
}

// A 403 from unrestrict is RD refusing a FILE, not the account: realDebridBlocked exists because RD
// rejects specific files, and refusalIsAboutTheAccount keys on 401/403 — so passing it through escalated
// one unplayable file into a sixty-second account-wide outage that took unrelated releases with it.
func TestRealDebrid_aForbiddenLinkIsNotAForbiddenAccount(t *testing.T) {
	const infoOK = `{"files":[{"id":1,"path":"Show.S01E01.mkv","bytes":900,"selected":1}],"links":["l1"]}`
	chain := func(unrestrictCode int) doer {
		return routed{routes: map[string]func() (*http.Response, error){
			"addMagnet":     ok(`{"id":"t1"}`),
			"torrents/info": ok(infoOK),
			"selectFiles":   ok(`{}`),
			"unrestrict":    status(unrestrictCode),
		}}
	}
	for _, tc := range []struct {
		code    int
		account bool
	}{
		{403, false}, // one file RD will not serve
		{401, true},  // the key itself
		{429, false}, // a throttle: per-release, not account-wide
	} {
		cache := NewMemoryCache(1 << 20)
		s := &realDebridStore{token: "tok", cache: cache, api: realDebridAPI, client: chain(tc.code)}
		_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H})
		if _, got := accountBackedOff(cache, ServiceRealDebrid, "tok"); got != tc.account {
			t.Errorf("unrestrict %d: account backoff = %v, want %v", tc.code, got, tc.account)
		}
	}
}

// The warm-entry fast path returns a refusal from the same requestdl call the held path uses, and only
// the held path remembered it. A warm entry is the normal state during a binge, so a rejected key was
// re-asked once per poll — and the account gate a few lines above already said as much ("that branch
// records no refusal either, so it could not even set the key it skipped") without fixing it.
func TestTorBox_theWarmPathRemembersItsRefusal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		account bool
	}{
		{"a throttle", 429, false},
		{"a rejected key", 401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			cache := NewMemoryCache(1 << 20)
			cache.Put(resolveKey("tok", H),
				`{"torrentId":42,"files":[{"Index":0,"Name":"m.mkv","SizeBytes":9}]}`, resolveCacheTTL)
			s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
				client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "requestdl") {
						calls++
						return resp(tc.status, `{}`), nil
					}
					return resp(200, `{}`), nil
				}}}
			target := ResolveTarget{InfoHash: H, FileIdx: intp(0)}
			for i := 0; i < 10; i++ {
				_, _ = s.Resolve(context.Background(), target)
			}
			if _, ok := backedOff(cache, ServiceTorBox, "tok", H); !ok {
				t.Error("the refusal was not remembered, so every poll re-asks")
			}
			if calls > 1 {
				t.Errorf("asked %d times across ten polls", calls)
			}
			if _, ok := accountBackedOff(cache, ServiceTorBox, "tok"); ok != tc.account {
				t.Errorf("account backoff = %v, want %v", ok, tc.account)
			}
		})
	}
}

// listFiles and Status read the same mylist payload and must agree about whose entry it is. Status was
// left on first-of-either, and it is the reader that decides "downloading" against "dead": a finished
// stranger at the head of the array made a live download report nothing, which /play answers 404
// dead_link — the client blacklists a release that is downloading right now.
func TestTorBoxStatus_anExactIdBeatsAnUnlabelledEntry(t *testing.T) {
	const ours = 42
	unlabelledFinished := `{"progress":1,"download_finished":true,"eta":0,"download_speed":0}`
	strangerBusy := `{"id":9,"progress":0.11,"download_finished":false,"eta":9999,"download_speed":5}`
	oursBusy := `{"id":42,"progress":0.5,"download_finished":false,"eta":600,"download_speed":900000}`

	for _, tc := range []struct {
		name    string
		entries string
	}{
		{"an unlabelled finished entry first", unlabelledFinished + "," + oursBusy},
		{"a stranger downloading first", strangerBusy + "," + oursBusy},
		{"both ahead of ours", unlabelledFinished + "," + strangerBusy + "," + oursBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewMemoryCache(1 << 20)
			cache.Put(torrentIDKey("tok", H), strconv.Itoa(ours), resolveCacheTTL)
			s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
				client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
					return resp(200, `{"data":[`+tc.entries+`]}`), nil
				}}}
			got, ok := s.Status(context.Background(), ResolveTarget{InfoHash: H})
			if !ok {
				t.Fatal("our torrent is in the list and downloading; this must report on it")
			}
			if got.Progress != 0.5 {
				t.Errorf("reported %+v — that is another torrent's progress", got)
			}
		})
	}

	// With no exact match anywhere, an id-less entry is still the best available answer.
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), strconv.Itoa(ours), resolveCacheTTL)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			return resp(200, `{"data":[{"progress":0.25,"download_finished":false}]}`), nil
		}}}
	if got, ok := s.Status(context.Background(), ResolveTarget{InfoHash: H}); !ok || got.Progress != 0.25 {
		t.Errorf("an unlabelled lone entry is still ours: %+v ok=%v", got, ok)
	}
}

// A read-only caller is exempt from the gate that READS the per-release backoff, so it must not WRITE
// one either: the probe re-stamped a refusal it would never consult, which kept /play gated for exactly
// as long as the probe kept polling.
func TestTorBox_aReadOnlyResolveWritesNoBackoff(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	cache.Put(resolveKey("tok", H),
		`{"torrentId":42,"files":[{"Index":0,"Name":"m.mkv","SizeBytes":9}]}`, resolveCacheTTL)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "requestdl") {
				return resp(429, `{}`), nil
			}
			return resp(200, `{}`), nil
		}}}
	for i := 0; i < 5; i++ {
		_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0), NoAdd: true})
	}
	if _, ok := backedOff(cache, ServiceTorBox, "tok", H); ok {
		t.Error("a read-only resolve wrote the backoff that gates /play")
	}
	// And a real play still records it, so the poll loop is bounded where it matters.
	_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)})
	if _, ok := backedOff(cache, ServiceTorBox, "tok", H); !ok {
		t.Error("a queueing resolve must still remember the refusal")
	}
}

// recordRefusal feeds two memories and a read-only caller stands differently to each. The per-release
// key is an add-path guard, so a probe must not write one — it would gate the /play behind it for a
// minute with no upstream call. The ACCOUNT key is read unconditionally at the top of Resolve, NoAdd
// included, because a rejected key makes every request pointless; guarding both together left the probe
// unable to set the very key it then reads, so it re-asked a rejected endpoint once per poll.
func TestTorBox_aReadOnlyResolveWritesOnlyTheAccountRefusal(t *testing.T) {
	// Both paths that record: the warm resolve entry, and the held torrent id.
	for _, path := range []string{"warm", "held"} {
		for _, tc := range []struct {
			name           string
			status         int
			wantPerRelease bool
			wantAccount    bool
		}{
			{"a throttle", 429, false, false},
			// A rejected key writes both, because recordRefusal writes the per-release key alongside the
			// account one. That is harmless where it would not be for a throttle: the account gate blocks
			// every release on this token anyway, so the narrower key gates nothing that was not already
			// gated. What matters is that a THROTTLE — an add-path fact — writes neither.
			{"a rejected key", 401, true, true},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				calls := 0
				cache := NewMemoryCache(1 << 20)
				if path == "warm" {
					cache.Put(resolveKey("tok", H),
						`{"torrentId":42,"files":[{"Index":0,"Name":"m.mkv","SizeBytes":9}]}`, resolveCacheTTL)
				} else {
					cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
				}
				s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
					client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
						if strings.Contains(r.URL.Path, "requestdl") {
							calls++
							return resp(tc.status, `{}`), nil
						}
						return resp(200, `{"data":{"id":42,"files":[{"id":0,"name":"m.mkv","size":9}]}}`), nil
					}}}
				probe := ResolveTarget{InfoHash: H, FileIdx: intp(0), NoAdd: true}
				for i := 0; i < 5; i++ {
					_, _ = s.Resolve(context.Background(), probe)
				}
				if _, ok := backedOff(cache, ServiceTorBox, "tok", H); ok != tc.wantPerRelease {
					t.Errorf("per-release backoff = %v, want %v — a probe must not gate /play", ok, tc.wantPerRelease)
				}
				if _, ok := accountBackedOff(cache, ServiceTorBox, "tok"); ok != tc.wantAccount {
					t.Errorf("account backoff = %v, want %v", ok, tc.wantAccount)
				}
				// A dead key must stop the probe re-asking; a throttle is an add-path fact and does not.
				if tc.wantAccount && calls > 1 {
					t.Errorf("asked a rejected endpoint %d times across five polls", calls)
				}
			})
		}
	}

	// And a queueing resolve still writes the per-release key, which is where the bound is wanted.
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tok", H), "42", resolveCacheTTL)
	s := &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "requestdl") {
				return resp(429, `{}`), nil
			}
			return resp(200, `{"data":{"id":42,"files":[{"id":0,"name":"m.mkv","size":9}]}}`), nil
		}}}
	_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)})
	if _, ok := backedOff(cache, ServiceTorBox, "tok", H); !ok {
		t.Error("a queueing resolve must still remember the refusal")
	}
}

// The account listing is the WHOLE account, and it was pulled once per infohash because the only memo
// was the per-hash miss marker. A poster grid probing eight releases made eight identical full-list
// requests. The question "what does this account hold?" has one answer at a time.
func TestTorBox_theAccountListingIsFetchedOncePerAccount(t *testing.T) {
	// A realistic account: 300 torrents, one of which is the one being asked about.
	var entries []string
	for i := 0; i < 300; i++ {
		entries = append(entries, fmt.Sprintf(`{"id":%d,"hash":"%s"}`, i, repeat(fmt.Sprintf("%x", i%16), 40)))
	}
	entries = append(entries, fmt.Sprintf(`{"id":999,"hash":%q}`, H))
	payload := `{"success":true,"data":[` + strings.Join(entries, ",") + `]}`

	fetches, bytesOut := 0, 0
	cache := NewMemoryCache(1 << 20)
	newStore := func() *torBoxStore {
		return &torBoxStore{token: "tok", cache: cache, api: torboxAPI,
			client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "mylist") && !strings.Contains(r.URL.RawQuery, "id=") {
					fetches++
					bytesOut += len(payload)
					return resp(200, payload), nil
				}
				return resp(200, `{"data":{"progress":0.5,"download_finished":false}}`), nil
			}}}
	}

	// Eight different releases, the poster-grid shape. Only one is in the account.
	hashes := []string{H}
	for i := 0; i < 7; i++ {
		hashes = append(hashes, repeat(fmt.Sprintf("%x", i), 40))
	}
	for _, h := range hashes {
		_, _ = newStore().Status(context.Background(), ResolveTarget{InfoHash: h})
	}
	if fetches != 1 {
		t.Errorf("fetched the account listing %d times for %d releases (%d bytes)", fetches, len(hashes), bytesOut)
	}
	// And the answer is still right for both a hash it holds and one it does not.
	if _, ok := newStore().Status(context.Background(), ResolveTarget{InfoHash: H}); !ok {
		t.Error("a held, downloading torrent must still be found through the memo")
	}
	absent := strings.Repeat("dead", 10) // 40 chars, and not one of the generated single-digit hashes
	if _, ok := newStore().Status(context.Background(), ResolveTarget{InfoHash: absent}); ok {
		t.Error("a hash the account does not hold must not be found")
	}
}

// The same hash is asked about three times within seconds — by /stream, by ?probe=1, and by /play on a
// two-account install. "Held" is stable, so one answer serves all three. "Not held" is not: it becomes
// held the instant a download finishes, and noticing that transition is the entire job of the poll loop,
// so a negative is never memoised.
func TestCacheCheck_remembersHeldButNeverNotHeld(t *testing.T) {
	for _, st := range []struct {
		name string
		svc  DebridService
		body func(held bool) string
		make func(Cache, doer) Store
	}{
		{"torbox", ServiceTorBox, func(held bool) string {
			if held {
				return `{"data":{"` + H + `":{"name":"x"}}}`
			}
			return `{"data":{}}`
		}, func(c Cache, d doer) Store {
			return &torBoxStore{token: "tok", cache: c, api: torboxAPI, client: d}
		}},
		{"premiumize", ServicePremiumize, func(held bool) string {
			if held {
				return `{"status":"success","response":[true]}`
			}
			return `{"status":"success","response":[false]}`
		}, func(c Cache, d doer) Store {
			return &premiumizeStore{token: "tok", cache: c, api: premiumizeAPI, client: d}
		}},
	} {
		t.Run(st.name, func(t *testing.T) {
			// A release the account HOLDS: asked once, answered three times.
			calls := 0
			cache := NewMemoryCache(1 << 20)
			s := st.make(cache, mockDoer{fn: func(*http.Request) (*http.Response, error) {
				calls++
				return resp(200, st.body(true)), nil
			}})
			for i := 0; i < 3; i++ {
				got, err := s.CacheCheck(context.Background(), []string{H})
				if err != nil || !got[H] {
					t.Fatalf("check %d: %v %v", i, got, err)
				}
			}
			if calls != 1 {
				t.Errorf("asked %d times about a release the account holds", calls)
			}

			// A release it does NOT hold: asked every time, because the answer is what changes when the
			// download lands.
			calls = 0
			cache = NewMemoryCache(1 << 20)
			held := false
			s = st.make(cache, mockDoer{fn: func(*http.Request) (*http.Response, error) {
				calls++
				return resp(200, st.body(held)), nil
			}})
			for i := 0; i < 3; i++ {
				if got, _ := s.CacheCheck(context.Background(), []string{H}); got[H] {
					t.Fatalf("check %d claimed a release that is still downloading", i)
				}
			}
			if calls != 3 {
				t.Errorf("asked %d times about a downloading release — the poll must see it land", calls)
			}
			// It lands, and the very next check must notice.
			held = true
			if got, _ := s.CacheCheck(context.Background(), []string{H}); !got[H] {
				t.Error("the download finished and the next check did not notice")
			}
		})
	}
}
