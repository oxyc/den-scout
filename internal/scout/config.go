package scout

import (
	"encoding/json"
	"math"
	"regexp"
)

// DebridService / Indexer enums (ported from src/config.ts).
type DebridService string

const (
	ServiceTorBox     DebridService = "torbox"
	ServiceRealDebrid DebridService = "realdebrid"
	ServicePremiumize DebridService = "premiumize"
)

// debridServices is the resolve/priority order (TorBox first — it has the real cache API).
var debridServices = []DebridService{ServiceTorBox, ServiceRealDebrid, ServicePremiumize}

type Indexer string

// Every indexer a config may name.
var allIndexers = []Indexer{"torrentio", "comet", "mediafusion", "torz"}

// What an install gets when it names none — decided HERE rather than per install, so a dead host is
// dropped for every client by one deploy instead of a reconfiguration each.
var defaultIndexers = []Indexer{"torrentio", "mediafusion"}

// Indexers whose stream endpoint lives behind a per-install config segment (`/{config}/stream/...`),
// obtainable only from that addon's own /configure page.
//
// Asked without one they do not fail usefully: comet answers 403, and mediafusion answers **200 with an
// empty stream list** — a dead indexer wearing a healthy status. That empty success counted as a real
// response, so it voted on whether a release exists, and an episode with fifty releases was reported as
// having none. An indexer that cannot be asked properly must not be asked at all.
var configPathIndexers = map[Indexer]string{
	"comet":       "SCOUT_COMET_URL",
	"mediafusion": "SCOUT_MEDIAFUSION_URL",
	"torz":        "SCOUT_TORZ_URL",
}

// Named but known-broken indexers, removed from any config. An install's sealed config cannot be edited
// from the server, so without this an old blob keeps paying for a host that has been gone for months —
// and worse, the survivors look like a quorum: when torrentio shed a burst, mediafusion's empty answer
// became the whole result, and an episode torrentio serves 50 releases for read as "no source found".
//
// Only a host that is GONE belongs here. Comet was listed as "403s every request", which was a
// misreading of a live host: it 403s the BARE path and answers on a config path, exactly like
// mediafusion. That is what `configPathIndexers` handles — asked without a config it becomes an
// unaskable scraper, which costs no request and counts correctly in the quorum. Disabling it instead
// stripped it in validateConfig, before makeScrapers ever saw it, so `mintCometURL` — which needs no
// round trip at all, being local base64 — was dead code, and the install was left with one real source
// while every resilience rule here assumed several.
var disabledIndexers = map[Indexer]string{
	"torz": "host no longer resolves (no DNS)",
}
var validResolutions = map[string]bool{"2160p": true, "1080p": true, "720p": true, "480p": true}

const (
	// Ceiling on the base64 config path segment. See decodeConfig.
	maxConfigBlob = 8 << 10
	// Ceiling on accounts in one config. There are three services; more than a handful of accounts is
	// not a configuration, it is a way to make one request retain a large slice for a whole scrape.
	maxDebridAccounts = 8
	// Ceiling on results in one response, down from 200.
	//
	// This is the OTHER half of what one request can retain, and the larger half. Every stream in the
	// reply embeds a full copy of the config segment in its /play URL, so the response body is
	// len(blob) x resultCap — measured at 1,602,743 bytes for an 8 KiB blob and a 200 cap, 214 times the
	// blob itself. Capping the blob alone left 150 concurrent requests, every value inside its own
	// limit, pinning 311 MiB against a 230 MiB GOMEMLIMIT.
	//
	// The blob ceiling cannot come down to fix it: the largest config the field caps admit measures 6,563
	// bytes sealed, i.e. 80% of the 8 KiB already. So the multiplier is what gives. Fifty is well past
	// what anybody scrolls — the default is twenty, and the ranking exists precisely so the answer is
	// near the top — and it cuts the worst body fourfold.
	maxResultCap = 50
)

