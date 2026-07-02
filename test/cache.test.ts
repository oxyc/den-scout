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

  it("evicts the least-recently-used entry past maxEntries", async () => {
    const cache = new MemoryCache(() => 0, 2); // never expires; cap = 2
    await cache.put("a", "1", 100);
    await cache.put("b", "2", 100);
    await cache.get("a"); // touch 'a' → 'b' is now least-recently-used
    await cache.put("c", "3", 100); // over cap → evict 'b'
    expect(await cache.get("b")).toBeNull();
    expect(await cache.get("a")).toBe("1");
    expect(await cache.get("c")).toBe("3");
  });
});
