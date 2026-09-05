package scout

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A refusal names the service and its reason. This string reaches the app, which renders it to the
// viewer as "TorBox is refusing requests right now" — a sentence someone can act on, unlike the "no
// source found" it replaced.
func TestStoreUnavailableError_namesTheService(t *testing.T) {
	err := &StoreUnavailableError{Service: ServiceTorBox, Reason: "createtorrent http 429 (60 per 1 hour)"}
	msg := err.Error()
	if !strings.Contains(msg, "torbox") || !strings.Contains(msg, "429") {
		t.Errorf("a refusal must carry who refused and why: %s", msg)
	}
	if !strings.HasPrefix(msg, "store_unavailable:") {
		t.Errorf("prefix identifies the class: %s", msg)
	}
}

// The probe route cannot discover a refusal on its own — it deliberately never calls the endpoint that
// refuses. It reads the backoff the queueing path wrote, so a throttled account reads as "the service
// refused us" rather than as an absence, which the client cannot tell from a dead release.
func TestRecentRefusal_readsWhatTheQueueingPathWrote(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	store := &torBoxStore{token: "tok", cache: cache, api: "https://api.example",
		client: &stubDoer{status: 200, body: `{"data":[]}`}}
	pool := &StorePool{stores: []Store{store}}

	if _, _, ok := pool.RecentRefusal("hash-none"); ok {
		t.Error("nothing was refused, so nothing should be reported")
	}

	cache.Put(refusedKey(ServiceTorBox, "tok", "hash-refused"), "createtorrent http 429", time.Minute)
	svc, reason, ok := pool.RecentRefusal("hash-refused")
	if !ok || svc != ServiceTorBox || !strings.Contains(reason, "429") {
		t.Errorf("refusal not reported: %v %q %v", svc, reason, ok)
	}

	// A store with no cache remembers nothing, and must not claim otherwise.
	bare := &StorePool{stores: []Store{&torBoxStore{token: "tok", api: "https://api.example",
		client: &stubDoer{status: 200, body: `{"data":[]}`}}}}
	if _, _, ok := bare.RecentRefusal("hash-refused"); ok {
		t.Error("a store with no cache cannot report a refusal")
	}
}

// The disk tier turns itself off on the first write failure rather than logging the same fact per entry.
// Once off it must stay off and must not panic — the memory tier carries on alone.
func TestTieredCache_disablesOnceAndKeepsServing(t *testing.T) {
	// A path that cannot be created: the parent is a file, not a directory.
	c := NewTieredCache(1<<20, "/dev/null/not-a-dir")
	c.Put("k", "v", time.Minute)
	if !c.disabled() {
		t.Error("a failing disk tier should disable itself")
	}
	c.Put("k2", "v2", time.Minute) // must not panic once off

	// The memory tier still answers — losing persistence is not losing the cache.
	if got, ok := c.Get("k"); !ok || got != "v" {
		t.Errorf("memory tier stopped serving after the disk tier failed: %q %v", got, ok)
	}
}

// The disk tier round-trips a value and expires it. This is what makes a restart cheap: the container is
// redeployed on every image push, and a probe costs a debrid resolve to rebuild.
func TestTieredCache_persistsAndExpires(t *testing.T) {
	dir := t.TempDir()
	c := NewTieredCache(1<<20, dir)
	c.Put("live", "value", time.Minute)
	c.Put("dead", "gone", time.Millisecond)

	// A fresh cache over the same directory: memory is empty, so a hit proves it came off disk.
	reopened := NewTieredCache(1<<20, dir)
	if got, ok := reopened.Get("live"); !ok || got != "value" {
		t.Errorf("an entry did not survive a restart: %q %v", got, ok)
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok := reopened.Get("dead"); ok {
		t.Error("an expired entry was served from disk")
	}
	if _, ok := reopened.Get("never-written"); ok {
		t.Error("a key that was never written must miss")
	}
}

// A probe answers what is already true and starts nothing. `handleProbe` is the route that exists so a
// client can poll a wait without re-queueing the torrent on every poll — the bug that spent an hour's
// worth of the debrid's add allowance inside a single wait.
func TestHandleProbe_reportsWithoutQueueing(t *testing.T) {
	resolves := 0
	h := &handler{deps: Deps{
		Cache: NewMemoryCache(1 << 20),
		MakeStores: func(*Config) []Store {
			// The store ANSWERED and said it does not hold this — "not queued" is a fact here, not a
			// shrug. A store that could not be asked gets a different answer, tested below.
			return []Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{"abc": false},
				resolve: func() (string, error) {
					resolves++
					return "", nil
				}}}
		},
	}}
	rec := httptest.NewRecorder()
	pool := &StorePool{stores: h.deps.MakeStores(&Config{})}
	h.handleProbe(rec, context.Background(), probeConfig(), pool, "abc", ResolveTarget{InfoHash: "abc"})

	if resolves != 0 {
		t.Errorf("a probe resolved %d times — resolving adds the torrent", resolves)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("nothing queued should be 404 not_queued, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not_queued") {
		t.Errorf("the body must say which kind of empty this is: %s", rec.Body.String())
	}
}

// The probe's other three answers. Each is a different fact, and the app renders each differently — a
// download in progress, a release ready to play, and an account being refused are three situations that
// spent an evening being indistinguishable from one another.
func TestHandleProbe_distinguishesItsAnswers(t *testing.T) {
	probe := func(stores []Store) *httptest.ResponseRecorder {
		h := &handler{deps: Deps{Cache: NewMemoryCache(1 << 20),
			MakeStores: func(*Config) []Store { return stores }}}
		rec := httptest.NewRecorder()
		h.handleProbe(rec, context.Background(), probeConfig(), &StorePool{stores: stores}, "abc",
			ResolveTarget{InfoHash: "abc"})
		return rec
	}

	// Downloading → 202 with whatever progress the store reported.
	eta := 120
	downloading := probe([]Store{fakeStore{svc: ServiceTorBox,
		status: &StoreStatus{Progress: 0.42, ETASeconds: &eta}}})
	if downloading.Code != http.StatusAccepted {
		t.Errorf("a download in progress should be 202, got %d", downloading.Code)
	}
	if !strings.Contains(downloading.Body.String(), "0.42") {
		t.Errorf("progress must reach the client: %s", downloading.Body.String())
	}

	// Held by the store → 200 ready, without minting a link (that is /play's job).
	// Held AND resolvable without an add: both are required, because a cache check alone cannot say the
	// account can serve it — TorBox reports what TorBox has, not what this account has.
	ready := probe([]Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{"abc": true},
		resolve: func() (string, error) { return "https://cdn/x", nil }}})
	if ready.Code != http.StatusOK || !strings.Contains(ready.Body.String(), "ready") {
		t.Errorf("a held release should be 200 ready: %d %s", ready.Code, ready.Body.String())
	}

	// Recently refused → 503, naming the service. Never 404: the account was turned away, which says
	// nothing at all about whether the release exists.
	cache := NewMemoryCache(1 << 20)
	cache.Put(refusedKey(ServiceTorBox, "tok", "abc"), "createtorrent http 429", time.Minute)
	refusedStore := &torBoxStore{token: "tok", cache: cache, api: "https://api.example",
		client: &stubDoer{status: 200, body: `{"data":[]}`}}
	h := &handler{deps: Deps{Cache: NewMemoryCache(1 << 20),
		MakeStores: func(*Config) []Store { return []Store{refusedStore} }}}
	rec := httptest.NewRecorder()
	h.handleProbe(rec, context.Background(), probeConfig(), &StorePool{stores: []Store{refusedStore}}, "abc",
		ResolveTarget{InfoHash: "abc"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a refused account should be 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "store_unavailable") {
		t.Errorf("the client must be able to tell a refusal from an absence: %s", rec.Body.String())
	}
}

