package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const H = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestTorBoxCacheCheck(t *testing.T) {
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"data":{"`+H+`":{"name":"x"}}}`), nil
	}}
	s := &torBoxStore{token: "t", client: d, api: torboxAPI}
	m, err := s.CacheCheck(context.Background(), []string{H, other})
	if err != nil || !m[H] || m[other] {
		t.Errorf("cacheCheck: %v err=%v", m, err)
	}

	// every batch failing → errCheckFailed (lets the pool detect an outage)
	boom := &torBoxStore{token: "t", client: mockDoer{fn: func(*http.Request) (*http.Response, error) { return resp(503, "down"), nil }}, api: torboxAPI}
	if _, err := boom.CacheCheck(context.Background(), []string{H}); err == nil {
		t.Error("all-batch failure should return an error")
	}
}

// A batch that failed leaves its hashes OUT of the map rather than marking them uncached.
//
// Checks go out in batches of 100 and up to 500 hashes are checked, so one timed-out batch used to
// assert "TorBox does not hold this" about 100 releases — and report no error, because another batch
// succeeded. With cachedOnly on, that silently drops up to 100 genuinely playable releases with no
// degraded signal; without it, playing one pays an add for a torrent the account already had.
func TestTorBoxCacheCheck_aFailedBatchIsAbsentNotUncached(t *testing.T) {
	hashes := make([]string, cacheBatch+2)
	for i := range hashes {
		hashes[i] = fmt.Sprintf("%040x", i)
	}
	// The first batch answers (and holds nothing); the second — the last two hashes — fails.
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Count(r.URL.Query().Get("hash"), ",")+1 == cacheBatch {
			return resp(200, `{"data":{}}`), nil
		}
		return resp(503, "down"), nil
	}}
	s := &torBoxStore{token: "t", client: d, api: torboxAPI}
	m, err := s.CacheCheck(context.Background(), hashes)
	if err != nil {
		t.Fatalf("one good batch is not a total failure: %v", err)
	}
	answered, ok := m[hashes[0]]
	if !ok || answered {
		t.Errorf("a hash in the batch that came back must be present and false: present=%v value=%v", ok, answered)
	}
	if _, present := m[hashes[cacheBatch]]; present {
		t.Error("a hash in the FAILED batch must be absent — present-and-false claims TorBox does not hold it")
	}
}

// The account's whole torrent list is fetched to find a hash, and only hits were remembered. So a miss —
// which is every poll of a release that was never queued — refetched the entire list, once per poll, for
// the length of a download.
func TestTorBoxTorrentID_remembersAMiss(t *testing.T) {
	lists := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "mylist") {
			lists++
			return resp(200, `{"data":[]}`), nil
		}
		return resp(404, "{}"), nil
	}}
	s := &torBoxStore{token: "t", client: d, api: torboxAPI, cache: NewMemoryCache(1 << 20)}
	for i := 0; i < 5; i++ {
		if _, ok, _ := s.torrentID(context.Background(), H); ok {
			t.Fatal("an empty account holds nothing")
		}
	}
	if lists != 1 {
		t.Errorf("fetched the whole account list %d times for five polls of the same miss", lists)
	}
}

func TestTorBoxResolveMovie(t *testing.T) {
	var reqdl string
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "createtorrent"):
			return resp(200, `{"data":{"torrent_id":7}}`), nil
		case strings.Contains(r.URL.Path, "requestdl"):
			reqdl = r.URL.RawQuery
			return resp(200, `{"success":true,"data":"https://cdn.torbox/x.mkv"}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			t.Error("movie should not list files")
		}
		return resp(404, "{}"), nil
	}}
	s := &torBoxStore{token: "t", client: d, api: torboxAPI}
	link, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)})
	if err != nil || link != "https://cdn.torbox/x.mkv" {
		t.Fatalf("resolve: %q err=%v", link, err)
	}
	if !strings.Contains(reqdl, "file_id=0") {
		t.Errorf("requestdl missing file_id: %s", reqdl)
	}
}

func TestTorBoxBingeCacheAndNoPoison(t *testing.T) {
	// binge: 2 episodes on the same pack → createtorrent + mylist once.
	creates, lists, dls := 0, 0, 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "createtorrent"):
			creates++
			return resp(200, `{"data":{"torrent_id":9}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			lists++
			return resp(200, `{"data":{"files":[{"id":0,"name":"S01E01.mkv","size":10},{"id":1,"name":"S01E02.mkv","size":20}]}}`), nil
		case strings.Contains(r.URL.Path, "requestdl"):
			dls++
			return resp(200, `{"success":true,"data":"https://cdn/`+r.URL.Query().Get("file_id")+`"}`), nil
		}
		return resp(404, "{}"), nil
	}}
	s := &torBoxStore{token: "t", client: d, cache: NewMemoryCache(1 << 20), api: torboxAPI}
	ep1, _ := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(1)})
	ep2, _ := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if ep1 != "https://cdn/0" || ep2 != "https://cdn/1" {
		t.Errorf("binge picks: ep1=%q ep2=%q", ep1, ep2)
	}
	if creates != 1 || lists != 1 || dls != 2 {
		t.Errorf("binge cache: creates=%d lists=%d dls=%d (want 1,1,2)", creates, lists, dls)
	}

	// no-poison (#3): a failed mylist must not cache files:[] and mis-serve later episodes.
	lists2 := 0
	d2 := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "createtorrent"):
			return resp(200, `{"data":{"torrent_id":9}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			lists2++
			return resp(500, "boom"), nil // empty file list
		case strings.Contains(r.URL.Path, "requestdl"):
			return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
		}
		return resp(404, "{}"), nil
	}}
	s2 := &torBoxStore{token: "t", client: d2, cache: NewMemoryCache(1 << 20), api: torboxAPI}
	_, _ = s2.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(1)})
	_, _ = s2.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if lists2 != 2 {
		t.Errorf("no-poison: mylist should be retried (got %d, want 2)", lists2)
	}
}

func TestTorBoxDeadLink(t *testing.T) {
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "createtorrent") {
			return resp(200, `{"data":{"torrent_id":7}}`), nil
		}
		return resp(200, `{"success":false}`), nil
	}}
	s := &torBoxStore{token: "t", client: d, api: torboxAPI}
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)}); err == nil {
		t.Error("expected DeadLinkError")
	}
}

func TestRealDebrid(t *testing.T) {
	m, err := (&realDebridStore{}).CacheCheck(context.Background(), []string{H})
	if err != nil || m[H] {
		t.Errorf("RD cacheCheck should be all-false, no error: %v %v", m, err)
	}

	infoCalls := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "addMagnet"):
			return resp(201, `{"id":"t1"}`), nil
		case strings.Contains(r.URL.Path, "/torrents/info/"):
			infoCalls++
			if infoCalls == 1 {
				return resp(200, `{"files":[{"id":1,"path":"/movie.mkv","bytes":999}],"links":[]}`), nil
			}
			return resp(200, `{"files":[{"id":1,"path":"/movie.mkv","bytes":999}],"links":["https://rd/restricted"]}`), nil
		case strings.Contains(r.URL.Path, "selectFiles"):
			return resp(204, ""), nil
		case strings.Contains(r.URL.Path, "unrestrict/link"):
			return resp(200, `{"download":"https://rd/dl.mkv"}`), nil
		}
		return resp(404, "{}"), nil
	}}
	link, err := (&realDebridStore{token: "t", client: d, api: realDebridAPI}).Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if err != nil || link != "https://rd/dl.mkv" {
		t.Fatalf("RD resolve: %q err=%v", link, err)
	}

	// blocked filename → DeadLinkError (pool falls through)
	db := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "addMagnet") {
			return resp(201, `{"id":"t1"}`), nil
		}
		return resp(200, `{"files":[{"id":1,"path":"Movie.WEB-DL.x264.mkv","bytes":999}],"links":[]}`), nil
	}}
	if _, err := (&realDebridStore{token: "t", client: db, api: realDebridAPI}).Resolve(context.Background(), ResolveTarget{InfoHash: H}); err == nil {
		t.Error("RD-blocked filename should error")
	}
}

