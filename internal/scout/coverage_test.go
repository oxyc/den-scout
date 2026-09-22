package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// envelopeOf splits a stream-list body into its `streams` array and its `den` envelope.
func envelopeOf(t *testing.T, body string) (json.RawMessage, answerEnvelope) {
	t.Helper()
	var r struct {
		Streams json.RawMessage `json:"streams"`
		Den     *answerEnvelope `json:"den"`
	}
	if err := json.Unmarshal([]byte(body), &r); err != nil || r.Den == nil {
		t.Fatalf("no den envelope (err %v) in %s", err, body)
	}
	return r.Streams, *r.Den
}

// `empty` is the one kind that asserts a negative, so it needs every source to have answered and nothing
// degraded; anything short of that with nothing to serve is `unknown`.
func TestDecideAnswerKind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		streams  int
		complete bool
		degraded string
		want     string
	}{
		{"all answered, none found", 0, true, "", answerEmpty},
		{"one source missing, none found", 0, false, "", answerUnknown},
		{"every source failed", 0, false, "indexers", answerUnknown},
		{"all answered, cache check out, none served", 0, true, "cache-check", answerUnknown},
		{"one source missing, releases found", 3, false, "", answerPartial},
		{"all answered, releases found", 3, true, "", answerLive},
		{"all answered, releases found, cache check out", 3, true, "cache-check", answerLive},
	} {
		if got := decideAnswerKind(tc.streams, tc.complete, tc.degraded); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestClassifyScrape(t *testing.T) {
	asked := fakeScraper{name: "torrentio"}
	for _, tc := range []struct {
		name string
		sc   scraper
		err  error
		want string
	}{
		{"answered", asked, nil, outcomeAnswered},
		{"deadline", asked, fmt.Errorf("wrapped: %w", context.DeadlineExceeded), outcomeTimeout},
		{"queued past the budget", asked, fmt.Errorf("torrentio: queued past the scrape budget: %w",
			context.DeadlineExceeded), outcomeTimeout},
		{"http 408", asked, &httpStatusError{name: "torrentio", code: 408}, outcomeTimeout},
		{"http 429 (or a paused upstream)", asked, &httpStatusError{name: "torrentio", code: 429}, outcomeRefused},
		{"http 403", asked, &httpStatusError{name: "torrentio", code: 403}, outcomeRefused},
		{"http 502", asked, &httpStatusError{name: "torrentio", code: 502}, outcomeUnreachable},
		{"non-public household source", asked, fmt.Errorf("dial: %w", errNotPublic), outcomeRefused},
		{"network failure", asked, errors.New("connection reset"), outcomeUnreachable},
		{"no config URL", unaskableScraper{indexer: "mediafusion"}, errors.New("not asked"), outcomeMisconfigured},
		{"config mint failed", unaskableScraper{indexer: "mediafusion", transient: true}, errors.New("not asked"),
			outcomeUnreachable},
	} {
		if got := classifyScrape(tc.sc, tc.err); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every scraper appears in the coverage with what it did, and completeness follows from those outcomes.
func TestScrapeCovered(t *testing.T) {
	release := []RawStream{{InfoHash: repeat("a", 40), Title: "Movie 1080p"}}
	found := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) { return release, nil }}
	none := fakeScraper{"comet", func(context.Context) ([]RawStream, error) { return nil, nil }}
	hang := fakeScraper{"mediafusion", func(ctx context.Context) ([]RawStream, error) { <-ctx.Done(); return nil, ctx.Err() }}
	unconfigured := unaskableScraper{indexer: "mediafusion"}

	for _, tc := range []struct {
		name     string
		scrapers []scraper
		complete bool
		outcomes []string
		kind     string
	}{
		{"all answered, none found", []scraper{none, unconfigured}, true,
			[]string{outcomeAnswered, outcomeMisconfigured}, answerEmpty},
		{"one timed out, none found", []scraper{none, hang}, false,
			[]string{outcomeAnswered, outcomeTimeout}, answerUnknown},
		{"one timed out, releases found", []scraper{found, hang}, false,
			[]string{outcomeAnswered, outcomeTimeout}, answerPartial},
		{"all answered, releases found", []scraper{found, none}, true,
			[]string{outcomeAnswered, outcomeAnswered}, answerLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := scrapeCovered(context.Background(), tc.scrapers, scrapeQuery{}, 30*time.Millisecond, nil, 0)
			if res.complete != tc.complete {
				t.Errorf("complete = %v, want %v", res.complete, tc.complete)
			}
			if len(res.sources) != len(tc.scrapers) {
				t.Fatalf("%d sources in coverage, want every one of %d", len(res.sources), len(tc.scrapers))
			}
			for i, want := range tc.outcomes {
				s := res.sources[i]
				if s.ID != string(tc.scrapers[i].id()) || s.Outcome != want {
					t.Errorf("source %d = %s %s, want %s %s", i, s.ID, s.Outcome, tc.scrapers[i].id(), want)
				}
				if want == outcomeAnswered && s.ObservedAt.IsZero() {
					t.Errorf("source %d answered with no time", i)
				}
			}
			degraded := ""
			if !res.anyOK {
				degraded = "indexers"
			}
			if got := decideAnswerKind(len(res.seeds), res.complete, degraded); got != tc.kind {
				t.Errorf("answerKind = %q, want %q", got, tc.kind)
			}
		})
	}
}

