package scout

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const H = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestTorBoxCacheCheck(t *testing.T) {
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"data":{"`+H+`":{"name":"x"}}}`), nil
	}}
	s := &torBoxStore{token: "t", client: d, api: torboxAPI}
	m := s.CacheCheck(context.Background(), []string{H, other})
	if !m[H] || m[other] {
		t.Errorf("cacheCheck: %v", m)
	}
}

func TestTorBoxResolveMovie(t *testing.T) {
	var reqdl string
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "createtorrent"):
			return resp(200, `{"data":{"torrent_id":7}}`), nil
		case strings.Contains(r.URL.Path, "requestdl"):
			reqdl = r.URL.RawQuery
			return resp(200, `{"success":true,"data":"https://cdn.torbox/x.mkv"}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			t.Error("movie should not list files")
		}
		return resp(404, "{}"), nil
	}}
	s := &torBoxStore{token: "t", client: d, api: torboxAPI}
	link, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)})
	if err != nil || link != "https://cdn.torbox/x.mkv" {
		t.Fatalf("resolve: %q err=%v", link, err)
	}
	if !strings.Contains(reqdl, "file_id=0") {
		t.Errorf("requestdl missing file_id: %s", reqdl)
	}
}

func TestTorBoxBingeCacheAndNoPoison(t *testing.T) {
	// binge: 2 episodes on the same pack → createtorrent + mylist once.
	creates, lists, dls := 0, 0, 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "createtorrent"):
			creates++
			return resp(200, `{"data":{"torrent_id":9}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			lists++
			return resp(200, `{"data":{"files":[{"id":0,"name":"S01E01.mkv","size":10},{"id":1,"name":"S01E02.mkv","size":20}]}}`), nil
		case strings.Contains(r.URL.Path, "requestdl"):
			dls++
			return resp(200, `{"success":true,"data":"https://cdn/`+r.URL.Query().Get("file_id")+`"}`), nil
		}
		return resp(404, "{}"), nil
	}}
	s := &torBoxStore{token: "t", client: d, cache: NewMemoryCache(1 << 20), api: torboxAPI}
	ep1, _ := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(1)})
	ep2, _ := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if ep1 != "https://cdn/0" || ep2 != "https://cdn/1" {
		t.Errorf("binge picks: ep1=%q ep2=%q", ep1, ep2)
	}
	if creates != 1 || lists != 1 || dls != 2 {
		t.Errorf("binge cache: creates=%d lists=%d dls=%d (want 1,1,2)", creates, lists, dls)
	}

	// no-poison (#3): a failed mylist must not cache files:[] and mis-serve later episodes.
	lists2 := 0
	d2 := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "createtorrent"):
			return resp(200, `{"data":{"torrent_id":9}}`), nil
		case strings.Contains(r.URL.Path, "mylist"):
			lists2++
			return resp(500, "boom"), nil // empty file list
		case strings.Contains(r.URL.Path, "requestdl"):
			return resp(200, `{"success":true,"data":"https://cdn/x"}`), nil
		}
		return resp(404, "{}"), nil
	}}
	s2 := &torBoxStore{token: "t", client: d2, cache: NewMemoryCache(1 << 20), api: torboxAPI}
	_, _ = s2.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(1)})
	_, _ = s2.Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if lists2 != 2 {
		t.Errorf("no-poison: mylist should be retried (got %d, want 2)", lists2)
	}
}

func TestTorBoxDeadLink(t *testing.T) {
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "createtorrent") {
			return resp(200, `{"data":{"torrent_id":7}}`), nil
		}
		return resp(200, `{"success":false}`), nil
	}}
	s := &torBoxStore{token: "t", client: d, api: torboxAPI}
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: H, FileIdx: intp(0)}); err == nil {
		t.Error("expected DeadLinkError")
	}
}

func TestRealDebrid(t *testing.T) {
	m := (&realDebridStore{}).CacheCheck(context.Background(), []string{H})
	if m[H] {
		t.Error("RD cacheCheck should be all-false")
	}

	infoCalls := 0
	d := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "addMagnet"):
			return resp(201, `{"id":"t1"}`), nil
		case strings.Contains(r.URL.Path, "/torrents/info/"):
			infoCalls++
			if infoCalls == 1 {
				return resp(200, `{"files":[{"id":1,"path":"/movie.mkv","bytes":999}],"links":[]}`), nil
			}
			return resp(200, `{"files":[{"id":1,"path":"/movie.mkv","bytes":999}],"links":["https://rd/restricted"]}`), nil
		case strings.Contains(r.URL.Path, "selectFiles"):
			return resp(204, ""), nil
		case strings.Contains(r.URL.Path, "unrestrict/link"):
			return resp(200, `{"download":"https://rd/dl.mkv"}`), nil
		}
		return resp(404, "{}"), nil
	}}
	link, err := (&realDebridStore{token: "t", client: d, api: realDebridAPI}).Resolve(context.Background(), ResolveTarget{InfoHash: H})
	if err != nil || link != "https://rd/dl.mkv" {
		t.Fatalf("RD resolve: %q err=%v", link, err)
	}

	// blocked filename → DeadLinkError (pool falls through)
	db := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "addMagnet") {
			return resp(201, `{"id":"t1"}`), nil
		}
		return resp(200, `{"files":[{"id":1,"path":"Movie.WEB-DL.x264.mkv","bytes":999}],"links":[]}`), nil
	}}
	if _, err := (&realDebridStore{token: "t", client: db, api: realDebridAPI}).Resolve(context.Background(), ResolveTarget{InfoHash: H}); err == nil {
		t.Error("RD-blocked filename should error")
	}
}

func TestPremiumize(t *testing.T) {
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dc := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"status":"success","response":[true,false]}`), nil
	}}
	m := (&premiumizeStore{token: "t", client: dc, api: premiumizeAPI}).CacheCheck(context.Background(), []string{H, other})
	if !m[H] || m[other] {
		t.Errorf("PM cacheCheck: %v", m)
	}
	dr := mockDoer{fn: func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"status":"success","content":[{"path":"S01E01.mkv","link":"https://pm/1","size":10},{"path":"S01E02.mkv","link":"https://pm/2","size":20}]}`), nil
	}}
	link, err := (&premiumizeStore{token: "t", client: dr, api: premiumizeAPI}).Resolve(context.Background(), ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(2)})
	if err != nil || link != "https://pm/2" {
		t.Errorf("PM resolve: %q err=%v", link, err)
	}
}

type fakeStore struct {
	svc     DebridService
	check   map[string]bool
	resolve func() (string, error)
}

func (f fakeStore) Service() DebridService                                 { return f.svc }
func (f fakeStore) CacheCheck(context.Context, []string) map[string]bool   { return f.check }
func (f fakeStore) Resolve(context.Context, ResolveTarget) (string, error) { return f.resolve() }

func TestStorePool(t *testing.T) {
	// buildStores orders TorBox first regardless of config order
	cfg := &Config{Debrid: []DebridAccount{{ServicePremiumize, "p"}, {ServiceTorBox, "t"}, {ServiceRealDebrid, "r"}}}
	stores := buildStores(cfg, mockDoer{}, nil)
	if len(stores) != 3 || stores[0].Service() != ServiceTorBox || stores[2].Service() != ServicePremiumize {
		t.Errorf("store order: %v", []DebridService{stores[0].Service(), stores[1].Service(), stores[2].Service()})
	}

	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pool := &StorePool{stores: []Store{
		fakeStore{svc: ServiceTorBox, check: map[string]bool{H: true, other: false}},
		fakeStore{svc: ServicePremiumize, check: map[string]bool{H: false, other: true}},
	}}
	m := pool.CacheCheck(context.Background(), []string{H, other})
	if !m[H] || !m[other] {
		t.Errorf("pool union: %v", m)
	}

	deadOnly := &StorePool{stores: []Store{fakeStore{resolve: func() (string, error) { return "", &DeadLinkError{"x"} }}}}
	if _, err := deadOnly.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err == nil {
		t.Error("all-fail should error")
	}
	fallthr := &StorePool{stores: []Store{
		fakeStore{resolve: func() (string, error) { return "", &DeadLinkError{"x"} }},
		fakeStore{resolve: func() (string, error) { return "https://ok", nil }},
	}}
	if link, err := fallthr.Resolve(context.Background(), ResolveTarget{InfoHash: H}); err != nil || link != "https://ok" {
		t.Errorf("fallthrough: %q err=%v", link, err)
	}
}

func TestHasCacheTruthAndRDOnly(t *testing.T) {
	if hasCacheTruth(&Config{Debrid: []DebridAccount{{ServiceRealDebrid, "r"}}}) {
		t.Error("RD-only should have no cache truth")
	}
	if !hasCacheTruth(&Config{Debrid: []DebridAccount{{ServiceRealDebrid, "r"}, {ServiceTorBox, "t"}}}) {
		t.Error("torbox present → cache truth")
	}
	if !rdOnly(&Config{Debrid: []DebridAccount{{ServiceRealDebrid, "r"}}}) {
		t.Error("rd-only")
	}
	if rdOnly(&Config{Debrid: []DebridAccount{{ServiceRealDebrid, "r"}, {ServiceTorBox, "t"}}}) {
		t.Error("not rd-only when torbox present")
	}
}