// countedRepeatAt matches a `{n}` / `{n,}` / `{n,m}` quantifier at the start of what it is given.
var countedRepeatAt = regexp.MustCompile(`^\{\d+(,\d*)?\}`)

// hasCountedRepeat reports whether a user-supplied pattern contains a counted repetition — the one regex
// construct whose COMPILED size is not bounded by the pattern's length.
//
// Scanned with real escape state rather than the one-character lookbehind this first used. `[^\\]\{`
// asks "is the character before the brace a backslash", which is not the same question: in `a\\{1000}`
// the `\\` is an ESCAPED BACKSLASH and the `{1000}` after it is a live quantifier, so the lookbehind saw
// a backslash, called the brace escaped, and let it through. The residual was small — one escaped
// character repeated, ~0.1 MiB against the 53 MiB the guard was written for — but a guard that answers a
// different question than its comment claims is worth exactly nothing the next time someone edits it.
//
// WHAT THIS ALSO DROPS, stated in full because the previous version of this comment claimed a narrower
// reach than the code has, which is the exact fault it was written to complain about:
//
//   - Any counted repetition, however small — `[0-9]{2}`, `\b\d{4}\b`, `S\d{2}E\d{2}`. A four-digit year
//     is an ordinary thing to filter on, and `\d\d\d\d` is the workaround. Dropped rather than
//     count-capped on purpose: a cap has to reason about NESTED counts, which multiply, and that is a
//     lot of machinery to keep a brace.
//   - `\x{...}` hex escapes whose payload happens to be all digits (`\x{1000}`), which expand nothing.
//     `\x{1F600}` survives, since it has a hex letter in it. Obscure in a release-name filter.
//   - A brace inside a character class (`[a{2}]`), where it is a literal.
//
// All of that is over-rejection, which is the safe direction: the filter goes away, the list does not.
func hasCountedRepeat(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // whatever follows is escaped, including a backslash or a brace
		case '{':
			if countedRepeatAt.MatchString(s[i:]) {
				return true
			}
		}
	}
	return false
}

type DebridAccount struct {
	Service DebridService
	Token   string
}

type Filters struct {
	ExcludeCam  bool
	Resolutions []string // empty → all
	// PreferResolution sinks every OTHER resolution below this one instead of dropping it — "1080p
	// please, but show me the 4K rather than nothing". Every other knob here is a hard drop, which is
	// the wrong shape for a taste: a viewer who filters to 1080p to save bandwidth would rather see a
	// 4K remux than an empty list when that is all anybody has. "" → no preference.
	PreferResolution string
	HDROnly          bool
	MinSeeders       *int // nil → no filter
	MaxSizeGB        *int // nil → no filter
	ExcludeRegex     string
}

type Config struct {
	Debrid     []DebridAccount
	Indexers   []Indexer
	Filters    Filters
	CachedOnly bool
	ResultCap  int
}

// rawConfig mirrors the untrusted wire JSON before validation/clamping.
type rawConfig struct {
	Debrid []struct {
		Service string `json:"service"`
		Token   string `json:"token"`
	} `json:"debrid"`
	Indexers []string `json:"indexers"`
	Filters  *struct {
		ExcludeCam       *bool    `json:"excludeCam"`
		Resolutions      []string `json:"resolutions"`
		PreferResolution *string  `json:"preferResolution"`
		HDROnly          *bool    `json:"hdrOnly"`
		MinSeeders       *float64 `json:"minSeeders"`
		MaxSizeGB        *float64 `json:"maxSizeGB"`
		ExcludeRegex     *string  `json:"excludeRegex"`
	} `json:"filters"`
	CachedOnly *bool    `json:"cachedOnly"`
	ResultCap  *float64 `json:"resultCap"`
}

