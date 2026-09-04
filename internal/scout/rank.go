package scout

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dlclark/regexp2"
)

// Ported from src/rank.ts (VortX junkClass + additive quality score). All patterns run against a
// lowercased title. Pure; no I/O.

const gib = 1_073_741_824

// RawStream is a scrape result before ranking — the raw torrent fact plus the debrid cache truth.
type RawStream struct {
	InfoHash  string
	FileIdx   *int
	Title     string
	SizeBytes *int
	Seeders   *int
	Cached    bool
	// Whether anyone could answer the cachedness question this request. False means the check failed, so
	// `Cached` is a default rather than an observation — and must not be reported to a client as one.
	CacheKnown bool
	Source     string
	// What the file itself said, when it was probed. nil means "not asked" — distinct from "asked and it
	// told us nothing", so the client can tell an unprobed release from a genuinely featureless one.
	Probe *Probe
}

// matcher unifies RE2 (stdlib) and regexp2 (for the one lookbehind pattern).
type matcher interface{ match(string) bool }

type re2 struct{ *regexp.Regexp }

func (r re2) match(s string) bool { return r.MatchString(s) }

type r2 struct{ *regexp2.Regexp }

func (r r2) match(s string) bool { ok, _ := r.MatchString(s); return ok }

func mustRE2(p string) matcher { return re2{regexp.MustCompile(p)} }
func mustR2(p string) matcher  { return r2{regexp2.MustCompile(p, regexp2.None)} }

// Good-source markers downgrade bare cam/ts/scr to non-junk.
var goodSource = mustRE2(`remux|bluray|blu-ray|b[dr][ .\-_]?rip|web[ .\-_]?(dl|rip)?|hdtv|dvd[ .\-_]?rip`)

type junkPattern struct {
	class string
	re    matcher
}

// Unambiguous junk forms — always junk. The "upscaled" pattern uses lookbehind → regexp2 (RE2 can't).
var unambiguousJunk = []junkPattern{
	{"cam", mustRE2(`h[dq][ .\-_]?cam(rip)?|cam[ .\-_]?rip|s[ .\-]+print`)},
	{"telesync", mustRE2(`telesynch?|hd[ .\-_]?ts(rip)?|ts[ .\-_]?rip`)},
	{"telecine", mustRE2(`telecine|hd[ .\-_]?tc`)},
	{"screener", mustRE2(`(dvd|bd|br|web|hd)[ .\-_]?scr|p(re)?dvd(rip)?|screener`)},
	{"workprint", mustRE2(`workprint`)},
	{"r5", mustRE2(`\br5\b`)},
	{"upscaled", mustR2(`1xbet|read[ .\-_]?note|(?<!not[ .\-_])(?<!non[ .\-_])(upscaled?|up[ .\-_]?rez)|ai[ .\-_]?(upscaled?|enhanced?)|re[ .\-_]?graded?`)},
}

var (
	bareCam = mustRE2(`\bcam\b`)
	bareTS  = mustRE2(`\bts\b`)
	bareScr = mustRE2(`\bscr\b`)
)

// junkClass returns the junk class of a title, or "" if it's a legit source.
func junkClass(title string) string { return junkClassOf(strings.ToLower(title)) }

// junkClassOf assumes an already-lowercased title (audit #17: compute the lowercasing once).
func junkClassOf(t string) string {
	for _, j := range unambiguousJunk {
		if j.re.match(t) {
			return j.class
		}
	}
	if !goodSource.match(t) {
		if bareCam.match(t) {
			return "cam"
		}
		if bareTS.match(t) {
			return "telesync"
		}
		if bareScr.match(t) {
			return "screener"
		}
	}
	return ""
}

var (
	res2160  = mustRE2(`2160p?`)
	res1440  = mustRE2(`1440p?`)
	res1080  = mustRE2(`1080p?`)
	res720   = mustRE2(`720p?`)
	res576   = mustRE2(`576p?`)
	res540   = mustRE2(`540p?`)
	res480   = mustRE2(`480p?`)
	res4kUHD = mustRE2(`4k|uhd`)
)

// detectResolution: coarse bucket for the resolutions filter ("" when untagged).
func detectResolution(title string) string { return detectResolutionLower(strings.ToLower(title)) }

