# Follow-ups

Deferred items from the TypeScript → Go port. Everything else in the audit was folded into
the port itself (see the commit history and the intentional-deviation notes in the code).

## Active goal
- **Sealed config-in-URL** (get BYOK secrets out of plaintext addon URLs) — see
  [`docs/SEALED-CONFIG.md`](docs/SEALED-CONFIG.md). den-scout is the reference impl.

## Raised by the 2026-09 audit, deliberately NOT fixed there

Both are pre-existing and neither is touched by that changeset, so they were raised rather than folded
in. Measurements are from the audit and are reproducible.

### `/play`'s 8-second status budget can spend an add on a torrent already downloading

`handlePlay`'s "is it already downloading?" read runs on `statusBudget` (8s). TorBox's `Status` makes
**two** upstream calls inside it, one of them the account listing that `stores.go` itself measures at
~13 MB on a 2,000-torrent account — and a timed-out `Status` falls through to `ResolvePreferring`,
which queues the torrent. Measured against a 9-second listing: an add that a 45-second budget did not
make. It self-heals after one add per release (the torrent id is then cached for `resolveCacheTTL`), so
the cost is bounded — but opening a 10-episode season on a large account can spend 10 of the 50 hourly
adds on torrents that were already being fetched.

The tension is real and recorded in `handlePlay`'s own comment: the budget was *deliberately* shortened
from 45s because a poll-answering read must not hold a poll for forty-five seconds. Fixing this properly
means separating "how long may this read take" from "may a timeout queue an add" — most likely a
`NoAdd` retry on status timeout — rather than moving the number back.

### `indexerconfig.go`'s `minted` map is keyed by an unverified token, with a 12-hour TTL and no ceiling

`pruneMintedLocked` drops entries past their own TTL, which cannot bound a map whose keys the caller
chooses: a config's debrid token is never verified, comet's mint is local base64 with no round trip, so
every distinct token mints successfully and is retained for 12 hours. Estimated ~500 B/entry. Gated by
`SCOUT_MINT_INDEXER_CONFIGS`, which is **off by default**, which is why this is a follow-up and not a
fix. A size ceiling with LRU eviction is the shape.

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