// Every store draws the same line: being turned away is a fact about the account, not the release.
//
// Only TorBox did. On a Real-Debrid or Premiumize install a 429 became `DeadLinkError`, reached the app
// as 404 "this release does not exist", and the player then walked the whole candidate list collecting
// the identical non-answer — condemning healthy releases on the way. That is the bug that was fixed once
// and left unfixed twice.
func TestEveryStoreReportsARefusalAsARefusal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(doer) Store
		want  DebridService
	}{
		{"realdebrid", func(d doer) Store {
			return &realDebridStore{token: "t", client: d, api: "https://rd.example"}
		}, ServiceRealDebrid},
		{"premiumize", func(d doer) Store {
			return &premiumizeStore{token: "t", client: d, api: "https://pm.example"}
		}, ServicePremiumize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.build(&stubDoer{status: http.StatusTooManyRequests, body: `{}`})
			_, err := store.Resolve(context.Background(), ResolveTarget{InfoHash: repeat("e", 40)})
			if err == nil {
				t.Fatal("a 429 must be an error")
			}
			var unavailable *StoreUnavailableError
			if !errors.As(err, &unavailable) {
				t.Fatalf("a throttle must not read as a dead link: %T %v", err, err)
			}
			if unavailable.Service != tc.want {
				t.Errorf("wrong service named: %s", unavailable.Service)
			}
		})
	}
}

// An unconfigured indexer must not make "nobody has this" permanently unsayable.
//
// Counting an indexer that can never be asked as one that did not answer made the quorum unsatisfiable
// on the shipped default (mediafusion has no config URL), so every genuinely empty result was reported
// as an outage, negative caching never ran, and three unavailable titles in a row flipped /health to
// degraded on a perfectly healthy service. A permanent misconfiguration is not a transient failure.
func TestScrapeAll_unaskableIndexerDoesNotBlockAnEmptyVerdict(t *testing.T) {
	answeredEmpty := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) { return nil, nil }}
	unaskable := unaskableScraper{indexer: "mediafusion"}

	_, ok, _ := scrapeAll(context.Background(), []scraper{answeredEmpty, unaskable}, scrapeQuery{}, time.Second)
	if !ok {
		t.Error("every askable indexer answered, so the empty result is authoritative")
	}

	// A non-empty list is trusted whatever the coverage: whatever came back is real.
	found := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
		return []RawStream{{InfoHash: repeat("d", 40), Title: "a release"}}, nil
	}}
	if seeds, ok, _ := scrapeAll(context.Background(), []scraper{found, unaskable}, scrapeQuery{},
		time.Second); !ok || len(seeds) != 1 {
		t.Errorf("a non-empty result stands on its own: %d seeds, ok=%v", len(seeds), ok)
	}

	// A real failure still withholds the verdict — that is the rule this must not weaken.
	failed := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
		return nil, fmt.Errorf("502")
	}}
	if _, ok, _ := scrapeAll(context.Background(), []scraper{failed, unaskable}, scrapeQuery{}, time.Second); ok {
		t.Error("an indexer that failed means the empty result is not authoritative")
	}
}

// Season and episode are read off the stream id, and a movie has neither. A wrong answer here sends the
// probe at the wrong episode of a pack.
func TestSeasonEpisodeOf(t *testing.T) {
	// nil, not zero: episode 0 is a real episode on some series, so "no episode" needs its own value.
	if seasonOf(nil) != nil || episodeOf(nil) != nil {
		t.Error("a nil id has neither a season nor an episode")
	}
	sid := &StreamID{Type: "series", IMDb: "tt1", Season: 3, Episode: 7, HasEp: true}
	if s := seasonOf(sid); s == nil || *s != 3 {
		t.Errorf("season not read: %v", s)
	}
	if e := episodeOf(sid); e == nil || *e != 7 {
		t.Errorf("episode not read: %v", e)
	}
	movie := &StreamID{Type: "movie", IMDb: "tt2"}
	if seasonOf(movie) != nil || episodeOf(movie) != nil {
		t.Error("a movie has neither")
	}
}

// The backoff belongs to every store that can be refused, not just the one whose refusal was noticed
// first.
//
// Only TorBox remembered a refusal. The other two had no cache at all, so a client polling /play for the
// length of a download re-added the magnet once per poll — hundreds of adds for one wait, each leaving a
// duplicate torrent on the account. TorBox backing off correctly made this WORSE: ResolvePreferring fell
// straight through to whichever store had no memory of being turned away.
func TestRefusalBackoff_appliesToEveryStore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(Cache, doer) Store
	}{
		{"realdebrid", func(c Cache, d doer) Store {
			return &realDebridStore{token: "tok", client: d, cache: c, api: realDebridAPI}
		}},
		{"premiumize", func(c Cache, d doer) Store {
			return &premiumizeStore{token: "tok", client: d, cache: c, api: premiumizeAPI}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adds := 0
			d := mockDoer{fn: func(*http.Request) (*http.Response, error) {
				adds++
				return resp(503, `{"error":"busy"}`), nil
			}}
			store := tc.build(NewMemoryCache(1<<20), d)
			target := ResolveTarget{InfoHash: H}
			for i := 0; i < 6; i++ { // a client polling /play through a download
				_, err := store.Resolve(context.Background(), target)
				var unavailable *StoreUnavailableError
				if !errors.As(err, &unavailable) {
					t.Fatalf("poll %d: want a refusal, got %v", i, err)
				}
			}
			if adds != 1 {
				t.Errorf("added %d times across six polls — the backoff is not holding", adds)
			}
			if _, refused := store.(refusalReporter).RecentRefusal(H); !refused {
				t.Error("the store cannot report the refusal it just recorded")
			}
		})
	}
}

// A refusal is remembered per SERVICE. One store being throttled says nothing about another, and sharing
// a key would let TorBox's backoff silence a Premiumize that is answering perfectly well.
func TestRefusalBackoff_isPerService(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServiceTorBox, "tok", H, &StoreUnavailableError{Service: ServiceTorBox, Reason: "429"})

	if _, refused := backedOff(cache, ServiceTorBox, "tok", H); !refused {
		t.Error("torbox's own refusal was not remembered")
	}
	if _, refused := backedOff(cache, ServicePremiumize, "tok", H); refused {
		t.Error("torbox's refusal silenced premiumize")
	}
}

// A cancelled request is not a refusal. /play runs on the client's context and the client cancels
// aggressively, so recording one served a healthy release 503 for the next minute.
func TestRefusalBackoff_ignoresCancellation(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServiceTorBox, "tok", H, context.Canceled)
	if _, refused := backedOff(cache, ServiceTorBox, "tok", H); refused {
		t.Error("a cancelled request was remembered as a refusal by the store")
	}
	recordRefusal(cache, ServiceTorBox, "tok", H, context.DeadlineExceeded)
	if _, refused := backedOff(cache, ServiceTorBox, "tok", H); refused {
		t.Error("an expired deadline was remembered as a refusal by the store")
	}
}

// probeConfig — a TorBox install, so handleProbe's cache-truth branches are exercised.
func probeConfig() *Config {
	return &Config{Debrid: []DebridAccount{{Service: ServiceTorBox, Token: "tok"}}}
}

