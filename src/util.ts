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

/** JSON `Response` with the right content-type. */
export function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

/** HTML `Response`. */
export function html(body: string): Response {
  return new Response(body, { headers: { "content-type": "text/html; charset=utf-8" } });
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
