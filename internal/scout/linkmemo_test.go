package scout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// tallyStore counts every upstream-shaped call a pool can make of it, so a test can prove a memo hit
// made none. `resolve` is handed the 1-based number of the call.
type tallyStore struct {
	svc      DebridService
	resolve  func(n int32) (string, error)
	resolves atomic.Int32
	statuses atomic.Int32
	checks   atomic.Int32
}

func (c *tallyStore) Service() DebridService { return c.svc }
func (c *tallyStore) CacheCheck(context.Context, []string) (map[string]bool, error) {
	c.checks.Add(1)
	return map[string]bool{}, nil
}
func (c *tallyStore) Resolve(context.Context, ResolveTarget) (string, error) {
	return c.resolve(c.resolves.Add(1))
}
func (c *tallyStore) Status(context.Context, ResolveTarget) (StoreStatus, bool) {
	c.statuses.Add(1)
	return StoreStatus{}, false
}

func (c *tallyStore) calls() int32 { return c.resolves.Load() + c.statuses.Load() + c.checks.Load() }

// numberedLinks mints a different link on every resolve, so a test can tell which resolve a 302 came from.
func numberedLinks(n int32) (string, error) {
	return "https://cdn.example/minted-" + strconv.Itoa(int(n)) + ".mkv", nil
}

func TestLinkMemo_expiresAndStaysBounded(t *testing.T) {
	var m linkMemo
	now := time.Unix(1_000_000, 0)
	m.put("k", "https://a", false, now)
	if got, ok := m.get("k", now.Add(linkMemoTTL-time.Second)); !ok || got.link != "https://a" {
		t.Fatalf("a live entry was not returned: %+v ok=%v", got, ok)
	}
	if _, ok := m.get("k", now.Add(linkMemoTTL)); ok {
		t.Fatal("an entry was served past its TTL")
	}

	for i := 0; i < linkMemoMaxEntries+10; i++ {
		m.put("k"+strconv.Itoa(i), "https://x", false, now)
		if i == 0 {
			// Touched after insertion, so it is the most recently used when the ceiling bites.
			m.get("k0", now)
		}
	}
	if m.ll.Len() != linkMemoMaxEntries || len(m.items) != linkMemoMaxEntries {
		t.Fatalf("memo holds %d/%d entries, want %d", m.ll.Len(), len(m.items), linkMemoMaxEntries)
	}
	if _, ok := m.get("k1", now); ok {
		t.Error("the least recently used entry survived the ceiling")
	}
}

// markChecked must not restart the TTL: it counts from the mint, not from the last look.
func TestLinkMemo_markCheckedKeepsTheMintTime(t *testing.T) {
	var m linkMemo
	now := time.Unix(1_000_000, 0)
	m.put("k", "https://a", false, now)
	m.markChecked("k", "https://a")
	got, ok := m.get("k", now)
	if !ok || !got.checked || !got.expires.Equal(now.Add(linkMemoTTL)) {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	// A different link under the same key is a newer mint, and a check of the old one says nothing about it.
	m.put("k", "https://b", false, now)
	m.markChecked("k", "https://a")
	if got, _ := m.get("k", now); got.checked {
		t.Error("a check of one link was credited to another")
	}
}

// The key is the account set plus the file. Two installs must never share a link, and the probe's
// read-only target must land on the same entry /play reads.
func TestLinkMemoKey_scopesByAccountAndFile(t *testing.T) {
	a := &Config{Debrid: []DebridAccount{{Service: ServiceTorBox, Token: "one"}}}
	b := &Config{Debrid: []DebridAccount{{Service: ServiceTorBox, Token: "two"}}}
	hash := repeat("a", 40)
	movie := ResolveTarget{InfoHash: hash, FileIdx: intp(0)}
	if linkMemoKey(a, movie) == linkMemoKey(b, movie) {
		t.Error("two accounts share a memo key")
	}
	readOnly := movie
	readOnly.NoAdd = true
	if linkMemoKey(a, movie) != linkMemoKey(a, readOnly) {
		t.Error("the probe's NoAdd target keys differently from /play's")
	}
	ep1 := ResolveTarget{InfoHash: hash, Season: intp(1), Episode: intp(1)}
	ep2 := ResolveTarget{InfoHash: hash, Season: intp(1), Episode: intp(2)}
	if linkMemoKey(a, ep1) == linkMemoKey(a, ep2) {
		t.Error("two episodes of one pack share a memo key")
	}
}

// A repeat play of the same file inside the TTL is answered from the memo with nothing asked upstream.
func TestPlay_memoServesARepeatWithNoUpstreamCalls(t *testing.T) {
	store := &tallyStore{svc: ServiceTorBox, resolve: numberedLinks}
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)})

	first := do(h, path, nil)
	if first.Code != http.StatusFound {
		t.Fatalf("first play: %d", first.Code)
	}
	before := store.calls()
	second := do(h, path, nil)
	if second.Code != http.StatusFound || second.Header().Get("location") != first.Header().Get("location") {
		t.Fatalf("repeat play: %d %q, want 302 %q", second.Code, second.Header().Get("location"),
			first.Header().Get("location"))
	}
	if got := store.calls() - before; got != 0 {
		t.Errorf("a memo hit made %d store calls, want 0", got)
	}
}

