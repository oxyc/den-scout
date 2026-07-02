package scout

import (
	"encoding/json"
	"math"
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

var allIndexers = []Indexer{"torrentio", "comet", "mediafusion", "torz"}
var validResolutions = map[string]bool{"2160p": true, "1080p": true, "720p": true, "480p": true}

type DebridAccount struct {
	Service DebridService
	Token   string
}

type Filters struct {
	ExcludeCam   bool
	Resolutions  []string // empty → all
	HDROnly      bool
	MinSeeders   *int // nil → no filter
	MaxSizeGB    *int // nil → no filter
	ExcludeRegex string
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
		ExcludeCam   *bool    `json:"excludeCam"`
		Resolutions  []string `json:"resolutions"`
		HDROnly      *bool    `json:"hdrOnly"`
		MinSeeders   *float64 `json:"minSeeders"`
		MaxSizeGB    *float64 `json:"maxSizeGB"`
		ExcludeRegex *string  `json:"excludeRegex"`
	} `json:"filters"`
	CachedOnly *bool    `json:"cachedOnly"`
	ResultCap  *float64 `json:"resultCap"`
}

// decodeConfig decodes the base64url blob into a validated config, or ok=false (→ 400).
func decodeConfig(blob string) (*Config, bool) {
	data, err := b64urlDecode(blob)
	if err != nil {
		return nil, false
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
	if len(idx) == 0 {
		idx = append([]Indexer(nil), allIndexers...)
	}

	f := Filters{ExcludeCam: true} // default on
	if raw.Filters != nil {
		if raw.Filters.ExcludeCam != nil {
			f.ExcludeCam = *raw.Filters.ExcludeCam
		}
		for _, r := range raw.Filters.Resolutions {
			if validResolutions[r] {
				f.Resolutions = append(f.Resolutions, r)
			}
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
			f.ExcludeRegex = s
		}
	}

	cachedOnly := true
	if raw.CachedOnly != nil {
		cachedOnly = *raw.CachedOnly
	}
	resultCap := 20
	if raw.ResultCap != nil {
		resultCap = clampInt(int(math.Round(*raw.ResultCap)), 1, 200)
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
