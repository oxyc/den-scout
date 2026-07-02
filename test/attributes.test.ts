import { describe, it, expect } from "vitest";
import { streamAttributes } from "../src/attributes.js";
import type { RawStream } from "../src/rank.js";

function s(title: string, over: Partial<RawStream> = {}): RawStream {
  return { infoHash: "h", title, cached: false, source: "torrentio", ...over };
}

describe("streamAttributes", () => {
  it("parses a rich 4K release", () => {
    const a = streamAttributes(
      s("Movie.2024.2160p.BluRay.REMUX.DV.HDR.HEVC.Atmos", { sizeBytes: 40 * 1_073_741_824, seeders: 12, cached: true }),
    );
    expect(a).toMatchObject({
      resolution: "2160p",
      source: "remux",
      codec: "hevc",
      hdr: true,
      dolbyVision: true,
      audio: "Atmos",
      threeD: false,
      seeders: 12,
      cached: true,
    });
    expect(a.sizeBytes).toBe(40 * 1_073_741_824);
    expect(a.label).toContain("4K");
  });

  it("classifies source variants", () => {
    expect(streamAttributes(s("X 1080p WEB-DL")).source).toBe("webdl");
    expect(streamAttributes(s("X 1080p WEBRip")).source).toBe("webrip");
    expect(streamAttributes(s("X 1080p BluRay")).source).toBe("bluray");
    expect(streamAttributes(s("X 720p HDTV")).source).toBe("hdtv");
    expect(streamAttributes(s("X DVDRip")).source).toBe("dvdrip");
    expect(streamAttributes(s("X 2024 HDCAM")).source).toBe("cam");
    expect(streamAttributes(s("X mystery")).source).toBeNull();
  });

  it("detects codec + audio + 3D + nulls", () => {
    expect(streamAttributes(s("X AV1")).codec).toBe("av1");
    expect(streamAttributes(s("X x264")).codec).toBe("avc");
    expect(streamAttributes(s("X DTS-HD MA")).audio).toBe("DTS-HD");
    expect(streamAttributes(s("X EAC3")).audio).toBe("EAC3");
    expect(streamAttributes(s("X 1080p 3D HSBS")).threeD).toBe(true);
    const bare = streamAttributes(s("X untitled"));
    expect(bare.codec).toBeNull();
    expect(bare.audio).toBeNull();
    expect(bare.resolution).toBeNull();
    expect(bare.sizeBytes).toBeNull();
    expect(bare.seeders).toBeNull();
  });

  it("HDR without Dolby Vision", () => {
    const a = streamAttributes(s("Movie 2160p WEB-DL HDR10"));
    expect(a.hdr).toBe(true);
    expect(a.dolbyVision).toBe(false);
  });
});
