import { defineConfig } from "vitest/config";

// Plain node runtime (no workerd): the core is runtime-agnostic Web Fetch, and the stores /
// scrapers are exercised against fixtures via an injected `fetch`. Coverage is v8 (node has
// node:inspector, unlike workerd) and gated ≥90% lines on the new source (EPIC-den-scout §6).
export default defineConfig({
  test: {
    environment: "node",
    include: ["test/**/*.test.ts"],
    coverage: {
      provider: "v8",
      include: ["src/**/*.ts"],
      // The runtime entrypoints are thin process/network glue (no logic to unit-test); they are
      // smoke-tested by the Docker healthcheck + the homelab deploy doc, not vitest.
      exclude: ["src/server.ts", "src/worker.ts"],
      reporter: ["text", "lcov"],
      thresholds: { lines: 90, functions: 90, statements: 90 },
    },
  },
});
