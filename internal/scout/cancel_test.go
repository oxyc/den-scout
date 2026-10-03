package scout

import (
	"net/http"
	"path"
	"strings"
	"testing"
)

// cancelTorBoxHandler is a handler whose one store is a real TorBox store, answering upstream through
// `reply`. `prime` runs against the cache before any request is made, so a test can mark a hash as
// scout's own add (or leave it unmarked, for the "not ours" case) and seed the torrent id the way Resolve
// or Status would have.
func cancelTorBoxHandler(reply func(r *http.Request) *http.Response, prime func(cache Cache)) (http.Handler, *[]string) {
	var calls []string
	cache := NewMemoryCache(1 << 20)
	if prime != nil {
		prime(cache)
	}
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

func cancelPath(hash string) string {
	return "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: hash, FileIdx: intp(0)})
}

// 204: scout's own torrent, not finished, TorBox accepts the delete — and the memo/marker it leaves
// behind is gone afterwards, so a second cancel of the same hash finds nothing left to cancel.
func TestCancel_oursAndUnfinished(t *testing.T) {
	hash := repeat("a", 40)
	h, calls := cancelTorBoxHandler(func(r *http.Request) *http.Response {
		switch {
		case path.Base(r.URL.Path) == "mylist" && r.URL.Query().Get("id") != "":
			return resp(200, `{"success":true,"data":{"id":42,"download_finished":false,"download_state":"downloading"}}`)
		case path.Base(r.URL.Path) == "mylist":
			// The account listing (no `id`): empty, as it is once the delete below has taken effect.
			return resp(200, `{"success":true,"data":[]}`)
		case path.Base(r.URL.Path) == "controltorrent":
			return resp(200, `{"success":true}`)
		}
		return resp(404, `{}`)
	}, func(cache Cache) {
		cache.Put(torrentIDKey("tb-secret", hash), "42", resolveCacheTTL)
		markAddedByUs(cache, "tb-secret", hash)
	})
	rr := doMethod(h, http.MethodDelete, cancelPath(hash), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d, want 204", rr.Code)
	}
	if !sameCalls(*calls, []string{"mylist", "controltorrent"}) {
		t.Errorf("upstream calls = %v", *calls)
	}
	// The memo and marker must be gone: a second cancel of the same hash finds no known torrent at all
	// (mylist now answers empty, as TorBox's own account would once the delete has taken effect).
	rr2 := doMethod(h, http.MethodDelete, cancelPath(hash), nil)
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("cancelling the same hash again: %d, want 404 (memo should be cleared)", rr2.Code)
	}
}

// 409 finished: a completed download holds no slot and is kept, never deleted.
func TestCancel_finished(t *testing.T) {
	hash := repeat("a", 40)
	h, calls := cancelTorBoxHandler(func(r *http.Request) *http.Response {
		if path.Base(r.URL.Path) == "mylist" {
			return resp(200, `{"success":true,"data":{"id":42,"download_finished":true}}`)
		}
		return resp(404, `{}`)
	}, func(cache Cache) {
		cache.Put(torrentIDKey("tb-secret", hash), "42", resolveCacheTTL)
		markAddedByUs(cache, "tb-secret", hash)
	})
	rr := doMethod(h, http.MethodDelete, cancelPath(hash), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("cancel finished: %d, want 409", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "finished") {
		t.Errorf("body = %s, want an error naming \"finished\"", rr.Body.String())
	}
	for _, c := range *calls {
		if c == "controltorrent" {
			t.Fatalf("a finished torrent must never be deleted: calls = %v", *calls)
		}
	}
}

// 409 not_ours: the account held this hash before scout ever added it (no addedByUs marker).
func TestCancel_notOurs(t *testing.T) {
	hash := repeat("a", 40)
	h, _ := cancelTorBoxHandler(func(r *http.Request) *http.Response {
		return resp(404, `{}`)
	}, func(cache Cache) {
		cache.Put(torrentIDKey("tb-secret", hash), "42", resolveCacheTTL)
		// Deliberately no markAddedByUs: this torrent id was discovered, not added by this process.
	})
	rr := doMethod(h, http.MethodDelete, cancelPath(hash), nil)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "not_ours") {
		t.Fatalf("cancel not-ours: %d %s, want 409 not_ours", rr.Code, rr.Body.String())
	}
}

