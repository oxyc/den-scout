# Follow-ups

Deferred items from the TypeScript → Go port. Everything else in the audit was folded into
the port itself (see the commit history and the intentional-deviation notes in the code).

## Done

- **Sealed config-in-URL** (get BYOK secrets out of plaintext addon URLs) —
  [`docs/SEALED-CONFIG.md`](docs/SEALED-CONFIG.md) records den-scout as the reference impl and marks it
  DONE. Activate per deployment with `CONFIG_KEY`; unset means legacy plaintext URLs, which still
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

## One huge JSON value in an account listing can OOM the container

Not a regression, and not reachable from a caller — `torboxAPI` is a constant, so this needs
`api.torbox.app` itself to send it. Recorded because the number was understated in the source for
several rounds and is worse than the tolerance that was written against it.

`encoding/json`'s `Token` must buffer a whole token before it can hand it over, so a single value just
under `maxListingBytes` is live twice at once: the decoder's own buffer, growing through 64 MiB to
128 MiB with both halves live during the copy, plus the materialised string. Measured peak **~257 MiB of
live heap** against `GOMEMLIMIT=230MiB` in a 256 MB container, i.e. an OOM kill. The length filter on
`hash` does not help; it can only run once the token already exists. And because the body still decodes
as a valid listing, it is memoised and the spike repeats every `listingTTL`.

**How this is measured matters more than the number, because the number has now been wrong in both
directions.** It was understated at ~159 MiB, corrected to 256.7 MiB, then "corrected" again to 304 MiB
— and that last one was the regression, not the fix. The probe behind it fed the decoder a
`strings.Reader`, so the fixture held the whole body live in the heap for the duration and the peak
counted the body twice. Production reads off a socket and never materialises the body. Re-measured with
the body streamed a chunk at a time, the original figures reproduce: **257.3 MiB** at the 64 MiB cap and
**129.3 MiB** at a 32 MiB one. Measure it streamed, or every figure here comes out high by exactly one
body.

The peak is also sharply sensitive to where the body falls between the decoder's buffer doublings — a
63 MiB value peaks at 160 MiB, a value just under 64 MiB at 257 MiB — so the worst case has to be built
just under the cap rather than at a round number near it.

A second, unrelated mechanism in the same function was found and FIXED rather than recorded: deeply
nested brackets grow `Decoder`'s own token stack, which is live rather than garbage, so 10 MiB of `[`
peaked at ~231 MiB — a sixth of the byte cap. `maxSkipDepth` now refuses past 64 levels, which takes it
to a rounding error, and the refusal is remembered rather than retried — see the constant for why, and
note that the same shape nested inside `data[]` already lands on `listingBadEnvelope` through
`encoding/json`'s own 10,000-level ceiling. It is called out here because the first mitigation below was
written as if it covered this shape and does not: at a 32 MiB cap the nesting case still peaks at
**700 MiB or more**. That one is bimodal — 700 MiB or 847 MiB depending on whether the sampler catches
the token stack's doubling copy — so treat 700 as a floor rather than a value.

Three ways out for the huge-scalar case, none free:

- Lower `maxListingBytes`. The peak steps at the decoder's buffer doublings, so ~32 MiB would cap the
  peak near 128 MiB — 129.3 MiB measured streamed. But this cap governs real large
  accounts, and lowering it makes them read as oversized — indeterminate, then escalation. A functional
  regression traded for a hostile-upstream case.
- Bound a single token. `encoding/json` offers no hook; it would mean a hand-written scanner for the
  listing, which is a lot of surface for this.
- Leave it and alert on it. `scout_background_panics_total` will not see an OOM, but the container
  restart will.

Left alone deliberately. If TorBox ever legitimately returns listings near the cap, revisit — at that
point the first option stops being a regression and starts being correct.

## `?probe=1` now walks a store's read chain on every poll

Not a defect — it is what makes the probe and `/play` agree — but it is a real cost, and the comment on
the route understated it for a while, so it is recorded rather than left to be rediscovered.

The readiness enquiry asks any store holding a torrent id, which was the fix for a probe answering 404
`not_queued` for a release `/play` served a 302 for. For Real-Debrid that means the whole read chain per
poll. Measured on an RD-held release:

- **ready:** 4 calls — `GET info`, `POST selectFiles`, `GET info`, `POST unrestrict/link`
- **still downloading:** 3 calls — `GET info`, `POST selectFiles`, `GET info`

At the client's ~2s cadence that is roughly 90 RD calls a minute for the length of a download, including
a state-changing `selectFiles` on every poll and a fresh unrestricted link minted and discarded on every
ready poll. `/play` walks the same chain on the same cadence, so this is the price of the two routes
agreeing rather than a new class of work — but if a real account is ever seen hitting an RD rate limit
while polling, this is the first thing to look at. The obvious mitigation is a short-lived memo of the
read chain's verdict, keyed per hash, which nothing needs yet.

## A cross-host redirect on a GET still carries the credential in the query string

Measured, not theorised, and deliberately left for now.

`RefuseRedirectReplay` closed the body channel: a 307/308 can no longer replay a POST, which was
multiplying a charged add by up to ten and forwarding Premiumize's apikey — carried in the `directdl`
form body — to whatever host the redirect named. The **query** channel is still open. Two call sites put
a credential in the URL:

