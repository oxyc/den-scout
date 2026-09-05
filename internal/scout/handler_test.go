package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var validBlob = blob(`{"debrid":[{"service":"torbox","token":"tb-secret"}],"indexers":["torrentio"],"filters":{"excludeCam":true},"cachedOnly":true,"resultCap":20}`)

func testSeeds() []RawStream {
	return []RawStream{
		{InfoHash: repeat("a", 40), Title: "Movie 2160p WEB-DL HDR", SizeBytes: intp(18 * gib), Seeders: intp(100), FileIdx: intp(0)},
		{InfoHash: repeat("b", 40), Title: "Movie 1080p WEB-DL", SizeBytes: intp(8 * gib), Seeders: intp(50), FileIdx: intp(0)},
		{InfoHash: repeat("c", 40), Title: "Movie 2160p HDCAM", SizeBytes: intp(2 * gib), Seeders: intp(3)},
	}
}

func testDeps(over func(*Deps)) Deps {
	d := Deps{
		Cache:         NewMemoryCache(1 << 20),
		ScrapeTimeout: time.Second,
		ListTTL:       5 * time.Minute,
		MakeScrapers: func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) { return testSeeds(), nil }}}
		},
		MakeStores: func(*Config) []Store {
			return []Store{fakeStore{
				svc:     ServiceTorBox,
				check:   map[string]bool{repeat("a", 40): true, repeat("b", 40): true, repeat("c", 40): false},
				resolve: func() (string, error) { return "https://cdn.torbox/" + repeat("a", 40) + ".mkv", nil },
			}}
		},
	}
	if over != nil {
		over(&d)
	}
	return d
}

func do(h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	return doMethod(h, http.MethodGet, path, headers)
}

func postJSON(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "https://scout.example"+path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func doMethod(h http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://scout.example"+path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRoutesPagesAndManifest(t *testing.T) {
	h := NewHandler(testDeps(nil))
	if rr := do(h, "/configure", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), "Den Scout") || !strings.Contains(rr.Body.String(), "DenSeal") || rr.Header().Get("cache-control") != staticCache {
		t.Errorf("configure: %d cc=%q", rr.Code, rr.Header().Get("cache-control"))
	}
	if rr := do(h, "/health", nil); rr.Code != 200 || rr.Header().Get("cache-control") != noStore {
		t.Errorf("health: %d cc=%q", rr.Code, rr.Header().Get("cache-control"))
	}
	if rr := do(h, "/manifest.json", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"configurationRequired":true`) {
		t.Errorf("unconfigured manifest: %s", rr.Body.String())
	}
	if rr := do(h, "/"+validBlob+"/manifest.json", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"configurationRequired":false`) {
		t.Errorf("configured manifest: %s", rr.Body.String())
	}
	if rr := do(h, "/@@@/manifest.json", nil); rr.Code != 400 {
		t.Errorf("bad config: %d", rr.Code)
	}
	if rr := do(h, "/"+validBlob+"/bogus", nil); rr.Code != 404 {
		t.Errorf("unknown resource: %d", rr.Code)
	}
}

func TestRoutesSealedConfig(t *testing.T) {
	kr, _ := parseSealKeyring(vecPrivB64, "")
	// Same config as validBlob, but sealed to the addon's key.
	cfgJSON := []byte(`{"debrid":[{"service":"torbox","token":"tb-secret"}],"indexers":["torrentio"],"filters":{"excludeCam":true},"cachedOnly":true,"resultCap":20}`)
	sealed, _ := seal(&kr.keys[0].pub, cfgJSON)
	seg := b64urlEncode(append([]byte{sealedVersion}, sealed...))
	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr }))

	// /config-key serves the current public key so a client can seal to it.
	if rr := do(h, "/config-key", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), vecPubB64) {
		t.Errorf("config-key: %d %s", rr.Code, rr.Body.String())
	}
	// A SEALED URL resolves manifest + streams identically to the plaintext blob.
	if rr := do(h, "/"+seg+"/manifest.json", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"configurationRequired":false`) {
		t.Errorf("sealed manifest: %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(h, "/"+seg+"/stream/movie/tt1234567.json", nil); rr.Code != 200 || streamsLen(rr) != 2 {
		t.Errorf("sealed stream: %d n=%d", rr.Code, streamsLen(rr))
	}
	// Fail CLOSED: a sealed URL against a handler with no keyring → 400; /config-key → 404.
	noKey := NewHandler(testDeps(nil))
	if rr := do(noKey, "/"+seg+"/manifest.json", nil); rr.Code != 400 {
		t.Errorf("sealed with no keyring should 400: %d", rr.Code)
	}
	if rr := do(noKey, "/config-key", nil); rr.Code != 404 {
		t.Errorf("config-key without keyring should 404: %d", rr.Code)
	}
	// Back-compat: legacy plaintext still resolves when a keyring IS configured.
	if rr := do(h, "/"+validBlob+"/manifest.json", nil); rr.Code != 200 {
		t.Errorf("legacy plaintext with keyring: %d", rr.Code)
	}
}

func TestRoutesStream(t *testing.T) {
	h := NewHandler(testDeps(nil))
	rr := do(h, "/"+validBlob+"/stream/movie/tt1234567.json", nil)
	if rr.Code != 200 {
		t.Fatalf("stream: %d", rr.Code)
	}
	if !strings.HasPrefix(rr.Header().Get("cache-control"), "public, max-age=300, stale-while-revalidate=300") {
		t.Errorf("stream cache-control: %q", rr.Header().Get("cache-control"))
	}
	var body struct {
		Streams []struct {
			Name          string           `json:"name"`
			Title         string           `json:"title"`
			URL           string           `json:"url"`
			Attributes    StreamAttributes `json:"attributes"`
			BehaviorHints struct {
				BingeGroup string `json:"bingeGroup"`
			} `json:"behaviorHints"`
		} `json:"streams"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.Streams) != 2 { // CAM dropped, cached only, 4K then 1080p
		t.Fatalf("want 2 streams, got %d", len(body.Streams))
	}
	first := body.Streams[0]
	if first.Name != "Den Scout" || first.Title != "Movie 2160p WEB-DL HDR" || first.BehaviorHints.BingeGroup != "den-scout-tt1234567" {
		t.Errorf("first stream: %+v", first)
	}
	if !strings.Contains(first.URL, "/"+validBlob+"/play/") || strings.Contains(first.URL, "tb-secret") {
		t.Errorf("play url: %s", first.URL)
	}
	tok := first.URL[strings.LastIndex(first.URL, "/play/")+len("/play/"):]
	if pt, ok := decodePlayToken(tok); !ok || pt.InfoHash != repeat("a", 40) {
		t.Errorf("play token: %v ok=%v", pt, ok)
	}
	if first.Attributes.Resolution == nil || *first.Attributes.Resolution != "2160p" {
		t.Errorf("attributes: %+v", first.Attributes)
	}
	if rr := do(h, "/"+validBlob+"/stream/movie/nope.json", nil); rr.Code != 400 {
		t.Errorf("bad id: %d", rr.Code)
	}
}

