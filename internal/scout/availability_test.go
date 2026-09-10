package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func availabilityOf(t *testing.T, h http.Handler, config, id string) string {
	t.Helper()
	rr := postJSON(h, "/"+config+"/availability", `{"ids":["`+id+`"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("availability: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Availability map[string]string `json:"availability"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Availability[id]
}

// waitForVerdict asks until the check behind the replies has answered.
func waitForVerdict(t *testing.T, h http.Handler, config, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for got := availabilityOf(t, h, config, id); got != want; got = availabilityOf(t, h, config, id) {
		if time.Now().After(deadline) {
			t.Fatalf("availability of %s: still %q, want %q", id, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sealedConfig(t *testing.T, kr *sealKeyring, cfgJSON string) string {
	t.Helper()
	sealed, err := seal(&kr.keys[0].pub, []byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	return b64urlEncode(append([]byte{sealedVersion}, sealed...))
}

func countingScraper(n *atomic.Int32, streams []RawStream, err error) func(*Config) []scraper {
	return func(*Config) []scraper {
		return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
			n.Add(1)
			return streams, err
		}}}
	}
}

// The first ask answers unknown and checks behind the reply; a later ask has the verdict.
func TestAvailability_checksBehindTheReply(t *testing.T) {
	h := NewHandler(testDeps(nil))
	if got := availabilityOf(t, h, validBlob, "tt1234567"); got != "unknown" {
		t.Fatalf("first ask: %q, want unknown", got)
	}
	waitForVerdict(t, h, validBlob, "tt1234567", "available")
}

// Every indexer answering with nothing is a verdict. A failed scrape is not: it stays unknown, and holds
// off the next check rather than scraping again on every ask.
func TestAvailability_emptyIsUnavailableButAFailureIsUnknown(t *testing.T) {
	var none atomic.Int32
	empty := NewHandler(testDeps(func(d *Deps) { d.MakeScrapers = countingScraper(&none, nil, nil) }))
	waitForVerdict(t, empty, validBlob, "tt1234567", "unavailable")

	var scrapes atomic.Int32
	deps := testDeps(func(d *Deps) { d.MakeScrapers = countingScraper(&scrapes, nil, errors.New("indexer down")) })
	failing := NewHandler(deps)
	config, _ := decodeConfig(nil, validBlob)
	key := verdictPrefix(config) + "tt1234567"
	availabilityOf(t, failing, validBlob, "tt1234567")
	deadline := time.Now().Add(3 * time.Second)
	for v, _ := deps.Cache.Get(key); v != verdictUndetermined; v, _ = deps.Cache.Get(key) {
		if time.Now().After(deadline) {
			t.Fatal("the failed check left no marker")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := availabilityOf(t, failing, validBlob, "tt1234567"); got != "unknown" {
		t.Errorf("after a failed check: %q, want unknown", got)
	}
	time.Sleep(20 * time.Millisecond)
	if n := scrapes.Load(); n != 1 {
		t.Errorf("scrapes = %d, want 1: a failed check must hold off the next", n)
	}
}

// A stream list someone opened answers the availability question too, with no check of its own.
func TestAvailability_aStreamListRecordsTheVerdict(t *testing.T) {
	var scrapes atomic.Int32
	h := NewHandler(testDeps(func(d *Deps) { d.MakeScrapers = countingScraper(&scrapes, testSeeds(), nil) }))
	if rr := do(h, "/"+validBlob+"/stream/movie/tt1234567.json", nil); rr.Code != http.StatusOK {
		t.Fatalf("stream: %d", rr.Code)
	}
	if got := availabilityOf(t, h, validBlob, "tt1234567"); got != "available" {
		t.Errorf("after the stream list: %q, want available", got)
	}
	if n := scrapes.Load(); n != 1 {
		t.Errorf("scrapes = %d, want 1", n)
	}
}

// The scoped config answers availability — reading the verdicts the full config wrote — and its manifest,
// and nothing that lists or plays streams. A scope on a plaintext config, or an unknown one, is refused.
func TestAvailability_theScopedConfig(t *testing.T) {
	kr, _ := parseSealKeyring(vecPrivB64, "")
	const cfg = `{"debrid":[{"service":"torbox","token":"tb-secret"}],"indexers":["torrentio"],"cachedOnly":true`
	full := sealedConfig(t, kr, cfg+`}`)
	scoped := sealedConfig(t, kr, cfg+`,"scope":"availability"}`)
	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr }))

	if rr := do(h, "/"+full+"/stream/movie/tt1234567.json", nil); rr.Code != http.StatusOK {
		t.Fatalf("full stream: %d", rr.Code)
	}
	if got := availabilityOf(t, h, scoped, "tt1234567"); got != "available" {
		t.Errorf("the scoped config should read the full config's verdict: %q", got)
	}
	if rr := do(h, "/"+scoped+"/manifest.json", nil); rr.Code != http.StatusOK {
		t.Errorf("scoped manifest: %d", rr.Code)
	}
	token := encodePlayToken(PlayTarget{InfoHash: repeat("a", 40)})
	for _, path := range []string{
		"/" + scoped + "/stream/movie/tt7654321.json",
		"/" + scoped + "/stream/movie/tt7654321.json?debug=1",
		"/" + scoped + "/play/" + token,
		"/" + scoped + "/play/" + token + "?probe=1",
	} {
		if rr := do(h, path, nil); rr.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", strings.ReplaceAll(path, scoped, "<scoped>"), rr.Code)
		}
	}
	plaintext := blob(cfg + `,"scope":"availability"}`)
	if rr := postJSON(h, "/"+plaintext+"/availability", `{"ids":[]}`); rr.Code != http.StatusBadRequest {
		t.Errorf("a plaintext scoped config: %d, want 400", rr.Code)
	}
	unknownScope := sealedConfig(t, kr, cfg+`,"scope":"everything"}`)
	if rr := postJSON(h, "/"+unknownScope+"/availability", `{"ids":[]}`); rr.Code != http.StatusBadRequest {
		t.Errorf("an unknown scope: %d, want 400", rr.Code)
	}
}

func TestAvailability_refusesWhatIsNotAPageOfMovieIDs(t *testing.T) {
	h := NewHandler(testDeps(nil))
	path := "/" + validBlob + "/availability"
	if rr := do(h, path, nil); rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("allow") != "POST" {
		t.Errorf("GET: %d allow=%q", rr.Code, rr.Header().Get("allow"))
	}
	ids := make([]string, maxAvailabilityIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf(`"tt%d"`, i+1)
	}
	for name, body := range map[string]string{
		"not json":       "ids",
		"too many":       `{"ids":[` + strings.Join(ids, ",") + `]}`,
		"not an imdb id": `{"ids":["tt1","1234"]}`,
		"an episode":     `{"ids":["tt1:1:1"]}`,
	} {
		if rr := postJSON(h, path, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rr.Code)
		}
	}
	if rr := postJSON(h, "/@@@/availability", `{"ids":[]}`); rr.Code != http.StatusBadRequest {
		t.Errorf("bad config: %d, want 400", rr.Code)
	}
}
