package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// The runtime-agnostic core as an http.Handler (ported from src/handler.ts). Off-device by design:
// the app never sees a torrent or a debrid token. Routes are served at the service root.

const (
	staticCache = "public, max-age=3600"
	// The sealing key can rotate, so keep its freshness window short — the ETag is the primary
	// correctness mechanism (a rotated key changes the body hash and busts any stale cache).
	keyCache       = "public, max-age=300"
	noStore        = "no-store"
	jsonType       = "application/json"
	htmlType       = "text/html; charset=utf-8"
	defaultTimeout = 8 * time.Second
	defaultListTTL = 5 * time.Minute
	// Headroom over the scrape timeout for the cache-check phase of a detached list build.
	listBuildSlack = 20 * time.Second
	// A status read is one upstream call and runs on the client's poll cadence — keep it far under the
	// resolve budget so a wait answers promptly instead of hanging the poll.
	statusBudget = 8 * time.Second
	// Hard cap on a /play resolve (addMagnet→select→unrestrict across stores) so a slow debrid account
	// can't pin a goroutine/connection indefinitely.
	resolveBudget = 45 * time.Second
	// Upper bound on scraped seeds fed into the cache-check fan-out (guards outbound amplification).
	maxSeeds = 500
	// How long a list is kept when an indexer did not answer. Short enough that the missing releases
	// appear soon after that indexer recovers; long enough that a flaky upstream does not put the full
	// scrape and a debrid fan-out on every single request.
	partialListTTL = time.Minute
	// How long past its freshness a COMPLETE list may still be served while a rebuild runs behind it.
	//
	// The response header has advertised stale-while-revalidate since this route existed, and the server
	// implemented none of it: the request that arrived one second after expiry paid the whole eight-second
	// scrape plus a debrid cache-check fan-out, with a perfectly good list sitting in memory.
	//
	// Two minutes, not another full TTL. The window only has to cover the moment of expiry — a rebuild
	// finishes well inside it — and every second added here is a second every cached list occupies memory
	// and disk. It also bounds how wrong a served list may be: torrent availability moves, and a list
	// stale by an hour is not a kindness.
	staleServeWindow = 2 * time.Minute
)

// Deps injects the environment: the cache, timeouts, public origin, and the scraper/store factories
// (mocked in tests).
type Deps struct {
	Cache Cache
	// Client for reading the head of a resolved link. Separate from the scraper client so a probe's
	// longer body read can't tie up the timeouts tuned for indexer queries.
	ProbeClient   *http.Client
	ScrapeTimeout time.Duration
	ListTTL       time.Duration
	PublicURL     string // audit #8: fixed public origin; when empty, fall back to forwarded headers
	MakeScrapers  func(*Config) []scraper
	MakeStores    func(*Config) []Store
	// Meta resolves an id → title + release year (movies only) so mistagged torrents can be dropped.
	// Optional (nil = no year/title filter); a lookup failure returns ok=false and the list is served
	// unfiltered.
	Meta func(ctx context.Context, typ, imdb string) (cineMeta, bool)
	// SealKeyring decrypts a sealed config path segment (docs/SEALED-CONFIG.md). nil = sealed URLs
	// disabled (legacy plaintext still works); the current key's public half is served at /config-key.
	SealKeyring *sealKeyring
}

type handler struct {
	deps Deps
	sf   singleflight.Group

	// Consecutive fully-degraded builds (every indexer failed). Surfaced on /health so a scrape outage
	// — which otherwise looks like empty stream lists — is visible to an uptime monitor.
	scrapeFails atomic.Int32

	// Precomputed static responses (audit #15 — no per-request rebuild/rehash).
	manifestUnconf     string
	manifestUnconfETag string
	configureETag      string
}

// After this many consecutive builds where no indexer responded, /health reports "degraded".
const scrapeFailThreshold = 3