// The probe never queues a torrent, and says so honestly when it cannot tell.
//
// Three answers that were each wrong at some point: "ready" from a cache check alone (which reports what
// TorBox has, not what this account has), 404 "not_queued" when the check was simply unreachable, and a
// resolve that queued the very torrent the probe exists to avoid queueing.
func TestHandleProbe_neverQueuesAndSaysWhenItCannotTell(t *testing.T) {
	// A store that resolves happily UNLESS told not to add — exactly like the real ones. A fake that
	// refuses either way cannot tell "the probe asked for a read-only resolve" from "the probe let it
	// queue", which is the whole assertion.
	var sawNoAdd bool
	held := noAddAwareStore{svc: ServiceTorBox, cached: "abc", sawNoAdd: &sawNoAdd}
	h := &handler{deps: Deps{Cache: NewMemoryCache(1 << 20)}}

	rec := httptest.NewRecorder()
	h.handleProbe(rec, context.Background(), probeConfig(), &StorePool{stores: []Store{held}}, "abc",
		ResolveTarget{InfoHash: "abc"})
	if rec.Code == http.StatusOK {
		t.Error("cached-but-not-held answered 200 ready; playing it would start with a download")
	}
	if !sawNoAdd {
		t.Error("the probe resolved WITHOUT NoAdd — it is free to queue the torrent it exists to avoid")
	}

	// The check could not be reached at all → say so, rather than claiming nothing is queued.
	down := fakeStore{svc: ServiceTorBox, checkErr: errCheckFailed}
	rec = httptest.NewRecorder()
	h.handleProbe(rec, context.Background(), probeConfig(), &StorePool{stores: []Store{down}}, "abc",
		ResolveTarget{InfoHash: "abc"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("an unreachable cache check should be 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cache_check_unavailable") {
		t.Errorf("the body must name the reason: %s", rec.Body.String())
	}
}

// One step further out than the case above: the cache check WORKED, and the status read is the part that
// could not find out. 404 "not_queued" there is what makes a client blacklist a release that is
// downloading right now, so an indeterminate read gets "ask again" instead.
//
// Nothing asserted it — deleting the branch left the whole suite green, because the case it exists for
// (checkcached answers while mylist times out) is reachable only through a store that answers the two
// questions differently.
func TestHandleProbe_anIndeterminateStatusIsNotAnAbsence(t *testing.T) {
	h := &handler{deps: Deps{Cache: NewMemoryCache(1 << 20)}}
	// The cache check succeeds and honestly reports "not cached"; the status read cannot tell. Without
	// the status read, this is the ordinary 404 path — which is what the definitive case below asserts.
	uncertain := answeringStore{
		fakeStore: fakeStore{svc: ServiceTorBox, check: map[string]bool{"abc": false},
			resolve: func() (string, error) { return "", &DeadLinkError{"not held"} }},
		answer: statusUnknown,
	}
	rec := httptest.NewRecorder()
	h.handleProbe(rec, context.Background(), probeConfig(), &StorePool{stores: []Store{uncertain}}, "abc",
		ResolveTarget{InfoHash: "abc"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("an indeterminate status read should be 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "status_unavailable") {
		t.Errorf("the body must distinguish this from an unreachable cache check: %s", rec.Body.String())
	}

	// The same fixture answering DEFINITIVELY still 404s, or the branch above would just be a blanket 503
	// and the route would never report an absence at all.
	definitive := uncertain
	definitive.answer = statusNo
	rec = httptest.NewRecorder()
	h.handleProbe(rec, context.Background(), probeConfig(), &StorePool{stores: []Store{definitive}}, "abc",
		ResolveTarget{InfoHash: "abc"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("a store that answered should still be able to say nothing is queued, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// noAddAwareStore behaves like a real store: it can serve the release, but refuses when the caller
// forbids queueing and the account does not already hold it.
type noAddAwareStore struct {
	svc      DebridService
	cached   string
	sawNoAdd *bool
}

func (n noAddAwareStore) Service() DebridService { return n.svc }
func (n noAddAwareStore) CacheCheck(_ context.Context, hashes []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, h := range hashes {
		out[h] = h == n.cached
	}
	return out, nil
}
func (n noAddAwareStore) Resolve(_ context.Context, t ResolveTarget) (string, error) {
	if t.NoAdd {
		*n.sawNoAdd = true
		return "", errWouldAdd // TorBox has it; this ACCOUNT does not
	}
	return "https://cdn/queued-it", nil
}
func (n noAddAwareStore) Status(context.Context, ResolveTarget) (StoreStatus, bool) {
	return StoreStatus{}, false
}

// Scout's own refusals reach the CLIENT as scout's, not as the debrid's.
//
// errScoutSide was wired into the refusal memory and then the route went on reading the wrapped
// StoreUnavailableError and printing its service — so the app told the viewer TorBox was refusing while
// TorBox was answering perfectly well. Both scout-side refusals (the hourly allowance, and an add
// already in flight) go through this path.
func TestHandlePlay_namesScoutNotTheDebridForItsOwnRefusals(t *testing.T) {
	prev := globalAddBudget
	globalAddBudget = newAddBudget(time.Hour, 0) // nothing left this hour
	defer func() { globalAddBudget = prev }()

	cache := NewMemoryCache(1 << 20)
	h := NewHandler(Deps{
		Cache: cache,
		MakeStores: func(*Config) []Store {
			return []Store{&torBoxStore{token: "tok", client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
				return resp(200, `{"data":[]}`), nil
			}}, cache: cache, api: torboxAPI}}
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/"+validBlob+"/play/"+encodePlayToken(PlayTarget{InfoHash: H}), nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a spent allowance is still 'not now': got %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "torbox") || strings.Contains(body, "store_unavailable") {
		t.Errorf("scout's own ceiling was blamed on the debrid: %s", body)
	}
	if !strings.Contains(body, "scout_busy") {
		t.Errorf("the client cannot tell whose refusal this is: %s", body)
	}
}

// A read-only resolve is answered before the ADD backoff, and is never blocked by an add already in
// flight — both describe a state a NoAdd caller cannot have caused.
func TestResolve_noAddIsAnsweredAheadOfTheAddGuards(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	recordRefusal(cache, ServiceTorBox, "tok", H, &StoreUnavailableError{Service: ServiceTorBox, Reason: "429"})
	noteAddAttempt(cache, ServiceTorBox, "tok", H)

	s := &torBoxStore{token: "tok", client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
		return resp(200, `{"data":[]}`), nil
	}}, cache: cache, api: torboxAPI}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, NoAdd: true})
	if !errors.Is(err, errWouldAdd) {
		t.Errorf("a read-only resolve was blocked by an add-path guard: %v", err)
	}
}

// An add scout already sent is answered 202 "coming", not 503 "refused".
//
// The release IS being fetched — by us, moments ago. Answering 503 named the debrid and, on the tvOS
// client, stopped it trying other sources: the viewer was told their debrid was refusing, for a release
// scout had itself just queued.
func TestHandlePlay_anInFlightAddIsQueuedNotRefused(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	noteAddAttempt(cache, ServiceTorBox, "tok", H) // an add went out and was abandoned

	h := NewHandler(Deps{
		Cache: cache,
		MakeStores: func(*Config) []Store {
			return []Store{&torBoxStore{token: "tok", cache: cache, api: torboxAPI,
				client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
					return resp(200, `{"data":[]}`), nil // nothing queued that Status can see yet
				}}}}
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/"+validBlob+"/play/"+encodePlayToken(PlayTarget{InfoHash: H}), nil))

	if rec.Code != http.StatusAccepted {
		t.Errorf("an add already in flight is 202 coming, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "store_unavailable") {
		t.Errorf("a release scout itself queued was reported as the debrid refusing: %s", rec.Body.String())
	}
}

// A spent clock must not be remembered as the STORE refusing.
//
// The guard that stops an add being charged on a dead context returns a StoreUnavailableError, and
// recordRefusal files anything that is not a cancellation, not errScoutSide, not errAddInFlight and not
// errTorrentGone. So the refusal memory learned that TorBox had declined a release TorBox was never
// asked about: one cancelled poll — a viewer changing focus is enough — made a healthy release answer
// 503 on BOTH routes for the next sixty seconds, on a fresh clock, with the store skipped as "backing
// off". The clock is scout's, so the error says so, and errScoutSide is what keeps it out of the memory.
//
// All three stores, because only TorBox's guard had a test and the other two were free to drift.
func TestResolve_aSpentClockIsNotRememberedAsTheStoreRefusing(t *testing.T) {
	for _, svc := range []DebridService{ServiceTorBox, ServiceRealDebrid, ServicePremiumize} {
		t.Run(string(svc), func(t *testing.T) {
			token, hash := "clock-"+string(svc), repeat("7", 40)
			cache := NewMemoryCache(1 << 20)
			quiet := mockDoer{fn: func(*http.Request) (*http.Response, error) {
				return resp(200, `{"data":{}}`), nil
			}}
			var store Store
			switch svc {
			case ServiceRealDebrid:
				store = &realDebridStore{token: token, cache: cache, api: realDebridAPI, client: quiet}
			case ServicePremiumize:
				store = &premiumizeStore{token: token, cache: cache, api: premiumizeAPI, client: quiet}
			default:
				store = &torBoxStore{token: token, cache: cache, api: torboxAPI, client: quiet}
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := store.Resolve(ctx, ResolveTarget{InfoHash: hash})
			if err == nil {
				t.Fatal("a resolve on a spent clock should not succeed")
			}
			if !errors.Is(err, errScoutSide) {
				t.Errorf("a spent clock reported %v — not marked as scout's own, so recordRefusal files "+
					"it and the store is blamed for a request it never saw", err)
			}
			if reason, backed := backedOff(cache, svc, token, hash); backed {
				t.Errorf("a spent clock left a refusal against %s: %q — the next poll skips a healthy "+
					"store for a minute, on both /play and ?probe=1", svc, reason)
			}
		})
	}
}

// The probe stops believing an add at the same moment /play does.
//
// addInFlight, which /play's resolve consults, checks unknownTooLong FIRST and calls the release dead
// past addGiveUp. The pool's reporter read the attempt marker alone, and the two markers have different
// lifetimes — the attempt marker 90s, the unknown-outcome stamp 30 minutes — so they overlap: the last
// add before the give-up leaves an attempt marker live for up to ninety seconds past it. In that window
// ?probe=1 said 202 downloading while /play said 404 dead_link, which is the disagreement the reporter
// was added to remove, pointing the other way.
func TestAddInFlight_stopsBelievingAnAddAtTheGiveUp(t *testing.T) {
	token, hash := "giveup", repeat("8", 40)
	cache := NewMemoryCache(1 << 20)
	s := &realDebridStore{token: token, cache: cache, api: realDebridAPI}

	noteAddAttempt(cache, ServiceRealDebrid, token, hash)
	if !s.AddInFlight(hash) {
		t.Fatal("a fresh add attempt should read as in flight")
	}

	// Age the unknown-outcome stamp past the give-up, leaving the 90s attempt marker live beneath it.
	cache.Put(unknownOutcomeKey(ServiceRealDebrid, token, hash),
		strconv.FormatInt(time.Now().Add(-addGiveUp-time.Minute).Unix(), 10), unknownOutcomeTTL)

	if s.AddInFlight(hash) {
		t.Error("past the give-up the probe still called the add live, while /play calls the release " +
			"dead — the client is told to keep waiting for something /play has stopped waiting for")
	}
}

// The pool asks EVERY store, not just the first.
//
// Every fixture for this reporter built a single-store pool, so the loop was never exercised: a version
// that answered false for any pool with more than one store passed the whole suite, and would have made
// the probe blind to in-flight adds on exactly the multi-account installs it matters for.
func TestStorePoolAddInFlight_asksEveryStore(t *testing.T) {
	token, hash := "pool-loop", repeat("6", 40)
	cache := NewMemoryCache(1 << 20)
	noteAddAttempt(cache, ServicePremiumize, token, hash) // the marker is on the LAST store

	pool := &StorePool{stores: []Store{
		&torBoxStore{token: token, cache: cache, api: torboxAPI},
		&realDebridStore{token: token, cache: cache, api: realDebridAPI},
		&premiumizeStore{token: token, cache: cache, api: premiumizeAPI},
	}}
	if !pool.AddInFlight(hash) {
		t.Error("the pool missed an add in flight on a store that was not the first one asked")
	}
}

// A release Real-Debrid already holds is READY on the probe route, not "not queued".
//
// The probe's readiness branch was gated on the cache check alone, and RD publishes no cache API — it
// answers all-false by design, saying "RD contributes no cache truth". So the branch was unreachable on
// an RD-only install and the route fell through to 404, while /play resolved the very same release from
// a cache lookup plus an info call and returned a 302. The client polls the probe to know when its
// download has landed; it never learned that it had.
func TestProbe_aTorrentRealDebridAlreadyHoldsReadsAsReady(t *testing.T) {
	token, hash := "rd-held", repeat("f", 40)
	cache := NewMemoryCache(1 << 20)
	target := ResolveTarget{InfoHash: hash}
	cache.Put(rdTorrentKey(token, hash, target), "RDID123", time.Hour) // already bought

	client := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/torrents/info/"):
			return resp(200, `{"id":"RDID123","status":"downloaded",`+
				`"files":[{"id":1,"path":"/Movie.mkv","bytes":100,"selected":1}],`+
				`"links":["https://rd.example/link"]}`), nil
		case strings.Contains(r.URL.Path, "/unrestrict/link"):
			return resp(200, `{"download":"https://rd.example/final.mkv"}`), nil
		}
		return resp(200, `{}`), nil
	}}

	h := NewHandler(Deps{
		Cache: cache,
		MakeStores: func(*Config) []Store {
			return []Store{&realDebridStore{token: token, cache: cache, api: realDebridAPI, client: client}}
		},
	})
	tok := encodePlayToken(PlayTarget{InfoHash: hash})

	// The config must name the same service the pool holds, or the route stops at "cache check
	// unavailable" — hasCacheTruth reads the CONFIG — before reaching the branch under test.
	rdOnly := blob(`{"debrid":[{"service":"realdebrid","token":"rd"}],"indexers":["torrentio"],"resultCap":20}`)
	probe := httptest.NewRecorder()
	h.ServeHTTP(probe, httptest.NewRequest("GET", "/"+rdOnly+"/play/"+tok+"?probe=1", nil))
	if probe.Code == http.StatusNotFound {
		t.Errorf("?probe=1 answered 404 for a torrent the account already holds: %s — /play resolves it "+
			"from the same cache entry, so the client is told a release it can play does not exist",
			strings.TrimSpace(probe.Body.String()))
	}
	if probe.Code != http.StatusOK {
		t.Errorf("?probe=1 = %d, want 200 ready: %s", probe.Code, strings.TrimSpace(probe.Body.String()))
	}
}

// NOTE ON THIS TEST'S SCOPE: it pins premiumizeStore.AddInFlight at three marker ages and nothing else.
// It does NOT observe the agreement between the two routes, despite what an earlier version of this
// comment claimed — and that overclaim is why two real disagreements passed it: a queued transfer that
// directdl had since REFUSED, and one that had since COMPLETED. Its fixture answers
// success-with-no-content for every call, which is the one case where the marker alone is right. The
// route-level agreements are pinned by the two tests below it.
//
// Premiumize has a second way to be mid-fetch, and the probe could not see it either.
//
// directdl answering success with empty content means the transfer was queued: the store stamps
// pmQueuedKey and returns errAddInFlight, so /play says 202 downloading. That is not the add marker —
// settleAddAttempt clears the add marker as soon as the body is read — so a probe asking only about
// adds answered 404 for the whole ten minutes /play spends saying "coming".
//
// Both ends of the window are asserted. Past pendingGiveUp /play reports the release dead, so a probe
// still claiming 202 there would be the same defect pointing the other way.
func TestPremiumizeAddInFlight_believesAQueuedTransferOnlyUntilTheGiveUp(t *testing.T) {
	for _, tc := range []struct {
		name       string
		queuedAgo  time.Duration
		wantQueued bool
	}{
		{"just queued", 0, true},
		{"still believable", 9 * time.Minute, true},
		{"past the give-up", pendingGiveUp + time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, hash := "pm-"+tc.name, repeat("9", 40)
			cache := NewMemoryCache(1 << 20)
			// Stamp the marker at the age under test, the way noteQueued writes it.
			cache.Put(pmQueuedKey(token, hash),
				strconv.FormatInt(time.Now().Add(-tc.queuedAgo).Unix(), 10), queuedTTL)

			s := &premiumizeStore{token: token, cache: cache, api: premiumizeAPI,
				client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
					return resp(200, `{"status":"success","content":[]}`), nil // still nothing to serve
				}}}
			if got := s.AddInFlight(hash); got != tc.wantQueued {
				t.Errorf("AddInFlight = %v, want %v — the probe route reads this to decide between "+
					"202 downloading and 404 not_queued, and it must change sides at the same moment "+
					"/play does", got, tc.wantQueued)
			}
		})
	}
}

// HoldingServices asks EVERY store, and builds Real-Debrid's key the way /play writes it.
//
// Two gaps in one: the loop had no multi-store fixture, so stopping after the first store passed the
// whole suite; and the only test used a movie target, so dropping season/episode from the RD key — which
// would make the fix silently do nothing for series, most of the traffic — also passed.
func TestHoldingServices_asksEveryStoreAndKeysEpisodesCorrectly(t *testing.T) {
	token, hash := "holding", repeat("3", 40)
	cache := NewMemoryCache(1 << 20)
	season, episode := 2, 5
	target := ResolveTarget{InfoHash: hash, Season: &season, Episode: &episode}
	// Written the way /play writes it, episode selector and all, on the store asked LAST.
	cache.Put(rdTorrentKey(token, hash, target), "RDID-S2E5", time.Hour)

	pool := &StorePool{stores: []Store{
		&torBoxStore{token: token, cache: cache, api: torboxAPI},
		&realDebridStore{token: token, cache: cache, api: realDebridAPI},
	}}

	held := pool.HoldingServices(target)
	if len(held) != 1 || held[0] != ServiceRealDebrid {
		t.Errorf("HoldingServices = %v, want [realdebrid] — either the loop stopped at the first store, "+
			"or the episode selector is missing from the key, which makes the whole enquiry a no-op for "+
			"series", held)
	}
	// A different episode of the same pack is a different entry, so it must NOT match.
	other := 6
	if got := pool.HoldingServices(ResolveTarget{InfoHash: hash, Season: &season, Episode: &other}); len(got) != 0 {
		t.Errorf("episode 6 matched episode 5's entry: %v", got)
	}
	// And a different SEASON, same episode number. Without this the key could discriminate on episode
	// alone — measured: dropping the season from rdTorrentKey's selector passed the whole suite, because
	// every probe here used season 2.
	otherSeason := 3
	if got := pool.HoldingServices(ResolveTarget{InfoHash: hash, Season: &otherSeason, Episode: &episode}); len(got) != 0 {
		t.Errorf("S03E05 matched S02E05's entry: %v — the key is not discriminating on season", got)
	}
}

// EveryAddRefusedByScout means EVERY, and an empty pool refuses nothing.
//
// The quantifier was held by nothing: flipping it to ANY passed the entire suite, and shipped it would
// answer 503 scout_busy on a two-account install where one budget is spent and the other is healthy —
// while /play serves a 302 from the healthy account. That is a fresh disagreement of exactly the kind
// the branch was added to remove.
func TestEveryAddRefusedByScout_needsEveryAccountSpent(t *testing.T) {
	spent, healthy := "spent-"+t.Name(), "healthy-"+t.Name()
	for i := 0; i < addBudgetLimit; i++ {
		globalAddBudget.take(budgetAccount(ServiceTorBox, spent))
	}
	cache := NewMemoryCache(1 << 20)

	mixed := &StorePool{stores: []Store{
		&torBoxStore{token: spent, cache: cache, api: torboxAPI},
		&realDebridStore{token: healthy, cache: cache, api: realDebridAPI},
	}}
	if mixed.EveryAddRefusedByScout() {
		t.Error("one spent account out of two reported as every add refused — the probe would answer " +
			"503 scout_busy for a release the healthy account can still fetch")
	}

	allSpent := &StorePool{stores: []Store{&torBoxStore{token: spent, cache: cache, api: torboxAPI}}}
	if !allSpent.EveryAddRefusedByScout() {
		t.Error("the only account's allowance is spent and it was not reported")
	}

	if (&StorePool{}).EveryAddRefusedByScout() {
		t.Error("an empty pool refused every add — vacuously true is not true here")
	}
}

// A refusal on record outranks the queue marker, and NOT the other way round.
//
// premiumizeStore.AddInFlight checks the add marker first, then the refusal, then the queue marker.
// Hoisting the refusal check above the add marker passed the whole suite, and it inverts the precedence
// addStillBelievable's own comment calls the point: an add genuinely in flight would read as
// not-in-flight whenever a stale per-release refusal happened to exist.
func TestPremiumizeAddInFlight_anAddInFlightOutranksAStaleRefusal(t *testing.T) {
	token, hash := "pm-order", repeat("c", 40)
	cache := NewMemoryCache(1 << 20)
	noteAddAttempt(cache, ServicePremiumize, token, hash) // an add of ours IS out
	recordRefusal(cache, ServicePremiumize, token, hash,  // ...alongside an older per-release refusal
		&DeadLinkError{"premiumize directdl: error something earlier"})

	s := &premiumizeStore{token: token, cache: cache, api: premiumizeAPI}
	if !s.AddInFlight(hash) {
		t.Error("an add scout has out read as not-in-flight because a stale refusal exists — /play " +
			"answers 202 from that marker, so the probe must too")
	}
}

// A queued Premiumize transfer that directdl has SINCE refused must stop reading as "downloading".
//
// The test above pins the marker's boolean and drives only one route, which is how this escaped: it
// answers success-with-no-content for every call, and the disagreeing case is the one where directdl
// answers something else. Premiumize reports an unsupported magnet, an account at its limit and "not
// enough space" as HTTP 200 with status:error — none of those branches clears pmQueuedKey, so the
// marker alone kept the probe at 202 for the whole ten-minute window while /play had the verdict.
//
// Both routes are driven here, and the assertion is that they agree.
func TestProbeAndPlay_agreeWhenAQueuedPremiumizeTransferIsRefused(t *testing.T) {
	token, hash := "pm-refused", repeat("5", 40)
	cache := NewMemoryCache(1 << 20)
	cache.Put(pmQueuedKey(token, hash), strconv.FormatInt(time.Now().Unix(), 10), queuedTTL)

	// The shape an account out of space returns: HTTP 200, status error.
	client := mockDoer{fn: func(*http.Request) (*http.Response, error) {
		return resp(200, `{"status":"error","message":"not enough space"}`), nil
	}}
	h := NewHandler(Deps{
		Cache: cache,
		MakeStores: func(*Config) []Store {
			return []Store{&premiumizeStore{token: token, cache: cache, api: premiumizeAPI, client: client}}
		},
	})
	pmBlob := blob(`{"debrid":[{"service":"premiumize","token":"` + token + `"}],` +
		`"indexers":["torrentio"],"resultCap":20}`)
	tok := encodePlayToken(PlayTarget{InfoHash: hash})

	// The PROBE goes first, which is the order a real client uses — it polls to draw its progress bar and
	// only calls /play when told the release is ready. An earlier version issued a /play first, which
	// recorded the refusal on the probe's behalf and so hid whether the probe could learn it alone.
	probeFirst := httptest.NewRecorder()
	h.ServeHTTP(probeFirst, httptest.NewRequest("GET", "/"+pmBlob+"/play/"+tok+"?probe=1", nil))
	if probeFirst.Code == http.StatusAccepted {
		t.Errorf("the first ?probe=1 says downloading (%s) for a transfer Premiumize refused on that "+
			"very call — nothing withdraws the queue marker's claim, so the client sits on a spinner it "+
			"cannot fall through", strings.TrimSpace(probeFirst.Body.String()))
	}

	probe := httptest.NewRecorder()
	h.ServeHTTP(probe, httptest.NewRequest("GET", "/"+pmBlob+"/play/"+tok+"?probe=1", nil))
	play := httptest.NewRecorder()
	h.ServeHTTP(play, httptest.NewRequest("GET", "/"+pmBlob+"/play/"+tok, nil))

	if probe.Code == http.StatusAccepted && play.Code != http.StatusAccepted {
		t.Errorf("?probe=1 still says 202 downloading (%s) while /play says %d (%s) — the client sits "+
			"on a 0%% spinner for ten minutes with nothing behind it",
			strings.TrimSpace(probe.Body.String()), play.Code, strings.TrimSpace(play.Body.String()))
	}
	if probe.Code != play.Code {
		t.Errorf("?probe=1 = %d (%s), /play = %d (%s) — one vocabulary, two answers",
			probe.Code, strings.TrimSpace(probe.Body.String()),
			play.Code, strings.TrimSpace(play.Body.String()))
	}
}

// A Premiumize transfer that has COMPLETED must read as ready, not as still downloading.
//
// Nothing on the probe path could clear pmQueuedKey: settleQueuedTransfer is reached only from
// directdl's content branch, and Premiumize refused a NoAdd target before consulting anything, so the
// probe never called directdl and the marker went on saying "coming". The probe therefore never
// transitioned to ready for Premiumize at all — 202 until the marker aged past pendingGiveUp, then 404,
// while /play answered 302 with a playable link. Measured before the fix: probe 202, /play 302.
//
// For an already-queued transfer directdl is a READ — the charge is suppressed in exactly that case —
// so letting a read-only caller through buys nothing and is the only way to discover completion.
// Every realistic outcome of Premiumize's /cache/check, because the readiness enquiry runs only when a
// store NAMES itself a holder, and PM's cache check was for a while the only channel that could. The
// first version of this test asserted the cached-true row alone — the one branch that worked — so the
// other four kept answering 202 for a completed transfer and the suite stayed green.
func TestProbeAndPlay_agreeWhenAPremiumizeTransferCompletes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cacheCheck func() (*http.Response, error)
	}{
		{"cache check says cached", func() (*http.Response, error) {
			return resp(200, `{"status":"success","response":[true]}`), nil
		}},
		{"cache check says not cached", func() (*http.Response, error) {
			return resp(200, `{"status":"success","response":[false]}`), nil
		}},
		{"cache check throttled", func() (*http.Response, error) { return resp(429, `{}`), nil }},
		{"cache check transport error", func() (*http.Response, error) {
			return nil, context.DeadlineExceeded
		}},
		{"cache check unreadable", func() (*http.Response, error) { return resp(200, `not json`), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, hash := "pm-complete-"+tc.name, repeat("2", 40)
			cache := NewMemoryCache(1 << 20)
			cache.Put(pmQueuedKey(token, hash), strconv.FormatInt(time.Now().Unix(), 10), queuedTTL)

			client := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "cache/check") {
					return tc.cacheCheck()
				}
				return resp(200, `{"status":"success","content":[`+
					`{"path":"Movie.mkv","link":"https://pm.example/final.mkv","size":100}]}`), nil
			}}
			h := NewHandler(Deps{
				Cache: cache,
				MakeStores: func(*Config) []Store {
					return []Store{&premiumizeStore{token: token, cache: cache, api: premiumizeAPI,
						client: client}}
				},
			})
			pmBlob := blob(`{"debrid":[{"service":"premiumize","token":"` + token + `"}],` +
				`"indexers":["torrentio"],"resultCap":20}`)
			tok := encodePlayToken(PlayTarget{InfoHash: hash})

			// THREE polls, because a client polls. One poll cannot see a route that destroys the state
			// its own next answer depends on: the readiness enquiry used to clear the queue marker, so
			// the probe reported ready once and 404 "not queued" forever after, while /play kept serving
			// the same transfer.
			for i := 1; i <= 3; i++ {
				probe := httptest.NewRecorder()
				h.ServeHTTP(probe, httptest.NewRequest("GET", "/"+pmBlob+"/play/"+tok+"?probe=1", nil))
				if probe.Code == http.StatusAccepted {
					t.Errorf("poll %d: ?probe=1 says downloading (%s) for a completed transfer",
						i, strings.TrimSpace(probe.Body.String()))
				}
				if probe.Code != http.StatusOK {
					t.Errorf("poll %d: ?probe=1 = %d (%s), want 200 ready",
						i, probe.Code, strings.TrimSpace(probe.Body.String()))
				}
			}
			play := httptest.NewRecorder()
			h.ServeHTTP(play, httptest.NewRequest("GET", "/"+pmBlob+"/play/"+tok, nil))
			if play.Code != http.StatusFound {
				t.Fatalf("/play = %d, want 302 — the fixture is not serving a completed transfer",
					play.Code)
			}
		})
	}
}