// 404 not_queued: nothing of ours to cancel — no known torrent id, and TorBox's own listing agrees.
func TestCancel_notQueued(t *testing.T) {
	hash := repeat("a", 40)
	h, _ := cancelTorBoxHandler(func(r *http.Request) *http.Response {
		return resp(200, `{"success":true,"data":[]}`)
	}, nil)
	rr := doMethod(h, http.MethodDelete, cancelPath(hash), nil)
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "not_queued") {
		t.Fatalf("cancel nothing queued: %d %s, want 404 not_queued", rr.Code, rr.Body.String())
	}
}

// 503: TorBox refuses the delete itself (a throttle or a fault), not a verdict about the release.
func TestCancel_refused(t *testing.T) {
	hash := repeat("a", 40)
	h, _ := cancelTorBoxHandler(func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "mylist":
			return resp(200, `{"success":true,"data":{"id":42,"download_finished":false}}`)
		case "controltorrent":
			return resp(500, `{"error":"server_error"}`)
		}
		return resp(404, `{}`)
	}, func(cache Cache) {
		cache.Put(torrentIDKey("tb-secret", hash), "42", resolveCacheTTL)
		markAddedByUs(cache, "tb-secret", hash)
	})
	rr := doMethod(h, http.MethodDelete, cancelPath(hash), nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancel refused: %d, want 503", rr.Code)
	}
}

// 501 unsupported: Real-Debrid and Premiumize have no Cancel at all.
func TestCancel_unsupportedStore(t *testing.T) {
	hash := repeat("a", 40)
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store {
			return []Store{&realDebridStore{token: "rd-secret", client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				return resp(404, `{}`), nil
			}}}}
		}
	}))
	rr := doMethod(h, http.MethodDelete, cancelPath(hash), nil)
	if rr.Code != http.StatusNotImplemented || !strings.Contains(rr.Body.String(), "unsupported") {
		t.Fatalf("cancel on an unsupported store: %d %s, want 501 unsupported", rr.Code, rr.Body.String())
	}
}

// ?op=reannounce: the same safety rules, but TorBox is asked to reannounce rather than delete, and the
// memo is NOT cleared — the torrent is still ours and still in flight.
func TestCancel_reannounce(t *testing.T) {
	hash := repeat("a", 40)
	deleted := false
	h, calls := cancelTorBoxHandler(func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "mylist":
			return resp(200, `{"success":true,"data":{"id":42,"download_finished":false,"download_state":"stalled (no seeds)","seeds":0}}`)
		case "controltorrent":
			deleted = true
			return resp(200, `{"success":true}`)
		}
		return resp(404, `{}`)
	}, func(cache Cache) {
		cache.Put(torrentIDKey("tb-secret", hash), "42", resolveCacheTTL)
		markAddedByUs(cache, "tb-secret", hash)
	})
	rr := doMethod(h, http.MethodDelete, cancelPath(hash)+"?op=reannounce", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("reannounce: %d, want 202", rr.Code)
	}
	if !sameCalls(*calls, []string{"mylist", "controltorrent"}) {
		t.Errorf("upstream calls = %v", *calls)
	}
	if !deleted {
		t.Fatal("precondition failed: controltorrent was never called")
	}
	// The memo must survive a reannounce: a later plain cancel still finds the torrent ours.
	*calls = nil
	rr2 := doMethod(h, http.MethodDelete, cancelPath(hash), nil)
	if rr2.Code != http.StatusNoContent {
		t.Fatalf("cancel after reannounce: %d, want 204 (the memo must have survived the reannounce)", rr2.Code)
	}
}