// NewHandler builds the scout HTTP handler.
func NewHandler(deps Deps) http.Handler {
	if deps.ScrapeTimeout == 0 {
		deps.ScrapeTimeout = defaultTimeout
	}
	if deps.ListTTL == 0 {
		deps.ListTTL = defaultListTTL
	}
	h := &handler{deps: deps}
	b, _ := json.Marshal(buildManifest(nil))
	h.manifestUnconf = string(b)
	h.manifestUnconfETag = etagFor(h.manifestUnconf)
	h.configureETag = etagFor(configurePage)
	return http.HandlerFunc(h.serve)
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	// Convert any unforeseen panic into a clean 500 instead of a dropped connection (net/http would
	// recover it, but the client would see a reset). No-op if the response was already partly written.
	defer func() {
		if rec := recover(); rec != nil {
			writeJSON(w, http.StatusInternalServerError, errBody("internal"), noStore)
		}
	}()
	// Every route here is a read, so GET/HEAD is the entire vocabulary — and until this gate existed the
	// handler dispatched on PATH ALONE, which made that an assumption rather than a rule. `/play` is the
	// one that mattered: it resolves, and resolving an uncached release ADDS it, against an allowance of
	// fifty an hour. So any verb at all — a link unfurler's POST, a crawler's PUT — reached the one code
	// path this package spends the most effort keeping accidental callers out of (see the probe route's
	// `?probe=1`, probeTop's held-only rule, and addbudget.go).
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, errBody("method_not_allowed"), noStore)
		return
	}
	path := r.URL.Path
	switch path {
	case "/", "/configure", "/configure/":
		h.conditional(w, r, configurePage, h.configureETag, htmlType, staticCache)
		return
	case "/health":
		// Stays 200 (liveness — don't trip the container HEALTHCHECK), but reports the degraded scrape
		// state so a monitor sees a total-indexer outage instead of just "empty results".
		status := map[string]any{"status": "ok"}
		if h.scrapeFails.Load() >= scrapeFailThreshold {
			status = map[string]any{"status": "degraded", "reason": "indexers"}
		}
		// A spent add budget refuses every play with the same 503 a throttled debrid gives, and the only
		// other evidence is one log line per refusal. The tightest remaining allowance is what an operator
		// or a monitor needs; per-account detail is withheld because this route is unauthenticated.
		if left, accounts := globalAddBudget.lowest(); accounts > 0 {
			status["addBudgetRemaining"] = left
		}
		writeJSON(w, http.StatusOK, status, noStore)
		return
	case "/metrics":
		// Prometheus text format. Unauthenticated like /health, and safe to be, because of what it does
		// NOT carry: no debrid service or account labels, no ids, no titles — see metrics.go's render.
		w.Header().Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("cache-control", noStore)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(metrics.render(cachePersistentGauge(h.deps.Cache))))
		return
	case "/manifest.json":
		h.conditional(w, r, h.manifestUnconf, h.manifestUnconfETag, jsonType, staticCache)
		return
	case "/config-key":
		// The current X25519 public key (base64) so /configure (or den-app) can seal the config to it.
		// 404 when sealed configs are disabled (no key configured) — the page then keeps plaintext.
		if pub := h.deps.SealKeyring.currentPubBase64(); pub != "" {
			// ETag over the key JSON so a rotated key isn't masked by a stale cache and If-None-Match
			// yields 304, consistent with the other cacheable routes.
			body, _ := json.Marshal(map[string]string{"key": pub})
			h.conditional(w, r, string(body), etagFor(string(body)), jsonType, keyCache)
		} else {
			writeJSON(w, http.StatusNotFound, errBody("no_key"), noStore)
		}
		return
	}

	parts := splitPath(path) // ["<config>", "stream"|"play"|"manifest.json", ...]
	configBlob := ""
	if len(parts) > 0 {
		configBlob = parts[0]
	}
	var resource string
	if len(parts) > 1 {
		resource = parts[1]
	}

	switch resource {
	case "manifest.json":
		config, ok := decodeConfig(h.deps.SealKeyring, configBlob)
		if !ok {
			writeJSON(w, http.StatusBadRequest, errBody("bad_config"), noStore)
			return
		}
		body, _ := json.Marshal(buildManifest(config))
		h.conditional(w, r, string(body), "", jsonType, staticCache)
	case "stream":
		h.handleStream(w, r, configBlob, parts)
	case "play":
		h.handlePlay(w, r, configBlob, parts)
	default:
		writeJSON(w, http.StatusNotFound, errBody("not_found"), noStore)
	}
}