// A read-only probe poll must not write Premiumize's ADD-path in-flight marker.
//
// The NoAdd gate was widened so the probe could see a queued transfer finish, and everything below that
// gate is add-path bookkeeping. A probe runs on the client's context under statusBudget and clients
// cancel aggressively, so a cut-off poll left a ninety-second marker behind — and /play then
// short-circuits on that marker WITHOUT calling directdl, answering 202 "downloading" for a release
// Premiumize already holds. Unplayable for the marker's life, with no Status API to clear it.
//
// A read-only route stranding a release by writing add-path memory is exactly the contract
// recordRefusalFor states, and the commit that widened this gate fixed that contract for Real-Debrid
// while missing the store it had just widened.
func TestProbe_aReadOnlyPollWritesNoPremiumizeAddMemory(t *testing.T) {
	token, hash := "pm-readonly", repeat("1", 40)
	cache := NewMemoryCache(1 << 20)
	cache.Put(pmQueuedKey(token, hash), strconv.FormatInt(time.Now().Unix(), 10), queuedTTL)

	s := &premiumizeStore{token: token, cache: cache, api: premiumizeAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return nil, context.Canceled // the poll is cut off mid-call, as a focus change does
		}}}

	_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: hash, NoAdd: true})

	if addOutcomeUnknown(cache, ServicePremiumize, token, hash) {
		t.Error("a read-only poll left an add-path in-flight marker — /play then answers 202 " +
			"'downloading' without calling directdl, for a release Premiumize already holds")
	}
	// The queue marker must survive a read-only poll: it is the only thing naming Premiumize a holder,
	// so clearing it from here makes the very next probe answer 404 for a transfer /play still serves.
	if !alreadyQueued(cache, token, hash) {
		t.Error("a read-only poll cleared the queue marker — the next poll names no holder and answers " +
			"404 not_queued for a release /play resolves from the same marker")
	}
}