func TestPremiumize(t *testing.T) {
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dc := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"status":"success","response":[true,false]}`), nil
	}}
	m, err := (&premiumizeStore{token: "t", client: dc, api: premiumizeAPI}).CacheCheck(context.Background(), []string{H, other})
	if err != nil || !m[H] || m[other] {
		t.Errorf("PM cacheCheck: %v err=%v", m, err)
	}
	dr := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"status":"success","content":[{"path":"S01E01.mkv","link":"https://pm/1","size":10},{"path":"S01E02.mkv","link":"https://pm/2","size":20}]}`), nil
	}}
	link, err := (&premiumizeStore{token: "t", client: dr, api: premiumizeAPI}).Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if err != nil || link != "https://pm/2" {
		t.Errorf("PM resolve: %q err=%v", link, err)
	}
}

type fakeStore struct {
	svc      DebridService
	check    map[string]bool
	checkErr error
	resolve  func() (string, error)
	status   *StoreStatus
	// statusBlocks makes Status outlast whatever budget it is given, the way a slow account listing does.
	statusBlocks bool
}

func (f fakeStore) Service() DebridService { return f.svc }
func (f fakeStore) CacheCheck(context.Context, []string) (map[string]bool, error) {
	return f.check, f.checkErr
}
func (f fakeStore) Resolve(context.Context, ResolveTarget) (string, error) { return f.resolve() }
func (f fakeStore) Status(ctx context.Context, _ ResolveTarget) (StoreStatus, bool) {
	if f.statusBlocks {
		<-ctx.Done()
		return StoreStatus{}, false
	}
	if f.status == nil {
		return StoreStatus{}, false
	}
	return *f.status, true
}

// answeringStore implements the optional three-valued statusAnswerer, and DISAGREES with its own
// two-valued Status on purpose — that is the only way a test can tell which one the pool consulted.
type answeringStore struct {
	fakeStore
	answer statusAnswer
	asked  *int
	// onAsk runs after the answer is recorded, so a test can make the world change mid-loop — the caller
	// hanging up being the one that drives the pool's out-of-time branch.
	onAsk func()
}

func (a answeringStore) StatusAnswer(_ context.Context, _ ResolveTarget) (StoreStatus, statusAnswer) {
	if a.asked != nil {
		*a.asked++
	}
	// Deliberately does NOT honour the embedded fakeStore's statusBlocks. That was added to drive the
	// pool's out-of-time branch and does not reach it — a store's own slice expiring leaves the pool with
	// time in hand, so it simply moves on. onAsk drives that branch instead, and a bare <-ctx.Done() here
	// would hang forever the first time a test passed a context with no deadline.
	if a.onAsk != nil {
		a.onAsk()
	}
	return StoreStatus{Progress: 0.5}, a.answer
}

// The pool asks statusAnswerer where a store has one, in preference to the bool Status.
//
// Nothing asserted it: making the type switch never match left the whole suite green, and the fallback
// answers only yes/no — so every "could not find out" collapsed to "nobody is fetching it", which is the
// answer that makes /play queue a duplicate against an account that is already refusing.
func TestStorePool_prefersTheThreeValuedAnswer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		answer        statusAnswer
		wantOK        bool
		wantUnknown   bool
		wantStoreAsks int
	}{
		// The bool Status of this fixture always answers (StoreStatus{}, false), i.e. "no". Neither of
		// these two outcomes is reachable through it.
		{"unknown is carried, not flattened to no", statusUnknown, false, true, 1},
		{"downloading is carried too", statusDownloading, true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := 0
			pool := &StorePool{stores: []Store{answeringStore{
				fakeStore: fakeStore{svc: ServiceTorBox}, answer: tc.answer, asked: &asked}}}
			status, ok, unknown := pool.Status(context.Background(), ResolveTarget{InfoHash: H})
			if ok != tc.wantOK || unknown != tc.wantUnknown {
				t.Errorf("pool answered ok=%v unknown=%v, want %v/%v", ok, unknown, tc.wantOK, tc.wantUnknown)
			}
			if asked != tc.wantStoreAsks {
				t.Errorf("StatusAnswer called %d times, want %d — the pool used the bool fallback",
					asked, tc.wantStoreAsks)
			}
			if tc.wantOK && status.Progress != 0.5 {
				t.Errorf("the status itself was dropped: %+v", status)
			}
		})
	}
}

func TestStorePool(t *testing.T) {
	// buildStores orders TorBox first regardless of config order
	cfg := &Config{Debrid: []DebridAccount{{ServicePremiumize, "p"}, {ServiceTorBox, "t"}, {ServiceRealDebrid, "r"}}}
	stores := buildStores(cfg, mockDoer{}, nil)
	if len(stores) != 3 || stores[0].Service() != ServiceTorBox || stores[2].Service() != ServicePremiumize {
		t.Errorf("store order: %v", []DebridService{stores[0].Service(), stores[1].Service(), stores[2].Service()})
	}

	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pool := &StorePool{stores: []Store{
		fakeStore{svc: ServiceTorBox, check: map[string]bool{H: true, other: false}},
		fakeStore{svc: ServicePremiumize, check: map[string]bool{H: false, other: true}},
	}}
	m, truthOK := pool.CacheCheck(context.Background(), []string{H, other})
	if !m.Cached(H) || !m.Cached(other) {
		t.Errorf("pool union: %v / %v", m.HeldBy(H), m.HeldBy(other))
	}
	if !truthOK {
		t.Error("pool truthOK should be true when a cache-truth store succeeded")
	}

	// all cache-truth stores erroring → truthOK false (an outage the handler must not cache)
	down := &StorePool{stores: []Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{H: false}, checkErr: errCheckFailed}}}
	if _, ok := down.CacheCheck(context.Background(), []string{H}); ok {
		t.Error("truthOK should be false when every cache-truth store failed")
	}
	// RD succeeding is not cache truth → truthOK false
	rd := &StorePool{stores: []Store{fakeStore{svc: ServiceRealDebrid, check: map[string]bool{H: false}}}}
	if _, ok := rd.CacheCheck(context.Background(), []string{H}); ok {
		t.Error("RD success is not cache truth")
	}

	deadOnly := &StorePool{stores: []Store{fakeStore{resolve: func() (string, error) { return "", &DeadLinkError{"x"} }}}}
	if _, err := deadOnly.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err == nil {
		t.Error("all-fail should error")
	}
	fallthr := &StorePool{stores: []Store{
		fakeStore{resolve: func() (string, error) { return "", &DeadLinkError{"x"} }},
		fakeStore{resolve: func() (string, error) { return "https://ok", nil }},
	}}
	if link, err := fallthr.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err != nil || link != "https://ok" {
		t.Errorf("fallthrough: %q err=%v", link, err)
	}
}

