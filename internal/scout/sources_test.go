package scout

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestNormalizeSources(t *testing.T) {
	mf := "https://mediafusion.example/D-secret"
	got := normalizeSources([]string{
		" " + mf + "/manifest.json ",                // what an addon's configure page hands out
		"stremio://comet.example/cfg/manifest.json", // an install button's link
		mf + "/",                   // the first one again
		"http://plain.example/cfg", // not https
		"https://127.0.0.1/cfg",    // a non-public address, written out
		"https://[::1]/cfg",
		"https://100.101.102.103/cfg", // the tailnet
		"https://localhost/cfg",
		"https://user:pw@creds.example/cfg",
		"https://query.example/cfg?token=x",
		"https://long.example/" + strings.Repeat("x", maxSourceURL),
		"https://third.example/cfg",
		"https://fourth.example/cfg", // past maxSources
	})
	want := []string{mf, "https://comet.example/cfg", "https://third.example/cfg"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("sources = %q, want %q", got, want)
	}
}

// Sources ride in the install config, and an unusable one is dropped rather than refusing the config, like
// every other field there.
func TestDecodeConfig_carriesSources(t *testing.T) {
	c, ok := decodeConfig(nil, blob(`{"debrid":[{"service":"torbox","token":"t"}],`+
		`"sources":["https://mf.example/cfg/manifest.json","http://nope.example"]}`))
	if !ok || len(c.Sources) != 1 || c.Sources[0] != "https://mf.example/cfg" {
		t.Fatalf("config = %+v, ok = %v", c, ok)
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"1.1.1.1":                true,
		"2606:4700:4700::1111":   true,
		"10.0.0.1":               false,
		"172.16.5.4":             false,
		"192.168.86.193":         false, // den itself
		"127.0.0.1":              false,
		"169.254.169.254":        false, // cloud metadata
		"100.64.0.1":             false, // tailnet / CGNAT
		"0.0.0.0":                false,
		"255.255.255.255":        false,
		"::1":                    false,
		"fe80::1":                false,
		"fd7a:115c:a1e0::1":      false, // tailnet IPv6
		"::ffff:192.168.1.1":     false, // IPv4-mapped
		"64:ff9b::c0a8:101":      false, // NAT64 to 192.168.1.1
		"2002:c0a8:101::1":       false, // 6to4 around 192.168.1.1
		"224.0.0.1":              false,
		"198.18.0.1":             false,
		"2001:4860:4860::8888":   true,
		"::ffff:8.8.8.8":         true,
		"2001:db8::1":            true, // documentation range, unroutable either way
		"192.0.2.1":              true, // documentation range, unroutable either way
		"203.0.113.9":            true,
		"93.184.216.34":          true,
		"2a00:1450:4001::200e":   true,
		"240.0.0.1":              false,
		"fec0::1":                false,
		"64:ff9b:1::1":           false,
		"100.127.255.255":        false,
		"100.128.0.1":            true,
		"172.32.0.1":             true,
		"192.169.0.1":            true,
		"11.0.0.1":               true,
		"126.255.255.255":        true,
		"128.0.0.1":              true,
		"9.255.255.255":          true,
		"1.0.0.0":                true,
		"223.255.255.255":        true,
		"::":                     false,
		"ff02::1":                false,
		"fc00::1":                false,
		"2001:470::1":            true,
		"::ffff:100.64.0.1":      false,
		"::ffff:169.254.169.254": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The guard sits on the connection, not on the name: a host that resolves to the box's own network is
// refused at dial time, whatever the config said.
func TestSourceClient_refusesANonPublicAddress(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a source on a loopback address was reached")
	}))
	defer srv.Close()
	_, err := newSourceClient().Get(srv.URL + "/manifest.json")
	if !errors.Is(err, errNotPublic) {
		t.Fatalf("err = %v, want the non-public refusal", err)
	}

	// And a scraper asking it reports the refusal once, without spending the budget on retries.
	calls := 0
	sc := &stremioScraper{indexer: ownSources, label: "loopback", baseURL: srv.URL, limiter: sourceLimiter,
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) { calls++; return nil, errNotPublic }}}
	if _, err := sc.scrape(context.Background(), scrapeQuery{Type: "movie", IMDb: "tt1"}); err == nil || calls != 1 {
		t.Errorf("err = %v after %d request(s), want one refused request", err, calls)
	}
}

func TestMakeScrapers_asksHouseholdSourcesThroughTheGuardedClient(t *testing.T) {
	plain, guarded := mockDoer{}, mockDoer{fn: func(*http.Request) (*http.Response, error) { return resp(200, "{}"), nil }}
	cfg := &Config{Indexers: []Indexer{"torrentio"}, Sources: []string{"https://mf.example/cfg"}}
	got := makeScrapers(cfg, plain, guarded, nil)
	if len(got) != 2 {
		t.Fatalf("scrapers = %v", got)
	}
	own, ok := got[0].(*stremioScraper)
	if !ok || own.id() != ownSources || own.baseURL != "https://mf.example/cfg" || own.limiter != sourceLimiter {
		t.Fatalf("household source scraper = %+v", got[0])
	}
	if own.client.(mockDoer).fn == nil {
		t.Error("a household source must be fetched with the guarded client")
	}
	if own.name() != "own source mf.example" {
		t.Errorf("name = %q", own.name())
	}
}

// An addon configured with a debrid account links to its own resolver rather than naming the torrent; the
// hash in that link is what scout checks its own debrid for.
func TestParseStremioStreams_readsTheHashOutOfAPlaybackLink(t *testing.T) {
	h1, h2, h3 := strings.Repeat("a", 40), strings.Repeat("B", 40), strings.Repeat("c", 40)
	body := `{"streams":[
{"name":"MediaFusion","description":"Movie.2024.1080p.WEB-DL","url":"https://mf.example/streaming_provider/SECRET/playback/torbox/` + h1 + `"},
{"name":"Comet","description":"Movie.2024.2160p.BluRay","url":"https://comet.example/CFG/playback/` + h2 + `/0/Movie.mkv"},
{"name":"Torrentio","title":"Movie.2024.720p","url":"https://torrentio.example/resolve/torbox/KEY/` + h3 + `/Movie.mkv/1"},
{"name":"no hash","url":"https://x.example/playback/short"},
{"name":"hash only in the query","url":"https://x.example/play?h=` + h1 + `"},
{"name":"field wins","infoHash":"` + h3 + `","url":"https://x.example/playback/` + h1 + `"}
]}`
	seeds := parseStremioStreams([]byte(body), "own")
	var got []string
	for _, s := range seeds {
		got = append(got, s.InfoHash)
	}
	want := []string{h1, strings.ToLower(h2), h3, h3}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("hashes = %v, want %v", got, want)
	}
}

func TestBoundedHostLimiter_forgetsHostsPastItsCeiling(t *testing.T) {
	l := newBoundedHostLimiter(time.Hour, 1, 2)
	ctx := context.Background()
	l.wait(ctx, "a.example")
	l.wait(ctx, "b.example")
	l.wait(ctx, "c.example")
	if n := len(l.buckets); n != 1 {
		t.Errorf("buckets = %d, want the map started over at the ceiling", n)
	}
}
