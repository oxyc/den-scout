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
		"PORT":                 "9000",
		"SCRAPE_TIMEOUT_MS":    "5000",
		"LIST_TTL_SECS":        "60",
		"PUBLIC_BASE_URL":      "https://scout.example",
		"MEDIAFUSION_URL":      "https://mf.self/CONFIG",
		"MEMORY_CACHE_BYTES":   "1048576",
		"CACHE_DIR":            "/cache",
		"CINEMETA_URL":         "https://cinemeta.self",
		"CONFIG_KEY":           "current-key",
		"CONFIG_KEYS_PREV":     "old-a,old-b",
		"METRICS_TOKEN":        "metrics-secret",
		"MINT_INDEXER_CONFIGS": "1",
	}
	s = SettingsFromEnv(func(k string) string { return env[k] })
	if s.Port != "9000" || s.ScrapeTimeout != 5*time.Second || s.ListTTL != 60*time.Second ||
		s.PublicURL != "https://scout.example" || s.IndexerURLs["mediafusion"] != "https://mf.self/CONFIG" || s.CacheBytes != 1<<20 {
		t.Errorf("from env: %+v", s)
	}
	if s.CacheDir != "/cache" || s.ConfigKey != "current-key" || s.ConfigKeysPrev != "old-a,old-b" ||
		s.MetricsToken != "metrics-secret" || s.CinemetaURL != "https://cinemeta.self" || !s.MintIndexerConfigs {
		t.Errorf("unprefixed names from env: %+v", s)
	}

	// No variable carries the addon's name — the container is the namespace — and the SCOUT_-prefixed
	// spellings they replaced are not read at all: a stale env file must show up as an unset key, not keep
	// working by accident.
	old := map[string]string{
		"SCOUT_PUBLIC_URL":           "https://old.example",
		"SCOUT_CACHE_DIR":            "/old",
		"SCOUT_CONFIG_KEY":           "old",
		"SCOUT_CONFIG_KEYS_PREV":     "old",
		"SCOUT_METRICS_TOKEN":        "old",
		"SCOUT_SCRAPE_TIMEOUT_MS":    "5000",
		"SCOUT_LIST_TTL_SECONDS":     "60",
		"SCOUT_CACHE_BYTES":          "1048576",
		"SCOUT_CINEMETA_URL":         "https://old.example",
		"SCOUT_MINT_INDEXER_CONFIGS": "true",
		"SCOUT_TORRENTIO_URL":        "https://old.example",
		"SCOUT_COMET_URL":            "https://old.example",
		"SCOUT_MEDIAFUSION_URL":      "https://old.example",
		"SCOUT_TORZ_URL":             "https://old.example",
	}
	s = SettingsFromEnv(func(k string) string { return old[k] })
	if s.PublicURL != "" || s.CacheDir == "/old" || s.ConfigKey != "" || s.ConfigKeysPrev != "" || s.MetricsToken != "" ||
		s.ScrapeTimeout != defaultTimeout || s.ListTTL != defaultListTTL || s.CacheBytes != 48<<20 ||
		s.CinemetaURL != cinemetaBase || s.MintIndexerConfigs || len(s.IndexerURLs) != 0 {
		t.Errorf("a retired SCOUT_ name was still read: %+v", s)
	}

	// non-numeric / non-positive fall back to defaults
	s = SettingsFromEnv(func(k string) string {
		if k == "SCRAPE_TIMEOUT_MS" {
			return "-1"
		}
		if k == "LIST_TTL_SECS" {
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
	un, ok := sc[1].(unaskableScraper)
	if !ok {
		t.Errorf("comet should be unaskable without a config URL, got %T", sc[1])
	}
	// NOT transient. The distinction decides whether the operator is told to configure something or
	// told an upstream is down, and it is read one level up to decide whether an empty list is an
	// outage. Inverting it on this, the shipped default, reported every genuinely empty title as an
	// indexer outage: X-Scout-Degraded on every response, no-store so negative caching never runs, and
	// three in a row flipping /health to degraded on a service that is perfectly healthy.
	//
	// Only the CONSUMPTION of this flag was covered, never its production, so the inversion was green.
	if un.transient {
		t.Error("an indexer with no config URL and no minting is a misconfiguration, not an outage — " +
			"marking it transient suppresses the operator's log line and reports healthy empty lists " +
			"as a degraded scrape")
	}
	// Given the URL, it is asked.
	withComet := BuildDeps(SettingsFromEnv(func(k string) string {
		if k == "COMET_URL" {
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
