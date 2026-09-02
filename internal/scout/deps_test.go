package scout

import (
	"context"
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
	// comet needs a per-install config URL and this environment supplies none, so it is never requested —
	// asked bare it can only 403. But it stays in the list as a source that CANNOT answer, rather than
	// disappearing from it: dropping it would take it out of the quorum too, and "an empty result is
	// authoritative only when every indexer answered" would quietly become "…every indexer we still
	// bother asking", which is how one 200-empty becomes a confident "this release does not exist".
	sc := deps.MakeScrapers(cfg)
	if len(sc) != 2 || sc[0].id() != "torrentio" || sc[1].id() != "comet" {
		t.Fatalf("makeScrapers: %v", sc)
	}
	if _, err := sc[1].scrape(context.Background(), scrapeQuery{}); err == nil {
		t.Error("an unaskable indexer must fail, so it counts as a source that did not answer")
	}
	if _, ok := sc[1].(unaskableScraper); !ok {
		t.Errorf("comet should be unaskable without a config URL, got %T", sc[1])
	}
	// Given the URL, it is asked.
	withComet := BuildDeps(SettingsFromEnv(func(k string) string {
		if k == "SCOUT_COMET_URL" {
			return "https://comet.example/CONFIG"
		}
		return ""
	}), &http.Client{}, NewMemoryCache(1<<20))
	if sc := withComet.MakeScrapers(cfg); len(sc) != 2 || sc[1].id() != "comet" {
		t.Errorf("makeScrapers with comet URL: %v", sc)
	}
	if st := deps.MakeStores(cfg); len(st) != 1 || st[0].Service() != ServiceTorBox {
		t.Errorf("makeStores: %v", st)
	}
}