// decodeConfig decodes the config path segment into a validated config, or ok=false (→ 400). The segment
// is base64url; the decoded bytes are either a SEALED blob (first byte == sealedVersion → decrypt with the
// keyring) or a legacy plaintext JSON config (first byte '{'). Sealed with no keyring, or a decrypt
// failure, fails CLOSED — never falls through to an empty/partial config. See docs/SEALED-CONFIG.md.
func decodeConfig(kr *sealKeyring, blob string) (*Config, bool) {
	// Refuse an absurd segment before decoding it. Nothing bounded this, and net/http accepts a request
	// line up to its 1 MiB default — measured: a real server hands the handler a 1,054,721-character
	// path, and 716,770 characters of it decode to a VALID config with 16,289 debrid accounts. The build
	// then retains the blob and the parsed config for the length of a scrape, so ~1.4 MiB of live heap per
	// in-flight request, with distinct blobs defeating both the cache and the singleflight: 150 concurrent
	// misses measured 207 MiB inside a 230 MiB GOMEMLIMIT.
	//
	// 8 KiB, and the margin is thinner than it looks: the largest config the FIELD caps admit —
	// maxDebridAccounts accounts each with a 512-character token, all four indexers, every filter, a
	// 256-character regex — measures 6,498 bytes plain and 6,563 sealed, so 80% of this is legitimately
	// reachable. Those two numbers are computed by the test rather than trusted here. It cannot come down without lowering those caps too, which is why the response-side
	// multiplier is bounded by maxResultCap instead. A config from /configure is ~1 KB.
	//
	// This bounds the segment, NOT what a request retains: the reply embeds a copy of the segment per
	// stream, so see maxResultCap for the other half.
	if len(blob) > maxConfigBlob {
		return nil, false
	}
	data, err := b64urlDecode(blob)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	if data[0] == sealedVersion {
		if kr == nil {
			return nil, false // sealed URL but no key configured → can't open; refuse
		}
		pt, err := kr.open(data[1:])
		if err != nil {
			return nil, false
		}
		data = pt
	}
	var raw rawConfig
	if json.Unmarshal(data, &raw) != nil {
		return nil, false
	}
	return validateConfig(&raw)
}

