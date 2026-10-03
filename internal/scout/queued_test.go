package scout

import (
	"context"
	"errors"
	"net/http"
	"path"
	"testing"
)

// Decoding `queued_id`: createtorrent answering it rather than `torrent_id` used to fall through to the
// generic "no torrent_id" DeadLinkError (stores.go, pre-den#204), which the client blacklisted a release
// over while TorBox still held the add and would start it eventually. addMagnet must read it as a
// genuine success — a torrent id of 0 is not "nothing happened".
func TestAddMagnet_queuedIdIsNotADeadLink(t *testing.T) {
	s := &torBoxStore{token: "tb-queued-addmagnet", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			return resp(200, `{"success":true,"data":{"queued_id":7}}`), nil
		}}}
	torrentID, queuedID, err := s.addMagnet(context.Background(), repeat("a", 40), false, false)
	if err != nil {
		t.Fatalf("a queued_id answer must not be an error: %v", err)
	}
	if torrentID != 0 || queuedID != 7 {
		t.Fatalf("addMagnet returned (%d, %d), want (0, 7)", torrentID, queuedID)
	}
	if id, ok := s.knownQueuedID(repeat("a", 40)); !ok || id != 7 {
		t.Errorf("knownQueuedID = (%d, %v), want (7, true)", id, ok)
	}
	if !wasAddedByUs(s.cache, s.accountIdentity(), repeat("a", 40)) {
		t.Error("a queued add must still be marked as ours — Cancel needs it")
	}
}

// Resolve answers errAddQueued, not a dead link, for a parked add — and does not re-add on the next poll.
func TestResolve_queuedAddIsPendingNotDead(t *testing.T) {
	hash := repeat("a", 40)
	createtorrentCalls := 0
	s := &torBoxStore{token: "tb-queued-resolve", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			switch path.Base(r.URL.Path) {
			case "createtorrent":
				createtorrentCalls++
				return resp(200, `{"success":true,"data":{"queued_id":7}}`), nil
			case "mylist":
				// Still parked: the account listing does not carry it yet.
				return resp(200, `{"success":true,"data":[]}`), nil
			}
			return resp(404, `{}`), nil
		}}}
	_, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: hash, FileIdx: intp(0)})
	if !errors.Is(err, errAddQueued) {
		t.Fatalf("first resolve: err = %v, want errAddQueued", err)
	}
	// A second poll must not send createtorrent again — the knownQueuedID gate takes over.
	_, err = s.Resolve(context.Background(), ResolveTarget{InfoHash: hash, FileIdx: intp(0)})
	if !errors.Is(err, errAddQueued) {
		t.Fatalf("second resolve: err = %v, want errAddQueued", err)
	}
	if createtorrentCalls != 1 {
		t.Errorf("createtorrent called %d times across two polls, want 1", createtorrentCalls)
	}
}

// Once TorBox promotes the queued add to a real torrent (the account listing now carries it), Resolve
// serves it like any other held torrent — no second add, no lingering "queued" answer.
func TestResolve_queuedAddPromotedToATorrent(t *testing.T) {
	hash := repeat("a", 40)
	s := &torBoxStore{token: "tb-queued-promoted", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			switch path.Base(r.URL.Path) {
			case "createtorrent":
				return resp(200, `{"success":true,"data":{"queued_id":7}}`), nil
			case "mylist":
				if r.URL.Query().Get("id") != "" {
					return resp(200, `{"success":true,"data":{"id":42,"download_finished":false}}`), nil
				}
				// Promoted: the account listing now carries the torrent.
				return resp(200, `{"success":true,"data":[{"id":42,"hash":"`+hash+`"}]}`), nil
			case "requestdl":
				return resp(200, `{"success":true,"data":"https://cdn.torbox/promoted.mkv"}`), nil
			}
			return resp(404, `{}`), nil
		}}}
	// First poll: parked.
	if _, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: hash, FileIdx: intp(0)}); !errors.Is(err, errAddQueued) {
		t.Fatalf("first resolve: err = %v, want errAddQueued", err)
	}
	// Second poll: promoted, and playable.
	link, err := s.Resolve(context.Background(), ResolveTarget{InfoHash: hash, FileIdx: intp(0)})
	if err != nil || link != "https://cdn.torbox/promoted.mkv" {
		t.Fatalf("resolve after promotion: link=%q err=%v", link, err)
	}
	if _, stillQueued := s.knownQueuedID(hash); stillQueued {
		t.Error("the queued marker must be cleared once promoted")
	}
}

// The add charge is NOT refunded for a queued_id answer — createtorrent genuinely accepted the add; it is
// merely parked. Refunding it would let an account loop past its own hourly allowance by polling a
// release that is, correctly, always answered as "queued".
func TestAddMagnet_queuedChargeIsNotRefunded(t *testing.T) {
	account := "tb-queued-budget-" + repeat("x", 8)
	s := &torBoxStore{token: account, api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			return resp(200, `{"success":true,"data":{"queued_id":7}}`), nil
		}}}
	before := globalAddBudget.remaining(budgetAccount(ServiceTorBox, s.accountIdentity()))
	if _, _, err := s.addMagnet(context.Background(), repeat("a", 40), false, false); err != nil {
		t.Fatalf("addMagnet: %v", err)
	}
	after := globalAddBudget.remaining(budgetAccount(ServiceTorBox, s.accountIdentity()))
	if after != before-1 {
		t.Errorf("remaining budget = %d after a queued add, want %d (charged, not refunded)", after, before-1)
	}
}

// /play answers 202 for a release TorBox queued, never 404 — den#204.
func TestPlay_queuedAddAnswers202(t *testing.T) {
	hash := repeat("b", 40)
	h, _ := cancelTorBoxHandler(func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "createtorrent":
			return resp(200, `{"success":true,"data":{"queued_id":9}}`)
		case "mylist":
			return resp(200, `{"success":true,"data":[]}`)
		}
		return resp(404, `{}`)
	}, nil)
	rr := do(h, "/"+validBlob+"/play/"+encodePlayToken(PlayTarget{InfoHash: hash, FileIdx: intp(0)}), nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("play on a queued add: %d %s, want 202", rr.Code, rr.Body.String())
	}
}
