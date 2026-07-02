/**
 * A Stremio stream addon as a Scout indexer. Torrentio, Comet, MediaFusion and Torz all speak the
 * same `GET <base>/stream/<type>/<id>.json` protocol and emit the annotated-stream shape, so one
 * configured client covers all four — the base URL is the only thing that differs (MediaFusion's
 * base includes its encrypted-config segment; see the deploy doc). Reuse over four near-clones.
 */
import type { Indexer } from "../config.js";
import type { FetchLike, RawStreamSeed, Scraper, ScrapeQuery } from "./types.js";
import { parseStremioStreams } from "./parse.js";

export class StremioAddonScraper implements Scraper {
  constructor(
    readonly id: Indexer,
    private readonly baseUrl: string,
    private readonly fetch: FetchLike,
  ) {}

  async scrape(query: ScrapeQuery, signal: AbortSignal): Promise<RawStreamSeed[]> {
    const stremId = query.type === "series" ? `${query.imdbId}:${query.season}:${query.episode}` : query.imdbId;
    const url = `${this.baseUrl.replace(/\/$/, "")}/stream/${query.type}/${encodeURIComponent(stremId)}.json`;
    const res = await this.fetch(url, { signal, headers: { accept: "application/json" } });
    if (!res.ok) throw new Error(`${this.id} http ${res.status}`);
    const body = await res.json();
    return parseStremioStreams(body, this.id);
  }
}