- `stores.go` `requestDownload` — `token=<debrid token>`
- `stores.go` `premiumizeStore.CacheCheck` — `apikey=<debrid token>`

Go strips `Authorization` across hosts but never strips the query string, so a redirect to a different
host that preserves the query hands the credential over. Measured against a genuinely different hostname
(same host on another port proves nothing — Go compares host without port):

```
requestdl   302 -> foreign host:  tokenReachedForeignHost=1   authHeaderForwarded=0
cache/check 302 -> foreign host:  apikeyReachedForeignHost=1
```

**Reachability is the same class as the listing OOM above**: the API hosts are constants, so it needs
`api.torbox.app` / `api.premiumize.me` itself, a terminator in front of one, or the operator's own proxy
(`main.go` sets `Proxy: http.ProxyFromEnvironment`) to answer with a cross-host redirect that keeps the
query.

Left alone because the honest fix is structural rather than a guard. The rule wanted here is by URL and
host, not by method: a debrid API call should refuse any redirect that changes host, while the probe
client must keep following them, since reading a playback link through a CDN redirect is its entire job.
That means splitting the single shared `http.Client` into a store client and a probe client — a change
worth making deliberately rather than as the last edit before a tag. Revisit with that split; the guard
that exists already covers the more expensive half.

## The test suite shares process-global state between tests

Not a regression — both cases below reproduce at every commit tried, including before the audit branch.
Recorded because they cap how hard the suite can be re-run, which is the main tool the audit rounds
verify with.

**The hourly add budget accumulates across repetitions.**

```
go test ./internal/scout -count=3 -shuffle=on -cpu=1,4
--- FAIL: TestRealDebridResolve_happyPathReturnsTheUnrestrictedLink
    link = "", err = ... realdebrid scout's own hourly add budget for this account is spent
```

It is process-global and keyed by account token (`addbudget.go`), so tests spending an add against the
same token add up. Two passes stay under the 50/hour ceiling; three do not.

**The metrics counters are shared, and some builds are counted by another test.**

```
go test ./internal/scout -count=2 -shuffle=1788610563644591000 -race
--- FAIL: TestMetrics_reportsWhatWasPreviouslyLogOnly
    metrics_test.go:87: scout_list_builds_total{result="ok"} moved by 2, want 1
```

`metrics` is a package-level var (`metrics.go`) and the test measures a delta around one build, so any
other test — or a background rebuild goroutine outliving the test that started it — landing between the
two readings breaks it. Order-dependent, so it needs the seed above to reproduce.

Both are the same root cause: package-level mutable state with no per-test isolation. The fix is
test-side — a distinct token per test, and a metrics set that can be swapped or reset in a `t.Cleanup`.
Left alone deliberately: both predate this branch and touching them means editing many unrelated tests.
`-count=2` without a seed is usually green, which is what the audit rounds used, but that is the ceiling
being generous rather than the tests being isolated.

## #13 — debrid file selection for multi-file packs

**Addressed** in commit `cfe8f1f`:
- TorBox no longer passes Torrentio's `fileIdx` straight through as its `file_id`; it lists the pack
  for any series episode and name-matches, and maps a bare `fileIdx` positionally to TorBox's own id
  (raw passthrough only when there's no list — single-file fast path / list failure).
- RD + Premiumize now prefer the episode name-match over the positional `fileIdx`.

**Residual: an absolute-numbered pack gets a confidently wrong episode.** `largestEpisodeCandidate`
only excludes files that *rule themselves out* by naming a different episode, and `labelledEpisodeRe`
does not recognise bare absolute numbering (`Show - 24.mkv`), which is the normal shape for anime on
public indexers. So no file in such a pack rules itself out, the last-resort "largest video" fires, and
the answer tracks SIZE rather than the episode asked for. Measured on a two-file pack:

```
Show - 24.mkv (1 GiB) + Show - 25.mkv (3 GiB), asked S02E01 → Show - 25.mkv
Show - 24.mkv (3 GiB) + Show - 25.mkv (1 GiB), asked S02E01 → Show - 24.mkv
Show - 24.mkv (3 GiB) + Show - 25.mkv (1 GiB), asked S01E01 → Show - 24.mkv
```

The third is the sharp one: episode 1 is plainly not in a pack of episodes 24 and 25, and it is served
with a 302 and no error. A *labelled* pack missing the episode refuses correctly
(`torrent holds no file for that episode`), so the two spellings of the same situation behave opposite
ways — and the wrong-episode-with-a-302 symptom is the one `unlabelledCandidates` was written to remove.

Not fixed here because the fallback is load-bearing for the shape it was built for — a single feature
plus a sample, where the largest video IS the episode — and narrowing it means teaching the matcher
absolute numbering, which needs the season's episode count to map E01 of season 2 onto absolute 25.
That is a real feature, not a guard. Revisit with #13's other residual.

**Residual (minor):** the *precedence* when a `fileIdx` is present WITHOUT an episode selector for a
movie delivered inside a multi-file pack is still best-effort (raw/positional). Rare; revisit only if a
concrete miss shows up. RD/PM still identify files positionally into their own listing, which holds for
the common cases.
