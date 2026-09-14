package scout

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
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
			got, reason := checkLink(t.Context(), srv.Client(), srv.URL+"/f.mkv", tc.wantSize, 0)
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
	if got, _ := checkLink(t.Context(), http.DefaultClient, closedURL+"/f.mkv", 0, 0); got != linkUnverified {
		t.Errorf("a refused connection gave verdict %d, want unverified", got)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if got, _ := checkLink(ctx, slow.Client(), slow.URL+"/f.mkv", 0, 0); got != linkUnverified {
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

// sizedCDN serves each path in `totals` as a file of that many bytes, answering the one-byte range the link
// check asks for. Any other path answers 200 with no Content-Range.
func sizedCDN(t *testing.T, totals map[string]int64) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "video/x-matroska")
		if n, ok := totals[r.URL.Path]; ok {
			w.Header().Set("content-range", "bytes 0-0/"+strconv.FormatInt(n, 10))
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write([]byte{0})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The release size condemns a link only where nothing better is known: a movie, whose file size no store
// has, served at under half the release.
func TestVerifyLink_wrongFileOnlyForAMovieWhoseFileSizeIsUnknown(t *testing.T) {
	cdn := sizedCDN(t, map[string]int64{"/sample.mkv": 522})
	h := &handler{deps: Deps{ProbeClient: cdn.Client()}}
	unknown := &StorePool{stores: []Store{&torBoxStore{token: "t", cache: NewMemoryCache(1 << 20), api: torboxAPI}}}
	cache := NewMemoryCache(1 << 20)
	cache.Put(resolveKey("t", H), `{"torrentId":9,"files":[{"Index":0,"Name":"Movie.mkv","SizeBytes":522}]}`,
		resolveCacheTTL)
	listed := &StorePool{stores: []Store{&torBoxStore{token: "t", cache: cache, api: torboxAPI}}}
	const release = 16_000_000_000
	for _, tc := range []struct {
		name string
		pool *StorePool
		rt   ResolveTarget
		path string
		want linkVerdict
	}{
		{"a movie whose file size is unknown", unknown,
			ResolveTarget{InfoHash: H, FileIdx: intp(0), ReleaseSize: release}, "/sample.mkv", linkWrongFile},
		{"no release size", unknown, ResolveTarget{InfoHash: H, FileIdx: intp(0)}, "/sample.mkv", linkPlayable},
		{"an episode", unknown,
			ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2), ReleaseSize: release}, "/sample.mkv", linkPlayable},
		{"the file size is known", listed,
			ResolveTarget{InfoHash: H, FileIdx: intp(0), ReleaseSize: release}, "/sample.mkv", linkPlayable},
		{"at least half the release", unknown,
			ResolveTarget{InfoHash: H, FileIdx: intp(0), ReleaseSize: 1000}, "/sample.mkv", linkPlayable},
		{"no total served", unknown,
			ResolveTarget{InfoHash: H, FileIdx: intp(0), ReleaseSize: release}, "/whole.mkv", linkPlayable},
	} {
		if got := h.verifyLink(t.Context(), tc.pool, tc.rt, cdn.URL+tc.path); got != tc.want {
			t.Errorf("%s: verdict %d, want %d", tc.name, got, tc.want)
		}
	}
}

type tbFile struct {
	id   int
	name string
	size int64
}

// tbTorrent answers as TorBox holding torrent 42 with `files`: mylist lists them, and requestdl links each
// file id to `<cdn>/<id>.mkv`.
func tbTorrent(cdn string, files []tbFile) func(*http.Request) *http.Response {
	return func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "requestdl":
			return resp(200, `{"success":true,"data":"`+cdn+"/"+r.URL.Query().Get("file_id")+`.mkv"}`)
		case "mylist":
			var list []string
			for _, f := range files {
				list = append(list, fmt.Sprintf(`{"id":%d,"name":%q,"size":%d}`, f.id, f.name, f.size))
			}
			return resp(200, `{"success":true,"data":{"id":42,"files":[`+strings.Join(list, ",")+`]}}`)
		}
		return resp(404, `{}`)
	}
}

// sizedTicketPlay is a /play handler over a held TorBox torrent whose links the check can reach, and the
// ticket path for `target`.
func sizedTicketPlay(t *testing.T, cdn *httptest.Server, files []tbFile, target PlayTarget) (http.Handler, *[]string, string) {
	kr := ticketKeyring(t)
	h, calls := heldTorBoxPlay(target.InfoHash, tbTorrent(cdn.URL, files), func(d *Deps) {
		d.ProbeClient = cdn.Client()
		d.SealKeyring = kr
	})
	ticket := newTicketKeys(kr).mint(&Config{Debrid: []DebridAccount{{ServiceTorBox, "tb-secret"}}}, target,
		time.Now().Add(time.Hour))
	return h, calls, "/p/" + ticket
}

