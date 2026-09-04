package scout

import (
	"fmt"
	"testing"
)

func rs(title string, mut func(*RawStream)) RawStream {
	s := RawStream{InfoHash: "h", Title: title, Source: "torrentio"}
	if mut != nil {
		mut(&s)
	}
	return s
}

func TestJunkClass(t *testing.T) {
	junk := map[string]string{
		"Movie 2024 HDCAM x264":  "cam",
		"Movie 2024 hd.cam":      "cam",
		"Movie 2024 HD-TS":       "telesync",
		"Movie 2024 DVDScr":      "screener",
		"Movie 2024 WORKPRINT":   "workprint",
		"Movie 2024 AI-upscaled": "upscaled",
		"Movie 2024 R5":          "r5",
		"Movie 2024 TELECINE":    "telecine",
		"Movie 2024 cam":         "cam",
		"Movie 2024 ts":          "telesync",
		"Movie 2024 scr":         "screener",
	}
	for title, want := range junk {
		if got := junkClass(title); got != want {
			t.Errorf("junkClass(%q)=%q want %q", title, got, want)
		}
	}
	notJunk := []string{
		"Show S01E01 WEB-DL cam.crew.release",
		"Movie 2024 BluRay REMUX ts-audio",
		"Movie.2024.2160p.WEB-DL.DV.HDR.x265",
	}
	for _, title := range notJunk {
		if got := junkClass(title); got != "" {
			t.Errorf("junkClass(%q)=%q want none", title, got)
		}
	}
}

func TestQualityScore(t *testing.T) {
	cached := qualityScore(rs("Movie 720p WEB-DL", func(s *RawStream) { s.Cached = true }))
	uncached := qualityScore(rs("Movie 2160p REMUX", nil))
	if cached <= uncached {
		t.Errorf("cached (%d) should beat uncached (%d)", cached, uncached)
	}

	cam := qualityScore(rs("Movie 2160p HDCAM", nil))
	legit := qualityScore(rs("Movie 480p WEB-DL", nil))
	if cam >= legit-50000 {
		t.Errorf("junk (%d) should be far below legit (%d)", cam, legit)
	}

	remux := qualityScore(rs("Movie 1080p REMUX", nil))
	webdl := qualityScore(rs("Movie 1080p WEB-DL", nil))
	hdtv := qualityScore(rs("Movie 1080p HDTV", nil))
	if remux <= webdl || webdl <= hdtv {
		t.Errorf("expected remux(%d) > webdl(%d) > hdtv(%d)", remux, webdl, hdtv)
	}

	if qualityScore(rs("Movie 2160p WEB-DL AV1", nil)) >= qualityScore(rs("Movie 2160p WEB-DL", nil)) {
		t.Error("av1 at 4k should be penalized")
	}
	if qualityScore(rs("Movie 1080p BluRay 3D HSBS", nil)) >= qualityScore(rs("Movie 1080p BluRay", nil)) {
		t.Error("3d should be penalized")
	}
	if qualityScore(rs("Movie 2160p WEB-DL DoVi", nil)) <= qualityScore(rs("Movie 2160p WEB-DL", nil)) {
		t.Error("DoVi should be rewarded")
	}
	if qualityScore(rs("Movie 2160p REMUX Atmos", nil)) <= qualityScore(rs("Movie 2160p REMUX", nil)) {
		t.Error("Atmos should be rewarded")
	}
	// Ranked by what Den DELIVERS: DD+ (stream-copy) over the same title in TrueHD/DTS (bridged to 5.1),
	// and DD+ Atmos (real Atmos) over TrueHD Atmos (Atmos lost in the bridge).
	if qualityScore(rs("Movie 2160p WEB-DL DDP5.1 Atmos", nil)) <= qualityScore(rs("Movie 2160p WEB-DL TrueHD Atmos 7.1", nil)) {
		t.Error("DD+ Atmos (delivered) should outrank TrueHD Atmos (bridged to 5.1)")
	}
	if qualityScore(rs("Movie 1080p BluRay DDP5.1", nil)) <= qualityScore(rs("Movie 1080p BluRay DTS-HD MA 5.1", nil)) {
		t.Error("DD+ (stream-copy) should outrank DTS-HD (bridged)")
	}
	small := qualityScore(rs("Movie 4K WEB", func(s *RawStream) { s.SizeBytes = intp(1 * gib) }))
	large := qualityScore(rs("Movie 4K WEB", func(s *RawStream) { s.SizeBytes = intp(20 * gib) }))
	if small >= large {
		t.Error("a tiny 4k file should score below a large one")
	}
}

