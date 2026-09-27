package scout

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Stream availability for a page of titles at once: the question Den's poster rows ask — does this movie
// have anything to play? — answered in one request instead of a probe per poster, for the TV and for a
// browser that cannot probe addons itself.
//
// It answers AT ONCE from the verdicts on hand and says "unknown" for the rest, checking those behind the
// reply: an uncached title can take the whole scrape timeout, and a row of posters must not wait on its
// slowest title. The client asks again for what came back unknown, and unknown never hides a title.
//
// Movies only, like the TV's own probes — a series needs per-episode resolution.

const (
	// The TV's verdict lifetimes, asymmetric for its reason: a title with streams keeps them, while a
	// just-released one gains them within hours, so "none" is asked again in minutes.
	availableTTL   = 30 * 24 * time.Hour
	unavailableTTL = 10 * time.Minute
	// How long a check that could not tell holds off the next one for that title. Without it, every ask
	// during an indexer outage starts another scrape for every unknown title on the page.
	availabilityRetryAfter = time.Minute
	// A page of posters is a few dozen titles; a request naming more is not a page.
	maxAvailabilityIDs  = 100
	maxAvailabilityBody = 16 << 10
	// Checks running behind replies, across the process. Each is a full scrape plus a debrid cache check,
	// paced by the indexer limiter; a title past this ceiling stays unknown and is checked on a later ask.
	maxAvailabilityChecks = 4
)

// Cached verdicts. verdictUndetermined is a check that could not tell, kept for availabilityRetryAfter.
const (
	verdictAvailable    = "1"
	verdictUnavailable  = "0"
	verdictUndetermined = "?"
)

// handleAvailability — POST /<config>/availability {"ids":["tt…"]} →
// {"availability":{"tt…":"available"|"unavailable"|"unknown"}}.
func (h *handler) handleAvailability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, errBody("method_not_allowed"), noStore)
		return
	}
	parts := splitPath(r.URL.Path)
	if len(parts) != 2 {
		writeJSON(w, http.StatusNotFound, errBody("not_found"), noStore)
		return
	}
	config, ok := h.openConfig(parts[0])
	if !ok {
		writeJSON(w, http.StatusBadRequest, errBody("bad_config"), noStore)
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAvailabilityBody)).Decode(&body) != nil ||
		len(body.IDs) > maxAvailabilityIDs {
		writeJSON(w, http.StatusBadRequest, errBody("bad_request"), noStore)
		return
	}
	for _, id := range body.IDs {
		if !imdbRe.MatchString(id) {
			writeJSON(w, http.StatusBadRequest, errBody("bad_id"), noStore)
			return
		}
	}
	prefix := verdictPrefix(config)
	out := make(map[string]string, len(body.IDs))
	for _, id := range body.IDs {
		switch v, _ := h.deps.Cache.Get(prefix + id); v {
		case verdictAvailable:
			out[id] = "available"
		case verdictUnavailable:
			out[id] = "unavailable"
		case verdictUndetermined:
			out[id] = "unknown"
		default:
			out[id] = "unknown"
			h.checkBehind(config, id, prefix+id)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"availability": out}, noStore)
}

// verdictPrefix keys a config's verdicts. The scope is left out, so the scoped config a browser holds reads
// the verdicts the TV's full config writes: the same account, indexers and filters give the same answer.
// The install id and epoch are left out for the same reason — the browser mints its scoped config
// separately, so it carries an id of its own.
func verdictPrefix(config *Config) string {
	c := *config
	c.Scope, c.IID, c.Epoch = "", "", 0
	b, _ := json.Marshal(c)
	return "avail:" + keyHash(string(b)) + ":"
}

func (h *handler) recordVerdict(key string, available bool) {
	if available {
		cachePut(h.deps.Cache, key, verdictAvailable, availableTTL, CacheDurable)
	} else {
		cachePut(h.deps.Cache, key, verdictUnavailable, unavailableTTL, CacheVolatile)
	}
}

// One playable release settles availability; an empty partial list cannot settle absence. This also
// covers a nonempty scrape whose surviving releases were all removed by the viewer's filters.
func (h *handler) recordListVerdict(key string, list rankedList) {
	if list.degraded != "" || (len(list.ranked) == 0 && !list.complete) {
		cachePut(h.deps.Cache, key, verdictUndetermined, availabilityRetryAfter, CacheVolatile)
		return
	}
	h.recordVerdict(key, len(list.ranked) > 0)
}

// checkBehind starts a background check of one movie, unless one is already running for it or the ceiling
// is reached. Booked before the goroutine exists, so a flood of asks costs a map lookup each rather than a
// goroutine each.
//
// Detached from the request: the reply has gone by the time the check finishes, which is the point.
func (h *handler) checkBehind(config *Config, imdb, key string) {
	h.availMu.Lock()
	if h.availInFlight == nil {
		h.availInFlight = map[string]bool{}
	}
	if h.availInFlight[key] || len(h.availInFlight) >= maxAvailabilityChecks {
		h.availMu.Unlock()
		return
	}
	h.availInFlight[key] = true
	h.availMu.Unlock()
	go func() {
		// A background goroutine, so the request's recover cannot see a panic here — and the booking is
		// released from a defer, so a panic cannot strand the key either.
		defer recoverBackground("availability check")
		defer func() {
			h.availMu.Lock()
			delete(h.availInFlight, key)
			h.availMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), h.deps.ScrapeTimeout+listBuildSlack)
		defer cancel()
		// Cache truth only changes ordering unless the install explicitly asks for cached-only results.
		// Availability is a boolean, so the default path can answer without spending a debrid cache check.
		if !config.CachedOnly && configHasService(config, ServiceTorBox) {
			metrics.torboxAvoid("availability_default")
		}
		list := h.rankAvailability(ctx, config, &StreamID{Type: "movie", IMDb: imdb})
		h.recordListVerdict(key, list)
	}()
}

func (h *handler) rankAvailability(ctx context.Context, config *Config, sid *StreamID) rankedList {
	if !config.CachedOnly || !hasCacheTruth(config) {
		return h.rankList(ctx, config, sid, nil, nil, false)
	}
	// Apply every ordinary filter before asking the debrid, but do not let the display cap hide a lower
	// ranked held release: availability asks whether ANY playable candidate exists, not which twenty to
	// show. Cache truth is then read one provider-sized batch at a time and stops on the first certain yes.
	filterConfig := *config
	filterConfig.CachedOnly = false
	filterConfig.ResultCap = maxSeeds
	list := h.rankList(ctx, &filterConfig, sid, nil, nil, false)
	if list.degraded != "" || len(list.ranked) == 0 {
		return list
	}
	pool := &StorePool{stores: h.deps.MakeStores(config)}
	unknown := false
	for start := 0; start < len(list.ranked); start += cacheBatch {
		end := min(start+cacheBatch, len(list.ranked))
		hashes := make([]string, end-start)
		for i := start; i < end; i++ {
			hashes[i-start] = list.ranked[i].InfoHash
		}
		truth, ok := pool.CacheCheck(ctx, hashes)
		for i, hash := range hashes {
			if truth.Cached(hash) {
				stream := list.ranked[start+i]
				stream.Cached, stream.CacheKnown = true, true
				list.ranked = []RawStream{stream}
				list.truth = truth
				return list
			}
		}
		if !ok || !truth.Complete() {
			unknown = true
		}
	}
	list.ranked = nil
	if unknown {
		list.degraded = "cache-check"
	}
	return list
}
