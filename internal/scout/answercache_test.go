package scout

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type askCountingScraper struct {
	base  string
	calls *int
	fail  bool
}

type heldAnswerScraper struct {
	base    string
	calls   *atomic.Int32
	started chan<- struct{}
	release <-chan struct{}
}

func (s heldAnswerScraper) id() Indexer       { return "torrentio" }
func (s heldAnswerScraper) answerKey() string { return keyHash(s.base) }
func (s heldAnswerScraper) scrape(ctx context.Context, _ scrapeQuery) ([]RawStream, error) {
	s.calls.Add(1)
	s.started <- struct{}{}
	select {
	case <-s.release:
		return []RawStream{{InfoHash: repeat("b", 40), Title: "Shared 1080p WEB-DL"}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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

func TestScrapeAllCached_coalescesAProviderQuestionInFlight(t *testing.T) {
	query := scrapeQuery{Type: "movie", IMDb: "tt-flight"}
	release := make(chan struct{})
	started := make(chan struct{}, 6)
	var calls atomic.Int32
	up := heldAnswerScraper{base: "https://configured.example/account-a", calls: &calls,
		started: started, release: release}

	results := make(chan []RawStream, 6)
	for range 6 {
		go func() {
			out, _, _ := scrapeAllCached(context.Background(), []scraper{up}, query, time.Second, nil, time.Minute)
			results <- out
		}()
	}
	<-started
	// With no settled-answer cache, only callers that have joined this in-flight question can avoid a request.
	time.Sleep(10 * time.Millisecond)
	close(release)
	got := make([][]RawStream, 0, 6)
	for range 6 {
		answer := <-results
		if len(answer) != 1 {
			t.Fatalf("shared answer has %d releases, want 1", len(answer))
		}
		got = append(got, answer)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("same provider and title started %d indexer requests, want 1", got)
	}
	got[0][0].Title = "caller-local annotation"
	if got[1][0].Title == got[0][0].Title {
		t.Fatal("shared callers received the same mutable RawStream slice")
	}
}

func TestScrapeAllCached_keepsProviderIdentitiesApart(t *testing.T) {
	query := scrapeQuery{Type: "movie", IMDb: "tt-identities"}
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	var calls atomic.Int32
	one := heldAnswerScraper{base: "https://configured.example/account-a", calls: &calls,
		started: started, release: release}
	two := heldAnswerScraper{base: "https://configured.example/account-b", calls: &calls,
		started: started, release: release}

	done := make(chan struct{}, 2)
	for _, up := range []scraper{one, two} {
		go func(up scraper) {
			scrapeAllCached(context.Background(), []scraper{up}, query, time.Second, NewMemoryCache(1<<20), time.Minute)
			done <- struct{}{}
		}(up)
	}
	<-started
	<-started
	if got := calls.Load(); got != 2 {
		t.Fatalf("two configured identities started %d requests, want 2", got)
	}
	close(release)
	<-done
	<-done
}

func TestScrapeAllCached_oneCallerLeavingDoesNotCancelTheSharedQuestion(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	query := scrapeQuery{Type: "movie", IMDb: "tt-leaving"}
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	up := heldAnswerScraper{base: "https://configured.example/account-a", calls: &calls,
		started: started, release: release}

	leaving, cancel := context.WithCancel(context.Background())
	left := make(chan []RawStream, 1)
	go func() {
		out, _, _ := scrapeAllCached(leaving, []scraper{up}, query, time.Second, cache, time.Minute)
		left <- out
	}()
	<-started
	cancel()
	if got := <-left; len(got) != 0 {
		t.Fatalf("the caller that left got %d releases, want none", len(got))
	}

	stayed := make(chan []RawStream, 1)
	go func() {
		out, _, _ := scrapeAllCached(context.Background(), []scraper{up}, query, time.Second, cache, time.Minute)
		stayed <- out
	}()
	close(release)
	if got := <-stayed; len(got) != 1 {
		t.Fatalf("the caller that stayed got %d releases, want 1", len(got))
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("leaving restarted the shared provider request: %d calls", got)
	}
}

func TestScrapeAllCached_doesNotStartDetachedWorkForACanceledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	up := askCountingScraper{base: "https://configured.example/canceled", calls: &calls}
	out, ok, complete := scrapeAllCached(ctx, []scraper{up}, scrapeQuery{Type: "movie", IMDb: "tt-canceled"},
		time.Second, nil, time.Minute)
	if len(out) != 0 || ok || complete || calls != 0 {
		t.Fatalf("canceled scrape = %d releases, ok=%v complete=%v calls=%d", len(out), ok, complete, calls)
	}
}
