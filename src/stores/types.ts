/**
 * The debrid Store contract (SCOUT-01) — Scout's port of VortX's "debrid crate". Two operations:
 *   cacheCheck  — which infohashes does this account already have cached? (drives cached-only + rank)
 *   resolve     — turn one infohash (+ file) into a fresh, playable https link (the /play redirect).
 *
 * Scout resolves SERVER-SIDE (unlike Singularity's on-device model): the token stays on the server,
 * the app only ever sees the resulting https URL. Every store is built from a `DebridAccount` token
 * and an injected `fetch`, so it is fully unit-testable without hitting the real API.
 */
import type { DebridService } from "../config.js";
import type { FetchLike } from "../scrape/types.js";

/** What to resolve: an infohash plus either the exact file, or the series episode to pick from a pack. */
export interface ResolveTarget {
  infoHash: string;
  fileIdx?: number;
  season?: number;
  episode?: number;
}

export interface Store {
  readonly service: DebridService;
  /** Map every input hash → cached?. Missing/failed hashes MUST map to `false`, never throw. */
  cacheCheck(infoHashes: string[]): Promise<Map<string, boolean>>;
  /** Resolve to a playable https URL, or throw `DeadLinkError` if the file can't be delivered. */
  resolve(target: ResolveTarget): Promise<string>;
}

/** Thrown by `resolve` when a link is gone/unavailable → the route answers 404 so the client falls through. */
export class DeadLinkError extends Error {
  constructor(message = "dead_link") {
    super(message);
    this.name = "DeadLinkError";
  }
}

export type { FetchLike };

/** `magnet:?xt=urn:btih:<hash>` — every store adds a torrent by magnet, not a .torrent file. */
export function magnetFor(infoHash: string): string {
  return `magnet:?xt=urn:btih:${infoHash}`;
}

/** All-false map (the cache-check floor when a provider has no usable cache API, e.g. Real-Debrid). */
export function allUncached(infoHashes: string[]): Map<string, boolean> {
  return new Map(infoHashes.map((h) => [h, false]));
}