func TestHasCacheTruthAndRDOnly(t *testing.T) {
	if hasCacheTruth(&Config{Debrid: []DebridAccount{{ServiceRealDebrid, "r"}}}) {
		t.Error("RD-only should have no cache truth")
	}
	if !hasCacheTruth(&Config{Debrid: []DebridAccount{{ServiceRealDebrid, "r"}, {ServiceTorBox, "t"}}}) {
		t.Error("torbox present → cache truth")
	}
	if !rdOnly(&Config{Debrid: []DebridAccount{{ServiceRealDebrid, "r"}}}) {
		t.Error("rd-only")
	}
	if rdOnly(&Config{Debrid: []DebridAccount{{ServiceRealDebrid, "r"}, {ServiceTorBox, "t"}}}) {
		t.Error("not rd-only when torbox present")
	}
}

// A queued release is the whole point of Status: TorBox has been asked for it, the link isn't ready, and
// the client must be told "downloading" rather than "dead". These cover the shapes that made that answer
// wrong — a torrent the user deleted, an empty payload, and the series path, where the resolve entry is
// deliberately NOT written because the file list isn't known yet.
func TestTorBoxStatus(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantOK   bool
		wantProg float64
	}{
		{"downloading", `{"success":true,"data":{"progress":0.42,"download_finished":false,"eta":300}}`, true, 0.42},
		{"array payload", `{"success":true,"data":[{"progress":0.1,"download_finished":false}]}`, true, 0.1},
		{"finished but unresolvable", `{"success":true,"data":{"progress":1,"download_finished":true}}`, false, 0},
		{"deleted torrent", `{"success":false,"data":null}`, false, 0},
		{"null payload", `{"success":true,"data":null}`, false, 0},
		{"empty object", `{"success":true,"data":{}}`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewMemoryCache(1 << 16)
			cache.Put(torrentIDKey("t", H), "7", time.Hour)
			s := &torBoxStore{token: "t", cache: cache, api: torboxAPI, client: mockDoer{
				fn: func(*http.Request) (*http.Response, error) { return resp(200, tc.body), nil },
			}}
			got, ok := s.Status(context.Background(), ResolveTarget{InfoHash: H})
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Progress != tc.wantProg {
				t.Errorf("progress = %v, want %v", got.Progress, tc.wantProg)
			}
		})
	}

	// No torrent id remembered, and the account doesn't hold it either → nothing to report, rather than
	// a promise we can't keep.
	empty := &torBoxStore{token: "t", cache: NewMemoryCache(1 << 16), api: torboxAPI, client: mockDoer{
		fn: func(*http.Request) (*http.Response, error) { return resp(200, `{"data":[]}`), nil },
	}}
	if _, ok := empty.Status(context.Background(), ResolveTarget{InfoHash: H}); ok {
		t.Error("an unknown infohash must not report a download")
	}
}

// Losing the cached torrent id is not losing the download: a redeploy or a pruned cache used to turn a
// perfectly healthy fetch into a 404, which a client can only read as a dead link. The id is rediscovered
// from the account's own list — and remembered, so the next poll is a single-id lookup again.
func TestTorBoxStatusRediscoversLostTorrentID(t *testing.T) {
	var listCalls int
	// The indexer's hash is upper-case, TorBox reports lower-case — so this covers the fold too.
	target := strings.ToUpper(H)
	cache := NewMemoryCache(1 << 16) // deliberately empty: the id was never written, or has expired
	s := &torBoxStore{token: "t", cache: cache, api: torboxAPI, client: mockDoer{
		fn: func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.RawQuery, "id=") {
				return resp(200, `{"data":{"progress":0.25,"download_finished":false}}`), nil
			}
			listCalls++
			return resp(200, `{"data":[{"id":7,"hash":"`+H+`"}]}`), nil
		},
	}}
	got, ok := s.Status(context.Background(), ResolveTarget{InfoHash: target})
	if !ok {
		t.Fatal("a download the account is holding must be reported, cached id or not")
	}
	if got.Progress != 0.25 {
		t.Errorf("progress = %v, want 0.25", got.Progress)
	}
	if raw, cached := cache.Get(torrentIDKey("t", target)); !cached || raw != "7" {
		t.Errorf("the rediscovered id must be remembered; got %q cached=%v", raw, cached)
	}
	// Second call must reuse the cache rather than list the whole account again.
	if _, ok := s.Status(context.Background(), ResolveTarget{InfoHash: target}); !ok {
		t.Fatal("second status")
	}
	if listCalls != 1 {
		t.Errorf("account listed %d times, want 1", listCalls)
	}
}

// The series path is the one that used to lose the torrent id: `Resolve` refuses to cache an entry whose
// file list is empty, and a just-queued torrent has no files yet. The id must survive that anyway.
func TestTorBoxRemembersTorrentIDForQueuedEpisode(t *testing.T) {
	cache := NewMemoryCache(1 << 16)
	s := &torBoxStore{token: "t", cache: cache, api: torboxAPI, client: mockDoer{
		fn: func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.Contains(r.URL.Path, "createtorrent"):
				return resp(200, `{"data":{"torrent_id":7}}`), nil
			case strings.Contains(r.URL.Path, "mylist"):
				return resp(200, `{"success":true,"data":{"files":[]}}`), nil // queued: no files yet
			default:
				return resp(200, `{"success":false}`), nil // link not ready
			}
		},
	}}
	season, episode := 1, 3
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: &season, Episode: &episode})
	if err == nil {
		t.Fatal("expected the link request to fail while the torrent is still queued")
	}
	if _, ok := cache.Get(torrentIDKey("t", H)); !ok {
		t.Error("the torrent id must be remembered so Status can report the download")
	}
}

// A store that could not answer is not a store saying "no", and the pool has to report the difference.
//
// Inferring it from the caller's own clock does not work once the budget is sliced per store: store 0 can
// burn its slice and time out while the pool still returns long before the caller's deadline. "Did my
// whole budget elapse" was then false exactly when a store HAD failed to answer, so /play queued a second
// copy of a torrent store 0 was already fetching — the bug the escalation exists to prevent, reappearing
// for every multi-account install.
func TestStorePoolStatus_reportsWhenAStoreCouldNotAnswer(t *testing.T) {
	wedged := fakeStore{svc: ServiceTorBox, status: nil, statusBlocks: true}
	silent := fakeStore{svc: ServiceRealDebrid} // answers at once: "not downloading"

	// One store that runs out of time → indeterminate, even though the pool returns early.
	pool := &StorePool{stores: []Store{wedged, silent}}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, ok, unknown := pool.Status(ctx, ResolveTarget{InfoHash: H})
	if ok {
		t.Fatal("a wedged store reported a download")
	}
	if !unknown {
		t.Error("a store that ran out of time was reported as a definitive 'not downloading'")
	}
	if elapsed := time.Since(start); elapsed >= 80*time.Millisecond {
		t.Errorf("the first store consumed the whole budget (%v) — the slices are not being applied", elapsed)
	}

	// Every store answering promptly → a real answer, not indeterminate.
	clean := &StorePool{stores: []Store{silent, silent}}
	cctx, ccancel := context.WithTimeout(context.Background(), time.Second)
	defer ccancel()
	if _, ok, unknown := clean.Status(cctx, ResolveTarget{InfoHash: H}); ok || unknown {
		t.Errorf("prompt answers reported ok=%v unknown=%v, want false/false", ok, unknown)
	}
}

