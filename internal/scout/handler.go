package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// The runtime-agnostic core as an http.Handler (ported from src/handler.ts). Off-device by design:
// the app never sees a torrent or a debrid token. Routes are served at the service root.

const (
	staticCache    = "public, max-age=3600"
	noStore        = "no-store"
	jsonType       = "application/json"
	htmlType       = "text/html; charset=utf-8"
	defaultTimeout = 8 * time.Second
	defaultListTTL = 5 * time.Minute
	// Headroom over the scrape timeout for the cache-check phase of a detached list build.
	listBuildSlack = 20 * time.Second
	// Hard cap on a /play resolve (addMagnet→select→unrestrict across stores) so a slow debrid account
	// can't pin a goroutine/connection indefinitely.
	resolveBudget = 45 * time.Second
	// Upper bound on scraped seeds fed into the cache-check fan-out (guards outbound amplification).
	maxSeeds = 500
)

// Deps injects the environment: the cache, timeouts, public origin, and the scraper/store factories
// (mocked in tests).
type Deps struct {
	Cache         Cache
	ScrapeTimeout time.Duration
	ListTTL       time.Duration
	PublicURL     string // audit #8: fixed public origin; when empty, fall back to forwarded headers
	MakeScrapers  func(*Config) []scraper
	MakeStores    func(*Config) []Store
	// MetaYear resolves an id → release year (movies only) so mistagged torrents can be dropped. Optional
	// (nil = no year filter); a lookup failure returns ok=false and the list is served unfiltered.
	MetaYear func(ctx context.Context, typ, imdb string) (int, bool)
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
	path := r.URL.Path
	switch path {
	case "/", "/configure", "/configure/":
		h.conditional(w, r, configurePage, h.configureETag, htmlType, staticCache)
		return
	case "/health":
		// Stays 200 (liveness — don't trip the container HEALTHCHECK), but reports the degraded scrape
		// state so a monitor sees a total-indexer outage instead of just "empty results".
		status := map[string]string{"status": "ok"}
		if h.scrapeFails.Load() >= scrapeFailThreshold {
			status = map[string]string{"status": "degraded", "reason": "indexers"}
		}
		writeJSON(w, http.StatusOK, status, noStore)
		return
	case "/manifest.json":
		h.conditional(w, r, h.manifestUnconf, h.manifestUnconfETag, jsonType, staticCache)
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
		config, ok := decodeConfig(configBlob)
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
	origin := h.publicOrigin(r)
	// audit #7 (collision-resistant key) + #8 (origin part) + #16 (key off the raw blob, decode later).
	cacheKey := "list:" + keyHash(configBlob) + ":" + keyHash(origin) + ":" + streamCacheID(sid)

	if hit, ok := h.deps.Cache.Get(cacheKey); ok {
		etag, body := splitCached(hit)
		h.conditional(w, r, body, etag, jsonType, listCache)
		return
	}

	// Miss: decode now (#16 — a warm hit never pays decode/validate).
	config, ok := decodeConfig(configBlob)
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
		value, degraded := h.buildStreamList(buildCtx, config, configBlob, sid, origin, cacheKey)
		return buildResult{value: value, degraded: degraded}, nil
	})
	res := v.(buildResult)
	// Signal a degraded build so the app can say "sources temporarily unavailable" rather than treating
	// an empty list as "no results" (a total indexer/cache-check outage otherwise looks identical).
	if res.degraded != "" {
		w.Header().Set("X-Scout-Degraded", res.degraded)
	}
	etag, body := splitCached(res.value)
	h.conditional(w, r, body, etag, jsonType, listCache)
}

// buildResult carries the singleflight build's body plus a degraded reason ("" when healthy).
type buildResult struct {
	value    string
	degraded string
}

