package scout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stubDoer answers one canned response and records what it was asked.
type stubDoer struct {
	status   int
	body     string
	err      error
	reqs     int
	lastBody string
}

func (d *stubDoer) Do(req *http.Request) (*http.Response, error) {
	d.reqs++
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		d.lastBody = string(b)
	}
	if d.err != nil {
		return nil, d.err
	}
	return &http.Response{StatusCode: d.status, Body: io.NopCloser(strings.NewReader(d.body))}, nil
}

func resetMinted() {
	mintedMu.Lock()
	minted = map[string]mintedConfig{}
	mintedMu.Unlock()
}

// The comet config is built locally — it carries the debrid token, which is the whole reason this is
// opt-in, so the shape is worth pinning.
func TestMintCometURL_carriesTheAccount(t *testing.T) {
	got := mintCometURL(DebridAccount{Service: ServiceTorBox, Token: "tb-token"})
	if !strings.HasPrefix(got, defaultIndexerURLs["comet"]+"/") {
		t.Fatalf("not a comet URL: %s", got)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, defaultIndexerURLs["comet"]+"/"))
	if err != nil {
		t.Fatalf("config segment is not base64: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config segment is not JSON: %v", err)
	}
	if cfg["debridService"] != "torbox" || cfg["debridApiKey"] != "tb-token" {
		t.Errorf("account not carried: %v / %v", cfg["debridService"], cfg["debridApiKey"])
	}
}

// MediaFusion encrypts server-side, so a mint is a round trip that can fail — and a failure must not
// produce a half-formed URL that then 404s on every search.
func TestMintMediaFusionURL(t *testing.T) {
	ok := &stubDoer{status: 200, body: `{"encrypted_str":"ENCRYPTED","status":"success"}`}
	got := mintMediaFusionURL(context.Background(), DebridAccount{Service: ServiceTorBox, Token: "t"}, ok)
	if got != defaultIndexerURLs["mediafusion"]+"/ENCRYPTED" {
		t.Errorf("minted URL: %s", got)
	}
	if !strings.Contains(ok.lastBody, `"token":"t"`) {
		t.Errorf("the account was not sent: %s", ok.lastBody)
	}

	refused := &stubDoer{status: 200, body: `{"message":"invalid user data","status":"error"}`}
	if got := mintMediaFusionURL(context.Background(), DebridAccount{Token: "t"}, refused); got != "" {
		t.Errorf("a refusal must yield no URL, got %s", got)
	}
	transport := &stubDoer{err: fmt.Errorf("dial tcp: refused")}
	if got := mintMediaFusionURL(context.Background(), DebridAccount{Token: "t"}, transport); got != "" {
		t.Errorf("an unreachable host must yield no URL, got %s", got)
	}
	if got := mintMediaFusionURL(context.Background(), DebridAccount{Token: "t"}, nil); got != "" {
		t.Errorf("no client must yield no URL, got %s", got)
	}
}

// Minted once per account, then reused: a season opens with one request per episode, and minting on each
// would put a round trip to a third party in front of every one of them.
func TestIndexerBaseWithConfig_cachesPerAccount(t *testing.T) {
	resetMinted()
	d := &stubDoer{status: 200, body: `{"encrypted_str":"E1","status":"success"}`}
	cfg := &Config{Debrid: []DebridAccount{{Service: ServiceTorBox, Token: "acct-a"}}}

	first, _ := indexerBaseWithConfig(context.Background(), "mediafusion", cfg, d)
	second, _ := indexerBaseWithConfig(context.Background(), "mediafusion", cfg, d)
	if first == "" || first != second {
		t.Fatalf("expected a stable minted URL, got %q then %q", first, second)
	}
	if d.reqs != 1 {
		t.Errorf("minted %d times, want 1", d.reqs)
	}

	// A different account must not reuse the first account's config — that would search one person's
	// debrid with another's token.
	other := &Config{Debrid: []DebridAccount{{Service: ServiceTorBox, Token: "acct-b"}}}
	d.body = `{"encrypted_str":"E2","status":"success"}`
	if got, _ := indexerBaseWithConfig(context.Background(), "mediafusion", other, d); got == first {
		t.Errorf("a second account reused the first's config: %s", got)
	}
}

