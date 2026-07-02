package scout

import (
	"testing"
	"time"
)

func blob(jsonStr string) string { return b64urlEncode([]byte(jsonStr)) }

func TestDecodeConfig(t *testing.T) {
	c, ok := decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"tb-secret"}],"indexers":["torrentio"],"filters":{"excludeCam":true},"cachedOnly":true,"resultCap":20}`))
	if !ok || len(c.Debrid) != 1 || c.Debrid[0].Service != ServiceTorBox || c.Debrid[0].Token != "tb-secret" || !c.CachedOnly {
		t.Fatalf("valid config: %+v ok=%v", c, ok)
	}

	// reject configs with no valid debrid account
	for _, bad := range []string{
		`{"debrid":[],"indexers":["torrentio"]}`,
		`{"debrid":[{"service":"nope","token":"x"}]}`,
		`{"debrid":[{"service":"torbox","token":""}]}`,
	} {
		if _, ok := decodeConfig(nil, blob(bad)); ok {
			t.Errorf("expected reject: %s", bad)
		}
	}
	if _, ok := decodeConfig(nil, "!!!not-base64!!!"); ok {
		t.Error("garbage blob should be rejected")
	}

	// clamp + drop unknown; #12: minSeeders:-5 becomes nil (no filter), not 0.
	c, ok = decodeConfig(nil, blob(`{"debrid":[{"service":"realdebrid","token":"rd"}],"resultCap":9999,"filters":{"excludeRegex":"`+repeat("x", 400)+`","minSeeders":-5},"evil":"ignored"}`))
	if !ok || c.ResultCap != 200 || len(c.Filters.ExcludeRegex) != 256 || c.Filters.MinSeeders != nil {
		t.Fatalf("clamp: resultCap=%d regexLen=%d minSeeders=%v", c.ResultCap, len(c.Filters.ExcludeRegex), c.Filters.MinSeeders)
	}

	// valid optional filters + dedupe indexers (#10) + drop bogus
	c, _ = decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],"indexers":["torrentio","bogus","comet","torrentio"],"filters":{"resolutions":["2160p","nope","1080p"],"hdrOnly":true,"maxSizeGB":40}}`))
	if len(c.Indexers) != 2 || c.Indexers[0] != "torrentio" || c.Indexers[1] != "comet" {
		t.Errorf("indexers: %v", c.Indexers)
	}
	if len(c.Filters.Resolutions) != 2 || !c.Filters.HDROnly || c.Filters.MaxSizeGB == nil || *c.Filters.MaxSizeGB != 40 {
		t.Errorf("filters: %+v", c.Filters)
	}

	// defaults
	c, _ = decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}]}`))
	if !c.Filters.ExcludeCam || !c.CachedOnly || len(c.Indexers) != 4 {
		t.Errorf("defaults: %+v", c)
	}
	// explicit off
	c, _ = decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],"cachedOnly":false,"filters":{"excludeCam":false}}`))
	if c.Filters.ExcludeCam || c.CachedOnly {
		t.Errorf("explicit off not honored: %+v", c)
	}
}

func TestB64urlRoundTrip(t *testing.T) {
	in := "héllo—世界"
	dec, err := b64urlDecode(b64urlEncode([]byte(in)))
	if err != nil || string(dec) != in {
		t.Errorf("b64url round-trip: %q err=%v", dec, err)
	}
}

func TestParseStreamID(t *testing.T) {
	if s, ok := parseStreamID("movie", "tt1234567.json"); !ok || s.Type != "movie" || s.IMDb != "tt1234567" {
		t.Errorf("movie: %+v ok=%v", s, ok)
	}
	if s, ok := parseStreamID("series", "tt1234567:2:5.json"); !ok || !s.HasEp || s.Season != 2 || s.Episode != 5 {
		t.Errorf("series: %+v ok=%v", s, ok)
	}
	for _, bad := range [][2]string{{"catalog", "tt1"}, {"movie", "xx1"}, {"series", "tt1:2"}, {"series", "tt1:x:y"}} {
		if _, ok := parseStreamID(bad[0], bad[1]); ok {
			t.Errorf("expected reject: %v", bad)
		}
	}
}

