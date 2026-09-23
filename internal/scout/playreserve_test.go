package scout

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A prefetch stops at the reserve; a viewer waiting spends down to zero; both come back as the window
// drains, each at its own ceiling.
func TestAddBudget_prefetchStopsAtTheReserveAndPlaySpendsToZero(t *testing.T) {
	now := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)
	b := newAddBudget(time.Hour, 10)
	b.reserve = 3
	b.now = func() time.Time { return now }

	// Seven prefetches, a minute apart: the last one leaves exactly the reserve.
	for i := 0; i < 7; i++ {
		if !b.take("acct", true) {
			t.Fatalf("prefetch %d refused with %d left, above the reserve of 3", i+1, b.remaining("acct"))
		}
		now = now.Add(time.Minute)
	}
	if b.take("acct", true) {
		t.Fatal("a prefetch spent into the reserve kept for Play")
	}
	if got := b.remaining("acct"); got != 3 {
		t.Fatalf("remaining = %d after the refused prefetch, want 3 — a refusal must spend nothing", got)
	}
	// The first charge (20:00) drains at 21:00, which frees one prefetch.
	if got := b.freesIn("acct", true); got != 53*time.Minute {
		t.Errorf("prefetch freesIn = %v, want 53m (until the 20:00 charge drains at 21:00)", got)
	}
	if got := b.freesIn("acct", false); got != 0 {
		t.Errorf("a viewer was told to wait (%v) with the reserve still there", got)
	}

	// The viewer spends the reserve, down to zero, and no further.
	for i := 0; i < 3; i++ {
		if !b.take("acct", false) {
			t.Fatalf("Play refused with %d left", b.remaining("acct"))
		}
	}
	if b.take("acct", false) {
		t.Fatal("Play spent past the whole allowance")
	}
	if b.take("acct", true) {
		t.Fatal("a prefetch was served from a spent allowance")
	}

	// 21:00:30 — the 20:00 charge has drained: one slot, which is the viewer's, not the prefetch's.
	now = time.Date(2026, 9, 22, 21, 0, 30, 0, time.UTC)
	if b.take("acct", true) {
		t.Error("a prefetch took the one slot that freed while the account was below the reserve")
	}
	if !b.take("acct", false) {
		t.Error("Play did not recover as the window drained")
	}

	// 21:06:30 — every prefetch charge (20:00–20:06) has drained; three viewer charges from 20:07 and one
	// from 21:00:30 remain, so six are left and three of them are the prefetch's.
	now = time.Date(2026, 9, 22, 21, 6, 30, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if !b.take("acct", true) {
			t.Fatalf("prefetch %d did not recover as the window drained (%d left)", i+1, b.remaining("acct"))
		}
	}
	if b.take("acct", true) {
		t.Error("a recovered prefetch spent into the reserve")
	}
}

// With no flag the reserve does not exist: a viewer's ceiling is the whole allowance, exactly as before.
func TestAddBudget_withoutTheFlagTheReserveChangesNothing(t *testing.T) {
	b := newAddBudget(time.Hour, 5)
	b.reserve = 4
	for i := 0; i < 5; i++ {
		if !b.take("acct", false) {
			t.Fatalf("add %d of 5 refused — the reserve leaked into the default intent", i+1)
		}
	}
	if b.take("acct", false) {
		t.Error("a sixth add against an allowance of 5")
	}
}

// A reserve refusal is its own error: scout-side (never filed as the store's refusal) and apart from a spent
// budget, so the route can answer it with its own code.
func TestSpendAdd_prefetchRefusalIsTheReserveNotTheBudget(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 6)
	globalAddBudget.reserve = 5
	defer func() { globalAddBudget = prev }()

	before := metrics.addRefusedPrefetch.Load()
	if err := spendAdd(ServiceTorBox, "tok", H, true); err != nil {
		t.Fatalf("the one prefetch add above the reserve was refused: %v", err)
	}
	err := spendAdd(ServiceTorBox, "tok", H, true)
	if !errors.Is(err, errPlayReserve) || !errors.Is(err, errScoutSide) {
		t.Fatalf("want errPlayReserve wrapped as scout-side, got %v", err)
	}
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServiceTorBox, "tok", H, err)
	if _, remembered := backedOff(cache, ServiceTorBox, "tok", H); remembered {
		t.Error("the reserve was written into the store's refusal memory")
	}
	if got := metrics.addRefusedPrefetch.Load() - before; got != 1 {
		t.Errorf("prefetch refusals counted %d, want 1", got)
	}

	playBefore := metrics.addRefusedPlay.Load()
	for i := 0; i < 5; i++ {
		if err := spendAdd(ServiceTorBox, "tok", H, false); err != nil {
			t.Fatalf("Play refused with the reserve left: %v", err)
		}
	}
	err = spendAdd(ServiceTorBox, "tok", H, false)
	if err == nil || errors.Is(err, errPlayReserve) {
		t.Fatalf("a spent budget must refuse Play as spent, not as the reserve: %v", err)
	}
	if got := metrics.addRefusedPlay.Load() - playBefore; got != 1 {
		t.Errorf("play refusals counted %d, want 1", got)
	}
}