// With no debrid account there is nothing to mint from, and comet is built without a round trip.
func TestIndexerBaseWithConfig_edges(t *testing.T) {
	resetMinted()
	d := &stubDoer{status: 200, body: `{"encrypted_str":"E","status":"success"}`}
	if got, transient := indexerBaseWithConfig(context.Background(), "mediafusion", &Config{}, d); got != "" || transient {
		t.Errorf("no account should mint nothing, and permanently so: %q transient=%v", got, transient)
	}
	if _, ok := primaryDebrid(nil); ok {
		t.Error("a nil config has no account")
	}
	cfg := &Config{Debrid: []DebridAccount{{Service: ServiceTorBox, Token: "t"}}}
	if got, _ := indexerBaseWithConfig(context.Background(), "comet", cfg, d); got == "" {
		t.Error("comet mints locally and should need no round trip")
	}
	if got, _ := indexerBaseWithConfig(context.Background(), "torrentio", cfg, d); got != "" {
		t.Errorf("torrentio needs no config segment, got %s", got)
	}
}

// A mint that FAILED because the indexer was unreachable is an outage, not a misconfiguration, and the
// two must stay distinguishable.
//
// They were not. A failed mint is cached for two minutes and makes the indexer "unaskable", which the
// empty-result quorum EXCUSES — so one 10s timeout on a loaded homelab promoted torrentio's empty answer
// to an authoritative "this release does not exist", cached and served with stale-if-error for a day.
// A transient failure means we do not know what that indexer would have said, which is precisely the
// state the quorum exists to detect.
func TestIndexerBaseWithConfig_separatesOutageFromMisconfiguration(t *testing.T) {
	cfg := &Config{Debrid: []DebridAccount{{Service: ServiceTorBox, Token: "t"}}}

	resetMinted()
	unreachable := &stubDoer{err: fmt.Errorf("dial tcp: i/o timeout")}
	got, transient := indexerBaseWithConfig(context.Background(), "mediafusion", cfg, unreachable)
	if got != "" || !transient {
		t.Errorf("an unreachable indexer is a transient failure: %q transient=%v", got, transient)
	}

	// The classification must survive the failure cache, or the two minutes it is replayed for
	// reintroduce the same confusion.
	if _, cached := indexerBaseWithConfig(context.Background(), "mediafusion", cfg, unreachable); !cached {
		t.Error("the cached failure forgot that it was transient")
	}
	if unreachable.reqs != 1 {
		t.Errorf("the failure was re-POSTed %d times instead of being cached", unreachable.reqs)
	}
}

// A transient mint failure COUNTS as an indexer that did not answer, so an empty list stays
// non-authoritative. Only a permanently unconfigured one is excused from the quorum.
func TestUnaskableScraper_transientStillCountsInTheQuorum(t *testing.T) {
	answered := fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) { return nil, nil }}
	budget := 50 * time.Millisecond

	unconfigured := []scraper{answered, unaskableScraper{indexer: "mediafusion"}}
	if _, ok, _ := scrapeAll(context.Background(), unconfigured, scrapeQuery{}, budget); !ok {
		t.Error("an unconfigured indexer must not make an empty result look like an outage")
	}

	outage := []scraper{answered, unaskableScraper{indexer: "mediafusion", transient: true}}
	if _, ok, _ := scrapeAll(context.Background(), outage, scrapeQuery{}, budget); ok {
		t.Error("an indexer that could not be reached must leave the empty result non-authoritative")
	}
}

// Off unless the operator says otherwise: this decides whether the debrid token leaves the homelab.
func TestMintingIsOptIn(t *testing.T) {
	if SettingsFromEnv(func(string) string { return "" }).MintIndexerConfigs {
		t.Error("minting must default to off — it sends the debrid token to a third party")
	}
	on := SettingsFromEnv(func(k string) string {
		if k == "SCOUT_MINT_INDEXER_CONFIGS" {
			return "true"
		}
		return ""
	})
	if !on.MintIndexerConfigs {
		t.Error("SCOUT_MINT_INDEXER_CONFIGS=true should enable it")
	}
}

