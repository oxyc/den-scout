# Tasks

**Status: 10 of 11 shipped** (branch `audit-fixes`). Three implemented differently from the sketch
below, and one reverted — all recorded in their commit messages:

- **#10 REVERTED, and moved to "Verified dead".** The claim it rested on — "the year half is unsafe
  for series but the title-token half transfers" — was wrong. The token check is safe for movies only
  because it judges a release solely when that release carries no parseable year, and a movie release
  almost always carries one; a series episode release carries `S04E01` and no year, so the check
  judges nearly everything. Measured against real shows, five of six returned an EMPTY list: Shōgun
  and Pokémon (`[a-z0-9]+` cannot match `ō`/`é`), Attack on Titan (romaji releases), Money Heist and
  Squid Game (original titles). The test that shipped with it used The Bear, the one case that works.
- **#5** stamps freshness inside the cached entry instead of adding a `Cache` method — one lookup on
  the hot path instead of two, and neither backend changes.
- **#1** fixes the deployment with a named volume, not a tmpfs: a tmpfs survives a restart but not the
  container recreate an image push performs, which is the case `diskcache.go` was written for. The
  "make it louder" half became the `scout_cache_persistent` gauge in #6.
- **#6** ended up behind a bearer token (`SCOUT_METRICS_TOKEN`, route 404s without one) rather than
  relying on aggregation discipline: withholding debrid labels answered the credential-oracle question
  but not the two the counters raise on their own — a cache-miss counter is a timeline of when the
  household is watching, and per-indexer counters disclose per-install configuration.

Audited work list. Every item was checked against the source before it was written down — the
"Verified dead" section at the bottom records the ones that did **not** survive, so they don't get
proposed again.

**Priorities, in order: performance, resource usage, correctness.** Verify each item still holds
before implementing it; if the evidence no longer matches the code, skip it and note why.

Line references are from the audit and may drift — treat them as a starting point, not a contract.

---

## P0 — resource and stability bugs

### 1. The disk cache tier is disabled in the documented deployment

`deps.go:51` defaults `CacheDir` to `os.TempDir()/den-scout-cache`. `DEPLOY.md:29` runs the container
`read_only: true` with no `tmpfs` and no `volumes`. `NewTieredCache`'s `MkdirAll` therefore fails and
`diskcache.go:40-43` silently drops to memory-only after one log line.

