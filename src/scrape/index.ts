/**
 * Indexer fan-out (SCOUT-02). Scrape all configured indexers concurrently, each under its own
 * timeout, and gather whatever responded — a slow or dead indexer never blocks the others or fails
 * the request. Then dedupe by infohash so the same torrent from three indexers becomes one row.
 */
import type { Indexer } from "../config.js";
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

/** Build the scrapers for the configured indexers, using `urls` (defaults + any env overrides). */
export function makeScrapers(
  indexers: Indexer[],
  fetch: FetchLike,
  urls: Partial<Record<Indexer, string>> = {},
): Scraper[] {
  return indexers.map((id) => new StremioAddonScraper(id, urls[id] ?? DEFAULT_INDEXER_URLS[id], fetch));
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
