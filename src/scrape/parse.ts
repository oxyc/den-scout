/**
 * Shared parser for the Stremio "annotated stream" wire shape that Torrentio, Comet, MediaFusion and
 * Torz all emit: a `streams[]` where each item carries an `infoHash` (+ optional `fileIdx`) and packs
 * the release title, size and seeders into `name`/`title`/`description` text (often emoji-tagged:
 * `👤 seeders`, `💾 size`). We keep only torrent-shaped rows (an infohash) — Scout does its own
 * cache-check + resolve, so we ignore any pre-resolved `url` a debrid-aware addon returns.
 */
import type { RawStreamSeed } from "./types.js";

interface WireStream {
  name?: string;
  title?: string;
  description?: string;
  infoHash?: string;
  fileIdx?: number;
  behaviorHints?: {
    bingeGroup?: string;
    filename?: string;
    videoSize?: number;
  };
}

const GIB = 1_073_741_824;
const MIB = 1_048_576;

/** `💾 15.2 GB`, `Size: 15.2 GB`, or a bare `15.2 GB` / `700 MB` → bytes (GB treated as GiB). */
export function parseSize(text: string): number | undefined {
  const m = text.match(/(?:💾\s*)?([\d.]+)\s*(gib|gb|mib|mb)\b/i);
  if (!m) return undefined;
  const value = Number(m[1]);
  if (!Number.isFinite(value) || value <= 0) return undefined;
  const unit = m[2].toLowerCase();
  const bytes = unit.startsWith("g") ? value * GIB : value * MIB;
  return Math.round(bytes);
}

/** `👤 123`, `👥 123`, `Seeders: 123` → 123. */
export function parseSeeders(text: string): number | undefined {
  const m = text.match(/(?:👤|👥)\s*(\d+)/) ?? text.match(/seed(?:ers)?[:\s]+(\d+)/i);
  if (!m) return undefined;
  const n = Number(m[1]);
  return Number.isInteger(n) ? n : undefined;
}

/** Normalize an infohash: 40-char SHA-1 hex or 32-char base32, lowercased; else `null`. */
function normalizeHash(hash: unknown): string | null {
  if (typeof hash !== "string") return null;
  const h = hash.trim().toLowerCase();
  return /^[a-z0-9]{40}$|^[a-z0-9]{32}$/.test(h) ? h : null;
}

/** Parse one addon's JSON body into seeds. Tolerant of missing fields; drops non-torrent rows. */
export function parseStremioStreams(body: unknown, source: string): RawStreamSeed[] {
  if (typeof body !== "object" || body === null) return [];
  const streams = (body as { streams?: unknown }).streams;
  if (!Array.isArray(streams)) return [];

  const seeds: RawStreamSeed[] = [];
  for (const raw of streams) {
    if (typeof raw !== "object" || raw === null) continue;
    const s = raw as WireStream;
    const infoHash = normalizeHash(s.infoHash);
    if (!infoHash) continue;

    const text = [s.name, s.title, s.description].filter((x): x is string => typeof x === "string").join("\n");
    const releaseTitle = s.behaviorHints?.filename?.trim() || firstMeaningfulLine(text) || infoHash;
    const sizeBytes = s.behaviorHints?.videoSize && s.behaviorHints.videoSize > 0 ? s.behaviorHints.videoSize : parseSize(text);

    seeds.push({
      infoHash,
      fileIdx: typeof s.fileIdx === "number" && s.fileIdx >= 0 ? s.fileIdx : undefined,
      title: releaseTitle,
      sizeBytes,
      seeders: parseSeeders(text),
      source,
    });
  }
  return seeds;
}

/** First line that looks like a release title (skips a bare provider/quality header line). */
function firstMeaningfulLine(text: string): string | undefined {
  const lines = text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);
  // A line with a resolution/source token is the release title; else fall back to the first line.
  const titled = lines.find((l) => /\b(2160p|1080p|720p|480p|remux|bluray|web[ .\-_]?dl|web[ .\-_]?rip|hdtv|x264|x265|hevc)\b/i.test(l));
  return titled ?? lines[0];
}
