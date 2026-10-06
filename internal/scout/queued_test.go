package scout

import (
	"context"
	"encoding/json"
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

// createtorrent answering 400 DIFF_ISSUE "Download already queued." for a hash with no queued-id marker
// (e.g. one added before ownership tracking existed) used to fall straight through to the generic
// DeadLinkError — a release genuinely sitting in TorBox's own queue read as dead. addMagnet must discover
// the queue entry via getqueued and report it the same way a direct queued_id answer is.
func TestAddMagnet_alreadyQueuedIsDiscoveredViaGetqueued(t *testing.T) {
	hash := repeat("a", 40)
	var calls []string
	s := &torBoxStore{token: "tb-diffissue", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			calls = append(calls, path.Base(r.URL.Path))
			switch path.Base(r.URL.Path) {
			case "createtorrent":
				return resp(400, `{"error":"DIFF_ISSUE","detail":"Download already queued."}`), nil
			case "getqueued":
				return resp(200, `{"success":true,"data":[{"id":55,"hash":"`+hash+`"}]}`), nil
			}
			return resp(404, `{}`), nil
		}}}
	torrentID, queuedID, err := s.addMagnet(context.Background(), hash, false, false)
	if err != nil {
		t.Fatalf("already-queued must not be an error: %v", err)
	}
	if torrentID != 0 || queuedID != 55 {
		t.Fatalf("addMagnet returned (%d, %d), want (0, 55)", torrentID, queuedID)
	}
	if id, ok := s.knownQueuedID(hash); !ok || id != 55 {
		t.Errorf("knownQueuedID = (%d, %v), want (55, true)", id, ok)
	}
	if !wasAddedByUs(s.cache, s.accountIdentity(), hash) {
		t.Error("a discovered queue entry must still be marked as ours — Cancel needs it")
	}
	if !sameCalls(calls, []string{"createtorrent", "getqueued"}) {
		t.Errorf("upstream calls = %v", calls)
	}
}

// A plain 400 that is NOT "already queued" — a malformed magnet, an account past its limit — must stay a
// dead link. The DIFF_ISSUE branch must not swallow every 400.
func TestAddMagnet_ordinary400StaysADeadLink(t *testing.T) {
	hash := repeat("a", 40)
	getqueuedCalls := 0
	s := &torBoxStore{token: "tb-plain400", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			if path.Base(r.URL.Path) == "getqueued" {
				getqueuedCalls++
			}
			return resp(400, `{"error":"BAD_MAGNET","detail":"Invalid magnet link."}`), nil
		}}}
	_, _, err := s.addMagnet(context.Background(), hash, false, false)
	var dead *DeadLinkError
	if !errors.As(err, &dead) {
		t.Fatalf("addMagnet err = %v, want a DeadLinkError", err)
	}
	if getqueuedCalls != 0 {
		t.Errorf("getqueued called %d times for an ordinary 400, want 0", getqueuedCalls)
	}
}

// DIFF_ISSUE is reported, but the hash is not actually in the queue listing (a stale TorBox-side report,
// or a race): addMagnet must not fabricate a queued id and must fall back to the dead-link answer.
func TestAddMagnet_alreadyQueuedButHashMissingFromListingStaysADeadLink(t *testing.T) {
	hash := repeat("a", 40)
	s := &torBoxStore{token: "tb-diffissue-miss", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			switch path.Base(r.URL.Path) {
			case "createtorrent":
				return resp(400, `{"error":"DIFF_ISSUE","detail":"Download already queued."}`), nil
			case "getqueued":
				return resp(200, `{"success":true,"data":[]}`), nil
			}
			return resp(404, `{}`), nil
		}}}
	_, _, err := s.addMagnet(context.Background(), hash, false, false)
	var dead *DeadLinkError
	if !errors.As(err, &dead) {
		t.Fatalf("addMagnet err = %v, want a DeadLinkError", err)
	}
	if _, ok := s.knownQueuedID(hash); ok {
		t.Error("no queued marker should be written when the hash was not found in the listing")
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

// StatusAnswer only ever asked mylist (the torrent listing), never TorBox's own queue — so a release
// parked in the queue with no cached marker yet (a cold hash, or one whose marker's TTL lapsed) answered
// "not fetching this" exactly like one TorBox never heard of. That is what made the probe route answer
// 404 not_queued forever for a release still genuinely downloading, once den-scout started checking only
// the active list.
func TestStatusAnswer_queuedReleaseWithNoMarkerIsFoundViaGetqueued(t *testing.T) {
	hash := repeat("d", 40)
	s := &torBoxStore{token: "tb-status-queued", api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			switch path.Base(r.URL.Path) {
			case "mylist":
				// The torrent listing: TorBox has never heard of this hash as a torrent at all.
				return resp(200, `{"success":true,"data":[]}`), nil
			case "getqueued":
				return resp(200, `{"success":true,"data":[{"id":77,"hash":"`+hash+`"}]}`), nil
			}
			return resp(404, `{}`), nil
		}}}
	status, ok := s.Status(context.Background(), ResolveTarget{InfoHash: hash})
	if !ok {
		t.Fatalf("Status ok = false, want true for a release sitting in TorBox's own queue")
	}
	if status.State != fetchQueued {
		t.Errorf("status.State = %q, want %q", status.State, fetchQueued)
	}
	if status.Service != ServiceTorBox {
		t.Errorf("status.Service = %q, want %q", status.Service, ServiceTorBox)
	}
	if _, found := s.knownQueuedID(hash); !found {
		t.Error("a discovered queue entry should be memoised so the next poll avoids another getqueued call")
	}
}

// The probe route (?probe=1) is what Home's progress bar polls, and must answer the same "coming" state
// as /play for a release parked in TorBox's queue — never 404 not_queued, which the client reads as the
// release being gone.
func TestProbe_queuedReleaseAnswers202NotNotQueued(t *testing.T) {
	hash := repeat("e", 40)
	h, _ := cancelTorBoxHandler(func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "mylist":
			return resp(200, `{"success":true,"data":[]}`)
		case "getqueued":
			return resp(200, `{"success":true,"data":[{"id":88,"hash":"`+hash+`"}]}`)
		}
		return resp(404, `{}`)
	}, nil)
	rr := do(h, "/"+validBlob+"/play/"+encodePlayToken(PlayTarget{InfoHash: hash, FileIdx: intp(0)})+"?probe=1", nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("probe on a queued release: %d %s, want 202", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("probe body did not decode: %v", err)
	}
	if body["state"] != "queued" {
		t.Errorf("probe state = %v, want %q", body["state"], "queued")
	}
	if body["service"] != string(ServiceTorBox) {
		t.Errorf("probe service = %v, want %q", body["service"], ServiceTorBox)
	}
}
