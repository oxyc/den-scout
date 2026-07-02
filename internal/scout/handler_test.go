package scout

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	req := httptest.NewRequest(http.MethodGet, "https://scout.example"+path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRoutesPagesAndManifest(t *testing.T) {
	h := NewHandler(testDeps(nil))
	if rr := do(h, "/configure", nil); rr.Code != 200 || !strings.Contains(rr.Body.String(), "Configure Den Scout") || rr.Header().Get("cache-control") != staticCache {
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