// Expired mint entries are reclaimed on write.
//
// Expiry was only consulted on READ, so an entry nobody asked for again stayed resident forever — and
// the key is derived from a token the config supplies unverified, with comet's mint needing no network
// at all, so every distinct token minted successfully and was kept.
func TestMinted_expiredEntriesAreReclaimed(t *testing.T) {
	resetMinted()
	mintedMu.Lock()
	for i := 0; i < 500; i++ {
		minted[fmt.Sprintf("comet:%d", i)] = mintedConfig{url: "u", at: time.Now().Add(-2 * mintedTTL)}
	}
	live := "comet:live"
	minted[live] = mintedConfig{url: "u", at: time.Now()}
	mintedMu.Unlock()

	cfg := &Config{Debrid: []DebridAccount{{Service: ServiceTorBox, Token: "fresh"}}}
	if url, _ := indexerBaseWithConfig(context.Background(), "comet", cfg, nil); url == "" {
		t.Fatal("comet mints locally and should succeed")
	}
	mintedMu.Lock()
	defer mintedMu.Unlock()
	if len(minted) > 2 {
		t.Errorf("%d entries retained; expired mints are never reclaimed", len(minted))
	}
	if _, ok := minted[live]; !ok {
		t.Error("a live entry was pruned")
	}
}

// The minted-config cache is bounded by COUNT as well as by age.
//
// TTL pruning alone cannot bound it, and that is what made it look guarded: the key derives from a token
// the config supplies and nobody verifies, and comet's mint is local base64 with no round trip, so every
// distinct token a caller invents mints successfully and is held for twelve hours. At ~500 bytes an
// entry that is a memory bomb with a twelve-hour fuse inside a 230 MiB heap.
func TestMintedCache_isBoundedByCount(t *testing.T) {
	mintedMu.Lock()
	saved := minted
	minted = map[string]mintedConfig{}
	mintedMu.Unlock()
	t.Cleanup(func() {
		mintedMu.Lock()
		minted = saved
		mintedMu.Unlock()
	})

	// Far more distinct, unexpired entries than the ceiling — all well inside mintedTTL so nothing expires
	// out, and all IDLE past the protect window so all of them are eviction candidates.
	now := time.Now()
	idleBase := now.Add(-time.Hour)
	mintedMu.Lock()
	for i := 0; i < maxMintedEntries*3; i++ {
		// Staggered so "least recently used first" is a defined order.
		stamp := idleBase.Add(-time.Duration(i) * time.Second)
		minted[fmt.Sprintf("comet:%d", i)] = mintedConfig{url: "https://comet.example/x", at: now, used: stamp}
	}
	room := pruneMintedLocked()
	got := len(minted)
	_, newestKept := minted["comet:0"]
	_, oldestKept := minted[fmt.Sprintf("comet:%d", maxMintedEntries*3-1)]
	mintedMu.Unlock()
	if !room {
		t.Error("prune found no room despite every entry being idle and evictable")
	}

	if got >= maxMintedEntries {
		t.Errorf("kept %d entries, want under the %d ceiling (with room for the insert that follows)",
			got, maxMintedEntries)
	}
	if got == 0 {
		t.Error("the prune emptied the cache rather than trimming it")
	}
	if !newestKept {
		t.Error("the newest entry was evicted")
	}
	if oldestKept {
		t.Error("the oldest entry survived while newer ones were evicted")
	}
}

// A cache under the ceiling with nothing expired is left completely alone.
func TestMintedCache_pruneKeepsAHealthyCache(t *testing.T) {
	mintedMu.Lock()
	saved := minted
	minted = map[string]mintedConfig{}
	for i := 0; i < 5; i++ {
		minted[fmt.Sprintf("comet:%d", i)] = mintedConfig{url: "https://comet.example/x", at: time.Now()}
	}
	pruneMintedLocked()
	got := len(minted)
	minted = saved
	mintedMu.Unlock()
	if got != 5 {
		t.Errorf("a healthy cache of 5 was pruned to %d", got)
	}
}

