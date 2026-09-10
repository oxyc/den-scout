package scout

import (
	"net/http"
	"time"
)

// How long /play remembers answering "downloading" for a file, so the next poll skips the held step and
// asks status straight away. Refreshed by every such answer, so it lasts as long as the polling does and
// lapses soon after; the client polls every couple of seconds, so this is many polls of slack.
const playPendingTTL = 30 * time.Second

// playPendingKey is scoped like the link memo — the same accounts, the same file.
func playPendingKey(memoKey string) string { return "play:pending:" + memoKey }

// warmResolveBudget is the slice of the resolve clock the held step may take. A function rather than a
// value derived once, so it follows a statusBudget a test has shortened — the trap escalatedStatusBudget
// records falling into.
func warmResolveBudget() time.Duration { return statusBudget / 2 }

// writePlayQueued is /play's 202, noting that this file is being waited on.
func (h *handler) writePlayQueued(w http.ResponseWriter, pendingKey, hash string, status StoreStatus) {
	h.deps.Cache.Put(pendingKey, "1", playPendingTTL)
	writeQueued(w, hash, status)
}