// StatusDetail names the stores that could not answer, so the escalated retry asks only those.
//
// The retry used to go back through the whole pool, re-asking stores that had just answered
// definitively — one unknown store in a three-account pool cost nine store reads per /play instead of
// seven, and each TorBox read is up to two upstream calls, on a two-second poll cadence. Seven, not six:
// the post-failure read runs after an add may have landed and deliberately asks everyone again. Saying
// six here would read as an instruction to narrow that one too, which is the reversal this exists to
// forbid — see TestHandlePlay_theEscalationReAsksOnlyTheStoreThatCouldNotAnswer, which counts all three.
func TestStorePoolStatusDetail_namesOnlyTheStoresThatCouldNotAnswer(t *testing.T) {
	var uncertainAsks, definiteAsks int
	uncertain := answeringStore{fakeStore: fakeStore{svc: ServiceTorBox}, answer: statusUnknown, asked: &uncertainAsks}
	definite := answeringStore{fakeStore: fakeStore{svc: ServiceRealDebrid}, answer: statusNo, asked: &definiteAsks}

	pool := &StorePool{stores: []Store{definite, uncertain, definite}}
	_, ok, unknown := pool.StatusDetail(context.Background(), ResolveTarget{InfoHash: H})
	if ok {
		t.Fatal("nothing was downloading")
	}
	if len(unknown) != 1 {
		t.Fatalf("reported %d stores as unable to answer, want 1", len(unknown))
	}
	if unknown[0].Service() != ServiceTorBox {
		t.Errorf("named %s as the store that could not answer, want torbox", unknown[0].Service())
	}

	// The retry, as handlePlay performs it: only the named store is asked again.
	before := definiteAsks
	if _, _, _ = (&StorePool{stores: unknown}).StatusDetail(context.Background(), ResolveTarget{InfoHash: H}); definiteAsks != before {
		t.Errorf("the retry asked a store that had already answered definitively (%d extra reads)",
			definiteAsks-before)
	}
	if uncertainAsks != 2 {
		t.Errorf("the store that could not answer was asked %d times, want 2 (once, then the retry)", uncertainAsks)
	}
}

// Running out of time mid-loop keeps the stores that ALREADY said they could not answer, as well as the
// ones never reached.
//
// That branch had no coverage at all: returning nil from it — "every store answered no" — left the whole
// suite green. And returning only the unasked tail is worse than useless, because the escalation then
// re-asks stores that were never asked (which answer promptly, and say no) while never re-asking the one
// store that was uncertain. /play reads that as nobody fetching the torrent and spends an add on it.
func TestStorePoolStatusDetail_keepsUncertainStoresWhenTimeRunsOut(t *testing.T) {
	// THREE stores, one of each kind: answered, uncertain, never reached. With only the last two, the
	// correct answer and "just return the whole pool" are the same slice, so the test could not tell the
	// fix from the over-broad variant that re-asks a store which already answered — and returning
	// p.stores left the whole suite green.
	//
	// Store 1 says it cannot tell; the caller then hangs up, so store 2 is never reached. A store's own
	// slice expiring does not get here — the pool simply moves on with time left — so the branch is driven
	// the way production reaches it, by the request context ending.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	answered := answeringStore{fakeStore: fakeStore{svc: ServicePremiumize}, answer: statusNo}
	uncertain := answeringStore{fakeStore: fakeStore{svc: ServiceTorBox}, answer: statusUnknown, onAsk: cancel}
	unreached := fakeStore{svc: ServiceRealDebrid}

	pool := &StorePool{stores: []Store{answered, uncertain, unreached}}
	_, ok, unknown := pool.StatusDetail(ctx, ResolveTarget{InfoHash: H})
	if ok {
		t.Fatal("nothing was downloading")
	}
	var named []DebridService
	for _, st := range unknown {
		named = append(named, st.Service())
	}
	// Exactly the uncertain one and the unreached one: the tail alone would drop torbox, and the whole
	// pool would add premiumize, which answered.
	want := []DebridService{ServiceTorBox, ServiceRealDebrid}
	if len(named) != len(want) {
		t.Fatalf("named %v as unable to answer, want %v", named, want)
	}
	for i, w := range want {
		if named[i] != w {
			t.Fatalf("named %v as unable to answer, want %v", named, want)
		}
	}
}

// The account listing is decoded element by element, so its size bounds time and not memory — which is
// what makes the larger cap safe. A body past the cap must read as "no answer", never as "the account
// holds nothing", because the second costs a duplicate add.
func TestFetchAccountListing_streamsAndDetectsTruncation(t *testing.T) {
	var big strings.Builder
	big.WriteString(`{"success":true,"data":[`)
	for i := 0; i < 3000; i++ {
		if i > 0 {
			big.WriteString(",")
		}
		// Each entry carries a large junk field, exactly what makes a real listing big.
		fmt.Fprintf(&big, `{"id":%d,"hash":"%040x","files":"%s"}`, i, i, repeat("f", 400))
	}
	big.WriteString(`]}`)

	ids, ok, _ := decodeListing(json.NewDecoder(strings.NewReader(big.String())))
	if !ok {
		t.Fatal("a well-formed large listing was refused")
	}
	if len(ids) != 3000 {
		t.Errorf("decoded %d entries, want 3000", len(ids))
	}
	if got := ids[fmt.Sprintf("%040x", 7)]; got != 7 {
		t.Errorf("hash→id mapping wrong: %d", got)
	}

	// Truncation is detectable, and must not look like an empty account.
	// Constructed exactly as production does: the reader is given limit+1 so that "read the cap exactly"
	// and "cut off at the cap" are distinguishable.
	td := &truncationDetector{r: io.LimitReader(strings.NewReader(big.String()), (1<<10)+1), limit: 1 << 10}
	if _, ok, _ := decodeListing(json.NewDecoder(td)); ok {
		t.Error("a truncated listing decoded as a valid answer")
	}
	if !td.truncated() {
		t.Error("truncation went undetected — it would read as 'this account holds nothing'")
	}

	// An envelope with no data key is not a claim about the account either.
	if _, ok, _ := decodeListing(json.NewDecoder(strings.NewReader(`{"success":true}`))); ok {
		t.Error("a listing with no data key was treated as authoritative")
	}
	// A REPEATED data key is unreadable, whichever order it comes in. Every rule for picking one of them
	// is unsafe in one direction — a trailing null erases a list that was read, and an empty first array
	// beats a populated second one into an authoritative "holds nothing" that costs a duplicate add —
	// and the two safe directions are opposites, so no pick is safe. All four shapes must be ok=false.
	for _, body := range []string{
		`{"success":true,"data":null,"data":[{"id":7,"hash":"` + H + `"}]}`,
		`{"success":true,"data":[{"id":7,"hash":"` + H + `"}],"data":null}`,
		`{"success":true,"data":[],"data":[{"id":7,"hash":"` + H + `"}]}`,
		`{"success":true,"data":[{"id":7,"hash":"` + H + `"}],"data":[]}`,
	} {
		if ids, ok, _ := decodeListing(json.NewDecoder(strings.NewReader(body))); ok {
			t.Errorf("a duplicate data key was treated as an answer (%v): %s", ids, body)
		}
	}
	if _, ok, _ := decodeListing(json.NewDecoder(strings.NewReader(`{"success":false,"data":[]}`))); ok {
		t.Error("success:false was treated as authoritative")
	}
	// An explicitly empty account IS an answer.
	if ids, ok, _ := decodeListing(json.NewDecoder(strings.NewReader(`{"success":true,"data":[]}`))); !ok || len(ids) != 0 {
		t.Errorf("an empty account should be a real answer: ok=%v n=%d", ok, len(ids))
	}
}

