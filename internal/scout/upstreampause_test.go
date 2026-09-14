package scout

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type countingTransport struct {
	calls  int
	answer func(*http.Request) *http.Response
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls++
	return c.answer(req), nil
}

func answering(status int, header http.Header) func(*http.Request) *http.Response {
	return func(req *http.Request) *http.Response {
		if header == nil {
			header = http.Header{}
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}
	}
}

func get(t *testing.T, rt http.RoundTripper, url string, header http.Header) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

// A 429 with Retry-After closes that upstream to every later request for as long as it asked, answered locally
// with the time left; an answer after the pause ends it.
func TestPauseOnRefusal_waitsOutARetryAfterForEveryRequest(t *testing.T) {
	next := &countingTransport{answer: answering(http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}})}
	rt := PauseOnRefusal(next).(*pausingTransport)
	clock := time.Now()
	rt.now = func() time.Time { return clock }

	if resp := get(t, rt, "https://torrentio.example/stream/movie/tt1.json", nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("first answer = %d", resp.StatusCode)
	}
	paused := get(t, rt, "https://torrentio.example/stream/movie/tt2.json", nil)
	if next.calls != 1 {
		t.Fatalf("asked the upstream %d times inside its Retry-After", next.calls)
	}
	if paused.StatusCode != http.StatusTooManyRequests || paused.Header.Get("Retry-After") != "120" ||
		paused.Header.Get("X-Scout-Paused") != "1" {
		t.Errorf("paused answer = %d %v", paused.StatusCode, paused.Header)
	}
	if get(t, rt, "https://other.example/x", nil); next.calls != 2 {
		t.Error("a pause on one host held back another")
	}

	clock = clock.Add(121 * time.Second)
	next.answer = answering(http.StatusOK, nil)
	if resp := get(t, rt, "https://torrentio.example/stream/movie/tt3.json", nil); resp.StatusCode != http.StatusOK || next.calls != 3 {
		t.Errorf("after the pause: %d, %d calls", resp.StatusCode, next.calls)
	}
	if rt.left("torrentio.example") > 0 {
		t.Error("an answer did not end the pause")
	}
}

// One account's rate limit pauses that account, not every account on the same debrid API.
func TestPauseOnRefusal_pausesTheAccountNotTheAPI(t *testing.T) {
	next := &countingTransport{answer: answering(http.StatusTooManyRequests, nil)}
	rt := PauseOnRefusal(next)
	get(t, rt, "https://api.torbox.example/v1/api/torrents/checkcached", http.Header{"Authorization": {"Bearer a"}})
	get(t, rt, "https://api.torbox.example/v1/api/torrents/checkcached", http.Header{"Authorization": {"Bearer a"}})
	if next.calls != 1 {
		t.Errorf("the throttled account was asked again: %d calls", next.calls)
	}
	get(t, rt, "https://api.torbox.example/v1/api/torrents/checkcached", http.Header{"Authorization": {"Bearer b"}})
	get(t, rt, "https://premiumize.example/api/cache/check?apikey=c", nil)
	if next.calls != 3 {
		t.Errorf("another account was held back: %d calls", next.calls)
	}
}

// Without a Retry-After the pause starts short and doubles; a bare 503 is a shed request, not a limit, and pauses
// nothing.
func TestPauseOnRefusal_doublesWithoutRetryAfterAndLeavesABare503Alone(t *testing.T) {
	rt := PauseOnRefusal(&countingTransport{}).(*pausingTransport)
	first := rt.refused("h", 0, false)
	second := rt.refused("h", 0, false)
	if first < refusalPause || first > refusalPause*5/4 || second < 2*refusalPause || second > 2*refusalPause*5/4 {
		t.Errorf("pauses %v then %v", first, second)
	}
	for i := 0; i < 20; i++ {
		rt.refused("h", 0, false)
	}
	if p := rt.refused("h", 0, false); p > maxRefusalPause*5/4 {
		t.Errorf("pause grew past its ceiling: %v", p)
	}

	next := &countingTransport{answer: answering(http.StatusServiceUnavailable, nil)}
	shed := PauseOnRefusal(next)
	get(t, shed, "https://torrentio.example/a", nil)
	get(t, shed, "https://torrentio.example/b", nil)
	if next.calls != 2 {
		t.Errorf("a 503 without Retry-After paused the host: %d calls", next.calls)
	}
}

func TestRetryAfter_readsSecondsAndDatesAndCapsThem(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"30":                            30 * time.Second,
		"999999":                        maxRetryAfter,
		"Mon, 14 Sep 2026 12:02:00 GMT": 2 * time.Minute,
		"Mon, 14 Sep 2026 11:00:00 GMT": 0,
	} {
		if got, ok := retryAfter(http.Header{"Retry-After": {value}}, now); !ok || got != want {
			t.Errorf("Retry-After %q = %v %v, want %v", value, got, ok, want)
		}
	}
	for _, value := range []string{"", "soon", "-5"} {
		if _, ok := retryAfter(http.Header{"Retry-After": {value}}, now); ok {
			t.Errorf("Retry-After %q read as a wait", value)
		}
	}
}

// An answer that says the allowance is spent pauses the upstream until its reset, before any 429.
func TestPauseOnRefusal_waitsOutAnAllowanceAnAnswerSaysIsSpent(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for name, header := range map[string]http.Header{
		"draft":  {"Ratelimit": {`"hourly";r=0;t=90`}},
		"older":  {"Ratelimit-Remaining": {"0"}, "Ratelimit-Reset": {"90"}},
		"legacy": {"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Add(90*time.Second).Unix(), 10)}},
	} {
		if wait, spent := exhaustedFor(header, now); !spent || wait != 90*time.Second {
			t.Errorf("%s: %v %v, want 90s", name, wait, spent)
		}
	}
	for name, header := range map[string]http.Header{
		"requests left": {"Ratelimit": {`"hourly";r=4;t=90`}, "X-Ratelimit-Remaining": {"4"}},
		"nothing said":  {},
	} {
		if _, spent := exhaustedFor(header, now); spent {
			t.Errorf("%s read as spent", name)
		}
	}

	next := &countingTransport{answer: answering(http.StatusOK, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"60"}})}
	rt := PauseOnRefusal(next)
	get(t, rt, "https://api.torbox.example/v1/api/user/me", nil)
	if resp := get(t, rt, "https://api.torbox.example/v1/api/user/me", nil); resp.StatusCode != http.StatusTooManyRequests || next.calls != 1 {
		t.Errorf("asked a spent upstream again: %d, %d calls", resp.StatusCode, next.calls)
	}
}

func TestAddBudget_freesInWhenItsOldestChargeDrains(t *testing.T) {
	b := newAddBudget(time.Hour, 2)
	clock := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return clock }
	b.take("acct")
	if b.freesIn("acct") != 0 {
		t.Error("an account with allowance left must not be told to wait")
	}
	clock = clock.Add(10 * time.Minute)
	b.take("acct")
	if got := b.freesIn("acct"); got != 50*time.Minute {
		t.Errorf("freesIn = %v, want the first charge's 50 minutes left", got)
	}
}
