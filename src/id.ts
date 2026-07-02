/**
 * Stremio stream id parsing. Den bridges TMDB → IMDb, so ids are `tt<digits>` (movie) or
 * `tt<digits>:<season>:<episode>` (series episode). Anything else is rejected (→ 400).
 */

export interface StreamId {
  type: "movie" | "series";
  imdbId: string;
  season?: number;
  episode?: number;
}

/** Parse the `<type>` + `<id>.json` route segments, or `null` if malformed. */
export function parseStreamId(type: string, rawId: string): StreamId | null {
  if (type !== "movie" && type !== "series") return null;
  const id = rawId.replace(/\.json$/, "");
  const parts = id.split(":");
  const imdbId = parts[0] ?? "";
  if (!/^tt\d+$/.test(imdbId)) return null;

  if (type === "series") {
    const season = Number(parts[1]);
    const episode = Number(parts[2]);
    if (!Number.isInteger(season) || !Number.isInteger(episode) || season < 0 || episode < 0) return null;
    return { type, imdbId, season, episode };
  }
  return { type: "movie", imdbId };
}
