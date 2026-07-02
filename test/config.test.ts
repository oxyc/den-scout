import { describe, it, expect } from "vitest";
import { decodeConfig, encodeConfig, validateConfig, b64urlDecode, b64urlEncode, type ScoutConfig } from "../src/config.js";

const CONFIG: ScoutConfig = {
  debrid: [{ service: "torbox", token: "tb-secret" }],
  indexers: ["torrentio"],
  filters: { excludeCam: true },
  cachedOnly: true,
  resultCap: 20,
};

describe("scout config (base64url token-in-url)", () => {
  it("round-trips encode → decode", () => {
    const decoded = decodeConfig(encodeConfig(CONFIG));
    expect(decoded).toMatchObject({ debrid: [{ service: "torbox", token: "tb-secret" }], cachedOnly: true });
  });

  it("base64url encode/decode survives non-ascii", () => {
    expect(b64urlDecode(b64urlEncode("héllo—世界"))).toBe("héllo—世界");
  });

  it("rejects a config with no valid debrid account", () => {
    expect(validateConfig({ debrid: [], indexers: ["torrentio"] })).toBeNull();
    expect(validateConfig({ debrid: [{ service: "nope", token: "x" }] })).toBeNull();
    expect(validateConfig({ debrid: [{ service: "torbox", token: "" }] })).toBeNull();
    expect(validateConfig({ debrid: [{ service: "torbox", token: "x".repeat(999) }] })).toBeNull();
    expect(validateConfig("not-an-object")).toBeNull();
    expect(validateConfig(null)).toBeNull();
  });

  it("clamps limits and drops unknown fields", () => {
    const c = validateConfig({
      debrid: [{ service: "realdebrid", token: "rd" }],
      resultCap: 9999,
      filters: { excludeRegex: "x".repeat(400), minSeeders: -5 },
      evil: "ignored",
    });
    expect(c?.resultCap).toBe(200);
    expect(c?.filters.excludeRegex?.length).toBe(256);
    expect(c?.filters.minSeeders).toBe(0);
    expect((c as unknown as Record<string, unknown>).evil).toBeUndefined();
  });

  it("keeps valid optional filters (resolutions, hdrOnly, maxSizeGB)", () => {
    const c = validateConfig({
      debrid: [{ service: "torbox", token: "t" }],
      indexers: ["torrentio", "bogus", "comet"],
      filters: { resolutions: ["2160p", "nope", "1080p"], hdrOnly: true, maxSizeGB: 40 },
    });
    expect(c?.indexers).toEqual(["torrentio", "comet"]);
    expect(c?.filters.resolutions).toEqual(["2160p", "1080p"]);
    expect(c?.filters.hdrOnly).toBe(true);
    expect(c?.filters.maxSizeGB).toBe(40);
  });

  it("defaults excludeCam + cachedOnly on, indexers to all", () => {
    const c = validateConfig({ debrid: [{ service: "torbox", token: "t" }] });
    expect(c?.filters.excludeCam).toBe(true);
    expect(c?.cachedOnly).toBe(true);
    expect(c?.indexers).toEqual(["torrentio", "comet", "mediafusion", "torz"]);
  });

  it("respects explicit excludeCam:false / cachedOnly:false", () => {
    const c = validateConfig({ debrid: [{ service: "torbox", token: "t" }], cachedOnly: false, filters: { excludeCam: false } });
    expect(c?.filters.excludeCam).toBe(false);
    expect(c?.cachedOnly).toBe(false);
  });

  it("garbage blob → null", () => {
    expect(decodeConfig("!!!not-base64!!!")).toBeNull();
    expect(decodeConfig(b64urlEncode("not json"))).toBeNull();
  });
});
