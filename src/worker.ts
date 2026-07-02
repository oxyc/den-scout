/**
 * Cloudflare Worker entry — kept so Scout CAN run on the edge (the seed lived in den-edge). Excluded
 * from the Node build (tsconfig.build) and from coverage; the homelab deploy uses server.ts. A KV
 * binding would back the cache here instead of MemoryCache. Requires @cloudflare/workers-types to
 * typecheck, which this repo doesn't install by default — it's a reference entry, not the CI path.
 */
// @ts-nocheck
import { handleScout } from "./handler.js";
import { buildDeps, settingsFromEnv } from "./deps.js";
import { MemoryCache } from "./cache.js";

export default {
  async fetch(request: Request, env: Record<string, string>): Promise<Response> {
    const deps = buildDeps(fetch, settingsFromEnv(env), new MemoryCache());
    return handleScout(request, deps);
  },
};
