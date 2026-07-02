/**
 * Den Scout — the runtime-agnostic core (EPIC-den-scout). A single `handleScout(request, deps)` over
 * Web `Request`/`Response`, so it runs unchanged on Bun, Node (via @hono/node-server) or a CF Worker;
 * `deps` injects `fetch`, the cache, and the scraper/store factories (mocked in tests).
 *
 * Off-device by design: the app never sees CAM/torrent-shaped rows or holds a debrid token. Scout
 * scrapes indexers → dedupes by infohash → cache-checks the debrid store → ranks (VortX `junkClass`
 * + additive score) → returns a short list of clean, cached `https` streams whose `url` is a lazy
 * `/play` proxy. The token is used ONLY in `/play`, server-side.
 *
 * Routes (config is the base64url blob, Torrentio-style; served at the service root):
 *   GET /                                  → configure page
 *   GET /configure                         → configure page
 *   GET /health                            → { status: "ok" }
 *   GET /manifest.json                     → unconfigured manifest (configurationRequired)
 *   GET /<config>/manifest.json            → configured manifest
 *   GET /<config>/stream/<type>/<id>.json  → ranked, clean, cached streams
 *   GET /<config>/play/<token>             → 302 to the debrid link (only place the token is used)
 */
import { decodeConfig, type Indexer, type ScoutConfig } from "./config.js";
import { buildManifest } from "./manifest.js";
import { CONFIGURE_PAGE } from "./configure.js";
import { parseStreamId, type StreamId } from "./id.js";
import { rankStreams, type RawStream } from "./rank.js";
import { cleanLabel } from "./label.js";
import { decodePlayToken, encodePlayToken } from "./play.js";
import { scrapeAll } from "./scrape/index.js";
import type { FetchLike, Scraper } from "./scrape/types.js";
import { StorePool } from "./stores/index.js";
import type { Store } from "./stores/types.js";
import type { Cache } from "./cache.js";
import { fnv1a, html, json } from "./util.js";

export interface ScoutDeps {
  fetch: FetchLike;
  cache: Cache;
  makeScrapers: (indexers: Indexer[], fetch: FetchLike) => Scraper[];
  makeStores: (config: ScoutConfig, fetch: FetchLike) => Store[];
  /** Per-indexer scrape timeout (ms). */
  scrapeTimeoutMs: number;
  /** How long a ranked stream list stays cached (s). Cached truth is embedded — only cached rows survive. */
  listTtlSeconds: number;
}

export async function handleScout(request: Request, deps: ScoutDeps): Promise<Response> {
  const url = new URL(request.url);
  const path = url.pathname;

  if (path === "/" || path === "/configure" || path === "/configure/") return html(CONFIGURE_PAGE);
  if (path === "/health") return json({ status: "ok" });
  if (path === "/manifest.json") return json(buildManifest(null));

  const parts = path.split("/").filter(Boolean); // ["<config>", "stream"|"play"|"manifest.json", ...]
  const configBlob = parts[0] ?? "";
  const config = decodeConfig(configBlob);
  if (!config) return json({ error: "bad_config" }, 400);

  const resource = parts[1];
  if (resource === "manifest.json") return json(buildManifest(config));
  if (resource === "stream") return handleStream(request, config, configBlob, parts, deps);
  if (resource === "play") return handlePlay(config, parts, deps);
  return json({ error: "not_found" }, 404);
}

async function handleStream(
  request: Request,
  config: ScoutConfig,
  configBlob: string,
  parts: string[],
  deps: ScoutDeps,
): Promise<Response> {
  const sid = parseStreamId(parts[2] ?? "", parts[3] ?? "");
  if (!sid) return json({ error: "bad_id" }, 400);

  // Key the cache by the FNV of the config blob, NOT the token — the cache holds no secret.
  const cacheKey = `list:${fnv1a(configBlob)}:${streamCacheId(sid)}`;
  const hit = await deps.cache.get(cacheKey);
  if (hit) return json(JSON.parse(hit));

  const scrapers = deps.makeScrapers(config.indexers, deps.fetch);
  const seeds = await scrapeAll(
    scrapers,
    { type: sid.type, imdbId: sid.imdbId, season: sid.season, episode: sid.episode },
    deps.scrapeTimeoutMs,
  );

  const pool = new StorePool(deps.makeStores(config, deps.fetch));
  const truth = await pool.cacheCheck(seeds.map((s) => s.infoHash));
  const streams: RawStream[] = seeds.map((s) => ({ ...s, cached: truth.get(s.infoHash) ?? false }));

  const ranked = rankStreams(streams, {
    excludeCam: config.filters.excludeCam,
    resolutions: config.filters.resolutions,
    hdrOnly: config.filters.hdrOnly,
    minSeeders: config.filters.minSeeders,
    maxSizeGB: config.filters.maxSizeGB,
    excludeRegex: config.filters.excludeRegex,
    cachedOnly: config.cachedOnly,
    resultCap: config.resultCap,
  });

  const origin = publicOrigin(request, new URL(request.url));
  const body = { streams: ranked.map((s) => toStremioStream(s, sid, origin, configBlob)) };
  await deps.cache.put(cacheKey, JSON.stringify(body), deps.listTtlSeconds);
  return json(body);
}

async function handlePlay(config: ScoutConfig, parts: string[], deps: ScoutDeps): Promise<Response> {
  const target = decodePlayToken(parts[2] ?? "");
  if (!target) return json({ error: "bad_token" }, 400);

  const pool = new StorePool(deps.makeStores(config, deps.fetch));
  try {
    const link = await pool.resolve(target);
    // 302 (not 301) — the debrid link is freshly minted per play and must not be cached by the client.
    return new Response(null, { status: 302, headers: { location: link } });
  } catch {
    // The pool has already tried every store; nothing could deliver the file → dead link. 404 lets
    // the Stremio client fall through to the next stream instead of hard-failing playback.
    return json({ error: "dead_link" }, 404);
  }
}

/** Cache id for a title: the bare imdb id (movie) or `tt…:S:E` (a specific episode). */
function streamCacheId(sid: StreamId): string {
  return sid.type === "series" ? `${sid.imdbId}:${sid.season}:${sid.episode}` : sid.imdbId;
}

function toStremioStream(s: RawStream, sid: StreamId, origin: string, configBlob: string): Record<string, unknown> {
  const token = encodePlayToken({ infoHash: s.infoHash, fileIdx: s.fileIdx, season: sid.season, episode: sid.episode });
  return {
    name: "Den Scout",
    title: cleanLabel(s),
    url: `${origin}/${configBlob}/play/${token}`,
    behaviorHints: {
      // Group a show's episodes so Den's Up-Next (V2-03) auto-advances within the same source.
      bingeGroup: `den-scout-${sid.imdbId}`,
      notWebReady: false,
    },
  };
}

/**
 * The public origin to build `/play` URLs from. Behind Caddy the socket is plain http on an internal
 * host, but Caddy forwards `X-Forwarded-Proto` + `Host`, so honor those to emit the correct
 * `https://scout.<domain>` the app can reach (matches the trailer-service convention).
 */
function publicOrigin(request: Request, url: URL): string {
  const proto = request.headers.get("x-forwarded-proto")?.split(",")[0]?.trim() || url.protocol.replace(/:$/, "");
  const host =
    request.headers.get("x-forwarded-host")?.split(",")[0]?.trim() || request.headers.get("host") || url.host;
  return `${proto}://${host}`;
}
