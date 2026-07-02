import { describe, it, expect } from "vitest";
import { junkClass, qualityScore, rankStreams, detectResolution, realDebridBlocked, type RawStream } from "../src/rank.js";

function stream(title: string, over: Partial<RawStream> = {}): RawStream {
  return { infoHash: "h", title, cached: false, source: "torrentio", ...over };
}

describe("scout ranker — junkClass (VortX)", () => {
  it("detects separator variants of CAM/TS/screener", () => {
    expect(junkClass("Movie 2024 HDCAM x264")).toBe("cam");
    expect(junkClass("Movie 2024 hd.cam")).toBe("cam");
    expect(junkClass("Movie 2024 HD-TS")).toBe("telesync");
    expect(junkClass("Movie 2024 DVDScr")).toBe("screener");
    expect(junkClass("Movie 2024 WORKPRINT")).toBe("workprint");
    expect(junkClass("Movie 2024 AI-upscaled")).toBe("upscaled");
    expect(junkClass("Movie 2024 R5")).toBe("r5");
    expect(junkClass("Movie 2024 TELECINE")).toBe("telecine");
  });

  it("bare 'cam'/'ts' are NOT junk when a good source marker is present", () => {
    expect(junkClass("Show S01E01 WEB-DL cam.crew.release")).toBeNull();
    expect(junkClass("Movie 2024 BluRay REMUX ts-audio")).toBeNull();
  });

  it("bare 'cam' IS junk with no good source", () => {
    expect(junkClass("Movie 2024 cam")).toBe("cam");
    expect(junkClass("Movie 2024 ts")).toBe("telesync");
    expect(junkClass("Movie 2024 scr")).toBe("screener");
  });

  it("does not flag legit releases", () => {
    expect(junkClass("Movie.2024.2160p.WEB-DL.DV.HDR.x265")).toBeNull();
  });
});