// The handler dispatched on PATH alone, so /play answered any verb — and /play resolves, which ADDS an
// uncached release against a fifty-an-hour allowance. A link unfurler's HEAD or a crawler's POST reached
// the one route this package works hardest to keep accidental callers out of.
func TestRoutesRejectNonReadMethods(t *testing.T) {
	resolves := 0
	h := NewHandler(testDeps(func(d *Deps) {
		inner := d.MakeStores
		d.MakeStores = func(c *Config) []Store {
			stores := inner(c)
			return []Store{countingStore{Store: stores[0], resolved: &resolves}}
		}
	}))

	// Get a real play token the honest way, so the route under test is the one clients actually hit.
	rr := do(h, "/"+validBlob+"/stream/movie/tt1234567.json", nil)
	var body struct {
		Streams []struct {
			URL string `json:"url"`
		} `json:"streams"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.Streams) == 0 {
		t.Fatal("no streams to build a play URL from")
	}
	playURL := body.Streams[0].URL
	playPath := playURL[strings.Index(playURL, "/"+validBlob):]

	// GET is the one verb that may resolve.
	if got := do(h, playPath, nil); got.Code != 302 {
		t.Fatalf("GET /play: %d, want 302", got.Code)
	}
	if resolves != 1 {
		t.Fatalf("GET /play resolved %d times, want 1", resolves)
	}

	// HEAD is refused, not answered: what a caller wants here is a `location`, and a HEAD that starts
	// nothing cannot produce one — so answering it would spend upstream reads for a useless reply.
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete} {
		got := doMethod(h, method, playPath, nil)
		if got.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /play: %d, want 405", method, got.Code)
		}
		if got.Header().Get("allow") == "" {
			t.Errorf("%s /play: 405 without an Allow header", method)
		}
	}
	if resolves != 1 {
		t.Fatalf("a non-GET verb reached the debrid: %d resolves, want 1", resolves)
	}

	// Every other route is a plain read: POST is refused, HEAD is served.
	for _, path := range []string{"/", "/configure", "/health", "/manifest.json",
		"/" + validBlob + "/manifest.json", "/" + validBlob + "/stream/movie/tt1234567.json"} {
		if got := doMethod(h, http.MethodPost, path, nil); got.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d, want 405", path, got.Code)
		}
		if got := doMethod(h, http.MethodHead, path, nil); got.Code != 200 {
			t.Errorf("HEAD %s: %d, want 200", path, got.Code)
		}
	}
}

// countingStore counts resolves so a test can prove a route never reached the debrid.
type countingStore struct {
	Store
	resolved *int
}

func (c countingStore) Resolve(ctx context.Context, rt ResolveTarget) (string, error) {
	*c.resolved++
	return c.Store.Resolve(ctx, rt)
}

func TestRoutesETagAndSingleflight(t *testing.T) {
	h := NewHandler(testDeps(nil))
	first := do(h, "/"+validBlob+"/manifest.json", nil)
	etag := first.Header().Get("etag")
	if etag == "" {
		t.Fatal("no etag")
	}
	second := do(h, "/"+validBlob+"/manifest.json", map[string]string{"If-None-Match": etag})
	if second.Code != 304 || second.Body.Len() != 0 {
		t.Errorf("expected 304 empty, got %d len=%d", second.Code, second.Body.Len())
	}

	// singleflight: concurrent misses for the same title share one scrape.
	var count int32
	h2 := NewHandler(testDeps(func(d *Deps) {
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				atomic.AddInt32(&count, 1)
				time.Sleep(25 * time.Millisecond)
				return testSeeds(), nil
			}}}
		}
	}))
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); do(h2, "/"+validBlob+"/stream/movie/tt777.json", nil) }()
	}
	wg.Wait()
	if count != 1 {
		t.Errorf("singleflight: scrape ran %d times, want 1", count)
	}
}

func TestRoutesPlay(t *testing.T) {
	h := NewHandler(testDeps(nil))
	streamRR := do(h, "/"+validBlob+"/stream/movie/tt1.json", nil)
	var body struct {
		Streams []struct {
			URL string `json:"url"`
		} `json:"streams"`
	}
	_ = json.Unmarshal(streamRR.Body.Bytes(), &body)
	playPath := body.Streams[0].URL[strings.Index(body.Streams[0].URL, "/"+validBlob):]

	rr := do(h, playPath, nil)
	if rr.Code != 302 || rr.Header().Get("location") != "https://cdn.torbox/"+repeat("a", 40)+".mkv" {
		t.Errorf("play 302: %d loc=%q", rr.Code, rr.Header().Get("location"))
	}
	if rr.Header().Get("content-length") != "0" || rr.Header().Get("cache-control") != noStore || rr.Header().Get("content-type") != "" {
		t.Errorf("play 302 headers: cl=%q cc=%q ct=%q", rr.Header().Get("content-length"), rr.Header().Get("cache-control"), rr.Header().Get("content-type"))
	}
	if rr := do(h, "/"+validBlob+"/play/@@@", nil); rr.Code != 400 {
		t.Errorf("bad token: %d", rr.Code)
	}
	dead := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store {
			return []Store{fakeStore{svc: ServiceTorBox, resolve: func() (string, error) { return "", &DeadLinkError{"x"} }}}
		}
	}))
	if rr := do(dead, playPath, nil); rr.Code != 404 {
		t.Errorf("dead link: %d", rr.Code)
	}

	// Queued is NOT dead. An uncached release resolves only after the debrid has fetched it; answering
	// 404 in the meantime made the client blacklist the release (and every uncached one ranked below it,
	// since they all fail identically). 202 + progress lets the client wait instead.
	eta := 420
	queued := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store {
			return []Store{fakeStore{
				svc:     ServiceTorBox,
				resolve: func() (string, error) { return "", &DeadLinkError{"not ready"} },
				status:  &StoreStatus{Progress: 0.34, ETASeconds: &eta},
			}}
		}
	}))
	rr = do(queued, playPath, nil)
	if rr.Code != 202 {
		t.Fatalf("queued release should be 202, got %d", rr.Code)
	}
	var queuedBody struct {
		State      string  `json:"state"`
		Progress   float64 `json:"progress"`
		ETASeconds *int    `json:"etaSeconds"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &queuedBody) != nil || queuedBody.State != "downloading" ||
		queuedBody.Progress != 0.34 {
		t.Errorf("queued body: %s", rr.Body.String())
	}
	if queuedBody.ETASeconds == nil || *queuedBody.ETASeconds != 420 {
		t.Errorf("eta should be passed through only when the store reports one: %v", queuedBody.ETASeconds)
	}

	// A known-queued release answers from the status alone. A client polls this URL for the whole fetch,
	// so re-resolving per poll (~3 upstream calls) is how the account gets itself throttled.
	resolves := 0
	polled := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store {
			return []Store{fakeStore{
				svc: ServiceTorBox,
				resolve: func() (string, error) {
					resolves++
					return "", &DeadLinkError{"not ready"}
				},
				status: &StoreStatus{Progress: 0.5},
			}}
		}
	}))
	if rr := do(polled, playPath, nil); rr.Code != 202 {
		t.Fatalf("poll should stay 202, got %d", rr.Code)
	}
	if resolves != 0 {
		t.Errorf("a known-queued release re-resolved %d time(s); it should answer from status alone", resolves)
	}
}

func streamsLen(rr *httptest.ResponseRecorder) int {
	var body struct {
		Streams []json.RawMessage `json:"streams"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	return len(body.Streams)
}

func TestHealthDegradedOnScrapeOutage(t *testing.T) {
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) { return nil, context.Canceled }}}
		}
	}))
	if rr := do(h, "/health", nil); !strings.Contains(rr.Body.String(), `"ok"`) {
		t.Fatalf("health should start ok: %s", rr.Body.String())
	}
	for i := 0; i < scrapeFailThreshold; i++ {
		do(h, "/"+validBlob+"/stream/movie/tt"+string(rune('0'+i))+".json", nil) // distinct titles → each builds
	}
	rr := do(h, "/health", nil)
	if rr.Code != 200 {
		t.Errorf("health must stay 200 (liveness), got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "degraded") {
		t.Errorf("health should be degraded after %d scrape failures: %s", scrapeFailThreshold, rr.Body.String())
	}
}

func TestRoutesDegradedScrapeNotCached(t *testing.T) {
	// When every indexer fails, the empty list must NOT be cached — a later healthy request rebuilds.
	var healthy int32
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				if atomic.LoadInt32(&healthy) == 0 {
					return nil, context.Canceled
				}
				return testSeeds(), nil
			}}}
		}
	}))
	degradedRR := do(h, "/"+validBlob+"/stream/movie/tt42.json", nil)
	if n := streamsLen(degradedRR); n != 0 {
		t.Fatalf("degraded scrape should yield 0 streams, got %d", n)
	}
	if got := degradedRR.Header().Get("X-Scout-Degraded"); got != "indexers" {
		t.Errorf("degraded scrape should set X-Scout-Degraded: indexers, got %q", got)
	}
	atomic.StoreInt32(&healthy, 1)
	if n := streamsLen(do(h, "/"+validBlob+"/stream/movie/tt42.json", nil)); n == 0 {
		t.Error("empty degraded list was cached — a later healthy request should rebuild and return streams")
	}
}

func TestRoutesCacheTruthOutageSkipsCachedOnly(t *testing.T) {
	// TorBox present + cachedOnly, but its check errors (outage). The list must not be emptied — the
	// cached-only filter is skipped for the request (streams shown), and the degraded list isn't cached.
	var down int32 = 1
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store {
			if atomic.LoadInt32(&down) == 1 {
				return []Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{}, checkErr: errCheckFailed}}
			}
			return []Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{repeat("a", 40): true, repeat("b", 40): true}}}
		}
	}))
	if n := streamsLen(do(h, "/"+validBlob+"/stream/movie/tt43.json", nil)); n == 0 {
		t.Error("cache-truth outage should skip cachedOnly (show streams), not drop everything")
	}
	// not cached while degraded: once the store recovers, the request reflects real cache truth
	atomic.StoreInt32(&down, 0)
	if n := streamsLen(do(h, "/"+validBlob+"/stream/movie/tt43.json", nil)); n != 2 {
		t.Errorf("after recovery want 2 cached streams (cam dropped), got %d", n)
	}
}

func TestRoutesRDOnlyReturnsStreams(t *testing.T) {
	// audit #4: RD-only + cachedOnly:true would return empty; the fix skips cachedOnly so streams show.
	rdBlob := blob(`{"debrid":[{"service":"realdebrid","token":"rd"}],"indexers":["torrentio"],"filters":{"excludeCam":true},"cachedOnly":true,"resultCap":20}`)
	h := NewHandler(testDeps(func(d *Deps) {
		// RD-safe titles (no web-dl/bdrip/etc. that RD blocks) so the test isolates the cachedOnly skip.
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				return []RawStream{{InfoHash: repeat("a", 40), Title: "Movie 2160p REMUX HDR", SizeBytes: intp(18 * gib)}}, nil
			}}}
		}
		d.MakeStores = func(*Config) []Store {
			return []Store{fakeStore{svc: ServiceRealDebrid, check: map[string]bool{}}} // all-false
		}
	}))
	rr := do(h, "/"+rdBlob+"/stream/movie/tt5.json", nil)
	var body struct {
		Streams []json.RawMessage `json:"streams"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.Streams) == 0 {
		t.Error("RD-only should still return streams (cachedOnly skipped)")
	}
}

