import { describe, it, expect } from "vitest";
import { settingsFromEnv, buildDeps } from "../src/deps.js";
import { MemoryCache } from "../src/cache.js";
import type { FetchLike } from "../src/scrape/types.js";
import type { ScoutConfig } from "../src/config.js";

const fetchStub: FetchLike = async () => new Response("{}");

describe("settingsFromEnv", () => {
  it("uses defaults when unset", () => {
    const s = settingsFromEnv({});
    expect(s.scrapeTimeoutMs).toBe(8000);
    expect(s.listTtlSeconds).toBe(300);
    expect(s.indexerUrls).toEqual({});
  });

  it("reads timeouts and per-indexer URL overrides", () => {
    const s = settingsFromEnv({
      SCOUT_SCRAPE_TIMEOUT_MS: "5000",
      SCOUT_LIST_TTL_SECONDS: "60",
      SCOUT_MEDIAFUSION_URL: "https://mf.self/CONFIG",
      SCOUT_SCRAPE_TIMEOUT_MS_BAD: "x",
    });
    expect(s.scrapeTimeoutMs).toBe(5000);
    expect(s.listTtlSeconds).toBe(60);
    expect(s.indexerUrls.mediafusion).toBe("https://mf.self/CONFIG");
  });

  it("falls back on non-numeric / non-positive values", () => {
    expect(settingsFromEnv({ SCOUT_SCRAPE_TIMEOUT_MS: "-1", SCOUT_LIST_TTL_SECONDS: "abc" })).toMatchObject({
      scrapeTimeoutMs: 8000,
      listTtlSeconds: 300,
    });
  });
});

describe("buildDeps", () => {
  it("wires real scraper + store factories from settings", () => {
    const deps = buildDeps(fetchStub, settingsFromEnv({}), new MemoryCache());
    const config: ScoutConfig = { debrid: [{ service: "torbox", token: "t" }], indexers: ["torrentio", "comet"], filters: { excludeCam: true }, cachedOnly: true, resultCap: 20 };
    const scrapers = deps.makeScrapers(config, fetchStub);
    expect(scrapers.map((s) => s.id)).toEqual(["torrentio", "comet"]);
    expect(deps.makeStores(config, fetchStub).map((s) => s.service)).toEqual(["torbox"]);
    expect(deps.scrapeTimeoutMs).toBe(8000);
  });
});