// buildStreamList scrapes → cache-checks → ranks → serializes, caches "<etag>\x00<body>", returns it.
func (h *handler) buildStreamList(ctx context.Context, config *Config, configBlob string, sid *StreamID, origin, cacheKey string) (string, string) {
	q := scrapeQuery{Type: sid.Type, IMDb: sid.IMDb, Season: sid.Season, Episode: sid.Episode, HasEp: sid.HasEp}
	seeds, scrapeOK := scrapeAll(ctx, h.deps.MakeScrapers(config), q, h.deps.ScrapeTimeout)

	// Cap the seed set before the cache-check fan-out: a misbehaving/hostile indexer returning thousands
	// of tiny stream objects would otherwise mean hundreds of concurrent outbound debrid requests. The
	// cap is well above any real title's stream count, so it can't drop meaningful results.
	if len(seeds) > maxSeeds {
		seeds = seeds[:maxSeeds]
	}

	pool := &StorePool{stores: h.deps.MakeStores(config)}
	hashes := make([]string, len(seeds))
	for i, s := range seeds {
		hashes[i] = s.InfoHash
	}
	truth, truthOK := pool.CacheCheck(ctx, hashes)
	for i := range seeds {
		seeds[i].Cached = truth[seeds[i].InfoHash]
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

	truthDegraded := hasCacheTruth(config) && !truthOK
	degradedReason := ""
	if !scrapeOK {
		degradedReason = "indexers"
	} else if truthDegraded {
		degradedReason = "cache-check"
	}
	degraded := degradedReason != ""

	// audit #4: with no cache-truth store (RD-only), the cached-only filter would drop everything. Also
	// skip it when the cache-truth stores are unreachable this request (don't drop everything on a blip).
	effCachedOnly := config.CachedOnly && hasCacheTruth(config) && !truthDegraded
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

	// Expected release year (movies) → drop torrents mistagged with another film's id. Best-effort: a
	// lookup failure just means no year filter.
	var expectedYear *int
	if h.deps.MetaYear != nil {
		if y, ok := h.deps.MetaYear(ctx, sid.Type, sid.IMDb); ok {
			expectedYear = &y
		}
	}

	ranked := rankStreams(seeds, rankFilters{
		ExcludeCam:   config.Filters.ExcludeCam,
		Resolutions:  config.Filters.Resolutions,
		HDROnly:      config.Filters.HDROnly,
		MinSeeders:   config.Filters.MinSeeders,
		MaxSizeGB:    config.Filters.MaxSizeGB,
		ExcludeRegex: config.Filters.ExcludeRegex,
		CachedOnly:   effCachedOnly,
		ResultCap:    config.ResultCap,
		ExpectedYear: expectedYear,
	})

	out := make([]streamOut, 0, len(ranked))
	for _, s := range ranked {
		out = append(out, toStremioStream(s, sid, origin, configBlob))
	}
	body, _ := json.Marshal(streamsResponse{Streams: out})
	etag := etagFor(string(body))
	value := etag + "\x00" + string(body)
	if !degraded {
		h.deps.Cache.Put(cacheKey, value, h.deps.ListTTL)
	}
	return value, degradedReason
}

func (h *handler) handlePlay(w http.ResponseWriter, r *http.Request, configBlob string, parts []string) {
	target, ok := decodePlayToken(at(parts, 2))
	if !ok {
		writeJSON(w, http.StatusBadRequest, errBody("bad_token"), noStore)
		return
	}
	config, ok := decodeConfig(configBlob)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errBody("bad_config"), noStore)
		return
	}
	pool := &StorePool{stores: h.deps.MakeStores(config)}
	ctx, cancel := context.WithTimeout(r.Context(), resolveBudget)
	defer cancel()
	link, err := pool.Resolve(ctx, ResolveTarget{InfoHash: target.InfoHash, FileIdx: target.FileIdx, Season: target.Season, Episode: target.Episode})
	if err != nil {
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

func splitCached(v string) (etag, body string) {
	if i := strings.IndexByte(v, '\x00'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return "", v
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