func detectResolutionLower(t string) string {
	switch {
	case res2160.match(t):
		return "2160p"
	case res1080.match(t):
		return "1080p"
	case res720.match(t):
		return "720p"
	case res480.match(t) || res576.match(t) || res540.match(t):
		return "480p"
	}
	return ""
}

func resolutionBase(t string, sizeBytes *int) int {
	switch {
	case res2160.match(t):
		return 4000
	case res1440.match(t):
		return 1440
	case res1080.match(t):
		return 1080
	case res720.match(t):
		return 720
	case res576.match(t):
		return 576
	case res540.match(t):
		return 540
	case res480.match(t):
		return 480
	}
	if res4kUHD.match(t) && intOr(sizeBytes, 0) > 3*gib {
		return 4000
	}
	return 100
}

var (
	reRemux    = mustRE2(`\bremux\b`)
	reBluray   = mustRE2(`bluray|blu-ray`)
	reBrRip    = mustRE2(`b[dr][ .\-_]?rip`)
	reWebDL    = mustRE2(`web[ .\-_]?dl`)
	reWebRip   = mustRE2(`web[ .\-_]?rip`)
	reWeb      = mustRE2(`\bweb\b`)
	reHDTV     = mustRE2(`\bhdtv\b`)
	reDvdRip   = mustRE2(`dvd[ .\-_]?rip`)
	reLowSrc   = mustRE2(`tvrip|satrip|pdtv`)
	reDoVi     = mustRE2(`dolby vision|dolbyvision|dovi`)
	reHDR10p   = mustRE2(`hdr10\+|hdr10plus`)
	reHDR      = mustRE2(`\bhdr\b|\bhlg\b`)
	reAtmos    = mustRE2(`atmos`)
	reDTSX     = mustRE2(`dts:x|dtsx|dts-x`)
	reTrueHD   = mustRE2(`truehd|true-hd`)
	reDTSHDMA  = mustRE2(`dts-hd ma|dts-hd\.ma|dts-ma`)
	reDTSHD    = mustRE2(`dts-hd|dts hd|dtshd|flac|lpcm|pcm`)
	reEAC3     = mustRE2(`eac3|e-ac3|dd\+|ddp|ddplus`)
	reDTS      = mustRE2(`\bdts\b`)
	reAC3      = mustRE2(`ac3|\bdd\b|dolby digital`)
	reIs4K     = mustRE2(`2160p?|4k|uhd`)
	reAV1      = mustRE2(`av1`)
	re3D       = mustRE2(`\b3d\b|hsbs|half[ .\-_]?sbs|sbs[ .\-_]?3d`)
	reKorsubHC = mustRE2(`korsub|\bhc\b`)
	// HDROnly filter (audit: matches the TS hdrOnly regex).
	reHDROnly = mustRE2(`dolby vision|dolbyvision|dovi|\bhdr\b|hdr10|\bhlg\b`)
)

// capSeeds trims a scrape result to the most promising `limit` releases.
//
// The cap itself is right: it bounds the debrid cache-check fan-out, so a misbehaving or hostile indexer
// returning thousands of tiny stream objects cannot become hundreds of concurrent outbound requests.
// What was wrong is WHICH ones it kept. Trimming in scrape order is arbitrary — past the cap a 2160p
// REMUX sitting at the tail was dropped while cam junk at the head survived, and neither the viewer nor
// the log saw it happen.
//
// Scored on title-derived signals only, because this runs BEFORE the cache check: `Cached` is false on
// everything here, so its bonus is uniformly absent and cannot bias the choice. Seeders break ties.
// Selection only — the real ranking still runs afterwards over whatever survives.
func capSeeds(streams []RawStream, limit int) []RawStream {
	if limit <= 0 || len(streams) <= limit {
		return streams
	}
	type ranked struct {
		s       RawStream
		score   int
		seeders int
	}
	all := make([]ranked, len(streams))
	for i, s := range streams {
		all[i] = ranked{s, qualityScore(s), intOr(s.Seeders, 0)}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].seeders > all[j].seeders
	})
	out := make([]RawStream, limit)
	for i := range out {
		out[i] = all[i].s
	}
	return out
}

// qualityScore — additive, higher wins.
func qualityScore(s RawStream) int {
	t := strings.ToLower(s.Title)
	return qualityScoreLower(t, s, junkClassOf(t))
}

