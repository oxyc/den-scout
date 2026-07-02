import { describe, it, expect } from "vitest";
import { TorBoxStore } from "../src/stores/torbox.js";
import { RealDebridStore } from "../src/stores/realdebrid.js";
import { PremiumizeStore } from "../src/stores/premiumize.js";
import { StorePool, buildStores } from "../src/stores/index.js";
import { DeadLinkError, type Store } from "../src/stores/types.js";
import type { FetchLike } from "../src/scrape/types.js";
import type { ScoutConfig } from "../src/config.js";

/** A fetch double that dispatches on (method, url-substring) → JSON body. Records calls. */
function router(routes: Array<{ match: string; method?: string; status?: number; body: unknown }>): {
  fetch: FetchLike;
  urls: string[];
} {
  const urls: string[] = [];
  const fetch: FetchLike = async (url, init) => {
    urls.push(url);
    const method = (init?.method ?? "GET").toUpperCase();
    const route = routes.find((r) => url.includes(r.match) && (r.method ?? "GET") === method);
    if (!route) throw new Error(`unrouted ${method} ${url}`);
    return new Response(JSON.stringify(route.body), { status: route.status ?? 200 });
  };
  return { fetch, urls };
}

const H = "a".repeat(40);

describe("TorBoxStore", () => {
  it("cacheCheck maps present hashes → true, absent → false", async () => {
    const { fetch } = router([{ match: "checkcached", body: { data: { [H]: { name: "x" } } } }]);
    const map = await new TorBoxStore("tok", fetch).cacheCheck([H, "b".repeat(40)]);
    expect(map.get(H)).toBe(true);
    expect(map.get("b".repeat(40))).toBe(false);
  });

  it("cacheCheck batches >100 hashes and tolerates a failed batch", async () => {
    const hashes = Array.from({ length: 150 }, (_, i) => i.toString(16).padStart(40, "0"));
    let call = 0;
    const fetch: FetchLike = async (url) => {
      call++;
      // First batch OK (marks its first hash cached); second batch errors → those stay false.
      if (call === 1) return new Response(JSON.stringify({ data: { [hashes[0]]: {} } }), { status: 200 });
      return new Response("boom", { status: 500 });
    };
    const map = await new TorBoxStore("tok", fetch).cacheCheck(hashes);
    expect(call).toBe(2);
    expect(map.get(hashes[0])).toBe(true);
    expect(map.get(hashes[149])).toBe(false);
  });

  it("resolve (movie, explicit fileIdx) adds magnet then requests the link", async () => {
    const { fetch, urls } = router([
      { match: "createtorrent", method: "POST", body: { data: { torrent_id: 7 } } },
      { match: "requestdl", body: { success: true, data: "https://cdn.torbox/x.mkv" } },
    ]);
    const link = await new TorBoxStore("tok", fetch).resolve({ infoHash: H, fileIdx: 0 });
    expect(link).toBe("https://cdn.torbox/x.mkv");
    expect(urls.some((u) => u.includes("file_id=0"))).toBe(true);
    expect(urls.some((u) => u.includes("mylist"))).toBe(false); // fileIdx known → no file listing
  });

  it("resolve (series season-pack) lists files and picks the episode", async () => {
    const { fetch, urls } = router([
      { match: "createtorrent", method: "POST", body: { data: { torrent_id: 9 } } },
      {
        match: "mylist",
        body: { data: { files: [{ id: 0, name: "S01E01.mkv", size: 10 }, { id: 1, name: "S01E02.mkv", size: 20 }] } },
      },
      { match: "requestdl", body: { success: true, data: "https://cdn.torbox/ep2.mkv" } },
    ]);
    const link = await new TorBoxStore("tok", fetch).resolve({ infoHash: H, season: 1, episode: 2 });
    expect(link).toBe("https://cdn.torbox/ep2.mkv");
    expect(urls.some((u) => u.includes("file_id=1"))).toBe(true);
  });

  it("resolve throws DeadLinkError when TorBox has no link", async () => {
    const { fetch } = router([
      { match: "createtorrent", method: "POST", body: { data: { torrent_id: 7 } } },
      { match: "requestdl", body: { success: false } },
    ]);
    await expect(new TorBoxStore("tok", fetch).resolve({ infoHash: H, fileIdx: 0 })).rejects.toBeInstanceOf(DeadLinkError);
  });
});