func (h *handler) handleStream(w http.ResponseWriter, r *http.Request, configBlob string, parts []string) {
	typ := at(parts, 2)
	sid, ok := parseStreamID(typ, at(parts, 3))
	if !ok {
		writeJSON(w, http.StatusBadRequest, errBody("bad_id"), noStore)
		return
	}
	ttlSec := int(h.deps.ListTTL.Seconds())
	listCache := fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d, stale-if-error=86400", ttlSec, ttlSec)
	partialSec := int(partialListTTL.Seconds())
	partialListCache := fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d", partialSec, partialSec)
	// A stale body carries a short freshness window and no stale-if-error: the client should come back
	// soon, by which time the rebuild this response kicked off has landed. Holding a stale list for the
	// full TTL — and a day on any later error — is what the server is fixing, not something to pass on.
	staleListCache := fmt.Sprintf("public, max-age=%d", partialSec)
	origin := h.publicOrigin(r)
	// audit #7 (collision-resistant key) + #8 (origin part) + #16 (key off the raw blob, decode later).
	cacheKey := "list:" + keyHash(configBlob) + ":" + keyHash(origin) + ":" + streamCacheID(sid)

	if hit, ok := h.deps.Cache.Get(cacheKey); ok {
		// The completeness rides WITH the entry. Shortening the header only on the build meant the very
		// next requester — Stremio races and cancels addon requests, so that happens routinely — was told
		// to hold a knowingly-short list for five minutes and a day on stale-if-error, which is the harm
		// the shortening exists to prevent, defeated one branch over.
		complete, freshUntil, etag, body := splitCached(hit)
		header := listCache
		metrics.listCacheHit.Add(1)
		switch {
		case !complete:
			header = partialListCache
		case freshUntil > 0 && time.Now().Unix() >= freshUntil:
			metrics.listCacheStale.Add(1)
			// Stale but complete: answer now from what we have and refresh behind the reply. Only
			// COMPLETE lists get this — serving a knowingly-short one past its own expiry is the harm
			// the branch above exists to prevent, and a longer life is the last thing it should get.
			header = staleListCache
			h.rebuildBehind(r, configBlob, sid, origin, cacheKey)
		}
		h.conditional(w, r, body, etag, jsonType, header)
		return
	}

	// Miss: decode now (#16 — a warm hit never pays decode/validate).
	metrics.listCacheMiss.Add(1)
	config, ok := decodeConfig(h.deps.SealKeyring, configBlob)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errBody("bad_config"), noStore)
		return
	}

	// Detach the shared build from the request: a client disconnect (Stremio routinely races and cancels
	// addon requests) must not cancel the scrape mid-flight and let an empty list get cached and served
	// to every follower. WithoutCancel keeps request values; the timeout bounds the work.
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), h.deps.ScrapeTimeout+listBuildSlack)
	defer cancel()
	v, _, _ := h.sf.Do(cacheKey, func() (any, error) {
		value, degraded, complete := h.buildStreamList(buildCtx, config, configBlob, sid, origin, cacheKey)
		return buildResult{value: value, degraded: degraded, complete: complete}, nil
	})
	res := v.(buildResult)
	// Signal a degraded build so the app can say "sources temporarily unavailable" rather than treating
	// an empty list as "no results" (a total indexer/cache-check outage otherwise looks identical).
	if res.degraded != "" {
		w.Header().Set("X-Scout-Degraded", res.degraded)
	}
	_, _, etag, body := splitCached(res.value)
	// A degraded build is deliberately not cached server-side, "so the next request retries instead of
	// serving the blip for the whole TTL" — and then the same body went out with `max-age=300,
	// stale-if-error=86400`, which URLSession's shared cache honours. The guard was defeated one layer
	// down, on the only client that matters: an outage's empty list stuck on the device for five minutes,
	// and up to a day on any later error.
	cacheHeader := listCache
	switch {
	case res.degraded != "":
		cacheHeader = noStore
	case !res.complete:
		// The SAME lesson one line up, for the partial case: the server held this list for a minute and
		// then told the client to keep it for five, with stale-if-error for a day. A list knowingly
		// missing an indexer's releases must not outlive its short server-side life on the device.
		cacheHeader = partialListCache
	}
	h.conditional(w, r, body, etag, jsonType, cacheHeader)
}

// rebuildBehind refreshes an expired list after the stale answer has gone out.
//
// Deduped through the same singleflight key the foreground build uses, so a burst of viewers arriving on
// an expired title produces one scrape — and a foreground build already in flight absorbs this one rather
// than racing it.
//
// Its own context, not the request's: the reply is already written, so inheriting it would cancel the
// rebuild immediately. The config is decoded HERE rather than on the hit path, because a warm hit must
// keep paying nothing for decode/validate — this runs only on the rare stale hit.
func (h *handler) rebuildBehind(r *http.Request, configBlob string, sid *StreamID, origin, cacheKey string) {
	config, ok := decodeConfig(h.deps.SealKeyring, configBlob)
	if !ok {
		return // it decoded when the entry was built; if it no longer does, serving stale is all we can do
	}
	go func() {
		// The same lesson as the probe fan-out: this is a background goroutine, so the recover() on the
		// request goroutine cannot see a panic raised here, and an unrecovered one takes the process down.
		defer recoverProbe("stale list rebuild")
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()),
			h.deps.ScrapeTimeout+listBuildSlack)
		defer cancel()
		_, _, _ = h.sf.Do(cacheKey, func() (any, error) {
			value, degraded, complete := h.buildStreamList(ctx, config, configBlob, sid, origin, cacheKey)
			return buildResult{value: value, degraded: degraded, complete: complete}, nil
		})
	}()
}

// buildResult carries the singleflight build's body plus a degraded reason ("" when healthy).
type buildResult struct {
	value    string
	degraded string
	// complete — every askable indexer answered. A partial list is real and worth caching, just not for
	// as long, and not with a client freshness window longer than the server's own.
	complete bool
}