// The live failure: TorBox lists a remux's sample first, so the indexer's position 0 sent raw as a file id
// served 522 bytes of a 16 GB release. /play lists the torrent once, serves the feature, and the list it
// kept lets the next mint pick the feature with no list call.
func TestPlay_aMovieLinkToAnotherFileIsReResolvedWithTheFileList(t *testing.T) {
	cdn := sizedCDN(t, map[string]int64{"/0.mkv": 522, "/1.mkv": 16_000_000_000})
	h, calls, play := sizedTicketPlay(t, cdn,
		[]tbFile{{0, "Movie/Sample/sample.mkv", 522}, {1, "Movie/Movie.2160p.REMUX.mkv", 16_000_000_000}},
		PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0), ReleaseSize: 16_000_000_000})

	rr := do(h, play, nil)
	if rr.Code != http.StatusFound || rr.Header().Get("location") != cdn.URL+"/1.mkv" {
		t.Fatalf("play: %d %q, want 302 to the feature", rr.Code, rr.Header().Get("location"))
	}
	if want := []string{"requestdl", "mylist", "requestdl"}; !sameCalls(*calls, want) {
		t.Errorf("upstream calls = %v, want %v", *calls, want)
	}

	*calls = nil
	rr = do(h, play+"?fresh=1", nil)
	if rr.Code != http.StatusFound || rr.Header().Get("location") != cdn.URL+"/1.mkv" {
		t.Fatalf("second play: %d %q, want 302 to the feature", rr.Code, rr.Header().Get("location"))
	}
	if want := []string{"requestdl"}; !sameCalls(*calls, want) {
		t.Errorf("second play calls = %v, want %v — the list should have been kept", *calls, want)
	}
}

// A link of the release's size is the normal case, and costs the one link request it always did.
func TestPlay_aMovieLinkOfTheReleaseSizeCostsNothingMore(t *testing.T) {
	cdn := sizedCDN(t, map[string]int64{"/0.mkv": 16_000_000_000})
	h, calls, play := sizedTicketPlay(t, cdn, nil,
		PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0), ReleaseSize: 16_000_000_000})
	if rr := do(h, play, nil); rr.Code != http.StatusFound || rr.Header().Get("location") != cdn.URL+"/0.mkv" {
		t.Fatalf("play: %d %q", rr.Code, rr.Header().Get("location"))
	}
	if want := []string{"requestdl"}; !sameCalls(*calls, want) {
		t.Errorf("upstream calls = %v, want %v", *calls, want)
	}
}

// A collection's film is smaller than half the pack an indexer may report. The first check cannot tell that
// from a wrong file; the list can, and the film plays against its own listed size.
func TestPlay_aCollectionFilmSmallerThanHalfTheReleaseStillPlays(t *testing.T) {
	cdn := sizedCDN(t, map[string]int64{"/0.mkv": 9_000_000_000, "/1.mkv": 10_000_000_000, "/2.mkv": 11_000_000_000})
	h, calls, play := sizedTicketPlay(t, cdn,
		[]tbFile{{0, "Trilogy/Part.1.mkv", 9_000_000_000}, {1, "Trilogy/Part.2.mkv", 10_000_000_000},
			{2, "Trilogy/Part.3.mkv", 11_000_000_000}},
		PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0), ReleaseSize: 30_000_000_000})
	if rr := do(h, play, nil); rr.Code != http.StatusFound || rr.Header().Get("location") != cdn.URL+"/0.mkv" {
		t.Fatalf("play: %d %q, want 302 to part 1", rr.Code, rr.Header().Get("location"))
	}
	if want := []string{"requestdl", "mylist", "requestdl"}; !sameCalls(*calls, want) {
		t.Errorf("upstream calls = %v, want %v", *calls, want)
	}
}

// A re-resolve that cannot list serves the first link, as /play did before the release size was known: the
// size alone does not prove the file wrong (a collection's film, a store with no list), so it is not refused.
func TestPlay_aWrongFileWhoseReResolveFailsServesTheFirstLink(t *testing.T) {
	cdn := sizedCDN(t, map[string]int64{"/0.mkv": 522})
	hash := repeat("a", 40)
	kr := ticketKeyring(t)
	listing := tbTorrent(cdn.URL, nil)
	h, calls := heldTorBoxPlay(hash, func(r *http.Request) *http.Response {
		if path.Base(r.URL.Path) == "mylist" {
			return resp(500, "boom")
		}
		return listing(r)
	}, func(d *Deps) { d.ProbeClient = cdn.Client(); d.SealKeyring = kr })
	play := "/p/" + newTicketKeys(kr).mint(&Config{Debrid: []DebridAccount{{ServiceTorBox, "tb-secret"}}},
		PlayTarget{InfoHash: hash, FileIdx: intp(0), ReleaseSize: 16_000_000_000}, time.Now().Add(time.Hour))

	rr := do(h, play, nil)
	if rr.Code != http.StatusFound || rr.Header().Get("location") != cdn.URL+"/0.mkv" {
		t.Fatalf("play: %d %q, want 302 to the first link", rr.Code, rr.Header().Get("location"))
	}
	if want := []string{"requestdl", "mylist"}; !sameCalls(*calls, want) {
		t.Errorf("upstream calls = %v, want %v", *calls, want)
	}
}
