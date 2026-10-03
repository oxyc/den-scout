package scout

import (
	"encoding/json"
	"net/http"
	"path"
	"testing"
)

// accountTorBoxHandler is a handler whose one store is a real TorBox store answering /<config>/account's
// three upstream reads (user/me, mylist, getqueued) through `reply`.
func accountTorBoxHandler(token string, reply func(r *http.Request) *http.Response) (http.Handler, *int) {
	calls := 0
	store := &torBoxStore{token: token, api: torboxAPI, cache: NewMemoryCache(1 << 20),
		client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
			calls++
			return reply(r), nil
		}}}
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store { return []Store{store} }
	}))
	return h, &calls
}

func decodeAccountBody(t *testing.T, body []byte) []map[string]any {
	var out []map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("account body did not decode as an array: %v (%s)", err, body)
	}
	return out
}

// A known plan code maps to its name, active/finished entries are told apart, and the queue length is
// the account's own queued-torrents list — not scout's add budget.
func TestAccount_knownPlan(t *testing.T) {
	h, calls := accountTorBoxHandler("tb-account-known-plan", func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "me":
			return resp(200, `{"success":true,"data":{"plan":2,"cooldown_until":null,"premium_expires_at":"2027-01-02T00:00:00Z"}}`)
		case "mylist":
			return resp(200, `{"success":true,"data":[`+
				`{"active":true,"download_finished":false},`+ // counts
				`{"active":true,"download_finished":true},`+ // finished — does not count
				`{"active":false,"download_finished":false}`+ // not active — does not count
				`]}`)
		case "getqueued":
			return resp(200, `{"success":true,"data":[{"id":1},{"id":2}]}`)
		}
		return resp(404, `{}`)
	})
	rr := do(h, "/"+validBlob+"/account", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("account: %d", rr.Code)
	}
	out := decodeAccountBody(t, rr.Body.Bytes())
	if len(out) != 1 || out[0]["service"] != "torbox" || out[0]["plan"] != "pro" {
		t.Fatalf("account body = %v", out)
	}
	slots, ok := out[0]["slots"].(map[string]any)
	if !ok || slots["active"] != float64(1) {
		t.Errorf("slots = %v, want active=1", out[0]["slots"])
	}
	if out[0]["queued"] != float64(2) {
		t.Errorf("queued = %v, want 2", out[0]["queued"])
	}
	if out[0]["expiresAt"] != "2027-01-02T00:00:00Z" {
		t.Errorf("expiresAt = %v", out[0]["expiresAt"])
	}
	if _, has := out[0]["cooldownUntil"]; has {
		t.Errorf("cooldownUntil should be omitted when null, got %v", out[0]["cooldownUntil"])
	}
	if *calls != 3 {
		t.Errorf("upstream calls = %d, want 3 (user/me, mylist, getqueued)", *calls)
	}
}

// A plan code this package does not know still answers — "unknown" rather than a guess, and no slot
// limit anywhere: den#204's owner decision is to show "N active" alone.
func TestAccount_unknownPlan(t *testing.T) {
	h, _ := accountTorBoxHandler("tb-account-unknown-plan", func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "me":
			return resp(200, `{"success":true,"data":{"plan":99}}`)
		case "mylist":
			return resp(200, `{"success":true,"data":[]}`)
		case "getqueued":
			return resp(200, `{"success":true,"data":[]}`)
		}
		return resp(404, `{}`)
	})
	rr := do(h, "/"+validBlob+"/account", nil)
	out := decodeAccountBody(t, rr.Body.Bytes())
	if len(out) != 1 || out[0]["plan"] != "unknown" {
		t.Fatalf("account body = %v, want plan=unknown", out)
	}
	if _, has := out[0]["slots"].(map[string]any)["limit"]; has {
		t.Errorf("slots.limit must never be present: %v", out[0]["slots"])
	}
}

// A refused upstream degrades to the bare service name rather than failing the whole route: the Downloads
// shelf still has something to show (scout's own budget is reported from a separate, always-answerable
// source — see TestAccount_knownPlan).
func TestAccount_refusedUpstream(t *testing.T) {
	h, _ := accountTorBoxHandler("tb-account-refused", func(r *http.Request) *http.Response {
		return resp(500, `{"error":"server_error"}`)
	})
	rr := do(h, "/"+validBlob+"/account", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("account: %d, want 200 (degraded per-account, not a route failure)", rr.Code)
	}
	out := decodeAccountBody(t, rr.Body.Bytes())
	if len(out) != 1 || out[0]["service"] != "torbox" {
		t.Fatalf("account body = %v", out)
	}
	if _, has := out[0]["plan"]; has {
		t.Errorf("a refused read must not report a plan: %v", out[0])
	}
}

// Cached for 30s per account: a second request within the window costs no further upstream reads.
func TestAccount_cacheReuseWithin30s(t *testing.T) {
	h, calls := accountTorBoxHandler("tb-account-cache-reuse", func(r *http.Request) *http.Response {
		switch path.Base(r.URL.Path) {
		case "me":
			return resp(200, `{"success":true,"data":{"plan":1}}`)
		case "mylist":
			return resp(200, `{"success":true,"data":[]}`)
		case "getqueued":
			return resp(200, `{"success":true,"data":[]}`)
		}
		return resp(404, `{}`)
	})
	do(h, "/"+validBlob+"/account", nil)
	first := *calls
	do(h, "/"+validBlob+"/account", nil)
	if *calls != first {
		t.Errorf("a second read within 30s made %d more upstream calls, want 0", *calls-first)
	}
}

// A non-TorBox account answers with only its service name — Real-Debrid and Premiumize publish nothing
// comparable.
func TestAccount_nonTorBoxService(t *testing.T) {
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeStores = func(*Config) []Store {
			return []Store{&realDebridStore{token: "rd-account", client: mockDoer{fn: func(r *http.Request) (*http.Response, error) {
				return resp(404, `{}`), nil
			}}}}
		}
	}))
	rr := do(h, "/"+validBlob+"/account", nil)
	out := decodeAccountBody(t, rr.Body.Bytes())
	if len(out) != 1 || out[0]["service"] != "realdebrid" || len(out[0]) != 1 {
		t.Fatalf("account body = %v, want only {service: realdebrid}", out)
	}
}
