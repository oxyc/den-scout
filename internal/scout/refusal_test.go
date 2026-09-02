package scout

import (
	"context"
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

	cache.Put(refusedKey("tok", "hash-refused"), "createtorrent http 429", time.Minute)
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
			return []Store{fakeStore{svc: ServiceTorBox, resolve: func() (string, error) {
				resolves++
				return "", nil
			}}}
		},
	}}
	rec := httptest.NewRecorder()
	pool := &StorePool{stores: h.deps.MakeStores(&Config{})}
	h.handleProbe(rec, context.Background(), pool, "abc", ResolveTarget{InfoHash: "abc"})

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
		h.handleProbe(rec, context.Background(), &StorePool{stores: stores}, "abc",
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
	ready := probe([]Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{"abc": true}}})
	if ready.Code != http.StatusOK || !strings.Contains(ready.Body.String(), "ready") {
		t.Errorf("a held release should be 200 ready: %d %s", ready.Code, ready.Body.String())
	}

	// Recently refused → 503, naming the service. Never 404: the account was turned away, which says
	// nothing at all about whether the release exists.
	cache := NewMemoryCache(1 << 20)
	cache.Put(refusedKey("tok", "abc"), "createtorrent http 429", time.Minute)
	refusedStore := &torBoxStore{token: "tok", cache: cache, api: "https://api.example",
		client: &stubDoer{status: 200, body: `{"data":[]}`}}
	h := &handler{deps: Deps{Cache: NewMemoryCache(1 << 20),
		MakeStores: func(*Config) []Store { return []Store{refusedStore} }}}
	rec := httptest.NewRecorder()
	h.handleProbe(rec, context.Background(), &StorePool{stores: []Store{refusedStore}}, "abc",
		ResolveTarget{InfoHash: "abc"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a refused account should be 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "store_unavailable") {
		t.Errorf("the client must be able to tell a refusal from an absence: %s", rec.Body.String())
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