describe("scout ranker — scoring + filtering", () => {
  it("cached always beats uncached", () => {
    const cached = qualityScore(stream("Movie 720p WEB-DL", { cached: true }));
    const uncached = qualityScore(stream("Movie 2160p REMUX", { cached: false }));
    expect(cached).toBeGreaterThan(uncached);
  });

  it("junk is sunk far below any legit source", () => {
    const cam = qualityScore(stream("Movie 2160p HDCAM"));
    const legit = qualityScore(stream("Movie 480p WEB-DL"));
    expect(cam).toBeLessThan(legit - 50_000);
  });

  it("orders remux > web-dl > hdtv at equal resolution", () => {
    const remux = qualityScore(stream("Movie 1080p REMUX"));
    const webdl = qualityScore(stream("Movie 1080p WEB-DL"));
    const hdtv = qualityScore(stream("Movie 1080p HDTV"));
    expect(remux).toBeGreaterThan(webdl);
    expect(webdl).toBeGreaterThan(hdtv);
  });

  it("penalizes av1 at 4k and 3d hard", () => {
    expect(qualityScore(stream("Movie 2160p WEB-DL AV1"))).toBeLessThan(qualityScore(stream("Movie 2160p WEB-DL")));
    expect(qualityScore(stream("Movie 1080p BluRay 3D HSBS"))).toBeLessThan(qualityScore(stream("Movie 1080p BluRay")));
  });

  it("rewards HDR/DV and object audio", () => {
    expect(qualityScore(stream("Movie 2160p WEB-DL DoVi"))).toBeGreaterThan(qualityScore(stream("Movie 2160p WEB-DL")));
    expect(qualityScore(stream("Movie 2160p REMUX Atmos"))).toBeGreaterThan(qualityScore(stream("Movie 2160p REMUX")));
  });

  it("distrusts a tiny '4k' file but trusts a large one", () => {
    const GIB = 1_073_741_824;
    expect(qualityScore(stream("Movie 4K WEB", { sizeBytes: 1 * GIB }))).toBeLessThan(
      qualityScore(stream("Movie 4K WEB", { sizeBytes: 20 * GIB })),
    );
  });

  it("excludeCam drops junk, cachedOnly drops uncached, resultCap caps", () => {
    const streams = [
      stream("A 1080p WEB-DL", { cached: true }),
      stream("B 2160p HDCAM", { cached: true }),
      stream("C 1080p WEB-DL", { cached: false }),
    ];
    const ranked = rankStreams(streams, { excludeCam: true, cachedOnly: true, resultCap: 5 });
    expect(ranked.map((s) => s.title)).toEqual(["A 1080p WEB-DL"]);
  });

  it("applies resolutions (keeping untagged), minSeeders, maxSizeGB, excludeRegex", () => {
    const GIB = 1_073_741_824;
    const streams = [
      stream("4k", { title: "X 2160p WEB HDR", seeders: 10, sizeBytes: 30 * GIB, cached: true }),
      stream("1080 sdr", { title: "Y 1080p WEB", seeders: 10, sizeBytes: 8 * GIB, cached: true }),
      stream("untagged", { title: "Z untitled release", seeders: 10, sizeBytes: 2 * GIB, cached: true }),
      stream("lowseed", { title: "W 2160p WEB", seeders: 0, sizeBytes: 5 * GIB, cached: true }),
      stream("toobig", { title: "U 2160p WEB", seeders: 10, sizeBytes: 90 * GIB, cached: true }),
      stream("banned", { title: "V 2160p WEB YIFY", seeders: 10, sizeBytes: 5 * GIB, cached: true }),
    ];
    const ranked = rankStreams(streams, {
      excludeCam: true,
      cachedOnly: true,
      resultCap: 20,
      resolutions: ["2160p"],
      minSeeders: 1,
      maxSizeGB: 40,
      excludeRegex: "yify",
    });
    // 2160p + untagged (kept by the resolution rule) survive; 1080p, 0-seed, >40 GB and YIFY are gone.
    expect(ranked.map((s) => s.title).sort()).toEqual(["X 2160p WEB HDR", "Z untitled release"].sort());
  });

  it("hdrOnly keeps only HDR/DV streams", () => {
    const streams = [stream("A 2160p WEB HDR", { cached: true }), stream("B 2160p WEB", { cached: true })];
    const ranked = rankStreams(streams, { excludeCam: true, cachedOnly: true, resultCap: 20, hdrOnly: true });
    expect(ranked.map((s) => s.title)).toEqual(["A 2160p WEB HDR"]);
  });

  it("ignores a malformed excludeRegex instead of dropping everything", () => {
    const streams = [stream("A 1080p WEB-DL", { cached: true })];
    const ranked = rankStreams(streams, { excludeCam: true, cachedOnly: true, resultCap: 5, excludeRegex: "(" });
    expect(ranked).toHaveLength(1);
  });

  it("detectResolution buckets", () => {
    expect(detectResolution("x 2160p y")).toBe("2160p");
    expect(detectResolution("x 576p y")).toBe("480p");
    expect(detectResolution("x 720p y")).toBe("720p");
    expect(detectResolution("no res here")).toBeNull();
  });

  it("breaks quality ties by seeders", () => {
    const low = stream("Movie 1080p WEB-DL", { cached: true, seeders: 5 });
    const high = stream("Movie 1080p WEB-DL", { cached: true, seeders: 500 });
    const ranked = rankStreams([low, high], { excludeCam: true, cachedOnly: true, resultCap: 5 });
    expect(ranked[0].seeders).toBe(500);
  });
});

describe("real-debrid filename block", () => {
  it("blocks Type-1 substrings anywhere (case-insensitive)", () => {
    for (const t of ["Movie.2024.WEB-DL.x265", "Show.WEBRip.720p", "X.BDRip.1080p", "Y.HDRip", "Z.DVDRip"]) {
      expect(realDebridBlocked(t)).toBe(true);
    }
  });

  it("blocks Type-2 dot-adjacent Source.Codec", () => {
    for (const t of ["Movie.2024.BluRay.x264-GRP", "Show.HDTV.x264", "Show.HDTV.XviD", "Clip.WEB.x264", "Clip.WEB.h264"]) {
      expect(realDebridBlocked(t)).toBe(true);
    }
  });

  it("does not block clean releases (remux, x265, spaced bluray)", () => {
    expect(realDebridBlocked("Movie.2024.2160p.BluRay.REMUX.HEVC")).toBe(false);
    expect(realDebridBlocked("Movie.2024.WEB.x265")).toBe(false); // web.x265 isn't in the list
    expect(realDebridBlocked("Movie 2024 BluRay x264")).toBe(false); // not dot-adjacent
  });
});
