/**
 * Den Scout ranker — ported from VortX `app/SourcesShared/StreamRanking.swift` (`junkClass()` +
 * the additive quality score), which discards theatrical rips far more precisely than Den's old
 * on-device `StreamRanker` (EPIC-den-scout / SCOUT-03). Pure + unit-tested; no I/O.
 *
 * Two things this gets right that the old parser did not:
 *   1. Ambiguous bare tokens (`cam`/`ts`/`scr`) count as junk ONLY when no good-source marker is
 *      present — so a WEB-DL whose title happens to contain "ts" is not wrongly sunk.
 *   2. Separator-tolerant patterns (`[ .\-_]?`) catch `hd.cam` / `hd-cam` / `hdcam` alike.
 */

const GIB = 1_073_741_824;

/** A scrape result before ranking — the raw torrent fact plus the debrid cache truth. */
export interface RawStream {
  infoHash: string;
  fileIdx?: number;
  /** Full release title, parsed for quality (Torrentio encodes res/codec/size/seeders here). */
  title: string;
  sizeBytes?: number;
  seeders?: number;
  /** Debrid cache truth (from the store's cache-check) — the +8000 dominance term. */
  cached: boolean;
  /** Which indexer produced this (dedupe/telemetry). */
  source: string;
}

// MARK: - Junk detection (VortX junkClass)

/** Good-source markers — their presence downgrades the bare `cam`/`ts`/`scr` tokens to non-junk. */
const GOOD_SOURCE = /remux|bluray|blu-ray|b[dr][ .\-_]?rip|web[ .\-_]?(dl|rip)?|hdtv|dvd[ .\-_]?rip/;

/** Unambiguous junk forms — always junk regardless of any other marker in the title. */
const UNAMBIGUOUS_JUNK: ReadonlyArray<readonly [string, RegExp]> = [
  ["cam", /h[dq][ .\-_]?cam(rip)?|cam[ .\-_]?rip|s[ .\-]+print/],
  ["telesync", /telesynch?|hd[ .\-_]?ts(rip)?|ts[ .\-_]?rip/],
  ["telecine", /telecine|hd[ .\-_]?tc/],
  ["screener", /(dvd|bd|br|web|hd)[ .\-_]?scr|p(re)?dvd(rip)?|screener/],
  ["workprint", /workprint/],
  ["r5", /\br5\b/],
  ["upscaled", /1xbet|read[ .\-_]?note|(?<!not[ .\-_])(?<!non[ .\-_])(upscaled?|up[ .\-_]?rez)|ai[ .\-_]?(upscaled?|enhanced?)|re[ .\-_]?graded?/],
];

/** The junk class of a title (`"cam"`, `"telesync"`, …) or `null` if it's a legit source. */
export function junkClass(title: string): string | null {
  const t = title.toLowerCase();
  for (const [cls, re] of UNAMBIGUOUS_JUNK) if (re.test(t)) return cls;
  if (!GOOD_SOURCE.test(t)) {
    if (/\bcam\b/.test(t)) return "cam";
    if (/\bts\b/.test(t)) return "telesync";
    if (/\bscr\b/.test(t)) return "screener";
  }
  return null;
}

// MARK: - Quality parse

/** Coarse resolution bucket for the config `resolutions` filter (null when untagged). */
export function detectResolution(title: string): "2160p" | "1080p" | "720p" | "480p" | null {
  const t = title.toLowerCase();
  if (/2160p?/.test(t)) return "2160p";
  if (/1080p?/.test(t)) return "1080p";
  if (/720p?/.test(t)) return "720p";
  if (/480p?|576p?|540p?/.test(t)) return "480p";
  return null;
}

function resolutionBase(t: string, sizeBytes?: number): number {
  if (/2160p?/.test(t)) return 4000;
  if (/1440p?/.test(t)) return 1440;
  if (/1080p?/.test(t)) return 1080;
  if (/720p?/.test(t)) return 720;
  if (/576p?/.test(t)) return 576;
  if (/540p?/.test(t)) return 540;
  if (/480p?/.test(t)) return 480;
  // Marketing "4k"/"uhd" is only trusted when the file isn't implausibly small (defeats fake labels).
  if (/4k|uhd/.test(t) && (sizeBytes ?? 0) > 3 * GIB) return 4000;
  return 100;
}

