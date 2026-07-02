/**
 * Premiumize store (SCOUT-01). Has a real batched cache API (`/cache/check`) like TorBox, and a
 * one-shot `directdl` that adds + resolves in a single call. Docs: premiumize.me/api.
 */
import type { DebridService } from "../config.js";
import { type FetchLike, type ResolveTarget, type Store, DeadLinkError, magnetFor } from "./types.js";
import { pickEpisodeFile, type TorrentFile } from "../season.js";

const API = "https://www.premiumize.me/api";
const CACHE_BATCH = 100;

interface PmContent {
  path?: string;
  link?: string;
  size?: number;
}

export class PremiumizeStore implements Store {
  readonly service: DebridService = "premiumize";

  constructor(
    private readonly token: string,
    private readonly fetch: FetchLike,
    private readonly api: string = API,
  ) {}

  async cacheCheck(infoHashes: string[]): Promise<Map<string, boolean>> {
    const result = new Map<string, boolean>(infoHashes.map((h) => [h, false]));
    const batches: string[][] = [];
    for (let i = 0; i < infoHashes.length; i += CACHE_BATCH) batches.push(infoHashes.slice(i, i + CACHE_BATCH));
    await Promise.all(
      batches.map(async (batch) => {
        const params = new URLSearchParams({ apikey: this.token });
        for (const h of batch) params.append("items[]", h);
        try {
          const res = await this.fetch(`${this.api}/cache/check?${params}`, { headers: { accept: "application/json" } });
          if (!res.ok) return;
          const body = (await res.json()) as { status?: string; response?: boolean[] };
          if (body.status !== "success" || !Array.isArray(body.response)) return;
          batch.forEach((h, idx) => body.response![idx] && result.set(h, true));
        } catch {
          // leave this batch's hashes at false
        }
      }),
    );
    return result;
  }

  async resolve(target: ResolveTarget): Promise<string> {
    const params = new URLSearchParams({ apikey: this.token, src: magnetFor(target.infoHash) });
    const res = await this.fetch(`${this.api}/transfer/directdl`, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: params.toString(),
    });
    if (!res.ok) throw new DeadLinkError(`premiumize directdl http ${res.status}`);
    const body = (await res.json()) as { status?: string; content?: PmContent[] };
    const content = body.content ?? [];
    if (body.status !== "success" || content.length === 0) throw new DeadLinkError("premiumize no content");

    const files = content.map<TorrentFile>((c, index) => ({ index, name: c.path ?? "", sizeBytes: c.size }));
    const idx = this.pickIndex(files, target);
    const link = idx != null ? content[idx]?.link : undefined;
    if (!link) throw new DeadLinkError("premiumize no link");
    return link;
  }

  private pickIndex(files: TorrentFile[], target: ResolveTarget): number | undefined {
    if (files.length === 0) return undefined;
    if (target.fileIdx != null && files[target.fileIdx]) return target.fileIdx;
    if (target.season != null && target.episode != null) {
      return pickEpisodeFile(files, target.season, target.episode) ?? undefined;
    }
    return files.reduce((a, b) => ((b.sizeBytes ?? 0) > (a.sizeBytes ?? 0) ? b : a)).index;
  }
}
