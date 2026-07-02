/**
 * Wire the runtime-agnostic core to a concrete environment: an injected `fetch`, a cache backend,
 * and the real scraper/store factories. Settings come from env so the same image runs anywhere
 * (see the deploy doc for the vars). Tests build `ScoutDeps` directly with mocks instead.
 */
import { INDEXERS, type Indexer } from "./config.js";
import { MemoryCache, type Cache } from "./cache.js";
import type { FetchLike } from "./scrape/types.js";
import type { ScoutDeps } from "./handler.js";
import { makeScrapers } from "./scrape/index.js";
import { buildStores } from "./stores/index.js";

export interface ScoutSettings {
  scrapeTimeoutMs: number;
  listTtlSeconds: number;
  /** Per-indexer base-URL overrides (e.g. a self-hosted MediaFusion whose base includes its config). */
  indexerUrls: Partial<Record<Indexer, string>>;
}

export function settingsFromEnv(env: Record<string, string | undefined>): ScoutSettings {
  const indexerUrls: Partial<Record<Indexer, string>> = {};
  for (const id of INDEXERS) {
    const override = env[`SCOUT_${id.toUpperCase()}_URL`];
    if (override) indexerUrls[id] = override;
  }
  return {
    scrapeTimeoutMs: intEnv(env.SCOUT_SCRAPE_TIMEOUT_MS, 8000),
    listTtlSeconds: intEnv(env.SCOUT_LIST_TTL_SECONDS, 300),
    indexerUrls,
  };
}

export function buildDeps(fetchImpl: FetchLike, settings: ScoutSettings, cache: Cache = new MemoryCache()): ScoutDeps {
  return {
    fetch: fetchImpl,
    cache,
    makeScrapers: (config, fetch) => makeScrapers(config, fetch, settings.indexerUrls),
    makeStores: (config, fetch) => buildStores(config, fetch),
    scrapeTimeoutMs: settings.scrapeTimeoutMs,
    listTtlSeconds: settings.listTtlSeconds,
  };
}

function intEnv(value: string | undefined, fallback: number): number {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : fallback;
}
