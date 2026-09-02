package scout

import (
	"context"
	"encoding/json"
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
}

func (s *stremioScraper) id() Indexer { return s.indexer }

// A shed request is not an answer.
//
// Opening a season asks for every episode at once, so an indexer sees a burst and sheds part of it —
// torrentio answers 502 to some of them. Treated as a result, that turns into "no source found for this
// episode" for a release the same indexer serves 50 of a second later. These statuses are the indexer
// declining to answer right now, so the request is made again rather than reported as an outcome.
func retryableScrapeStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

// Short and jittered: the whole scrape is under an 8 s budget shared with three other indexers, so this
// buys a second chance without spending someone else's timeout. Jitter because a burst that is shed
// together would otherwise retry together and be shed together again.
var scrapeRetryBackoff = []time.Duration{250 * time.Millisecond, 700 * time.Millisecond}

func (s *stremioScraper) scrape(ctx context.Context, q scrapeQuery) ([]RawStream, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		streams, err, retryable := s.scrapeOnce(ctx, q)
		if err == nil {
			if attempt > 0 {
				log.Printf("scout: %s indexer answered on retry %d", s.indexer, attempt)
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
	u := strings.TrimRight(s.baseURL, "/") + "/stream/" + q.Type + "/" + url.QueryEscape(stremID) + ".json"
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
		log.Printf("scout: %s indexer unreachable", s.indexer)
		return nil, err, true // a transport failure mid-burst is worth one more try
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.Printf("scout: %s indexer returned http %d", s.indexer, resp.StatusCode)
		return nil, fmt.Errorf("%s http %d", s.indexer, resp.StatusCode),
			retryableScrapeStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxScrapeBytes))
	if err != nil {
		return nil, err, true
	}
	return parseStremioStreams(body, string(s.indexer)), nil, false
}

// --- fan-out + dedupe ---

var defaultIndexerURLs = map[Indexer]string{
	"torrentio":   "https://torrentio.strem.fun",
	"comet":       "https://comet.elfhosted.com",
	"mediafusion": "https://mediafusion.elfhosted.com",
	"torz":        "https://torz.strem.fun",
}

func makeScrapers(config *Config, client doer, urls map[Indexer]string) []scraper {
	out := make([]scraper, 0, len(config.Indexers))
	for _, id := range config.Indexers {
		// An indexer that needs a per-install config segment and hasn't been given one is not asked. It
		// cannot answer usefully, and its failure is silent in the worst way: mediafusion returns 200
		// with an empty list, which counts as a response and votes on whether a release exists. Leaving
		// it out means a genuine "nobody has this" still requires everyone who WAS asked to say so.
		base := baseURLFor(id, config, urls)
		if envVar, needsPath := configPathIndexers[id]; needsPath && urls[id] == "" {
			// Mint one from the debrid account we already hold, when the operator has allowed it. An
			// explicit URL always wins, so a token-free config that was pasted in stays in use.
			if mintIndexerConfigs {
				base = indexerBaseWithConfig(context.Background(), id, config, client)
			}
			if base == "" || !mintIndexerConfigs {
				logIndexerSkipOnce(id, envVar)
				continue
			}
		}
		out = append(out, &stremioScraper{indexer: id, baseURL: base, client: client})
	}
	return out
}

// Once per indexer per process: this runs on every request, and the operator needs the line once.
var indexerSkipLogged sync.Map

func logIndexerSkipOnce(id Indexer, envVar string) {
	if _, seen := indexerSkipLogged.LoadOrStore(id, true); seen {
		return
	}
	log.Printf("scout: %s needs a per-install config URL and has none — not querying it. Set %s to the "+
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
func scrapeAll(ctx context.Context, scrapers []scraper, q scrapeQuery, timeout time.Duration) ([]RawStream, bool) {
	results := make([][]RawStream, len(scrapers))
	respok := make([]bool, len(scrapers))
	g, gctx := errgroup.WithContext(ctx)
	for i, sc := range scrapers {
		i, sc := i, sc
		g.Go(func() error {
			cctx, cancel := context.WithTimeout(gctx, timeout)
			defer cancel()
			if r, err := sc.scrape(cctx, q); err == nil {
				results[i] = r
				respok[i] = true
			}
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
		if respok[i] {
			anyOK = true
		}
	}
	// An EMPTY result is only authoritative when EVERY indexer answered. "Someone responded" is enough to
	// trust a non-empty list — whatever came back is real — but not enough to state that a release does
	// not exist. torrentio 502s while another indexer legitimately has nothing, and the union of those
	// two was reported to the app as a confident "not available" for an episode that does exist.
	if len(all) == 0 {
		for _, ok := range respok {
			if !ok {
				anyOK = false
				break
			}
		}
	}
	return dedupe(all), anyOK
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
