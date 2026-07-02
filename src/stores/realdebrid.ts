/**
 * Real-Debrid store (SCOUT-01). RD deprecated its instant-availability endpoint, so there is no
 * usable batch cache API — `cacheCheck` reports all-uncached (TorBox is the cache-truth provider; a
 * hash cached on TorBox still wins for an RD+TorBox user). `resolve` DOES work: add magnet → select
 * the wanted file → unrestrict → https link. Docs: api.real-debrid.com/rest/1.0.
 */
import type { DebridService } from "../config.js";
import { type FetchLike, type ResolveTarget, type Store, DeadLinkError, allUncached, magnetFor } from "./types.js";
import { pickEpisodeFile, type TorrentFile } from "../season.js";

const API = "https://api.real-debrid.com/rest/1.0";

interface RdFile {
  id: number;
  path: string;
  bytes: number;
}
interface RdInfo {
  status?: string;
  files?: RdFile[];
  links?: string[];
}

export class RealDebridStore implements Store {
  readonly service: DebridService = "realdebrid";

  constructor(
    private readonly token: string,
    private readonly fetch: FetchLike,
    private readonly api: string = API,
  ) {}

  private get headers(): Record<string, string> {
    return { authorization: `Bearer ${this.token}`, accept: "application/json" };
  }

  async cacheCheck(infoHashes: string[]): Promise<Map<string, boolean>> {
    return allUncached(infoHashes);
  }

  async resolve(target: ResolveTarget): Promise<string> {
    const id = await this.addMagnet(target.infoHash);
    const info = await this.info(id);
    const files = (info.files ?? []).map<TorrentFile>((f) => ({ index: f.id, name: f.path, sizeBytes: f.bytes }));
    const fileId = this.pickFileId(files, target);
    if (fileId == null) throw new DeadLinkError("realdebrid no file");

    await this.selectFiles(id, fileId);
    const ready = await this.info(id);
    const restricted = ready.links?.[0];
    if (!restricted) throw new DeadLinkError("realdebrid not ready");
    return this.unrestrict(restricted);
  }

  private pickFileId(files: TorrentFile[], target: ResolveTarget): number | undefined {
    if (files.length === 0) return undefined;
    if (target.fileIdx != null) return files[target.fileIdx]?.index ?? files[0].index; // torrent-order position → RD id
    if (target.season != null && target.episode != null) {
      return pickEpisodeFile(files, target.season, target.episode) ?? undefined;
    }
    return files.reduce((a, b) => ((b.sizeBytes ?? 0) > (a.sizeBytes ?? 0) ? b : a)).index; // largest = the movie
  }

  private async addMagnet(infoHash: string): Promise<string> {
    const res = await this.post("/torrents/addMagnet", { magnet: magnetFor(infoHash) });
    if (!res.ok) throw new DeadLinkError(`realdebrid addMagnet http ${res.status}`);
    const body = (await res.json()) as { id?: string };
    if (!body.id) throw new DeadLinkError("realdebrid no torrent id");
    return body.id;
  }

  private async info(id: string): Promise<RdInfo> {
    const res = await this.fetch(`${this.api}/torrents/info/${id}`, { headers: this.headers });
    if (!res.ok) throw new DeadLinkError(`realdebrid info http ${res.status}`);
    return (await res.json()) as RdInfo;
  }

  private async selectFiles(id: string, fileId: number): Promise<void> {
    const res = await this.post(`/torrents/selectFiles/${id}`, { files: String(fileId) });
    if (!res.ok) throw new DeadLinkError(`realdebrid selectFiles http ${res.status}`);
  }

  private async unrestrict(link: string): Promise<string> {
    const res = await this.post("/unrestrict/link", { link });
    if (!res.ok) throw new DeadLinkError(`realdebrid unrestrict http ${res.status}`);
    const body = (await res.json()) as { download?: string };
    if (!body.download) throw new DeadLinkError("realdebrid no download");
    return body.download;
  }

  private post(path: string, fields: Record<string, string>): Promise<Response> {
    return this.fetch(`${this.api}${path}`, {
      method: "POST",
      headers: { ...this.headers, "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams(fields).toString(),
    });
  }
}
