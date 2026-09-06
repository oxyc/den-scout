package scout

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	CacheBytes    int // audit #1: byte budget for the in-memory cache
	// Where the durable tier writes. Empty disables persistence. The container is redeployed on every
	// image push, and a probe costs a debrid resolve to rebuild, so surviving a restart is worth a volume.
	CacheDir    string
	CinemetaURL string // metadata source for the year mistag filter (default: public Cinemeta)
	// Sealed config-in-URL (docs/SEALED-CONFIG.md). ConfigKey = current X25519 private key (base64);
	// ConfigKeysPrev = comma-separated prior keys (rotation). Empty ConfigKey → sealed URLs disabled.
	ConfigKey      string
	ConfigKeysPrev string
	// Bearer token for /metrics. Empty = the route 404s. The counters say when this install is being
	// watched and which indexers it uses, so they are not served to anyone who asks.
	MetricsToken string
	// Let scout build comet's and mediafusion's per-install config segments from the debrid account it
	// already holds, instead of skipping those indexers for want of a pasted URL. **This sends the debrid
	// token to those hosts**, so it is off unless the operator turns it on. An explicit SCOUT_<name>_URL
	// still wins, for a token-free config.
	MintIndexerConfigs bool
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
		CacheDir:       strEnvOr(get("SCOUT_CACHE_DIR"), filepath.Join(os.TempDir(), "den-scout-cache")),
		CinemetaURL:    orDefault(get("SCOUT_CINEMETA_URL"), cinemetaBase),
		ConfigKey:      get("SCOUT_CONFIG_KEY"),
		ConfigKeysPrev: get("SCOUT_CONFIG_KEYS_PREV"),
		MetricsToken:   get("SCOUT_METRICS_TOKEN"),
		MintIndexerConfigs: strings.EqualFold(get("SCOUT_MINT_INDEXER_CONFIGS"), "true") ||
			get("SCOUT_MINT_INDEXER_CONFIGS") == "1",
	}
}

// RefuseRedirectReplay is the CheckRedirect policy every client touching a debrid must carry.
//
// A redirect must never REPLAY a request that carries a body. Every add is charged once through
// spendAdd and then sent once — unless the endpoint answers 307 or 308, where Go's default policy
// re-sends the same method AND body to the new location, up to ten hops. Measured against all three
// stores: one charged add became two real POSTs on a single hop and ten on a redirect loop, so the
// fifty-an-hour ceiling would have permitted five hundred real adds. (301/302/303 are safe — Go
// downgrades them to GET and drops the body.)
//
// It also moves a credential. Premiumize's apikey travels in the directdl FORM BODY, and Go strips the
// Authorization header across hosts but replays the body regardless, so a 308 to a foreign host hands
// that token to an unrelated server verbatim.
//
// Refused by METHOD rather than by disabling redirects, because the same client reads playback links,
// and following a CDN's redirects on a GET is the entire job there. GET and HEAD carry no body to
// replay and keep working; anything else stops at the 3xx and the caller sees the non-2xx it is.
//
// Reachability is the same class as the listing OOM in FOLLOWUP.md — the API hosts are constants, so it
// needs the debrid itself, a terminator in front of it, or the operator's own proxy to answer 307/308.
// The cost of being wrong is a multiplied add budget, so it is guarded rather than argued about.
func RefuseRedirectReplay(req *http.Request, _ []*http.Request) error {
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		return nil
	}
	return http.ErrUseLastResponse
}

// BuildDeps wires the core to a concrete HTTP client + cache.
func BuildDeps(settings Settings, client *http.Client, cache Cache) Deps {
	// Decided once, at startup, from the operator's environment — never from a request.
	EnableIndexerConfigMinting(settings.MintIndexerConfigs)
	return Deps{
		Cache:         cache,
		ProbeClient:   client,
		ScrapeTimeout: settings.ScrapeTimeout,
		ListTTL:       settings.ListTTL,
		PublicURL:     settings.PublicURL,
		MakeScrapers:  func(c *Config) []scraper { return makeScrapers(c, client, settings.IndexerURLs) },
		MakeStores:    func(c *Config) []Store { return buildStores(c, client, cache) },
		Meta:          cinemetaMeta(client, settings.CinemetaURL),
		SealKeyring:   buildKeyring(settings.ConfigKey, settings.ConfigKeysPrev),
		MetricsToken:  settings.MetricsToken,
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

// strEnvOr is the string counterpart of intEnv: an unset or blank variable takes the default.
func strEnvOr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
