package scout

import (
	"context"
	"net/http"
	"path"
	"strings"
	"testing"
	"time"
)

// The shared shape, and LOG_IDENTITY's one job: strip the identity suffix, keep everything else.
func TestDecisionLine_identityGating(t *testing.T) {
	defer SetLogIdentity(true)

	SetLogIdentity(true)
	on := decisionLine("rank", "served", "picked from 3 candidates", "torrentio", 12*time.Millisecond, "req-1", "imdb=tt1 type=movie")
	for _, want := range []string{"event=rank", "outcome=served", "reason=picked from 3 candidates",
		"upstream=torrentio", "dur_ms=12", "rid=req-1", "imdb=tt1 type=movie"} {
		if !strings.Contains(on, want) {
			t.Errorf("LOG_IDENTITY=on: line lacks %q: %s", want, on)
		}
	}

	SetLogIdentity(false)
	off := decisionLine("rank", "served", "picked from 3 candidates", "torrentio", 12*time.Millisecond, "req-1", "imdb=tt1 type=movie")
	for _, want := range []string{"event=rank", "outcome=served", "upstream=torrentio", "dur_ms=12", "rid=req-1"} {
		if !strings.Contains(off, want) {
			t.Errorf("LOG_IDENTITY=off: line lost a non-identity field %q: %s", want, off)
		}
	}
	if strings.Contains(off, "imdb=") || strings.Contains(off, "type=movie") {
		t.Errorf("LOG_IDENTITY=off: identity survived: %s", off)
	}

	// upstream and rid are each omitted on their own when absent — neither forces the other's presence.
	bare := decisionLine("probe", "skipped", "not queued", "", 0, "", "")
	if strings.Contains(bare, "upstream=") || strings.Contains(bare, "rid=") {
		t.Errorf("an absent upstream/rid should not print at all: %s", bare)
	}
}

func TestTopDropReasons(t *testing.T) {
	if got := topDropReasons(nil, 3); got != "" {
		t.Errorf("nil map: got %q, want empty", got)
	}
	got := topDropReasons(map[string]int{"cam": 1, "resolution": 5, "minSeeders": 3, "hdrOnly": 1}, 3)
	if got != "resolution:5,minSeeders:3,cam:1" && got != "resolution:5,minSeeders:3,hdrOnly:1" {
		t.Errorf("top 3 by count, ties by name: got %q", got)
	}
	if !strings.HasPrefix(got, "resolution:5,minSeeders:3,") {
		t.Errorf("highest counts first: got %q", got)
	}
}

func TestFormatPick(t *testing.T) {
	s := RawStream{Title: "Movie.2024.2160p.BluRay.REMUX", InfoHash: repeat("a", 40), SizeBytes: intp(42 * gib), Cached: true}
	got := formatPick(s)
	for _, want := range []string{`"Movie.2024.2160p.BluRay.REMUX"`, "hash=" + repeat("a", 12), "size_gb=42.0", "cached"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatPick missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "uncached") {
		t.Errorf("a cached release must not also read uncached: %s", got)
	}
	// A release name longer than the cap is cut, never left to grow the log line without bound.
	long := RawStream{Title: strings.Repeat("x", maxLoggedTitle+50), InfoHash: repeat("b", 40)}
	if got := formatPick(long); strings.Contains(got, strings.Repeat("x", maxLoggedTitle+1)) {
		t.Errorf("formatPick did not truncate a long title: %s", got)
	}
}

// Every list build logs its own pick — not just ?debug=1 — with the title id, how many candidates were
// in contention, the chosen release and its runner-up, and the drop reason that removed the junk one.
// LOG_IDENTITY off keeps rid but drops all of that.
func TestRankDecisionLine(t *testing.T) {
	defer SetLogIdentity(true)
	out := captureLog(t)
	h := NewHandler(testDeps(nil))

	forgetLogged("rank-decision")
	do(h, "/"+validBlob+"/stream/movie/tt9000001.json", map[string]string{"X-Request-Id": "req-rank-1"})
	got := out.String()
	for _, want := range []string{"event=rank", "outcome=served", "rid=req-rank-1", "imdb=tt9000001", "type=movie",
		"top_drops=cam:1", "picked=", "runnerup1="} {
		if !strings.Contains(got, want) {
			t.Errorf("rank decision line missing %q:\n%s", want, got)
		}
	}

	SetLogIdentity(false)
	forgetLogged("rank-decision")
	do(h, "/"+validBlob+"/stream/movie/tt9000002.json", map[string]string{"X-Request-Id": "req-rank-2"})
	line2 := lastLineContaining(t, out.String(), "req-rank-2")
	if !strings.Contains(line2, "rid=req-rank-2") {
		t.Errorf("rid must survive LOG_IDENTITY=off: %s", line2)
	}
	for _, leak := range []string{"imdb=", "picked=", "top_drops="} {
		if strings.Contains(line2, leak) {
			t.Errorf("LOG_IDENTITY=off still carries %q: %s", leak, line2)
		}
	}
}

