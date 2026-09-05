# Follow-ups

Deferred items from the TypeScript → Go port. Everything else in the audit was folded into
the port itself (see the commit history and the intentional-deviation notes in the code).

## Done

- **Sealed config-in-URL** (get BYOK secrets out of plaintext addon URLs) —
  [`docs/SEALED-CONFIG.md`](docs/SEALED-CONFIG.md) records den-scout as the reference impl and marks it
  DONE. Activate per deployment with `SCOUT_CONFIG_KEY`; unset means legacy plaintext URLs, which still
  resolve. The opaque `configId` this once pointed at is an explicit **non-goal** — sealing solved the
  problem it was for.

### `/play`'s status budget no longer spends an add on a torrent already downloading — fixed

`pool.Status` reports "nobody is fetching it" and "I could not find out in time" both as `ok=false`, so
a timed-out read fell through to `ResolvePreferring`, which queues the torrent. TorBox's `Status` makes
two upstream calls, one of them the account listing `stores.go` measures at ~13 MB on a 2,000-torrent
account, so the 8s budget is genuinely reachable there; measured against a 9-second listing, that was an
add a 45-second budget did not make.

Fixed by giving the two questions two budgets rather than moving the number back — the budget was
shortened deliberately, because this read answers a poll on a two-second cadence. The poll-answering
read keeps `statusBudget`; only a read that actually TIMED OUT, which is the one path about to spend an
add, escalates to `resolveBudget` for a definitive answer. That path was going to block on the add
anyway, and it is self-limiting: once anything is queued the torrent id is cached and later reads are
fast. See `TestPlay_statusTimeoutDoesNotAdd` and its control.

### `indexerconfig.go`'s `minted` map now has a count ceiling — fixed

`pruneMintedLocked` dropped entries past their own TTL, which cannot bound a map whose keys the caller
chooses: the key derives from a token nobody verifies, and comet's mint is local base64 with no round
trip, so every distinct token minted successfully and was held for 12 hours (~500 B each). It now
enforces `maxMintedEntries` (256) oldest-first as well. A legitimate install has one entry per
(indexer, account) — under twenty.

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
