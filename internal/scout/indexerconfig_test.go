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
	if _, ok := scrapeAll(context.Background(), unconfigured, scrapeQuery{}, budget); !ok {
		t.Error("an unconfigured indexer must not make an empty result look like an outage")
	}

	outage := []scraper{answered, unaskableScraper{indexer: "mediafusion", transient: true}}
	if _, ok := scrapeAll(context.Background(), outage, scrapeQuery{}, budget); ok {
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
