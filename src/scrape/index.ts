/**
 * Indexer fan-out (SCOUT-02). Scrape all configured indexers concurrently, each under its own
 * timeout, and gather whatever responded — a slow or dead indexer never blocks the others or fails
 * the request. Then dedupe by infohash so the same torrent from three indexers becomes one row.
 */
import type { Indexer, ScoutConfig } from "../config.js";
import type { FetchLike, RawStreamSeed, Scraper, ScrapeQuery } from "./types.js";
import { StremioAddonScraper } from "./addon.js";
import { withTimeout } from "../util.js";

/** Public defaults; override per-indexer via env in the entry point (see deps.ts / deploy doc). */
export const DEFAULT_INDEXER_URLS: Record<Indexer, string> = {
  torrentio: "https://torrentio.strem.fun",
  comet: "https://comet.elfhosted.com",
  mediafusion: "https://mediafusion.elfhosted.com",
  torz: "https://torz.strem.fun",
};

/**
 * Build the scrapers for the configured indexers. Base URLs come from `urls` (env overrides) or the
 * public defaults; Torrentio additionally gets automated options derived from the config so we get a
 * good candidate set without exposing Torrentio's whole option matrix in Scout's UI (Scout ranks +
 * resolves server-side, so debrid/provider/language selection is unnecessary here).
 */
export function makeScrapers(
  config: ScoutConfig,
  fetch: FetchLike,
  urls: Partial<Record<Indexer, string>> = {},
): Scraper[] {
  return config.indexers.map((id) => new StremioAddonScraper(id, baseURL(id, config, urls), fetch));
}

function baseURL(id: Indexer, config: ScoutConfig, urls: Partial<Record<Indexer, string>>): string {
  if (urls[id]) return urls[id]!; // an explicit env override is used verbatim (self-hosted instances)
  if (id === "torrentio") return torrentioBase(config);
  return DEFAULT_INDEXER_URLS[id];
}

/**
 * Torrentio takes its options as a path segment (`/opt=a|opt=b/stream/…`). We DON'T pass the debrid
 * token — Scout does its own cache-check + resolve on the raw infohashes, so Torrentio must return
 * torrents, not pre-resolved links. We only automate the safe, config-derived bits: best-first sort,
 * and dropping CAM/screener at the source when the user's filter asks for it (Scout's ranker also
 * drops them, so this just trims payload). Resolution filtering stays in Scout, which keeps untagged.
 */
function torrentioBase(config: ScoutConfig): string {
  const opts = ["sort=qualitysize"];
  if (config.filters.excludeCam) opts.push("qualityfilter=cam,scr");
  return `${DEFAULT_INDEXER_URLS.torrentio}/${opts.join("|")}`;
}

/** Run every scraper concurrently under a per-indexer timeout; drop the ones that error/time out. */
export async function scrapeAll(scrapers: Scraper[], query: ScrapeQuery, timeoutMs: number): Promise<RawStreamSeed[]> {
  const settled = await Promise.allSettled(
    scrapers.map((s) => withTimeout((signal) => s.scrape(query, signal), timeoutMs)),
  );
  const seeds = settled.flatMap((r) => (r.status === "fulfilled" ? r.value : []));
  return dedupe(seeds);
}

/**
 * Dedupe by infohash, merging the richest facts across indexers: keep the first-seen title/source,
 * but fill a missing `fileIdx`/`sizeBytes` and take the max `seeders` (indexers disagree, and the
 * highest count is the most current). First-seen order is preserved (a stable tiebreak for the rank).
 */
export function dedupe(seeds: RawStreamSeed[]): RawStreamSeed[] {
  const byHash = new Map<string, RawStreamSeed>();
  for (const seed of seeds) {
    const existing = byHash.get(seed.infoHash);
    if (!existing) {
      byHash.set(seed.infoHash, { ...seed });
      continue;
    }
    if (existing.fileIdx == null && seed.fileIdx != null) existing.fileIdx = seed.fileIdx;
    if (existing.sizeBytes == null && seed.sizeBytes != null) existing.sizeBytes = seed.sizeBytes;
    if ((seed.seeders ?? 0) > (existing.seeders ?? 0)) existing.seeders = seed.seeders;
  }
  return [...byHash.values()];
}
