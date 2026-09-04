package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Cinemeta (the public Stremio metadata addon) maps an IMDb id → title/year. den-scout uses those to drop
// torrents a tracker mistagged with another title's id. It's a best-effort side lookup: any failure
// returns ok=false and the stream list is served unfiltered.
//
// MOVIES ONLY, for two reasons. The first is the obvious one: Cinemeta gives a series a span —
// "2019–2023" — of which firstYear can only take the start, and rank.go's year check allows ±1 around
// it, so a correctly named Show.S04.2023.1080p carrying the SEASON's air year would be dropped as a
// mistag.
//
// The second is why returning a title-without-year for series does not work either, which was tried and
// reverted. rank.go's title-token check is only safe because of its escape hatch: it judges a release
// solely when that release carries NO parseable year. A movie release almost always carries "(2024)", so
// the check sees a small tail of nameless junk — which is what it was written for. A series EPISODE
// release essentially never carries a year (it carries S04E01), so for series the escape hatch is
// structurally unavailable and the token check would judge nearly every release. Measured against real
// naming, that empties whole shows:
//
//	Shōgun          → tokens {sh, gun}  ([a-z0-9]+ cannot match "ō") vs Shogun.S01E01…     → dropped
//	Pokémon         → tokens {pok, mon}                              vs Pokemon.S01E01…    → dropped
//	Attack on Titan → vs [Erai-raws] Shingeki no Kyojin - 01          (romaji)              → dropped
//	Money Heist     → vs La.Casa.de.Papel.S01E01…                     (original title)      → dropped
//
// An episode-marker escape hatch would rescue the first three but not anime numbered "- 01", and the
// whole filter is speculative for series: the junk it was written to remove has only ever been observed
// on movies. So series are not looked up at all, and no filter is applied to them.

const cinemetaBase = "https://v3-cinemeta.strem.io"

// cineMeta is the subset of a Cinemeta record used to sanity-check tracker results. Year is 0 when
// unknown; Title is "" when unknown.
type cineMeta struct {
	Title string
	Year  int
}

// cinemetaMeta builds the Meta dependency against a Cinemeta-compatible base URL.
func cinemetaMeta(client doer, base string) func(context.Context, string, string) (cineMeta, bool) {
	base = strings.TrimRight(base, "/")
	return func(ctx context.Context, typ, imdb string) (cineMeta, bool) {
		if typ != "movie" {
			return cineMeta{}, false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/meta/movie/%s.json", base, imdb), nil)
		if err != nil {
			return cineMeta{}, false
		}
		req.Header.Set("accept", "application/json")
		req.Header.Set("user-agent", scrapeUserAgent)
		resp, err := client.Do(req)
		if err != nil {
			return cineMeta{}, false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return cineMeta{}, false
		}
		var body struct {
			Meta struct {
				Name        string `json:"name"`
				Year        string `json:"year"`
				ReleaseInfo string `json:"releaseInfo"`
			} `json:"meta"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, maxScrapeBytes)).Decode(&body) != nil {
			return cineMeta{}, false
		}
		m := cineMeta{Title: strings.TrimSpace(body.Meta.Name)}
		if y := firstYear(body.Meta.Year); y != 0 {
			m.Year = y
		} else {
			m.Year = firstYear(body.Meta.ReleaseInfo)
		}
		// Usable only if we learned at least one signal.
		return m, m.Year != 0 || m.Title != ""
	}
}

// firstYear pulls the first plausible 4-digit year out of a string like "2026" or "2019–2023".
func firstYear(s string) int {
	if m := yearToken.FindString(strings.ToLower(s)); m != "" {
		if y, err := strconv.Atoi(m); err == nil {
			return y
		}
	}
	return 0
}
