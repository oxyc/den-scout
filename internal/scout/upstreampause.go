package scout

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An upstream that says to stop is left alone by every request, not only the one it answered.
//
// Nothing here remembered a 429. An indexer or a debrid that throttled one request was asked again by the next
// — a season's worth of episode lists, every poster's availability check, every probe poll — and each one
// counted against the limit it had just said was spent. Retry-After was never read anywhere.
//
// So the transport under every outbound client keeps the refusal: a 429, or a 503 that names a Retry-After,
// pauses that upstream for as long as it asked (else 30s, doubling per refusal in a row up to 10 minutes). While
// the pause runs, a request to it is answered here with a 429 of our own carrying the time left, so no caller
// needs to know about the pause to honour it: each already treats a 429 as "not now". An answer ends the pause.
//
// Keyed by host and the credential the request carries, so one throttled debrid account pauses that account and
// not every other one on the same API, and an indexer — which limits by address — pauses for everyone.

const (
	refusalPause    = 30 * time.Second
	maxRefusalPause = 10 * time.Minute
	// A Retry-After past this is read as this: debrid limits are hourly, and an absurd value from a broken proxy
	// must not close an upstream for days.
	maxRetryAfter = time.Hour
	// How many paused upstreams are remembered. Hosts of household sources come from configs anyone can build.
	maxPausedUpstreams = 4096
)

// PauseOnRefusal wraps a transport so upstreams that refuse are left alone for as long as they ask.
func PauseOnRefusal(next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &pausingTransport{next: next, paused: map[string]*upstreamPause{}, now: time.Now}
}

type pausingTransport struct {
	next   http.RoundTripper
	mu     sync.Mutex
	paused map[string]*upstreamPause
	now    func() time.Time
}

type upstreamPause struct {
	until    time.Time
	refusals int
}

func (t *pausingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := pauseKey(req)
	if left := t.left(key); left > 0 {
		return pausedResponse(req, left), nil
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	asked, named := retryAfter(resp.Header, t.now())
	switch {
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusServiceUnavailable && named:
		pause := t.refused(key, asked, named)
		logLimited("upstream-paused", "%s answered %d — leaving it alone for %s", req.URL.Hostname(),
			resp.StatusCode, pause.Round(time.Second))
	case resp.StatusCode < 400:
		// An answer that also says the allowance is spent is waited out from here, rather than found out by a 429.
		if wait, spent := exhaustedFor(resp.Header, t.now()); spent {
			pause := t.refused(key, wait, true)
			logLimited("upstream-exhausted", "%s says its allowance is spent — leaving it alone for %s",
				req.URL.Hostname(), pause.Round(time.Second))
		} else {
			t.answered(key)
		}
	}
	return resp, nil
}

// exhaustedFor reports how long until an upstream's allowance comes back, when its answer says none is left: the IETF
// draft `RateLimit: "policy";r=0;t=30`, its older `RateLimit-Remaining` / `RateLimit-Reset`, or the common
// `X-RateLimit-Remaining` / `X-RateLimit-Reset`. A reset over a billion is a Unix time, not seconds to wait.
func exhaustedFor(h http.Header, now time.Time) (time.Duration, bool) {
	number := func(v string) (int64, bool) {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil && n >= 0
	}
	wait := func(reset int64) time.Duration {
		d := time.Duration(reset) * time.Second
		if reset > 1_000_000_000 {
			d = time.Unix(reset, 0).Sub(now)
		}
		return max(0, min(d, maxRetryAfter))
	}
	if field := h.Get("RateLimit"); field != "" {
		param := func(key string) (int64, bool) {
			for _, part := range strings.Split(field, ";") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(part), key+"="); ok {
					return number(v)
				}
			}
			return 0, false
		}
		if r, ok := param("r"); ok && r == 0 {
			t, _ := param("t")
			return wait(t), true
		}
	}
	for _, names := range [][2]string{{"RateLimit-Remaining", "RateLimit-Reset"}, {"X-RateLimit-Remaining", "X-RateLimit-Reset"}} {
		if r, ok := number(h.Get(names[0])); ok && h.Get(names[0]) != "" && r == 0 {
			reset, _ := number(h.Get(names[1]))
			return wait(reset), true
		}
	}
	return 0, false
}

func (t *pausingTransport) left(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.paused[key]
	if p == nil {
		return 0
	}
	return p.until.Sub(t.now())
}

// refused starts or extends the pause and says how long it is.
func (t *pausingTransport) refused(key string, asked time.Duration, named bool) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	p := t.paused[key]
	if p == nil {
		if len(t.paused) >= maxPausedUpstreams {
			t.pruneLocked(now)
		}
		p = &upstreamPause{}
		t.paused[key] = p
	}
	p.refusals++
	pause := asked
	if !named {
		pause = refusalPause << min(p.refusals-1, 10)
		if pause > maxRefusalPause {
			pause = maxRefusalPause
		}
		pause += time.Duration(rand.Int63n(int64(pause/4) + 1))
	}
	if until := now.Add(pause); until.After(p.until) {
		p.until = until
	}
	return pause
}

func (t *pausingTransport) answered(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.paused, key)
}

// pruneLocked drops pauses that have ended, and if none have, starts over: forgetting a pause costs one more
// refused request, while an unbounded map costs the process.
func (t *pausingTransport) pruneLocked(now time.Time) {
	for key, p := range t.paused {
		if !p.until.After(now) {
			delete(t.paused, key)
		}
	}
	if len(t.paused) >= maxPausedUpstreams {
		t.paused = map[string]*upstreamPause{}
	}
}

// pauseKey is the upstream a request goes to: its host, and a hash of the credential it carries, if any.
func pauseKey(req *http.Request) string {
	credential := req.Header.Get("Authorization")
	if credential == "" {
		q := req.URL.Query()
		for _, name := range []string{"apikey", "api_key", "token", "key"} {
			if v := q.Get(name); v != "" {
				credential = v
				break
			}
		}
	}
	if credential == "" {
		return req.URL.Host
	}
	sum := sha256.Sum256([]byte(credential))
	return req.URL.Host + "|" + hex.EncodeToString(sum[:8])
}

// retryAfter reads a Retry-After of seconds or an HTTP date, capped at maxRetryAfter; the bool says whether one
// was sent that reads.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
		d = time.Duration(min(secs, int64(maxRetryAfter/time.Second))) * time.Second
	} else if at, err := http.ParseTime(v); err == nil {
		d = at.Sub(now)
	} else {
		return 0, false
	}
	return max(0, min(d, maxRetryAfter)), true
}

// pausedResponse is the 429 a paused upstream is answered with, carrying the time left.
func pausedResponse(req *http.Request, left time.Duration) *http.Response {
	secs := int64((left + time.Second - 1) / time.Second)
	return &http.Response{
		Status:     "429 Too Many Requests",
		StatusCode: http.StatusTooManyRequests,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"Retry-After":    []string{strconv.FormatInt(secs, 10)},
			"X-Scout-Paused": []string{"1"},
		},
		Body:          io.NopCloser(strings.NewReader("")),
		ContentLength: 0,
		Request:       req,
	}
}