Consequence: the 30-day `probeTTL` (`probe_fanout.go:23`) and the tier's stated purpose
(`diskcache.go:19-22` — "the container is redeployed often … a probe costs a debrid resolve each to
rebuild") are not in effect. Every redeploy re-pays a debrid resolve per probed release.

- Give the container writable storage for the cache dir (`tmpfs` sized deliberately, or a named
  volume if persistence across host reboots is wanted) and keep `read_only: true`.
- `DEPLOY.md:42-46` still describes `MemoryCache` as *the* backend; it predates the tier. Fix it.
- Consider making a failed `MkdirAll` louder than one line, since it is silent capability loss.

**Accept:** a container started from the documented compose file writes `.ent` files and logs no
persistence-disabled line.

### 2. No panic recovery around the remote-bytes parsers

`probe_fanout.go:116-143` runs `ProbeTracks` / `ParseHead` inside a bare goroutine under errgroup.
The only `recover()` in the repo is on the request goroutine (`handler.go:103`). A parser panic on a
hostile or corrupt file takes down the **process**, not the probe.

- Recover inside the probe goroutine; log and treat as "not probed", which the pipeline already
  handles (`attributes.go` `withProbe` with a nil `Probe`).

**Accept:** a test that feeds the parser bytes engineered to panic leaves the handler serving.

### 3. Routes answer any HTTP verb

`handler.go:100-171` dispatches on path only — there is no `r.Method` check anywhere in the repo. A
`HEAD` or `POST` to `/<config>/play/<token>` runs a full resolve and can queue an add against the
50/hour budget (`addbudget.go:27`).

Given how much of this codebase exists to prevent accidental adds (`handler.go:522-533`,
`probe_fanout.go:52-64`), a link prefetcher issuing `HEAD` is that same bug with no guard.

- Allow `GET` (and `HEAD` where it is genuinely safe) on read routes; `405` otherwise.
- `HEAD` must not spend an add. Either answer it from the same read-only path `?probe=1` uses, or
  reject it — decide, and record the reason in a comment.
- If `POST /validate` (item 9) lands, it needs method dispatch anyway; build the seam once.

**Accept:** `HEAD`/`POST` on `/play` cannot reach `spendAdd`.

### 4. Unbounded recursion depth in the AVI walker

`probe_avi.go:18-65` — `walk` recurses on every nested `LIST`/`RIFF` with no depth counter. A crafted
1 MiB head permits ~87k frames, inside a 256 MiB container with `GOMEMLIMIT=230MiB` (`Dockerfile:26`).
It terminates and corrupts nothing, but it is the one genuinely unbounded quantity in the three
parsers.

- Add a depth bound (the real containers nest a handful deep; be generous and still finite).
- The MKV/MP4 parsers were audited as correctly bounds-checked (`probe.go:319-322`,
  `probe_mp4.go:158-164`, and the fixed-offset reads at `probe_mp4.go:66-70`, `76-87`, `176-180`,
  `186-194`). Confirm rather than rewrite.

**Accept:** a depth-bomb fixture returns cleanly instead of recursing.

---

## P1 — performance

### 5. Serve stale-while-revalidate, for complete entries only

`handler.go:181` advertises `stale-while-revalidate` and `stale-if-error=86400` to clients, but
nothing implements it server-side: a cache miss makes the caller wait for the full scrape plus debrid
fan-out.

Two facts that make this bigger than it looks — do not skip them:

- The cache **cannot** return an expired entry. `MemoryCache.Get` deletes it (`cache.go:51-53`) and
  `TieredCache.Get` unlinks the file (`diskcache.go:68-71`). The `Cache` interface is `Get`/`Put`
  only (`cache.go:12-15`), so this needs a new method implemented in both backends.
- The build is **not** currently fire-and-forget. `context.WithoutCancel` (`handler.go:212`)
  detaches the context, not the wait; `h.sf.Do` (`handler.go:214`) blocks the request.

Restrict staleness to `complete` entries. Serving a partial list stale contradicts `handler.go:225-239`
directly, and that comment is the record of a real bug.

**Accept:** a warm-but-expired complete entry is served immediately with the rebuild running behind
it, exactly once per key (singleflight), and a partial entry is never served past its own expiry.

### 6. `/metrics`

The only counters are `scrapeFails` (`handler.go:73`) and the add-budget aggregate
(`addbudget.go:136`). Everything else is computed and dropped: `respok[]` (`scrape.go:398`),
`degraded`/`complete` (`handler.go:244-250`), the probe cache hit/miss (`probe_fanout.go:69`).

- `expvar` is stdlib — no new dependency. Prometheus text format by hand is also fine.
- Counters must be cheap: atomics, no per-request allocation, no lock on the hot path.
- **Security:** `/health` is unauthenticated and `addbudget.go:130-135` deliberately withholds
  per-account detail because it is a token-confirmation oracle. On a single-install box, per-store
  labels are per-install labels — the same oracle. Bind metrics to a separate non-public route, or
  keep the same aggregation discipline. Decide and write down which.

**Accept:** indexer error rate, list-cache hit rate, degraded/partial build counts and probe hit rate
are readable without grepping logs.

---

## P2 — correctness and UX

### 7. Soft preferences instead of hard drops

Every knob in `Filters` (`config.go:64-71`) is a `continue` in `rankStreams` (`rank.go:461-502`).
"Prefer 1080p but still show 4K" is not expressible.

The pattern already exists on one axis: the `-100_000` junk sink (`rank.go:224`) is a soft
preference, with `ExcludeCam` layered on as the optional hard drop. Extend that idiom; do not invent
a new one. Filters are also already unknown-tolerant (`rank.go:479`, `489`, `492`) — preserve that.

Keep `rankStreams` a single pass and keep `qualityScore` allocation-free.

### 8. `?debug=1` on `/stream`

No such param exists (only `probe=1`, `handler.go:523`), and `rankStreams` records no drop reasons.
Return per-stream score breakdown and per-filter drop counts.

- **Never** include per-indexer base URLs: MediaFusion's carries an encrypted config minted from the
  debrid token (`scrape.go:285-287`, `indexerconfig.go:194-245`). `RawStream.Source` is just the
  indexer name and is safe.
- The config blob is already echoed in every stream URL in the body (`handler.go:644`), so a debug
  body adds no new credential exposure to a caller who already holds the URL.
- Must cost nothing when off — no accounting on the normal path.
- While here: the `scraped %d → ranked %d` line (`handler.go:382`) fires whenever `ResultCap` trims,
  i.e. on nearly every request, contrary to its own comment. Either narrow the condition or fix the
  comment.

### 9. `POST /validate` for the debrid token

`configure.html:3378` only checks the token is non-empty; a wrong key surfaces at play time. No store
implements an account call today (`stores.go` has only cache-check / add / resolve / status), so this
adds one upstream endpoint per service.

The page seals in-browser (`configure.html:3450`), so validate is the one place the browser hands the
server an **unsealed** token: `no-store`, never logged, no caching, and rate-limited.

### 10. Series mistag filter — the title-token half only

`cinemeta.go:30-31` already rejects series on purpose: "series span years; year-filtering them is
unreliable." Concretely, Cinemeta returns `"2019–2023"`, `firstYear` takes 2019, and `yearMismatch`'s
±1 window (`rank.go:423-436`) would drop a correctly-named `Show.S04.2023.1080p`.

The **year** half stays movies-only. The **title-token** half (`rank.go:474-477`) transfers — it only
fires on year-less titles and needs one significant-token overlap.

### 11. Documentation drift

- `SCOUT_CONFIG_KEY`, `SCOUT_CONFIG_KEYS_PREV`, `SCOUT_MINT_INDEXER_CONFIGS`, `SCOUT_CACHE_DIR`,
  `SCOUT_CINEMETA_URL` all exist (`deps.go:44-57`) but appear in neither `README.md`, `.env.example`,
  nor `DEPLOY.md`. Net effect: the sealed-config feature `docs/SEALED-CONFIG.md:3` marks DONE is
  **off** in the deploy recipe, and the opt-in that sends the debrid token to elfhosted
  (`indexerconfig.go:29-33`) is documented only inside that source file.
- `README.md:45-53` omits `/config-key` and `?probe=1`.
- `README.md:29-31` still names an opaque `configId` as future hardening.
  `docs/SEALED-CONFIG.md:130` lists it as an explicit **non-goal**. The README line is stale —
  correct it, don't carry it.

---

## Verified dead — do not implement

- **The series mistag filter (was #10).** Tried, measured, reverted — see the status note at the top
  and the reasoning recorded at the head of `internal/scout/cinemeta.go`.
  `TestTitleTokens_wouldEmptyRealSeries` pins the four counter-examples so this cannot be re-proposed
  on the strength of an ASCII, English-titled show.

- **Per-indexer circuit breaker.** `torz` is stripped by `validateConfig` (`config.go:147-152`) and
  never reaches `makeScrapers`, so it costs nothing today; NXDOMAIN plus retries is ~1.3s, not 8s.
  And there is no safe value for `unaskableScraper.transient`: `true` forces `complete=false` and puts
  a full scrape plus debrid fan-out on every minute — "a worse answer than the one it was fixing"
  (`scrape.go:437-442`); `false` excuses the indexer from the quorum and reinstates the "this release
  does not exist" regression (`config.go:44-53`, `scrape.go:426-435`).
- **Next-episode prewarm.** Stremio already requests every episode when a season is opened, and
  `scrape.go:200-204` names that burst as the cause of torrentio's shed 502s. Prewarm adds
  speculative traffic to the one burst `hostlimit.go:9-21` was sized around and explicitly "not there
  to tax".
- **Opaque `configId`.** Explicit non-goal (`docs/SEALED-CONFIG.md:130`); sealing already solved the
  plaintext-token problem. Only revocation is left unaddressed, and it costs the statelessness the
  design is built on.
- **`/feedback` playback signal.** Workable but expensive: `rank.go:14` declares the ranker "Pure; no
  I/O", and feeding server state into `qualityScore` ends that and makes the rank tests
  non-deterministic. It would also have to live under `/<config>/` or be an open ranking-poisoning
  endpoint.
