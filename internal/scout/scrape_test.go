package scout

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

const torrentioFixture = `{"streams":[
{"name":"Torrentio\n4k HDR","title":"The.Movie.2024.2160p.WEB-DL.DDP5.1.DV.HDR.HEVC-GROUP\n👤 142 💾 18.4 GB ⚙️ ThePirateBay","infoHash":"AABBCCDDEEFF00112233445566778899AABBCCDD","fileIdx":0},
{"name":"Torrentio","title":"already resolved somehow"},
{"infoHash":"bbbbccddeeff00112233445566778899aabbccdd","behaviorHints":{"filename":"Exact.Name.mkv","videoSize":2048}},
"garbage"
]}`

func TestParseStremioStreams(t *testing.T) {
	seeds := parseStremioStreams([]byte(torrentioFixture), "torrentio")
	if len(seeds) != 2 { // url-only + garbage dropped
		t.Fatalf("want 2 seeds, got %d: %+v", len(seeds), seeds)
	}
	if seeds[0].InfoHash != "aabbccddeeff00112233445566778899aabbccdd" || intOr(seeds[0].Seeders, 0) != 142 || seeds[0].FileIdx == nil {
		t.Errorf("seed0: %+v", seeds[0])
	}
	if seeds[0].SizeBytes == nil || *seeds[0].SizeBytes != gibBytes(18.4) {
		t.Errorf("seed0 size: %v", seeds[0].SizeBytes)
	}
	if seeds[1].Title != "Exact.Name.mkv" || seeds[1].SizeBytes == nil || *seeds[1].SizeBytes != 2048 {
		t.Errorf("seed1 (behaviorHints): %+v", seeds[1])
	}
}

func TestParseSizeSeeders(t *testing.T) {
	if s := parseSize("👤 50 💾 15.2 GB"); s == nil || *s != gibBytes(15.2) {
		t.Errorf("parseSize GB: %v", s)
	}
	if s := parseSize("700 MB"); s == nil || *s != 700*mib {
		t.Errorf("parseSize MB: %v", s)
	}
	if parseSize("no size") != nil {
		t.Error("parseSize none")
	}
	if s := parseSeeders("👤 123 ⚙️ prov"); s == nil || *s != 123 {
		t.Errorf("parseSeeders emoji: %v", s)
	}
	if s := parseSeeders("Seeders: 7"); s == nil || *s != 7 {
		t.Errorf("parseSeeders word: %v", s)
	}
	if parseSeeders("none") != nil {
		t.Error("parseSeeders none")
	}
}

func TestScraperURL(t *testing.T) {
	var seenURL, seenUA string
	client := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		seenURL = r.URL.String()
		seenUA = r.Header.Get("user-agent")
		return resp(200, `{"streams":[]}`), nil
	}}
	sc := &stremioScraper{indexer: "torrentio", baseURL: "https://torrentio.strem.fun", client: client}
	_, _ = sc.scrape(context.Background(), scrapeQuery{Type: "movie", IMDb: "tt1234567"})
	if seenURL != "https://torrentio.strem.fun/stream/movie/tt1234567.json" {
		t.Errorf("movie url: %s", seenURL)
	}
	// A non-default User-Agent is required — Torrentio 403s Go-http-client (regression guard).
	if seenUA == "" || strings.Contains(seenUA, "Go-http-client") {
		t.Errorf("scrape must send a browser-like User-Agent, got %q", seenUA)
	}
	sc2 := &stremioScraper{indexer: "comet", baseURL: "https://comet.example/", client: client}
	_, _ = sc2.scrape(context.Background(), scrapeQuery{Type: "series", IMDb: "tt99", Season: 2, Episode: 5, HasEp: true})
	if seenURL != "https://comet.example/stream/series/tt99%3A2%3A5.json" {
		t.Errorf("series url: %s", seenURL)
	}
	// non-200 → error (fan-out treats as no result)
	bad := &stremioScraper{indexer: "torz", baseURL: "https://torz.example", client: mockDoer{fn: func(*http.Request) (*http.Response, error) { return resp(502, "{}"), nil }}}
	if _, err := bad.scrape(context.Background(), scrapeQuery{Type: "movie", IMDb: "tt1"}); err == nil {
		t.Error("non-200 should error")
	}
}

// Torrentio is asked on its BARE path, whatever the filters say.
//
// The options segment was free to add and expensive to keep: with torrentio's origin down, Cloudflare
// answers from `stale-if-error` only for a URL it already holds, and a private options path is warm
// essentially never. Scout collected 502s for an episode whose bare path served 50 releases to a curl on
// the same machine. Nothing is lost — rankStreams drops cam/scr itself and applies its own ordering.
func TestMakeScrapersTorrentioUsesBarePath(t *testing.T) {
	cfg := &Config{Indexers: []Indexer{"torrentio", "mediafusion"}, Filters: Filters{ExcludeCam: true}}
	got := makeScrapers(cfg, mockDoer{}, map[Indexer]string{"mediafusion": "https://mf.self/CONFIG"})
	tor := got[0].(*stremioScraper)
	if tor.baseURL != "https://torrentio.strem.fun" {
		t.Errorf("torrentio base: %s", tor.baseURL)
	}
	mf := got[1].(*stremioScraper)
	if mf.baseURL != "https://mf.self/CONFIG" {
		t.Errorf("mediafusion override: %s", mf.baseURL)
	}
	// The filter setting must not change the URL at all — that coupling was the whole defect.
	off := makeScrapers(&Config{Indexers: []Indexer{"torrentio"}, Filters: Filters{ExcludeCam: false}}, mockDoer{}, nil)
	if off[0].(*stremioScraper).baseURL != "https://torrentio.strem.fun" {
		t.Errorf("excludeCam off: %s", off[0].(*stremioScraper).baseURL)
	}
	// An operator override still wins, so a self-hosted torrentio can be pointed at.
	custom := makeScrapers(cfg, mockDoer{}, map[Indexer]string{"torrentio": "https://tor.self/opts"})
	if custom[0].(*stremioScraper).baseURL != "https://tor.self/opts" {
		t.Errorf("override: %s", custom[0].(*stremioScraper).baseURL)
	}
}

