/**
 * Cache seam — the stream list + per-hash cache truth (SCOUT-03). The runtime-agnostic core only
 * needs get/put with a TTL; the entry point picks the backend (in-memory for dev/tests, Redis or a
 * SQLite volume in the homelab; a CF Worker would bind KV). No secrets are stored — keys are the
 * FNV of the config blob, never the token.
 */

export interface Cache {
  get(key: string): Promise<string | null>;
  put(key: string, value: string, ttlSeconds: number): Promise<void>;
}

/** In-memory TTL cache. The default backend and the one tests use (deterministic via `clock`). */
export class MemoryCache implements Cache {
  private readonly store = new Map<string, { value: string; expires: number }>();

  constructor(private readonly clock: () => number = () => Date.now()) {}

  async get(key: string): Promise<string | null> {
    const entry = this.store.get(key);
    if (!entry) return null;
    if (this.clock() > entry.expires) {
      this.store.delete(key);
      return null;
    }
    return entry.value;
  }

  async put(key: string, value: string, ttlSeconds: number): Promise<void> {
    this.store.set(key, { value, expires: this.clock() + ttlSeconds * 1000 });
  }
}