func TestPlayToken(t *testing.T) {
	movie := PlayTarget{InfoHash: repeat("a", 40)}
	if got, ok := decodePlayToken(encodePlayToken(movie)); !ok || got.InfoHash != movie.InfoHash || got.FileIdx != nil {
		t.Errorf("movie round-trip: %+v ok=%v", got, ok)
	}
	series := PlayTarget{InfoHash: repeat("b", 40), FileIdx: intp(3), Season: intp(1), Episode: intp(2)}
	got, ok := decodePlayToken(encodePlayToken(series))
	if !ok || got.InfoHash != series.InfoHash || *got.FileIdx != 3 || *got.Season != 1 || *got.Episode != 2 {
		t.Errorf("series round-trip: %+v ok=%v", got, ok)
	}
	if got, _ := decodePlayToken(encodePlayToken(PlayTarget{InfoHash: repeat("C", 40)})); got == nil || got.InfoHash != repeat("c", 40) {
		t.Error("infohash should be lowercased")
	}
	if _, ok := decodePlayToken("!!!"); ok {
		t.Error("garbage token should be rejected")
	}
	if _, ok := decodePlayToken(blob(`{"h":"short"}`)); ok {
		t.Error("short hash should be rejected")
	}
}

func TestPickEpisodeFile(t *testing.T) {
	files := []TorrentFile{
		{Index: 0, Name: "Show.S01E01.mkv", SizeBytes: intp(100)},
		{Index: 1, Name: "Show.S01E02.1080p.mkv", SizeBytes: intp(900)},
		{Index: 2, Name: "Show.S01E02.sample.mkv", SizeBytes: intp(5)},
		{Index: 3, Name: "readme.txt", SizeBytes: intp(1)},
	}
	if got := pickEpisodeFile(files, 1, 2); got == nil || *got != 1 {
		t.Errorf("SxxExx pick (largest on ties): got %v want 1", got)
	}
	if got := pickEpisodeFile([]TorrentFile{{Index: 7, Name: "Show 1x03.mp4"}}, 1, 3); got == nil || *got != 7 {
		t.Errorf("1x03: got %v", got)
	}
	if got := pickEpisodeFile([]TorrentFile{{Index: 8, Name: "Show.104.mp4"}}, 1, 4); got == nil || *got != 8 {
		t.Errorf("104: got %v", got)
	}
	if got := pickEpisodeFile([]TorrentFile{{Index: 4, Name: "a.mkv", SizeBytes: intp(10)}, {Index: 5, Name: "b.mkv", SizeBytes: intp(20)}}, 9, 9); got == nil || *got != 5 {
		t.Errorf("no-match → largest: got %v want 5", got)
	}
	if got := pickEpisodeFile(nil, 1, 1); got != nil {
		t.Error("no files → nil")
	}
}

func TestSelectFileIDTorBox(t *testing.T) {
	// TorBox file ids (Index) are NOT positions in Torrentio's list, so a series episode must be
	// name-matched, never resolved via the passed-through fileIdx (FOLLOWUP #13).
	pack := []TorrentFile{
		{Index: 50, Name: "Show.S01E01.mkv", SizeBytes: intp(100)},
		{Index: 55, Name: "Show.S01E02.mkv", SizeBytes: intp(100)},
	}
	s1, e2, wrong := 1, 2, 99
	if got := selectFileID(pack, ResolveTarget{Season: &s1, Episode: &e2, FileIdx: &wrong}); got == nil || *got != 55 {
		t.Errorf("episode name-match wins over fileIdx: got %v want 55 (TorBox id)", got)
	}
	// fileIdx-only → POSITION in the list mapped to TorBox's file id (files[1].Index == 55).
	one := 1
	if got := selectFileID(pack, ResolveTarget{FileIdx: &one}); got == nil || *got != 55 {
		t.Errorf("fileIdx position → TorBox id: got %v want 55", got)
	}
	// fileIdx-only with no file list (single-file fast path / list failure) → raw passthrough.
	seven := 7
	if got := selectFileID(nil, ResolveTarget{FileIdx: &seven}); got == nil || *got != 7 {
		t.Errorf("fileIdx passthrough when no list: got %v want 7", got)
	}
	if got := selectFileID(pack, ResolveTarget{}); got != nil {
		t.Errorf("no selector → nil, got %v", got)
	}
}

