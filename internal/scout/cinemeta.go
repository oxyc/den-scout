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

// Cinemeta (the public Stremio metadata addon) maps an IMDb id → title/year. den-scout uses only the
// year, and only for movies, to drop torrents a tracker mistagged with another film's id. It's a
// best-effort side lookup: any failure returns ok=false and the stream list is served unfiltered.

const cinemetaBase = "https://v3-cinemeta.strem.io"

// cinemetaYear builds the MetaYear dependency against a Cinemeta-compatible base URL.
func cinemetaYear(client doer, base string) func(context.Context, string, string) (int, bool) {
	base = strings.TrimRight(base, "/")
	return func(ctx context.Context, typ, imdb string) (int, bool) {
		if typ != "movie" {
			return 0, false // series span years; year-filtering them is unreliable
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/meta/movie/%s.json", base, imdb), nil)
		if err != nil {
			return 0, false
		}
		req.Header.Set("accept", "application/json")
		req.Header.Set("user-agent", scrapeUserAgent)
		resp, err := client.Do(req)
		if err != nil {
			return 0, false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return 0, false
		}
		var body struct {
			Meta struct {
				Year        string `json:"year"`
				ReleaseInfo string `json:"releaseInfo"`
			} `json:"meta"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, maxScrapeBytes)).Decode(&body) != nil {
			return 0, false
		}
		if y := firstYear(body.Meta.Year); y != 0 {
			return y, true
		}
		if y := firstYear(body.Meta.ReleaseInfo); y != 0 {
			return y, true
		}
		return 0, false
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
