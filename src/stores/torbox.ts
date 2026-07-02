/**
 * TorBox store (SCOUT-01) — the DEFAULT and the one with a real, batched cache API, which is why
 * cached-only playback "just works" here. Docs: api.torbox.app.
 *   cacheCheck → GET /torrents/checkcached?hash=<csv>&format=object   (batched)
 *   resolve    → POST /torrents/createtorrent (magnet) → GET /torrents/requestdl?...  → https link
 */
import type { DebridService } from "../config.js";
import { type FetchLike, type ResolveTarget, type Store, DeadLinkError, magnetFor } from "./types.js";
import { pickEpisodeFile, type TorrentFile } from "../season.js";
import type { Cache } from "../cache.js";

const API = "https://api.torbox.app/v1/api";
// TorBox accepts up to 500 hashes per checkcached call; 100 keeps the GET URL comfortably short.
const CACHE_BATCH = 100;
// A torrent's id + file layout are stable while it's in the account; cache them so binge episodes 2..N
// on the same season-pack infohash skip createtorrent + mylist and go straight to requestdl.
const RESOLVE_CACHE_TTL = 6 * 3600;

interface TbFile {
  id: number;
  name?: string;
  short_name?: string;
  size?: number;
}

interface ResolveEntry {
  torrentId: number;
  files: TorrentFile[];
}

export class TorBoxStore implements Store {
  readonly service: DebridService = "torbox";

  constructor(
    private readonly token: string,
    private readonly fetch: FetchLike,
    private readonly cache?: Cache,
    private readonly api: string = API,
  ) {}

  private get authHeaders(): Record<string, string> {
    return { authorization: `Bearer ${this.token}`, accept: "application/json" };
  }

  async cacheCheck(infoHashes: string[]): Promise<Map<string, boolean>> {
    const result = new Map<string, boolean>(infoHashes.map((h) => [h, false]));
    const batches: string[][] = [];
    for (let i = 0; i < infoHashes.length; i += CACHE_BATCH) batches.push(infoHashes.slice(i, i + CACHE_BATCH));
    // Batches are independent — run them concurrently so a >100-hash (season/large-swarm) check
    // isn't serial round-trips.
    await Promise.all(
      batches.map(async (batch) => {
        try {
          const url = `${this.api}/torrents/checkcached?hash=${batch.join(",")}&format=object&list_files=false`;
          const res = await this.fetch(url, { headers: this.authHeaders });
          if (!res.ok) return; // a failed batch leaves those hashes at their `false` default
          const body = (await res.json()) as { data?: Record<string, unknown> | null };
          const data = body.data ?? {};
          for (const hash of batch) if (data[hash] || data[hash.toUpperCase()]) result.set(hash, true);
        } catch {
          // Network blip on one batch must not sink the whole cache-check.
        }
      }),
    );
    return result;
  }

  async resolve(target: ResolveTarget): Promise<string> {
    // Fast path: a warm resolve entry (from an earlier episode of the same pack) → one requestdl call,
    // no createtorrent + mylist. If the cached id is stale (requestdl fails), fall through to a fresh add.
    const cacheKey = `torbox:resolve:${target.infoHash}`;
    const cached = await this.readEntry(cacheKey);
    if (cached) {
      try {
        return await this.requestDownload(cached.torrentId, selectFileId(cached.files, target));
      } catch {
        // stale torrent id / link — rebuild below
      }
    }

    const torrentId = await this.addMagnet(target.infoHash);
    // List files only when we need them to pick an episode from a pack; a movie/explicit-fileIdx doesn't.
    const files = target.fileIdx == null && target.season != null && target.episode != null
      ? await this.listFiles(torrentId)
      : [];
    await this.writeEntry(cacheKey, { torrentId, files });
    return this.requestDownload(torrentId, selectFileId(files, target));
  }

  private async requestDownload(torrentId: number, fileId: number | undefined): Promise<string> {
    const params = new URLSearchParams({ token: this.token, torrent_id: String(torrentId) });
    if (fileId != null) params.set("file_id", String(fileId));
    const res = await this.fetch(`${this.api}/torrents/requestdl?${params}`, { headers: this.authHeaders });
    if (!res.ok) throw new DeadLinkError(`torbox requestdl http ${res.status}`);
    const body = (await res.json()) as { success?: boolean; data?: unknown };
    const link = typeof body.data === "string" ? body.data : undefined;
    if (!body.success || !link) throw new DeadLinkError("torbox no link");
    return link;
  }

  private async readEntry(key: string): Promise<ResolveEntry | null> {
    if (!this.cache) return null;
    const raw = await this.cache.get(key);
    if (!raw) return null;
    try {
      return JSON.parse(raw) as ResolveEntry;
    } catch {
      return null;
    }
  }

  private async writeEntry(key: string, entry: ResolveEntry): Promise<void> {
    await this.cache?.put(key, JSON.stringify(entry), RESOLVE_CACHE_TTL);
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

/** The file index for a target given an already-loaded file list (pure — no I/O). */
function selectFileId(files: TorrentFile[], target: ResolveTarget): number | undefined {
  if (target.fileIdx != null) return target.fileIdx; // explicit file
  if (target.season == null || target.episode == null) return undefined; // movie → the whole torrent
  if (files.length === 0) return undefined;
  return pickEpisodeFile(files, target.season, target.episode) ?? undefined;
}