// Through the route: with the allowance at the reserve, /play?prefetch=1 is refused with its own code and a
// Retry-After and sends no add, while the same request without the flag still adds.
func TestHandlePlay_prefetchIsHeldBackAndPlayIsServed(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 5)
	globalAddBudget.reserve = 5 // only the reserve is left: no prefetch may add, a viewer still may
	defer func() { globalAddBudget = prev }()

	adds := 0
	cache := NewMemoryCache(1 << 20)
	h := NewHandler(Deps{
		Cache: cache,
		MakeStores: func(*Config) []Store {
			return []Store{&torBoxStore{token: "tok", client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if isAddEndpoint(r) {
					adds++
				}
				return resp(200, `{"data":[]}`), nil
			}}, cache: cache, api: torboxAPI}}
		},
	})
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: H})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path+"?prefetch=1", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"reserved_for_play"`) {
		t.Fatalf("prefetch at the reserve: got %d %s, want 503 reserved_for_play", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "scout_busy") || strings.Contains(rec.Body.String(), "store_unavailable") {
		t.Errorf("the reserve must not read as a spent budget or a refusing store: %s", rec.Body.String())
	}
	secs, err := strconv.Atoi(rec.Header().Get("retry-after"))
	if err != nil || secs < 1 {
		t.Errorf("retry-after = %q, want a positive number of seconds", rec.Header().Get("retry-after"))
	}
	if adds != 0 {
		t.Fatalf("a held-back prefetch sent %d adds", adds)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if strings.Contains(rec.Body.String(), "reserved_for_play") || strings.Contains(rec.Body.String(), "scout_busy") {
		t.Errorf("Play was refused by the budget with the reserve left: %d %s", rec.Code, rec.Body.String())
	}
	if adds != 1 {
		t.Errorf("Play sent %d adds, want 1 — the reserve is for it", adds)
	}
}

// /metrics reports the allowance as each intent sees it, and the refusals per intent.
func TestMetrics_reportTheAllowanceAndRefusalsPerIntent(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 10)
	globalAddBudget.reserve = 3
	defer func() { globalAddBudget = prev }()

	out := metrics.render(-1)
	for _, want := range []string{
		`scout_add_budget_remaining_by_intent{intent="play"} -1`,
		`scout_add_budget_remaining_by_intent{intent="prefetch"} -1`,
		`scout_add_budget_refusals_total{intent="play"}`,
		`scout_add_budget_refusals_total{intent="prefetch"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("before any spend, /metrics lacks %q", want)
		}
	}

	for i := 0; i < 8; i++ {
		globalAddBudget.take("acct", false)
	}
	out = metrics.render(-1)
	for _, want := range []string{
		"scout_add_budget_remaining 2\n",
		`scout_add_budget_remaining_by_intent{intent="play"} 2` + "\n",
		`scout_add_budget_remaining_by_intent{intent="prefetch"} 0` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics lacks %q:\n%s", want, out)
		}
	}
}

func TestParsePlayReserve(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", defaultPlayReserve},
		{"0", 0},
		{" 8 ", 8},
		{"-1", defaultPlayReserve},
		{"five", defaultPlayReserve},
		{strconv.Itoa(addBudgetLimit), defaultPlayReserve}, // would leave a prefetch nothing at all
	} {
		if got := parsePlayReserve(tc.in); got != tc.want {
			t.Errorf("parsePlayReserve(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if got := SettingsFromEnv(func(k string) string {
		if k == "PLAY_RESERVE_ADDS" {
			return "2"
		}
		return ""
	}).PlayReserveAdds; got != 2 {
		t.Errorf("PLAY_RESERVE_ADDS=2 read as %d", got)
	}
}

func TestSetPlayReserve(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 10)
	defer func() { globalAddBudget = prev }()
	SetPlayReserve(7)
	if got := globalAddBudget.playReserve(); got != 7 {
		t.Errorf("reserve = %d, want 7", got)
	}
}
