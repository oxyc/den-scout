/**
 * Den Scout Stremio manifest (SCOUT-01). A plain stream addon so any Stremio client — the Den app
 * included, via its existing `AddonClient` — installs it with zero code changes. `idPrefixes: ["tt"]`
 * because Den bridges TMDB → IMDb before it asks for streams.
 */
import type { ScoutConfig } from "./config.js";

const VERSION = "0.1.0";

export function buildManifest(config: ScoutConfig | null): Record<string, unknown> {
  const configured = config !== null;
  const services = configured ? config.debrid.map((d) => d.service).join(", ") : "";
  return {
    id: "com.den.scout",
    version: VERSION,
    name: "Den Scout",
    description: configured
      ? `Cached-first, ranked, junk-filtered streams via ${services}.`
      : "Self-hosted stream aggregator for Den — configure to add your debrid token.",
    resources: ["stream"],
    types: ["movie", "series"],
    idPrefixes: ["tt"],
    catalogs: [],
    behaviorHints: {
      configurable: true,
      // An unconfigured install (no token) must send the user to /configure before it's usable.
      configurationRequired: !configured,
    },
  };
}
