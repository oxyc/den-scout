/**
 * TorBox store (SCOUT-01) — the DEFAULT and the one with a real, batched cache API, which is why
 * cached-only playback "just works" here. Docs: api.torbox.app.
 *   cacheCheck → GET /torrents/checkcached?hash=<csv>&format=object   (batched)
 *   resolve    → POST /torrents/createtorrent (magnet) → GET /torrents/requestdl?...  → https link
 */
import type { DebridService } from "../config.js";
import { type FetchLike, type ResolveTarget, type Store, DeadLinkError, magnetFor } from "./types.js";
import { pickEpisodeFile, type TorrentFile } from "../season.js";

const API = "https://api.torbox.app/v1/api";
// TorBox accepts up to 500 hashes per checkcached call; 100 keeps the GET URL comfortably short.
const CACHE_BATCH = 100;

interface TbFile {
  id: number;
  name?: string;
  short_name?: string;
  size?: number;
}

export class TorBoxStore implements Store {
  readonly service: DebridService = "torbox";

  constructor(
    private readonly token: string,
    private readonly fetch: FetchLike,
    private readonly api: string = API,
  ) {}

  private get authHeaders(): Record<string, string> {
    return { authorization: `Bearer ${this.token}`, accept: "application/json" };
  }

  async cacheCheck(infoHashes: string[]): Promise<Map<string, boolean>> {
    const result = new Map<string, boolean>(infoHashes.map((h) => [h, false]));
    for (let i = 0; i < infoHashes.length; i += CACHE_BATCH) {
      const batch = infoHashes.slice(i, i + CACHE_BATCH);
      try {
        const url = `${this.api}/torrents/checkcached?hash=${batch.join(",")}&format=object&list_files=false`;
        const res = await this.fetch(url, { headers: this.authHeaders });
        if (!res.ok) continue; // a failed batch leaves those hashes at their `false` default
        const body = (await res.json()) as { data?: Record<string, unknown> | null };
        const data = body.data ?? {};
        for (const hash of batch) if (data[hash] || data[hash.toUpperCase()]) result.set(hash, true);
      } catch {
        // Network blip on one batch must not sink the whole cache-check.
      }
    }
    return result;
  }

  async resolve(target: ResolveTarget): Promise<string> {
    const torrentId = await this.addMagnet(target.infoHash);
    const fileId = await this.resolveFileId(torrentId, target);
    const params = new URLSearchParams({ token: this.token, torrent_id: String(torrentId) });
    if (fileId != null) params.set("file_id", String(fileId));
    const res = await this.fetch(`${this.api}/torrents/requestdl?${params}`, { headers: this.authHeaders });
    if (!res.ok) throw new DeadLinkError(`torbox requestdl http ${res.status}`);
    const body = (await res.json()) as { success?: boolean; data?: unknown };
    const link = typeof body.data === "string" ? body.data : undefined;
    if (!body.success || !link) throw new DeadLinkError("torbox no link");
    return link;
  }

  private async addMagnet(infoHash: string): Promise<number> {
    const form = new URLSearchParams({ magnet: magnetFor(infoHash), seed: "3", allow_zip: "false" });
    const res = await this.fetch(`${this.api}/torrents/createtorrent`, {
      method: "POST",
      headers: { ...this.authHeaders, "content-type": "application/x-www-form-urlencoded" },
      body: form.toString(),
    });
    if (!res.ok) throw new DeadLinkError(`torbox createtorrent http ${res.status}`);
    const body = (await res.json()) as { data?: { torrent_id?: number } | null };
    const id = body.data?.torrent_id;
    if (typeof id !== "number") throw new DeadLinkError("torbox no torrent_id");
    return id;
  }

  /** File index for the requested file: the explicit `fileIdx`, else the episode pick from the pack. */
  private async resolveFileId(torrentId: number, target: ResolveTarget): Promise<number | undefined> {
    if (target.fileIdx != null) return target.fileIdx;
    if (target.season == null || target.episode == null) return undefined; // movie → the whole torrent
    const files = await this.listFiles(torrentId);
    if (files.length === 0) return undefined;
    const picked = pickEpisodeFile(files, target.season, target.episode);
    return picked ?? undefined;
  }

  private async listFiles(torrentId: number): Promise<TorrentFile[]> {
    const res = await this.fetch(`${this.api}/torrents/mylist?id=${torrentId}&bypass_cache=true`, {
      headers: this.authHeaders,
    });
    if (!res.ok) return [];
    const body = (await res.json()) as { data?: { files?: TbFile[] } | Array<{ files?: TbFile[] }> | null };
    const entry = Array.isArray(body.data) ? body.data[0] : body.data;
    const files = entry?.files ?? [];
    return files.map((f) => ({ index: f.id, name: f.name ?? f.short_name ?? "", sizeBytes: f.size }));
  }
}