func qualityScoreLower(t string, s RawStream, junk string) int {
	score := 0
	if junk != "" {
		score -= 100_000
	}
	if s.Cached {
		score += 8000
	}
	score += resolutionBase(t, s.SizeBytes)

	switch {
	case reRemux.match(t):
		score += 230
	case reBluray.match(t) || reBrRip.match(t):
		score += 150
	case reWebDL.match(t):
		score += 75
	case reWebRip.match(t):
		score += 50
	case reWeb.match(t):
		score += 75
	}
	if reHDTV.match(t) {
		score -= 150
	}
	if reDvdRip.match(t) {
		score -= 200
	}
	if reLowSrc.match(t) {
		score -= 300
	}

	switch {
	case reDoVi.match(t):
		score += 30
	case reHDR10p.match(t):
		score += 24
	case reHDR.match(t):
		score += 18
	}

	// Audio scored by what Den DELIVERS, not the source spec: DD+/EAC3 stream-copies (and EAC3+JOC keeps
	// real Atmos), while TrueHD / DTS / DTS-HD bridge down to EAC3 5.1 (a transcode, losing Atmos + 7.1).
	// So a DD+ cut outranks a TrueHD/DTS cut of the same title even though the latter is "better" on paper.
	atmos := reAtmos.match(t)
	switch {
	case reEAC3.match(t) && atmos:
		score += 30 // DD+ Atmos — real Atmos, stream-copied on every route
	case reEAC3.match(t):
		score += 20 // DD+ 5.1/7.1 — stream-copy, native (7.1 preserved)
	case reAC3.match(t):
		score += 14 // AC3 — stream-copy, native 5.1
	case reDTSX.match(t) || reTrueHD.match(t) || reDTSHDMA.match(t) || reDTSHD.match(t):
		score += 12 // lossless/object source, but Den bridges → EAC3 5.1 (below native DD+)
	case reDTS.match(t):
		score += 8 // DTS core — bridged → 5.1
	case atmos:
		score += 20 // Atmos with no identifiable codec token — assume DD+ (most streaming Atmos is)
	}

	if s.SizeBytes != nil {
		add := int(math.Round(float64(*s.SizeBytes) / float64(gib) * 6))
		if add > 600 {
			add = 600
		}
		score += add
	}

	is4k := reIs4K.match(t)
	if reAV1.match(t) {
		if is4k {
			score -= 1500
		} else {
			score -= 150
		}
	}
	if re3D.match(t) {
		score -= 2000
	}
	if reKorsubHC.match(t) {
		score -= 200
	}
	return score
}

// Real-Debrid anti-piracy filename block (src/rank.ts realDebridBlocked).
var (
	rdBlockSubstrings  = []string{"web-dl", "webrip", "bdrip", "hdrip", "dvdrip"}
	rdBlockDotAdjacent = []string{"bluray.x264", "hdtv.x264", "hdtv.xvid", "web.x264", "web.h264"}
)

