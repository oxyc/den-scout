package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
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
}

func (a answeringStore) StatusAnswer(context.Context, ResolveTarget) (StoreStatus, statusAnswer) {
	if a.asked != nil {
		*a.asked++
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
// Nothing asserted it: replacing the element-by-element walk with the old Decode-into-slice left the
// whole suite green, and that buffering version is what peaked at 2.1x the body. The margin here is
// enormous (measured ~1 MiB against ~129 MiB on 40 MiB of JSON), so a coarse ceiling is plenty and there
// is no need to measure precisely enough to be flaky.
func TestDecodeListing_doesNotRetainTheBody(t *testing.T) {
	// The bulk sits in a TOP-LEVEL key that is neither success nor data, so it goes through skipValue's
	// default branch — which is the code under test. An earlier fixture put the bulk inside each array
	// element, where it is discarded by the element decode instead, so the skip branch never ran at all
	// and the test could not tell the streaming walk from the buffering decode it replaced.
	var body strings.Builder
	body.WriteString(`{"success":true,"unknown_field":[`)
	for i := 0; body.Len() < 24<<20; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"noise":"%s"}`, repeat("f", 6000))
	}
	body.WriteString(`],"data":[{"id":1,"hash":"` + repeat("a", 40) + `"}]}`)

	// TotalAlloc, not HeapAlloc: the buffering decode's cost is a transient PEAK, and it is unreachable
	// by the time a post-GC HeapAlloc reading is taken — an earlier version of this test measured
	// residency and passed against every implementation it forbids, including the RawMessage skip. What
	// GOMEMLIMIT actually sees is the volume allocated while the decode runs.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ids, ok, _ := decodeListing(json.NewDecoder(strings.NewReader(body.String())))
	runtime.ReadMemStats(&after)
	if !ok {
		t.Fatal("the fixture did not decode")
	}
	runtime.KeepAlive(ids)

	allocated := int64(after.TotalAlloc) - int64(before.TotalAlloc)
	// Measured on this fixture: the streaming walk allocates 1.06x the body (the decoder's own read
	// buffer, which it grows geometrically and reuses), buffering the skipped field into a
	// json.RawMessage allocates 3.7x. Two is the only number between them that is not a coincidence.
	if allocated > 2*int64(body.Len()) {
		t.Errorf("decoding a %d-byte listing allocated %d bytes — the body is being buffered, not streamed",
			body.Len(), allocated)
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
