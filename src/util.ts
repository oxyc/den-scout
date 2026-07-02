/** Small shared helpers. */

/** FNV-1a 32-bit hash → 8-char hex. Used to key caches by config WITHOUT putting the token in the key. */
export function fnv1a(input: string): string {
  let h = 0x811c9dc5;
  for (let i = 0; i < input.length; i++) {
    h ^= input.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return (h >>> 0).toString(16).padStart(8, "0");
}

/** JSON `Response` with the right content-type and an optional `Cache-Control`. */
export function json(body: unknown, status = 200, cacheControl?: string): Response {
  const headers: Record<string, string> = { "content-type": "application/json" };
  if (cacheControl) headers["cache-control"] = cacheControl;
  return new Response(JSON.stringify(body), { status, headers });
}

/**
 * A cacheable GET response with an `ETag` derived from the body, honoring `If-None-Match` → `304`.
 * After `Cache-Control: max-age` lapses, a client (or a CDN/Caddy in front) revalidates with a tiny
 * conditional GET and gets an empty 304 instead of the body when nothing changed. A strong ETag is
 * fine here: the body is deterministic for a given (config, id) at a point in time.
 */
export function conditionalResponse(request: Request, body: string, contentType: string, cacheControl: string): Response {
  const etag = `"${fnv1a(body)}"`;
  const ifNoneMatch = request.headers.get("if-none-match");
  if (ifNoneMatch && (ifNoneMatch.trim() === "*" || ifNoneMatch.split(",").some((tag) => tag.trim() === etag))) {
    return new Response(null, { status: 304, headers: { etag, "cache-control": cacheControl } });
  }
  return new Response(body, { status: 200, headers: { "content-type": contentType, "cache-control": cacheControl, etag } });
}

/**
 * Race a task against a per-task timeout. The task gets an `AbortSignal` (so a real fetch cancels),
 * but the timeout ALSO rejects on its own — a task that ignores the signal still can't hang the
 * fan-out. Resolves with the value, or rejects once `ms` elapses (the caller treats reject as "no
 * result").
 */
export function withTimeout<T>(fn: (signal: AbortSignal) => Promise<T>, ms: number): Promise<T> {
  const controller = new AbortController();
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => {
      controller.abort(new Error("timeout"));
      reject(new Error("timeout"));
    }, ms);
    fn(controller.signal).then(resolve, reject).finally(() => clearTimeout(timer));
  });
}
