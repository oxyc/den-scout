package scout

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestParseTitleIsReleaseNotIndexerLabel(t *testing.T) {
	// A stream with the indexer label as `name`, no behaviorHints, and a foreign release title: the
	// parsed title must be the release name, never the indexer label ("Torrentio").
	body := `{"streams":[{"name":"Torrentio","infoHash":"` + repeat("a", 40) +
		`","title":"El día de la Revelación (Spielberg's 2026) HdTScr Lat\n👤 5 💾 1.7 GB ⚙️ x"}]}`
	seeds := parseStremioStreams([]byte(body), "torrentio")
	if len(seeds) != 1 {
		t.Fatalf("want 1 seed, got %d", len(seeds))
	}
	if strings.Contains(seeds[0].Title, "Torrentio") || !strings.Contains(seeds[0].Title, "Revelaci") {
		t.Errorf("title should be the release name, got %q", seeds[0].Title)
	}
}

func TestReleaseYearsAndMismatch(t *testing.T) {
	if got := releaseYears("Movie 1920x1080 x264 2160p"); len(got) != 0 {
		t.Errorf("resolution/codec digits parsed as years: %v", got)
	}
	if got := releaseYears("Steven Spielberg (2024) [HDTV 1080p]"); len(got) != 1 || got[0] != 2024 {
		t.Errorf("want [2024], got %v", got)
	}
	// A Spanish-titled 2026 release must NOT be flagged for expected 2026 (title differs, year matches).
	if yearMismatch("El día de la Revelación (Spielberg's 2026) HdTScr", 2026) {
		t.Error("matching-year release flagged as mismatch")
	}
	if !yearMismatch("Steven Spielberg (2024) doc", 2026) {
		t.Error("2024 mistag should mismatch expected 2026")
	}
	if yearMismatch("Some Release WEB-DL", 2026) {
		t.Error("no-year title must not be a mismatch (can't tell → keep)")
	}
	if yearMismatch("Movie (2025)", 2026) {
		t.Error("±1 year should be tolerated")
	}
}

func TestRankDropsYearMistag(t *testing.T) {
	streams := []RawStream{
		rs("Steven Spielberg el rey midas de Hollywood (2024) [HDTV 1080p]", func(s *RawStream) { s.InfoHash = repeat("a", 40) }),
		rs("El día de la Revelación (Spielberg's 2026) HdTScr Lat", func(s *RawStream) { s.InfoHash = repeat("b", 40) }),
		rs("Disclosure Day (2026) CinemaCity", func(s *RawStream) { s.InfoHash = repeat("c", 40) }),
	}
	y := 2026
	out := rankStreams(streams, rankFilters{ExpectedYear: &y, ResultCap: 10})
	if len(out) != 2 {
		t.Fatalf("want 2 (2024 mistag dropped, both 2026 kept), got %d: %+v", len(out), out)
	}
	for _, s := range out {
		if strings.Contains(s.Title, "2024") {
			t.Errorf("mistagged 2024 stream survived: %q", s.Title)
		}
	}
}

// Why the title-token filter is NOT applied to series, pinned so nobody re-enables it on the strength of
// the friendliest example.
//
// The check is safe for movies only because it judges a release solely when that release carries no
// parseable year, and a movie release almost always carries one — so it sees a small tail of nameless
// junk. A series EPISODE release carries S04E01 and no year, so the escape hatch is structurally
// unavailable and the check would judge nearly everything. These are real shows, and every one of them
// would come back as an empty list.
func TestTitleTokens_wouldEmptyRealSeries(t *testing.T) {
	cases := []struct{ show, release, why string }{
		{"Shōgun", "Shogun.S01E01.1080p.WEB-DL.mkv", `[a-z0-9]+ cannot match "ō", so the tokens are {sh, gun}`},
		{"Pokémon", "Pokemon.S01E01.720p.mkv", "same, for é"},
		{"Attack on Titan", "[Erai-raws] Shingeki no Kyojin - 01 [1080p].mkv", "anime released under romaji"},
		{"Money Heist", "La.Casa.de.Papel.S01E01.1080p.WEB-DL.mkv", "released under its original title"},
	}
	for _, c := range cases {
		tokens := titleTokens(c.show)
		if len(releaseYears(c.release)) != 0 {
			t.Fatalf("%s: the fixture carries a year, which is not the case under test", c.show)
		}
		if titleOverlap(c.release, tokens) {
			t.Errorf("%s: this example no longer demonstrates the problem (%s)", c.show, c.why)
		}
	}
	// The one case that does work, which is what made the first attempt at this look fine.
	if !titleOverlap("The.Bear.S04E01.1080p.WEB-DL.mkv", titleTokens("The Bear")) {
		t.Error("an ASCII, English-titled show should overlap")
	}
}