func realDebridBlocked(title string) bool {
	t := strings.ToLower(title)
	for _, s := range rdBlockSubstrings {
		if strings.Contains(t, s) {
			return true
		}
	}
	for _, s := range rdBlockDotAdjacent {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// preferenceSink is what a soft preference costs a release that does not match it.
//
// Derived, not picked: it has to outweigh every QUALITY signal so the preferred bucket really does come
// first, and it must not outweigh cachedness or junk, which are facts about playability rather than
// taste. The quality signals span at most resolutionBase (3900) + remux (230) + HDR (30) + audio (30) +
// size (600) = 4790, so 5000 clears them all. Cached (+8000) still wins: a cached 4K above an uncached
// 1080p is the right answer even for someone who asked for 1080p, because the alternative is a wait.
// Junk (-100000) is untouched — a preference must never lift a CAM.
const preferenceSink = 5000

type rankFilters struct {
	ExcludeCam  bool
	Resolutions []string
	// PreferResolution sinks other resolutions below this one rather than dropping them. "" → off.
	PreferResolution string
	HDROnly          bool
	MinSeeders   *int
	MaxSizeGB    *int
	ExcludeRegex string
	CachedOnly   bool
	ResultCap    int
	// ExpectedYear (movies): drop a release whose parsed year is clearly different — trackers sometimes
	// mistag a torrent with another title's IMDb id. Year survives translation (a Spanish-titled release
	// of the same film keeps the year); title matching would wrongly drop it. nil = no year filter.
	ExpectedYear *int
	// ExpectedTitleTokens (movies): significant tokens of the requested title. Applied ONLY to a release
	// with no parseable year — where ExpectedYear can't judge it — requiring at least one token overlap.
	// This drops title-less junk ("B-Bead.mp4") that a mistagged id surfaces once excludeCam removes the
	// real releases, while keeping foreign-language releases (which carry the year, or a name/number
	// token). Empty = no title filter (best-effort; a Cinemeta lookup failure serves unfiltered).
	ExpectedTitleTokens map[string]bool
}

// titleTokenRe splits a release/title into alphanumeric tokens.
var titleTokenRe = regexp.MustCompile(`[a-z0-9]+`)

// titleStop are tokens ignored when comparing a release name to the requested title: release metadata
// (resolution/codec/source/container/language) and stopwords that would create false overlaps.
var titleStop = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "and": true, "to": true, "in": true, "el": true,
	"la": true, "los": true, "las": true, "de": true, "le": true, "il": true,
	"2160p": true, "1080p": true, "720p": true, "480p": true, "4k": true, "uhd": true, "hd": true,
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true, "av1": true, "xvid": true,
	"web": true, "webdl": true, "webrip": true, "bluray": true, "bdrip": true, "brrip": true,
	"hdtv": true, "hdts": true, "hdtc": true, "remux": true, "cam": true, "camrip": true, "ts": true,
	"hdr": true, "hdr10": true, "dv": true, "sdr": true, "aac": true, "ac3": true, "dts": true,
	"atmos": true, "ddp": true, "mp4": true, "mkv": true, "avi": true, "esp": true, "eng": true,
	"lat": true, "dub": true, "dubbed": true, "sub": true, "subs": true, "multi": true, "dual": true,
}

// titleTokens splits a title into significant lowercase tokens (stopwords / format-codec noise removed,
// bare years and single-letter noise dropped). Used to sanity-check a year-less release against the request.
func titleTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range titleTokenRe.FindAllString(strings.ToLower(s), -1) {
		if titleStop[tok] {
			continue
		}
		if len(tok) == 4 && yearToken.MatchString(tok) {
			continue // a bare year
		}
		if len(tok) < 2 && (tok < "0" || tok > "9") {
			continue // single-letter noise (keep single digits, e.g. "5" in "Toy Story 5")
		}
		out[tok] = true
	}
	return out
}

// titleOverlap reports whether the release shares at least one significant token with the expected title.
func titleOverlap(releaseTitle string, expected map[string]bool) bool {
	for tok := range titleTokens(releaseTitle) {
		if expected[tok] {
			return true
		}
	}
	return false
}

// yearToken matches a plausible 4-digit film year (1900–2039); releaseYears then rejects matches that
// are actually resolution/codec digits (1920x1080, 2160p, x264) by checking the neighbouring chars.
var yearToken = regexp.MustCompile(`19\d\d|20[0-3]\d`)

// releaseYears returns every plausible year in a release name (usually just one, e.g. "(2026)").
func releaseYears(title string) []int {
	t := strings.ToLower(title)
	var out []int
	for _, loc := range yearToken.FindAllStringIndex(t, -1) {
		var before, after byte = ' ', ' '
		if loc[0] > 0 {
			before = t[loc[0]-1]
		}
		if loc[1] < len(t) {
			after = t[loc[1]]
		}
		if (before >= '0' && before <= '9') || before == 'x' {
			continue // trailing digits of a bigger number / a resolution like 1920x…
		}
		if (after >= '0' && after <= '9') || after == 'p' || after == 'i' || after == 'x' {
			continue // 2160p, 1920x…, etc.
		}
		if y, err := strconv.Atoi(t[loc[0]:loc[1]]); err == nil {
			out = append(out, y)
		}
	}
	return out
}

// yearMismatch reports whether a title has a parseable year and none of them is within ±1 of want.
// No parseable year → not a mismatch (we can't tell, so keep the stream).
func yearMismatch(title string, want int) bool {
	years := releaseYears(title)
	if len(years) == 0 {
		return false
	}
	for _, y := range years {
		if y >= want-1 && y <= want+1 {
			return false
		}
	}
	return true
}

