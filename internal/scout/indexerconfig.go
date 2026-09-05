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

	"golang.org/x/sync/singleflight"
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
	// When it was minted — the clock the TTL runs on, and deliberately NOT refreshed by use: a minted
	// config should be re-minted every mintedTTL however busy it is.
	at time.Time
	// When it was last handed out — the clock EVICTION runs on, which is a different question.
	//
	// Evicting by `at` was first-in-first-out by creation, and that is exactly backwards under the attack
	// the ceiling exists for: a legitimate install's entries are the oldest things in the map by
	// construction (minted once, good for twelve hours), so a flood of caller-invented tokens evicted the
	// operator's own entries and kept the attacker's. Measured: four legitimate entries aged an hour, then
	// 300 attacker mints, and none of the four survived. Every later stream request then re-mints — for
	// mediafusion that is a POST with a 3s timeout sitting in front of the scrape, so the ceiling turned a
	// memory bomb into a latency one.
	used time.Time
	// transient marks a failure a retry could fix — the indexer was unreachable, timed out, or refused
	// the payload — as opposed to one no retry will: there is no debrid account to mint from. Only the
	// latter is a misconfiguration, and the difference decides whether the indexer is excluded from the
	// empty-result quorum or counts as one that did not answer. Collapsing them let a single 10s timeout
	// on a loaded homelab promote an empty torrentio result to authoritative for two minutes.
	transient bool
}

// How long a failure to mint is remembered. Short, because a host that is down now may be up in a
// minute and an indexer skipped is real coverage lost — but not zero, which is what made an unhealthy
// mediafusion get one POST per stream request.
const mintFailureTTL = 2 * time.Minute

// How long a mint may take. It runs BEFORE the scrape and inside the singleflight leader, so its ceiling
// has to leave the scrape budget intact rather than consume it.
const mintTimeout = 3 * time.Second

func (m mintedConfig) ttl() time.Duration {
	if m.url == "" {
		return mintFailureTTL
	}
	return mintedTTL
}

var (
	mintedMu sync.Mutex
	minted   = map[string]mintedConfig{}
	// Rate-limits the eviction log. Guarded by mintedMu, like everything else here.
	lastMintEvictionLog time.Time
	// One round trip per (indexer, account) in flight at a time — the cache alone only collapses
	// SEQUENTIAL repeats, and a burst of episode requests is anything but sequential.
	mintFlight singleflight.Group
)

// indexerBaseWithConfig returns a ready-to-use base URL for an indexer that needs a config segment, or
// "" when one cannot be built. Cached per (indexer, account) so a burst of episode requests mints once.
// The second result reports that a FAILURE was transient — see mintedConfig.transient. It is meaningless
// when a URL was returned.
func indexerBaseWithConfig(ctx context.Context, id Indexer, config *Config, client doer) (string, bool) {
	acct, ok := primaryDebrid(config)
	if !ok {
		return "", false // nothing to mint from, and no retry changes that
	}
	key := string(id) + ":" + keyHash(string(acct.Service)+":"+acct.Token)

	mintedMu.Lock()
	if m, hit := minted[key]; hit && time.Since(m.at) < m.ttl() {
		// Touched on read, which is what makes eviction least-recently-USED rather than oldest-minted.
		// The TTL still runs on `at`, so this cannot keep a stale config alive.
		m.used = time.Now()
		minted[key] = m
		mintedMu.Unlock()
		return m.url, m.transient
	}
	mintedMu.Unlock()

	// Coalesced: the lock above is released before the round trip, so a burst of concurrent list builds
	// each found the cache cold and each POSTed. Twelve at once meant twelve mints of the identical
	// config, all of them ahead of the scrape on the user-visible path.
	type mintResult struct {
		url       string
		transient bool
	}
	out, _, _ := mintFlight.Do(key, func() (any, error) {
		var url string
		// A mint that has to reach the indexer can fail for reasons a retry fixes; one computed locally
		// cannot, so only the former is transient.
		transient := false
		switch id {
		case "comet":
			url = mintCometURL(acct)
		case "mediafusion":
			url = mintMediaFusionURL(ctx, acct, client)
			transient = url == ""
		}
		return mintResult{url, transient}, nil
	})
	res, _ := out.(mintResult)
	url, transient := res.url, res.transient
	if url == "" {
		// Remember the FAILURE too, briefly. Only successes were cached, so while the host was unhealthy
		// every single stream request re-POSTed to it — with its own 10 s timeout, ahead of the scrape —
		// meaning the worse it was, the harder scout hit it. The opposite of what a limiter is for.
		mintedMu.Lock()
		pruneMintedLocked()
		now := time.Now()
		minted[key] = mintedConfig{url: "", at: now, used: now, transient: transient}
		mintedMu.Unlock()
		return "", transient
	}
	mintedMu.Lock()
	pruneMintedLocked()
	mintedNow := time.Now()
	minted[key] = mintedConfig{url: url, at: mintedNow, used: mintedNow}
	mintedMu.Unlock()
	log.Printf("scout: minted a config for the %s indexer from the %s account", id, acct.Service)
	return url, false
}

