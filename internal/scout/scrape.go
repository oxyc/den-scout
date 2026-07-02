package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// Indexer scrapers (ported from src/scrape/*). One shared Stremio-protocol client; fan-out with a
// per-indexer timeout, gather-what-responded, dedupe by infohash.

const maxScrapeBytes = 4 << 20 // 4 MiB cap on an addon response body

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
		text := strings.Join(nonEmpty(s.Name, s.Title, s.Description), "\n")
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

func (s *stremioScraper) scrape(ctx context.Context, q scrapeQuery) ([]RawStream, error) {
	stremID := q.IMDb
	if q.HasEp {
		stremID = fmt.Sprintf("%s:%d:%d", q.IMDb, q.Season, q.Episode)
	}
	u := strings.TrimRight(s.baseURL, "/") + "/stream/" + q.Type + "/" + url.QueryEscape(stremID) + ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s http %d", s.indexer, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxScrapeBytes))
	if err != nil {
		return nil, err
	}
	return parseStremioStreams(body, string(s.indexer)), nil
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
		out = append(out, &stremioScraper{indexer: id, baseURL: baseURLFor(id, config, urls), client: client})
	}
	return out
}

func baseURLFor(id Indexer, config *Config, urls map[Indexer]string) string {
	if u, ok := urls[id]; ok && u != "" {
		return u
	}
	if id == "torrentio" {
		opts := "sort=qualitysize"
		if config.Filters.ExcludeCam {
			opts += "|qualityfilter=cam,scr"
		}
		return defaultIndexerURLs["torrentio"] + "/" + opts
	}
	return defaultIndexerURLs[id]
}

// scrapeAll runs every scraper concurrently under a per-indexer timeout; drops those that error/time
// out; then dedupes by infohash.
func scrapeAll(ctx context.Context, scrapers []scraper, q scrapeQuery, timeout time.Duration) []RawStream {
	results := make([][]RawStream, len(scrapers))
	g, gctx := errgroup.WithContext(ctx)
	for i, sc := range scrapers {
		i, sc := i, sc
		g.Go(func() error {
			cctx, cancel := context.WithTimeout(gctx, timeout)
			defer cancel()
			if r, err := sc.scrape(cctx, q); err == nil {
				results[i] = r
			}
			return nil // never fail the group — gather what responded
		})
	}
	_ = g.Wait()
	var all []RawStream
	for _, r := range results {
		all = append(all, r...)
	}
	return dedupe(all)
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