func TestDetectResolution(t *testing.T) {
	cases := map[string]string{"x 2160p y": "2160p", "x 576p y": "480p", "x 720p y": "720p", "no res here": ""}
	for in, want := range cases {
		if got := detectResolution(in); got != want {
			t.Errorf("detectResolution(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRankStreams(t *testing.T) {
	// CacheKnown on all three: the check ran and answered for each of them. cachedOnly drops what is
	// KNOWN not to be cached — see below for what it must not drop.
	streams := []RawStream{
		rs("A 1080p WEB-DL", func(s *RawStream) { s.Cached, s.CacheKnown = true, true }),
		rs("B 2160p HDCAM", func(s *RawStream) { s.Cached, s.CacheKnown = true, true }),
		rs("C 1080p WEB-DL", func(s *RawStream) { s.CacheKnown = true }),
	}
	ranked := rankStreams(streams, rankFilters{ExcludeCam: true, CachedOnly: true, ResultCap: 5})
	if len(ranked) != 1 || ranked[0].Title != "A 1080p WEB-DL" {
		t.Errorf("excludeCam+cachedOnly: got %v", titles(ranked))
	}

	// A release nobody could CHECK is not a release known to be uncached. Cache checks batch, so one
	// failed batch leaves a hundred unknown while the rest of the list is perfectly well known — and
	// dropping those removed up to a hundred playable releases with nothing anywhere saying so.
	unchecked := []RawStream{
		rs("A 1080p WEB-DL", func(s *RawStream) { s.Cached, s.CacheKnown = true, true }),
		rs("B 1080p WEB-DL", func(s *RawStream) { s.CacheKnown = true }),  // known: not cached
		rs("C 1080p WEB-DL", func(s *RawStream) { s.CacheKnown = false }), // its batch failed
	}
	keptUnchecked := rankStreams(unchecked, rankFilters{CachedOnly: true, ResultCap: 5})
	if len(keptUnchecked) != 2 {
		t.Errorf("cachedOnly dropped a release nobody could check: %v", titles(keptUnchecked))
	}

	// hdrOnly keeps only HDR/DV.
	hdr := rankStreams([]RawStream{
		rs("A 2160p WEB HDR", func(s *RawStream) { s.Cached = true }),
		rs("B 2160p WEB", func(s *RawStream) { s.Cached = true }),
	}, rankFilters{ExcludeCam: true, CachedOnly: true, HDROnly: true, ResultCap: 5})
	if len(hdr) != 1 || hdr[0].Title != "A 2160p WEB HDR" {
		t.Errorf("hdrOnly: got %v", titles(hdr))
	}

	// seeders break ties.
	tie := rankStreams([]RawStream{
		rs("Movie 1080p WEB-DL", func(s *RawStream) { s.Cached = true; s.Seeders = intp(5) }),
		rs("Movie 1080p WEB-DL", func(s *RawStream) { s.Cached = true; s.Seeders = intp(500) }),
	}, rankFilters{ExcludeCam: true, CachedOnly: true, ResultCap: 5})
	if intOr(tie[0].Seeders, 0) != 500 {
		t.Errorf("seeders tiebreak: first has %d want 500", intOr(tie[0].Seeders, 0))
	}

	// resolutions keep untagged; minSeeders/maxSizeGB/excludeRegex apply.
	filtered := rankStreams([]RawStream{
		rs("X 2160p WEB HDR", func(s *RawStream) { s.Cached = true; s.Seeders = intp(10); s.SizeBytes = intp(30 * gib) }),
		rs("Y 1080p WEB", func(s *RawStream) { s.Cached = true; s.Seeders = intp(10); s.SizeBytes = intp(8 * gib) }),
		rs("Z untitled release", func(s *RawStream) { s.Cached = true; s.Seeders = intp(10); s.SizeBytes = intp(2 * gib) }),
		rs("W 2160p WEB", func(s *RawStream) { s.Cached = true; s.Seeders = intp(0); s.SizeBytes = intp(5 * gib) }),
		rs("U 2160p WEB", func(s *RawStream) { s.Cached = true; s.Seeders = intp(10); s.SizeBytes = intp(90 * gib) }),
		rs("V 2160p WEB YIFY", func(s *RawStream) { s.Cached = true; s.Seeders = intp(10); s.SizeBytes = intp(5 * gib) }),
	}, rankFilters{ExcludeCam: true, CachedOnly: true, ResultCap: 20, Resolutions: []string{"2160p"}, MinSeeders: intp(1), MaxSizeGB: intp(40), ExcludeRegex: "yify"})
	got := map[string]bool{}
	for _, s := range filtered {
		got[s.Title] = true
	}
	if len(filtered) != 2 || !got["X 2160p WEB HDR"] || !got["Z untitled release"] {
		t.Errorf("combined filters: got %v", titles(filtered))
	}

	// malformed excludeRegex is ignored, not fatal.
	ok := rankStreams([]RawStream{rs("A 1080p WEB-DL", func(s *RawStream) { s.Cached = true })}, rankFilters{ExcludeCam: true, CachedOnly: true, ResultCap: 5, ExcludeRegex: "("})
	if len(ok) != 1 {
		t.Errorf("bad excludeRegex should be ignored, got %d", len(ok))
	}
}

func TestRealDebridBlocked(t *testing.T) {
	blocked := []string{"Movie.2024.WEB-DL.x265", "Show.WEBRip.720p", "X.BDRip.1080p", "Y.HDRip", "Z.DVDRip",
		"Movie.2024.BluRay.x264-GRP", "Show.HDTV.x264", "Show.HDTV.XviD", "Clip.WEB.x264", "Clip.WEB.h264"}
	for _, tt := range blocked {
		if !realDebridBlocked(tt) {
			t.Errorf("realDebridBlocked(%q) should be true", tt)
		}
	}
	clean := []string{"Movie.2024.2160p.BluRay.REMUX.HEVC", "Movie.2024.WEB.x265", "Movie 2024 BluRay x264"}
	for _, tt := range clean {
		if realDebridBlocked(tt) {
			t.Errorf("realDebridBlocked(%q) should be false", tt)
		}
	}
}

func intp(n int) *int { return &n }

func titles(streams []RawStream) []string {
	out := make([]string, len(streams))
	for i, s := range streams {
		out[i] = s.Title
	}
	return out
}

// The seed cap keeps the BEST releases, not the first ones the scrape happened to return.
//
// The cap bounds the debrid cache-check fan-out, which is right — but it used to trim in scrape order.
// Past the cap a 2160p REMUX sitting at the tail was dropped while cam junk at the head survived, and
// neither the viewer nor the log saw it happen.
func TestCapSeeds_keepsTheBestNotTheFirst(t *testing.T) {
	var streams []RawStream
	for i := 0; i < 40; i++ { // junk arrives first, as an indexer's own order well might
		streams = append(streams, RawStream{
			InfoHash: fmt.Sprintf("%040x", i),
			Title:    fmt.Sprintf("Some.Film.2024.CAM.XviD-JUNK.%d.mkv", i),
			Seeders:  intp(1),
		})
	}
	gem := RawStream{InfoHash: repeat("f", 40), Title: "Some.Film.2024.2160p.UHD.BluRay.REMUX.HDR.mkv", Seeders: intp(50)}
	streams = append(streams, gem)

	kept := capSeeds(streams, 10)
	if len(kept) != 10 {
		t.Fatalf("capped to %d, want 10", len(kept))
	}
	found := false
	for _, s := range kept {
		if s.InfoHash == gem.InfoHash {
			found = true
		}
	}
	if !found {
		t.Error("the best release was dropped and forty cam rips were kept")
	}
}

// Under the cap nothing is touched — not reordered, not dropped. The real ranking runs afterwards and
// owns the order the viewer sees.
func TestCapSeeds_leavesASmallListAlone(t *testing.T) {
	streams := []RawStream{
		{InfoHash: "a", Title: "Some.Film.2024.CAM.mkv"},
		{InfoHash: "b", Title: "Some.Film.2024.2160p.REMUX.mkv"},
	}
	kept := capSeeds(streams, 10)
	if len(kept) != 2 || kept[0].InfoHash != "a" || kept[1].InfoHash != "b" {
		t.Errorf("a list under the cap was altered: %+v", kept)
	}
	if got := capSeeds(streams, 0); len(got) != 2 {
		t.Errorf("a non-positive cap must not drop anything: %+v", got)
	}
}

// Equal quality falls back to seeders — between two identical-looking releases, the one more people are
// sharing is the one that will actually download.
func TestCapSeeds_breaksTiesOnSeeders(t *testing.T) {
	streams := []RawStream{
		{InfoHash: "low", Title: "Some.Film.2024.1080p.WEB-DL.mkv", Seeders: intp(2)},
		{InfoHash: "high", Title: "Some.Film.2024.1080p.WEB-DL.mkv", Seeders: intp(900)},
	}
	if kept := capSeeds(streams, 1); len(kept) != 1 || kept[0].InfoHash != "high" {
		t.Errorf("tie not broken on seeders: %+v", kept)
	}
}

// A preference RANKS, it does not filter. Every other knob in Filters is a hard drop, which is the wrong
// shape for a taste: someone who filters to 1080p to save bandwidth would still rather see a 4K remux
// than an empty list when that is all anybody is seeding.
func TestRankStreams_preferResolutionRanksWithoutDropping(t *testing.T) {
	streams := []RawStream{
		rs("Film.2024.2160p.BluRay.REMUX.HDR.mkv", nil),
		rs("Film.2024.1080p.WEB-DL.mkv", nil),
		rs("Film.2024.720p.WEB-DL.mkv", nil),
	}
	got := rankStreams(streams, rankFilters{PreferResolution: "1080p", ResultCap: 20})
	if len(got) != 3 {
		t.Fatalf("a preference dropped releases: kept %d of 3", len(got))
	}
	if detectResolution(got[0].Title) != "1080p" {
		t.Errorf("preferred resolution did not rank first: %q", got[0].Title)
	}
	// …and below it, the normal quality order is untouched: the 4K remux still beats the 720p.
	if detectResolution(got[1].Title) != "2160p" {
		t.Errorf("non-preferred releases lost their own ordering: %q then %q", got[1].Title, got[2].Title)
	}
}

// The sink must clear every quality signal — otherwise "prefer 1080p" loses to a big 4K remux and means
// nothing — while staying under cachedness, which is a fact about whether it plays now rather than taste.
func TestRankStreams_preferenceOutweighsQualityButNotCached(t *testing.T) {
	best4K := rs("Film.2024.2160p.BluRay.REMUX.HDR.DDP5.1.Atmos.mkv", func(s *RawStream) {
		s.SizeBytes = intp(80 * gib) // maximum size bonus, on top of every other quality signal
	})
	plain1080 := rs("Film.2024.1080p.WEB-DL.mkv", nil)

	got := rankStreams([]RawStream{best4K, plain1080}, rankFilters{PreferResolution: "1080p", ResultCap: 20})
	if detectResolution(got[0].Title) != "1080p" {
		t.Errorf("the richest possible 4K outranked the preference: %q", got[0].Title)
	}

	// Cached still wins: waiting for a download is worse than a resolution you did not ask for.
	cached4K := best4K
	cached4K.Cached = true
	got = rankStreams([]RawStream{cached4K, plain1080}, rankFilters{PreferResolution: "1080p", ResultCap: 20})
	if detectResolution(got[0].Title) != "2160p" {
		t.Errorf("an uncached preference outranked a cached release: %q", got[0].Title)
	}
}

// An unreadable resolution is not sunk. Every hard filter here keeps what it cannot measure — the
// whitelist keeps untagged releases, minSeeders keeps unknown counts, maxSizeGB keeps unknown sizes — and
// punishing an unknown would assert something nobody measured.
func TestRankStreams_preferenceKeepsUntaggedResolutionNeutral(t *testing.T) {
	untagged := rs("Film.2024.WEB-DL.mkv", nil)
	other := rs("Film.2024.720p.WEB-DL.mkv", nil)
	got := rankStreams([]RawStream{other, untagged}, rankFilters{PreferResolution: "1080p", ResultCap: 20})
	if len(got) != 2 {
		t.Fatalf("kept %d of 2", len(got))
	}
	if got[0].Title != untagged.Title {
		t.Errorf("an untagged release was sunk alongside the non-preferred ones: %q first", got[0].Title)
	}
}

// A preference must never lift junk. The CAM sink is orders of magnitude larger for a reason.
func TestRankStreams_preferenceNeverLiftsJunk(t *testing.T) {
	cam := rs("Film.2024.1080p.HDCAM.x264.mkv", nil)
	legit := rs("Film.2024.2160p.WEB-DL.mkv", nil)
	got := rankStreams([]RawStream{cam, legit}, rankFilters{PreferResolution: "1080p", ResultCap: 20})
	if got[0].Title == cam.Title {
		t.Error("a preference lifted a CAM above a legitimate release")
	}
}