// buildStreamList scrapes → cache-checks → ranks → serializes, caches "<complete>\x00<etag>\x00<body>".
func (h *handler) buildStreamList(ctx context.Context, config *Config, configBlob string, sid *StreamID, origin, cacheKey string) (string, string, bool) {
	q := scrapeQuery{Type: sid.Type, IMDb: sid.IMDb, Season: sid.Season, Episode: sid.Episode, HasEp: sid.HasEp}
	seeds, scrapeOK, scrapeComplete := scrapeAll(ctx, h.deps.MakeScrapers(config), q, h.deps.ScrapeTimeout)

	// Cap the seed set before the cache-check fan-out: a misbehaving/hostile indexer returning thousands
	// of tiny stream objects would otherwise mean hundreds of concurrent outbound debrid requests. The
	// cap is well above any real title's stream count, so it rarely bites — but when it does it now keeps
	// the most promising releases rather than the first ones the scrape happened to return.
	if len(seeds) > maxSeeds {
		log.Printf("scout: %s %s: %d seeds capped to %d (best-scored kept)", sid.Type, sid.IMDb, len(seeds), maxSeeds)
		seeds = capSeeds(seeds, maxSeeds)
	}

	pool := &StorePool{stores: h.deps.MakeStores(config)}
	hashes := make([]string, len(seeds))
	for i, s := range seeds {
		hashes[i] = s.InfoHash
	}
	truth, truthOK := pool.CacheCheck(ctx, hashes)
	for i := range seeds {
		hash := seeds[i].InfoHash
		seeds[i].Cached = truth.Cached(hash)
		// Whether that false means "not held" or "nobody could ask" is decided here, once, and carried —
		// rather than being re-guessed by every consumer from a header they may not have. Per HASH, not
		// per request: the checks are batched, so one failed batch leaves 100 releases unknown while the
		// rest of the list is perfectly well known. Stamping the request-wide `truthOK` on all of them
		// asserted the answer for exactly the hashes nobody had an answer for.
		seeds[i].CacheKnown = truth.Known(hash)
	}
	// A degraded upstream (every indexer failed, or every cache-truth store's check failed) yields a
	// misleading empty/partial list; return it for this request but don't cache it, so the next request
	// retries instead of serving the blip for the whole TTL.
	// Track consecutive total-scrape failures for /health (reset on any successful scrape).
	if scrapeOK {
		h.scrapeFails.Store(0)
	} else {
		h.scrapeFails.Add(1)
	}

	// TOTAL failure and PARTIAL failure are different states and drive different decisions, so they get
	// different variables. Folding them into one turned off the cached-only filter for the whole request
	// on any partial failure — which handed a viewer who asked for "only what plays now" releases the
	// store had definitively said it does not hold. The coarse gate silently won over the per-hash filter
	// that was added in the same commit to make exactly this case work.
	//
	// A cache-truth store being entirely OUT is an outage too, even when the others answer normally:
	// their replies cannot rule anything out on its behalf. Treating that as an ordinary answer let a
	// cachedOnly request return an empty list, with no degraded header, cached for the full TTL.
	truthOut := hasCacheTruth(config) && (!truthOK || !truth.Complete())
	// audit #4: with no cache-truth store (RD-only), the cached-only filter would drop everything. Also
	// skip it when the cache-truth stores are unreachable ENTIRELY (don't drop everything on a blip). A
	// partial failure keeps the filter on — the per-hash CacheKnown check inside it drops only what is
	// known not to be held, and keeps what nobody could ask about.
	effCachedOnly := config.CachedOnly && hasCacheTruth(config) && !truthOut
	// RD-only: drop releases RD blocks by filename (they'd 404 at resolve).
	if rdOnly(config) {
		var kept []RawStream
		for _, s := range seeds {
			if !realDebridBlocked(s.Title) {
				kept = append(kept, s)
			}
		}
		seeds = kept
	}

	// Expected title + release year (movies) → drop torrents mistagged with another film's id. Best-effort:
	// a lookup failure just means no year/title filter.
	var expectedYear *int
	var expectedTitleTokens map[string]bool
	if h.deps.Meta != nil {
		if m, ok := h.deps.Meta(ctx, sid.Type, sid.IMDb); ok {
			if m.Year != 0 {
				y := m.Year
				expectedYear = &y
			}
			if m.Title != "" {
				expectedTitleTokens = titleTokens(m.Title)
			}
		}
	}

	ranked := rankStreams(seeds, rankFilters{
		ExcludeCam:          config.Filters.ExcludeCam,
		Resolutions:         config.Filters.Resolutions,
		HDROnly:             config.Filters.HDROnly,
		MinSeeders:          config.Filters.MinSeeders,
		MaxSizeGB:           config.Filters.MaxSizeGB,
		ExcludeRegex:        config.Filters.ExcludeRegex,
		CachedOnly:          effCachedOnly,
		ResultCap:           config.ResultCap,
		ExpectedYear:        expectedYear,
		ExpectedTitleTokens: expectedTitleTokens,
	})

	// Degraded is judged on what is actually SERVED, not on what was scraped.
	//
	// A partial cache-check failure is only a problem if it touched a release the viewer will see. Counted
	// over all 500 seeds instead, a failed batch covering releases that every filter would have dropped
	// still marked the response degraded — which means not cached, which means the next /stream re-runs
	// the whole eight-second scrape. That is a user-visible cost paid for a fact about nothing.
	unchecked := 0
	if hasCacheTruth(config) {
		for i := range ranked {
			if !ranked[i].CacheKnown {
				unchecked++
			}
		}
	}
	degradedReason := ""
	if !scrapeOK {
		degradedReason = "indexers"
	} else if truthOut || unchecked > 0 {
		degradedReason = "cache-check"
	}
	degraded := degradedReason != ""
	if unchecked > 0 {
		log.Printf("scout: %s %s: %d of %d served releases could not be cache-checked",
			sid.Type, sid.IMDb, unchecked, len(ranked))
	}

	// Ask the top few releases what they actually contain. After ranking, so the probe follows the order
	// the viewer will see; before serialisation, so the answer rides along in the same response and the
	// Where a list is lost. An empty answer has half a dozen possible authors — no indexer had it, a
	// filter removed everything, the cache-only gate dropped an uncached set — and they are
	// indistinguishable in the reply. Reported when the pipeline empties a list that started non-empty,
	// which is the case worth a line and the one that had us guessing at an episode torrentio was
	// serving 50 results for.
	// Also when nothing was scraped at all: "no indexer had it" and "a filter removed it" are the two
	// answers an empty reply can carry, and only this line separates them.
	if len(ranked) == 0 || len(ranked) < len(seeds) {
		log.Printf("scout: %s %s: scraped %d → ranked %d (cachedOnly=%t year=%v filters: res=%v minSeed=%d maxGB=%v cam=%t hdr=%t)",
			sid.Type, sid.IMDb, len(seeds), len(ranked), effCachedOnly, expectedYear,
			config.Filters.Resolutions, config.Filters.MinSeeders, config.Filters.MaxSizeGB,
			config.Filters.ExcludeCam, config.Filters.HDROnly)
	}
	h.probeTop(ctx, config, ranked, sid, truth)

	out := make([]streamOut, 0, len(ranked))
	for _, s := range ranked {
		out = append(out, toStremioStream(s, sid, origin, configBlob))
	}
	body, _ := json.Marshal(streamsResponse{Streams: out})
	etag := etagFor(string(body))
	// A list missing one indexer's releases is worth serving and worth caching — just not for as long
	// as a complete one. Refusing to cache it at all put the full scrape and a fresh debrid fan-out on
	// every single request whenever one upstream was flaky, which is a worse answer than the slightly
	// short list it was protecting against.
	ttl := h.deps.ListTTL
	if !scrapeComplete {
		ttl = partialListTTL
	}
	// The entry outlives its freshness by staleServeWindow so the hit path can answer from it while a
	// rebuild runs — but only when it is COMPLETE. A partial list expires exactly when it says it does:
	// it is already knowingly short, and the one thing it must not get is a longer life.
	now := time.Now()
	freshUntil := int64(0)
	hold := ttl
	if scrapeComplete {
		freshUntil = now.Add(ttl).Unix()
		hold = ttl + staleServeWindow
	}
	value := joinCached(scrapeComplete, freshUntil, etag, string(body))
	switch {
	case degraded:
		metrics.buildDegraded.Add(1)
	case !scrapeComplete:
		metrics.buildPartial.Add(1)
	default:
		metrics.buildOK.Add(1)
	}
	if !degraded {
		if !scrapeComplete {
			log.Printf("scout: %s %s: an indexer did not answer; caching this list for %s only",
				sid.Type, sid.IMDb, ttl)
		}
		h.deps.Cache.Put(cacheKey, value, hold)
	}
	return value, degradedReason, scrapeComplete
}