func TestCinemetaMeta(t *testing.T) {
	ok := cinemetaMeta(mockDoer{fn: func(*http.Request) (*http.Response, error) {
		return resp(200, `{"meta":{"id":"tt1","name":"Disclosure Day","year":"2026"}}`), nil
	}}, "https://cinemeta.example")
	if m, found := ok(context.Background(), "movie", "tt15047880"); !found || m.Year != 2026 || m.Title != "Disclosure Day" {
		t.Errorf("movie meta: %+v found=%v", m, found)
	}
	// A series is not looked up at all, and no request is made for one. Both halves of the mistag filter
	// are unsafe for series — see the reasoning at the top of cinemeta.go and
	// TestTitleTokens_wouldEmptyRealSeries.
	asked := false
	series := cinemetaMeta(mockDoer{fn: func(*http.Request) (*http.Response, error) {
		asked = true
		return resp(200, `{"meta":{"id":"tt1","name":"The Bear","releaseInfo":"2022–2025"}}`), nil
	}}, "https://cinemeta.example")
	if _, found := series(context.Background(), "series", "tt1"); found {
		t.Error("series should return found=false")
	}
	if asked {
		t.Error("a series was looked up — the result cannot be used, so the request is waste")
	}
	// upstream failure → found=false (list served unfiltered)
	bad := cinemetaMeta(mockDoer{fn: func(*http.Request) (*http.Response, error) { return resp(500, ""), nil }}, "x")
	if _, found := bad(context.Background(), "movie", "tt1"); found {
		t.Error("cinemeta failure should return found=false")
	}
}

func TestRankDropsYearlessJunk(t *testing.T) {
	streams := []RawStream{
		rs("B-Bead.mp4", func(s *RawStream) { s.InfoHash = repeat("a", 40) }),                    // no year, no overlap → drop
		rs("Random Junk File", func(s *RawStream) { s.InfoHash = repeat("b", 40) }),              // no year, no overlap → drop
		rs("Los Backrooms HDTV Lat", func(s *RawStream) { s.InfoHash = repeat("c", 40) }),        // no year, shares "backrooms" → keep (foreign-lang)
		rs("Backrooms.2026.1080p.WEB.h265", func(s *RawStream) { s.InfoHash = repeat("d", 40) }), // has matching year → keep
	}
	y := 2026
	out := rankStreams(streams, rankFilters{
		ExpectedYear:        &y,
		ExpectedTitleTokens: titleTokens("Backrooms"),
		ResultCap:           10,
	})
	if len(out) != 2 {
		t.Fatalf("want 2 (year-less junk dropped, foreign-lang + matching-year kept), got %d: %+v", len(out), out)
	}
	for _, s := range out {
		if strings.Contains(s.Title, "B-Bead") || strings.Contains(s.Title, "Random Junk") {
			t.Errorf("year-less junk survived: %q", s.Title)
		}
	}
}

func TestDetectAudioCodecBranches(t *testing.T) {
	audio := map[string]string{
		"x DTS:X": "DTS:X", "x TrueHD": "TrueHD", "x DTS-HD MA": "DTS-HD", "x FLAC": "FLAC",
		"x EAC3": "EAC3", "x DTS": "DTS", "x AC3": "AC3", "x Atmos": "Atmos",
	}
	for title, want := range audio {
		if a := streamAttributes(RawStream{Title: title}).Audio; a == nil || *a != want {
			t.Errorf("audio(%q)=%v want %q", title, a, want)
		}
	}
	codec := map[string]string{"x AV1": "av1", "x x265": "hevc", "x HEVC": "hevc", "x x264": "avc", "x h264": "avc"}
	for title, want := range codec {
		if c := streamAttributes(RawStream{Title: title}).Codec; c == nil || *c != want {
			t.Errorf("codec(%q)=%v want %q", title, c, want)
		}
	}
	if streamAttributes(RawStream{Title: "x mystery"}).Codec != nil {
		t.Error("no codec → nil")
	}
}