// A listing larger than the ordinary store cap still parses, because the account listing has its own.
//
// 4 MiB truncates a real large account (~13 MB at 2,000 torrents), and a truncated listing read back as
// "this account holds nothing" — so /play queued a second copy of a torrent already downloading. The
// fixture sits between the two caps deliberately: it fails under maxStoreBytes and passes under
// maxListingBytes, which is the only way this asserts the cap and not just the decoder.
func TestFetchAccountListing_largeAccountIsNotTruncated(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"success":true,"data":[`)
	for i := 0; body.Len() < 6<<20; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":%d,"hash":"%040x","files":"%s"}`, i, i, repeat("f", 2000))
	}
	body.WriteString(`]}`)
	if body.Len() <= maxStoreBytes {
		t.Fatalf("fixture is %d bytes, not past the %d ordinary cap", body.Len(), maxStoreBytes)
	}
	if body.Len() >= maxListingBytes {
		t.Fatalf("fixture is %d bytes, past even the %d listing cap", body.Len(), maxListingBytes)
	}

	s := &torBoxStore{token: "t", api: torboxAPI, client: mockDoer{func(*http.Request) (*http.Response, error) {
		return resp(200, body.String()), nil
	}}}
	ids, ok, _ := s.fetchAccountListing(context.Background())
	if !ok {
		t.Fatal("a large but legitimate account listing was refused — it reads back as 'holds nothing'")
	}
	if len(ids) < 100 {
		t.Errorf("decoded only %d entries from a %d-byte listing", len(ids), body.Len())
	}
}

// Hands each run of the singleflight test its own account token — see the note at its top.
var listingFlightTestSeq atomic.Int64

// Joining an in-flight listing fetch respects the JOINER's budget, and the leader survives its caller.
//
// singleflight is context-blind: Do blocks a follower on the leader's WaitGroup with the follower's own
// deadline unobserved, and runs the fetch on whichever caller arrived first. Both halves were live and
// neither had a test. A follower with a 100 ms budget was measured returning after 851 ms — on a /play
// poll read capped at statusBudget so a wait answers promptly — and a follower with ten seconds left was
// measured inheriting a leader's 120 ms failure.
func TestAccountListing_singleflightRespectsEachCallersBudget(t *testing.T) {
	// A token unique to this invocation, because listingFlight is process-wide and keyed on the token
	// alone. With a fixed one, a -count rerun can join the PREVIOUS iteration's flight before it has
	// finished deregistering, get that iteration's result, and never call this iteration's transport — so
	// the leader blocks at <-arrivals forever and a legible failure becomes a package-wide timeout.
	token := fmt.Sprintf("shared-%d", listingFlightTestSeq.Add(1))
	release := make(chan struct{})
	// Released on EVERY exit, including a t.Fatal, so a failing assertion never strands an in-flight fetch
	// holding the singleflight entry.
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	// One send per fetch STARTED, so "did anyone start a second fetch" is answerable without reading a
	// counter across goroutines.
	arrivals := make(chan struct{}, 4)
	newStore := func(token string) *torBoxStore {
		return &torBoxStore{token: token, api: torboxAPI, cache: NewMemoryCache(1 << 20),
			client: mockDoer{func(r *http.Request) (*http.Response, error) {
				arrivals <- struct{}{}
				// Honours the request context, like a real transport — otherwise cancelling the leader
				// changes nothing and the detachment half of this test asserts nothing. Cancellation is
				// checked FIRST and on its own: once both it and release are ready a plain two-way select
				// picks at random, which would make the detachment assertion a coin flip.
				select {
				case <-r.Context().Done():
					return nil, r.Context().Err()
				default:
				}
				select {
				case <-release: // held until the test lets the leader finish
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				return resp(200, `{"success":true,"data":[{"id":4,"hash":"`+H+`"}]}`), nil
			}}}
	}

	// The leader: a long budget, and it goes away before the fetch completes. A DEADLINE, not a bare
	// cancel — the leader's context is only carved into a detached one when it has a deadline to copy, so
	// a cancel-only fixture leaves that whole branch unexecuted and the detachment untested.
	leader := newStore(token)
	leaderCtx, cancelLeader := context.WithTimeout(context.Background(), 30*time.Second)
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = leader.accountListing(leaderCtx)
	}()
	<-arrivals // the fetch is in flight

	// A follower on the same account with a SHORT budget must not wait for the leader. Run in a goroutine
	// with its own ceiling: blocked on the leader's WaitGroup this never returns at all, and a test that
	// simply called it would hang the package for the whole -timeout instead of saying why.
	follower := newStore(token)
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	answered := make(chan bool, 1)
	go func() {
		_, ok := follower.accountListing(short)
		answered <- ok
	}()
	select {
	case ok := <-answered:
		if ok {
			t.Error("a follower that ran out of time reported an answer")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the follower is still waiting well past its 50ms budget — it is blocked on the " +
			"leader's clock, not its own")
	}

	// A joiner with a long budget attaches to the SAME in-flight fetch — asserted, not assumed, by the
	// fetch count. An earlier version of this simply called accountListing after cancelling the leader,
	// which lets the caller start a NEW flight of its own and so passes whether or not the leader's fetch
	// survived. It has to be attached before the leader goes away, or it proves nothing.
	joiner := newStore(token)
	joinerCtx, cancelJoiner := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelJoiner()
	joined := make(chan bool, 1)
	go func() {
		_, ok := joiner.accountListing(joinerCtx)
		joined <- ok
	}()
	select {
	case <-arrivals:
		t.Fatal("the joiner started a second fetch instead of attaching to the one in flight, so " +
			"cancelling the leader would prove nothing about whether its fetch survives")
	case <-time.After(250 * time.Millisecond):
		// No second fetch started: the joiner is attached to the leader's.
	}

	// Now the leader's caller hangs up. The fetch must survive it, because the joiner is waiting on it.
	// Release only once the leader's call has fully RETURNED, so anything it cancels on the way out has
	// already happened — otherwise the fetch can finish first and the assertion never gets to fire.
	cancelLeader()
	<-leaderDone
	releaseOnce()
	select {
	case ok := <-joined:
		if !ok {
			t.Error("a caller with ten seconds left inherited the departed leader's cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the joining caller never returned")
	}
}

// A panic inside the listing fetch is abandoned, not fatal.
//
// Moving the fetch to DoChan made this urgent and silent: singleflight re-raises a panic from the flight
// with `go panic(e)` on a fresh goroutine whenever anyone is waiting on a channel, and nothing can recover
// it there. Under the blocking Do the panic unwound into the caller and the handler's own recover turned
// it into a 500. So a change made for context handling quietly converted a recoverable panic into a dead
// container — every viewer losing playback because one account listing tripped a parser.
func TestAccountListing_aPanicInTheFetchIsAbandonedNotFatal(t *testing.T) {
	before := metrics.backgroundPanic.Load()
	s := &torBoxStore{token: "panics", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{func(*http.Request) (*http.Response, error) {
			panic("a parser went off the end")
		}}}

	// Two callers only to exercise the shared-result path; ONE is already enough to make the panic fatal.
	// DoChan registers a channel for the first caller too — `c := &call{chans: []chan<- Result{ch}}` — so
	// the re-raise on a bare goroutine happens from the very first call, and there is no arrival count
	// that avoids it. That is why the recover has to live inside the closure rather than around whatever
	// starts it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		go func() {
			ids, ok := s.accountListing(ctx)
			done <- ok || ids != nil
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case answered := <-done:
			if answered {
				t.Error("a panicking fetch reported an answer")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a caller never returned from a panicking fetch")
		}
	}
	if got := metrics.backgroundPanic.Load(); got <= before {
		t.Errorf("the panic was not counted (%d → %d) — an operator has nothing to alert on", before, got)
	}
}

