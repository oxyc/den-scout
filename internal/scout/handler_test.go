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
