import type { Indexer } from "../config.js";
import type { RawStream } from "../rank.js";

/** A scrape result before the debrid cache-check adds `cached`. */
export type RawStreamSeed = Omit<RawStream, "cached">;

export interface ScrapeQuery {
  type: "movie" | "series";
  imdbId: string;
  season?: number;
  episode?: number;
}

/** Fetch signature — `globalThis.fetch`, an undici fetch, or a test double. */
export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

/** A single indexer. Bound to its base URL + `fetch` at construction; `scrape` just runs one query. */
export interface Scraper {
  readonly id: Indexer;
  scrape(query: ScrapeQuery, signal: AbortSignal): Promise<RawStreamSeed[]>;
}