// StatusAnswer's doubt comes from the lookup it already did, not from a second one.
//
// The three-valued answer was first built by re-running findTorrentByHash purely to learn whether the
// listing had been readable. That call goes straight to accountListing and so steps over torrentMissKey,
// whose entire purpose is that a remembered miss costs no fetch — measured at four listing requests per
// /play against a throttled account where one was enough, on the exact path that is already refusing.
func TestTorBoxStatusAnswer_takesItsDoubtFromTheLookupItAlreadyDid(t *testing.T) {
	for _, tc := range []struct {
		name string
		// The listing reply, which decides whether the miss is authoritative.
		listing    func() *http.Response
		wantAnswer statusAnswer
		// Fetches over two polls: the first reads the listing, the second must read nothing — either
		// because the miss was remembered or because the oversize/failure path decided for itself.
		wantFetches int
	}{
		// Read in full, hash absent: an authoritative no, remembered, so the second poll costs nothing.
		{"an empty account is a definitive no", func() *http.Response {
			return resp(200, `{"success":true,"data":[]}`)
		}, statusNo, 1},
		// The list was never read, so "not in it" is not a fact about the account. The miss must NOT be
		// remembered here — that retry is the only thing that rediscovers a queued torrent — so the
		// second poll does fetch again, and neither poll may fetch twice.
		{"an unreadable listing is doubt, not a no", func() *http.Response {
			return resp(503, "down")
		}, statusUnknown, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetches := 0
			s := &torBoxStore{token: "t", api: torboxAPI, cache: NewMemoryCache(1 << 20),
				client: mockDoer{func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "mylist") && r.URL.Query().Get("id") == "" {
						fetches++
						return tc.listing(), nil
					}
					t.Errorf("unexpected request: %s", r.URL)
					return resp(404, "{}"), nil
				}}}
			for i := 0; i < 2; i++ {
				if _, answer := s.StatusAnswer(context.Background(), ResolveTarget{InfoHash: H}); answer != tc.wantAnswer {
					t.Fatalf("poll %d answered %v, want %v", i, answer, tc.wantAnswer)
				}
			}
			if fetches != tc.wantFetches {
				t.Errorf("listed the account %d times over two polls, want %d", fetches, tc.wantFetches)
			}
		})
	}
}

// The memo is bounded by TORRENTS held, not by accounts, and an expired entry stops being resident.
//
// Both shipped with no coverage. An account ceiling is what byte budgets exist to replace — 32 accounts
// times maxListingEntries admits 127 MiB, over half of GOMEMLIMIT and outside the cache budget that used
// to bound this — and expiry that only ran inside a put left a single-account install holding a stale
// listing forever, because nothing else was ever fetched to trigger the sweep.
func TestListingMemo_isBoundedByTorrentsAndDropsExpiredEntries(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	t.Cleanup(func() {
		listingMemoMu.Lock()
		listingMemo = map[listingMemoKey]listingMemoEntry{}
		listingMemoMu.Unlock()
	})

	// Each account holds a tenth of the ceiling, so fifteen of them cannot all fit.
	const per = maxListingMemoEntries / 10
	ids := func(n int) map[string]int {
		m := make(map[string]int, n)
		for i := 0; i < n; i++ {
			m[fmt.Sprintf("%040x", i)] = i
		}
		return m
	}
	for i := 0; i < 15; i++ {
		putCachedListing(cache, fmt.Sprintf("acct-%d", i), ids(per))
	}

	listingMemoMu.Lock()
	total, accounts := 0, len(listingMemo)
	for _, e := range listingMemo {
		total += len(e.ids)
	}
	listingMemoMu.Unlock()
	if total > maxListingMemoEntries {
		t.Errorf("the memo holds %d torrents across %d accounts, ceiling is %d",
			total, accounts, maxListingMemoEntries)
	}
	// And it is actually full, or the flood never reached the ceiling and this asserts nothing.
	if total < maxListingMemoEntries-per {
		t.Errorf("the memo holds only %d torrents — the flood never reached the %d ceiling",
			total, maxListingMemoEntries)
	}

	// REFRESHING an account evicts nobody, because the entry it replaces stops counting first. Counting
	// it twice — once in the running total and again as the incoming listing — makes a listing that fits
	// perfectly well look like an overflow, and evicts a different account on every 15 s refresh. Each
	// eviction is a full re-pull of that account's listing, ~13 MB at 2,000 torrents, on the poll path.
	listingMemoMu.Lock()
	listingMemo = map[listingMemoKey]listingMemoEntry{}
	listingMemoMu.Unlock()
	const half = maxListingMemoEntries / 2
	putCachedListing(cache, "a", ids(half))
	putCachedListing(cache, "b", ids(half))
	for i := 0; i < 3; i++ {
		putCachedListing(cache, "a", ids(half)) // a's own TTL refresh
		listingMemoMu.Lock()
		_, bKept := listingMemo[listingMemoKey{cache, "b"}]
		listingMemoMu.Unlock()
		if !bKept {
			t.Fatalf("refresh %d of account a evicted account b, which still fits", i)
		}
	}

	// An expired entry is dropped on LOOKUP, not left for the next account's put.
	listingMemoMu.Lock()
	listingMemo = map[listingMemoKey]listingMemoEntry{
		{cache, "stale"}: {ids: ids(3), at: time.Now().Add(-listingTTL - time.Second)},
	}
	listingMemoMu.Unlock()
	if _, hit := cachedListing(cache, "stale"); hit {
		t.Error("an expired listing was served")
	}
	listingMemoMu.Lock()
	left := len(listingMemo)
	listingMemoMu.Unlock()
	if left != 0 {
		t.Errorf("the expired entry is still resident (%d left) — on a single-account install nothing "+
			"else is ever fetched to sweep it", left)
	}
}

// uncomparableCache is a Cache that cannot be a map key. Cache is exported and Deps.Cache takes any
// implementation, so this is a shape a caller outside this package can genuinely supply.
type uncomparableCache struct {
	Cache
	_ []int // a slice field makes the struct uncomparable; a value receiver puts it in the interface
}