func TestPickFileIDPrefersEpisodeMatch(t *testing.T) {
	s1, e2, wrong := 1, 2, 0 // fileIdx=0 would (wrongly) point at E01 by position
	rdPack := []TorrentFile{
		{Index: 10, Name: "Show.S01E01.mkv", SizeBytes: intp(100)},
		{Index: 20, Name: "Show.S01E02.mkv", SizeBytes: intp(100)},
	}
	if got := (&realDebridStore{}).pickFileID(rdPack, ResolveTarget{Season: &s1, Episode: &e2, FileIdx: &wrong}); got == nil || *got != 20 {
		t.Errorf("RD episode match over fileIdx: got %v want 20", got)
	}
	pmPack := []TorrentFile{ // Premiumize index == position
		{Index: 0, Name: "Show.S01E01.mkv", SizeBytes: intp(100)},
		{Index: 1, Name: "Show.S01E02.mkv", SizeBytes: intp(100)},
	}
	if got := (&premiumizeStore{}).pickIndex(pmPack, ResolveTarget{Season: &s1, Episode: &e2, FileIdx: &wrong}); got == nil || *got != 1 {
		t.Errorf("PM episode match over fileIdx: got %v want 1", got)
	}
}

func TestCleanLabelAndSize(t *testing.T) {
	s := RawStream{Title: "Movie 2160p WEB-DL HDR Atmos", SizeBytes: intp(18 * gib)}
	if got := cleanLabel(s); got != "4K • WEB-DL • HDR • Atmos • 18 GB" {
		t.Errorf("cleanLabel=%q", got)
	}
	sz := 3.4 * float64(gib)
	if got := sizeLabel(int(sz)); got != "3.4 GB" {
		t.Errorf("sizeLabel small=%q", got)
	}
	if got := sizeLabel(700 * mib); got != "700 MB" {
		t.Errorf("sizeLabel mb=%q", got)
	}
	if got := cleanLabel(RawStream{Title: "mysterious"}); got != "Stream" {
		t.Errorf("cleanLabel empty=%q", got)
	}
}

func TestStreamAttributes(t *testing.T) {
	a := streamAttributes(RawStream{Title: "Movie.2160p.BluRay.REMUX.DV.HDR.HEVC.Atmos", SizeBytes: intp(40 * gib), Seeders: intp(12), Cached: true})
	if a.Resolution == nil || *a.Resolution != "2160p" || a.Source == nil || *a.Source != "remux" ||
		a.Codec == nil || *a.Codec != "hevc" || !a.HDR || !a.DolbyVision || a.Audio == nil || *a.Audio != "Atmos" ||
		a.ThreeD || !a.Cached || a.Seeders == nil || *a.Seeders != 12 {
		t.Errorf("rich attrs: %+v", a)
	}
	sources := map[string]string{"X 1080p WEB-DL": "webdl", "X 1080p WEBRip": "webrip", "X 1080p BluRay": "bluray", "X 720p HDTV": "hdtv", "X DVDRip": "dvdrip", "X 2024 HDCAM": "cam"}
	for title, want := range sources {
		if s := streamAttributes(RawStream{Title: title}).Source; s == nil || *s != want {
			t.Errorf("source(%q)=%v want %q", title, s, want)
		}
	}
	if streamAttributes(RawStream{Title: "X mystery"}).Source != nil {
		t.Error("no source → nil")
	}
	if a := streamAttributes(RawStream{Title: "Movie 2160p WEB-DL HDR10"}); !a.HDR || a.DolbyVision {
		t.Error("HDR10 without DV")
	}
}

func TestMemoryCacheTTLAndEviction(t *testing.T) {
	now := time.Unix(1000, 0)
	c := NewMemoryCache(1 << 20)
	c.now = func() time.Time { return now }
	c.Put("k", "v", 10*time.Second)
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Fatal("should hit within TTL")
	}
	now = now.Add(11 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatal("should expire past TTL")
	}

	// byte-budget LRU eviction (audit #1): a small budget evicts the least-recently-used.
	c2 := NewMemoryCache(20) // ~ room for 2 small entries
	c2.now = func() time.Time { return now }
	c2.Put("a", "1", time.Hour) // 2 bytes
	c2.Put("b", "2", time.Hour)
	_, _ = c2.Get("a")                            // touch a → b is LRU
	c2.Put("cccccccccc", "3333333333", time.Hour) // 20 bytes → forces eviction
	if _, ok := c2.Get("b"); ok {
		t.Error("b should have been evicted (LRU) under the byte budget")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
