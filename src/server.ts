/**
 * Node/Bun entry (Hono). A thin adapter: it hands every request to the runtime-agnostic
 * `handleScout` core. The homelab runs this behind Caddy (https + a stable domain); `handleScout`
 * reads `X-Forwarded-Proto`/`Host` to build correct `/play` URLs. Bun serves the same `app.fetch`.
 */
import { Hono } from "hono";
import { serve } from "@hono/node-server";
import { handleScout } from "./handler.js";
import { buildDeps, settingsFromEnv } from "./deps.js";
import type { FetchLike } from "./scrape/types.js";

const deps = buildDeps(globalThis.fetch as unknown as FetchLike, settingsFromEnv(process.env));

const app = new Hono();
app.all("*", (c) => handleScout(c.req.raw, deps));

const port = Number(process.env.PORT ?? 8080);
serve({ fetch: app.fetch, port });
// eslint-disable-next-line no-console
console.log(`den-scout listening on :${port}`);

export { app };