// A PARTIAL cache-check failure is degraded, and the response must say so.
//
// `truthOK` only asks whether some store answered about something, so one failed batch out of five left
// it true: the list went out and was CACHED as authoritative with releases nobody had examined, and no
// header said so. The check must be per hash, over what is actually SERVED.
func TestStream_partialCacheCheckIsDegraded(t *testing.T) {
	// The store answers for one hash and omits the other — what a half-failed batch looks like.
	answered := repeat("a", 40)
	notHeld := repeat("b", 40)
	unknown := repeat("c", 40)
	h := NewHandler(Deps{
		Cache:         NewMemoryCache(1 << 20),
		ScrapeTimeout: time.Second,
		MakeScrapers: func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				return []RawStream{
					{InfoHash: answered, Title: "A 1080p WEB-DL", Seeders: intp(10)},
					{InfoHash: notHeld, Title: "B 1080p WEB-DL", Seeders: intp(10)},
					{InfoHash: unknown, Title: "C 1080p WEB-DL", Seeders: intp(10)},
				}, nil
			}}}
		},
		MakeStores: func(*Config) []Store {
			// Answered for two of three: one held, one definitively NOT held, one its batch never covered.
			// All three are needed — with only "held" and "unchecked", keeping the filter per-hash and
			// switching it off entirely serve the same two streams, so the test cannot tell them apart.
			return []Store{fakeStore{svc: ServiceTorBox,
				check: map[string]bool{answered: true, notHeld: false}}}
		},
	})
	rec := do(h, "/"+validBlob+"/stream/movie/tt1234567.json", nil)

	if got := rec.Header().Get("X-Scout-Degraded"); got != "cache-check" {
		t.Errorf("X-Scout-Degraded = %q, want cache-check — a release nobody could check was served as fact", got)
	}
	// validBlob sets cachedOnly. The held release and the unchecked one are served; the one the store
	// definitively does not hold is dropped. Switching the filter off wholesale on a partial failure —
	// the reverted regression — would serve all three.
	if n := streamsLen(rec); n != 2 {
		t.Errorf("served %d streams, want 2: the held one and the one nobody could check", n)
	}
	if strings.Contains(rec.Body.String(), notHeld) {
		t.Error("a release the store said it does not hold was served to a cachedOnly request")
	}
}

// Degraded is judged on what is SERVED, not on what was scraped.
//
// A failed cache-check batch covering releases every filter would drop still marked the response
// degraded — which means not cached, which means the next /stream re-runs the whole 8s scrape. A
// user-visible cost paid for a fact about releases nobody will ever see.
func TestStream_uncheckedReleasesThatAreFilteredOutDoNotDegrade(t *testing.T) {
	held := repeat("a", 40)
	junk := repeat("b", 40) // unchecked AND dropped by excludeCam, so it cannot affect the answer
	h := NewHandler(Deps{
		Cache:         NewMemoryCache(1 << 20),
		ScrapeTimeout: time.Second,
		MakeScrapers: func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				return []RawStream{
					{InfoHash: held, Title: "A 1080p WEB-DL", Seeders: intp(10)},
					{InfoHash: junk, Title: "B 2160p HDCAM", Seeders: intp(10)},
				}, nil
			}}}
		},
		MakeStores: func(*Config) []Store {
			return []Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{held: true}}}
		},
	})
	rec := do(h, "/"+validBlob+"/stream/movie/tt7654321.json", nil)

	if got := rec.Header().Get("X-Scout-Degraded"); got != "" {
		t.Errorf("X-Scout-Degraded = %q: an unchecked release nobody will see is not a degraded answer", got)
	}
	if n := streamsLen(rec); n != 1 {
		t.Errorf("served %d streams, want 1", n)
	}
}

// One cache-truth store being entirely OUT is a degraded answer, even when the other replies normally.
//
// Its silence is why the survivor's "no" cannot rule anything out. Treated as an ordinary answer, a
// cachedOnly request came back {"streams":[]} — no degraded header, cached for the full TTL and held for
// a day on stale-if-error. An empty list is what "broken" looks like to the app.
func TestStream_oneStoreEntirelyDownIsDegradedAndStopsFiltering(t *testing.T) {
	held := repeat("a", 40)
	h := NewHandler(Deps{
		Cache:         NewMemoryCache(1 << 20),
		ScrapeTimeout: time.Second,
		MakeScrapers: func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				return []RawStream{{InfoHash: held, Title: "A 1080p WEB-DL", Seeders: intp(10)}}, nil
			}}}
		},
		MakeStores: func(*Config) []Store {
			return []Store{
				// TorBox answers "I do not hold it"; Premiumize's check is down and says nothing. The
				// release may be sitting on Premiumize, so it must not be dropped.
				fakeStore{svc: ServiceTorBox, check: map[string]bool{held: false}},
				fakeStore{svc: ServicePremiumize, checkErr: errCheckFailed},
			}
		},
	})
	rec := do(h, "/"+validBlob+"/stream/movie/tt5555555.json", nil)

	if got := rec.Header().Get("X-Scout-Degraded"); got != "cache-check" {
		t.Errorf("X-Scout-Degraded = %q: a store being out is not an ordinary answer", got)
	}
	if n := streamsLen(rec); n != 1 {
		t.Errorf("served %d streams: cachedOnly must stop filtering when a store cannot be asked", n)
	}
	if cc := rec.Header().Get("cache-control"); !strings.Contains(cc, "no-store") {
		t.Errorf("cache-control = %q: a degraded list must not be cached", cc)
	}
}

// A store being out is degraded even when every SERVED release is confirmed held by the survivor.
//
// The per-hash unknown count does not catch this case: a holder's "yes" is knowledge, so `unchecked` is
// zero and the response looks perfectly healthy — while one account's cache check is down and the list
// may be missing everything that account alone holds. Cached for the full TTL, an outage would leave no
// trace at all.
func TestStream_aDownStoreIsDegradedEvenWhenEverythingServedIsKnownHeld(t *testing.T) {
	held := repeat("a", 40)
	h := NewHandler(Deps{
		Cache:         NewMemoryCache(1 << 20),
		ScrapeTimeout: time.Second,
		MakeScrapers: func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				return []RawStream{{InfoHash: held, Title: "A 1080p WEB-DL", Seeders: intp(10)}}, nil
			}}}
		},
		MakeStores: func(*Config) []Store {
			return []Store{
				fakeStore{svc: ServiceTorBox, check: map[string]bool{held: true}}, // a confirmed yes
				fakeStore{svc: ServicePremiumize, checkErr: errCheckFailed},       // entirely down
			}
		},
	})
	rec := do(h, "/"+validBlob+"/stream/movie/tt6666666.json", nil)

	if n := streamsLen(rec); n != 1 {
		t.Fatalf("served %d streams, want the held one", n)
	}
	if got := rec.Header().Get("X-Scout-Degraded"); got != "cache-check" {
		t.Errorf("X-Scout-Degraded = %q: a store is down and nothing in the response says so", got)
	}
	if cc := rec.Header().Get("cache-control"); !strings.Contains(cc, "no-store") {
		t.Errorf("cache-control = %q: a list built during an outage must not be cached", cc)
	}
}

// A title with no releases is a COMPLETE answer, not an outage.
//
// The cache check is never asked anything when there are no hashes, and leaving that as "incomplete"
// raised the outage flag over a question nobody put: every such title came back degraded and no-store,
// and was re-scraped in full on every single request, forever.
func TestStream_noReleasesIsAnAnswerNotAnOutage(t *testing.T) {
	scrapes := 0
	h := NewHandler(Deps{
		Cache:         NewMemoryCache(1 << 20),
		ScrapeTimeout: time.Second,
		MakeScrapers: func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				scrapes++
				return nil, nil // asked, answered, and it has nothing
			}}}
		},
		MakeStores: func(*Config) []Store {
			return []Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{}}}
		},
	})
	first := do(h, "/"+validBlob+"/stream/movie/tt7777777.json", nil)
	if got := first.Header().Get("X-Scout-Degraded"); got != "" {
		t.Errorf("X-Scout-Degraded = %q for a title that simply has no releases", got)
	}
	do(h, "/"+validBlob+"/stream/movie/tt7777777.json", nil)
	if scrapes != 1 {
		t.Errorf("scraped %d times: an authoritative empty answer must be cached like any other", scrapes)
	}
}