// maxMintedEntries caps the cache by COUNT as well as by age.
//
// TTL pruning alone cannot bound this map, and that is the whole problem with it: the key is derived
// from a token the config supplies and nobody verifies, comet's mint is local base64 with no round trip
// at all, so every distinct token a caller invents mints successfully and is then held for the full
// twelve hours. At roughly 500 bytes an entry, an unbounded map inside a 230 MiB heap is a memory bomb
// with a twelve-hour fuse — and TTL pruning makes it look guarded.
//
// A legitimate install has one entry per (indexer, account): four indexers and a handful of accounts,
// so under twenty. 256 is an order of magnitude above that. Measured at the ceiling with the largest
// token validateConfig admits (512 chars, which the minted comet URL embeds): ~1,193 bytes an entry,
// ~298 KB in total — trivial against a 230 MiB heap, and the point is the bound rather than the size.
const maxMintedEntries = 256

// pruneMintedLocked drops entries past their own TTL, then evicts until there is room for the one the
// caller (holding mintedMu) is about to insert. Expiry was only ever consulted on READ, so an entry
// nobody asked for again stayed resident forever.
//
// It used to return "is there room?" for the caller to gate its insert on, which was load-bearing only
// while recently-used entries were protected and the eviction pass could therefore find nothing to drop.
// That protection is gone (see below); plain LRU always makes room, the answer was always yes, and a
// guard that cannot be false hides the bound rather than enforcing it.
func pruneMintedLocked() {
	for k, m := range minted {
		if time.Since(m.at) >= m.ttl() {
			delete(minted, k)
		}
	}
	if len(minted) < maxMintedEntries {
		return
	}
	// Least-recently-USED first. Sorting by mint time instead evicted the operator's own entries
	// preferentially, since a legitimate install's are the oldest in the map by construction — minted once
	// and good for twelve hours.
	//
	// Protecting recently-used entries from eviction was tried here and REVERTED, because it protects the
	// flood's resident entries exactly as well as the operator's, and the flood's are the ones already in
	// the map. Once 256 slots were full the operator was refused a slot permanently rather than merely
	// evicted from one: measured at 0 of 5 requests served from the cache, against 4 of 5 under plain LRU,
	// and the lockout got ~150x cheaper to mount because holding a slot only needs a touch every five
	// minutes instead of out-minting the operator. Plain LRU is the better of the two.
	//
	// What remains true, and is the honest limit of this ceiling: a sustained flood of caller-invented
	// tokens degrades the minted cache to re-minting, which for mediafusion is a POST in front of the
	// scrape. That is a latency cost, not a memory one — the bound this exists for still holds — and the
	// whole feature is off unless SCOUT_MINT_INDEXER_CONFIGS is set.
	//
	// A scan for the single oldest, not a sort of the whole map. Only one entry has to go — the caller is
	// about to insert exactly one — and sorting 256 entries to drop one of them cost 31 µs and 15 KB per
	// insert against 4 µs under the ceiling, all of it holding mintedMu, which serialises every mint in
	// the process. That is 7.5x the wall clock precisely under the flood this ceiling exists to absorb.
	for len(minted) >= maxMintedEntries {
		var oldest string
		used := time.Time{}
		for k, m := range minted {
			if used.IsZero() || m.used.Before(used) {
				oldest, used = k, m.used
			}
		}
		delete(minted, oldest)
	}
	// Once at the ceiling every insert evicts, so logging per eviction is one line per request under
	// exactly the flood this ceiling exists to absorb — unbounded log volume in place of unbounded
	// memory, written while holding the mint mutex. Once a minute is enough to notice.
	if time.Since(lastMintEvictionLog) > time.Minute {
		lastMintEvictionLog = time.Now()
		log.Printf("scout: minted-config cache is at its %d-entry ceiling; evicting least-recently-used",
			maxMintedEntries)
	}
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
	// Under the 8s per-indexer scrape budget this sits in front of, not over it. At 10s a slow mediafusion
	// could burn the whole budget before a single indexer had been asked anything.
	ctx, cancel := context.WithTimeout(ctx, mintTimeout)
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
