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

// Cinemeta (the public Stremio metadata addon) maps an IMDb id → title/year. den-scout uses the year
// and title, and only for movies, to drop torrents a tracker mistagged with another film's id. It's a
// best-effort side lookup: any failure returns ok=false and the stream list is served unfiltered.

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
			return cineMeta{}, false // series span years; year-filtering them is unreliable
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
