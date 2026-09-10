package scout

import (
	"net/http"
	"path"
	"testing"
)

// heldTorBoxPlay is a handler whose one store is a real TorBox store already holding `hash` as torrent 42,
// answering upstream through `reply`. Every upstream call is recorded by its endpoint name, in order.
func heldTorBoxPlay(hash string, reply func(r *http.Request) *http.Response) (http.Handler, *[]string) {
	var calls []string
	cache := NewMemoryCache(1 << 20)
	cache.Put(torrentIDKey("tb-secret", hash), "42", resolveCacheTTL)
	store := &torBoxStore{token: "tb-secret", cache: cache, api: torboxAPI,
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			calls = append(calls, path.Base(r.URL.Path))
			return reply(r), nil
		}}}
	h := NewHandler(testDeps(func(d *Deps) {
		d.Cache = cache
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	return h, &calls
}

func sameCalls(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A play of a release the account already holds resolves straight away: no status read first, and no add.
// The status read was a mylist round trip that, for a finished torrent, only ever said "not downloading".
func TestPlay_aHeldReleaseSkipsTheStatusRead(t *testing.T) {
	hash := repeat("a", 40)
	for _, tc := range []struct {
		name   string
		target PlayTarget
		want   []string
	}{
		// Was mylist (status) + requestdl.
		{"movie", PlayTarget{InfoHash: hash, FileIdx: intp(0)}, []string{"requestdl"}},
		// Was mylist (status) + mylist (file list) + requestdl.
		{"episode", PlayTarget{InfoHash: hash, Season: intp(1), Episode: intp(2)}, []string{"mylist", "requestdl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, calls := heldTorBoxPlay(hash, func(r *http.Request) *http.Response {
				switch path.Base(r.URL.Path) {
				case "requestdl":
					return resp(200, `{"success":true,"data":"https://cdn.torbox/held.mkv"}`)
				case "mylist":
					return resp(200, `{"success":true,"data":{"id":42,"files":[`+
						`{"id":0,"name":"Show.S01E01.mkv","size":10},{"id":1,"name":"Show.S01E02.mkv","size":20}]}}`)
				}
				return resp(404, `{}`)
			})
			rr := do(h, "/"+validBlob+"/play/"+encodePlayToken(tc.target), nil)
			if rr.Code != http.StatusFound || rr.Header().Get("location") != "https://cdn.torbox/held.mkv" {
				t.Fatalf("play: %d %q", rr.Code, rr.Header().Get("location"))
			}
			if !sameCalls(*calls, tc.want) {
				t.Errorf("upstream calls = %v, want %v", *calls, tc.want)
			}
		})
	}
}

// When the read-only resolve cannot serve, /play runs exactly what it ran before: the status read, and
// then the resolve that may add.
func TestPlay_aHeldResolveThatFailsFallsBackToTheOldSequence(t *testing.T) {
	hash := repeat("a", 40)
	requestdls := 0
	h, calls := heldTorBoxPlay(hash, func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "requestdl":
			requestdls++
			if requestdls == 1 {
				return resp(200, `{"success":false}`) // "no link yet"
			}
			return resp(200, `{"success":true,"data":"https://cdn.torbox/second.mkv"}`)
		case "mylist":
			return resp(200, `{"success":true,"data":{"id":42,"progress":1,"download_finished":true}}`)
		}
		return resp(404, `{}`)
	})
	rr := do(h, "/"+validBlob+"/play/"+encodePlayToken(PlayTarget{InfoHash: hash, FileIdx: intp(0)}), nil)
	if rr.Code != http.StatusFound || rr.Header().Get("location") != "https://cdn.torbox/second.mkv" {
		t.Fatalf("play: %d %q", rr.Code, rr.Header().Get("location"))
	}
	if want := []string{"requestdl", "mylist", "requestdl"}; !sameCalls(*calls, want) {
		t.Errorf("upstream calls = %v, want %v (read-only try, then status, then the old resolve)", *calls, want)
	}
}

// NoAdd is what keeps the new first step add-free, and this is the case where it has to: the id the cache
// remembers is stale, so the only way to a link is to buy the torrent again. The read-only step must
// decline and leave that to the old sequence, which reads status before it adds.
func TestPlay_theHeldStepNeverAddsEvenForAStaleID(t *testing.T) {
	hash := repeat("a", 40)
	h, calls := heldTorBoxPlay(hash, func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "requestdl":
			if r.URL.Query().Get("torrent_id") == "42" {
				return resp(404, `{}`) // TorBox no longer has torrent 42
			}
			return resp(200, `{"success":true,"data":"https://cdn.torbox/rebought.mkv"}`)
		case "mylist":
			return resp(200, `{"success":true,"data":[]}`)
		case "createtorrent":
			return resp(200, `{"success":true,"data":{"torrent_id":43}}`)
		}
		return resp(404, `{}`)
	})
	do(h, "/"+validBlob+"/play/"+encodePlayToken(PlayTarget{InfoHash: hash, FileIdx: intp(0)}), nil)

	if len(*calls) == 0 || (*calls)[0] != "requestdl" {
		t.Fatalf("upstream calls = %v, want the read-only resolve first", *calls)
	}
	for _, c := range *calls {
		if c == "mylist" {
			break
		}
		if c == "createtorrent" {
			t.Fatalf("added before any status read: %v — the held step spent an add", *calls)
		}
	}
}

// A download in progress keeps its one-call poll. The held step would cost a failing link request on
// every poll, so once /play has answered "downloading" it goes straight to the status read until the
// wait is over.
func TestPlay_aDownloadInProgressIsPolledWithTheStatusReadAlone(t *testing.T) {
	hash := repeat("a", 40)
	h, calls := heldTorBoxPlay(hash, func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "requestdl":
			return resp(200, `{"success":false}`)
		case "mylist":
			return resp(200, `{"success":true,"data":{"id":42,"progress":0.5,"download_finished":false}}`)
		}
		return resp(404, `{}`)
	})
	playPath := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: hash, FileIdx: intp(0)})

	if rr := do(h, playPath, nil); rr.Code != http.StatusAccepted {
		t.Fatalf("first poll: %d, want 202", rr.Code)
	}
	if want := []string{"requestdl", "mylist"}; !sameCalls(*calls, want) {
		t.Errorf("first poll calls = %v, want %v", *calls, want)
	}
	*calls = nil
	if rr := do(h, playPath, nil); rr.Code != http.StatusAccepted {
		t.Fatalf("second poll: %d, want 202", rr.Code)
	}
	if want := []string{"mylist"}; !sameCalls(*calls, want) {
		t.Errorf("second poll calls = %v, want %v", *calls, want)
	}
}
