package scout

import (
	"encoding/json"
	"regexp"
	"runtime"
	"strings"
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
	c, _ = decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],"indexers":["torrentio","bogus","torz","torrentio"],"filters":{"resolutions":["2160p","nope","1080p"],"hdrOnly":true,"maxSizeGB":40}}`))
	// torz is named here but centrally disabled (its host has no DNS), so it is dropped despite being
	// asked for: a sealed config cannot be edited from the server, and carrying a dead host makes the
	// survivors look like a quorum when one of them sheds a request.
	if len(c.Indexers) != 1 || c.Indexers[0] != "torrentio" {
		t.Errorf("indexers: %v", c.Indexers)
	}

	if len(c.Filters.Resolutions) != 2 || !c.Filters.HDROnly || c.Filters.MaxSizeGB == nil || *c.Filters.MaxSizeGB != 40 {
		t.Errorf("filters: %+v", c.Filters)
	}

	// A counted repetition is dropped from a user pattern. RE2 is linear at MATCH time, which is what
	// makes this filter safe to run over a stream list — but nothing bounded it at COMPILE time, and `{n}`
	// is expanded into the program. 250 characters inside the existing cap compile to 53 MiB of live heap,
	// from an unauthenticated caller, inside a 230 MiB GOMEMLIMIT.
	for _, bad := range []string{
		`(?:` + repeat("x", 240) + `){1000}`,
		`a{1000}`,
		`a{100,}`,
		`a{2,50}`,
		// The escape-state cases. The first version of this guard asked "is the character before the
		// brace a backslash", which is a different question: here the `\\` is an ESCAPED BACKSLASH and
		// the quantifier after it is live, so the lookbehind called it escaped and let it through.
		`\\{1000}`,
		`a\\{1000}`,
		`{1000}`, // at the very start, where there is no preceding character at all
	} {
		c, ok := decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],"filters":{"excludeRegex":`+jsonString(bad)+`}}`))
		if !ok {
			t.Fatalf("config carrying %q was rejected outright", bad)
		}
		if c.Filters.ExcludeRegex != "" {
			t.Errorf("counted repetition survived validation: %q", c.Filters.ExcludeRegex)
		}
	}
	// The patterns people actually write are untouched — including a genuinely escaped brace, a unicode
	// class (whose braces carry no digits), and a brace inside a character class.
	for _, good := range []string{`hindi|tamil|dubbed`, `\bhc\b`, `x\{3\}`, `\p{Latin}`, `[{]1000}`} {
		c, _ := decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],"filters":{"excludeRegex":`+jsonString(good)+`}}`))
		if c.Filters.ExcludeRegex != good {
			t.Errorf("a legitimate pattern was dropped: %q → %q", good, c.Filters.ExcludeRegex)
		}
	}

	// preferResolution is whitelisted like every other resolution field. An unknown value becomes "no
	// preference" rather than a preference nothing can satisfy — which would sink every release equally.
	pref, _ := decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],"filters":{"preferResolution":"1080p"}}`))
	if pref.Filters.PreferResolution != "1080p" {
		t.Errorf("preferResolution: %q", pref.Filters.PreferResolution)
	}
	bogus, _ := decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],"filters":{"preferResolution":"8k"}}`))
	if bogus.Filters.PreferResolution != "" {
		t.Errorf("an unknown preferResolution was carried: %q", bogus.Filters.PreferResolution)
	}

	// A live host that needs a config PATH is not disabled. comet 403s the bare path and answers on a
	// config path, which `configPathIndexers` already handles — disabling it stripped it before
	// makeScrapers could mint one, leaving the install with a single real source.
	withComet, _ := decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],"indexers":["torrentio","comet"]}`))
	if len(withComet.Indexers) != 2 {
		t.Errorf("comet needs a config path, not a headstone: %v", withComet.Indexers)
	}

	// defaults. A config carrying only the account defers everything else to the server, which is what
	// /configure builds by default — so these ARE the shipped defaults, not incidental fallbacks.
	// cachedOnly is deliberately off: inheriting "hide anything not already fetched" can leave a title
	// showing one scrap of a release, or nothing, while good ones sit a download away.
	c, _ = decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}]}`))
	// The default indexer set is the working one, not every one this server knows how to talk to.
	if !c.Filters.ExcludeCam || c.CachedOnly || c.ResultCap != 20 ||
		len(c.Indexers) != 2 || c.Indexers[0] != "torrentio" || c.Indexers[1] != "mediafusion" {
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
	if got, err := pickEpisodeFile(files, 1, 2); err != nil || got == nil || *got != 1 {
		t.Errorf("SxxExx pick (largest on ties): got %v, %v want 1", got, err)
	}
	if got, err := pickEpisodeFile([]TorrentFile{{Index: 7, Name: "Show 1x03.mp4"}}, 1, 3); err != nil || got == nil || *got != 7 {
		t.Errorf("1x03: got %v, %v", got, err)
	}
	if got, err := pickEpisodeFile([]TorrentFile{{Index: 8, Name: "Show.104.mp4"}}, 1, 4); err != nil || got == nil || *got != 8 {
		t.Errorf("104: got %v, %v", got, err)
	}
	// Neither name is episode-labelled and there is more than one candidate, so this function has NO
	// opinion — the largest-video fallback moved out to the callers, where it comes after the indexer's
	// fileIdx instead of pre-empting it. Answering here made a guess beat a fact: a non-nil result makes
	// every caller return before FileIdx is read, so a bare-numbered pack ("[Grp] Show - 03") served the
	// same file for every episode of it.
	if got, err := pickEpisodeFile([]TorrentFile{{Index: 4, Name: "a.mkv", SizeBytes: intp(10)}, {Index: 5, Name: "b.mkv", SizeBytes: intp(20)}}, 9, 9); err != nil || got != nil {
		t.Errorf("no-match on a multi-file pool → no opinion: got %v, %v", got, err)
	}
	// One candidate is different: there the largest really is the only thing it could be.
	if got, err := pickEpisodeFile([]TorrentFile{{Index: 4, Name: "a.mkv", SizeBytes: intp(10)}}, 9, 9); err != nil || got == nil || *got != 4 {
		t.Errorf("a lone unlabelled video is still the answer: got %v, %v want 4", got, err)
	}
	if got, err := pickEpisodeFile(nil, 1, 1); got != nil || err != nil {
		t.Errorf("no files → nil: got %v, %v", got, err)
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
	if got, err := selectFileID(pack, ResolveTarget{Season: &s1, Episode: &e2, FileIdx: &wrong}); err != nil || got == nil || *got != 55 {
		t.Errorf("episode name-match wins over fileIdx: got %v, %v want 55 (TorBox id)", got, err)
	}
	// fileIdx-only → POSITION in the list mapped to TorBox's file id (files[1].Index == 55).
	one := 1
	if got, err := selectFileID(pack, ResolveTarget{FileIdx: &one}); err != nil || got == nil || *got != 55 {
		t.Errorf("fileIdx position → TorBox id: got %v, %v want 55", got, err)
	}
	// fileIdx-only with no file list (single-file fast path / list failure) → raw passthrough.
	seven := 7
	if got, err := selectFileID(nil, ResolveTarget{FileIdx: &seven}); err != nil || got == nil || *got != 7 {
		t.Errorf("fileIdx passthrough when no list: got %v, %v want 7", got, err)
	}
	if got, err := selectFileID(pack, ResolveTarget{}); got != nil || err != nil {
		t.Errorf("no selector → nil, got %v, %v", got, err)
	}
}

func TestPickFileIDPrefersEpisodeMatch(t *testing.T) {
	s1, e2, wrong := 1, 2, 0 // fileIdx=0 would (wrongly) point at E01 by position
	rdPack := []TorrentFile{
		{Index: 10, Name: "Show.S01E01.mkv", SizeBytes: intp(100)},
		{Index: 20, Name: "Show.S01E02.mkv", SizeBytes: intp(100)},
	}
	if got, err := (&realDebridStore{}).pickFileID(rdPack, ResolveTarget{Season: &s1, Episode: &e2, FileIdx: &wrong}); err != nil || got == nil || *got != 20 {
		t.Errorf("RD episode match over fileIdx: got %v, %v want 20", got, err)
	}
	pmPack := []TorrentFile{ // Premiumize index == position
		{Index: 0, Name: "Show.S01E01.mkv", SizeBytes: intp(100)},
		{Index: 1, Name: "Show.S01E02.mkv", SizeBytes: intp(100)},
	}
	if got, err := (&premiumizeStore{}).pickIndex(pmPack, ResolveTarget{Season: &s1, Episode: &e2, FileIdx: &wrong}); err != nil || got == nil || *got != 1 {
		t.Errorf("PM episode match over fileIdx: got %v, %v want 1", got, err)
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
	a := streamAttributes(RawStream{Title: "Movie.2160p.BluRay.REMUX.DV.HDR.HEVC.Atmos", SizeBytes: intp(40 * gib),
		Seeders: intp(12), Cached: true, CacheKnown: true})
	if a.Resolution == nil || *a.Resolution != "2160p" || a.Source == nil || *a.Source != "remux" ||
		a.Codec == nil || *a.Codec != "hevc" || !a.HDR || !a.DolbyVision || a.Audio == nil || *a.Audio != "Atmos" ||
		a.HDRFormat == nil || *a.HDRFormat != "HDR" || a.ThreeD || a.Cached == nil || !*a.Cached ||
		a.Seeders == nil || *a.Seeders != 12 {
		t.Errorf("rich attrs: %+v", a)
	}

	// Cachedness is reported only when it was observed. A failed cache check leaves `Cached` at its zero
	// value, and sending that as `false` is a claim nobody made — the client reads a definite "must
	// download" and queues a fetch for a release the debrid may already hold.
	unknown := streamAttributes(RawStream{Title: "Movie.1080p.WEB-DL", Cached: false, CacheKnown: false})
	if unknown.Cached != nil {
		t.Errorf("unchecked cachedness must be omitted, got %v", *unknown.Cached)
	}
	held := streamAttributes(RawStream{Title: "Movie.1080p.WEB-DL", Cached: false, CacheKnown: true})
	if held.Cached == nil || *held.Cached {
		t.Errorf("an observed 'not held' must be reported as false, got %v", held.Cached)
	}
	// HDR10-family variants are distinguished (a stream can be DV *and* carry an HDR10 base).
	hdrCases := map[string]string{
		"Movie 2160p WEB-DL DDP5.1 DV HDR10+ HEVC": "HDR10+", // HDR10+ beats a bare hdr10 token
		"Movie 2160p WEB-DL HLG HEVC":              "HLG",
		"Movie 2160p WEB-DL HDR10 HEVC":            "HDR10",
		"Movie 2160p WEB-DL HDR HEVC":              "HDR",
		"Movie 1080p BluRay x264":                  "",
	}
	for title, want := range hdrCases {
		f := streamAttributes(RawStream{Title: title}).HDRFormat
		got := ""
		if f != nil {
			got = *f
		}
		if got != want {
			t.Errorf("hdrFormat(%q)=%q want %q", title, got, want)
		}
	}

	// Source-truth audio: normalized codec family + channel layout + atmos flag (so the client can
	// compute what Den actually delivers). DDP5 1 (space) still resolves to 5.1.
	type ac struct {
		codec, channels string
		atmos           bool
	}
	audioCases := map[string]ac{
		"Movie 2160p WEB-DL DDP5.1 Atmos HEVC":    {"eac3", "5.1", true},
		"Movie 2160p REMUX TrueHD.Atmos.7.1 HEVC": {"truehd", "7.1", true},
		"Movie 1080p BluRay DTS-HD.MA.5.1 x264":   {"dtshdma", "5.1", false},
		"Movie 2160p UHD BluRay DTS-X 7.1 HEVC":   {"dtsx", "7.1", false},
		"Movie 2160p WEB-DL DDP5 1 HEVC":          {"eac3", "5.1", false},
		"Movie 1080p BluRay AC3 2.0 x264":         {"ac3", "2.0", false},
	}
	for title, want := range audioCases {
		g := streamAttributes(RawStream{Title: title})
		codec, chans := "", ""
		if g.AudioCodec != nil {
			codec = *g.AudioCodec
		}
		if g.AudioChannels != nil {
			chans = *g.AudioChannels
		}
		if codec != want.codec || chans != want.channels || g.Atmos != want.atmos {
			t.Errorf("audio(%q)= {%q %q %v} want {%q %q %v}", title, codec, chans, g.Atmos, want.codec, want.channels, want.atmos)
		}
	}
	if !streamAttributes(RawStream{Title: "Movie 2160p WEB-DL KORSUB HEVC"}).HardcodedSubs {
		t.Error("KORSUB → hardcodedSubs")
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

// jsonString quotes a value for embedding in a config fixture, so a pattern full of backslashes and
// braces reaches the decoder exactly as a client would have sent it.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// An absurd config segment is refused before it is decoded, and the slices a build carries are bounded.
//
// Nothing capped the segment, and net/http accepts a request line up to its 1 MiB default: 716,770
// characters of path decoded to a VALID config with 16,289 debrid accounts, which one request then
// retained for the length of a scrape. Distinct blobs defeat both the list cache and the singleflight, so
// 150 concurrent misses measured 207 MiB inside a 230 MiB GOMEMLIMIT.
func TestDecodeConfig_boundsWhatOneRequestCanRetain(t *testing.T) {
	// A segment past the ceiling is refused without being decoded.
	if _, ok := decodeConfig(nil, repeat("A", maxConfigBlob+1)); ok {
		t.Error("an oversized config segment was accepted")
	}
	// An honest segment is nowhere near it — the real /configure output is a few hundred bytes.
	honest := blob(`{"debrid":[{"service":"torbox","token":"` + repeat("t", 64) + `"}],` +
		`"indexers":["torrentio","comet","mediafusion"],` +
		`"filters":{"excludeCam":true,"hdrOnly":true,"resolutions":["2160p","1080p"],` +
		`"preferResolution":"1080p","minSeeders":3,"maxSizeGB":40,"excludeRegex":"hindi|tamil|dubbed"},` +
		`"cachedOnly":true,"resultCap":20}`)
	if len(honest) > maxConfigBlob/4 {
		t.Errorf("a full honest config is %d bytes, uncomfortably close to the %d cap", len(honest), maxConfigBlob)
	}
	if _, ok := decodeConfig(nil, honest); !ok {
		t.Fatal("a full honest config was refused")
	}

	// Accounts are capped, and repeated resolutions are deduped rather than retained — four valid values
	// mean a repeat carries no information, and rankStreams scans that slice per release.
	// Sized to fit under the blob cap, so this exercises the FIELD caps rather than being refused by the
	// segment cap one layer up.
	var accounts, resolutions []string
	for i := 0; i < 100; i++ {
		accounts = append(accounts, `{"service":"torbox","token":"t"}`)
		resolutions = append(resolutions, `"1080p"`)
	}
	cfg, ok := decodeConfig(nil, blob(`{"debrid":[`+strings.Join(accounts, ",")+`],`+
		`"filters":{"resolutions":[`+strings.Join(resolutions, ",")+`]}}`))
	if !ok {
		t.Fatal("the padded config was refused outright")
	}
	if len(cfg.Debrid) > maxDebridAccounts {
		t.Errorf("kept %d debrid accounts, want at most %d", len(cfg.Debrid), maxDebridAccounts)
	}
	if len(cfg.Filters.Resolutions) != 1 {
		t.Errorf("kept %d resolutions for one repeated value, want 1", len(cfg.Filters.Resolutions))
	}
}

// maxCompiledMiB bounds what a pattern surviving validation may cost to compile.
//
// Ten, and both ends of that are measured. The worst SURVIVING shape found is a repeated Unicode
// CATEGORY — `\pC|` sixty-two times — at 3.1 MiB, so a tighter bound fails on a legal pattern; the bomb
// the guard exists for is 53.1 MiB, so a looser one stops catching it. An earlier version of this test
// asserted 2 MiB and passed only because its samples used `\p{Latin}`, a SCRIPT of ~30 rune ranges,
// where `\pL` and `\pC` are categories of hundreds — the same construct and length at ninety times the
// cost. Sixteen concurrent worst-survivors is ~50 MiB of peak inside a 230 MiB GOMEMLIMIT, which is why
// 3.1 MiB is tolerable and 53 is not.
const maxCompiledMiB = 10

// The guard exists for a memory bound, so the bound is what gets asserted — not just which patterns the
// matcher happens to flag.
//
// A user pattern is up to 256 characters, compiled fresh on every list build, from a caller whose debrid
// token is never verified. The BOMB IS IN THE TABLE deliberately: it is the only entry containing a
// brace, so it is the only thing coupling this test to the guard. Without it, disabling
// hasCountedRepeat entirely left this test green — every flagged pattern hits the `continue` below and
// asserts nothing, so the test claimed a bound it could not have noticed being broken.
func TestExcludeRegex_compiledSizeIsBounded(t *testing.T) {
	worst := []string{
		// The bomb. Dropped by the guard; if the guard ever stops dropping it, it lands at ~53 MiB and
		// this test is what says so.
		`(?:` + strings.Repeat("x", 240) + `){1000}`,
		// Unicode categories — the worst shapes that legitimately SURVIVE, at 1.7-3.1 MiB.
		strings.Repeat(`\pC|`, 62) + "a",
		strings.Repeat(`\pL|`, 62) + "a",
		strings.Repeat(`\pC`, 83),
		// And the cheap shapes, all under 0.05 MiB.
		strings.Repeat("ab|", 84),
		strings.Repeat("(", 60) + "a" + strings.Repeat(")", 60) + "*",
		strings.Repeat("k", 250),
		strings.Repeat("[a-zA-Z0-9]", 23),
		strings.Repeat(`\p{Latin}`, 28),
		strings.Repeat("(a*)", 62),
	}
	for _, pat := range worst {
		if len(pat) > 256 {
			pat = pat[:256]
		}
		cfg, ok := decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],"filters":{"excludeRegex":`+jsonString(pat)+`}}`))
		if !ok {
			t.Fatalf("rejected outright: %.40q", pat)
		}
		if cfg.Filters.ExcludeRegex == "" {
			continue // the guard dropped it, so it is bounded by construction
		}
		var m1, m2 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m1)
		re, err := regexp.Compile("(?i)" + cfg.Filters.ExcludeRegex)
		runtime.ReadMemStats(&m2)
		mib := float64(m2.TotalAlloc-m1.TotalAlloc) / (1 << 20)
		if err == nil && mib > maxCompiledMiB {
			t.Errorf("a %d-char pattern that survived validation compiled to %.1f MiB (max %d): %.40q",
				len(pat), mib, maxCompiledMiB, pat)
		}
		_ = re
	}
}