// lastLineContaining returns the last line of a captured log buffer containing needle, for asserting on
// one decision line within a buffer that accumulated several.
func lastLineContaining(t *testing.T, buf, needle string) string {
	t.Helper()
	lines := strings.Split(buf, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], needle) {
			return lines[i]
		}
	}
	t.Fatalf("no line containing %q in:\n%s", needle, buf)
	return ""
}

// A scrape failure names the indexer, the error CLASS, how long it took and the request/title that hit
// it — never the indexer's base URL, which for a configured source can carry an encrypted config.
func TestScrapeDecisionLine(t *testing.T) {
	out := captureLog(t)

	forgetLogged("indexer-status:torz")
	bad := &stremioScraper{indexer: "torz", baseURL: "https://torz.example/SECRET-PATH",
		client: mockDoer{fn: func(*http.Request) (*http.Response, error) { return resp(502, "{}"), nil }}}
	_, _, _ = bad.scrapeOnce(context.Background(), scrapeQuery{Type: "movie", IMDb: "tt1234567", Rid: "req-scrape-1"})
	got := out.String()
	for _, want := range []string{"event=scrape", "upstream=torz", "rid=req-scrape-1", "imdb=tt1234567",
		"type=movie", "reason=http 502"} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape decision line missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "SECRET-PATH") || strings.Contains(got, "torz.example") {
		t.Errorf("the indexer base URL leaked into the log:\n%s", got)
	}

	// A transport failure never logs the error itself — Go's http.Client wraps a *url.Error carrying the
	// full request URL, which for a debrid-backed indexer carries the account token in its query string.
	forgetLogged("indexer-unreachable:torz")
	unreachable := &stremioScraper{indexer: "torz", baseURL: "https://torz.example/SECRET-PATH", client: &failingDoer{}}
	_, _, _ = unreachable.scrapeOnce(context.Background(), scrapeQuery{Type: "movie", IMDb: "tt7654321", Rid: "req-scrape-2"})
	got = out.String()
	if !strings.Contains(got, "rid=req-scrape-2") || !strings.Contains(got, "event=scrape") {
		t.Errorf("transport-failure decision line missing its shape:\n%s", got)
	}
	if strings.Contains(got, "SECRET-PATH") || strings.Contains(got, leakToken) {
		t.Errorf("the transport error (or its URL) leaked into the log:\n%s", got)
	}
}

// A play/probe/cancel decision line never carries the debrid token, even when the store's own error
// text would have — the redaction is at the store (stores.go), and this proves the new line respects it
// end to end, through the real DELETE /play route.
func TestCancelDecisionLine_neverLeaksTheToken(t *testing.T) {
	out := captureLog(t)
	forgetLogged("cancel-store-unavailable")
	hash := repeat("a", 40)
	h, _ := cancelTorBoxHandler(func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "mylist":
			if r.URL.Query().Get("id") != "" {
				return resp(200, `{"success":true,"data":{"id":42,"download_finished":false}}`)
			}
			return resp(200, `{"success":true,"data":[]}`)
		case "controltorrent":
			return resp(503, `{"error":"proxy_error","detail":"upstream rejected Bearer tb-secret"}`)
		}
		return resp(404, `{}`)
	}, func(cache Cache) {
		cache.Put(torrentIDKey("tb-secret", hash), "42", resolveCacheTTL)
		markAddedByUs(cache, "tb-secret", hash)
	})
	rr := doMethod(h, http.MethodDelete, cancelPath(hash), map[string]string{"X-Request-Id": "req-cancel-1"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancel: %d, want 503: %s", rr.Code, rr.Body.String())
	}
	got := out.String()
	if !strings.Contains(got, "event=cancel") || !strings.Contains(got, "rid=req-cancel-1") || !strings.Contains(got, "upstream=torbox") {
		t.Errorf("cancel decision line missing its shape:\n%s", got)
	}
	// The precondition: the refusal detail really did reach the error, so redaction — not the fixture's
	// silence — is what the assertion below is proving.
	if !strings.Contains(got, "proxy_error") {
		t.Fatalf("precondition failed: the refusal detail never reached the line:\n%s", got)
	}
	if strings.Contains(got, "tb-secret") {
		t.Fatalf("the debrid token leaked into the cancel decision line:\n%s", got)
	}
}
