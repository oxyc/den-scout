package scout

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Every way a debrid can fail to hand over a link. These matter more than the happy path: a resolve that
// returns an error lets the pool fall through to another service, while one that panics, hangs, or
// returns an empty string with a nil error takes the whole request down with it. The 2026-08-20 outage
// was exactly this layer misbehaving under a service that had stopped answering.

// routed dispatches by URL substring so a multi-call flow (addMagnet → info → selectFiles → unrestrict)
// can fail at one specific step while the others behave.
type routed struct {
	routes map[string]func() (*http.Response, error)
	fallbk func() (*http.Response, error)
}

func (r routed) Do(req *http.Request) (*http.Response, error) {
	for frag, fn := range r.routes {
		if strings.Contains(req.URL.String(), frag) {
			return fn()
		}
	}
	if r.fallbk != nil {
		return r.fallbk()
	}
	return resp(200, `{}`), nil
}

func ok(body string) func() (*http.Response, error) {
	return func() (*http.Response, error) { return resp(200, body), nil }
}

func status(code int) func() (*http.Response, error) {
	return func() (*http.Response, error) { return resp(code, `{}`), nil }
}

func boom() (*http.Response, error) { return nil, errors.New("connection reset") }

func wantDead(t *testing.T, link string, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error so the pool can fall through, got link %q", what, link)
	}
	if link != "" {
		t.Errorf("%s: returned a link alongside an error: %q", what, link)
	}
}

func TestRealDebridResolve_everyStepCanFail(t *testing.T) {
	const addOK = `{"id":"abc"}`
	const infoOK = `{"files":[{"id":1,"path":"Show.S01E01.mkv","bytes":900}],"links":["https://rd/l"]}`

	cases := []struct {
		name   string
		client doer
	}{
		{"addMagnet transport error", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": boom,
		}}},
		{"addMagnet rejected", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": status(401),
		}}},
		{"addMagnet returned no id", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": ok(`{}`),
		}}},
		{"info http error", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": ok(addOK), "torrents/info": status(503),
		}}},
		{"info body is not json", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": ok(addOK), "torrents/info": ok(`<html>maintenance</html>`),
		}}},
		{"torrent has no files", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": ok(addOK), "torrents/info": ok(`{"files":[]}`),
		}}},
		{"selectFiles rejected", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": ok(addOK), "torrents/info": ok(infoOK), "selectFiles": status(403),
		}}},
		{"nothing ready to unrestrict", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": ok(addOK), "selectFiles": ok(`{}`),
			"torrents/info": ok(`{"files":[{"id":1,"path":"a.mkv","bytes":9}],"links":[]}`),
		}}},
		{"unrestrict rejected", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": ok(addOK), "torrents/info": ok(infoOK), "selectFiles": ok(`{}`),
			"unrestrict": status(429),
		}}},
		{"unrestrict returned no download url", routed{routes: map[string]func() (*http.Response, error){
			"addMagnet": ok(addOK), "torrents/info": ok(infoOK), "selectFiles": ok(`{}`),
			"unrestrict": ok(`{"download":""}`),
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &realDebridStore{token: "k", api: realDebridAPI, client: c.client}
			link, err := s.Resolve(t.Context(), ResolveTarget{InfoHash: H})
			wantDead(t, link, err, c.name)
		})
	}
}

func TestRealDebridResolve_happyPathReturnsTheUnrestrictedLink(t *testing.T) {
	s := &realDebridStore{token: "k", api: realDebridAPI, client: routed{
		routes: map[string]func() (*http.Response, error){
			"addMagnet":     ok(`{"id":"abc"}`),
			"torrents/info": ok(`{"files":[{"id":1,"path":"Show.S01E01.mkv","bytes":900}],"links":["https://rd/l"]}`),
			"selectFiles":   ok(`{}`),
			"unrestrict":    ok(`{"download":"https://rd/final.mkv"}`),
		}}}
	link, err := s.Resolve(t.Context(), ResolveTarget{InfoHash: H})
	if err != nil || link != "https://rd/final.mkv" {
		t.Fatalf("link = %q, err = %v", link, err)
	}
}

// RD refuses certain release SOURCE tags outright (web-dl, webrip, bdrip and friends). Failing fast on
// those is the point: the pool then tries another service, instead of spending an unrestrict call to
// surface a link that will not play.
func TestRealDebridResolve_blockedFilenameFailsFast(t *testing.T) {
	var unrestricted bool
	s := &realDebridStore{token: "k", api: realDebridAPI, client: routed{
		routes: map[string]func() (*http.Response, error){
			"addMagnet":     ok(`{"id":"abc"}`),
			"torrents/info": ok(`{"files":[{"id":1,"path":"` + blockedName(t) + `","bytes":900}],"links":["x"]}`),
			"unrestrict": func() (*http.Response, error) {
				unrestricted = true
				return resp(200, `{"download":"nope"}`), nil
			},
		}}}
	_, err := s.Resolve(t.Context(), ResolveTarget{InfoHash: H})
	if err == nil {
		t.Fatal("a blocked filename must not resolve")
	}
	if unrestricted {
		t.Error("it reached unrestrict — the point is to give up before spending that call")
	}
}