func (c uncomparableCache) Get(string) (string, bool)         { return "", false }
func (c uncomparableCache) Put(string, string, time.Duration) {}

// optionCache is comparable as a TYPE — an interface field satisfies Comparable() — and panics as a
// VALUE, because the field holds a slice. A static check passes it straight into the map.
type optionCache struct {
	Cache
	opts any
}

func (c optionCache) Get(string) (string, bool)         { return "", false }
func (c optionCache) Put(string, string, time.Duration) {}

// A Cache that cannot be a map key gets no memo, rather than panicking inside a request handler.
//
// Both shapes, because they fail different checks. The uncomparable TYPE is caught statically; the
// comparable type holding an uncomparable VALUE is not, and a static check alone waves it through into
// the map insert. Cache is exported and Deps.Cache takes any implementation, so both are shapes a caller
// outside this package can supply.
func TestListingMemo_survivesAnUncomparableCache(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache Cache
	}{
		{"an uncomparable type", uncomparableCache{}},
		{"a comparable type holding an uncomparable value", optionCache{opts: []string{"a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if memoisable(tc.cache) {
				t.Fatal("the fixture is memoisable, so this asserts nothing")
			}
			// Both halves, because either one panics on its own.
			if _, hit := cachedListing(tc.cache, "k"); hit {
				t.Error("an unmemoisable cache reported a hit")
			}
			putCachedListing(tc.cache, "k", map[string]int{H: 1})
		})
	}
	// A comparable value in the same field is memoisable — or the guard is just refusing everything.
	if !memoisable(optionCache{opts: "a string is fine"}) {
		t.Error("a comparable Cache was refused the memo")
	}

	// And a store built on an unmemoisable cache still works, just without the memo.
	var cache Cache = uncomparableCache{}
	fetches := 0
	s := &torBoxStore{token: "uncomparable", api: torboxAPI, cache: cache,
		client: mockDoer{func(*http.Request) (*http.Response, error) {
			fetches++
			return resp(200, `{"success":true,"data":[{"id":4,"hash":"`+H+`"}]}`), nil
		}}}
	for i := 0; i < 2; i++ {
		if ids, ok := s.accountListing(context.Background()); !ok || ids[H] != 4 {
			t.Fatalf("read %d: ok=%v ids=%v", i, ok, ids)
		}
	}
	if fetches != 2 {
		t.Errorf("fetched %d times; without a memo every read is its own fetch", fetches)
	}
}

// A memo HIT hands every caller the same parsed map, rather than re-parsing the listing per caller.
//
// The memo used to be JSON in the shared Cache, so each hit re-ran json.Unmarshal and produced a private
// copy: 14 ms and 9.1 MB per hit on an account near maxListingEntries, and 26.5 MB alive at once across
// the eight concurrent probes a poster grid makes. maxListingEntries reads as though it bounded what the
// listing costs, and it did not — the real quantity was entries times concurrent callers.
func TestAccountListing_memoHitsShareOneParsedMap(t *testing.T) {
	const entries = 2000
	var body strings.Builder
	body.WriteString(`{"success":true,"data":[`)
	for i := 0; i < entries; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":%d,"hash":"%040x"}`, i, i)
	}
	body.WriteString(`]}`)

	fetches := 0
	s := &torBoxStore{token: "memo", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{func(*http.Request) (*http.Response, error) {
			fetches++
			return resp(200, body.String()), nil
		}}}

	first, ok := s.accountListing(context.Background())
	if !ok || len(first) != entries {
		t.Fatalf("first read: ok=%v entries=%d", ok, len(first))
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const hits = 8
	got := make([]map[string]int, hits)
	for i := range got {
		ids, ok := s.accountListing(context.Background())
		if !ok {
			t.Fatalf("memo hit %d missed", i)
		}
		got[i] = ids
	}
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(got)

	if fetches != 1 {
		t.Errorf("fetched the listing %d times for nine reads", fetches)
	}
	// Same map, not an equal one: a private copy per caller is the cost this exists to remove, and two
	// maps with the same contents would satisfy any assertion phrased on contents. Compared by the
	// underlying pointer rather than by writing a probe key through one of them — production shares this
	// map read-only, and a test that mutates it models the one thing callers must never do.
	for i, ids := range got {
		if len(ids) != len(first) {
			t.Fatalf("memo hit %d returned %d entries, want %d", i, len(ids), len(first))
		}
		if reflect.ValueOf(ids).Pointer() != reflect.ValueOf(first).Pointer() {
			t.Errorf("memo hit %d returned a private copy of the listing, not the shared map", i)
		}
	}
	// Eight hits on a 2,000-entry account allocated ~445 KB each through the old path, so anything in
	// that range means the parse is back. A shared map costs a handful of words per hit.
	if allocated := int64(after.TotalAlloc) - int64(before.TotalAlloc); allocated > 64<<10 {
		t.Errorf("%d memo hits allocated %d bytes — the listing is being re-parsed per caller",
			hits, allocated)
	}
}

// An account whose listing is too big to read is not re-pulled on every attempt — but a TRANSIENT
// failure still is, because that retry is the only thing that rediscovers a queued torrent.
//
// Only successes were memoised, so an oversized account re-pulled the whole body every time: a /play
// makes up to three status reads and a client polls it every two seconds, which is tens of megabytes of
// egress per request to reach the same answer each time. Oversize is a property of the account, not a
// blip, so it is the one failure worth remembering.
func TestAccountListing_remembersOversizedButRetriesTransient(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"success":true,"data":[`)
	for i := 0; body.Len() < maxListingBytes+(1<<20); i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":%d,"hash":"%040x","files":"%s"}`, i, i, repeat("f", 4000))
	}
	body.WriteString(`]}`)

	fetches := 0
	oversized := &torBoxStore{token: "t", api: torboxAPI, cache: NewMemoryCache(4 << 20),
		client: mockDoer{func(*http.Request) (*http.Response, error) {
			fetches++
			return resp(200, body.String()), nil
		}}}
	for i := 0; i < 3; i++ {
		if ids, ok := oversized.accountListing(context.Background()); ok || ids != nil {
			t.Fatal("a truncated listing must not read as an answer")
		}
	}
	if fetches != 1 {
		t.Errorf("pulled the oversized listing %d times for three attempts — it is not being remembered", fetches)
	}

	// The ENTRY cap takes the same road, and had to be given it separately: it trips before the body is
	// drained, so the byte counter never reaches the cap, truncated() stays false, and the oversize memo
	// was never written. That left the entry cap re-pulling the whole listing on every poll — the exact
	// egress the memo above exists to stop — and, because the read now yields "could not find out",
	// /play escalated and did it twice per request.
	var compact strings.Builder
	compact.WriteString(`{"success":true,"data":[`)
	for i := 0; i <= maxListingEntries; i++ {
		if i > 0 {
			compact.WriteString(",")
		}
		fmt.Fprintf(&compact, `{"id":%d,"hash":"%040x"}`, i, i)
	}
	compact.WriteString(`]}`)
	if compact.Len() >= maxListingBytes {
		t.Fatalf("the fixture is %d bytes, past the %d byte cap — it would assert the wrong cap",
			compact.Len(), maxListingBytes)
	}

	entryFetches := 0
	tooMany := &torBoxStore{token: "t3", api: torboxAPI, cache: NewMemoryCache(4 << 20),
		client: mockDoer{func(*http.Request) (*http.Response, error) {
			entryFetches++
			return resp(200, compact.String()), nil
		}}}
	for i := 0; i < 3; i++ {
		if ids, ok := tooMany.accountListing(context.Background()); ok || ids != nil {
			t.Fatal("a listing past the entry cap must not read as an answer")
		}
	}
	if entryFetches != 1 {
		t.Errorf("pulled the over-entry-count listing %d times for three attempts — it is not being remembered",
			entryFetches)
	}

	// A transient failure must NOT be remembered: suppressing that retry is how a queued torrent stops
	// being rediscoverable, which this package has a separate test for.
	transientFetches := 0
	flaky := &torBoxStore{token: "t2", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{func(*http.Request) (*http.Response, error) {
			transientFetches++
			return resp(503, "down"), nil
		}}}
	for i := 0; i < 3; i++ {
		flaky.accountListing(context.Background())
	}
	if transientFetches != 3 {
		t.Errorf("a transient listing failure was memoised (%d fetches for three attempts) — a queued "+
			"torrent would stop being rediscoverable", transientFetches)
	}
}