// A partial but non-empty list is served AND cached. Whatever came back is real.
//
// Refusing to trust it put the full 8s scrape and a fresh debrid cache-check fan-out on every single
// /stream for as long as one indexer stayed flaky, and told the app "sources temporarily unavailable"
// over a perfectly good list. It is cached for less time instead, so the missing releases appear soon
// after that indexer recovers.
func TestStream_aPartialListIsStillServedAndCached(t *testing.T) {
	scrapes := 0
	h := NewHandler(Deps{
		Cache:         NewMemoryCache(1 << 20),
		ScrapeTimeout: 50 * time.Millisecond,
		MakeScrapers: func(*Config) []scraper {
			return []scraper{
				fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
					scrapes++
					return []RawStream{{InfoHash: repeat("a", 40), Title: "A 1080p WEB-DL", Seeders: intp(10)}}, nil
				}},
				fakeScraper{"comet", func(context.Context) ([]RawStream, error) { return nil, context.Canceled }},
			}
		},
		MakeStores: func(*Config) []Store {
			return []Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{repeat("a", 40): true}}}
		},
	})
	first := do(h, "/"+validBlob+"/stream/movie/tt8888888.json", nil)
	if n := streamsLen(first); n != 1 {
		t.Fatalf("served %d streams; what the healthy indexer returned is real", n)
	}
	if got := first.Header().Get("X-Scout-Degraded"); got != "" {
		t.Errorf("X-Scout-Degraded = %q over a list that is short, not broken", got)
	}
	// The client must not be told to hold it longer than the server does. The server keeps a partial list
	// for a minute; advertising the full five-minute max-age pinned a knowingly-short list on the device
	// for five, and up to a day on stale-if-error — the guard defeated one layer down, again.
	cc := first.Header().Get("cache-control")
	if !strings.Contains(cc, fmt.Sprintf("max-age=%d", int(partialListTTL.Seconds()))) {
		t.Errorf("cache-control = %q: the client would hold this longer than the server does", cc)
	}
	if strings.Contains(cc, "stale-if-error") {
		t.Errorf("cache-control = %q: a short list must not survive a day of errors on the device", cc)
	}

	// The SECOND requester is served from cache, and must be told the same short freshness. Stremio races
	// and cancels addon requests, so a duplicate inside the one-minute window is routine — and it was
	// getting max-age=300 plus a day of stale-if-error for a list the server keeps for sixty seconds.
	second := do(h, "/"+validBlob+"/stream/movie/tt8888888.json", nil)
	hitCC := second.Header().Get("cache-control")
	if !strings.Contains(hitCC, fmt.Sprintf("max-age=%d", int(partialListTTL.Seconds()))) {
		t.Errorf("cache hit sent %q — the shortening is defeated one branch over", hitCC)
	}
	if strings.Contains(hitCC, "stale-if-error") {
		t.Errorf("cache hit sent %q: a short list must not survive a day of errors", hitCC)
	}
	if scrapes != 1 {
		t.Errorf("scraped %d times: a partial list must be cached, or every request pays the full scrape", scrapes)
	}
}

// unthrottleDebug gives a test its own debug allowance. debugLimiter is a package-level bucket under one
// shared key, so without this every test that issues a ?debug=1 request spends from the same three and
// the suite becomes order- and timing-dependent — `go test -count=2` failed unrelated tests with an
// opaque 429.
func unthrottleDebug(t *testing.T) {
	t.Helper()
	saved := debugLimiter
	debugLimiter = newHostLimiter(time.Millisecond, 10000)
	t.Cleanup(func() { debugLimiter = saved })
}

// "Where did this list go" answered exactly, instead of guessed from a log line.
func TestStreamDebug_reportsWhereTheListWent(t *testing.T) {
	unthrottleDebug(t)
	cache := &recordingCache{Cache: NewMemoryCache(1 << 20)}
	h := NewHandler(testDeps(func(d *Deps) { d.Cache = cache }))
	path := "/" + validBlob + "/stream/movie/tt1234567.json"

	rr := do(h, path+"?debug=1", nil)
	if rr.Code != 200 {
		t.Fatalf("debug: %d", rr.Code)
	}
	var body struct {
		Streams []struct {
			Title string `json:"title"`
		} `json:"streams"`
		Debug *struct {
			Deduped   int            `json:"deduped"`
			Ranked    int            `json:"ranked"`
			DroppedBy map[string]int `json:"droppedBy"`
			Kept      []struct {
				Title      string `json:"title"`
				Score      int    `json:"score"`
				Resolution string `json:"resolution"`
				Cached     bool   `json:"cached"`
			} `json:"kept"`
		} `json:"debug"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Debug == nil {
		t.Fatal("?debug=1 returned no debug block")
	}
	// The fixture has three seeds: a CAM (dropped) and two cached releases (kept).
	if body.Debug.Deduped != 3 || body.Debug.Ranked != 2 {
		t.Errorf("counts: deduped=%d ranked=%d, want 3 and 2", body.Debug.Deduped, body.Debug.Ranked)
	}
	if body.Debug.DroppedBy["cam"] != 1 {
		t.Errorf("the CAM drop was not attributed: %v", body.Debug.DroppedBy)
	}
	if len(body.Debug.Kept) != len(body.Streams) {
		t.Fatalf("kept %d scores for %d streams", len(body.Debug.Kept), len(body.Streams))
	}
	// Scores are recorded in SERVED order, so they explain the ordering the viewer sees.
	for i := range body.Streams {
		if body.Debug.Kept[i].Title != body.Streams[i].Title {
			t.Errorf("kept[%d]=%q but stream[%d]=%q", i, body.Debug.Kept[i].Title, i, body.Streams[i].Title)
		}
	}
	if body.Debug.Kept[0].Score <= body.Debug.Kept[1].Score {
		t.Errorf("scores do not explain the order: %d then %d", body.Debug.Kept[0].Score, body.Debug.Kept[1].Score)
	}
}

// A debug build is a diagnostic, not an entry other viewers should be served — and must not displace the
// good one. It also must not be answered FROM the cache, which carries no accounting.
func TestStreamDebug_isNeverCached(t *testing.T) {
	unthrottleDebug(t)
	cache := &recordingCache{Cache: NewMemoryCache(1 << 20)}
	h := NewHandler(testDeps(func(d *Deps) { d.Cache = cache }))
	path := "/" + validBlob + "/stream/movie/tt1234567.json"

	if rr := do(h, path+"?debug=1", nil); rr.Code != 200 {
		t.Fatalf("debug: %d", rr.Code)
	}
	if key := cache.lastKey(); key != "" {
		if v, ok := cache.Get(key); ok && strings.Contains(v, "droppedBy") {
			t.Error("a debug build was cached and would be served to everyone else")
		}
	}
	// A normal request afterwards is unaffected and carries no debug block.
	plain := do(h, path, nil)
	if strings.Contains(plain.Body.String(), "\"debug\"") {
		t.Error("a normal response carries a debug block")
	}
}

// The accounting reconciles: deduped minus every attributed drop equals ranked.
//
// The two filters that run OUTSIDE rankStreams are the ones worth a test — the seed cap and the RD
// filename filter. Measured after them, `deduped` read as "this is all the indexers had" while both had
// already removed releases that appeared in no drop tally, and on an RD-only install that is most of the
// list: realDebridBlocked matches web-dl, webrip, bdrip, hdrip and dvdrip as substrings.
func TestStreamDebug_accountingReconciles(t *testing.T) {
	unthrottleDebug(t)
	// An RD-only config, so realDebridBlocked runs, and enough seeds to trip the 500 cap.
	rdBlob := blob(`{"debrid":[{"service":"realdebrid","token":"rd"}],"indexers":["torrentio"],"resultCap":20}`)
	seeds := make([]RawStream, 0, maxSeeds+40)
	for i := 0; i < maxSeeds+20; i++ {
		// Blocked by RD's filename rule.
		seeds = append(seeds, RawStream{
			InfoHash: fmt.Sprintf("%040x", i),
			Title:    fmt.Sprintf("Film.2024.1080p.WEB-DL.x264-G%d.mkv", i),
		})
	}
	for i := 0; i < 20; i++ {
		// Survives it.
		seeds = append(seeds, RawStream{
			InfoHash: fmt.Sprintf("%040x", 100000+i),
			Title:    fmt.Sprintf("Film.2024.1080p.BluRay.REMUX-G%d.mkv", i),
		})
	}
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				return append([]RawStream(nil), seeds...), nil
			}}}
		}
		d.MakeStores = func(*Config) []Store { return []Store{fakeStore{svc: ServiceRealDebrid}} }
	}))

	rr := do(h, "/"+rdBlob+"/stream/movie/tt1234567.json?debug=1", nil)
	if rr.Code != 200 {
		t.Fatalf("debug: %d", rr.Code)
	}
	var body struct {
		Debug struct {
			Deduped   int            `json:"deduped"`
			Ranked    int            `json:"ranked"`
			DroppedBy map[string]int `json:"droppedBy"`
		} `json:"debug"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Debug.Deduped != len(seeds) {
		t.Errorf("deduped = %d, want the %d the indexer returned before anything trimmed them",
			body.Debug.Deduped, len(seeds))
	}
	if body.Debug.DroppedBy["seedCap"] != len(seeds)-maxSeeds {
		t.Errorf("seedCap drops = %d, want %d", body.Debug.DroppedBy["seedCap"], len(seeds)-maxSeeds)
	}
	if body.Debug.DroppedBy["realDebridBlocked"] == 0 {
		t.Errorf("the RD filename filter dropped nothing it admitted to: %v", body.Debug.DroppedBy)
	}
	dropped := 0
	for _, n := range body.Debug.DroppedBy {
		dropped += n
	}
	if body.Debug.Deduped-dropped != body.Debug.Ranked {
		t.Errorf("accounting does not reconcile: deduped %d - dropped %d != ranked %d (%v)",
			body.Debug.Deduped, dropped, body.Debug.Ranked, body.Debug.DroppedBy)
	}
}

// ?debug=1 is the only /stream path with neither the list cache nor a shared singleflight in front of the
// debrid cache-check fan-out, and there is no host limiter anywhere on the debrid side — so without a
// ceiling here, one sequential curl loop is an unbounded stream of scrapes and checkcached batches.
func TestStreamDebug_isRateLimited(t *testing.T) {
	saved := debugLimiter
	debugLimiter = newHostLimiter(time.Hour, 2) // the ceiling itself is what's under test here
	t.Cleanup(func() { debugLimiter = saved })

	scrapes := 0
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeScrapers = func(*Config) []scraper {
			scrapes++
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				return testSeeds(), nil
			}}}
		}
	}))
	path := "/" + validBlob + "/stream/movie/tt1234567.json?debug=1"

	for i := 0; i < 2; i++ {
		if rr := do(h, path, nil); rr.Code != 200 {
			t.Fatalf("debug %d: %d, want 200", i, rr.Code)
		}
	}
	rr := do(h, path, nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("third debug: %d, want 429", rr.Code)
	}
	if rr.Header().Get("retry-after") == "" {
		t.Error("429 without a Retry-After")
	}
	if scrapes != 2 {
		t.Errorf("%d builds ran, want 2 — a refused debug must not reach the indexers or the debrid", scrapes)
	}
	// A plain request is unaffected: the ceiling is on the diagnostic, not on watching things.
	if rr := do(h, "/"+validBlob+"/stream/movie/tt1234567.json", nil); rr.Code != 200 {
		t.Errorf("normal request after debug throttling: %d", rr.Code)
	}
}

