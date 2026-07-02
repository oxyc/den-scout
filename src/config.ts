/**
 * Den Scout config — the base64url-encoded blob that rides in the addon URL
 * (`/scout/<config>/manifest.json`), Torrentio-style (SCOUT-01 / EPIC-den-scout).
 *
 * DECISION (2026-07-01): the debrid token lives IN the URL for now (what the user asked for;
 * the common Stremio-addon pattern). It is a bearer credential — the app must persist it via
 * SecureStore (Keychain), never log it, and it must never appear in server logs. A future
 * hardening swaps the token for an opaque `configId` that maps to a server-side secret; the
 * decode/validate seam here is the only thing that would change.
 */

export const DEBRID_SERVICES = ["torbox", "realdebrid", "premiumize"] as const;
export type DebridService = (typeof DEBRID_SERVICES)[number];

export const INDEXERS = ["torrentio", "comet", "mediafusion", "torz"] as const;
export type Indexer = (typeof INDEXERS)[number];

export const RESOLUTIONS = ["2160p", "1080p", "720p", "480p"] as const;
export type Resolution = (typeof RESOLUTIONS)[number];

export interface DebridAccount {
  service: DebridService;
  token: string;
}

export interface ScoutFilters {
  /** Drop CAM/TS/screener/junk entirely (VortX `junkClass`). Default on. */
  excludeCam: boolean;
  /** Keep only these resolutions (untagged streams are kept). Empty/absent → all. */
  resolutions?: Resolution[];
  /** Keep only HDR/DV streams. */
  hdrOnly?: boolean;
  minSeeders?: number;
  maxSizeGB?: number;
  /** User regex, applied case-insensitively to the release title; clamped ≤256 chars. */
  excludeRegex?: string;
}

export interface ScoutConfig {
  debrid: DebridAccount[];
  indexers: Indexer[];
  filters: ScoutFilters;
  /** Return only debrid-cached streams so playback "just works" (TorBox cache truth). Default on. */
  cachedOnly: boolean;
  /** Cap the returned list (the app never needs 200 rows). */
  resultCap: number;
}

// MARK: - base64url

/** base64url-decode (`-`→`+`, `_`→`/`, re-pad) → UTF-8 string. Runtime-safe (no atob UTF-8 loss). */
export function b64urlDecode(input: string): string {
  const b64 = input.replace(/-/g, "+").replace(/_/g, "/");
  const padded = b64.padEnd(Math.ceil(b64.length / 4) * 4, "=");
  const bin = atob(padded);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

/** UTF-8 string → base64url (strips `=` padding). Mirrors the `/configure` page's client encoder. */
export function b64urlEncode(input: string): string {
  const bytes = new TextEncoder().encode(input);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// MARK: - decode + validate

/** Decode the URL blob into a validated config, or `null` if malformed/invalid (→ 400). */
export function decodeConfig(blob: string): ScoutConfig | null {
  let raw: unknown;
  try {
    raw = JSON.parse(b64urlDecode(blob));
  } catch {
    return null;
  }
  return validateConfig(raw);
}

/** Encode a config back into a URL blob (used by tests + the configure page's server side). */
export function encodeConfig(config: ScoutConfig): string {
  return b64urlEncode(JSON.stringify(config));
}

/**
 * Strict-whitelist + clamp an untrusted config (Singularity's `validateConfig` posture): unknown
 * fields don't survive, numeric limits are clamped, the regex is length-bounded. At least one valid
 * debrid account is required — Scout resolves server-side, so a config with no token is useless.
 */
export function validateConfig(raw: unknown): ScoutConfig | null {
  if (typeof raw !== "object" || raw === null) return null;
  const r = raw as Record<string, unknown>;

  const debrid = Array.isArray(r.debrid) ? r.debrid.flatMap(sanitizeDebrid) : [];
  if (debrid.length === 0) return null;

  const indexers = Array.isArray(r.indexers)
    ? r.indexers.filter((x): x is Indexer => (INDEXERS as readonly string[]).includes(x as string))
    : [];

  const f = typeof r.filters === "object" && r.filters !== null ? (r.filters as Record<string, unknown>) : {};
  const resolutions = Array.isArray(f.resolutions)
    ? f.resolutions.filter((x): x is Resolution => (RESOLUTIONS as readonly string[]).includes(x as string))
    : undefined;

  const filters: ScoutFilters = {
    excludeCam: f.excludeCam !== false, // default on
    resolutions: resolutions && resolutions.length > 0 ? resolutions : undefined,
    hdrOnly: f.hdrOnly === true ? true : undefined,
    minSeeders: clampInt(f.minSeeders, 0, 100_000),
    maxSizeGB: clampInt(f.maxSizeGB, 0, 1000),
    excludeRegex: typeof f.excludeRegex === "string" && f.excludeRegex.length > 0 ? f.excludeRegex.slice(0, 256) : undefined,
  };

  return {
    debrid,
    indexers: indexers.length > 0 ? indexers : [...INDEXERS],
    filters,
    cachedOnly: r.cachedOnly !== false, // default on — streams just work
    resultCap: clampInt(r.resultCap, 1, 200) ?? 20,
  };
}

function sanitizeDebrid(item: unknown): DebridAccount[] {
  if (typeof item !== "object" || item === null) return [];
  const d = item as Record<string, unknown>;
  const service = d.service;
  const token = d.token;
  if (typeof service !== "string" || !(DEBRID_SERVICES as readonly string[]).includes(service)) return [];
  if (typeof token !== "string" || token.length === 0 || token.length > 512) return [];
  return [{ service: service as DebridService, token }];
}

function clampInt(value: unknown, min: number, max: number): number | undefined {
  if (typeof value !== "number" || !Number.isFinite(value)) return undefined;
  return Math.min(Math.max(Math.round(value), min), max);
}
