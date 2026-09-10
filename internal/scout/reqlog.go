package scout

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// logRequests writes one line per response — `<METHOD> <path> <status> <ms>ms[ rid=<id>]` — when
// LOG_REQUESTS is on. NewHandler only wraps the handler when it is, so with it off a request pays nothing
// at all.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK // nothing written at all is net/http's empty 200
		}
		rid := ""
		if id := requestID(r.Header.Get("X-Request-Id")); id != "" {
			rid = " rid=" + id
		}
		log.Printf("%s %s %d %dms%s", r.Method, redactPath(r.URL.Path), status, time.Since(start).Milliseconds(), rid)
	})
}

// requestID is the caller's X-Request-Id as it may be logged: the app sends one per request and logs it
// too, so its line and this one can be joined exactly. It comes off the network, so only [A-Za-z0-9_-]
// survives and it is cut at 32 — nothing it carries can forge a line or smuggle a credential into the log.
func requestID(v string) string {
	var b strings.Builder
	for _, c := range v {
		if b.Len() == 32 {
			break
		}
		if c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// redactPath is the path as it may be logged. The first segment of any per-install route IS the
// credential — sealed or not, whoever holds it can spend the account's quota — and a /play token names
// exactly which release a household is watching, so both are replaced. A /p/<ticket> is both at once — a
// credential for one release — so the ticket goes as well. The query string is never passed in, so it
// cannot leak either.
//
// Split exactly as the handler splits (splitPath drops empty segments), so a `//<config>/…` path is
// redacted at the segment the handler reads as the config, not the empty one before it. What is kept is
// escaped, so a path cannot write a newline into the log and forge a line of its own.
func redactPath(p string) string {
	switch p {
	case "/", "/configure", "/configure/", "/health", "/metrics", "/manifest.json", "/config-key", "/validate":
		return p
	}
	parts := splitPath(p)
	if len(parts) == 0 {
		return "/"
	}
	out := make([]string, len(parts))
	ticketed := len(parts) == 2 && parts[0] == ticketRoute
	for i, seg := range parts {
		switch {
		case ticketed && i == 0:
			out[i] = ticketRoute
		case ticketed && i == 1:
			out[i] = "<ticket>"
		case i == 0:
			out[i] = "<config>"
		case i == 2 && parts[1] == "play":
			out[i] = "<token>"
		default:
			out[i] = url.PathEscape(seg)
		}
	}
	return "/" + strings.Join(out, "/")
}

// statusRecorder remembers the status a handler wrote, for the request line.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// WriteString keeps the zero-copy path conditional() relies on: embedding the interface alone would hide
// the underlying writer's WriteString, and io.WriteString would fall back to copying the whole body.
func (s *statusRecorder) WriteString(str string) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return io.WriteString(s.ResponseWriter, str)
}
