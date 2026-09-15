package scout

import (
	"context"
	"errors"
	"testing"
	"time"
)

type askCountingScraper struct {
	base  string
	calls *int
	fail  bool
}

func (c askCountingScraper) id() Indexer       { return "torrentio" }
func (c askCountingScraper) answerKey() string { return keyHash(c.base) }
func (c askCountingScraper) scrape(context.Context, scrapeQuery) ([]RawStream, error) {
	*c.calls++
	if c.fail {
		return nil, errors.New("indexer down")
	}
	return []RawStream{{InfoHash: repeat("a", 40), Title: "Movie 1080p WEB-DL"}}, nil
}

// An indexer is asked once per title while its answer is fresh, whichever list is built from it; a failure is
// never kept, so the next build asks again.
func TestScrapeAllCached_keepsAnswersNotFailures(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	ctx := context.Background()
	movie := scrapeQuery{Type: "movie", IMDb: "tt1"}

	calls := 0
	up := askCountingScraper{base: "https://torrentio.example", calls: &calls}
	for range 2 {
		out, ok, complete := scrapeAllCached(ctx, []scraper{up}, movie, time.Second, cache, time.Minute)
		if len(out) != 1 || !ok || !complete {
			t.Fatalf("got %d releases, ok=%v complete=%v", len(out), ok, complete)
		}
	}
	if calls != 1 {
		t.Errorf("asked %d times for one title, want 1", calls)
	}
	episode := scrapeQuery{Type: "series", IMDb: "tt1", Season: 1, Episode: 2, HasEp: true}
	scrapeAllCached(ctx, []scraper{up}, episode, time.Second, cache, time.Minute)
	if calls != 2 {
		t.Errorf("asked %d times after another title, want 2", calls)
	}

	failures := 0
	down := askCountingScraper{base: "https://down.example", calls: &failures, fail: true}
	for range 2 {
		if _, ok, _ := scrapeAllCached(ctx, []scraper{down}, movie, time.Second, cache, time.Minute); ok {
			t.Error("a failed indexer reported an answer")
		}
	}
	if failures != 2 {
		t.Errorf("a failing indexer was asked %d times, want 2: its failure must not be kept", failures)
	}
}
