package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
}

type handler struct {
	deps Deps
	sf   singleflight.Group

	// Precomputed static responses (audit #15 — no per-request rebuild/rehash).
	manifestUnconf     string
	manifestUnconfETag string
	configureETag      string
}

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
	path := r.URL.Path
	switch path {
	case "/", "/configure", "/configure/":
		h.conditional(w, r, configurePage, h.configureETag, htmlType, staticCache)
		return
	case "/health":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"}, noStore)
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

	v, _, _ := h.sf.Do(cacheKey, func() (any, error) {
		return h.buildStreamList(r.Context(), config, configBlob, sid, origin, cacheKey), nil
	})
	etag, body := splitCached(v.(string))
	h.conditional(w, r, body, etag, jsonType, listCache)
}

// buildStreamList scrapes → cache-checks → ranks → serializes, caches "<etag>\x00<body>", returns it.
func (h *handler) buildStreamList(ctx context.Context, config *Config, configBlob string, sid *StreamID, origin, cacheKey string) string {
	q := scrapeQuery{Type: sid.Type, IMDb: sid.IMDb, Season: sid.Season, Episode: sid.Episode, HasEp: sid.HasEp}
	seeds := scrapeAll(ctx, h.deps.MakeScrapers(config), q, h.deps.ScrapeTimeout)

	pool := &StorePool{stores: h.deps.MakeStores(config)}
	hashes := make([]string, len(seeds))
	for i, s := range seeds {
		hashes[i] = s.InfoHash
	}
	truth := pool.CacheCheck(ctx, hashes)
	for i := range seeds {
		seeds[i].Cached = truth[seeds[i].InfoHash]
	}

	// audit #4: with no cache-truth store (RD-only), the cached-only filter would drop everything.
	effCachedOnly := config.CachedOnly && hasCacheTruth(config)
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

	ranked := rankStreams(seeds, rankFilters{
		ExcludeCam:   config.Filters.ExcludeCam,
		Resolutions:  config.Filters.Resolutions,
		HDROnly:      config.Filters.HDROnly,
		MinSeeders:   config.Filters.MinSeeders,
		MaxSizeGB:    config.Filters.MaxSizeGB,
		ExcludeRegex: config.Filters.ExcludeRegex,
		CachedOnly:   effCachedOnly,
		ResultCap:    config.ResultCap,
	})

	out := make([]streamOut, 0, len(ranked))
	for _, s := range ranked {
		out = append(out, toStremioStream(s, sid, origin, configBlob))
	}
	body, _ := json.Marshal(streamsResponse{Streams: out})
	etag := etagFor(string(body))
	value := etag + "\x00" + string(body)
	h.deps.Cache.Put(cacheKey, value, h.deps.ListTTL)
	return value
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
	link, err := pool.Resolve(r.Context(), ResolveTarget{InfoHash: target.InfoHash, FileIdx: target.FileIdx, Season: target.Season, Episode: target.Episode})
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
