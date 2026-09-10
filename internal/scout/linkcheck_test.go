package scout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckLink_verdicts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		ctype    string
		rng      string
		wantSize int64
		want     linkVerdict
	}{
		{"206 of the listed size", 206, "video/x-matroska", "bytes 0-0/1000", 1000, linkPlayable},
		{"200, the range ignored", 200, "application/octet-stream", "", 1000, linkPlayable},
		{"size unknown to the debrid", 206, "video/mp4", "bytes 0-0/5555", 0, linkPlayable},
		{"total not stated", 206, "video/mp4", "bytes 0-0/*", 1000, linkPlayable},
		{"within the tolerance", 206, "video/mp4", "bytes 0-0/1015", 1000, linkPlayable},
		{"a different file", 206, "video/mp4", "bytes 0-0/1100", 1000, linkBroken},
		{"an error page", 200, "text/html; charset=utf-8", "", 0, linkBroken},
		{"a JSON error body", 200, "application/json", "", 0, linkBroken},
		{"not found", 404, "text/plain", "", 0, linkBroken},
		{"forbidden", 403, "text/plain", "", 0, linkBroken},
		{"throttled", 429, "text/plain", "", 0, linkUnverified},
		{"CDN fault", 503, "text/plain", "", 0, linkUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=0-0" {
					t.Errorf("range = %q, want bytes=0-0", r.Header.Get("Range"))
				}
				w.Header().Set("content-type", tc.ctype)
				if tc.rng != "" {
					w.Header().Set("content-range", tc.rng)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte{0})
			}))
			defer srv.Close()
			got, reason := checkLink(t.Context(), srv.Client(), srv.URL+"/f.mkv", tc.wantSize)
			if got != tc.want {
				t.Errorf("verdict = %d (%s), want %d", got, reason, tc.want)
			}
		})
	}
}

// The check failing to get an answer says nothing about the link, so it never condemns one.
func TestCheckLink_itsOwnFailureDoesNotCondemnTheLink(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	if got, _ := checkLink(t.Context(), http.DefaultClient, closedURL+"/f.mkv", 0); got != linkUnverified {
		t.Errorf("a refused connection gave verdict %d, want unverified", got)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if got, _ := checkLink(ctx, slow.Client(), slow.URL+"/f.mkv", 0); got != linkUnverified {
		t.Errorf("a timed-out check gave verdict %d, want unverified", got)
	}
}

// A minted link that fails its check is a dead link to the client, and is not remembered.
func TestPlay_aLinkThatFailsItsCheckIsADeadLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte("<html>error</html>"))
	}))
	defer srv.Close()
	store := &tallyStore{svc: ServiceTorBox, resolve: func(int32) (string, error) { return srv.URL + "/f.mkv", nil }}
	h := NewHandler(testDeps(func(d *Deps) {
		d.ProbeClient = srv.Client()
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)})

	for i := 1; i <= 2; i++ {
		rr := do(h, path, nil)
		if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "dead_link") {
			t.Fatalf("play %d: %d %s, want 404 dead_link", i, rr.Code, rr.Body.String())
		}
	}
	if got := store.resolves.Load(); got != 2 {
		t.Errorf("resolves = %d, want 2 — a rejected link must not be served from the memo", got)
	}
}

// A check that cannot answer lets the link through, and leaves it to be checked again on the next hit.
func TestPlay_aCheckThatCannotAnswerPassesTheLinkThrough(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	link := closed.URL + "/f.mkv"
	closed.Close()
	store := &tallyStore{svc: ServiceTorBox, resolve: func(int32) (string, error) { return link, nil }}
	h := &handler{deps: testDeps(func(d *Deps) {
		d.ProbeClient = http.DefaultClient
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	})}
	config, _ := decodeConfig(nil, validBlob)
	target := ResolveTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)}
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: target.InfoHash, FileIdx: target.FileIdx})

	rr := do(http.HandlerFunc(h.serve), path, nil)
	if rr.Code != http.StatusFound || rr.Header().Get("location") != link {
		t.Fatalf("play: %d %q, want 302 to the unchecked link", rr.Code, rr.Header().Get("location"))
	}
	hit, ok := h.links.get(linkMemoKey(config, target), time.Now())
	if !ok || hit.checked {
		t.Fatalf("memo = %+v ok=%v, want an unchecked entry", hit, ok)
	}
}

// A link that passed its check once is not checked again on a memo hit.
func TestPlay_aCheckedLinkIsNotCheckedAgain(t *testing.T) {
	var checks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		checks.Add(1)
		w.Header().Set("content-type", "video/x-matroska")
		w.Header().Set("content-range", "bytes 0-0/1000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	defer srv.Close()
	store := &tallyStore{svc: ServiceTorBox, resolve: func(int32) (string, error) { return srv.URL + "/f.mkv", nil }}
	h := NewHandler(testDeps(func(d *Deps) {
		d.ProbeClient = srv.Client()
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)})

	for i := 1; i <= 3; i++ {
		if rr := do(h, path, nil); rr.Code != http.StatusFound {
			t.Fatalf("play %d: %d", i, rr.Code)
		}
	}
	if got := checks.Load(); got != 1 {
		t.Errorf("link checked %d times, want once", got)
	}
}

// Where the sizes come from: TorBox's resolve entry, and what RD's info listed for the chosen file.
func TestKnownFileSize_fromWhatTheStoresAlreadyHold(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	tb := &torBoxStore{token: "t", cache: cache, api: torboxAPI}
	cache.Put(resolveKey("t", H), `{"torrentId":9,"files":[{"Index":0,"Name":"S01E01.mkv","SizeBytes":10},`+
		`{"Index":1,"Name":"S01E02.mkv","SizeBytes":20}]}`, resolveCacheTTL)
	if n, ok := tb.KnownFileSize(ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)}); !ok || n != 20 {
		t.Errorf("torbox size = %d ok=%v, want 20", n, ok)
	}
	if _, ok := tb.KnownFileSize(ResolveTarget{InfoHash: repeat("f", 40)}); ok {
		t.Error("torbox claimed a size for a release it has no entry for")
	}

	target := ResolveTarget{InfoHash: H}
	rdCache := NewMemoryCache(1 << 20)
	rdCache.Put(rdTorrentKey("k", H, target), "abc", resolveCacheTTL)
	rd := &realDebridStore{token: "k", api: realDebridAPI, cache: rdCache, client: routed{
		routes: map[string]func() (*http.Response, error){
			"torrents/info": ok(`{"status":"downloaded","links":["https://rd/l"],` +
				`"files":[{"id":1,"path":"Movie.mkv","bytes":900,"selected":1}]}`),
			"unrestrict": ok(`{"download":"https://rd/final.mkv"}`),
		}}}
	if _, err := rd.Resolve(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if n, ok := rd.KnownFileSize(target); !ok || n != 900 {
		t.Errorf("realdebrid size = %d ok=%v, want 900", n, ok)
	}
}
