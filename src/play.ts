/**
 * The `/play` token — the ONLY place a torrent fact leaves Scout, and the isolated decode/validate
 * seam the future opaque-configId hardening would swap (SCOUT-04). It carries just enough to resolve
 * one file server-side: the infohash, and either the exact file index or the series episode coords
 * (so a season-pack hash can pick the right file at resolve time). The debrid token is NOT in here —
 * it rides in the config blob on the URL path.
 */
import { b64urlDecode, b64urlEncode } from "./config.js";

export interface PlayTarget {
  infoHash: string;
  fileIdx?: number;
  season?: number;
  episode?: number;
}

export function encodePlayToken(target: PlayTarget): string {
  const compact: Record<string, unknown> = { h: target.infoHash };
  if (target.fileIdx != null) compact.f = target.fileIdx;
  if (target.season != null) compact.s = target.season;
  if (target.episode != null) compact.e = target.episode;
  return b64urlEncode(JSON.stringify(compact));
}

/** Decode + validate a play token, or `null` (→ 400). Infohash must be 32–40 hex/base32 chars. */
export function decodePlayToken(token: string): PlayTarget | null {
  let raw: unknown;
  try {
    raw = JSON.parse(b64urlDecode(token));
  } catch {
    return null;
  }
  if (typeof raw !== "object" || raw === null) return null;
  const r = raw as Record<string, unknown>;
  const infoHash = typeof r.h === "string" ? r.h.toLowerCase() : "";
  if (!/^[a-z0-9]{32,40}$/.test(infoHash)) return null;

  const target: PlayTarget = { infoHash };
  if (isNonNegInt(r.f)) target.fileIdx = r.f;
  if (isNonNegInt(r.s)) target.season = r.s;
  if (isNonNegInt(r.e)) target.episode = r.e;
  return target;
}

function isNonNegInt(v: unknown): v is number {
  return typeof v === "number" && Number.isInteger(v) && v >= 0;
}