// The one thing this route must never emit. MediaFusion's base URL carries an encrypted config minted
// from the debrid token, which is why scrape.go logs the indexer name and never the address.
func TestStreamDebug_leaksNoUpstreamURLsOrTokens(t *testing.T) {
	unthrottleDebug(t)
	h := NewHandler(testDeps(nil))
	body := do(h, "/"+validBlob+"/stream/movie/tt1234567.json?debug=1", nil).Body.String()
	for _, forbidden := range []string{"tb-secret", "mediafusion.elfhosted", "torrentio.strem.fun", "https://comet"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("?debug=1 disclosed %q", forbidden)
		}
	}
}

// recordingCache remembers the key a list was written under, so a test can age that entry without having
// to rebuild the handler's cache-key formula.
type recordingCache struct {
	Cache
	mu   sync.Mutex
	last string
}

func (c *recordingCache) Put(key, value string, ttl time.Duration) {
	c.mu.Lock()
	c.last = key
	c.mu.Unlock()
	c.Cache.Put(key, value, ttl)
}

func (c *recordingCache) lastKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// An expired-but-complete list is answered from immediately, with the rebuild running behind the reply.
//
// The response header has advertised stale-while-revalidate since this route existed and the server
// implemented none of it: a request one second past expiry paid the whole scrape plus a debrid
// cache-check fan-out with a good list sitting right there.
func TestStreamList_servesStaleWhileRebuilding(t *testing.T) {
	cache := &recordingCache{Cache: NewMemoryCache(1 << 20)}
	scraped := make(chan struct{}, 4)
	h := NewHandler(testDeps(func(d *Deps) {
		d.Cache = cache
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				scraped <- struct{}{}
				return testSeeds(), nil
			}}}
		}
	}))
	path := "/" + validBlob + "/stream/movie/tt1234567.json"

	cold := do(h, path, nil)
	if cold.Code != 200 {
		t.Fatalf("cold build: %d", cold.Code)
	}
	<-scraped

	// Age the entry past its freshness without touching its physical expiry — exactly the state the
	// the stale window exists to hold.
	key := cache.lastKey()
	held, ok := cache.Get(key)
	if !ok {
		t.Fatal("the cold build cached nothing")
	}
	complete, _, etag, body := splitCached(held)
	cache.Put(key, joinCached(complete, time.Now().Add(-time.Second).Unix(), etag, body), time.Minute)

	stale := do(h, path, nil)
	if stale.Code != 200 {
		t.Fatalf("stale hit: %d", stale.Code)
	}
	if stale.Body.String() != body {
		t.Error("the stale hit did not serve the cached body")
	}
	// Told to come back soon, and NOT given stale-if-error: holding a stale list for the full TTL, and a
	// day on any later error, is the harm being fixed rather than something to pass on to the device.
	if cc := stale.Header().Get("cache-control"); cc != "public, max-age=60" {
		t.Errorf("stale cache-control = %q", cc)
	}
	select {
	case <-scraped:
	case <-time.After(5 * time.Second):
		t.Fatal("no rebuild ran behind the stale answer — the entry would just expire")
	}
}

// The stale window is a ceiling scaled by the configured TTL, not an absolute. Pinned at two minutes, an
// operator running SCOUT_LIST_TTL_SECONDS=30 got 30 seconds of freshness followed by two minutes of
// staleness — an entry spending 80% of its life stale — and was told to hold the stale body for 60s,
// twice the freshness they configured.
func TestStreamList_staleWindowScalesWithTheTTL(t *testing.T) {
	if got := staleWindowFor(30 * time.Second); got != 30*time.Second {
		t.Errorf("short TTL: window %s, want it capped at the TTL", got)
	}
	if got := staleWindowFor(5 * time.Minute); got != maxStaleServeWindow {
		t.Errorf("long TTL: window %s, want the %s ceiling", got, maxStaleServeWindow)
	}

	cache := &recordingCache{Cache: NewMemoryCache(1 << 20)}
	h := NewHandler(testDeps(func(d *Deps) {
		d.Cache = cache
		d.ListTTL = 30 * time.Second
	}))
	path := "/" + validBlob + "/stream/movie/tt1234567.json"
	do(h, path, nil)

	held, _ := cache.Get(cache.lastKey())
	complete, _, etag, body := splitCached(held)
	cache.Put(cache.lastKey(), joinCached(complete, time.Now().Add(-time.Second).Unix(), etag, body), time.Minute)
	// The stale reply must never claim a longer freshness than the operator configured.
	if cc := do(h, path, nil).Header().Get("cache-control"); cc != "public, max-age=30" {
		t.Errorf("stale cache-control = %q, want max-age capped at the 30s TTL", cc)
	}
}

// One goroutine per stale KEY, not per stale REQUEST.
//
// singleflight collapses the work but not the spawn, so every stale hit used to start a goroutine that
// then parked behind the leader at ~3 KB apiece — and the request goroutine returns immediately, so the
// flood is fire-and-forget. Roughly 60k of those is enough to push a 230 MiB heap into an OOM kill.
func TestStreamList_staleRebuildSpawnsOneGoroutinePerKey(t *testing.T) {
	release := make(chan struct{})
	var builds atomic.Int32
	cache := &recordingCache{Cache: NewMemoryCache(1 << 20)}
	h := NewHandler(testDeps(func(d *Deps) {
		d.Cache = cache
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				if builds.Add(1) > 1 {
					<-release // hold the rebuild open so the flood lands while it is in flight
				}
				return testSeeds(), nil
			}}}
		}
	}))
	path := "/" + validBlob + "/stream/movie/tt1234567.json"
	do(h, path, nil) // seed
	key := cache.lastKey()
	held, _ := cache.Get(key)
	complete, _, etag, body := splitCached(held)
	cache.Put(key, joinCached(complete, time.Now().Add(-time.Second).Unix(), etag, body), time.Minute)

	before := runtime.NumGoroutine()
	for i := 0; i < 300; i++ {
		if rr := do(h, path, nil); rr.Code != 200 {
			t.Fatalf("stale hit %d: %d", i, rr.Code)
		}
	}
	// One rebuild is in flight and parked; the other 299 requests must not have started anything.
	if grew := runtime.NumGoroutine() - before; grew > 5 {
		close(release)
		t.Fatalf("300 stale hits grew the goroutine count by %d, want ~1", grew)
	}
	close(release)
}

// The rebuild gate, tested directly against injected clock values.
//
// The end-to-end test below cannot reach the cool-off: every stale hit it makes lands inside the 28-second
// BOOKING window, so the gate alone satisfies it and disabling the cool-off left it green. The booking
// window is not injectable (listBuildSlack is a package constant), so the cool-off gets a unit test
// instead of a contrived integration one.
func TestRebuildGate(t *testing.T) {
	h := &handler{}
	t0 := time.Now()
	const budget = 28 * time.Second

	gen, ok := h.bookRebuild("k", t0, t0.Add(budget))
	if !ok {
		t.Fatal("first booking refused")
	}
	if _, ok := h.bookRebuild("k", t0.Add(time.Second), t0.Add(budget)); ok {
		t.Error("a second booking was granted while the first was live")
	}
	// A healthy rebuild clears the key immediately, so the next expiry rebuilds at once.
	h.releaseRebuild("k", gen, time.Time{})
	gen2, ok := h.bookRebuild("k", t0.Add(2*time.Second), t0.Add(budget))
	if !ok {
		t.Fatal("a released key refused the next booking")
	}

	// A DEGRADED rebuild installs a cool-off, which outlives the booking window it replaces. Without it,
	// an outage has every request re-book a full scrape plus debrid fan-out for the rest of the stale life.
	h.releaseRebuild("k", gen2, t0.Add(rebuildCooloff))
	if _, ok := h.bookRebuild("k", t0.Add(budget+time.Second), t0.Add(budget)); ok {
		t.Error("the cool-off did not outlive the booking window — a degraded key rebuilds immediately")
	}
	if _, ok := h.bookRebuild("k", t0.Add(rebuildCooloff+time.Second), t0.Add(budget)); !ok {
		t.Error("the cool-off never expired")
	}
}

// A global ceiling as well as the per-key one. bookRebuild dedupes per KEY, which stopped one goroutine
// per stale REQUEST but left one per distinct stale key — and a caller picks the keys. Measured before
// this: 400 stale keys from a single sequential caller produced 801 goroutines, each holding a finished
// list body.
func TestRebuildGate_boundsConcurrentRebuilds(t *testing.T) {
	h := &handler{}
	t0 := time.Now()
	gens := map[string]uint64{}
	for i := 0; i < maxConcurrentRebuilds; i++ {
		key := fmt.Sprintf("k%d", i)
		gen, ok := h.bookRebuild(key, t0, t0.Add(time.Minute))
		if !ok {
			t.Fatalf("booking %d refused below the ceiling", i)
		}
		gens[key] = gen
	}
	if _, ok := h.bookRebuild("one-too-many", t0, t0.Add(time.Minute)); ok {
		t.Errorf("booked %d concurrent rebuilds, want a ceiling of %d",
			maxConcurrentRebuilds+1, maxConcurrentRebuilds)
	}
	// Finishing one frees exactly one slot.
	h.releaseRebuild("k0", gens["k0"], time.Time{})
	if _, ok := h.bookRebuild("after-release", t0, t0.Add(time.Minute)); !ok {
		t.Error("a freed slot was not reusable")
	}
}