// Eviction is least-recently-USED, not oldest-minted — and the difference is the whole point.
//
// A legitimate install's entries are the OLDEST things in the map by construction: minted once and good
// for twelve hours. Evicting by mint time therefore threw them out first and kept a flood of
// caller-invented ones, so every later stream request re-minted — for mediafusion a POST with a 3s
// timeout sitting in front of the scrape. The ceiling turned a memory bomb into a latency one.
func TestMintedCache_keepsTheEntriesActuallyInUse(t *testing.T) {
	mintedMu.Lock()
	saved := minted
	minted = map[string]mintedConfig{}
	mintedMu.Unlock()
	t.Cleanup(func() {
		mintedMu.Lock()
		minted = saved
		mintedMu.Unlock()
	})

	now := time.Now()
	mintedMu.Lock()
	// The operator's own: minted an hour ago and served just now. Sorting by MINT time evicts exactly
	// these, because a legitimate install's entries are the oldest in the map by construction.
	legit := []string{"comet:real", "mediafusion:real"}
	for _, k := range legit {
		minted[k] = mintedConfig{url: "https://real.example/x", at: now.Add(-time.Hour), used: now}
	}
	// Freshly minted entries that nobody has come back for.
	for i := 0; i < maxMintedEntries*2; i++ {
		stamp := now.Add(-time.Duration(i+1) * time.Second)
		minted[fmt.Sprintf("comet:flood%d", i)] = mintedConfig{url: "https://flood.example/x", at: stamp, used: stamp}
	}
	room := pruneMintedLocked()
	var survived int
	for _, k := range legit {
		if _, ok := minted[k]; ok {
			survived++
		}
	}
	total := len(minted)
	mintedMu.Unlock()

	if survived != len(legit) {
		t.Errorf("%d of %d in-use entries survived — eviction is not ordered by last use", survived, len(legit))
	}
	if !room || total >= maxMintedEntries {
		t.Errorf("ceiling not enforced: room=%v total=%d, want room and under %d", room, total, maxMintedEntries)
	}
}

// An entry idle past the protect window is evictable again, so a genuinely disused config cannot pin a
// slot until its 12-hour TTL runs out.
func TestMintedCache_reclaimsIdleEntries(t *testing.T) {
	mintedMu.Lock()
	saved := minted
	minted = map[string]mintedConfig{}
	mintedMu.Unlock()
	t.Cleanup(func() {
		mintedMu.Lock()
		minted = saved
		mintedMu.Unlock()
	})

	now := time.Now()
	mintedMu.Lock()
	for i := 0; i < maxMintedEntries+10; i++ {
		stamp := now.Add(-time.Duration(i) * time.Second)
		minted[fmt.Sprintf("comet:idle%d", i)] = mintedConfig{url: "https://x.example/x", at: now, used: stamp}
	}
	room := pruneMintedLocked()
	total := len(minted)
	mintedMu.Unlock()

	if !room || total >= maxMintedEntries {
		t.Errorf("idle entries were not reclaimed: room=%v total=%d, want room and under %d",
			room, total, maxMintedEntries)
	}
}

// The ceiling holds through the REAL caller, not just through pruneMintedLocked.
//
// Under a flood the prune may evict nothing, and then the caller-side check is the only thing bounding
// the map — so it has to be exercised where the caller lives. Making both call sites insert
// unconditionally left the whole suite green while 1,000 caller-invented tokens sat in the map.
func TestIndexerBaseWithConfig_mapStaysBoundedUnderAFlood(t *testing.T) {
	resetMinted()
	t.Cleanup(resetMinted)

	// comet mints locally, so a flood costs no round trips — which is exactly why it is the cheap attack.
	d := &stubDoer{status: 200, body: `{"encrypted_str":"E","status":"success"}`}
	for i := 0; i < maxMintedEntries*4; i++ {
		cfg := &Config{Debrid: []DebridAccount{{Service: ServiceTorBox, Token: fmt.Sprintf("invented-%d", i)}}}
		if got, _ := indexerBaseWithConfig(context.Background(), "comet", cfg, d); got == "" {
			t.Fatalf("mint %d produced no URL", i)
		}
	}

	mintedMu.Lock()
	total := len(minted)
	mintedMu.Unlock()
	if total > maxMintedEntries {
		t.Errorf("the map holds %d entries after a flood of distinct tokens, ceiling is %d",
			total, maxMintedEntries)
	}
}
