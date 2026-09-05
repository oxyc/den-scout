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

// A real IMDb id is `tt` plus seven or eight digits; ten leaves room for a couple of decades of growth.
//
// The bound matters because `\d+` did not have one. This segment is retained for the whole build — it
// goes into the cache key, into the rebuild gate's map key, and into the bingeGroup of EVERY stream in
// the response — and it is also concatenated into the outbound indexer URL. Measured on a server with
// net/http's DEFAULT 1 MiB header allowance: `tt` followed by 900,000 digits was accepted, pinned 2.7 MiB
// per in-flight request (150 concurrent parked at the scrape reached 408 MiB against a 230 MiB
// GOMEMLIMIT), and turned one small inbound GET into a ~900 KB outbound URL per configured indexer —
// which is the "ban risk" hostlimit.go was written to prevent, paced by the limiter but not sized by it.
//
// main.go now sets MaxHeaderBytes, so the request line cannot carry an id anywhere near that size and
// those figures are no longer reachable over HTTP. This stays as the second of the two bounds: the
// transport limit is one number in one file, and this is the one that holds if it is ever raised or if a
// caller reaches parseStreamID by some other route.
var imdbRe = regexp.MustCompile(`^tt\d{1,10}$`)

// maxStreamIDLen bounds the whole id segment. The longest real one is `tt1234567890:999:999.json`, at 25
// characters.
//
// Checked BEFORE the split, and that order is the point: Split allocates one string per part whatever
// the regex below would later say, so an id of many colons is a slice of that many strings, paid before
// the id ever reaches imdbRe to be rejected. Do not move this below the Split, and do not delete it as
// redundant with `\d{1,10}` — the regex only sees parts[0]. TestParseStreamID has a case that ONLY this
// check can refuse, so that neither of those edits can pass quietly.
const maxStreamIDLen = 64

// parseStreamID parses the <type> + <id>.json route segments, or ok=false (→ 400).
func parseStreamID(typ, rawID string) (*StreamID, bool) {
	if typ != "movie" && typ != "series" {
		return nil, false
	}
	if len(rawID) > maxStreamIDLen {
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
