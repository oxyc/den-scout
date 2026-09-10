package scout

import (
	"bytes"
	"log"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// lockedBuffer is a log sink a test can read while background goroutines from other tests still write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the standard logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	out := &lockedBuffer{}
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(out)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return out
}

// Only the per-install segments go; the route and the stream id stay, so the line still says what was
// asked for. A path cannot smuggle a newline in and forge a line of its own.
func TestRedactPath(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/configure/", "/configure/"},
		{"/health", "/health"},
		{"/metrics", "/metrics"},
		{"/manifest.json", "/manifest.json"},
		{"/config-key", "/config-key"},
		{"/validate", "/validate"},
		{"/" + validBlob + "/manifest.json", "/<config>/manifest.json"},
		{"/" + validBlob + "/stream/series/tt1:2:3.json", "/<config>/stream/series/tt1:2:3.json"},
		{"/" + validBlob + "/play/secret-token", "/<config>/play/<token>"},
		{"//" + validBlob + "/play/secret-token", "/<config>/play/<token>"},
		{"/p/secret-ticket", "/p/<ticket>"},
		{"//p//secret-ticket", "/p/<ticket>"},
		{"/nope", "/<config>"},
		// A slash is a segment boundary, as it is to the handler; the newline and spaces are escaped.
		{"/cfg/stream/movie/tt1\nGET /health 200 0ms.json", "/<config>/stream/movie/tt1%0AGET%20/health%20200%200ms.json"},
	} {
		if got := redactPath(c.in); got != c.want {
			t.Errorf("redactPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Unset, empty and exactly "0" leave it off; any other value turns it on.
func TestLogRequestsSetting(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "1": true, "true": true, "yes": true} {
		s := SettingsFromEnv(func(k string) string {
			if k == "LOG_REQUESTS" {
				return v
			}
			return ""
		})
		if s.LogRequests != want || BuildDeps(s, nil, NewMemoryCache(1<<10)).LogRequests != want {
			t.Errorf("LOG_REQUESTS=%q: got %v, want %v", v, s.LogRequests, want)
		}
	}
}

// Off by default and silent; on, one line per response with the config, the token and the whole query
// string gone.
func TestRequestLog(t *testing.T) {
	out := captureLog(t)
	do(NewHandler(testDeps(nil)), "/health", nil)
	if strings.Contains(out.String(), "/health") {
		t.Fatalf("a request was logged with LOG_REQUESTS off:\n%s", out)
	}

	h := NewHandler(testDeps(func(d *Deps) { d.LogRequests = true }))
	do(h, "/health", nil)
	do(h, "/"+validBlob+"/play/tok-secret?probe=1&token=query-secret", nil)
	got := out.String()
	for _, want := range []string{`(?m)^GET /health 200 \d+ms$`, `(?m)^GET /<config>/play/<token> 400 \d+ms$`} {
		if !regexp.MustCompile(want).MatchString(got) {
			t.Errorf("no line matching %s in:\n%s", want, got)
		}
	}
	for _, leak := range []string{validBlob, "tok-secret", "query-secret", "probe"} {
		if strings.Contains(got, leak) {
			t.Errorf("the request log carried %q:\n%s", leak, got)
		}
	}
}

// The app's X-Request-Id ends the line, so its log and this one join exactly; a hostile value is cut down
// to what cannot forge a line, and no header means no rid at all.
func TestRequestLogCarriesTheRequestID(t *testing.T) {
	out := captureLog(t)
	h := NewHandler(testDeps(func(d *Deps) { d.LogRequests = true }))
	do(h, "/health", map[string]string{"X-Request-Id": "a1B2-c3_d4"})
	do(h, "/manifest.json", map[string]string{"X-Request-Id": "x y\nGET /<config>/play 200 0ms" + strings.Repeat("z", 100)})
	do(h, "/config-key", nil)
	do(h, "/configure", map[string]string{"X-Request-Id": " \n<>"})
	got := out.String()
	for _, want := range []string{
		`(?m)^GET /health 200 \d+ms rid=a1B2-c3_d4$`,
		`(?m)^GET /manifest\.json 200 \d+ms rid=xyGETconfigplay2000mszzzzzzzzzzz$`,
		`(?m)^GET /config-key \d+ \d+ms$`,
		`(?m)^GET /configure 200 \d+ms$`,
	} {
		if !regexp.MustCompile(want).MatchString(got) {
			t.Errorf("no line matching %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<config>/play") {
		t.Errorf("a request id forged a line:\n%s", got)
	}
}