// writeQueued — the "it's coming" answer: 202 plus whatever the store actually reported. `etaSeconds` is
// omitted rather than guessed when the service doesn't supply one.
func writeQueued(w http.ResponseWriter, hash string, status StoreStatus) {
	// The wait, as the viewer sees it. A percentage alone can't distinguish a slow fetch from a dead
	// swarm, so the rate is logged beside it — this is the line to read when someone says "it's stuck".
	rate := "unknown"
	if status.BytesPerSecond != nil {
		rate = fmt.Sprintf("%.1f Mbps", float64(*status.BytesPerSecond)*8/1e6)
	}
	eta := "none"
	if status.ETASeconds != nil {
		eta = fmt.Sprintf("%ds", *status.ETASeconds)
	}
	log.Printf("scout: play %s → 202 downloading %.1f%% at %s, eta %s",
		shortHash(hash), status.Progress*100, rate, eta)
	writeQueuedBody(w, status)
}

func writeQueuedBody(w http.ResponseWriter, status StoreStatus) {
	body := map[string]any{"state": "downloading", "progress": status.Progress}
	if status.ETASeconds != nil {
		body["etaSeconds"] = *status.ETASeconds
	}
	if status.BytesPerSecond != nil {
		body["bytesPerSecond"] = *status.BytesPerSecond
	}
	writeJSON(w, http.StatusAccepted, body, noStore)
}