func TestPremiumizeResolve_failureModes(t *testing.T) {
	cases := []struct {
		name string
		fn   func() (*http.Response, error)
	}{
		{"transport error", boom},
		{"http error", status(500)},
		{"body is not json", ok(`not json at all`)},
		{"status not success", ok(`{"status":"error","content":[{"path":"a.mkv","link":"x"}]}`)},
		{"no content", ok(`{"status":"success","content":[]}`)},
		{"content carries no link", ok(`{"status":"success","content":[{"path":"a.mkv","link":""}]}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &premiumizeStore{token: "k", api: premiumizeAPI, client: routed{fallbk: c.fn}}
			link, err := s.Resolve(t.Context(), ResolveTarget{InfoHash: H})
			wantDead(t, link, err, c.name)
		})
	}

	s := &premiumizeStore{token: "k", api: premiumizeAPI, client: routed{
		fallbk: ok(`{"status":"success","content":[
			{"path":"Show.S01E01.mkv","link":"https://pm/1","size":10},
			{"path":"Show.S01E02.mkv","link":"https://pm/2","size":20}]}`)}}
	season, episode := 1, 2
	link, err := s.Resolve(t.Context(), ResolveTarget{InfoHash: H, Season: &season, Episode: &episode})
	if err != nil || link != "https://pm/2" {
		t.Fatalf("the named episode must win: link = %q, err = %v", link, err)
	}
}

// TorBox's add is the step that decides whether a release is queued at all — and a failure here is what
// left `Status` with no torrent id to report during the outage.
func TestTorBoxResolve_addFailures(t *testing.T) {
	cases := []struct {
		name string
		fn   func() (*http.Response, error)
	}{
		{"transport error", boom},
		{"rate limited", status(429)},
		{"service down", status(503)},
		{"no torrent_id in the body", ok(`{"data":{}}`)},
		{"body is not json", ok(`<html>502</html>`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &torBoxStore{token: "k", api: torboxAPI, cache: NewMemoryCache(1 << 16),
				client: routed{fallbk: c.fn}}
			link, err := s.Resolve(t.Context(), ResolveTarget{InfoHash: H})
			wantDead(t, link, err, c.name)
			// Nothing was queued, so nothing may be claimed as downloading — a false "downloading"
			// here is a wait the viewer sits through forever.
			if _, okStatus := s.Status(t.Context(), ResolveTarget{InfoHash: H}); okStatus {
				t.Error("claimed a download after the add failed")
			}
		})
	}
}

// A lost torrent id is rediscovered from the account list — but only when the account actually holds it.
func TestFindTorrentByHash_failureModes(t *testing.T) {
	cases := []struct {
		name string
		fn   func() (*http.Response, error)
	}{
		{"transport error", boom},
		{"http error", status(500)},
		{"body is not json", ok(`nope`)},
		{"account holds nothing", ok(`{"data":[]}`)},
		{"account holds a different torrent", ok(`{"data":[{"id":3,"hash":"beef"}]}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &torBoxStore{token: "k", api: torboxAPI, cache: NewMemoryCache(1 << 16),
				client: routed{fallbk: c.fn}}
			if _, found := s.findTorrentByHash(t.Context(), H); found {
				t.Errorf("%s: claimed to find a torrent id", c.name)
			}
		})
	}
}

// listFiles is best-effort: a failure yields no files rather than an error, because a pack whose file
// list can't be read is still playable by index.
func TestTorBoxListFiles_degradesToEmpty(t *testing.T) {
	for _, fn := range []func() (*http.Response, error){boom, status(500), ok(`garbage`), ok(`{"data":{}}`)} {
		s := &torBoxStore{token: "k", api: torboxAPI, client: routed{fallbk: fn}}
		if files := s.listFiles(t.Context(), 1); len(files) != 0 {
			t.Errorf("expected no files, got %d", len(files))
		}
	}
}

// A name RD's matcher rejects, asserted to actually be one so the test and the block list can't drift
// apart into a test that passes for the wrong reason.
func blockedName(t *testing.T) string {
	t.Helper()
	const name = "Show.S01E01.WEB-DL.x264.mkv"
	if !realDebridBlocked(name) {
		t.Fatalf("%q is no longer a blocked name — pick one from rdBlockSubstrings", name)
	}
	return name
}
