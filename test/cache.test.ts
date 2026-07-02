import { describe, it, expect } from "vitest";
import { MemoryCache } from "../src/cache.js";

describe("MemoryCache", () => {
  it("stores and returns a value within its TTL", async () => {
    let t = 1000;
    const cache = new MemoryCache(() => t);
    await cache.put("k", "v", 10);
    expect(await cache.get("k")).toBe("v");
    t = 1000 + 9_000;
    expect(await cache.get("k")).toBe("v");
  });

  it("expires and evicts past the TTL", async () => {
    let t = 0;
    const cache = new MemoryCache(() => t);
    await cache.put("k", "v", 5);
    t = 5_001;
    expect(await cache.get("k")).toBeNull();
    expect(await cache.get("k")).toBeNull();
  });

  it("misses on an unknown key", async () => {
    expect(await new MemoryCache().get("nope")).toBeNull();
  });
});