// handleProbe answers "can this play yet?" without changing anything. Same vocabulary as /play so the
// client reads one set of statuses: 202 while downloading, 200 once the store holds it, 404 when neither
// is true — which here means "nothing has been queued", not "this release is dead".
func (h *handler) handleProbe(w http.ResponseWriter, ctx context.Context, config *Config, pool *StorePool,
	infoHash string, rt ResolveTarget) {
	if status, ok := pool.Status(ctx, rt); ok {
		writeQueued(w, infoHash, status)
		return
	}
	// "Ready" has to mean the ACCOUNT can serve it without queueing anything, which a cache check cannot
	// establish on its own: TorBox's checkcached reports what TorBox has, not what this account has. So
	// the claim is settled by a NoAdd resolve — the stores refuse rather than queue — and a cached-but-
	// not-held release correctly falls through to the "nothing queued" answer instead of promising a
	// playback that would start with a download.
	probeTruth, truthOK := pool.CacheCheck(ctx, []string{infoHash})
	var refusedUs *StoreUnavailableError
	if probeTruth.Cached(infoHash) {
		readOnly := rt
		readOnly.NoAdd = true
		_, err := pool.ResolveCachedOnly(ctx, readOnly, probeTruth.HeldBy(infoHash))
		if err == nil {
			log.Printf("scout: probe %s → 200 ready", shortHash(infoHash))
			writeJSON(w, http.StatusOK, map[string]any{"state": "ready"}, noStore)
			return
		}
		// The probe DOES discover refusals — it just used to throw them away, inspecting only `err ==
		// nil` and then relying on the backoff cache to have been written by someone else. Any refusal
		// that reaches here unrecorded fell through to the 404 below, which this file calls a claim
		// rather than a shrug: `errNoFileList` is never recorded at all, and TorBox's warm-entry fast
		// path returns a refusal without recording it, so during a throttle the very URL the client
		// polls to draw its progress bar said "nothing is queued" while /play said the debrid was
		// refusing. Reading the error we already have needs no cache to have been written first.
		_ = errors.As(err, &refusedUs)
	}
	if refusedUs != nil {
		log.Printf("scout: probe %s → 503, %s %s", shortHash(infoHash), refusedUs.Service, refusedUs.Reason)
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "store_unavailable", "service": refusedUs.Service}, noStore)
		return
	}
	// A refusal the store recorded moments ago outranks "nothing queued". Without this the probe reports
	// a throttled account as an absence, which reads as a release nobody can deliver — and the client
	// condemns a perfectly good one. This still matters for a refusal recorded by an EARLIER poll, whose
	// store the probe may not reach again.
	if svc, reason, ok := pool.RecentRefusal(infoHash); ok {
		log.Printf("scout: probe %s → 503, %s %s", shortHash(infoHash), svc, reason)
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "store_unavailable", "service": svc}, noStore)
		return
	}
	// With the cache check down we do not know whether anything is queued, and 404 "not_queued" is a
	// claim, not a shrug — the client reads it as a release nobody has. Say the store could not be asked.
	if !truthOK && hasCacheTruth(config) {
		log.Printf("scout: probe %s → 503, cache check unavailable", shortHash(infoHash))
		writeJSON(w, http.StatusServiceUnavailable, errBody("cache_check_unavailable"), noStore)
		return
	}
	log.Printf("scout: probe %s → 404 not queued", shortHash(infoHash))
	writeJSON(w, http.StatusNotFound, errBody("not_queued"), noStore)
}

