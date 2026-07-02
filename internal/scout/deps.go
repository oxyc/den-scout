package scout

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Settings come from env so the same binary runs anywhere. No debrid secret lives here — the token
// rides in the per-install addon URL.
type Settings struct {
	Port          string
	ScrapeTimeout time.Duration
	ListTTL       time.Duration
	PublicURL     string // audit #8
	IndexerURLs   map[Indexer]string
	CacheBytes    int    // audit #1: byte budget for the in-memory cache
	CinemetaURL   string // metadata source for the year mistag filter (default: public Cinemeta)
	// Sealed config-in-URL (docs/SEALED-CONFIG.md). ConfigKey = current X25519 private key (base64);
	// ConfigKeysPrev = comma-separated prior keys (rotation). Empty ConfigKey → sealed URLs disabled.
	ConfigKey      string
	ConfigKeysPrev string
}

func SettingsFromEnv(get func(string) string) Settings {
	urls := map[Indexer]string{}
	for _, id := range allIndexers {
		if v := get("SCOUT_" + strings.ToUpper(string(id)) + "_URL"); v != "" {
			urls[id] = v
		}
	}
	return Settings{
		Port:           orDefault(get("PORT"), "8080"),
		ScrapeTimeout:  durEnv(get("SCOUT_SCRAPE_TIMEOUT_MS"), time.Millisecond, defaultTimeout),
		ListTTL:        durEnv(get("SCOUT_LIST_TTL_SECONDS"), time.Second, defaultListTTL),
		PublicURL:      get("SCOUT_PUBLIC_URL"),
		IndexerURLs:    urls,
		CacheBytes:     intEnv(get("SCOUT_CACHE_BYTES"), 48<<20), // 48 MiB
		CinemetaURL:    orDefault(get("SCOUT_CINEMETA_URL"), cinemetaBase),
		ConfigKey:      get("SCOUT_CONFIG_KEY"),
		ConfigKeysPrev: get("SCOUT_CONFIG_KEYS_PREV"),
	}
}

// BuildDeps wires the core to a concrete HTTP client + cache.
func BuildDeps(settings Settings, client *http.Client, cache Cache) Deps {
	return Deps{
		Cache:         cache,
		ScrapeTimeout: settings.ScrapeTimeout,
		ListTTL:       settings.ListTTL,
		PublicURL:     settings.PublicURL,
		MakeScrapers:  func(c *Config) []scraper { return makeScrapers(c, client, settings.IndexerURLs) },
		MakeStores:    func(c *Config) []Store { return buildStores(c, client, cache) },
		Meta:          cinemetaMeta(client, settings.CinemetaURL),
		SealKeyring:   buildKeyring(settings.ConfigKey, settings.ConfigKeysPrev),
	}
}

// buildKeyring parses the sealed-config keyring from env. A malformed key disables sealed URLs (legacy
// plaintext keeps working) and logs loudly rather than crashing the addon.
func buildKeyring(current, prev string) *sealKeyring {
	kr, err := parseSealKeyring(current, prev)
	if err != nil {
		log.Printf("den-scout: SCOUT_CONFIG_KEY invalid — sealed configs disabled: %v", err)
		return nil
	}
	return kr
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func durEnv(v string, unit, d time.Duration) time.Duration {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return d
	}
	return time.Duration(n) * unit
}

func intEnv(v string, d int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return d
	}
	return n
}