// fresh=1 skips the memo and replaces it: the client could not open the last link.
func TestPlay_freshBypassesAndReplacesTheMemo(t *testing.T) {
	store := &tallyStore{svc: ServiceTorBox, resolve: numberedLinks}
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)})

	if rr := do(h, path, nil); rr.Header().Get("location") != "https://cdn.example/minted-1.mkv" {
		t.Fatalf("first play: %d %q", rr.Code, rr.Header().Get("location"))
	}
	if rr := do(h, path+"?fresh=1", nil); rr.Header().Get("location") != "https://cdn.example/minted-2.mkv" {
		t.Fatalf("fresh play did not mint a new link: %d %q", rr.Code, rr.Header().Get("location"))
	}
	if rr := do(h, path, nil); rr.Header().Get("location") != "https://cdn.example/minted-2.mkv" {
		t.Fatalf("the fresh link did not replace the memo: %q", rr.Header().Get("location"))
	}
	if got := store.resolves.Load(); got != 2 {
		t.Errorf("resolves = %d, want 2", got)
	}
}

// fresh=1 drops the old link even when the new resolve fails, so the link the client just reported as
// unopenable is not served again by the next plain request.
func TestPlay_freshDropsTheOldLinkEvenWhenTheResolveFails(t *testing.T) {
	store := &tallyStore{svc: ServiceTorBox, resolve: func(n int32) (string, error) {
		if n == 1 {
			return numberedLinks(n)
		}
		return "", &DeadLinkError{"gone"}
	}}
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)})

	do(h, path, nil)
	if rr := do(h, path+"?fresh=1", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("fresh play: %d, want 404", rr.Code)
	}
	if rr := do(h, path, nil); rr.Code == http.StatusFound {
		t.Fatalf("the link the client reported as unopenable was served again: %q", rr.Header().Get("location"))
	}
}

// A failure is never remembered: the next play asks again.
func TestPlay_aFailedResolveIsNotMemoised(t *testing.T) {
	store := &tallyStore{svc: ServiceTorBox, resolve: func(n int32) (string, error) {
		if n == 1 {
			return "", &DeadLinkError{"not yet"}
		}
		return numberedLinks(n)
	}}
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)})

	if rr := do(h, path, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("first play: %d, want 404", rr.Code)
	}
	if rr := do(h, path, nil); rr.Code != http.StatusFound {
		t.Fatalf("second play: %d, want 302 from a second resolve", rr.Code)
	}
	if got := store.resolves.Load(); got != 2 {
		t.Errorf("resolves = %d, want 2", got)
	}
}

// The probe fan-out mints a link per probed release; /play then serves that link instead of minting the
// same file again.
func TestProbeBehind_fillsTheLinkMemoForPlay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/tracks.mkv")
	}))
	t.Cleanup(srv.Close)

	store := &tallyStore{svc: ServiceTorBox, resolve: func(int32) (string, error) {
		return srv.URL + "/tracks.mkv", nil
	}}
	h := &handler{deps: testDeps(func(d *Deps) {
		d.ProbeClient = srv.Client()
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	})}
	config, ok := decodeConfig(nil, validBlob)
	if !ok {
		t.Fatal("validBlob did not decode")
	}
	hash := repeat("a", 40)
	target := ResolveTarget{InfoHash: hash, FileIdx: intp(0)}
	probeTarget := target
	probeTarget.NoAdd = true
	h.probeBehind(config, []probeJob{{key: "probe:v1:" + hash + ":0", holders: []DebridService{ServiceTorBox},
		target: probeTarget}})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := h.links.get(linkMemoKey(config, target), time.Now()); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the probe never filled the link memo")
		}
		time.Sleep(10 * time.Millisecond)
	}

	before := store.calls()
	rr := do(http.HandlerFunc(h.serve), "/"+validBlob+"/play/"+encodePlayToken(PlayTarget{InfoHash: hash, FileIdx: intp(0)}), nil)
	if rr.Code != http.StatusFound || rr.Header().Get("location") != srv.URL+"/tracks.mkv" {
		t.Fatalf("play: %d %q, want 302 to the probe's link", rr.Code, rr.Header().Get("location"))
	}
	if got := store.calls() - before; got != 0 {
		t.Errorf("play re-asked the store %d times for a link the probe had just minted", got)
	}
}
