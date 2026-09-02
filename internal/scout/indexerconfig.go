package scout

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Minting the per-install config segment that comet and mediafusion require.
//
// Both serve streams only from `/{config}/stream/...`, where the config is produced by their own
// /configure page. Asked bare they do not fail usefully — comet answers 403 and mediafusion answers 200
// with an empty list, a dead indexer wearing a healthy status — so scout skips an indexer it cannot
// address, and the install was left querying exactly ONE source. Every resilience rule in this package
// (the answered-quorum, the shed-request retry, dropping dead indexers) then reduces to "torrentio said
// so".
//
// Scout is already given the debrid account on every request, so it can build those configs itself
// rather than asking the operator to paste two URLs. **This sends the debrid token to the indexer**,
// which is a real change in who holds it: it goes from "our homelab and the debrid" to "and the two
// indexer hosts too". That is why it is opt-in via SCOUT_MINT_INDEXER_CONFIGS, and why an explicit
// SCOUT_COMET_URL / SCOUT_MEDIAFUSION_URL always wins — an operator who has pasted a token-free config
// keeps it.
//
// The indexers are asked to SEARCH, not to resolve: scout takes infohashes from them and does its own
// debrid work, so a minted config should never cause an add against the account. That is an assumption
// about their behaviour, not something this package can enforce, and it is why the add-quota guards in
// probe_fanout.go and stores.go matter independently of this file.

// mintedTTL — how long a minted config is reused. Long enough that minting is rare, short enough that a
// rotated token stops being replayed to a third party within the day.
const mintedTTL = 12 * time.Hour

// Opt-in, because it decides who holds the debrid token. Set by SCOUT_MINT_INDEXER_CONFIGS at startup.
var mintIndexerConfigs bool

// EnableIndexerConfigMinting is called once from BuildDeps. A package-level switch rather than a field
// on Config, because it is an operator decision about credential handling — not an install's preference,
// and not something a request should be able to turn on.
func EnableIndexerConfigMinting(on bool) { mintIndexerConfigs = on }

type mintedConfig struct {
	url string
	at  time.Time
}

// How long a failure to mint is remembered. Short, because a host that is down now may be up in a
// minute and an indexer skipped is real coverage lost — but not zero, which is what made an unhealthy
// mediafusion get one POST per stream request.
const mintFailureTTL = 2 * time.Minute

func (m mintedConfig) ttl() time.Duration {
	if m.url == "" {
		return mintFailureTTL
	}
	return mintedTTL
}

var (
	mintedMu sync.Mutex
	minted   = map[string]mintedConfig{}
)

// indexerBaseWithConfig returns a ready-to-use base URL for an indexer that needs a config segment, or
// "" when one cannot be built. Cached per (indexer, account) so a burst of episode requests mints once.
func indexerBaseWithConfig(ctx context.Context, id Indexer, config *Config, client doer) string {
	acct, ok := primaryDebrid(config)
	if !ok {
		return ""
	}
	key := string(id) + ":" + keyHash(string(acct.Service)+":"+acct.Token)

	mintedMu.Lock()
	if m, hit := minted[key]; hit && time.Since(m.at) < m.ttl() {
		mintedMu.Unlock()
		return m.url
	}
	mintedMu.Unlock()

	var url string
	switch id {
	case "comet":
		url = mintCometURL(acct)
	case "mediafusion":
		url = mintMediaFusionURL(ctx, acct, client)
	}
	if url == "" {
		// Remember the FAILURE too, briefly. Only successes were cached, so while the host was unhealthy
		// every single stream request re-POSTed to it — with its own 10 s timeout, ahead of the scrape —
		// meaning the worse it was, the harder scout hit it. The opposite of what a limiter is for.
		mintedMu.Lock()
		minted[key] = mintedConfig{url: "", at: time.Now()}
		mintedMu.Unlock()
		return ""
	}
	mintedMu.Lock()
	minted[key] = mintedConfig{url: url, at: time.Now()}
	mintedMu.Unlock()
	log.Printf("scout: minted a config for the %s indexer from the %s account", id, acct.Service)
	return url
}

// primaryDebrid picks the account a minted config should speak for. First configured wins — the same
// order ResolvePreferring uses when nothing is known about who holds a release.
func primaryDebrid(config *Config) (DebridAccount, bool) {
	if config == nil || len(config.Debrid) == 0 {
		return DebridAccount{}, false
	}
	return config.Debrid[0], true
}

// Comet's config is base64 of a plain JSON object; no round trip to the service is needed.
func mintCometURL(acct DebridAccount) string {
	service := string(acct.Service)
	if acct.Service == ServiceRealDebrid {
		service = "realdebrid"
	}
	body := map[string]any{
		"maxResultsPerResolution":   0,
		"maxSize":                   0,
		"cachedOnly":                false,
		"removeTrash":               false,
		"resultFormat":              []string{"all"},
		"debridService":             service,
		"debridApiKey":              acct.Token,
		"debridStreamProxyPassword": "",
		"languages":                 map[string]any{"required": []string{}, "exclude": []string{}, "preferred": []string{}},
		"resolutions":               map[string]any{},
		"options":                   map[string]any{},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	return defaultIndexerURLs["comet"] + "/" + base64.StdEncoding.EncodeToString(raw)
}

// MediaFusion encrypts its config server-side: POST the settings, get an opaque string back.
func mintMediaFusionURL(ctx context.Context, acct DebridAccount, client doer) string {
	if client == nil {
		return ""
	}
	body := map[string]any{
		"streaming_provider": map[string]any{
			"service": string(acct.Service),
			"token":   acct.Token,
		},
		"selected_resolutions":     []any{"4k", "2160p", "1440p", "1080p", "720p", "480p", nil},
		"enable_catalogs":          false,
		"max_size":                 "inf",
		"torrent_sorting_priority": []any{},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		defaultIndexerURLs["mediafusion"]+"/encrypt-user-data", bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", scrapeUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("scout: could not mint a mediafusion config: %v", err)
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Encrypted string `json:"encrypted_str"`
		Status    string `json:"status"`
		Message   string `json:"message"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out) != nil {
		return ""
	}
	// Its own words when it refuses — "invalid user data: Invalid max_size" is the difference between a
	// bad payload and an unreachable host, and guessing between them wastes an evening.
	if out.Status != "success" || out.Encrypted == "" {
		log.Printf("scout: mediafusion refused the config: %s %s",
			out.Status, strings.TrimSpace(out.Message))
		return ""
	}
	return fmt.Sprintf("%s/%s", defaultIndexerURLs["mediafusion"], out.Encrypted)
}
