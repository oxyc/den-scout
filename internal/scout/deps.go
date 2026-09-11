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
	// token to those hosts**, so it is off unless the operator turns it on. An explicit <NAME>_URL (e.g.
	// COMET_URL) still wins, for a token-free config.
	MintIndexerConfigs bool
	// One redacted line per response (reqlog.go). Off when LOG_REQUESTS is unset, empty or exactly "0";
	// any other value turns it on — the rule every den addon shares.
	LogRequests bool
	// Revocation (docs/SEALED-CONFIG.md, "Revoking installs"). RevokedInstalls holds the valid ids from
	// REVOKED_INSTALLS; ConfigEpoch (CONFIG_EPOCH) refuses every config minted under an older epoch.
	RevokedInstalls []string
	ConfigEpoch     int
	// REQUIRE_INSTALL_ID: refuse a full config that carries no install id — every link built before ids
	// existed. The one lever for retiring those: they can't be named, and raising CONFIG_EPOCH would also
	// refuse the links minted since, which carry the same epoch.
	RequireInstallID bool
	// REMUX_KEY: den-remux's service key. A request carrying it in X-Den-Remux-Key may list and play with a
	// scoped (availability-only) config, so the browser's scoped URL can drive den-remux without the browser
	// ever holding a stream-capable config. Empty = a scoped config never lists or plays.
	RemuxKey string
	// PLAY_TICKET_TTL_SECS: how long a /p/ ticket in a stream list stays good.
	PlayTicketTTL time.Duration
	// LEGACY_PLAY_UNTIL: when the /<config>/play route closes, once tickets are on. Zero = never.
	LegacyPlayUntil time.Time
}

// StartupSummary is the one line an operator reads to confirm what this process is running with. Nothing
// secret goes in it: an indexer URL override can carry an encrypted config segment, so overrides are named
// by indexer, never by address, and keys and tokens appear only as on or off.
func StartupSummary(s Settings, persistent bool) string {
	names := func(ids []Indexer) string {
		if len(ids) == 0 {
			return "none"
		}
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = string(id)
		}
		return strings.Join(out, ",")
	}
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	var overridden, disabled []Indexer
	for _, id := range allIndexers {
		if s.IndexerURLs[id] != "" {
			overridden = append(overridden, id)
		}
		if _, dead := disabledIndexers[id]; dead {
			disabled = append(disabled, id)
		}
	}
	// The fleet's one startup shape: `den-<addon> <version> listening on :<port> — <shared k=v> <own k=v>`.
	return "den-scout " + manifestVersion + " listening on :" + s.Port +
		" — metrics=" + onOff(s.MetricsToken != "") + " log_requests=" + onOff(s.LogRequests) +
		" sealed=" + onOff(s.ConfigKey != "") +
		" revoked=" + strconv.Itoa(len(s.RevokedInstalls)) + " epoch=" + strconv.Itoa(s.ConfigEpoch) +
		" require_iid=" + onOff(s.RequireInstallID) + " remux=" + onOff(s.RemuxKey != "") +
		" indexers=" + names(defaultIndexers) + " overrides=" + names(overridden) +
		" disabled=" + names(disabled) + " minting=" + onOff(s.MintIndexerConfigs) +
		" cache=" + s.CacheDir + " cache_persistent=" + onOff(persistent)
}

func SettingsFromEnv(get func(string) string) Settings {
	urls := map[Indexer]string{}
	for _, id := range allIndexers {
		if v := get(strings.ToUpper(string(id)) + "_URL"); v != "" {
			urls[id] = v
		}
	}
	return Settings{
		Port:           orDefault(get("PORT"), "8080"),
		ScrapeTimeout:  durEnv(get("SCRAPE_TIMEOUT_SECS"), time.Second, defaultTimeout),
		ListTTL:        durEnv(get("LIST_TTL_SECS"), time.Second, defaultListTTL),
		PublicURL:      get("PUBLIC_BASE_URL"),
		IndexerURLs:    urls,
		CacheBytes:     intEnv(get("MEMORY_CACHE_BYTES"), 48<<20), // 48 MiB
		CacheDir:       strEnvOr(get("CACHE_DIR"), filepath.Join(os.TempDir(), "den-scout-cache")),
		CinemetaURL:    orDefault(get("CINEMETA_URL"), cinemetaBase),
		ConfigKey:      get("CONFIG_KEY"),
		ConfigKeysPrev: get("CONFIG_KEYS_PREV"),
		MetricsToken:   get("METRICS_TOKEN"),
		MintIndexerConfigs: strings.EqualFold(get("MINT_INDEXER_CONFIGS"), "true") ||
			get("MINT_INDEXER_CONFIGS") == "1",
		LogRequests:      get("LOG_REQUESTS") != "" && get("LOG_REQUESTS") != "0",
		RevokedInstalls:  parseRevokedInstalls(get("REVOKED_INSTALLS")),
		ConfigEpoch:      parseConfigEpoch(get("CONFIG_EPOCH")),
		RequireInstallID: get("REQUIRE_INSTALL_ID") != "" && get("REQUIRE_INSTALL_ID") != "0",
		RemuxKey:         get("REMUX_KEY"),
		PlayTicketTTL:    durEnv(get("PLAY_TICKET_TTL_SECS"), time.Second, defaultPlayTicketTTL),
		// Only read with a key set: without one there are no tickets, and the legacy route is the only one.
		LegacyPlayUntil: parseLegacyPlayUntil(get("LEGACY_PLAY_UNTIL"), get("CONFIG_KEY") != ""),
	}
}