// A refusal Premiumize ANSWERS must be recorded even by a read-only poll, because for this store that
// key is what withdraws the queue marker's "coming" claim.
//
// This half was previously asserted with a fixture whose error was context.Canceled — which recordRefusal
// excludes by itself, so the assertion could not fail for the reason it named, and reverting the whole
// change passed the entire suite. The errors below are the ones Premiumize actually returns: "not enough
// space" and an invalid magnet arrive as HTTP 200 with status:error.
func TestPremiumize_aReadOnlyPollRecordsAnAnsweredRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"out of space", `{"status":"error","message":"not enough space"}`, 200},
		{"server error", `{}`, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, hash := "pm-answered-"+tc.name, repeat("1", 40)
			cache := NewMemoryCache(1 << 20)
			cache.Put(pmQueuedKey(token, hash), strconv.FormatInt(time.Now().Unix(), 10), queuedTTL)

			s := &premiumizeStore{token: token, cache: cache, api: premiumizeAPI,
				client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
					return resp(tc.code, tc.body), nil
				}}}

			_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: hash, NoAdd: true})

			if _, backed := backedOff(cache, ServicePremiumize, token, hash); !backed {
				t.Error("an answered refusal was not recorded by a read-only poll — nothing then " +
					"withdraws the queue marker's claim, so the probe reports 202 downloading on every " +
					"poll of a transfer Premiumize has refused, re-asking directdl each time")
			}
			if s.AddInFlight(hash) {
				t.Error("the queue marker still reads as in-flight after Premiumize refused the " +
					"transfer — the client sits on a spinner it cannot fall through")
			}
		})
	}
}