// A body of EXACTLY the cap is not truncated, which is the case the limit+1 read exists for.
//
// The detector is fed a LimitReader of limit+1 so that "read the cap exactly" and "cut off at the cap"
// produce different byte counts. With the comparison at >= instead of >, a listing that landed precisely
// on maxListingBytes parsed perfectly and was then discarded AND remembered as oversized for the listing
// TTL — every poll for the next fifteen seconds answering "no idea" about an account that had just been
// read in full. Nothing covered it: the existing truncation case is a body far past the cap, which reads
// the same under either comparison.
func TestTruncationDetector_aBodyOfExactlyTheCapIsWhole(t *testing.T) {
	const limit = 1 << 10
	for _, tc := range []struct {
		name string
		size int
		want bool
	}{
		{"one short", limit - 1, false},
		{"exactly the cap", limit, false},
		{"one over", limit + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Constructed exactly as fetchAccountListing does it.
			td := &truncationDetector{r: io.LimitReader(strings.NewReader(repeat("x", tc.size)), limit+1), limit: limit}
			n, err := io.Copy(io.Discard, td)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got := td.truncated(); got != tc.want {
				t.Errorf("a %d-byte body under a %d-byte cap read %d bytes and reported truncated=%v, want %v",
					tc.size, limit, n, got, tc.want)
			}
		})
	}
}

// The listing decode must not RETAIN the body — that property, not just "it parses", is what keeps a
// 64 MiB cap safe inside a 230 MiB budget.
//
// TWO fixtures, because the decode has two places it could buffer and each one needs the bulk to be
// somewhere the other does not look. Bulk inside `data[]` is discarded by the element decode, so it
// cannot see a skipped top-level field being materialised; bulk in a top-level field never enters the
// element walk, so it cannot see that walk replaced by a single Decode into a slice. A previous version
// of this test had only the second fixture and went green while `data[]` was buffered whole.
//
// Each shape gets its own ceiling, from measurement rather than a shared round number: the numbers are
// far enough apart that a coarse ceiling is plenty and there is no need to measure precisely enough to
// be flaky.
func TestDecodeListing_doesNotRetainTheBody(t *testing.T) {
	// A top-level key that is neither success nor data goes through skipValue's default branch. Its walk
	// allocates about one body's worth, because Token materialises each string it steps over; buffering
	// the same field into a json.RawMessage allocates 3.7x.
	var skipped strings.Builder
	skipped.WriteString(`{"success":true,"unknown_field":[`)
	for i := 0; skipped.Len() < 24<<20; i++ {
		if i > 0 {
			skipped.WriteString(",")
		}
		fmt.Fprintf(&skipped, `{"noise":"%s"}`, repeat("f", 6000))
	}
	skipped.WriteString(`],"data":[{"id":1,"hash":"` + repeat("a", 40) + `"}]}`)

	// The `data[]` walk keeps only hash→id, so its bulk costs almost nothing — 0.03x. The gap to any
	// buffering form is wide enough that the ceiling can sit well under the body size, which is what makes
	// this shape the sharper of the two.
	//
	// 0.5x rather than something nearer the measured 0.03x, because a ceiling has to sit under the
	// CHEAPEST regression rather than the loudest, and the buffering forms are not all alike. Capturing
	// the whole array at once — into []struct or []json.RawMessage — measures between 2.7x and 3.7x
	// depending on how the bytes are captured. The cheapest form is per-element: decode each entry into a
	// json.RawMessage and unmarshal them afterwards, which retains the raw bytes without ever holding the
	// array, and measures 1.08x. 0.5 clears that by half and still sits 16x above the shipped walk.
	var inData strings.Builder
	inData.WriteString(`{"success":true,"data":[`)
	for i := 0; inData.Len() < 24<<20; i++ {
		if i > 0 {
			inData.WriteString(",")
		}
		fmt.Fprintf(&inData, `{"id":%d,"hash":"%040x","files":"%s"}`, i, i, repeat("f", 6000))
	}
	inData.WriteString(`]}`)

	for _, tc := range []struct {
		name string
		body string
		// Ceiling as a fraction of the body, x1000 to stay in integers.
		ceilingPerMille int64
	}{
		{"bulk in a skipped top-level field", skipped.String(), 2000},
		{"bulk inside the data array", inData.String(), 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// TotalAlloc, not HeapAlloc: the buffering decode's cost is a transient PEAK, and it is
			// unreachable by the time a post-GC HeapAlloc reading is taken — an earlier version of this
			// test measured residency and passed against every implementation it forbids, including the
			// RawMessage skip. What GOMEMLIMIT actually sees is the volume allocated while it runs.
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			ids, ok, _ := decodeListing(json.NewDecoder(strings.NewReader(tc.body)))
			runtime.ReadMemStats(&after)
			if !ok {
				t.Fatal("the fixture did not decode")
			}
			runtime.KeepAlive(ids)

			allocated := int64(after.TotalAlloc) - int64(before.TotalAlloc)
			ceiling := int64(len(tc.body)) * tc.ceilingPerMille / 1000
			if allocated > ceiling {
				t.Errorf("decoding a %d-byte listing allocated %d bytes, ceiling %d — the body is being "+
					"buffered, not streamed", len(tc.body), allocated, ceiling)
			}
		})
	}
}

// The listing map is bounded by ENTRY COUNT as well as by bytes: the byte cap admits ~1M minimal entries,
// which measured 346 MiB of retained map against a 230 MiB GOMEMLIMIT.
func TestDecodeListing_boundsEntryCount(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"success":true,"data":[`)
	for i := 0; i <= maxListingEntries; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":%d,"hash":"%040x"}`, i, i)
	}
	body.WriteString(`]}`)

	// tooMany must be reported separately, because that is what makes the caller remember the account as
	// oversized instead of re-pulling and re-discarding the whole listing on every poll.
	if _, ok, tooMany := decodeListing(json.NewDecoder(strings.NewReader(body.String()))); ok || !tooMany {
		t.Errorf("a listing with more than %d entries: ok=%v tooMany=%v (want false,true)", maxListingEntries, ok, tooMany)
	}
}
