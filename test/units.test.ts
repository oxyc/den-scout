import { describe, it, expect } from "vitest";
import { parseStreamId } from "../src/id.js";
import { encodePlayToken, decodePlayToken } from "../src/play.js";
import { cleanLabel, sizeLabel } from "../src/label.js";
import { pickEpisodeFile, type TorrentFile } from "../src/season.js";
import { fnv1a, withTimeout } from "../src/util.js";
import { parseSize, parseSeeders, parseStremioStreams } from "../src/scrape/parse.js";
import type { RawStream } from "../src/rank.js";

describe("parseStreamId", () => {
  it("parses movie + strips .json", () => {
    expect(parseStreamId("movie", "tt1234567.json")).toEqual({ type: "movie", imdbId: "tt1234567" });
  });
  it("parses series episode", () => {
    expect(parseStreamId("series", "tt1234567:2:5.json")).toEqual({ type: "series", imdbId: "tt1234567", season: 2, episode: 5 });
  });
  it("rejects bad type / id / episode", () => {
    expect(parseStreamId("catalog", "tt1")).toBeNull();
    expect(parseStreamId("movie", "xx1")).toBeNull();
    expect(parseStreamId("series", "tt1:2")).toBeNull();
    expect(parseStreamId("series", "tt1:x:y")).toBeNull();
  });
});

describe("play token seam", () => {
  it("round-trips a movie target (fileIdx omitted)", () => {
    expect(decodePlayToken(encodePlayToken({ infoHash: "a".repeat(40) }))).toEqual({ infoHash: "a".repeat(40) });
  });
  it("round-trips a series target", () => {
    const t = { infoHash: "b".repeat(40), fileIdx: 3, season: 1, episode: 2 };
    expect(decodePlayToken(encodePlayToken(t))).toEqual(t);
  });
  it("lowercases + validates the infohash, rejects junk", () => {
    expect(decodePlayToken(encodePlayToken({ infoHash: "C".repeat(40) }))).toEqual({ infoHash: "c".repeat(40) });
    expect(decodePlayToken("!!!")).toBeNull();
    expect(decodePlayToken(Buffer.from(JSON.stringify({ h: "short" })).toString("base64url"))).toBeNull();
    expect(decodePlayToken(Buffer.from(JSON.stringify(["not", "obj"])).toString("base64url"))).toBeNull();
  });
});

describe("cleanLabel", () => {
  const GIB = 1_073_741_824;
  const s = (title: string, sizeBytes?: number): RawStream => ({ infoHash: "h", title, sizeBytes, cached: true, source: "x" });
  it("builds a bullet summary", () => {
    expect(cleanLabel(s("Movie 2160p WEB-DL HDR Atmos", 18 * GIB))).toBe("4K • WEB-DL • HDR • Atmos • 18 GB");
  });
  it("shows a decimal for small files, MB under a gig", () => {
    expect(sizeLabel(3.4 * GIB)).toBe("3.4 GB");
    expect(sizeLabel(700 * 1024 * 1024)).toBe("700 MB");
  });
  it("falls back to Stream when nothing is known", () => {
    expect(cleanLabel(s("mysterious"))).toBe("Stream");
  });
});

describe("pickEpisodeFile (season-pack map)", () => {
  const files: TorrentFile[] = [
    { index: 0, name: "Show.S01E01.mkv", sizeBytes: 100 },
    { index: 1, name: "Show.S01E02.1080p.mkv", sizeBytes: 900 },
    { index: 2, name: "Show.S01E02.sample.mkv", sizeBytes: 5 },
    { index: 3, name: "readme.txt", sizeBytes: 1 },
  ];
  it("picks the SxxExx video file, largest on ties (skips sample)", () => {
    expect(pickEpisodeFile(files, 1, 2)).toBe(1);
  });
  it("supports 1x02 and 102 forms", () => {
    expect(pickEpisodeFile([{ index: 7, name: "Show 1x03.mp4" }], 1, 3)).toBe(7);
    expect(pickEpisodeFile([{ index: 8, name: "Show.104.mp4" }], 1, 4)).toBe(8);
  });
  it("falls back to the largest video when no marker matches", () => {
    expect(pickEpisodeFile([{ index: 4, name: "a.mkv", sizeBytes: 10 }, { index: 5, name: "b.mkv", sizeBytes: 20 }], 9, 9)).toBe(5);
  });
  it("returns null with no files", () => {
    expect(pickEpisodeFile([], 1, 1)).toBeNull();
  });
});

describe("util", () => {
  it("fnv1a is stable + 8 hex chars", () => {
    expect(fnv1a("abc")).toMatch(/^[0-9a-f]{8}$/);
    expect(fnv1a("abc")).toBe(fnv1a("abc"));
    expect(fnv1a("abc")).not.toBe(fnv1a("abd"));
  });
  it("withTimeout resolves fast work and rejects slow work", async () => {
    await expect(withTimeout(async () => 42, 50)).resolves.toBe(42);
    await expect(
      withTimeout((signal) => new Promise((_, rej) => signal.addEventListener("abort", () => rej(new Error("aborted")))), 10),
    ).rejects.toThrow();
  });
});

describe("scrape parse helpers", () => {
  it("parses size + seeders from emoji-annotated text", () => {
    expect(parseSize("👤 50 💾 15.2 GB")).toBe(Math.round(15.2 * 1_073_741_824));
    expect(parseSize("700 MB")).toBe(700 * 1_048_576);
    expect(parseSize("no size")).toBeUndefined();
    expect(parseSeeders("👤 123 ⚙️ prov")).toBe(123);
    expect(parseSeeders("Seeders: 7")).toBe(7);
    expect(parseSeeders("none")).toBeUndefined();
  });
  it("keeps only infohash rows and prefers behaviorHints", () => {
    const seeds = parseStremioStreams(
      {
        streams: [
          { name: "Torrentio\n4k", title: "The.Movie.2024.2160p.WEB-DL\n👤 30 💾 18 GB", infoHash: "A".repeat(40), fileIdx: 2 },
          { name: "resolved", title: "no hash", url: "https://x/y.mkv" },
          { infoHash: "b".repeat(40), behaviorHints: { filename: "Exact.Name.mkv", videoSize: 2048 } },
          "garbage",
        ],
      },
      "torrentio",
    );
    expect(seeds).toHaveLength(2);
    expect(seeds[0]).toMatchObject({ infoHash: "a".repeat(40), fileIdx: 2, seeders: 30, source: "torrentio" });
    expect(seeds[0].title).toContain("2160p");
    expect(seeds[1]).toMatchObject({ title: "Exact.Name.mkv", sizeBytes: 2048 });
  });
  it("tolerates a non-object / streamless body", () => {
    expect(parseStremioStreams(null, "x")).toEqual([]);
    expect(parseStremioStreams({ nope: 1 }, "x")).toEqual([]);
  });
});