// A rejected KEY outranks an add in flight, on the probe route as in every store.
//
// All three stores check accountBackedOff above addInFlight and say why — a dead key is not a wait. The
// probe consulted its in-flight reporter first, so a live marker pre-empted the rejected key and the two
// routes disagreed: /play 503 naming the debrid, ?probe=1 202 downloading, for the same release at the
// same instant, for up to ninety seconds per outstanding add.
func TestProbeAndPlay_agreeWhenTheAccountKeyIsRejected(t *testing.T) {
	token, hash := "dead-key", repeat("0", 40)
	cache := NewMemoryCache(1 << 20)
	noteAddAttempt(cache, ServiceTorBox, token, hash)                    // an add of ours is out
	recordRefusal(cache, ServiceTorBox, token, repeat("f", 40),          // ...and the key is rejected
		&StoreUnavailableError{Service: ServiceTorBox, Status: http.StatusUnauthorized,
			Reason: "createtorrent http 401"})

	h := NewHandler(Deps{
		Cache: cache,
		MakeStores: func(*Config) []Store {
			return []Store{&torBoxStore{token: token, cache: cache, api: torboxAPI,
				client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
					return resp(200, `{"success":true,"data":[]}`), nil
				}}}}
		},
	})
	tbBlob := blob(`{"debrid":[{"service":"torbox","token":"` + token + `"}],` +
		`"indexers":["torrentio"],"resultCap":20}`)
	tok := encodePlayToken(PlayTarget{InfoHash: hash})

	probe := httptest.NewRecorder()
	h.ServeHTTP(probe, httptest.NewRequest("GET", "/"+tbBlob+"/play/"+tok+"?probe=1", nil))
	play := httptest.NewRecorder()
	h.ServeHTTP(play, httptest.NewRequest("GET", "/"+tbBlob+"/play/"+tok, nil))

	if probe.Code == http.StatusAccepted && play.Code != http.StatusAccepted {
		t.Errorf("?probe=1 says downloading (%s) while /play says %d (%s) — the viewer watches a spinner "+
			"for a release nothing is fetching, on an account whose key needs replacing",
			strings.TrimSpace(probe.Body.String()), play.Code, strings.TrimSpace(play.Body.String()))
	}
	if probe.Code != play.Code {
		t.Errorf("?probe=1 = %d (%s), /play = %d (%s) — one vocabulary, two answers",
			probe.Code, strings.TrimSpace(probe.Body.String()),
			play.Code, strings.TrimSpace(play.Body.String()))
	}
}