describe("RealDebridStore", () => {
  it("cacheCheck is all-false (no usable RD cache API)", async () => {
    const map = await new RealDebridStore("tok", async () => new Response("{}")).cacheCheck([H]);
    expect(map.get(H)).toBe(false);
  });

  it("resolve adds → selects → unrestricts to a download link", async () => {
    let infoCalls = 0;
    const fetch: FetchLike = async (url, init) => {
      const method = (init?.method ?? "GET").toUpperCase();
      if (url.includes("addMagnet")) return new Response(JSON.stringify({ id: "t1" }), { status: 201 });
      if (url.includes("/torrents/info/")) {
        infoCalls++;
        const links = infoCalls === 1 ? [] : ["https://real-debrid/restricted"];
        return new Response(JSON.stringify({ files: [{ id: 1, path: "/movie.mkv", bytes: 999 }], links }));
      }
      if (url.includes("selectFiles") && method === "POST") return new Response(null, { status: 204 });
      if (url.includes("unrestrict/link") && method === "POST")
        return new Response(JSON.stringify({ download: "https://real-debrid/dl.mkv" }));
      throw new Error(`unrouted ${url}`);
    };
    const link = await new RealDebridStore("tok", fetch).resolve({ infoHash: H });
    expect(link).toBe("https://real-debrid/dl.mkv");
  });

  it("resolve refuses an RD-blocked filename (so the pool falls through)", async () => {
    const fetch: FetchLike = async (url) => {
      if (url.includes("addMagnet")) return new Response(JSON.stringify({ id: "t1" }), { status: 201 });
      if (url.includes("/torrents/info/"))
        return new Response(JSON.stringify({ files: [{ id: 1, path: "Movie.WEB-DL.x264.mkv", bytes: 999 }], links: [] }));
      throw new Error(`unexpected ${url}`);
    };
    await expect(new RealDebridStore("tok", fetch).resolve({ infoHash: H })).rejects.toBeInstanceOf(DeadLinkError);
  });

  it("resolve throws DeadLinkError when the torrent has no files", async () => {
    const fetch: FetchLike = async (url) => {
      if (url.includes("addMagnet")) return new Response(JSON.stringify({ id: "t1" }), { status: 201 });
      if (url.includes("/torrents/info/")) return new Response(JSON.stringify({ files: [], links: [] }));
      throw new Error("unexpected");
    };
    await expect(new RealDebridStore("tok", fetch).resolve({ infoHash: H })).rejects.toBeInstanceOf(DeadLinkError);
  });
});

describe("PremiumizeStore", () => {
  it("cacheCheck maps response[] positionally", async () => {
    const other = "b".repeat(40);
    const { fetch } = router([{ match: "cache/check", body: { status: "success", response: [true, false] } }]);
    const map = await new PremiumizeStore("tok", fetch).cacheCheck([H, other]);
    expect(map.get(H)).toBe(true);
    expect(map.get(other)).toBe(false);
  });

  it("resolve picks a file from directdl content", async () => {
    const { fetch } = router([
      {
        match: "transfer/directdl",
        method: "POST",
        body: { status: "success", content: [{ path: "S01E01.mkv", link: "https://pm/1", size: 10 }, { path: "S01E02.mkv", link: "https://pm/2", size: 20 }] },
      },
    ]);
    const link = await new PremiumizeStore("tok", fetch).resolve({ infoHash: H, season: 1, episode: 2 });
    expect(link).toBe("https://pm/2");
  });

  it("resolve throws DeadLinkError on empty content", async () => {
    const { fetch } = router([{ match: "transfer/directdl", method: "POST", body: { status: "error", content: [] } }]);
    await expect(new PremiumizeStore("tok", fetch).resolve({ infoHash: H })).rejects.toBeInstanceOf(DeadLinkError);
  });
});

describe("StorePool + buildStores", () => {
  const cfg = (services: ScoutConfig["debrid"]): ScoutConfig => ({
    debrid: services,
    indexers: ["torrentio"],
    filters: { excludeCam: true },
    cachedOnly: true,
    resultCap: 20,
  });

  it("orders TorBox first regardless of config order", () => {
    const stores = buildStores(cfg([{ service: "premiumize", token: "p" }, { service: "torbox", token: "t" }, { service: "realdebrid", token: "r" }]), async () => new Response("{}"));
    expect(stores.map((s) => s.service)).toEqual(["torbox", "realdebrid", "premiumize"]);
  });

  it("cacheCheck unions across stores", async () => {
    const s1: Store = { service: "torbox", cacheCheck: async () => new Map([[H, true], ["b".repeat(40), false]]), resolve: async () => "x" };
    const s2: Store = { service: "premiumize", cacheCheck: async () => new Map([[H, false], ["b".repeat(40), true]]), resolve: async () => "x" };
    const map = await new StorePool([s1, s2]).cacheCheck([H, "b".repeat(40)]);
    expect(map.get(H)).toBe(true);
    expect(map.get("b".repeat(40))).toBe(true);
  });

  it("cacheCheck of an empty hash list returns an empty map", async () => {
    expect((await new StorePool([]).cacheCheck([])).size).toBe(0);
  });

  it("resolve falls through to the next store, then errors if all fail", async () => {
    const bad: Store = { service: "torbox", cacheCheck: async () => new Map(), resolve: async () => { throw new DeadLinkError(); } };
    const good: Store = { service: "realdebrid", cacheCheck: async () => new Map(), resolve: async () => "https://ok" };
    expect(await new StorePool([bad, good]).resolve({ infoHash: H })).toBe("https://ok");
    await expect(new StorePool([bad]).resolve({ infoHash: H })).rejects.toBeInstanceOf(DeadLinkError);
  });
});
