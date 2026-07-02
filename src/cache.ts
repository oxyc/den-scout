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

/**
 * In-memory TTL + LRU cache. The default backend and the one tests use (deterministic via `clock`).
 * Bounded by `maxEntries` with least-recently-used eviction so the long tail of one-off titles can't
 * grow the heap without limit and get the container OOM-killed under its `mem_limit` — expired entries
 * are also dropped on access, and a `Map`'s insertion order gives us LRU for free.
 */
export class MemoryCache implements Cache {
  private readonly store = new Map<string, { value: string; expires: number }>();

  constructor(
    private readonly clock: () => number = () => Date.now(),
    private readonly maxEntries = 5000,
  ) {}

  async get(key: string): Promise<string | null> {
    const entry = this.store.get(key);
    if (!entry) return null;
    if (this.clock() > entry.expires) {
      this.store.delete(key);
      return null;
    }
    // LRU touch: re-insert so this key becomes most-recently-used (moves to the end of the Map).
    this.store.delete(key);
    this.store.set(key, entry);
    return entry.value;
  }

  async put(key: string, value: string, ttlSeconds: number): Promise<void> {
    this.store.delete(key); // re-insert at the end (most-recently-used)
    this.store.set(key, { value, expires: this.clock() + ttlSeconds * 1000 });
    if (this.store.size > this.maxEntries) {
      const oldest = this.store.keys().next().value; // first key = least-recently-used
      if (oldest !== undefined) this.store.delete(oldest);
    }
  }
}
