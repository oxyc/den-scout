/**
 * Clean stream labels (SCOUT-03). Den shows a short, human line — "4K • HDR • Atmos • 18 GB" — not
 * the raw scene release name. The junk is already gone by the time we get here (ranker dropped it),
 * so this only has to summarize quality + size.
 */
import type { RawStream } from "./rank.js";

const GIB = 1_073_741_824;

function resolutionLabel(t: string, sizeBytes?: number): string | null {
  if (/2160p?/.test(t)) return "4K";
  if (/1440p?/.test(t)) return "1440p";
  if (/1080p?/.test(t)) return "1080p";
  if (/720p?/.test(t)) return "720p";
  if (/480p?|576p?|540p?/.test(t)) return "SD";
  if (/4k|uhd/.test(t) && (sizeBytes ?? 0) > 3 * GIB) return "4K";
  return null;
}

function dynamicRangeLabel(t: string): string | null {
  if (/dolby vision|dolbyvision|dovi/.test(t)) return "Dolby Vision";
  if (/hdr10\+|hdr10plus/.test(t)) return "HDR10+";
  if (/\bhdr\b|hdr10|\bhlg\b/.test(t)) return "HDR";
  return null;
}

function sourceLabel(t: string): string | null {
  if (/\bremux\b/.test(t)) return "REMUX";
  if (/bluray|blu-ray|b[dr][ .\-_]?rip/.test(t)) return "BluRay";
  if (/web[ .\-_]?dl/.test(t)) return "WEB-DL";
  if (/web[ .\-_]?rip|\bweb\b/.test(t)) return "WEB";
  return null;
}

/** Bytes → "18 GB" / "720 MB" (one decimal under 10 GB so small files stay legible). */
export function sizeLabel(bytes: number): string {
  if (bytes >= GIB) {
    const gb = bytes / GIB;
    return `${gb >= 10 ? Math.round(gb) : gb.toFixed(1)} GB`;
  }
  return `${Math.max(1, Math.round(bytes / (1024 * 1024)))} MB`;
}

/** The bullet-joined summary line shown as the stream's title in Den. */
export function cleanLabel(s: RawStream): string {
  const t = s.title.toLowerCase();
  const parts: string[] = [];

  const res = resolutionLabel(t, s.sizeBytes);
  if (res) parts.push(res);

  const source = sourceLabel(t);
  if (source) parts.push(source);

  const dr = dynamicRangeLabel(t);
  if (dr) parts.push(dr);

  if (/atmos/.test(t)) parts.push("Atmos");

  if (s.sizeBytes) parts.push(sizeLabel(s.sizeBytes));

  return parts.length > 0 ? parts.join(" • ") : "Stream";
}