// A dead key on ONE account must not outrank a live add on ANOTHER.
//
// Two rules meet here. Per store, a rejected key beats that store's own add in flight — every Resolve
// says so. Across stores, an add in flight anywhere beats a refusal elsewhere — ResolvePreferring
// returns `coming` before `refused` precisely so store order cannot decide the verdict. Applying the
// first rule at POOL level broke the second: with TorBox's key expired and Real-Debrid fetching, the
// probe answered 503 naming torbox while /play answered 202 downloading — naming a store that was not
// the one fetching, on the URL the client polls for its progress bar.
//
// The sibling single-account case is pinned by TestProbeAndPlay_agreeWhenTheAccountKeyIsRejected, which
// configures one store and so cannot see this. Both are needed.
func TestProbeAndPlay_agreeWhenOneAccountsKeyIsDeadAndAnotherIsFetching(t *testing.T) {
	token, hash := "two-accounts", repeat("b", 40)
	cache := NewMemoryCache(1 << 20)
	// TorBox's key is rejected...
	cache.Put(accountRefusedKey(ServiceTorBox, token), "createtorrent http 401", refusalBackoff)
	// ...while Real-Debrid has an add out for the release being watched.
	noteAddAttempt(cache, ServiceRealDebrid, token, hash)

	quiet := mockDoer{fn: func(*http.Request) (*http.Response, error) {
		return resp(200, `{"success":true,"data":[]}`), nil
	}}
	h := NewHandler(Deps{
		Cache: cache,
		MakeStores: func(*Config) []Store {
			return []Store{
				&torBoxStore{token: token, cache: cache, api: torboxAPI, client: quiet},
				&realDebridStore{token: token, cache: cache, api: realDebridAPI, client: quiet},
			}
		},
	})
	twoBlob := blob(`{"debrid":[{"service":"torbox","token":"` + token + `"},` +
		`{"service":"realdebrid","token":"` + token + `"}],"indexers":["torrentio"],"resultCap":20}`)
	tok := encodePlayToken(PlayTarget{InfoHash: hash})

	probe := httptest.NewRecorder()
	h.ServeHTTP(probe, httptest.NewRequest("GET", "/"+twoBlob+"/play/"+tok+"?probe=1", nil))
	play := httptest.NewRecorder()
	h.ServeHTTP(play, httptest.NewRequest("GET", "/"+twoBlob+"/play/"+tok, nil))

	if probe.Code != play.Code {
		t.Errorf("?probe=1 = %d (%s), /play = %d (%s) — a dead key on one account is outranking a live "+
			"add on another, so the probe names a store that is not the one fetching",
			probe.Code, strings.TrimSpace(probe.Body.String()),
			play.Code, strings.TrimSpace(play.Body.String()))
	}
	if probe.Code != http.StatusAccepted {
		t.Errorf("?probe=1 = %d, want 202 — Real-Debrid is fetching it", probe.Code)
	}
}