// The slot must come back even when the lease is already gone. A rebuild that overruns its lease has it
// swept by a later booking, so releasing only a lease we still own leaked a slot every time that
// happened — and eight of those means no title ever refreshes in the background again.
func TestRebuildGate_sweptLeaseStillReturnsItsSlot(t *testing.T) {
	h := &handler{}
	t0 := time.Now()

	// Book and overrun: a short lease, then a later booking elsewhere sweeps it.
	gen, _ := h.bookRebuild("overrun", t0, t0.Add(time.Second))
	later := t0.Add(10 * time.Second)
	sweeper, _ := h.bookRebuild("sweeper", later, later.Add(time.Minute))
	if _, held := h.rebuilds["overrun"]; held {
		t.Fatal("the expired lease was not swept, so this test proves nothing")
	}
	// The overrun rebuild finally finishes and releases a lease that is no longer in the map.
	h.releaseRebuild("overrun", gen, time.Time{})
	h.releaseRebuild("sweeper", sweeper, time.Time{})

	// Every slot must be free again.
	for i := 0; i < maxConcurrentRebuilds; i++ {
		if _, ok := h.bookRebuild(fmt.Sprintf("fresh%d", i), later, later.Add(time.Minute)); !ok {
			t.Fatalf("only %d of %d slots came back — a swept lease leaked one",
				i, maxConcurrentRebuilds)
		}
	}
}

// A lease is a wall-clock reservation, so a rebuild can outrun it — its context deadline and its budget
// are the same 28 seconds, and it still has a marshal and a disk write to do afterwards. Without a fencing
// token that late release deleted whatever sat at the key: a NEWER booking (leaking a rebuild slot) or a
// cool-off just installed (restoring per-request scrapes during an outage).
func TestRebuildGate_staleLeaseReleasesNothing(t *testing.T) {
	h := &handler{}
	t0 := time.Now()

	old, _ := h.bookRebuild("k", t0, t0.Add(time.Second))
	// The lease expires and a second request books the key.
	fresh, ok := h.bookRebuild("k", t0.Add(2*time.Second), t0.Add(30*time.Second))
	if !ok || fresh == old {
		t.Fatalf("second booking: ok=%v gen=%d (first was %d)", ok, fresh, old)
	}
	// The first, overrun rebuild now finishes and tries to release. Released ONCE — releaseRebuild is
	// called exactly once per granted booking, and an earlier version of this test released `old` twice,
	// which drove the live count below what production can reach and was absorbed silently by the floor
	// in releaseRebuild. A test that breaks the invariant is a test that cannot check it.
	h.releaseRebuild("k", old, time.Time{})
	if _, ok := h.bookRebuild("k", t0.Add(3*time.Second), t0.Add(30*time.Second)); ok {
		t.Error("a superseded lease deleted the live booking — two rebuilds can now run for one key")
	}
	// And a superseded lease must not wipe a cool-off either. `fresh` installs one and is thereby spent;
	// a THIRD booking stands in for the overrun rebuild that arrives afterwards.
	h.releaseRebuild("k", fresh, t0.Add(time.Hour))
	late, ok := h.bookRebuild("other", t0.Add(4*time.Second), t0.Add(30*time.Second))
	if !ok {
		t.Fatal("could not book a second key")
	}
	h.releaseRebuild("k", late, time.Time{}) // right gen, wrong key — must not touch k's cool-off
	if _, ok := h.bookRebuild("k", t0.Add(5*time.Second), t0.Add(30*time.Second)); ok {
		t.Error("a superseded lease wiped the cool-off")
	}
	h.releaseRebuild("other", late, time.Time{})
}

// Every granted booking returns its slot exactly once, so the ceiling neither seizes up nor loosens.
//
// The counter is the thing maxConcurrentRebuilds is enforced on, and nothing was checking that it comes
// back to rest: a release that never happens stops background refreshes forever, and one that happens
// twice quietly raises the ceiling — which is the harm the ceiling exists to prevent.
func TestRebuildGate_slotAccountingBalances(t *testing.T) {
	h := &handler{}
	t0 := time.Now()
	for round := 0; round < 5; round++ {
		var gens []uint64
		for i := 0; i < maxConcurrentRebuilds; i++ {
			gen, ok := h.bookRebuild(fmt.Sprintf("r%d-k%d", round, i), t0, t0.Add(time.Minute))
			if !ok {
				t.Fatalf("round %d: booking %d refused — slots did not come back", round, i)
			}
			gens = append(gens, gen)
		}
		for i, gen := range gens {
			h.releaseRebuild(fmt.Sprintf("r%d-k%d", round, i), gen, time.Time{})
		}
	}
	h.rebuildMu.Lock()
	live := h.rebuildsLive
	h.rebuildMu.Unlock()
	if live != 0 {
		t.Errorf("after 40 balanced book/release pairs the live count is %d, want 0", live)
	}
}

// End to end, an outage does not put a scrape on every stale request. Note this exercises the BOOKING
// gate, not the cool-off — every hit here lands inside the booking window; TestRebuildGate covers the
// cool-off directly, because the booking window cannot be shortened from a test.
func TestStreamList_outageDoesNotScrapePerRequest(t *testing.T) {
	var scrapes atomic.Int32
	healthy := atomic.Bool{}
	healthy.Store(true)
	cache := &recordingCache{Cache: NewMemoryCache(1 << 20)}
	h := NewHandler(testDeps(func(d *Deps) {
		d.Cache = cache
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				scrapes.Add(1)
				if !healthy.Load() {
					return nil, context.Canceled // every indexer down → degraded build
				}
				return testSeeds(), nil
			}}}
		}
	}))
	path := "/" + validBlob + "/stream/movie/tt1234567.json"
	do(h, path, nil)
	key := cache.lastKey()
	held, _ := cache.Get(key)
	complete, _, etag, body := splitCached(held)

	healthy.Store(false)
	for i := 0; i < 20; i++ {
		// Re-stale the entry before each request, as a real expired entry would be.
		cache.Put(key, joinCached(complete, time.Now().Add(-time.Second).Unix(), etag, body), time.Minute)
		do(h, path, nil)
	}
	// Let the one permitted rebuild finish and register its cool-off.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && scrapes.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := scrapes.Load(); got > 3 {
		t.Errorf("%d scrapes for 20 stale hits during an outage — the cool-off is not holding", got)
	}
}

// A partial list must never be served stale. It is already knowingly short, and a longer life is the one
// thing it must not get — the same harm the shortened header exists to prevent, one branch over.
func TestStreamList_partialListGetsNoStaleWindow(t *testing.T) {
	cache := &recordingCache{Cache: NewMemoryCache(1 << 20)}
	h := NewHandler(testDeps(func(d *Deps) {
		d.Cache = cache
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{
				fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) { return testSeeds(), nil }},
				fakeScraper{"comet", func(context.Context) ([]RawStream, error) { return nil, context.Canceled }},
			}
		}
	}))
	if rr := do(h, "/"+validBlob+"/stream/movie/tt1234567.json", nil); rr.Code != 200 {
		t.Fatalf("partial build: %d", rr.Code)
	}
	held, ok := cache.Get(cache.lastKey())
	if !ok {
		t.Fatal("the partial build cached nothing")
	}
	complete, freshUntil, _, _ := splitCached(held)
	if complete {
		t.Fatal("a scrape with a failed indexer was recorded as complete")
	}
	if freshUntil != 0 {
		t.Errorf("a partial list carries a freshness stamp (%d) — it would outlive its own expiry", freshUntil)
	}
}

// A cached entry written by an older build has no completeness recorded, and must be read as INCOMPLETE.
//
// A redeploy is exactly when short lists are in flight. Claiming completeness for one served it with a
// five-minute max-age and a day of stale-if-error — the harm the three-part format exists to prevent,
// reached through the one branch that handles entries surviving a deploy.
func TestSplitCached_readsAnOlderEntryConservatively(t *testing.T) {
	complete, _, etag, body := splitCached("W/\"abc\"\x00{\"streams\":[]}")
	if complete {
		t.Error("an entry with no completeness recorded must not be assumed complete")
	}
	if etag != "W/\"abc\"" || body != "{\"streams\":[]}" {
		t.Errorf("legacy entry mis-split: etag=%q body=%q", etag, body)
	}

	// And the round trip of the current format.
	for _, want := range []bool{true, false} {
		got, _, e, b := splitCached(joinCached(want, 0, "E", "B"))
		if got != want || e != "E" || b != "B" {
			t.Errorf("round trip complete=%v: got %v %q %q", want, got, e, b)
		}
	}
	// A body containing the separator must not be truncated.
	_, _, _, weird := splitCached(joinCached(true, 0, "E", "a\x00b"))
	if weird != "a\x00b" {
		t.Errorf("body with a separator was truncated: %q", weird)
	}
	// The freshness stamp round-trips beside completeness.
	if c, f, e, b := splitCached(joinCached(true, 1757000000, "E", "B")); !c || f != 1757000000 || e != "E" || b != "B" {
		t.Errorf("stamped round trip: complete=%v fresh=%d etag=%q body=%q", c, f, e, b)
	}
	// A three-field entry from a build that predates the stamp reports freshUntil 0 — "no stamp", which
	// the hit path reads as fresh. It has to: such an entry was written with its freshness AS its expiry,
	// so the cache still holding it is proof it has not passed.
	if c, f, _, _ := splitCached("1\x00E\x00B"); !c || f != 0 {
		t.Errorf("pre-stamp entry: complete=%v freshUntil=%d, want complete with no stamp", c, f)
	}
}

