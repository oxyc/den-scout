package scout

import (
	"net/http"
	"testing"
	"time"
)

func TestSettingsFromEnv(t *testing.T) {
	// defaults when unset
	s := SettingsFromEnv(func(string) string { return "" })
	if s.Port != "8080" || s.ScrapeTimeout != defaultTimeout || s.ListTTL != defaultListTTL || s.CacheBytes != 48<<20 || len(s.IndexerURLs) != 0 {
		t.Errorf("defaults: %+v", s)
	}

	env := map[string]string{
		"PORT":                    "9000",
		"SCOUT_SCRAPE_TIMEOUT_MS": "5000",
		"SCOUT_LIST_TTL_SECONDS":  "60",
		"SCOUT_PUBLIC_URL":        "https://scout.example",
		"SCOUT_MEDIAFUSION_URL":   "https://mf.self/CONFIG",
		"SCOUT_CACHE_BYTES":       "1048576",
	}
	s = SettingsFromEnv(func(k string) string { return env[k] })
	if s.Port != "9000" || s.ScrapeTimeout != 5*time.Second || s.ListTTL != 60*time.Second ||
		s.PublicURL != "https://scout.example" || s.IndexerURLs["mediafusion"] != "https://mf.self/CONFIG" || s.CacheBytes != 1<<20 {
		t.Errorf("from env: %+v", s)
	}

	// non-numeric / non-positive fall back to defaults
	s = SettingsFromEnv(func(k string) string {
		if k == "SCOUT_SCRAPE_TIMEOUT_MS" {
			return "-1"
		}
		if k == "SCOUT_LIST_TTL_SECONDS" {
			return "abc"
		}
		return ""
	})
	if s.ScrapeTimeout != defaultTimeout || s.ListTTL != defaultListTTL {
		t.Errorf("bad values should fall back: %+v", s)
	}
}

func TestBuildDeps(t *testing.T) {
	settings := SettingsFromEnv(func(string) string { return "" })
	deps := BuildDeps(settings, &http.Client{}, NewMemoryCache(1<<20))
	cfg := &Config{Indexers: []Indexer{"torrentio", "comet"}, Filters: Filters{ExcludeCam: true}, Debrid: []DebridAccount{{ServiceTorBox, "t"}}}
	if sc := deps.MakeScrapers(cfg); len(sc) != 2 || sc[0].id() != "torrentio" {
		t.Errorf("makeScrapers: %v", sc)
	}
	if st := deps.MakeStores(cfg); len(st) != 1 || st[0].Service() != ServiceTorBox {
		t.Errorf("makeStores: %v", st)
	}
}
