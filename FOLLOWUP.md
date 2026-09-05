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
read keeps `statusBudget`; only a read that could not FIND OUT, which is the one path about to spend an
add, escalates for a definitive answer. That path was going to block on the add anyway, and it is
self-limiting: once anything is queued the torrent id is cached and later reads are fast. See
`TestPlay_statusTimeoutDoesNotAdd` and its control.

Two details of that escalation were each got wrong once, so they are recorded in the code rather than
here — see `escalatedStatusCtx` and `escalatedStatusBudget` in `handler.go`. In short: it takes double
the status budget **carved out of what remains of the resolve clock**, not a fresh `resolveBudget`, which
outlived the clock it was gating and made the add impossible; and it fires on the store reporting that it
could not find out, not on the caller's own deadline, which the pool's per-store budget slicing had made
a different question.

### `indexerconfig.go`'s `minted` map now has a count ceiling — fixed

`pruneMintedLocked` dropped entries past their own TTL, which cannot bound a map whose keys the caller
chooses: the key derives from a token nobody verifies, and comet's mint is local base64 with no round
trip, so every distinct token minted successfully and was held for 12 hours (~500 B each). It now
enforces `maxMintedEntries` (256) as well, least-recently-USED first. Not oldest-first: a legitimate
install's entries are the oldest in the map by construction — minted once, good for twelve hours — so
sorting by mint time evicts exactly the operator's own. Protecting recently-used entries from eviction
was tried on top of that and reverted; the reasoning is in `pruneMintedLocked`. A legitimate install has
one entry per (indexer, account) — under twenty.

## The test suite is order-dependent above `-count=2`

Not a regression — reproduced at every commit tried, including before the audit branch. Recorded because
it silently caps how hard the suite can be re-run, which is the main tool the audit rounds verify with.

```
go test ./internal/scout -count=3 -shuffle=on -cpu=1,4
--- FAIL: TestRealDebridResolve_happyPathReturnsTheUnrestrictedLink
    link = "", err = ... realdebrid scout's own hourly add budget for this account is spent
```

The hourly add budget is process-global and keyed by account token (`addbudget.go`), so tests that spend
an add against the same token accumulate across repetitions. Two passes stay under the 50/hour ceiling;
three do not. `-count=2 -shuffle=on -race` is therefore clean and is what the audit rounds used, but that
is a coincidence of the ceiling rather than isolation.

The fix is test-side: give each test its own token, or reset the budget in a `t.Cleanup`. Left alone
deliberately — it predates this branch and touching it means editing many unrelated tests.

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