// parseRevokedInstalls reads REVOKED_INSTALLS, a comma-separated list of install ids. A malformed entry is
// skipped and said, like a malformed CONFIG_KEYS_PREV entry: it could never match a config, because
// validateConfig refuses a config carrying such an id.
func parseRevokedInstalls(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !validInstallID(s) {
			log.Printf("skipping a malformed REVOKED_INSTALLS entry (want a %d-character install id)", installIDLen)
			continue
		}
		out = append(out, s)
	}
	return out
}

// parseConfigEpoch reads CONFIG_EPOCH. A value that does not parse is said loudly and read as 0, which
// refuses nothing: refusing every install over a typo would take the whole addon down.
func parseConfigEpoch(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > maxConfigEpoch {
		log.Printf("CONFIG_EPOCH=%q is not a non-negative integer — ignoring it, so no config is refused by epoch", v)
		return 0
	}
	return n
}

// parseLegacyPlayUntil reads LEGACY_PLAY_UNTIL, as den-reel reads PLAY_SIGNING_GRACE_UNTIL: unset (or no
// tickets to move to) leaves the legacy route open, and a value that does not parse is said loudly and read
// as already passed. A typo then refuses the stragglers and says why, instead of leaving open a route that
// was meant to close.
func parseLegacyPlayUntil(v string, ticketsOn bool) time.Time {
	v = strings.TrimSpace(v)
	if v == "" || !ticketsOn {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		log.Printf("LEGACY_PLAY_UNTIL=%q is not an RFC 3339 timestamp such as 2026-09-17T12:00:00Z — "+
			"reading it as passed, so the legacy /play route is refused from now on", v)
		return time.Unix(0, 0)
	}
	return t
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
//
// The query string is the other channel. TorBox's requestdl carries `token=` and Premiumize's cache
// check `apikey=`, and Go never strips a query across hosts, so a GET redirect from a debrid API host to
// anywhere else would hand the credential over. Those redirects are refused by HOST: a playback link on
// a CDN still follows its redirects, which is the probe's whole job on the same client.
func RefuseRedirectReplay(req *http.Request, via []*http.Request) error {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return http.ErrUseLastResponse
	}
	if len(via) > 0 && credentialQueryHosts[via[0].URL.Hostname()] && req.URL.Hostname() != via[0].URL.Hostname() {
		return http.ErrUseLastResponse
	}
	return nil
}

// credentialQueryHosts are the debrid API hosts whose requests can carry the account credential in the
// query string (see RefuseRedirectReplay).
var credentialQueryHosts = map[string]bool{
	"api.torbox.app":      true,
	"www.premiumize.me":   true,
	"api.real-debrid.com": true,
}

// BuildDeps wires the core to a concrete HTTP client + cache.
func BuildDeps(settings Settings, client *http.Client, cache Cache) Deps {
	// Decided once, at startup, from the operator's environment — never from a request.
	EnableIndexerConfigMinting(settings.MintIndexerConfigs)
	revoked := make(map[string]bool, len(settings.RevokedInstalls))
	for _, iid := range settings.RevokedInstalls {
		revoked[iid] = true
	}
	// A client can first use a list's tickets up to 3×LIST_TTL_SECS after the list was built (see
	// defaultPlayTicketTTL), so a TTL at or under that hands out tickets that are dead on arrival.
	if settings.ConfigKey != "" && settings.PlayTicketTTL <= 3*settings.ListTTL {
		log.Printf("PLAY_TICKET_TTL_SECS (%s) is not above 3×LIST_TTL_SECS (%s) — a stream list's play URLs "+
			"can expire before the client uses them", settings.PlayTicketTTL, 3*settings.ListTTL)
	}
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
		LogRequests:   settings.LogRequests,

		RevokedInstalls:  revoked,
		ConfigEpoch:      settings.ConfigEpoch,
		RequireInstallID: settings.RequireInstallID,
		RemuxKey:         settings.RemuxKey,
		PlayTicketTTL:    settings.PlayTicketTTL,
		LegacyPlayUntil:  settings.LegacyPlayUntil,
	}
}

// buildKeyring parses the sealed-config keyring from env. A malformed key disables sealed URLs (legacy
// plaintext keeps working) and logs loudly rather than crashing the addon.
func buildKeyring(current, prev string) *sealKeyring {
	kr, err := parseSealKeyring(current, prev)
	if err != nil {
		log.Printf("CONFIG_KEY invalid — sealed configs disabled: %v", err)
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