// TorBox must rank its own add in flight above a refusal recorded in the meantime, as RD and PM do.
//
// The per-release backoff was hoisted above the warm fast path so a binge could not bypass it, and that
// hoist put it above the in-flight marker for TorBox alone — reversing the order the gate further down
// states in as many words. Both markers can be live at once without planting anything: /play has no
// singleflight and Stremio races requests, so one poll's createtorrent can be answered 429 (settling its
// attempt and recording the refusal) while another's connection resets, leaving its attempt marker.
func TestTorBox_anAddInFlightOutranksARefusalRecordedMeanwhile(t *testing.T) {
	token, hash := "tb-order", repeat("a", 40)
	cache := NewMemoryCache(1 << 20)
	noteAddAttempt(cache, ServiceTorBox, token, hash)
	cache.Put(refusedKey(ServiceTorBox, token, hash), "createtorrent http 429", refusalBackoff)

	s := &torBoxStore{token: token, cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) {
			return resp(200, `{"success":true,"data":[]}`), nil
		}}}

	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: hash})
	if !errors.Is(err, errAddInFlight) {
		t.Errorf("torbox answered %v, want errAddInFlight — it is blaming the store for a release scout "+
			"has an add out for, which is the one answer this file says never to give", err)
	}
}

// A refusal SCOUT made must not read as "nothing is queued".
//
// recordRefusal excludes errScoutSide on purpose — scout's own ceiling is not the debrid declining, and
// filing it as one blames a healthy service — so the backoff memory the probe reads stays empty and the
// route fell through all three of its "could not ask" guards to 404. The budget is per account and
// rolling, so once the ceiling is reached every uncached release on that account answers that way for
// the rest of the hour, and 404 is the answer that makes a client blacklist a release.
func TestProbeAndPlay_agreeWhenScoutsOwnBudgetIsSpent(t *testing.T) {
	token, hash := "budget-"+t.Name(), repeat("4", 40)
	// Spend the account's allowance the way a busy hour does.
	for i := 0; i < addBudgetLimit; i++ {
		globalAddBudget.take(budgetAccount(ServiceTorBox, token))
	}

	cache := NewMemoryCache(1 << 20)
	h := NewHandler(Deps{
		Cache: cache,
		MakeStores: func(*Config) []Store {
			// The upstream must answer DEFINITIVELY, or the probe stops at "a store could not answer"
			// and both routes return 503 for unrelated reasons — which is a green test that proves
			// nothing. checkcached needs a decodable object; the account listing needs a valid empty
			// envelope so Status says "not downloading" rather than "could not find out".
			return []Store{&torBoxStore{token: token, cache: cache, api: torboxAPI,
				client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "checkcached") {
						return resp(200, `{"data":{}}`), nil
					}
					return resp(200, `{"success":true,"data":[]}`), nil
				}}}}
		},
	})
	tbBlob := blob(`{"debrid":[{"service":"torbox","token":"` + token + `"}],` +
		`"indexers":["torrentio"],"resultCap":20}`)
	tok := encodePlayToken(PlayTarget{InfoHash: hash})

	probe := httptest.NewRecorder()
	h.ServeHTTP(probe, httptest.NewRequest("GET", "/"+tbBlob+"/play/"+tok+"?probe=1", nil))
	play := httptest.NewRecorder()
	h.ServeHTTP(play, httptest.NewRequest("GET", "/"+tbBlob+"/play/"+tok, nil))

	if probe.Code == http.StatusNotFound {
		t.Errorf("?probe=1 answered 404 not_queued while scout's own allowance is spent (/play = %d %s) "+
			"— scout could not ask, which is not the same as nobody having it",
			play.Code, strings.TrimSpace(play.Body.String()))
	}
	if probe.Code != play.Code {
		t.Errorf("?probe=1 = %d (%s), /play = %d (%s) — one vocabulary, two answers",
			probe.Code, strings.TrimSpace(probe.Body.String()),
			play.Code, strings.TrimSpace(play.Body.String()))
	}
}

// ?probe=1 and /play must give the SAME answer about an add scout has out.
//
// They are one vocabulary by design — the doc on handleProbe says so — and this is the one fact in the
// package that is about us rather than about a service. /play reads it through errAddInFlight, raised
// from inside the resolve. The probe route never resolves, and all three stores answer a NoAdd target
// with errWouldAdd BEFORE they consult the marker, so it had no way to learn the state at all and fell
// through to 404 "not_queued" — for the same release, at the same instant, that /play was answering 202
// "downloading".
//
// A 404 there is the single failure that route exists to prevent: it is the URL the client polls to
// draw its progress bar, and it reads a 404 as a release nobody has. The window is not brief for two of
// the three services — Real-Debrid and Premiumize have no Status API to rediscover the torrent with, so
// nothing clears the marker before addAttemptTTL expires 90 seconds later.
//
// Asserted as an agreement between the two routes rather than as a status code, because the defect was
// the DISAGREEMENT; pinning one route's number invites the other to drift again.
//
// This fixture reaches the disagreement one branch earlier than the reported 404: with every upstream
// answering an empty body the probe stops at 503 (`status_unavailable` for TorBox, which has a Status
// API, `cache_check_unavailable` for the two that do not) while /play answers 202. Removing the guard
// reproduces that, which is the same defect at a different exit — the route not knowing a fact it
// holds. Reaching the literal 404 needs a healthy cache check AND a store that answers "not queued",
// which is a fuller fixture than this invariant needs.
func TestProbeAndPlay_agreeThatAnInFlightAddIsDownloading(t *testing.T) {
	for _, svc := range []DebridService{ServiceTorBox, ServiceRealDebrid, ServicePremiumize} {
		t.Run(string(svc), func(t *testing.T) {
			cache := NewMemoryCache(1 << 20)
			noteAddAttempt(cache, svc, "tok", H) // an add went out and nothing came back

			// Every upstream answers a well-formed "nothing here", so the marker is the only evidence
			// either route has. `{"data":{}}` rather than `{"data":[]}`: the cache check decodes data as
			// an object, so an array fails the decode and the probe stops at "cache check unavailable"
			// before it ever reaches the branch under test.
			quiet := mockDoer{fn: func(*http.Request) (*http.Response, error) {
				return resp(200, `{"data":{}}`), nil
			}}
			h := NewHandler(Deps{
				Cache: cache,
				MakeStores: func(*Config) []Store {
					switch svc {
					case ServiceRealDebrid:
						return []Store{&realDebridStore{token: "tok", cache: cache, api: realDebridAPI, client: quiet}}
					case ServicePremiumize:
						return []Store{&premiumizeStore{token: "tok", cache: cache, api: premiumizeAPI, client: quiet}}
					}
					return []Store{&torBoxStore{token: "tok", cache: cache, api: torboxAPI, client: quiet}}
				},
			})
			tok := encodePlayToken(PlayTarget{InfoHash: H})

			probe := httptest.NewRecorder()
			h.ServeHTTP(probe, httptest.NewRequest("GET", "/"+validBlob+"/play/"+tok+"?probe=1", nil))
			play := httptest.NewRecorder()
			h.ServeHTTP(play, httptest.NewRequest("GET", "/"+validBlob+"/play/"+tok, nil))

			// The agreement is the invariant. Comparing the two routes rather than asserting one number
			// keeps this pinned to the defect — a disagreement — instead of to whichever status a
			// fixture happens to produce.
			if probe.Code != play.Code {
				t.Errorf("?probe=1 answered %d (%s) while /play answered %d (%s) for the same release at "+
					"the same instant — the two routes are one vocabulary, and the client polls the probe "+
					"to draw its progress bar", probe.Code, strings.TrimSpace(probe.Body.String()),
					play.Code, strings.TrimSpace(play.Body.String()))
			}
			if probe.Code == http.StatusNotFound {
				t.Errorf("?probe=1 answered 404 for a release scout has an add out for: %s — the client "+
					"reads that as a release nobody has and blacklists it", probe.Body.String())
			}
			if probe.Code != http.StatusAccepted {
				t.Errorf("?probe=1 = %d, want 202 downloading: %s", probe.Code, probe.Body.String())
			}
		})
	}
}
