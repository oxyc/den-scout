import { describe, it, expect, vi } from "vitest";
import { handleScout, type ScoutDeps } from "../src/handler.js";
import { encodeConfig, type ScoutConfig } from "../src/config.js";
import { MemoryCache } from "../src/cache.js";
import { decodePlayToken } from "../src/play.js";
import type { RawStreamSeed, Scraper } from "../src/scrape/types.js";
import type { ResolveTarget, Store } from "../src/stores/types.js";
import { DeadLinkError } from "../src/stores/types.js";

const CONFIG: ScoutConfig = {
  debrid: [{ service: "torbox", token: "tb-secret" }],
  indexers: ["torrentio"],
  filters: { excludeCam: true },
  cachedOnly: true,
  resultCap: 20,
};
const BLOB = encodeConfig(CONFIG);

const SEEDS: RawStreamSeed[] = [
  { infoHash: "a".repeat(40), title: "Movie 2160p WEB-DL HDR", sizeBytes: 18 * 1_073_741_824, seeders: 100, source: "torrentio", fileIdx: 0 },
  { infoHash: "b".repeat(40), title: "Movie 1080p WEB-DL", sizeBytes: 8 * 1_073_741_824, seeders: 50, source: "torrentio", fileIdx: 0 },
  { infoHash: "c".repeat(40), title: "Movie 2160p HDCAM", sizeBytes: 2 * 1_073_741_824, seeders: 3, source: "torrentio" },
];

function deps(over: Partial<ScoutDeps> = {}): ScoutDeps {
  const cacheStore: Store = {
    service: "torbox",
    // 'a' + 'b' cached, the CAM 'c' not — proves cachedOnly + excludeCam both bite.
    cacheCheck: async (hashes) => new Map(hashes.map((h) => [h, h !== "c".repeat(40)])),
    resolve: async (t: ResolveTarget) => `https://cdn.torbox/${t.infoHash}.mkv`,
  };
  const scraper: Scraper = { id: "torrentio", scrape: async () => SEEDS };
  return {
    fetch: async () => new Response("{}"),
    cache: new MemoryCache(),
    makeScrapers: () => [scraper],
    makeStores: () => [cacheStore],
    scrapeTimeoutMs: 100,
    listTtlSeconds: 300,
    ...over,
  };
}

async function call(path: string, d = deps(), init?: RequestInit): Promise<Response> {
  return handleScout(new Request(`https://scout.example${path}`, init), d);
}

describe("scout routes — pages + manifest", () => {
  it("/ and /configure serve the config page", async () => {
    for (const p of ["/", "/configure"]) {
      const res = await call(p);
      expect(res.status).toBe(200);
      expect(res.headers.get("content-type")).toContain("text/html");
      expect(await res.text()).toContain("Configure Den Scout");
    }
  });

  it("/health is ok", async () => {
    expect(await (await call("/health")).json()).toEqual({ status: "ok" });
  });

  it("/manifest.json is configurationRequired", async () => {
    const m = (await (await call("/manifest.json")).json()) as { behaviorHints: { configurationRequired: boolean } };
    expect(m.behaviorHints.configurationRequired).toBe(true);
  });

  it("configured manifest resolves and advertises stream", async () => {
    const m = (await (await call(`/${BLOB}/manifest.json`)).json()) as { resources: string[]; behaviorHints: { configurationRequired: boolean } };
    expect(m.resources).toContain("stream");
    expect(m.behaviorHints.configurationRequired).toBe(false);
  });

  it("bad config blob → 400; unknown resource → 404", async () => {
    expect((await call("/@@@/manifest.json")).status).toBe(400);
    expect((await call(`/${BLOB}/bogus`)).status).toBe(404);
  });

  it("sets cache headers: static for manifest/configure, no-store for health", async () => {
    expect((await call("/configure")).headers.get("cache-control")).toBe("public, max-age=3600");
    expect((await call(`/${BLOB}/manifest.json`)).headers.get("cache-control")).toBe("public, max-age=3600");
    expect((await call("/health")).headers.get("cache-control")).toBe("no-store");
  });

  it("revalidates manifest via ETag → 304 (empty body, cache-control preserved)", async () => {
    const first = await call(`/${BLOB}/manifest.json`);
    const etag = first.headers.get("etag");
    expect(etag).toMatch(/^"[0-9a-f]{8}"$/);
    const second = await call(`/${BLOB}/manifest.json`, deps(), { headers: { "if-none-match": etag! } });
    expect(second.status).toBe(304);
    expect(await second.text()).toBe("");
    expect(second.headers.get("cache-control")).toBe("public, max-age=3600");
  });
});