// rankStreams filters then sorts by qualityScore (seeders tiebreak, stable), then caps. Single pass
// over the filters (audit #18), lowercasing + junkClass computed once per stream (audit #17). The
// user excludeRegex runs on RE2 (linear-time → ReDoS-safe, audit #9).
func rankStreams(streams []RawStream, f rankFilters) []RawStream {
	var excludeRe *regexp.Regexp
	if f.ExcludeRegex != "" {
		excludeRe, _ = regexp.Compile("(?i)" + f.ExcludeRegex) // malformed/incompatible → nil → ignored
	}
	allowed := map[string]bool{}
	for _, r := range f.Resolutions {
		allowed[r] = true
	}

	type scored struct {
		s       RawStream
		idx     int
		score   int
		seeders int
	}
	var out []scored
	for i, s := range streams {
		lower := strings.ToLower(s.Title)
		junk := junkClassOf(lower)
		if f.ExcludeCam && junk != "" {
			continue
		}
		if excludeRe != nil && excludeRe.MatchString(s.Title) {
			continue
		}
		// Drop a mistagged torrent (another film's IMDb id) — its release year is clearly wrong.
		if f.ExpectedYear != nil && yearMismatch(s.Title, *f.ExpectedYear) {
			continue
		}
		// A release with no parseable year can't be year-checked; if we know the title, require at least
		// one significant-token overlap so year-less junk ("B-Bead.mp4") is dropped. Foreign-language
		// releases survive on the year gate above, or on a shared name/number token.
		if len(f.ExpectedTitleTokens) > 0 && len(releaseYears(s.Title)) == 0 &&
			!titleOverlap(s.Title, f.ExpectedTitleTokens) {
			continue
		}
		// Computed once for both the hard filter and the soft preference, and only when one of them is
		// actually set — an install using neither pays nothing for this.
		res := ""
		if len(allowed) > 0 || f.PreferResolution != "" {
			res = detectResolutionLower(lower)
		}
		if len(allowed) > 0 && res != "" && !allowed[res] {
			continue
		}
		if f.HDROnly && !reHDROnly.match(lower) {
			continue
		}
		// Only drop when the seeder count is actually known — many indexer entries omit it, and treating
		// missing as 0 would discard otherwise-good (even cached) results (mirrors the size filter, which
		// keeps unknown sizes).
		if f.MinSeeders != nil && s.Seeders != nil && *s.Seeders < *f.MinSeeders {
			continue
		}
		if f.MaxSizeGB != nil && intOr(s.SizeBytes, 0) > *f.MaxSizeGB*gib {
			continue
		}
		// "Show me only what plays now" is a filter on what we KNOW is not cached, not on what we failed
		// to find out. Cache checks batch, so one failed batch leaves a hundred releases unknown while the
		// rest of the list is perfectly well known — and dropping those removed up to a hundred playable
		// releases from the list with nothing anywhere saying so. A release nobody could check survives
		// and ranks below the confirmed-cached ones, which is the honest ordering.
		if f.CachedOnly && !s.Cached && s.CacheKnown {
			continue
		}
		score := qualityScoreLower(lower, s, junk)
		// A release whose resolution could not be read is NOT sunk, matching every hard filter here: the
		// resolution whitelist keeps untagged releases, minSeeders keeps unknown seeder counts, maxSizeGB
		// keeps unknown sizes. Punishing an unknown would be asserting something nobody measured.
		if f.PreferResolution != "" && res != "" && res != f.PreferResolution {
			score -= preferenceSink
		}
		out = append(out, scored{s, i, score, intOr(s.Seeders, 0)})
	}

	sort.SliceStable(out, func(a, b int) bool {
		if out[a].score != out[b].score {
			return out[a].score > out[b].score
		}
		if out[a].seeders != out[b].seeders {
			return out[a].seeders > out[b].seeders
		}
		return out[a].idx < out[b].idx
	})
	if len(out) > f.ResultCap {
		out = out[:f.ResultCap]
	}
	res := make([]RawStream, len(out))
	for i := range out {
		res[i] = out[i].s
	}
	return res
}

func intOr(p *int, d int) int {
	if p == nil {
		return d
	}
	return *p
}