func (h *handler) handlePlay(w http.ResponseWriter, r *http.Request, configBlob string, parts []string) {
	// GET only — HEAD is refused rather than answered.
	//
	// A HEAD here would have to either resolve (the whole point of not allowing it: an uncached release
	// is ADDED by that resolve) or be answered from the read-only path `?probe=1` uses. The second was
	// the tempting option and it is worse than useless: what a caller wants from this route is the
	// `location` of a freshly-minted link, and a HEAD that starts nothing cannot produce one — so it
	// would spend up to three upstream reads per prefetch to return a header no client can play from.
	//
	// Nothing legitimate issues it: Stremio asks for the list, and Den's AVPlayer follows the 302 with a
	// GET. What does issue HEAD is link prefetchers and unfurlers, which is precisely the traffic that
	// must not reach a debrid account.
	if r.Method != http.MethodGet {
		w.Header().Set("allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, errBody("method_not_allowed"), noStore)
		return
	}
	target, ok := decodePlayToken(at(parts, 2))
	if !ok {
		writeJSON(w, http.StatusBadRequest, errBody("bad_token"), noStore)
		return
	}
	config, ok := decodeConfig(h.deps.SealKeyring, configBlob)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errBody("bad_config"), noStore)
		return
	}
	pool := &StorePool{stores: h.deps.MakeStores(config)}
	ctx, cancel := context.WithTimeout(r.Context(), resolveBudget)
	defer cancel()
	rt := ResolveTarget{InfoHash: target.InfoHash, FileIdx: target.FileIdx, Season: target.Season, Episode: target.Episode}

	// ?probe=1 — answer, don't act. Asking /play for an uncached release is what QUEUES it, so a client
	// polling this URL to render a progress bar was adding the torrent again on every poll. TorBox allows
	// 60 adds an hour; a single three-minute wait spent thirty-six of them, and the account was throttled
	// out of playing anything at all. A probe reports what is true right now and starts nothing.
	if r.URL.Query().Get("probe") == "1" {
		// On the STATUS budget, not the resolve one. A probe answers and starts nothing, and the client
		// polls it on a two-second cadence — so it belongs under the ceiling statusBudget's own comment
		// describes ("far under the resolve budget so a wait answers promptly instead of hanging the
		// poll"), rather than pinning a goroutine and a connection for forty-five seconds against a slow
		// debrid. It makes up to three upstream calls, all reads.
		probeCtx, probeCancel := context.WithTimeout(r.Context(), statusBudget)
		defer probeCancel()
		h.handleProbe(w, probeCtx, config, pool, target.InfoHash, rt)
		return
	}

	// Already known to be downloading? Answer from the status alone. A waiting client polls this URL for
	// the whole fetch, and a full resolve is ~3 upstream calls (cache miss → add → link), so re-running it
	// per poll is how an account gets itself throttled — and a throttled 5xx is exactly what a client
	// cannot distinguish from a real answer.
	//
	// On the STATUS budget, like the probe route and the post-failure retry below. This one had kept the
	// 45-second resolve budget, so the read that exists to answer a poll PROMPTLY could hold the poll for
	// forty-five seconds against a slow debrid — the thing statusBudget's own comment forbids. Two of its
	// three siblings already agreed; this was the third.
	statusCtx, statusCancel := context.WithTimeout(r.Context(), statusBudget)
	defer statusCancel()
	if status, ok := pool.Status(statusCtx, rt); ok {
		writeQueued(w, target.InfoHash, status)
		return
	}

	// Which services already hold it, so the resolve starts with one that can serve now rather than one
	// that would have to download it. Only worth asking with more than one account configured — with one
	// there is nothing to choose between, and it would be a wasted upstream call on every play.
	var preferred []DebridService
	if len(config.Debrid) > 1 {
		playTruth, _ := pool.CacheCheck(ctx, []string{target.InfoHash})
		preferred = playTruth.HeldBy(target.InfoHash)
	}
	link, err := pool.ResolvePreferring(ctx, rt, preferred)
	if err != nil {
		// Queued is not dead. An uncached release is added to the debrid by the Resolve above and then
		// takes minutes to land; answering 404 for that made the client remember a perfectly good release
		// as dead (and every uncached one below it, since they all fail the same way). 202 says "coming",
		// with whatever progress the store actually reports — the client polls this same URL and plays
		// when it turns into the usual 302.
		//
		// A FRESH context: the resolve above may have failed by exhausting `resolveBudget` (a slow add on
		// an uncached release is exactly that case), and reusing a spent context would turn every such
		// wait into a 404.
		statusCtx, statusCancel := context.WithTimeout(context.WithoutCancel(r.Context()), statusBudget)
		defer statusCancel()
		if status, ok := pool.Status(statusCtx, rt); ok {
			writeQueued(w, target.InfoHash, status)
			return
		}
		// An add scout already sent is not a refusal at all — the release is being fetched, by us, right
		// now. Answering 503 made the client tell the viewer their debrid was refusing AND stop trying
		// other sources, for a release scout had queued moments earlier. 202 is simply what is true.
		if errors.Is(err, errAddInFlight) {
			log.Printf("scout: play %s → 202, an add is already in flight", shortHash(target.InfoHash))
			writeQueued(w, target.InfoHash, StoreStatus{})
			return
		}
		// A refusal SCOUT made — its hourly allowance — is not the debrid refusing, and must not be
		// reported as one. errScoutSide was added to keep these apart in the refusal memory, and then the
		// route went on saying "torbox" anyway: the app told the viewer the debrid was refusing while
		// TorBox was answering perfectly well. Still a 503, because it is still "not now, try again", but
		// named as ours.
		if errors.Is(err, errScoutSide) {
			log.Printf("scout: play %s → 503 (scout-side), %v", shortHash(target.InfoHash), err)
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{"error": "scout_busy", "detail": scoutSideReason(err)}, noStore)
			return
		}
		// A debrid that throttled or faulted is not a dead release, and answering 404 made the two
		// identical: the client fell through every remaining source collecting the same non-answer, then
		// waited indefinitely on a download nothing had started. 503 says whose problem it is.
		var unavailable *StoreUnavailableError
		if errors.As(err, &unavailable) {
			log.Printf("scout: play %s → 503, %v", shortHash(target.InfoHash), err)
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]any{"error": "store_unavailable", "service": unavailable.Service}, noStore)
			return
		}
		// The client reads this 404 as "still fetching" and polls for as long as the viewer will sit
		// there, so it is worth saying which of the two happened: no store could add it, and no store
		// admits to downloading it either. A wait with nothing behind it is the one case a spinner
		// cannot distinguish from a slow release.
		log.Printf("scout: play %s → 404, no store resolved it and none reports a download: %v",
			shortHash(target.InfoHash), err)
		writeJSON(w, http.StatusNotFound, errBody("dead_link"), noStore)
		return
	}
	// Standard bodyless 302 (audit / AetherEngine fix): explicit Content-Length:0 so the Node/Go layer
	// doesn't send a chunked empty body that strict redirect-followers read as 0 bytes and fail on.
	w.Header().Set("location", link)
	w.Header().Set("cache-control", noStore)
	w.Header().Set("content-length", "0")
	w.WriteHeader(http.StatusFound)
}

type streamOut struct {
	Name          string           `json:"name"`
	Title         string           `json:"title"`
	URL           string           `json:"url"`
	Attributes    StreamAttributes `json:"attributes"`
	BehaviorHints streamHints      `json:"behaviorHints"`
}

type streamHints struct {
	BingeGroup  string `json:"bingeGroup"`
	NotWebReady bool   `json:"notWebReady"`
}