// A cached value with NO separator has no completeness recorded either, and must be read the same
// conservative way as the legacy one-separator form. Two answers to one question is how the format got
// into trouble in the first place.
func TestSplitCached_treatsEveryUnknownFormTheSameWay(t *testing.T) {
	bare, oneSep := "just-a-body", "etag\x00body"
	bareComplete, _, _, bareBody := splitCached(bare)
	sepComplete, _, _, _ := splitCached(oneSep)
	if bareComplete != sepComplete {
		t.Errorf("unknown completeness answered %v with no separator and %v with one", bareComplete, sepComplete)
	}
	if bareComplete {
		t.Error("completeness that was never recorded must not be assumed")
	}
	if bareBody != bare {
		t.Errorf("a separator-less value must be served whole: %q", bareBody)
	}
}

// deadlineStore records the deadline each call arrives with, so a test can assert which budget the
// handler built rather than which one it meant to.
type deadlineStore struct {
	svc DebridService
	// EVERY call, not the last one: handlePlay reads status twice on the plain route, and the second
	// read was already correct — so keeping only the newest made the first one's budget invisible.
	statusAt []time.Duration
	resolve  func() (string, error)
	// unknown makes the store answer "could not find out", which is the ONLY thing that fires the
	// escalation. Without it the escalated read never happens, and an assertion phrased as "at most one
	// read may exceed the ordinary budget" is vacuously true no matter what the handler does.
	unknown bool
}

func (d *deadlineStore) Service() DebridService { return d.svc }
func (d *deadlineStore) CacheCheck(context.Context, []string) (map[string]bool, error) {
	return nil, nil
}
func (d *deadlineStore) Resolve(context.Context, ResolveTarget) (string, error) { return d.resolve() }
func (d *deadlineStore) Status(ctx context.Context, _ ResolveTarget) (StoreStatus, bool) {
	if dl, ok := ctx.Deadline(); ok {
		d.statusAt = append(d.statusAt, time.Until(dl).Round(time.Second))
	}
	return StoreStatus{}, false
}
func (d *deadlineStore) StatusAnswer(ctx context.Context, t ResolveTarget) (StoreStatus, statusAnswer) {
	st, ok := d.Status(ctx, t)
	switch {
	case d.unknown:
		return st, statusUnknown
	case ok:
		return st, statusDownloading
	}
	return st, statusNo
}

// A read that exists to answer a POLL must run under the status budget. A client polls /play for the
// length of a fetch, so a read meant to answer promptly must not be able to hold that poll for
// forty-five seconds against a slow debrid. Nothing asserted which budget these used, so the two that
// were already right and the one that was not looked identical to the suite.
func TestHandlePlay_pollReadsUseTheStatusBudget(t *testing.T) {
	for _, route := range []string{"", "?probe=1"} {
		// Both answers, because the rule differs between them and an earlier version ran only the left
		// one — where no read is ever escalated, so the clause permitting the escalation excused every
		// read at once and the ceiling silently became the doubled budget for all of them.
		for _, unknown := range []bool{false, true} {
			name := "play" + route
			if unknown {
				name += "/unknown"
			}
			t.Run(name, func(t *testing.T) {
				store := &deadlineStore{svc: ServiceTorBox, unknown: unknown,
					resolve: func() (string, error) { return "", &DeadLinkError{"nothing here"} }}
				h := NewHandler(testDeps(func(d *Deps) {
					d.MakeStores = func(*Config) []Store { return []Store{store} }
				}))
				tok := encodePlayToken(PlayTarget{InfoHash: repeat("a", 40)})
				do(h, "/"+validBlob+"/play/"+tok+route, nil)

				if len(store.statusAt) == 0 {
					t.Fatal("the status read never happened, so this asserts nothing")
				}
				// The rule, stated exactly: a read on the poll route never takes the RESOLVE budget, and
				// at most one — the escalation, on the path about to spend an add — may take double the
				// status budget. An earlier version asserted a flat statusBudget, which read as
				// forbidding the escalation; cutting the escalation to fit made it a guard that could
				// not succeed, because the retry is sliced from the same budget as the read that just
				// timed out.
				//
				// The ceiling is 2*statusBudget SPELLED OUT, not escalatedStatusBudget(). Taking it from
				// the same function the handler derives the deadline from made the multiplier the one
				// thing this test could not see: changing it to 2.75x left the whole suite green, and at
				// anything up to 2.8125x — the point where escalatedStatusCtx's own guard starts
				// declining — the single poll-blocking read grows with nothing objecting.
				ceiling := 2 * statusBudget
				doubled := 0
				for i, left := range store.statusAt {
					if left > ceiling {
						t.Errorf("status read %d carried a %v deadline, want no more than %v — a poll "+
							"read must not be able to hold the poll for the whole resolve budget",
							i, left, ceiling)
					}
					if left > statusBudget {
						doubled++
					}
				}
				// Exactly one doubled read on the path that escalates, and none anywhere else. The probe
				// never escalates — it answers 503 "ask again" rather than spending an add, so it has no
				// reason to buy a second opinion.
				want := 0
				if unknown && route == "" {
					want = 1
				}
				if doubled != want {
					t.Errorf("%d of %v reads exceeded the ordinary status budget, want exactly %d",
						doubled, store.statusAt, want)
				}
			})
		}
	}
}

// The narrowed escalation, asserted where handlePlay actually performs it.
//
// The pool-level property had a test and the CALL SITE did not: reverting handlePlay to escalate through
// the whole pool left the entire suite green, so the fix that saves the upstream calls was unpinned while
// the mechanism it uses was covered. Counted in store reads because that is the quantity that matters —
// each TorBox read is up to two upstream calls, on a two-second poll cadence.
func TestHandlePlay_theEscalationReAsksOnlyTheStoreThatCouldNotAnswer(t *testing.T) {
	dead := func() (string, error) { return "", &DeadLinkError{"nothing here"} }
	var uncertainAsks, firstAsks, lastAsks int
	stores := []Store{
		answeringStore{fakeStore: fakeStore{svc: ServiceRealDebrid, resolve: dead}, answer: statusNo, asked: &firstAsks},
		answeringStore{fakeStore: fakeStore{svc: ServiceTorBox, resolve: dead}, answer: statusUnknown, asked: &uncertainAsks},
		answeringStore{fakeStore: fakeStore{svc: ServicePremiumize, resolve: dead}, answer: statusNo, asked: &lastAsks},
	}
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store { return stores }
	}))
	tok := encodePlayToken(PlayTarget{InfoHash: repeat("a", 40)})
	do(h, "/"+validBlob+"/play/"+tok, nil)

	// Three reads happen on this path: the poll read, the escalation, and the post-failure read after the
	// resolve. Only the escalation is narrowed — the post-failure one runs after an add may have landed,
	// so every store's answer can have changed and all of them are asked again.
	if uncertainAsks != 3 {
		t.Errorf("the store that could not answer was read %d times, want 3 (poll, escalation, post-failure)",
			uncertainAsks)
	}
	if firstAsks != 2 || lastAsks != 2 {
		t.Errorf("stores that answered definitively were read %d and %d times, want 2 each — the "+
			"escalation is re-asking stores that already gave a definitive answer",
			firstAsks, lastAsks)
	}
}

// The same rule, on the ONE path that is allowed an exception: a first read that timed out earns a
// second, longer one, because the alternative is queueing a torrent already downloading.
//
// The exception is bounded, and the bound is the whole point — the first version of it took a FRESH
// resolveBudget, which always outlives the resolve clock started at the top of handlePlay, so the read
// burned the entire budget, the add became impossible, and the release was never queued at all. That is
// worse than the duplicate add it set out to prevent.
//
// Asserted on escalatedStatusCtx directly rather than through a request. An end-to-end version of this
// depended on a real budget elapsing, which made it flake on its own fixture at about one run in thirty:
// StorePool.Status declines to ask a store whose budget is already spent, so ordinary scheduling jitter
// could delete the first read. The property here is arithmetic and deserves an arithmetic test; that the
// handler ESCALATES at all is pinned separately, and without any clock, by
// TestPlay_statusTimeoutDoesNotAdd asserting two status reads.
func TestEscalatedStatusCtx_cannotOutliveTheResolveBudget(t *testing.T) {
	// A resolve budget with plenty left: the escalation is granted, and bounded by its own ceiling.
	parent, cancel := context.WithTimeout(context.Background(), resolveBudget)
	defer cancel()
	ctx, escCancel, ok := escalatedStatusCtx(parent)
	if !ok {
		t.Fatal("a full resolve budget refused the escalation")
	}
	defer escCancel()

	got, has := ctx.Deadline()
	if !has {
		t.Fatal("the escalated context carries no deadline")
	}
	// Spelled out rather than taken from escalatedStatusBudget(), which is what the code under test
	// derives the deadline from — asserting a value against its own source pins nothing. The multiplier
	// is the number that matters: it decides how long a single poll can block, and how much of the
	// resolve clock is left for the add afterwards.
	if left := time.Until(got); left > 2*statusBudget {
		t.Errorf("escalated read got %v, want no more than the %v ceiling", left, 2*statusBudget)
	}
	if got := escalatedStatusBudget(); got != 2*statusBudget {
		t.Errorf("the escalation is %v, want exactly twice the %v status budget", got, statusBudget)
	}
	parentDeadline, _ := parent.Deadline()
	if got.After(parentDeadline) {
		t.Error("the escalated read outlives the resolve budget it is gating — the add becomes impossible")
	}

	// Too little left to do the add afterwards: declined, so the resolve keeps what remains.
	tight, tightCancel := context.WithTimeout(context.Background(), escalatedStatusBudget())
	defer tightCancel()
	if _, c, ok := escalatedStatusCtx(tight); ok {
		if c != nil {
			c()
		}
		t.Error("escalated with too little budget left to add afterwards")
	}
	// A parent with no deadline at all has nothing to carve from.
	if _, c, ok := escalatedStatusCtx(context.Background()); ok {
		if c != nil {
			c()
		}
		t.Error("escalated from a context with no deadline")
	}
}

