package scout

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	_, ok := scrapeAll(context.Background(), []scraper{answeredEmpty, unaskable}, scrapeQuery{}, time.Second)
	if !ok {
		t.Error("every askable indexer answered, so the empty result is authoritative")
	}

	// A non-empty list is trusted whatever the coverage: whatever came back is real.
	found := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
		return []RawStream{{InfoHash: repeat("d", 40), Title: "a release"}}, nil
	}}
	if seeds, ok := scrapeAll(context.Background(), []scraper{found, unaskable}, scrapeQuery{},
		time.Second); !ok || len(seeds) != 1 {
		t.Errorf("a non-empty result stands on its own: %d seeds, ok=%v", len(seeds), ok)
	}

	// A real failure still withholds the verdict — that is the rule this must not weaken.
	failed := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
		return nil, fmt.Errorf("502")
	}}
	if _, ok := scrapeAll(context.Background(), []scraper{failed, unaskable}, scrapeQuery{}, time.Second); ok {
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
	recordRefusal(cache, ServiceTorBox, "tok", H, &StoreUnavailableError{ServiceTorBox, "429"})

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
	recordRefusal(cache, ServiceTorBox, "tok", H, &StoreUnavailableError{ServiceTorBox, "429"})
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