func TestQualityScoreBranches(t *testing.T) {
	// Exercise the source ladder, audio ladder, and penalties (values are internal; just drive them).
	for _, title := range []string{
		"Movie 1080p WEBRip", "Movie 1080p WEB", "Movie 480p DVDRip", "Movie 720p TVRip",
		"Movie 1080p BluRay DTS-HD MA", "Movie 1080p BluRay TrueHD", "Movie 1080p WEB EAC3",
		"Movie 1080p WEB DTS", "Movie 1080p WEB AC3", "Movie 1080p WEB FLAC", "Movie 1080p WEB DTS:X",
		"Movie 1440p WEB", "Movie 576p WEB", "Movie 540p WEB", "Movie korsub", "Movie HC",
	} {
		_ = qualityScore(rs(title, nil))
	}
	// a large "4k"/uhd (no explicit 2160p) with a big file is trusted as 4K.
	big := qualityScore(rs("Movie UHD WEB", func(s *RawStream) { s.SizeBytes = intp(20 * gib) }))
	tiny := qualityScore(rs("Movie UHD WEB", func(s *RawStream) { s.SizeBytes = intp(1 * gib) }))
	if big <= tiny {
		t.Error("large uhd should outscore tiny uhd")
	}
}

func TestLabelBranches(t *testing.T) {
	cases := map[string]string{
		"Movie 1440p BluRay": "1440p • BluRay",
		"Movie 480p WEB":     "SD • WEB",
		"Movie 720p WEBRip":  "720p • WEB",
	}
	for title, want := range cases {
		if got := cleanLabel(RawStream{Title: title}); got != want {
			t.Errorf("cleanLabel(%q)=%q want %q", title, got, want)
		}
	}
	// 1440p has no bucket of its own; it must land on one of the two coarse answers rather than invent a
	// third. The previous version of this had an empty branch body, so it could not fail whatever came
	// back — it asserted nothing at all.
	if got := detectResolution("x 1440p"); got != "1080p" && got != "" {
		t.Errorf("detectResolution(1440p) = %q, want 1080p or none", got)
	}
}

func TestSeasonAbsoluteNumberingNoResolutionMatch(t *testing.T) {
	p := episodePatterns(7, 20) // concatenated form is "720"
	if matchesEpisode("Show.720p.WEB-DL.x264.mkv", p) {
		t.Error("S7E20 must not match a 720p resolution token")
	}
	if !matchesEpisode("Show.720.mkv", p) {
		t.Error("S7E20 should still match a bare 720 episode token")
	}
	if !matchesEpisode("Show.S07E20.1080p.mkv", episodePatterns(7, 20)) {
		t.Error("S07E20 form should still match")
	}
}

func TestRankMinSeedersKeepsUnknown(t *testing.T) {
	streams := []RawStream{
		rs("Movie 1080p WEB", func(s *RawStream) { s.InfoHash = repeat("a", 40) }),                     // seeders unknown
		rs("Movie 720p WEB", func(s *RawStream) { s.InfoHash = repeat("b", 40); s.Seeders = intp(1) }), // known, below threshold
	}
	out := rankStreams(streams, rankFilters{MinSeeders: intp(5), ResultCap: 10})
	if len(out) != 1 || out[0].InfoHash != repeat("a", 40) {
		t.Errorf("minSeeders should keep unknown-seeder streams and drop known-below-threshold: %+v", out)
	}
}

func TestSeriesStreamAndForwardedOrigin(t *testing.T) {
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				return []RawStream{{InfoHash: repeat("a", 40), Title: "Show 1080p WEB-DL", SizeBytes: intp(3 * gib), FileIdx: intp(4)}}, nil
			}}}
		}
		d.MakeStores = func(*Config) []Store {
			return []Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{repeat("a", 40): true}}}
		}
	}))
	rr := do(h, "/"+validBlob+"/stream/series/tt99:1:2.json", map[string]string{
		"X-Forwarded-Proto": "https", "X-Forwarded-Host": "scout.den.example",
	})
	if rr.Code != 200 {
		t.Fatalf("series stream: %d", rr.Code)
	}
	var body struct {
		Streams []struct {
			URL           string `json:"url"`
			BehaviorHints struct {
				BingeGroup string `json:"bingeGroup"`
			} `json:"behaviorHints"`
		} `json:"streams"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.Streams) != 1 {
		t.Fatalf("want 1 series stream, got %d", len(body.Streams))
	}
	first := body.Streams[0]
	if !strings.HasPrefix(first.URL, "https://scout.den.example/") {
		t.Errorf("forwarded origin: %s", first.URL)
	}
	if first.BehaviorHints.BingeGroup != "den-scout-tt99" {
		t.Errorf("bingeGroup: %s", first.BehaviorHints.BingeGroup)
	}
	tok := first.URL[strings.LastIndex(first.URL, "/play/")+len("/play/"):]
	pt, ok := decodePlayToken(tok)
	if !ok || pt.Season == nil || *pt.Season != 1 || pt.Episode == nil || *pt.Episode != 2 {
		t.Errorf("series play token: %+v ok=%v", pt, ok)
	}
}