/** Additive quality score — higher wins. Big-magnitude tiers enforce ordering (junk ≪ cached ≪ rest). */
export function qualityScore(s: RawStream): number {
  const t = s.title.toLowerCase();
  let score = 0;

  if (junkClass(t) !== null) score -= 100_000; // sink CAM/TS/screener below every legit source
  if (s.cached) score += 8000; // cache dominance — a cached stream always beats an uncached one

  score += resolutionBase(t, s.sizeBytes);

  // Source class ladder.
  if (/\bremux\b/.test(t)) score += 230;
  else if (/bluray|blu-ray/.test(t) || /b[dr][ .\-_]?rip/.test(t)) score += 150;
  else if (/web[ .\-_]?dl/.test(t)) score += 75;
  else if (/web[ .\-_]?rip/.test(t)) score += 50;
  else if (/\bweb\b/.test(t)) score += 75;
  if (/\bhdtv\b/.test(t)) score -= 150;
  if (/dvd[ .\-_]?rip/.test(t)) score -= 200;
  if (/tvrip|satrip|pdtv/.test(t)) score -= 300;

  // HDR / video range (specific first).
  if (/dolby vision|dolbyvision|dovi/.test(t)) score += 30;
  else if (/hdr10\+|hdr10plus/.test(t)) score += 24;
  else if (/\bhdr\b|\bhlg\b/.test(t)) score += 18;

  // Audio (object-based > lossless > lossy).
  if (/atmos/.test(t)) score += 26;
  else if (/dts:x|dtsx|dts-x/.test(t)) score += 24;
  else if (/truehd|true-hd/.test(t)) score += 20;
  else if (/dts-hd ma|dts-hd\.ma|dts-ma/.test(t)) score += 16;
  else if (/dts-hd|dts hd|dtshd|flac|lpcm|pcm/.test(t)) score += 12;
  else if (/eac3|e-ac3|dd\+|ddp|ddplus/.test(t)) score += 8;
  else if (/\bdts\b/.test(t)) score += 6;
  else if (/ac3|\bdd\b|dolby digital/.test(t)) score += 4;

  // File size, capped so a bloated encode can't dominate.
  if (s.sizeBytes) score += Math.min(Math.round((s.sizeBytes / GIB) * 6), 600);

  // Penalties for what this hardware / display can't handle well.
  const is4k = /2160p?|4k|uhd/.test(t);
  if (/av1/.test(t)) score += is4k ? -1500 : -150; // no A15 hw decode
  if (/\b3d\b|hsbs|half[ .\-_]?sbs|sbs[ .\-_]?3d/.test(t)) score -= 2000; // flat panel
  if (/korsub|\bhc\b/.test(t)) score -= 200; // hardcoded subs

  return score;
}

// MARK: - Real-Debrid filename block

/**
 * Real-Debrid refuses to serve releases whose filename matches its anti-piracy patterns, so an
 * RD-only user must never be offered them (they'd 404 at resolve). Two forms, case-insensitive:
 *   1. substring anywhere: web-dl, webrip, bdrip, hdrip, dvdrip
 *   2. dot-adjacent Source.Codec: bluray.x264, hdtv.x264, hdtv.xvid, web.x264, web.h264
 * ~34% of PirateBay releases hit this. TorBox/Premiumize don't block, so this is applied only when
 * Real-Debrid is the ONLY store (else another store serves the file and RD is skipped for it).
 */
const RD_BLOCK_SUBSTRINGS = ["web-dl", "webrip", "bdrip", "hdrip", "dvdrip"] as const;
const RD_BLOCK_DOT_ADJACENT = ["bluray.x264", "hdtv.x264", "hdtv.xvid", "web.x264", "web.h264"] as const;

export function realDebridBlocked(title: string): boolean {
  const t = title.toLowerCase();
  // The dot-adjacent forms embed literal dots, so a lowercased substring test is exactly the rule.
  return RD_BLOCK_SUBSTRINGS.some((s) => t.includes(s)) || RD_BLOCK_DOT_ADJACENT.some((s) => t.includes(s));
}

// MARK: - Filter + rank

interface RankFilters {
  excludeCam: boolean;
  resolutions?: ReadonlyArray<"2160p" | "1080p" | "720p" | "480p">;
  hdrOnly?: boolean;
  minSeeders?: number;
  maxSizeGB?: number;
  excludeRegex?: string;
  cachedOnly: boolean;
  resultCap: number;
}

/**
 * Apply the config's filters, then sort by `qualityScore` (stable — original order breaks ties),
 * then cap. `resolutions` keeps untagged streams (never over-filter a legit but poorly-named rip).
 */
export function rankStreams(streams: RawStream[], filters: RankFilters): RawStream[] {
  let out = streams;

  if (filters.excludeCam) out = out.filter((s) => junkClass(s.title) === null);

  if (filters.excludeRegex) {
    try {
      const re = new RegExp(filters.excludeRegex, "i");
      out = out.filter((s) => !re.test(s.title));
    } catch {
      // A malformed user regex is ignored rather than dropping every stream.
    }
  }

  if (filters.resolutions && filters.resolutions.length > 0) {
    const allowed = new Set(filters.resolutions);
    out = out.filter((s) => {
      const res = detectResolution(s.title);
      return res === null || allowed.has(res);
    });
  }

  if (filters.hdrOnly) out = out.filter((s) => /dolby vision|dolbyvision|dovi|\bhdr\b|hdr10|\bhlg\b/i.test(s.title));
  if (filters.minSeeders != null) out = out.filter((s) => (s.seeders ?? 0) >= filters.minSeeders!);
  if (filters.maxSizeGB != null) out = out.filter((s) => (s.sizeBytes ?? 0) <= filters.maxSizeGB! * GIB);
  if (filters.cachedOnly) out = out.filter((s) => s.cached);

  return out
    .map((s, index) => ({ s, index, score: qualityScore(s) }))
    // Quality first; seeders break ties (a healthier swarm resolves/streams more reliably when a
    // title isn't cached); original order is the final stable tiebreak.
    .sort((a, b) => b.score - a.score || (b.s.seeders ?? 0) - (a.s.seeders ?? 0) || a.index - b.index)
    .slice(0, filters.resultCap)
    .map((x) => x.s);
}
