package scout

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripperFunc lets a test drive Deps.ProbeClient, which is a concrete *http.Client.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The limiter paces per upstream host and would otherwise make a test that checks several tokens sit
// through its cooldown. Swapped out rather than worked around, so what is under test stays the logic.
func unthrottleValidate(t *testing.T) {
	t.Helper()
	saved := validateLimiter
	validateLimiter = newHostLimiter(time.Millisecond, 10000)
	t.Cleanup(func() { validateLimiter = saved })
}

// Each service is asked the cheapest authenticated read it offers, and the verdict comes from the right
// place — which is not the status code for all of them.
func TestValidateDebridToken_perService(t *testing.T) {
	unthrottleValidate(t)

	cases := []struct {
		name      string
		svc       DebridService
		reply     *http.Response
		wantValid bool
		wantURL   string
		wantAuth  bool
	}{
		{"torbox ok", ServiceTorBox, resp(200, `{"data":{}}`), true, torboxAPI + "/user/me", true},
		{"torbox rejected", ServiceTorBox, resp(401, `{}`), false, "", true},
		{"torbox down", ServiceTorBox, resp(500, `{}`), false, "", true},
		{"realdebrid ok", ServiceRealDebrid, resp(200, `{"id":1}`), true, realDebridAPI + "/user", true},
		{"realdebrid rejected", ServiceRealDebrid, resp(403, `{}`), false, "", true},
		// Premiumize answers 200 for a bad key and puts the verdict in the body, so the status alone is
		// not the answer for every service.
		{"premiumize ok", ServicePremiumize, resp(200, `{"status":"success"}`), true, "", false},
		{"premiumize rejected", ServicePremiumize, resp(200, `{"status":"error"}`), false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotURL, gotAuth string
			client := mockDoer{func(r *http.Request) (*http.Response, error) {
				gotURL = r.URL.String()
				gotAuth = r.Header.Get("authorization")
				return tc.reply, nil
			}}
			got := validateDebridToken(context.Background(), client, tc.svc, "the-token")
			if got.Valid != tc.wantValid {
				t.Errorf("valid = %v, want %v (reason %q)", got.Valid, tc.wantValid, got.Reason)
			}
			if !got.Valid && got.Reason == "" {
				t.Error("an invalid verdict came back with no reason to show anyone")
			}
			if tc.wantURL != "" && gotURL != tc.wantURL {
				t.Errorf("asked %q, want %q", gotURL, tc.wantURL)
			}
			if tc.wantAuth && gotAuth != "Bearer the-token" {
				t.Errorf("authorization = %q", gotAuth)
			}
			// Premiumize authenticates in the query string, so it must NOT also send a bearer header.
			if !tc.wantAuth && gotAuth != "" {
				t.Errorf("premiumize sent an authorization header: %q", gotAuth)
			}
		})
	}
}

// A provider being down is not a wrong token. Telling someone their key is bad because a service was
// unreachable is worse than telling them nothing, because they will go and regenerate a working key.
func TestValidateDebridToken_unreachableIsNotInvalid(t *testing.T) {
	unthrottleValidate(t)
	client := mockDoer{func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}}
	got := validateDebridToken(context.Background(), client, ServiceTorBox, "t")
	if got.Valid {
		t.Fatal("an unreachable service reported the token as valid")
	}
	if !strings.Contains(got.Reason, "could not reach") {
		t.Errorf("reason = %q, want it to say the service could not be reached", got.Reason)
	}
	// The reason is shown to a user and must never carry the token or the upstream URL — they travel
	// together, and this is the one route holding an unsealed token.
	if strings.Contains(got.Reason, "t") && strings.Contains(got.Reason, torboxAPI) {
		t.Errorf("reason leaked the request: %q", got.Reason)
	}
}

// Refused, not queued. The route is an unauthenticated credential-testing oracle pointed at three third
// parties, and the first version made it WAIT for its token — so a flood did not slow down, it parked a
// goroutine and a connection per request for the whole budget, inside a 230 MiB heap. 429 costs nothing.
func TestValidateRoute_refusesRatherThanQueues(t *testing.T) {
	saved := validateLimiter
	validateLimiter = newHostLimiter(time.Hour, 1) // one allowed, then the allowance is gone for an hour
	t.Cleanup(func() { validateLimiter = saved })

	var upstream int
	h := NewHandler(testDeps(func(d *Deps) {
		d.ProbeClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			upstream++
			return resp(200, `{"data":{}}`), nil
		})}
	}))
	body := `{"service":"torbox","token":"tb-secret"}`

	if rr := postJSON(h, "/validate", body); rr.Code != 200 {
		t.Fatalf("first check: %d, want 200", rr.Code)
	}
	start := time.Now()
	rr := postJSON(h, "/validate", body)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second check: %d, want 429", rr.Code)
	}
	// The point of the 429 is that it is immediate — a queued request is the cost being avoided.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the refusal took %s — it queued instead of shedding", elapsed)
	}
	if rr.Header().Get("retry-after") == "" {
		t.Error("429 without a Retry-After")
	}
	if upstream != 1 {
		t.Errorf("upstream called %d times, want 1 — a refused check must not reach the provider", upstream)
	}
}

// The route: POST only, strict about what it accepts, and never cached.
func TestValidateRoute(t *testing.T) {
	unthrottleValidate(t)
	var asked int
	h := NewHandler(testDeps(func(d *Deps) {
		d.ProbeClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			asked++
			return resp(200, `{"data":{}}`), nil
		})}
	}))

	rr := postJSON(h, "/validate", `{"service":"torbox","token":"tb-secret"}`)
	if rr.Code != 200 {
		t.Fatalf("validate: %d", rr.Code)
	}
	if cc := rr.Header().Get("cache-control"); cc != noStore {
		t.Errorf("cache-control = %q, want no-store — this reply is about a secret", cc)
	}
	var got validateResult
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid {
		t.Errorf("valid token reported invalid: %+v", got)
	}

	// A GET must not reach the upstream — this route exists to spend one read, on purpose, per press.
	before := asked
	if rr := do(h, "/validate", nil); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /validate: %d, want 405", rr.Code)
	}
	if asked != before {
		t.Error("a GET reached the debrid service")
	}

	// Unknown service, empty token, oversized token and malformed JSON are all refused before any
	// upstream call — the route must not become a way to send arbitrary strings to a third party.
	before = asked
	for _, body := range []string{
		`{"service":"nope","token":"x"}`,
		`{"service":"torbox","token":""}`,
		`{"service":"torbox","token":"` + repeat("x", 513) + `"}`,
		`not json`,
	} {
		if rr := postJSON(h, "/validate", body); rr.Code != http.StatusBadRequest {
			t.Errorf("body %.40s: %d, want 400", body, rr.Code)
		}
	}
	if asked != before {
		t.Errorf("a rejected request still reached the upstream (%d calls)", asked-before)
	}
}
