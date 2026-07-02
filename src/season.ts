/**
 * Season-pack → episode file-index map (SCOUT-05). When a scraped torrent is a whole-season pack,
 * the scrape gives no per-episode `fileIdx`, so `tt…:S:E` would resolve to the wrong file. Given the
 * torrent's file list (from the debrid store), pick the file that matches SxxExx. Pure + testable.
 */

export interface TorrentFile {
  /** The debrid store's file index (what `resolve` ultimately requests). */
  index: number;
  name: string;
  sizeBytes?: number;
}

const VIDEO_EXT = /\.(mkv|mp4|avi|m4v|ts|mov|wmv|flv|webm)$/i;

/**
 * The episode-match patterns for one (season, episode): `S01E02`, `1x02`, `S1.E2`,
 * `Season 1 Episode 2`, or a bare `102`/`0102`. Compiled once per resolve, then tested against every
 * file — not recompiled per file.
 */
function episodePatterns(season: number, episode: number): RegExp[] {
  const s = String(season);
  const e = String(episode);
  const s2 = s.padStart(2, "0");
  const e2 = e.padStart(2, "0");
  return [
    new RegExp(`s0*${s}[ ._-]*e0*${e}(?!\\d)`), // s01e02 / s1.e2 / s01 e02
    new RegExp(`\\b0*${s}x0*${e}(?!\\d)`), // 1x02
    new RegExp(`season[ ._-]*0*${s}[ ._-]*episode[ ._-]*0*${e}(?!\\d)`), // season 1 episode 2
    new RegExp(`\\b${s}${e2}(?!\\d)`), // 102 (S1E02) — season digit + 2-digit episode
    new RegExp(`\\b${s2}${e2}(?!\\d)`), // 0102
  ];
}

function matchesEpisode(name: string, patterns: RegExp[]): boolean {
  const n = name.toLowerCase();
  return patterns.some((re) => re.test(n));
}

/**
 * Pick the file index for an episode from a torrent's file list. Prefers an SxxExx match among video
 * files; if none matches (single-file torrent, or an already-correct pack), returns the largest video
 * file's index; `null` only when there is no video file at all.
 */
export function pickEpisodeFile(files: TorrentFile[], season: number, episode: number): number | null {
  const videos = files.filter((f) => VIDEO_EXT.test(f.name));
  const pool = videos.length > 0 ? videos : files;
  if (pool.length === 0) return null;

  const patterns = episodePatterns(season, episode);
  const matched = pool.filter((f) => matchesEpisode(f.name, patterns));
  if (matched.length > 0) {
    // Ties (e.g. a sample + the real file) → the largest, which is the actual episode.
    return matched.reduce((a, b) => ((b.sizeBytes ?? 0) > (a.sizeBytes ?? 0) ? b : a)).index;
  }

  // No episode marker: a single-file torrent for this episode, or a pack we can't disambiguate.
  return pool.reduce((a, b) => ((b.sizeBytes ?? 0) > (a.sizeBytes ?? 0) ? b : a)).index;
}
