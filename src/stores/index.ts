/**
 * Store pool. Builds one Store per configured debrid account in service-priority order
 * (TorBox → Real-Debrid → Premiumize; TorBox first because it has the real cache API), then:
 *   cacheCheck — union across stores (cached in ANY store ⇒ cached), so a multi-debrid user gets the
 *                widest cached set. Every store's cacheCheck is failure-tolerant, so this never throws.
 *   resolve    — try stores in the same priority order; first playable link wins; else DeadLinkError.
 */
import { DEBRID_SERVICES, type ScoutConfig } from "../config.js";
import { type FetchLike, type ResolveTarget, type Store, DeadLinkError } from "./types.js";
import { TorBoxStore } from "./torbox.js";
import { RealDebridStore } from "./realdebrid.js";
import { PremiumizeStore } from "./premiumize.js";

/** One store per account, ordered by `DEBRID_SERVICES` (TorBox first) regardless of config order. */
export function buildStores(config: ScoutConfig, fetch: FetchLike): Store[] {
  const byService = new Map(config.debrid.map((d) => [d.service, d.token]));
  const stores: Store[] = [];
  for (const service of DEBRID_SERVICES) {
    const token = byService.get(service);
    if (!token) continue;
    if (service === "torbox") stores.push(new TorBoxStore(token, fetch));
    else if (service === "realdebrid") stores.push(new RealDebridStore(token, fetch));
    else if (service === "premiumize") stores.push(new PremiumizeStore(token, fetch));
  }
  return stores;
}

export class StorePool {
  constructor(private readonly stores: Store[]) {}

  async cacheCheck(infoHashes: string[]): Promise<Map<string, boolean>> {
    const result = new Map<string, boolean>(infoHashes.map((h) => [h, false]));
    if (infoHashes.length === 0) return result;
    // Stores are independent (the result is a union), and each cacheCheck is failure-tolerant, so run
    // them concurrently — a TorBox+Premiumize user pays the slower store's latency, not the sum.
    const maps = await Promise.all(this.stores.map((store) => store.cacheCheck(infoHashes)));
    for (const map of maps) for (const [hash, cached] of map) if (cached) result.set(hash, true);
    return result;
  }

  async resolve(target: ResolveTarget): Promise<string> {
    for (const store of this.stores) {
      try {
        return await store.resolve(target);
      } catch {
        // Try the next store — a hash cached on TorBox but not RD still plays.
      }
    }
    throw new DeadLinkError("no store could resolve");
  }
}