// The probe DISCOVERS refusals; it just used to throw them away, inspecting only `err == nil` and then
// relying on the backoff cache to have been written by someone else. A refusal that reaches it
// unrecorded fell through to 404 "not_queued" — a claim, not a shrug — so during a throttle the very URL
// the client polls to draw its progress bar said nothing was queued while /play said the debrid was
// refusing. Both unrecorded classes are driven here: errNoFileList, which is never recorded anywhere,
// and TorBox's warm-entry fast path.
func TestHandleProbe_reportsARefusalItDiscoversItself(t *testing.T) {
	held := repeat("a", 40)
	for _, tc := range []struct {
		name  string
		warm  bool
		reply func(*http.Request) *http.Response
	}{
		{"mylist answers something unusable", false, func(r *http.Request) *http.Response {
			if strings.Contains(r.URL.Path, "mylist") {
				return resp(200, `{"unexpected":"envelope"}`)
			}
			return resp(200, `{"data":{"`+held+`":{"name":"x"}}}`)
		}},
		{"a throttled link request on a warm entry", true, func(r *http.Request) *http.Response {
			if strings.Contains(r.URL.Path, "requestdl") {
				return resp(429, `{}`)
			}
			if strings.Contains(r.URL.Path, "mylist") {
				return resp(200, `{"data":{"id":42,"files":[{"id":0,"name":"m.mkv","size":9}]}}`)
			}
			return resp(200, `{"data":{"`+held+`":{"name":"x"}}}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewMemoryCache(1 << 20)
			// The account HOLDS it, so the probe's NoAdd resolve reaches the read path rather than
			// returning "this would queue" before it asks anything.
			cache.Put(torrentIDKey("tb-secret", held), "42", resolveCacheTTL)
			if tc.warm {
				cache.Put(resolveKey("tb-secret", held),
					`{"torrentId":42,"files":[{"Index":0,"Name":"m.mkv","SizeBytes":9}]}`, resolveCacheTTL)
			}
			store := &torBoxStore{token: "tb-secret", cache: cache, api: torboxAPI,
				client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
					return tc.reply(r), nil
				}}}
			h := NewHandler(testDeps(func(d *Deps) {
				d.MakeStores = func(*Config) []Store { return []Store{store} }
			}))
			tok := encodePlayToken(PlayTarget{InfoHash: held, Season: intp(1), Episode: intp(2)})
			rec := do(h, "/"+validBlob+"/play/"+tok+"?probe=1", nil)
			if rec.Code == http.StatusNotFound {
				t.Errorf("probe answered %d %s — a refusal it saw itself was reported as an absence",
					rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// slowStatusStore answers Status by waiting out the caller's context the first `slowCalls` times, then
// reports a download in progress. It is the shape of a real TorBox account with a large listing: Status
// makes two upstream calls, one of them an account listing measured at ~13 MB on 2,000 torrents.
type slowStatusStore struct {
	svc DebridService
	// How many leading Status calls wait out the caller's context instead of answering.
	slowCalls int32
	// When set, a call that is not slow answers "nobody is fetching it" rather than "downloading".
	neverDownloading bool
	statusHits       int32
	resolves         int32
}

func (s *slowStatusStore) Service() DebridService { return s.svc }
func (s *slowStatusStore) CacheCheck(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (s *slowStatusStore) Resolve(context.Context, ResolveTarget) (string, error) {
	atomic.AddInt32(&s.resolves, 1)
	return "https://cdn.example/added.mkv", nil
}
func (s *slowStatusStore) Status(ctx context.Context, _ ResolveTarget) (StoreStatus, bool) {
	if atomic.AddInt32(&s.statusHits, 1) <= atomic.LoadInt32(&s.slowCalls) {
		<-ctx.Done() // outlast the caller's budget, exactly as a slow account listing does
		return StoreStatus{}, false
	}
	if s.neverDownloading {
		return StoreStatus{}, false
	}
	return StoreStatus{Progress: 0.4}, true
}

// A status read that TIMED OUT must not lead to an add.
//
// pool.Status reports "nobody is fetching it" and "I could not find out in time" both as ok=false, so a
// timed-out read fell through to the resolve — which queues the torrent, very often one already
// downloading. Opening a ten-episode season on a large account could spend ten of the fifty hourly adds
// re-queueing torrents in flight.
func TestPlay_statusTimeoutDoesNotAdd(t *testing.T) {
	// The shipped 8s is not a unit-test wait, but not so small that scheduling jitter can spend it before
	// the read is even attempted — StorePool.Status declines to ask a store whose budget is already gone.
	saved := statusBudget
	statusBudget = 150 * time.Millisecond
	t.Cleanup(func() { statusBudget = saved })

	store := &slowStatusStore{svc: ServiceTorBox, slowCalls: 1} // slow once, then answers
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	tok := encodePlayToken(PlayTarget{InfoHash: repeat("a", 40)})

	rr := do(h, "/"+validBlob+"/play/"+tok, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("play: %d, want 202 — the release is downloading", rr.Code)
	}
	if got := atomic.LoadInt32(&store.resolves); got != 0 {
		t.Errorf("a timed-out status read still queued the torrent (%d resolves)", got)
	}
	// The second, longer read is what answered — the first one is the timeout.
	if got := atomic.LoadInt32(&store.statusHits); got != 2 {
		t.Errorf("status called %d times, want 2 (the short read, then the definitive one)", got)
	}
}

// The control: a status read that answers PROMPTLY with "nobody is fetching it" is a real answer, and
// the add proceeds exactly as before. Without this, the fix above could simply be refusing to ever add.
func TestPlay_promptNotDownloadingStillAdds(t *testing.T) {
	saved := statusBudget
	statusBudget = 150 * time.Millisecond
	t.Cleanup(func() { statusBudget = saved })

	store := &slowStatusStore{svc: ServiceTorBox, neverDownloading: true} // answers at once, and says no
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	tok := encodePlayToken(PlayTarget{InfoHash: repeat("b", 40)})

	rr := do(h, "/"+validBlob+"/play/"+tok, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("play: %d, want 302", rr.Code)
	}
	if got := atomic.LoadInt32(&store.resolves); got != 1 {
		t.Errorf("resolves = %d, want 1 — a prompt 'not downloading' must still add", got)
	}
}

// The same guarantee with SEVERAL accounts configured, which is where it was actually broken.
//
// Both other /play fixtures use a single store, and at one store the budget slice is the whole budget —
// so the gate looked fine while, on any multi-account install, a slow first store timed out on its slice
// and a fast second store answered "no" early, leaving budget on the clock. The handler read that as a
// definitive answer and queued a second copy of a torrent the first store was already fetching:
// measured 404 dead_link plus one add per configured store.
func TestPlay_multiStoreStatusTimeoutDoesNotAdd(t *testing.T) {
	saved := statusBudget
	statusBudget = 150 * time.Millisecond
	t.Cleanup(func() { statusBudget = saved })

	// TorBox is slow once (the big account listing), then reports the download.
	slow := &slowStatusStore{svc: ServiceTorBox, slowCalls: 1}
	// The other accounts answer at once and know nothing, which is what used to end the pool early.
	var quickResolves int32
	quick := func(svc DebridService) Store {
		return &countingResolveStore{Store: &slowStatusStore{svc: svc, neverDownloading: true}, n: &quickResolves}
	}
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store {
			return []Store{slow, quick(ServiceRealDebrid), quick(ServicePremiumize)}
		}
	}))
	tok := encodePlayToken(PlayTarget{InfoHash: repeat("a", 40)})

	rr := do(h, "/"+validBlob+"/play/"+tok, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("play: %d, want 202 — TorBox is downloading it", rr.Code)
	}
	if got := atomic.LoadInt32(&slow.resolves) + atomic.LoadInt32(&quickResolves); got != 0 {
		t.Errorf("a store that ran out of time still led to %d resolves — one add per configured account", got)
	}
}

// countingResolveStore counts resolves so a test can prove no store was asked to queue anything.
type countingResolveStore struct {
	Store
	n *int32
}

func (c *countingResolveStore) Resolve(ctx context.Context, rt ResolveTarget) (string, error) {
	atomic.AddInt32(c.n, 1)
	return c.Store.Resolve(ctx, rt)
}