// An answer reused from the indexer answer cache is an answer, marked as reused.
func TestScrapeCovered_reusedAnswerIsMarked(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	sc := &stremioScraper{indexer: "torrentio", baseURL: "https://torrentio.example"}
	keepAnswer(cache, time.Minute, sc, scrapeQuery{Type: "movie", IMDb: "tt1"}, testSeeds())
	res := scrapeCovered(context.Background(), []scraper{sc}, scrapeQuery{Type: "movie", IMDb: "tt1"}, time.Second,
		cache, time.Minute)
	if s := res.sources[0]; s.Outcome != outcomeAnswered || !s.Cached || s.Items != 3 || !res.complete {
		t.Errorf("reused answer: %+v, complete %v", s, res.complete)
	}
}

// Through the handler: the body carries the envelope, the header and the envelope agree, and a pass with a
// silent source never says `empty`.
func TestStream_envelopeReportsCoverage(t *testing.T) {
	answersEmpty := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) { return nil, nil }}
	answersSeeds := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) { return testSeeds(), nil }}
	timesOut := fakeScraper{"comet", func(ctx context.Context) ([]RawStream, error) { <-ctx.Done(); return nil, ctx.Err() }}

	for _, tc := range []struct {
		name     string
		scrapers []scraper
		kind     string
		degraded string
		complete bool
		outcomes []string
		streams  bool
	}{
		{"all answered, none found", []scraper{answersEmpty}, answerEmpty, "", true,
			[]string{outcomeAnswered}, false},
		{"one timed out, none found", []scraper{answersEmpty, timesOut}, answerUnknown, "indexers", false,
			[]string{outcomeAnswered, outcomeTimeout}, false},
		{"one timed out, releases found", []scraper{answersSeeds, timesOut}, answerPartial, "", false,
			[]string{outcomeAnswered, outcomeTimeout}, true},
		{"all answered, releases found", []scraper{answersSeeds}, answerLive, "", true,
			[]string{outcomeAnswered}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(testDeps(func(d *Deps) {
				d.ScrapeTimeout = 30 * time.Millisecond
				d.MakeScrapers = func(*Config) []scraper { return tc.scrapers }
			}))
			rr := do(h, "/"+validBlob+"/stream/movie/tt1234567.json", nil)
			streams, env := envelopeOf(t, rr.Body.String())
			if env.V != answerEnvelopeVersion || env.AnswerKind != tc.kind || env.Coverage.Complete != tc.complete {
				t.Errorf("envelope = %+v, want %s, complete %v", env, tc.kind, tc.complete)
			}
			if header := rr.Header().Get("X-Den-Degraded"); header != tc.degraded || env.Degraded != header {
				t.Errorf("X-Den-Degraded %q, envelope degraded %q, want both %q", header, env.Degraded, tc.degraded)
			}
			if env.GeneratedAt.IsZero() || (tc.degraded == "") == env.ExpiresAt.IsZero() {
				t.Errorf("generatedAt %v, expiresAt %v: a cached list says when it expires, an uncached one does not",
					env.GeneratedAt, env.ExpiresAt)
			}
			if len(env.Coverage.Sources) != len(tc.outcomes) {
				t.Fatalf("coverage %+v, want %d sources", env.Coverage.Sources, len(tc.outcomes))
			}
			for i, want := range tc.outcomes {
				if got := env.Coverage.Sources[i].Outcome; got != want {
					t.Errorf("source %d outcome %q, want %q", i, got, want)
				}
			}
			// What a client that ignores `den` reads is still a plain Stremio stream list.
			var list []streamOut
			if err := json.Unmarshal(streams, &list); err != nil || list == nil || (len(list) > 0) != tc.streams {
				t.Errorf("streams = %s (err %v)", streams, err)
			}
		})
	}
}

// An indexer the config names but this build has disabled is listed, not dropped, and does not hold the
// list back from being complete.
func TestStream_quarantinedIndexerAppearsInCoverage(t *testing.T) {
	cfg := blob(`{"debrid":[{"service":"torbox","token":"tb-secret"}],"indexers":["torrentio","torz"]}`)
	h := NewHandler(testDeps(nil))
	rr := do(h, "/"+cfg+"/stream/movie/tt1234567.json", nil)
	_, env := envelopeOf(t, rr.Body.String())
	if !env.Coverage.Complete || env.AnswerKind != answerLive {
		t.Errorf("envelope = %+v, want live and complete", env)
	}
	var torz *sourceReport
	for i := range env.Coverage.Sources {
		if env.Coverage.Sources[i].ID == "torz" {
			torz = &env.Coverage.Sources[i]
		}
	}
	if torz == nil || torz.Outcome != outcomeQuarantined {
		t.Errorf("torz in coverage: %+v, want quarantined", env.Coverage.Sources)
	}
}
