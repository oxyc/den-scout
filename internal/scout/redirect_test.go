package scout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// An upstream redirect must not turn one charged add into several real ones.
//
// Every add is charged once through spendAdd and then sent once — but Go's default redirect policy
// re-sends the same method AND body on a 307 or 308, up to ten hops. Measured before the guard: one
// charged add became two real POSTs on a single hop and ten on a loop, so the fifty-an-hour ceiling
// would have permitted five hundred real adds against the account. Premiumize is worse than a count:
// its apikey rides in the directdl form body, and Go replays the body across hosts even though it
// strips the Authorization header, so a 308 elsewhere hands the token to an unrelated server.
//
// The redirect target is a SEPARATE path, so what is asserted is the number of add requests that
// actually reach a second location — the thing that costs the account.
func TestRefuseRedirectReplay_anAddIsNotReplayedToTheRedirectTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"307 temporary", http.StatusTemporaryRedirect}, // preserves method + body
		{"308 permanent", http.StatusPermanentRedirect}, // preserves method + body
		{"302 found", http.StatusFound},                 // Go downgrades to GET and drops the body
	} {
		t.Run(tc.name, func(t *testing.T) {
			var firstHop, replayed int32
			mux := http.NewServeMux()
			mux.HandleFunc("/torrents/createtorrent", func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&firstHop, 1)
				http.Redirect(w, r, "/moved", tc.status)
			})
			mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					atomic.AddInt32(&replayed, 1)
				}
				_, _ = w.Write([]byte(`{"data":{"torrent_id":9}}`))
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			token, hash := "redirect-"+tc.name, repeat("a", 40)
			before := globalAddBudget.remaining(budgetAccount(ServiceTorBox, token))

			s := &torBoxStore{token: token, cache: NewMemoryCache(1 << 20), api: srv.URL,
				client: &http.Client{CheckRedirect: RefuseRedirectReplay}}
			_, _ = s.Resolve(context.Background(), ResolveTarget{InfoHash: hash})

			if got := atomic.LoadInt32(&replayed); got != 0 {
				t.Errorf("the add was re-sent to the redirect target %d time(s) — it is charged once and "+
					"delivered %d times, so the hourly ceiling guards nothing, and Premiumize's apikey "+
					"would travel with it", got, got+1)
			}
			if got := atomic.LoadInt32(&firstHop); got != 1 {
				t.Errorf("the add reached the original endpoint %d times, want 1", got)
			}
			if after := globalAddBudget.remaining(budgetAccount(ServiceTorBox, token)); before-after > 1 {
				t.Errorf("the hourly allowance moved by %d for one resolve", before-after)
			}
		})
	}
}

// A credential in the query string must not follow a redirect off the debrid's API host — Go strips the
// Authorization header across hosts but never the query. A CDN's own redirects are untouched.
func TestRefuseRedirectReplay_credentialQueryStaysOnTheDebridHost(t *testing.T) {
	req := func(raw string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	requestdl := req("https://api.torbox.app/v1/api/torrents/requestdl?token=secret&torrent_id=1")
	if err := RefuseRedirectReplay(req("https://elsewhere.example/x?token=secret"), []*http.Request{requestdl}); err != http.ErrUseLastResponse {
		t.Errorf("a redirect from the TorBox API to another host was followed (err %v) — the token goes with it", err)
	}
	if err := RefuseRedirectReplay(req("https://api.torbox.app/v1/api/other"), []*http.Request{requestdl}); err != nil {
		t.Errorf("a same-host redirect was refused: %v", err)
	}
	cacheCheck := req("https://www.premiumize.me/api/cache/check?apikey=secret")
	if err := RefuseRedirectReplay(req("https://elsewhere.example/"), []*http.Request{cacheCheck}); err != http.ErrUseLastResponse {
		t.Errorf("a redirect from Premiumize to another host was followed (err %v) — the apikey goes with it", err)
	}
	playback := req("https://store-1.tb-cdn.st/dld/abc?token=link")
	if err := RefuseRedirectReplay(req("https://store-2.tb-cdn.st/dld/abc"), []*http.Request{playback}); err != nil {
		t.Errorf("a playback link's CDN redirect was refused: %v", err)
	}
}

// GET and HEAD must still follow redirects: reading a playback link is exactly that, and the same
// client does it.
func TestRefuseRedirectReplay_readsStillFollowRedirects(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			var served int32
			mux := http.NewServeMux()
			mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&served, 1)
				w.WriteHeader(http.StatusOK)
			})
			mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/final", http.StatusFound)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			req, err := http.NewRequest(method, srv.URL+"/start", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := (&http.Client{CheckRedirect: RefuseRedirectReplay}).Do(req)
			if err != nil {
				t.Fatalf("%s: %v", method, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&served) != 1 {
				t.Errorf("%s did not follow the redirect (status %d, final hit %d) — a playback link "+
					"is read this way, and CDNs redirect", method, resp.StatusCode, served)
			}
		})
	}
}
