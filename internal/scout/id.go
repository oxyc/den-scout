package scout

import (
	"regexp"
	"strconv"
	"strings"
)

// StreamID is a parsed Stremio stream id: tt<digits> (movie) or tt<digits>:S:E (series episode).
type StreamID struct {
	Type    string // "movie" | "series"
	IMDb    string
	Season  int
	Episode int
	HasEp   bool
}

var imdbRe = regexp.MustCompile(`^tt\d+$`)

// parseStreamID parses the <type> + <id>.json route segments, or ok=false (→ 400).
func parseStreamID(typ, rawID string) (*StreamID, bool) {
	if typ != "movie" && typ != "series" {
		return nil, false
	}
	id := strings.TrimSuffix(rawID, ".json")
	parts := strings.Split(id, ":")
	imdb := parts[0]
	if !imdbRe.MatchString(imdb) {
		return nil, false
	}
	if typ == "series" {
		if len(parts) < 3 {
			return nil, false
		}
		season, err1 := strconv.Atoi(parts[1])
		episode, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil || season < 0 || episode < 0 {
			return nil, false
		}
		return &StreamID{Type: "series", IMDb: imdb, Season: season, Episode: episode, HasEp: true}, true
	}
	return &StreamID{Type: "movie", IMDb: imdb}, true
}