// validateConfig strict-whitelists + clamps an untrusted config (mirrors src/config.ts).
func validateConfig(raw *rawConfig) (*Config, bool) {
	var debrid []DebridAccount
	for _, d := range raw.Debrid {
		if !isDebridService(d.Service) || d.Token == "" || len(d.Token) > 512 {
			continue
		}
		debrid = append(debrid, DebridAccount{Service: DebridService(d.Service), Token: d.Token})
		// Stop at the ceiling rather than keeping every account named. With the blob capped this can no
		// longer be large, but it is the slice a whole build carries, and a bound here is one line.
		if len(debrid) == maxDebridAccounts {
			break
		}
	}
	if len(debrid) == 0 {
		return nil, false
	}

	var idx []Indexer
	for _, i := range raw.Indexers {
		if isIndexer(i) {
			idx = append(idx, Indexer(i))
		}
	}
	idx = dedupeIndexers(idx) // audit #10
	var kept []Indexer
	for _, i := range idx {
		if _, dead := disabledIndexers[i]; !dead {
			kept = append(kept, i)
		}
	}
	idx = kept
	if len(idx) == 0 {
		idx = append([]Indexer(nil), defaultIndexers...)
	}

	f := Filters{ExcludeCam: true} // default on
	if raw.Filters != nil {
		if raw.Filters.ExcludeCam != nil {
			f.ExcludeCam = *raw.Filters.ExcludeCam
		}
		// Deduped as well as whitelisted. There are four valid values, so a repeated one carries no
		// information, and a config naming "1080p" ten thousand times retained ten thousand strings for
		// the length of the build. (Not a per-release scan: rankStreams turns this into a set once per
		// call, so the cost was ten thousand map inserts per build plus the retention — the retention is
		// the reason.)
		for _, r := range raw.Filters.Resolutions {
			if validResolutions[r] && !containsString(f.Resolutions, r) {
				f.Resolutions = append(f.Resolutions, r)
			}
		}
		// Whitelisted like every other resolution field; an unknown value is dropped, not carried, so it
		// becomes "no preference" rather than a preference nothing can ever satisfy.
		if raw.Filters.PreferResolution != nil && validResolutions[*raw.Filters.PreferResolution] {
			f.PreferResolution = *raw.Filters.PreferResolution
		}
		if raw.Filters.HDROnly != nil {
			f.HDROnly = *raw.Filters.HDROnly
		}
		// audit #12: minSeeders/maxSizeGB <= 0 is a no-op filter, so treat it as unset (nil).
		f.MinSeeders = clampPosInt(raw.Filters.MinSeeders, 100000)
		f.MaxSizeGB = clampPosInt(raw.Filters.MaxSizeGB, 1000)
		if raw.Filters.ExcludeRegex != nil {
			s := *raw.Filters.ExcludeRegex
			if len(s) > 256 {
				s = s[:256]
			}
			// Counted repetition is dropped, and the length cap is not enough on its own.
			//
			// RE2 is linear at MATCH time, which is what makes this filter safe to run over a stream list
			// (audit #9). Nothing bounded it at COMPILE time: `{n}` is expanded into the program, so 250
			// characters of `(?:x{240}){1000}` — inside the cap, from an unauthenticated caller, because a
			// config's debrid token is never verified — compiles to 53 MiB of live heap. Measured against
			// 0.03 MiB for the same 250 characters without braces, inside a 230 MiB GOMEMLIMIT; sixteen
			// concurrent requests took the container past its 256 MB limit.
			//
			// Dropping the whole quantifier rather than capping the count: this filter exists to exclude
			// words a viewer does not want in a release name ("hindi|dubbed|hc"), and none of that needs
			// counted repetition. Without `{}` the compiled program is bounded by the pattern length.
			// Dropped, not rejected, to match the existing tolerance for a malformed pattern.
			if hasCountedRepeat(s) {
				s = ""
			}
			f.ExcludeRegex = s
		}
	}

	// Default OFF. A config that omits this is deferring to the server, and hiding everything the debrid
	// hasn't already fetched is too strong a default to inherit silently: it can leave a title with one
	// scrap of a release, or nothing at all, while several good ones sit a download away. An explicit
	// `cachedOnly: true` still pins it for anyone who wants only instant playback.
	cachedOnly := false
	if raw.CachedOnly != nil {
		cachedOnly = *raw.CachedOnly
	}
	resultCap := 20
	if raw.ResultCap != nil && isFinite(*raw.ResultCap) {
		resultCap = clampInt(int(math.Round(*raw.ResultCap)), 1, maxResultCap)
	}

	return &Config{Debrid: debrid, Indexers: idx, Filters: f, CachedOnly: cachedOnly, ResultCap: resultCap}, true
}

func isDebridService(s string) bool {
	for _, d := range debridServices {
		if string(d) == s {
			return true
		}
	}
	return false
}

func isIndexer(s string) bool {
	for _, i := range allIndexers {
		if string(i) == s {
			return true
		}
	}
	return false
}

func dedupeIndexers(in []Indexer) []Indexer {
	seen := make(map[Indexer]bool, len(in))
	var out []Indexer
	for _, i := range in {
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	return out
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampPosInt rounds v, returns nil if absent or <= 0 (no-op filter, audit #12), else clamps to [1,max].
func clampPosInt(v *float64, max int) *int {
	if v == nil || !isFinite(*v) {
		return nil
	}
	n := int(math.Round(*v))
	if n <= 0 {
		return nil
	}
	if n > max {
		n = max
	}
	return &n
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// containsString is a linear scan, which is right here: the slice it guards holds at most the four
// valid resolutions, so a map would cost an allocation to save nothing.
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
