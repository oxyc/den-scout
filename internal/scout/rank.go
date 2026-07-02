package scout

import (
	"math"
	"regexp"
	"sort"
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
	Source    string
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

	switch {
	case reAtmos.match(t):
		score += 26
	case reDTSX.match(t):
		score += 24
	case reTrueHD.match(t):
		score += 20
	case reDTSHDMA.match(t):
		score += 16
	case reDTSHD.match(t):
		score += 12
	case reEAC3.match(t):
		score += 8
	case reDTS.match(t):
		score += 6
	case reAC3.match(t):
		score += 4
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

type rankFilters struct {
	ExcludeCam   bool
	Resolutions  []string
	HDROnly      bool
	MinSeeders   *int
	MaxSizeGB    *int
	ExcludeRegex string
	CachedOnly   bool
	ResultCap    int
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
		if len(allowed) > 0 {
			if res := detectResolutionLower(lower); res != "" && !allowed[res] {
				continue
			}
		}
		if f.HDROnly && !reHDROnly.match(lower) {
			continue
		}
		if f.MinSeeders != nil && intOr(s.Seeders, 0) < *f.MinSeeders {
			continue
		}
		if f.MaxSizeGB != nil && intOr(s.SizeBytes, 0) > *f.MaxSizeGB*gib {
			continue
		}
		if f.CachedOnly && !s.Cached {
			continue
		}
		out = append(out, scored{s, i, qualityScoreLower(lower, s, junk), intOr(s.Seeders, 0)})
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
