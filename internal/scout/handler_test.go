package scout

import (
	"context"
	"encoding/json"
	"fmt"
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

// A cached entry written by an older build has no completeness recorded, and must be read as INCOMPLETE.
//
// A redeploy is exactly when short lists are in flight. Claiming completeness for one served it with a
// five-minute max-age and a day of stale-if-error — the harm the three-part format exists to prevent,
// reached through the one branch that handles entries surviving a deploy.
func TestSplitCached_readsAnOlderEntryConservatively(t *testing.T) {
	complete, etag, body := splitCached("W/\"abc\"\x00{\"streams\":[]}")
	if complete {
		t.Error("an entry with no completeness recorded must not be assumed complete")
	}
	if etag != "W/\"abc\"" || body != "{\"streams\":[]}" {
		t.Errorf("legacy entry mis-split: etag=%q body=%q", etag, body)
	}

	// And the round trip of the current format.
	for _, want := range []bool{true, false} {
		got, e, b := splitCached(joinCached(want, "E", "B"))
		if got != want || e != "E" || b != "B" {
			t.Errorf("round trip complete=%v: got %v %q %q", want, got, e, b)
		}
	}
	// A body containing the separator must not be truncated.
	_, _, weird := splitCached(joinCached(true, "E", "a\x00b"))
	if weird != "a\x00b" {
		t.Errorf("body with a separator was truncated: %q", weird)
	}
}