type streamsResponse struct {
	Streams []streamOut `json:"streams"`
}

func toStremioStream(s RawStream, sid *StreamID, origin, configBlob string) streamOut {
	token := encodePlayToken(PlayTarget{InfoHash: s.InfoHash, FileIdx: s.FileIdx, Season: seasonPtr(sid), Episode: episodePtr(sid)})
	return streamOut{
		Name:          "Den Scout",
		Title:         s.Title, // raw release name
		URL:           origin + "/" + configBlob + "/play/" + token,
		Attributes:    streamAttributes(s),
		BehaviorHints: streamHints{BingeGroup: "den-scout-" + sid.IMDb, NotWebReady: false},
	}
}

func seasonPtr(sid *StreamID) *int {
	if !sid.HasEp {
		return nil
	}
	s := sid.Season
	return &s
}
func episodePtr(sid *StreamID) *int {
	if !sid.HasEp {
		return nil
	}
	e := sid.Episode
	return &e
}

func streamCacheID(sid *StreamID) string {
	if sid.HasEp {
		return fmt.Sprintf("%s:%d:%d", sid.IMDb, sid.Season, sid.Episode)
	}
	return sid.IMDb
}

func rdOnly(config *Config) bool {
	if len(config.Debrid) == 0 {
		return false
	}
	for _, d := range config.Debrid {
		if d.Service != ServiceRealDebrid {
			return false
		}
	}
	return true
}

// publicOrigin: SCOUT_PUBLIC_URL when set (audit #8), else forwarded headers / Host.
func (h *handler) publicOrigin(r *http.Request) string {
	if h.deps.PublicURL != "" {
		return strings.TrimRight(h.deps.PublicURL, "/")
	}
	proto := firstHeader(r, "X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := firstHeader(r, "X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return proto + "://" + host
}

func firstHeader(r *http.Request, name string) string {
	v := r.Header.Get(name)
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// conditional serves a cacheable GET with ETag/If-None-Match → 304. etag may be precomputed ("" → hash).
func (h *handler) conditional(w http.ResponseWriter, r *http.Request, body, etag, contentType, cacheControl string) {
	if etag == "" {
		etag = etagFor(body)
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.Header().Set("etag", etag)
		w.Header().Set("cache-control", cacheControl)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("content-type", contentType)
	w.Header().Set("cache-control", cacheControl)
	w.Header().Set("etag", etag)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func etagFor(body string) string { return `"` + etagHex(body) + `"` }

func etagMatches(ifNoneMatch, etag string) bool {
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, tag := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimSpace(tag) == etag {
			return true
		}
	}
	return false
}

// A cached list is "<complete>[|<fresh-until-unix>]\x00<etag>\x00<body>". Completeness is stored with the
// entry because the cache-hit branch has to answer the same question the build did, and could not
// otherwise know; the freshness stamp is stored beside it because the entry now OUTLIVES its freshness
// (see staleServeWindow) and only the entry itself knows when it stopped being current.
//
// The stamp is a suffix on the existing first field rather than a fourth field, so that an entry written
// by an older build — a disk tier surviving a redeploy is exactly when that happens — still parses here,
// and so that the hot path stays two IndexByte calls and no allocation.
func splitCached(v string) (complete bool, freshUntil int64, etag, body string) {
	first := strings.IndexByte(v, '\x00')
	if first < 0 {
		// No separator at all: completeness unknown, so the same conservative answer as the legacy
		// one-separator case below. Claiming completeness here while denying it three lines down would be
		// two answers to one question.
		return false, 0, "", v
	}
	rest := v[first+1:]
	second := strings.IndexByte(rest, '\x00')
	if second < 0 {
		// An entry written before completeness was recorded. Treat it as INCOMPLETE: a redeploy is
		// exactly when short lists are in flight, and claiming completeness for one would serve it with a
		// five-minute max-age and a day of stale-if-error — the harm this format exists to prevent. The
		// cost of being wrong the other way is one extra scrape within the minute.
		return false, 0, v[:first], rest
	}
	flag := v[:first]
	// A stamp-less entry predates this format. It was written with its freshness AS its expiry, so the
	// cache holding it at all is proof it is still fresh — freshUntil 0 says exactly that.
	if bar := strings.IndexByte(flag, '|'); bar >= 0 {
		freshUntil, _ = strconv.ParseInt(flag[bar+1:], 10, 64)
		flag = flag[:bar]
	}
	return flag == "1", freshUntil, rest[:second], rest[second+1:]
}

func joinCached(complete bool, freshUntil int64, etag, body string) string {
	flag := "0"
	if complete {
		flag = "1"
	}
	if freshUntil > 0 {
		flag += "|" + strconv.FormatInt(freshUntil, 10)
	}
	return flag + "\x00" + etag + "\x00" + body
}

func writeJSON(w http.ResponseWriter, status int, body any, cacheControl string) {
	b, _ := json.Marshal(body)
	w.Header().Set("content-type", jsonType)
	if cacheControl != "" {
		w.Header().Set("cache-control", cacheControl)
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func at(parts []string, i int) string {
	if i < len(parts) {
		return parts[i]
	}
	return ""
}
