package scout

import (
	"log"
	"sync"
	"time"
)

// The log records changes of state, not events. A request-path line fires on every request for as long
// as its cause lasts — an indexer that is down is down for every scrape, a client waiting on a download
// polls every two seconds — so each such line goes through logLimited, and a condition writes at most
// one line a minute however much traffic it sees. Per-request detail is what LOG_REQUESTS is for.
const logInterval = time.Minute

var limitedLog = struct {
	mu   sync.Mutex
	last map[string]limitedEntry
}{last: map[string]limitedEntry{}}

type limitedEntry struct {
	at         time.Time
	suppressed int
}

// logLimited writes the line unless the same condition already wrote one inside logInterval; the next
// line of that condition to get through says how many were held back in between. There is no timer — a
// held-back count is reported by the condition's own next line, whenever that comes, or not at all if it
// never recurs, which is the state change the silence already told you about.
//
// The key names the CONDITION and must come from a fixed vocabulary: a literal, an indexer id, a debrid
// service. Never a hash, a title or anything a caller picks, or the map grows with traffic.
func logLimited(key, format string, args ...any) {
	now := time.Now()
	limitedLog.mu.Lock()
	e, seen := limitedLog.last[key]
	if seen && now.Sub(e.at) < logInterval {
		e.suppressed++
		limitedLog.last[key] = e
		limitedLog.mu.Unlock()
		return
	}
	limitedLog.last[key] = limitedEntry{at: now}
	limitedLog.mu.Unlock()
	if e.suppressed > 0 {
		format += " (%d more like this since the last line)"
		args = append(args, e.suppressed)
	}
	log.Printf(format, args...)
}
