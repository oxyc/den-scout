package scout

import "testing"

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
	streams := []RawStream{
		rs("A 1080p WEB-DL", func(s *RawStream) { s.Cached = true }),
		rs("B 2160p HDCAM", func(s *RawStream) { s.Cached = true }),
		rs("C 1080p WEB-DL", nil),
	}
	ranked := rankStreams(streams, rankFilters{ExcludeCam: true, CachedOnly: true, ResultCap: 5})
	if len(ranked) != 1 || ranked[0].Title != "A 1080p WEB-DL" {
		t.Errorf("excludeCam+cachedOnly: got %v", titles(ranked))
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
