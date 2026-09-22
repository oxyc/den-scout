package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Indexer scrapers (ported from src/scrape/*). One shared Stremio-protocol client; fan-out with a
// per-indexer timeout, gather-what-responded, dedupe by infohash.

const maxScrapeBytes = 2 << 20 // 2 MiB cap on an addon response body (a stream list is far smaller)

// scrapeUserAgent is a browser-like UA: Torrentio & co. 403 the default Go-http-client signature.
const scrapeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// doer is the injectable HTTP client (an *http.Client, or a test double).
type doer interface {
	Do(*http.Request) (*http.Response, error)
}

type scrapeQuery struct {
	Type    string
	IMDb    string
	Season  int
	Episode int
	HasEp   bool
}

type scraper interface {
	id() Indexer
	scrape(ctx context.Context, q scrapeQuery) ([]RawStream, error)
}

// --- shared parse ---

type wireStream struct {
	Name          string `json:"name"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	InfoHash      string `json:"infoHash"`
	URL           string `json:"url"`
	FileIdx       *int   `json:"fileIdx"`
	BehaviorHints *struct {
		Filename  string `json:"filename"`
		VideoSize *int   `json:"videoSize"`
	} `json:"behaviorHints"`
}

var (
	sizeRe     = regexp.MustCompile(`(?i)(?:💾\s*)?([0-9.]+)\s*(gib|gb|mib|mb)\b`)
	seedEmoji  = regexp.MustCompile(`(?:👤|👥)\s*(\d+)`)
	seedWord   = regexp.MustCompile(`(?i)seed(?:ers)?[:\s]+(\d+)`)
	hashNorm   = regexp.MustCompile(`^[a-z0-9]{40}$|^[a-z0-9]{32}$`)
	titleToken = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|remux|bluray|web[ .\-_]?dl|web[ .\-_]?rip|hdtv|x264|x265|hevc)\b`)
)

func parseSize(text string) *int {
	m := sizeRe.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 {
		return nil
	}
	unit := strings.ToLower(m[2])
	var bytes float64
	if strings.HasPrefix(unit, "g") {
		bytes = v * gib
	} else {
		bytes = v * mib
	}
	n := int(bytes + 0.5)
	return &n
}

func parseSeeders(text string) *int {
	m := seedEmoji.FindStringSubmatch(text)
	if m == nil {
		m = seedWord.FindStringSubmatch(text)
	}
	if m == nil {
		return nil
	}
	if n, err := strconv.Atoi(m[1]); err == nil {
		return &n
	}
	return nil
}

func normalizeHash(h string) (string, bool) {
	h = strings.ToLower(strings.TrimSpace(h))
	return h, hashNorm.MatchString(h)
}

// hashInPath finds the infohash in a debrid playback link. An addon configured with a debrid account
// answers with links to its own resolver instead of an `infoHash` — MediaFusion's
// `/streaming_provider/<secret>/playback/<provider>/<hash>`, Comet's `/<config>/playback/<hash>/…`,
// Torrentio's `/resolve/<service>/<key>/<hash>/…` — and the hash is all scout needs to check its own
// debrid. Only a whole path segment of 40 hex characters counts, and only in the path: the query is where a
// link would carry a key.
func hashInPath(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil {
		return "", false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if len(seg) == 40 && hexHash.MatchString(seg) {
			return strings.ToLower(seg), true
		}
	}
	return "", false
}

var hexHash = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func firstMeaningfulLine(text string) string {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	for _, l := range lines {
		if titleToken.MatchString(l) {
			return l
		}
	}
	if len(lines) > 0 {
		return lines[0]
	}
	return ""
}

func parseStremioStreams(body []byte, source string) []RawStream {
	var parsed struct {
		Streams []json.RawMessage `json:"streams"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return nil
	}
	var out []RawStream
	for _, raw := range parsed.Streams {
		var s wireStream
		if json.Unmarshal(raw, &s) != nil {
			continue // tolerate a non-object element
		}
		hash, ok := normalizeHash(s.InfoHash)
		if !ok && s.InfoHash == "" {
			hash, ok = hashInPath(s.URL)
		}
		if !ok {
			continue
		}
		// Title/metadata come from the release fields only — NOT s.Name, which is the indexer's label
		// ("Torrentio", "Torrentio\n1080p"): feeding it in made the title fall back to "Torrentio" when
		// no line looked like a release name.
		text := strings.Join(nonEmpty(s.Title, s.Description), "\n")
		title := ""
		if s.BehaviorHints != nil {
			title = strings.TrimSpace(s.BehaviorHints.Filename)
		}
		if title == "" {
			title = firstMeaningfulLine(text)
		}
		if title == "" {
			title = hash
		}
		var size *int
		if s.BehaviorHints != nil && s.BehaviorHints.VideoSize != nil && *s.BehaviorHints.VideoSize > 0 {
			size = s.BehaviorHints.VideoSize
		} else {
			size = parseSize(text)
		}
		var fileIdx *int
		if s.FileIdx != nil && *s.FileIdx >= 0 {
			fileIdx = s.FileIdx
		}
		out = append(out, RawStream{
			InfoHash:  hash,
			FileIdx:   fileIdx,
			Title:     title,
			SizeBytes: size,
			Seeders:   parseSeeders(text),
			Source:    source,
		})
	}
	return out
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// --- Stremio addon scraper ---

type stremioScraper struct {
	indexer Indexer
	baseURL string
	client  doer
	// label names the scraper in log lines when its indexer id is shared — a household source's host.
	label string
	// limiter paces the scraper; nil is indexerLimiter.
	limiter *hostLimiter
}

func (s *stremioScraper) id() Indexer { return s.indexer }

func (s *stremioScraper) name() string {
	if s.label != "" {
		return string(s.indexer) + " source " + s.label
	}
	return string(s.indexer)
}

// A shed request is not an answer.
//
// Opening a season asks for every episode at once, so an indexer sees a burst and sheds part of it —
// torrentio answers 502 to some of them. Treated as a result, that turns into "no source found for this
// episode" for a release the same indexer serves 50 of a second later. These statuses are the indexer
// declining to answer right now, so the request is made again rather than reported as an outcome.
//
// Not a 429. That is the indexer saying it is rate limiting, and asking again inside the same second only spends
// more of the limit; the transport pauses the host instead, for as long as it asked (upstreampause.go). The same
// goes for a shed request carrying a Retry-After: the transport has paused the host, so a retry would be answered
// by that pause.
func retryableScrapeStatus(status int) bool {
	return status == http.StatusRequestTimeout || status >= 500
}

// Short and jittered: the whole scrape is under an 8 s budget shared with three other indexers, so this
// buys a second chance without spending someone else's timeout. Jitter because a burst that is shed
// together would otherwise retry together and be shed together again.
var scrapeRetryBackoff = []time.Duration{250 * time.Millisecond, 700 * time.Millisecond}

// Sized against the largest batch the client actually issues, not picked round.
//
// Den's poster grid fires up to 8 availability checks at once, so a burst of 6 made every poster screen
// pay a queue for no benefit — the limiter is not there to tax the normal case. 10 covers that batch plus
// a focus dwell and a play.
//
// The sustained rate is a runaway detector, not a demand ceiling. A heavy evening is a few hundred
// requests an hour; 1/s per host is still an order of magnitude above that, while a loop that re-asks
// forever — the documented failure here — meets a wall it can be seen hitting. Per host, so a slow
// indexer cannot delay a healthy one.
// Burst sized to a household's largest legitimate burst — opening a season is one request per episode,
// and 10 turned a 24-episode show into fourteen seconds of queueing against an eight-second budget. The
// sustained rate is what actually protects the indexer; the burst only decides whether normal use waits.
var indexerLimiter = newHostLimiter(time.Second, 30)

func (s *stremioScraper) scrape(ctx context.Context, q scrapeQuery) ([]RawStream, error) {
	// One token per logical scrape, not per attempt. Pacing inside the retry loop spent up to three, and
	// spent the extra two exactly when the host was already shedding — so the limiter throttled hardest
	// during the one failure it exists for, and each abandoned wait left its token spent, deepening the
	// debt for the next caller. The jittered backoff below already paces the retries.
	limiter := s.limiter
	if limiter == nil {
		limiter = indexerLimiter
	}
	if parsed, perr := url.Parse(strings.TrimRight(s.baseURL, "/")); perr == nil {
		limiter.wait(ctx, parsed.Host)
	}
	// The limiter returns two different ways — the token arrived, or the caller's deadline passed — and
	// only one of them means "go ahead". Falling through on the second sends a request that cannot
	// succeed and then reports its failure as `indexer unreachable`: scout's own queue, logged and
	// counted as the upstream's outage. The caller still learns nothing came back, which is true and is
	// what keeps an empty result non-authoritative; it just stops learning it as a lie about the indexer.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: queued past the scrape budget: %w", s.name(), err)
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		streams, err, retryable := s.scrapeOnce(ctx, q)
		if err == nil {
			if attempt > 0 {
				logLimited("indexer-retry:"+string(s.indexer), "%s indexer answered on retry %d", s.name(), attempt)
			}
			return streams, nil
		}
		lastErr = err
		if !retryable || attempt >= len(scrapeRetryBackoff) {
			return nil, lastErr
		}
		delay := scrapeRetryBackoff[attempt]
		delay += time.Duration(rand.Int63n(int64(delay / 2)))
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(delay):
		}
	}
}

// scrapeOnce performs one request. The third return says whether the failure is worth another attempt.
func (s *stremioScraper) scrapeOnce(ctx context.Context, q scrapeQuery) ([]RawStream, error, bool) {
	stremID := q.IMDb
	if q.HasEp {
		stremID = fmt.Sprintf("%s:%d:%d", q.IMDb, q.Season, q.Episode)
	}
	// The id goes in as Stremio clients send it, colons and all (`tt123:2:1`), not percent-encoded. It needs no
	// escaping — an IMDb id and two numbers — and the encoded form is a different URL to a CDN: with torrentio's
	// origin down, Cloudflare answers from `stale-if-error` only for the URL the whole world requests, so every
	// series episode came back 521 while the same `tt24022296:2:1` served five releases to a plain curl.
	u := strings.TrimRight(s.baseURL, "/") + "/stream/" + q.Type + "/" + stremID + ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err, false // a malformed request will be malformed again
	}
	req.Header.Set("accept", "application/json")
	// Torrentio (and peers) 403 the default Go-http-client User-Agent as a bot signature — send a
	// browser UA so the scrape isn't rejected. Without this every indexer returns 403 → zero streams.
	req.Header.Set("user-agent", scrapeUserAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		// Log the indexer name + reason (never the URL — MediaFusion's carries its encrypted config) so a
		// scrape outage is visible in the server log instead of silently becoming an empty stream list.
		// Once a minute per indexer: an outage fails every scrape, and the line only has to say it is down.
		logLimited("indexer-unreachable:"+string(s.indexer), "%s indexer unreachable", s.name())
		// A refused address will be refused again; a retry would only spend the budget.
		return nil, err, !errors.Is(err, errNotPublic)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		logLimited("indexer-status:"+string(s.indexer), "%s indexer returned http %d", s.name(), resp.StatusCode)
		return nil, &httpStatusError{name: s.name(), code: resp.StatusCode},
			retryableScrapeStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxScrapeBytes))
	if err != nil {
		return nil, err, true
	}
	return parseStremioStreams(body, string(s.indexer)), nil, false
}

// httpStatusError is an indexer answering with a status other than 200, kept typed so coverage can tell a
// refusal from an outage.
type httpStatusError struct {
	name string
	code int
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("%s http %d", e.name, e.code) }

// --- fan-out + dedupe ---

var defaultIndexerURLs = map[Indexer]string{
	"torrentio":   "https://torrentio.strem.fun",
	"comet":       "https://comet.elfhosted.com",
	"mediafusion": "https://mediafusion.elfhosted.com",
	"torz":        "https://torz.strem.fun",
}

func makeScrapers(config *Config, client, sourceClient doer, urls map[Indexer]string) []scraper {
	out := make([]scraper, 0, len(config.Indexers)+len(config.Sources))
	// Household sources are asked like any indexer, and count in the quorum like one: the household chose
	// them, so a silent one leaves the list incomplete. Fetched only through sourceClient, which refuses to
	// connect anywhere but the public internet.
	for _, base := range config.Sources {
		out = append(out, &stremioScraper{indexer: ownSources, baseURL: base, client: sourceClient,
			label: sourceHost(base), limiter: sourceLimiter})
	}
	for _, id := range config.Indexers {
		// An indexer that needs a per-install config segment and hasn't been given one is not asked. It
		// cannot answer usefully, and its failure is silent in the worst way: mediafusion returns 200
		// with an empty list, which counts as a response and votes on whether a release exists. Leaving
		// it out means a genuine "nobody has this" still requires everyone who WAS asked to say so.
		base := baseURLFor(id, config, urls)
		if envVar, needsPath := configPathIndexers[id]; needsPath && urls[id] == "" {
			// Mint one from the debrid account we already hold, when the operator has allowed it. An
			// explicit URL always wins, so a token-free config that was pasted in stays in use.
			transient := false
			if mintIndexerConfigs {
				base, transient = indexerBaseWithConfig(context.Background(), id, config, client)
			}
			if base == "" || !mintIndexerConfigs {
				// A transient mint failure is an outage, not a misconfiguration — don't tell the operator
				// to configure something that already is. The mint logs its own reason.
				if !transient {
					logIndexerSkipOnce(id, envVar)
				}
				// Kept in the list as a scraper that cannot answer, rather than dropped from it. Dropping
				// removed it from the quorum too, so "an empty result is authoritative only when EVERY
				// indexer answered" silently became "…every indexer we still bother asking" — and with
				// the other two unconfigured, one torrentio 200-empty was again a confident "this release
				// does not exist". That is the same bug the skip was introduced to fix, one level up.
				out = append(out, unaskableScraper{indexer: id, transient: transient})
				continue
			}
		}
		out = append(out, &stremioScraper{indexer: id, baseURL: base, client: client})
	}
	return out
}

// unaskableScraper stands in for a configured indexer that cannot be addressed — it needs a per-install
// config segment and none was supplied or could be minted. It fails without making a request, so it
// costs nothing and still counts as a source that did not answer.
//
// `transient` separates "not configured" from "the mint failed just now". Only the first is excluded
// from the empty-result quorum; a transient failure means we genuinely do not know what this indexer
// would have said, which is exactly the state the quorum exists to detect.
type unaskableScraper struct {
	indexer   Indexer
	transient bool
}

func (s unaskableScraper) id() Indexer { return s.indexer }

func (s unaskableScraper) scrape(context.Context, scrapeQuery) ([]RawStream, error) {
	return nil, fmt.Errorf("%s: no config URL, not asked", s.indexer)
}

// Once per indexer per process: this runs on every request, and the operator needs the line once.
var indexerSkipLogged sync.Map

func logIndexerSkipOnce(id Indexer, envVar string) {
	if _, seen := indexerSkipLogged.LoadOrStore(id, true); seen {
		return
	}
	log.Printf("%s needs a per-install config URL and has none — not querying it. Set %s to the "+
		"address from that addon's /configure page.", id, envVar)
}

func baseURLFor(id Indexer, config *Config, urls map[Indexer]string) string {
	if u, ok := urls[id]; ok && u != "" {
		return u
	}
	// Deliberately no torrentio options segment. Asking for `sort=qualitysize|qualityfilter=cam,scr`
	// bought nothing — `rankStreams` drops cam/scr itself and re-sorts by its own quality score — and it
	// cost everything: with torrentio's origin down, Cloudflare answers from `stale-if-error` only for a
	// URL it already holds. The bare path is warm because the whole world requests it; a private options
	// path is warm essentially never, so scout got a 502 for an episode that served 50 releases to a
	// plain curl from the same machine, and reported it as "no source found".
	return defaultIndexerURLs[id]
}

// scrapeAll runs every scraper concurrently under a per-indexer timeout; drops those that error/time
// out; then dedupes by infohash. The bool reports whether at least one scraper responded — a false
// (every indexer failed/timed out) means an empty result is a degraded blip, not a genuine "no
// torrents", so the caller must not cache it.
// Returns the releases, whether an EMPTY result may be trusted, and whether every askable indexer
// answered. The last is not the same question as the second: a partial non-empty list is worth serving
// and worth caching, just for less time.
func scrapeAll(ctx context.Context, scrapers []scraper, q scrapeQuery, timeout time.Duration) ([]RawStream, bool, bool) {
	return scrapeAllCached(ctx, scrapers, q, timeout, nil, 0)
}

// An indexer's answer to one question, kept apart from every list built from it.
//
// Every install that asks torrentio about a title asks the same URL and gets the same answer, and a list ranked
// for a browser rather than the TV, or filtered differently, is built from those same answers again. So each is
// cached on its own, keyed by what was asked: the indexer's URL, hashed because a MediaFusion URL carries its
// encrypted config, and the title. Answers only, never a failure, and for as long as a list stays fresh.
type cacheableScraper interface{ answerKey() string }

func (s *stremioScraper) answerKey() string { return keyHash(strings.TrimRight(s.baseURL, "/")) }

func answerCacheKey(sc scraper, q scrapeQuery) (string, bool) {
	c, ok := sc.(cacheableScraper)
	if !ok {
		return "", false
	}
	id := q.IMDb
	if q.HasEp {
		id = fmt.Sprintf("%s:%d:%d", q.IMDb, q.Season, q.Episode)
	}
	return "answer:v1:" + c.answerKey() + ":" + q.Type + ":" + id, true
}

func keptAnswer(cache Cache, sc scraper, q scrapeQuery) ([]RawStream, bool) {
	key, ok := answerCacheKey(sc, q)
	if !ok || cache == nil {
		return nil, false
	}
	raw, hit := cache.Get(key)
	if !hit {
		return nil, false
	}
	var r []RawStream
	if json.Unmarshal([]byte(raw), &r) != nil {
		return nil, false
	}
	return r, true
}

func keepAnswer(cache Cache, ttl time.Duration, sc scraper, q scrapeQuery, r []RawStream) {
	key, ok := answerCacheKey(sc, q)
	if !ok || cache == nil || ttl <= 0 {
		return
	}
	if b, err := json.Marshal(r); err == nil {
		cache.Put(key, string(b), ttl)
	}
}

// scrapeAllCached is scrapeAll answering from each indexer's kept answer where there is one, and keeping
// each new one for ttl.
func scrapeAllCached(ctx context.Context, scrapers []scraper, q scrapeQuery, timeout time.Duration, cache Cache,
	ttl time.Duration) ([]RawStream, bool, bool) {
	res := scrapeCovered(ctx, scrapers, q, timeout, cache, ttl)
	return res.seeds, res.anyOK, res.complete
}

// scrapeResult is one pass over every scraper: the deduped releases, whether an empty list may be trusted,
// whether every askable source answered, and what each source did, in scraper order.
type scrapeResult struct {
	seeds    []RawStream
	anyOK    bool
	complete bool
	sources  []sourceReport
}

// scrapeCovered is scrapeAllCached keeping each source's report.
func scrapeCovered(ctx context.Context, scrapers []scraper, q scrapeQuery, timeout time.Duration, cache Cache,
	ttl time.Duration) scrapeResult {
	results := make([][]RawStream, len(scrapers))
	reports := make([]sourceReport, len(scrapers))
	g, gctx := errgroup.WithContext(ctx)
	for i, sc := range scrapers {
		i, sc := i, sc
		reports[i] = reportFor(sc)
		g.Go(func() error {
			// Not counted in the indexer metrics below: nothing was asked.
			if r, ok := keptAnswer(cache, sc, q); ok {
				results[i] = r
				reports[i].Outcome, reports[i].Items, reports[i].Cached = outcomeAnswered, len(r), true
				return nil
			}
			cctx, cancel := context.WithTimeout(gctx, timeout)
			defer cancel()
			start := time.Now()
			r, err := sc.scrape(cctx, q)
			reports[i].Outcome = classifyScrape(sc, err)
			if _, unasked := sc.(unaskableScraper); !unasked {
				reports[i].LatencyMS = time.Since(start).Milliseconds()
				reports[i].ObservedAt = time.Now().UTC().Truncate(time.Millisecond)
			}
			if err == nil {
				results[i] = r
				reports[i].Items = len(r)
				keepAnswer(cache, ttl, sc, q, r)
			}
			// Counted here because this is where the answer is already known. An unaskable scraper is
			// counted too: "asked nobody, so nobody answered" is a state worth being able to see, and
			// its failure ratio being exactly 1 is how it looks.
			metrics.indexerResult(sc.id(), err == nil, len(r))
			return nil // never fail the group — gather what responded
		})
	}
	_ = g.Wait()
	var all []RawStream
	// Nothing to ask is not an answer. This used to read as success on the grounds that "no indexers
	// configured" is an operator choice rather than a scrape failure — but defaults mean a config can no
	// longer end up with none, so an empty scraper list now means every one of them was skipped for
	// want of a config URL. Reporting that as a confirmed-empty result is how a misconfiguration
	// becomes "this release does not exist".
	anyOK := false
	for i, r := range results {
		all = append(all, r...)
		if reports[i].Outcome == outcomeAnswered {
			anyOK = true
		}
	}
	// An EMPTY result is only authoritative when EVERY indexer answered. "Someone responded" is enough to
	// trust a non-empty list — whatever came back is real — but not enough to state that a release does
	// not exist. torrentio 502s while another indexer legitimately has nothing, and the union of those
	// two was reported to the app as a confident "not available" for an episode that does exist.
	// An indexer that can NEVER be asked is excluded from this judgement. Counting it as one that did not
	// answer looked right and made the condition unsatisfiable: with mediafusion unconfigured — the
	// shipped default — every genuinely empty result was reported as an indexer outage, negative caching
	// never ran, and three unavailable titles in a row flipped /health to degraded on a healthy service.
	// A permanent misconfiguration is not a transient failure, and feeding both into one counter loses
	// the difference. It is reported once per process by `logIndexerSkipOnce` instead.
	//
	// A NON-empty partial list is a different case, and conflating the two was a mistake worth recording.
	// Marking it non-authoritative made every response uncacheable whenever ONE indexer was flaky: the
	// full 8s scrape and a fresh debrid cache-check fan-out on every single /stream, plus a degraded
	// header that has the app tell the viewer "sources temporarily unavailable" over a perfectly good
	// list. That is a worse answer than the one it was fixing. The list is served AND cached; `complete`
	// carries the incompleteness so the caller can hold it for a shorter time instead.
	complete := true
	for _, rep := range reports {
		// Only a PERMANENTLY unaskable indexer is excused. One whose config could not be minted this
		// minute is an outage: it would have voted, and we do not know how.
		if excusedFromQuorum(rep.Outcome) {
			continue
		}
		if rep.Outcome != outcomeAnswered {
			complete = false
			if len(all) == 0 {
				anyOK = false // an EMPTY list is only authoritative when everyone answered
			}
			break
		}
	}
	return scrapeResult{seeds: dedupe(all), anyOK: anyOK, complete: complete, sources: reports}
}

// dedupe by infohash, merging the richest facts (fill missing fileIdx/size, max seeders); first-seen
// order preserved.
func dedupe(seeds []RawStream) []RawStream {
	index := make(map[string]int)
	var out []RawStream
	for _, s := range seeds {
		if pos, ok := index[s.InfoHash]; ok {
			e := &out[pos]
			if e.FileIdx == nil && s.FileIdx != nil {
				e.FileIdx = s.FileIdx
			}
			if e.SizeBytes == nil && s.SizeBytes != nil {
				e.SizeBytes = s.SizeBytes
			}
			if intOr(s.Seeders, 0) > intOr(e.Seeders, 0) {
				e.Seeders = s.Seeders
			}
			continue
		}
		index[s.InfoHash] = len(out)
		out = append(out, s)
	}
	return out
}
