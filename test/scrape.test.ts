import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { StremioAddonScraper } from "../src/scrape/addon.js";
import { makeScrapers, scrapeAll, dedupe, DEFAULT_INDEXER_URLS } from "../src/scrape/index.js";
import type { FetchLike, RawStreamSeed, Scraper, ScrapeQuery } from "../src/scrape/types.js";

function fixture(name: string): unknown {
  return JSON.parse(readFileSync(fileURLToPath(new URL(`./fixtures/${name}`, import.meta.url)), "utf8"));
}

function jsonFetch(body: unknown, status = 200): FetchLike {
  return async () => new Response(JSON.stringify(body), { status });
}

describe("StremioAddonScraper vs fixtures", () => {
  it("builds the movie stream URL and parses torrent rows (drops the url-only row)", async () => {
    let seenUrl = "";
    const fetch: FetchLike = async (url) => {
      seenUrl = url;
      return new Response(JSON.stringify(fixture("torrentio-movie.json")), { status: 200 });
    };
    const scraper = new StremioAddonScraper("torrentio", "https://torrentio.strem.fun", fetch);
    const seeds = await scraper.scrape({ type: "movie", imdbId: "tt1234567" }, new AbortController().signal);

    expect(seenUrl).toBe("https://torrentio.strem.fun/stream/movie/tt1234567.json");
    expect(seeds.map((s) => s.infoHash)).toEqual([
      "aabbccddeeff00112233445566778899aabbccdd",
      "1111111111111111111111111111111111111111",
      "2222222222222222222222222222222222222222",
    ]);
    expect(seeds[0]).toMatchObject({ seeders: 142, fileIdx: 0, source: "torrentio" });
    expect(seeds[0].sizeBytes).toBe(Math.round(18.4 * 1_073_741_824));
  });

  it("builds the series stream URL (id:S:E) and reads Seeders/Size text", async () => {
    let seenUrl = "";
    const fetch: FetchLike = async (url) => {
      seenUrl = url;
      return new Response(JSON.stringify(fixture("comet-series.json")), { status: 200 });
    };
    const scraper = new StremioAddonScraper("comet", "https://comet.example/", fetch);
    const seeds = await scraper.scrape({ type: "series", imdbId: "tt99", season: 2, episode: 5 }, new AbortController().signal);

    expect(seenUrl).toBe("https://comet.example/stream/series/tt99%3A2%3A5.json");
    expect(seeds[0]).toMatchObject({ fileIdx: 4, seeders: 40 });
    expect(seeds[0].sizeBytes).toBe(Math.round(4.2 * 1_073_741_824));
  });

  it("throws on a non-200 (so the fan-out treats it as no result)", async () => {
    const scraper = new StremioAddonScraper("torz", "https://torz.example", jsonFetch({}, 502));
    await expect(scraper.scrape({ type: "movie", imdbId: "tt1" }, new AbortController().signal)).rejects.toThrow();
  });
});

describe("makeScrapers", () => {
  it("uses defaults and honors per-indexer overrides", () => {
    const scrapers = makeScrapers(["torrentio", "mediafusion"], jsonFetch({}), { mediafusion: "https://mf.self/CONFIG" });
    expect(scrapers.map((s) => s.id)).toEqual(["torrentio", "mediafusion"]);
    expect(DEFAULT_INDEXER_URLS.torrentio).toContain("torrentio");
  });
});

describe("scrapeAll fan-out", () => {
  const query: ScrapeQuery = { type: "movie", imdbId: "tt1" };
  const seed = (infoHash: string, over: Partial<RawStreamSeed> = {}): RawStreamSeed => ({ infoHash, title: "t", source: "x", ...over });

  function fake(id: string, behavior: () => Promise<RawStreamSeed[]>): Scraper {
    return { id: id as Scraper["id"], scrape: () => behavior() };
  }

  it("gathers what responded; a thrown or timed-out indexer is dropped, not fatal", async () => {
    const ok = fake("torrentio", async () => [seed("a".repeat(40))]);
    const boom = fake("comet", async () => {
      throw new Error("down");
    });
    const hang = fake("torz", () => new Promise<never>(() => {})); // never resolves → timeout
    const out = await scrapeAll([ok, boom, hang], query, 30);
    expect(out.map((s) => s.infoHash)).toEqual(["a".repeat(40)]);
  });

  it("dedupes across indexers by infohash, merging facts", async () => {
    const a = fake("torrentio", async () => [seed("h".repeat(40), { title: "first", seeders: 10 })]);
    const b = fake("comet", async () => [seed("h".repeat(40), { title: "second", seeders: 99, fileIdx: 3, sizeBytes: 500 })]);
    const out = await scrapeAll([a, b], query, 100);
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({ title: "first", seeders: 99, fileIdx: 3, sizeBytes: 500 });
  });
});

describe("dedupe", () => {
  it("keeps first, fills missing fileIdx/size, takes max seeders", () => {
    const out = dedupe([
      { infoHash: "x".repeat(40), title: "a", source: "t", seeders: 5 },
      { infoHash: "x".repeat(40), title: "b", source: "c", seeders: 50, fileIdx: 2, sizeBytes: 10 },
      { infoHash: "y".repeat(40), title: "c", source: "t" },
    ]);
    expect(out).toHaveLength(2);
    expect(out[0]).toMatchObject({ title: "a", seeders: 50, fileIdx: 2, sizeBytes: 10 });
  });
});