type fakeScraper struct {
	name Indexer
	fn   func(context.Context) ([]RawStream, error)
}

func (f fakeScraper) id() Indexer { return f.name }
func (f fakeScraper) scrape(ctx context.Context, _ scrapeQuery) ([]RawStream, error) {
	return f.fn(ctx)
}

func TestScrapeAll(t *testing.T) {
	ok := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
		return []RawStream{{InfoHash: repeat("a", 40), Title: "first", Seeders: intp(10)}}, nil
	}}
	boom := fakeScraper{"comet", func(context.Context) ([]RawStream, error) { return nil, context.Canceled }}
	hang := fakeScraper{"torz", func(ctx context.Context) ([]RawStream, error) { <-ctx.Done(); return nil, ctx.Err() }}
	out, anyOK, complete := scrapeAll(context.Background(), []scraper{ok, boom, hang}, scrapeQuery{}, 30*time.Millisecond)
	if len(out) != 1 || out[0].InfoHash != repeat("a", 40) {
		t.Errorf("scrapeAll gather-what-responded: %+v", out)
	}
	// A partial NON-EMPTY answer is still worth serving and worth caching — whatever came back is real.
	// Marking it non-authoritative put the full scrape and a fresh debrid fan-out on every request while
	// one indexer was flaky, which is a worse answer than the slightly short list it was protecting
	// against. What it is NOT is complete, and that is carried separately so it can be held for less time.
	if !anyOK {
		t.Error("a non-empty list is real; refusing to trust it re-scrapes on every request")
	}
	if complete {
		t.Error("two of three indexers failed; the list cannot be described as complete")
	}

	// Every indexer answering IS complete, empty or not — otherwise nothing would ever be cached in full.
	quiet := fakeScraper{"mediafusion", func(context.Context) ([]RawStream, error) { return nil, nil }}
	if _, _, whole := scrapeAll(context.Background(), []scraper{ok, quiet}, scrapeQuery{}, 30*time.Millisecond); !whole {
		t.Error("every indexer answered; that is a complete list")
	}

	// every scraper failing → anyOK false (a degraded blip, not a genuine empty)
	if _, ok, _ := scrapeAll(context.Background(), []scraper{boom}, scrapeQuery{}, 30*time.Millisecond); ok {
		t.Error("anyOK should be false when every scraper failed")
	}

	// dedupe merges facts across indexers
	a := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
		return []RawStream{{InfoHash: repeat("h", 40), Title: "first", Seeders: intp(10)}}, nil
	}}
	b := fakeScraper{"comet", func(context.Context) ([]RawStream, error) {
		return []RawStream{{InfoHash: repeat("h", 40), Title: "second", Seeders: intp(99), FileIdx: intp(3), SizeBytes: intp(500)}}, nil
	}}
	merged, _, _ := scrapeAll(context.Background(), []scraper{a, b}, scrapeQuery{}, time.Second)
	if len(merged) != 1 || merged[0].Title != "first" || intOr(merged[0].Seeders, 0) != 99 || merged[0].FileIdx == nil || merged[0].SizeBytes == nil {
		t.Errorf("dedupe merge: %+v", merged[0])
	}
}

func gibBytes(f float64) int { return int(f*float64(gib) + 0.5) }

// A scrape that spends its whole budget queueing behind the limiter must not then ask the indexer.
//
// The request cannot succeed — its context is already dead — and its failure was reported as
// "indexer unreachable", so scout's own queue was logged and counted as the upstream's outage. That is
// the difference between "we could not get to it in time" and "it is down", and only the second is a
// reason to go looking at the indexer.
func TestScrape_aBudgetSpentQueueingIsNotAnIndexerOutage(t *testing.T) {
	const host = "queued-past-budget.example"
	// Drain the burst so the next caller genuinely has to wait for a token. These are free by
	// definition — the burst exists so a household's normal use never queues.
	for range 30 {
		indexerLimiter.wait(context.Background(), host)
	}

	var asked bool
	sc := &stremioScraper{indexer: "torrentio", baseURL: "https://" + host, client: mockDoer{
		fn: func(*http.Request) (*http.Response, error) {
			asked = true
			return resp(200, `{"streams":[]}`), nil
		}}}

	// Shorter than the ~1s the drained bucket now makes a caller wait.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	streams, err := sc.scrape(ctx, scrapeQuery{IMDb: "tt0111161", Type: "movie"})

	if asked {
		t.Error("asked the indexer with a context that had already expired")
	}
	if err == nil {
		t.Fatalf("a scrape that never happened must report an error, got %d streams", len(streams))
	}
	// The caller still learns nothing came back — that is what keeps an empty result non-authoritative.
	// What it must be able to tell is WHY, so a deadline is not filed against the indexer's health.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the budget running out must be reported as such, got %v", err)
	}
}
