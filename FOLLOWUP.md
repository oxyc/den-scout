# Follow-ups

Deferred items from the TypeScript → Go port. Everything else in the audit was folded into
the port itself (see the commit history and the intentional-deviation notes in the code).

## Active goal
- **Sealed config-in-URL** (get BYOK secrets out of plaintext addon URLs) — see
  [`docs/SEALED-CONFIG.md`](docs/SEALED-CONFIG.md). den-scout is the reference impl.

## #13 — debrid file selection for multi-file packs

**Addressed** in commit `cfe8f1f`:
- TorBox no longer passes Torrentio's `fileIdx` straight through as its `file_id`; it lists the pack
  for any series episode and name-matches, and maps a bare `fileIdx` positionally to TorBox's own id
  (raw passthrough only when there's no list — single-file fast path / list failure).
- RD + Premiumize now prefer the episode name-match over the positional `fileIdx`.

**Residual (minor):** the *precedence* when a `fileIdx` is present WITHOUT an episode selector for a
movie delivered inside a multi-file pack is still best-effort (raw/positional). Rare; revisit only if a
concrete miss shows up. RD/PM still identify files positionally into their own listing, which holds for
the common cases.