describe("scout /stream", () => {
  it("returns ranked, clean, cached streams with /play proxy URLs and a bingeGroup", async () => {
    const res = await call(`/${BLOB}/stream/movie/tt1234567.json`);
    expect(res.status).toBe(200);
    const body = (await res.json()) as {
      streams: Array<{ name: string; title: string; url: string; behaviorHints: { bingeGroup: string }; attributes: { label: string } }>;
    };

    // CAM dropped (excludeCam), only cached rows, 4K ranked above 1080p.
    expect(body.streams).toHaveLength(2);
    // title is the raw release name; the clean summary lives in attributes.label.
    expect(body.streams[0].title).toBe("Movie 2160p WEB-DL HDR");
    expect(body.streams[1].title).toBe("Movie 1080p WEB-DL");
    expect(body.streams[0].attributes.label).toBe("4K • WEB-DL • HDR • 18 GB");

    const first = body.streams[0];
    expect(first.name).toBe("Den Scout");
    expect(first.url.startsWith("https://scout.example/" + BLOB + "/play/")).toBe(true);
    expect(first.behaviorHints.bingeGroup).toBe("den-scout-tt1234567");
    // The proxy URL carries no token — only the play target.
    expect(first.url).not.toContain("tb-secret");
    const token = first.url.split("/play/")[1];
    expect(decodePlayToken(token)).toMatchObject({ infoHash: "a".repeat(40), fileIdx: 0 });
  });

  it("bad id → 400", async () => {
    expect((await call(`/${BLOB}/stream/movie/nope.json`)).status).toBe(400);
  });

  it("includes pre-parsed attributes on each stream (so the app doesn't re-parse titles)", async () => {
    const res = await call(`/${BLOB}/stream/movie/tt1234567.json`);
    const body = (await res.json()) as { streams: Array<{ attributes: Record<string, unknown> }> };
    expect(body.streams[0].attributes).toMatchObject({
      resolution: "2160p",
      source: "webdl",
      hdr: true,
      cached: true,
    });
  });

  it("stream list is client-cacheable for the TTL with SWR/stale-if-error", async () => {
    const res = await call(`/${BLOB}/stream/movie/tt9.json`, deps({ listTtlSeconds: 120 }));
    expect(res.headers.get("cache-control")).toBe("public, max-age=120, stale-while-revalidate=120, stale-if-error=86400");
  });

  it("coalesces concurrent misses for the same title into a single scrape", async () => {
    const scrape = vi.fn(async () => SEEDS);
    const d = deps({ makeScrapers: () => [{ id: "torrentio", scrape }] });
    const [a, b] = await Promise.all([
      call(`/${BLOB}/stream/movie/tt777.json`, d),
      call(`/${BLOB}/stream/movie/tt777.json`, d),
    ]);
    expect(a.status).toBe(200);
    expect(b.status).toBe(200);
    expect(scrape).toHaveBeenCalledTimes(1);
  });

  it("stream list supports ETag revalidation → 304", async () => {
    const d = deps();
    const first = await call(`/${BLOB}/stream/movie/tt1.json`, d);
    const etag = first.headers.get("etag");
    expect(etag).toBeTruthy();
    const second = await call(`/${BLOB}/stream/movie/tt1.json`, d, { headers: { "if-none-match": etag! } });
    expect(second.status).toBe(304);
  });

  it("filters RD-blocked releases when Real-Debrid is the only debrid", async () => {
    const rdConfig: ScoutConfig = {
      debrid: [{ service: "realdebrid", token: "rd" }],
      indexers: ["torrentio"],
      filters: { excludeCam: true },
      cachedOnly: false,
      resultCap: 20,
    };
    const rdBlob = encodeConfig(rdConfig);
    const rdSeeds: RawStreamSeed[] = [
      { infoHash: "a".repeat(40), title: "Movie.2160p.BluRay.x264-GRP", sizeBytes: 10 * 1_073_741_824, seeders: 10, source: "torrentio", fileIdx: 0 },
      { infoHash: "b".repeat(40), title: "Movie.2160p.BluRay.REMUX", sizeBytes: 40 * 1_073_741_824, seeders: 10, source: "torrentio", fileIdx: 0 },
    ];
    const d = deps({ makeScrapers: () => [{ id: "torrentio", scrape: async () => rdSeeds }] });
    const res = await handleScout(new Request(`https://scout.example/${rdBlob}/stream/movie/tt5.json`), d);
    const body = (await res.json()) as { streams: Array<{ title: string }> };
    expect(body.streams).toHaveLength(1);
    expect(body.streams[0].title).toContain("REMUX");
  });

  it("serves the second call from cache (scraper runs once)", async () => {
    const scrape = vi.fn(async () => SEEDS);
    const d = deps({ makeScrapers: () => [{ id: "torrentio", scrape }] });
    await call(`/${BLOB}/stream/movie/tt42.json`, d);
    await call(`/${BLOB}/stream/movie/tt42.json`, d);
    expect(scrape).toHaveBeenCalledTimes(1);
  });

  it("honors X-Forwarded-Proto/Host for the /play origin (behind Caddy)", async () => {
    const res = await call(`/${BLOB}/stream/movie/tt7.json`, deps(), {
      headers: { "x-forwarded-proto": "https", "x-forwarded-host": "scout.den.example" },
    });
    const body = (await res.json()) as { streams: Array<{ url: string }> };
    expect(body.streams[0].url.startsWith("https://scout.den.example/")).toBe(true);
  });
});

describe("scout /play", () => {
  it("302-redirects to the resolved debrid link", async () => {
    const streamRes = await call(`/${BLOB}/stream/movie/tt1.json`);
    const url = ((await streamRes.json()) as { streams: Array<{ url: string }> }).streams[0].url;
    const playPath = new URL(url).pathname;
    const res = await call(playPath);
    expect(res.status).toBe(302);
    expect(res.headers.get("location")).toBe(`https://cdn.torbox/${"a".repeat(40)}.mkv`);
    // The redirect must never be cached — RD/TorBox links are freshly minted and IP-bound.
    expect(res.headers.get("cache-control")).toBe("no-store");
  });

  it("bad token → 400", async () => {
    expect((await call(`/${BLOB}/play/@@@`)).status).toBe(400);
  });

  it("dead link → 404 so the client falls through", async () => {
    const dead: Store = { service: "torbox", cacheCheck: async () => new Map(), resolve: async () => { throw new DeadLinkError(); } };
    const goodToken = ((await (await call(`/${BLOB}/stream/movie/tt1.json`)).json()) as { streams: Array<{ url: string }> }).streams[0].url;
    const playPath = new URL(goodToken).pathname;
    const res = await call(playPath, deps({ makeStores: () => [dead] }));
    expect(res.status).toBe(404);
  });
});
