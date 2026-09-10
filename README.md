# den-scout

A self-hosted **Stremio stream addon** for [Den](https://github.com/oxyc/den). It turns "find me a
stream" into a short list of **clean, cached, ranked** `https` streams that just play — the Den app
never sees a torrent, a CAM rip, or a debrid token.

```
Den (Apple TV) ──/<config>/stream/movie/tt…──►  scout   scrape → dedupe → cache-check → rank
                                                   │
                ◄──── { streams:[ {url:/play/…} ] }┘   clean titles ("4K • HDR • 18 GB")
Den (AVPlayer) ──GET /<config>/play/<token>──►  scout   store.resolve → 302 → cached debrid link
```

Everything happens **off-device**: Scout scrapes the torrent indexers, cache-checks your debrid
account, ranks with a precise junk filter (a port of VortX's `junkClass`), and hands back only
cached streams whose `url` is a lazy `/play` proxy. The debrid token is used in exactly one place —
resolving `/play` server-side — and never leaves the server.

## Why it exists

Den's on-device ranker had to parse hostile scene titles on an A15 and still leaked the occasional
CAM row; and putting a debrid token on the Apple TV is a non-starter. Scout moves scrape + rank +
resolve to a server you control, so the app just renders what comes back.

## How it works

1. **Config in the URL** (Torrentio-style): a base64url blob — debrid service + token, indexers,
   filters — rides in the addon path. Build it at `/configure`, which **seals** it in the browser to
   the server's X25519 public key (served at `/config-key`) when one is configured, so the link
   carries ciphertext rather than your token; legacy plaintext blobs still resolve. See
   [`docs/SEALED-CONFIG.md`](docs/SEALED-CONFIG.md). Sealed or not, the link remains a bearer
   credential — whoever holds it can spend the account's quota — so the Den app keeps it in the
   Keychain and never logs it.
2. **Scrape** the configured indexers concurrently (Torrentio, Comet, MediaFusion, Torz), each under
   its own timeout — a slow indexer never blocks the rest. Dedupe by infohash.
3. **Cache-check** the debrid store(s). TorBox has a real batched cache API and is the default;
   Real-Debrid and Premiumize are also supported (Premiumize also has a real cache API; RD has no
   usable one, so a hash cached on TorBox still wins for an RD+TorBox user).
4. **Rank** (`internal/scout/rank.go`) — sink CAM/TS/screeners far below any legit source, cached above uncached,
   then resolution/source/HDR/audio/size. Return the top N as clean `https` streams.
5. **`/play`** decodes the opaque token → `store.resolve` → **302** to the freshly-minted cached
   link. Dead link → 404 so the client falls through to the next stream. Season packs map `tt…:S:E`
   to the exact file index.

### Architecture

The core is `NewHandler(Deps)` (`internal/scout/handler.go`), an `http.Handler` where `Deps` injects
the HTTP client, cache backend, and scraper/store factories — which is also how the tests drive
everything against fixtures with a mock `doer`. `cmd/den-scout` is a thin `main()` that wires the
env-configured deps and serves until SIGTERM, then drains in-flight requests for up to 8s.

The cache is a `TieredCache` (`internal/scout/diskcache.go`): a byte-bounded in-memory LRU in front of a
durable disk tier at `CACHE_DIR`. The disk tier holds stream lists and 30-day track probes — a probe
costs a debrid resolve to rebuild — and has a real ceiling: expired entries are swept hourly, and the
sweep also enforces a 256 MiB budget, evicting oldest-first. A second replica would need a shared cache;
the `Cache` interface in `internal/scout/cache.go` is the seam.

User-supplied `excludeRegex` runs on Go's stdlib `regexp` (RE2 — linear-time, no catastrophic
backtracking). The internal quality/season patterns that need lookaround use `dlclark/regexp2`;
user input is never routed through it.

## Routes

```
GET  /                                   configure page
GET  /configure                          configure page
GET  /health                             always 200: { status: "ok" } | { status: "degraded", reason, detail }
GET  /metrics                            Prometheus text (bearer token; 404 unless METRICS_TOKEN is set)
GET  /config-key                         { key } — X25519 public key for sealing (404 if unset)
POST /validate                           { service, token } → { valid, reason } — check a debrid key
GET  /manifest.json                      unconfigured manifest (configurationRequired)
GET  /<config>/manifest.json             configured manifest
GET  /<config>/stream/<movie|series>/<id>.json   ranked, clean, cached streams
GET  /<config>/stream/…?debug=1          the same list plus per-filter drop counts and scores
GET  /<config>/play/<token>              302 → cached debrid link
GET  /<config>/play/<token>?probe=1      "can this play yet?" — reports, starts nothing
```

Everything is `GET` (and `HEAD`) except `/validate`. `/play` is `GET` only: resolving is what *adds* an
uncached release, so a prefetcher's `HEAD` is refused rather than answered.

Every response carries `Access-Control-Allow-Origin: *`, and `OPTIONS` on any path is a `204` CORS
preflight. An unknown path — and `/metrics` without its token — is `404` with `{"error":"not_found"}`.

A stream list that is served but cannot be trusted carries `X-Den-Degraded` — `indexers` when no indexer
answered, `cache-check` when the debrid could not be asked about a release in it — and is `no-store`, so
the app can say "sources temporarily unavailable" instead of "nothing found".

Stream and play responses carry `Server-Timing`: a built list names `scrape`, `cache-check` and (when
probing is on) `probe`; a list served from cache says `cache;desc=hit` or `cache;desc=stale`; a play names
`resolve` once one has run; every one ends with `total`. Durations are milliseconds.

`<id>` is `tt…` (movie) or `tt…:S:E` (series episode). Scout advertises `idPrefixes: ["tt"]` because
Den bridges TMDB → IMDb before it asks for streams.

## Configuration

den-scout holds **no** debrid secret — the token is per-install, in the addon URL, not the server. Env
only tunes runtime behaviour; every variable is listed, commented, in `.env.example`.

| Variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | listen port |
| `PUBLIC_BASE_URL` | — | external origin for `/play` URLs; when set, `X-Forwarded-*`/`Host` are ignored |
| `SCRAPE_TIMEOUT_MS` | `8000` | per-indexer scrape timeout |
| `LIST_TTL_SECS` | `300` | how long a ranked stream list stays fresh |
| `MEMORY_CACHE_BYTES` | `50331648` (48 MiB) | in-memory cache byte budget |
| `CACHE_DIR` | `/cache` in the image (else `$TMPDIR/den-scout-cache`) | durable cache tier; **must be writable** or persistence disables itself after one log line |
| `CINEMETA_URL` | `https://v3-cinemeta.strem.io` | metadata source for the mistagged-torrent filter |
| `METRICS_TOKEN` | — | bearer token for `/metrics`; unset = the route 404s (the counters reveal when the install is being watched) |
| `CONFIG_KEY` | — | base64 X25519 private key enabling **sealed** config URLs; unset = plaintext only |
| `CONFIG_KEYS_PREV` | — | prior keys, comma-separated, so a rotation doesn't break live installs |
| `MINT_INDEXER_CONFIGS` | `false` | let scout build comet/mediafusion config segments from the debrid token. **This sends the token to those hosts**; an explicit `COMET_URL`/`MEDIAFUSION_URL` always wins |
| `TORRENTIO_URL` | `https://torrentio.strem.fun` | indexer base-URL override |
| `COMET_URL` | `https://comet.elfhosted.com` | indexer base-URL override, including its per-install config segment |
| `MEDIAFUSION_URL` | `https://mediafusion.elfhosted.com` | indexer base-URL override, including its encrypted-config segment (it 401s without one) |
| `TORZ_URL` | `https://torz.strem.fun` | indexer base-URL override |

The image also sets `GOMEMLIMIT=230MiB`, read by the Go runtime, to sit under the 256 MiB container cap
(Go's GC is not cgroup-aware); change both together.

## Run

A single static Go binary, no runtime dependencies.

```
go run ./cmd/den-scout            # serves :8080
go test ./...                     # all tests
go test -cover ./internal/...     # coverage (gated ≥90% on the logic package)
go build -o den-scout ./cmd/den-scout
```

Docker — distroless/static, ~15 MB, non-root:

```
docker build -t den-scout .
docker run -p 8080:8080 den-scout
```

## Deploy

The box runs scout as a Podman Quadlet unit from the private `den` repo's `deploy/`: host port 8080 (LAN
http), a 256 MiB memory cap, read-only rootfs, uid 65532, and the cache bind-mounted from
`/var/lib/den/scout-cache` (owned by 65532) at `/cache`. `den-update` pulls a new image — published to
`ghcr.io/oxyc/den-scout` on a `v*` tag — and verifies `/health` and `/manifest.json` before pinning it.
Provisioning, env rendering, releases and rollback are in that repo's `deploy/README.md`. No secrets
belong in the env file; the debrid token rides in each install's URL.

If the cache mount is missing or not writable, memory keeps serving and nothing looks wrong, but every
redeploy re-pays a debrid resolve per probed release. `scout_cache_persistent` on `/metrics` reads 1
when the disk tier is writing.

**Fixed egress IP (Real-Debrid).** RD binds an unrestricted link to the IP that requested it, so resolve
and playback must leave from the same address. The box has one static WAN IP, so this holds with
nothing to configure; if scout ever moves behind a NAT with a changing IP, give it a fixed-IP egress
first. TorBox and Premiumize are not IP-bound.

**Sealed config keys.** Without `CONFIG_KEY` the addon URL carries the debrid token in plain text, and
`/configure` marks the link "Not sealed". Generate a key, and back it up — losing it makes every URL
sealed to it undecryptable:

```
head -c 32 /dev/urandom | base64
```

To rotate, move the current key into `CONFIG_KEYS_PREV` and set a fresh `CONFIG_KEY`; keep the old one
until every install is re-sealed. The runbook is in [`docs/SEALED-CONFIG.md`](docs/SEALED-CONFIG.md).

**Smoke test.**

```
curl -fsS http://<host>:8080/health                             # {"status":"ok"}
curl -fsS http://<host>:8080/manifest.json | jq .version        # the release you expect
curl -fsS http://<host>:8080/configure | grep "<title>Den Scout"
curl -fsS -H "Authorization: Bearer $METRICS_TOKEN" http://<host>:8080/metrics | grep -E "build_info|cache_persistent"

# build <config> at http://<host>:8080/configure (pick your debrid, paste the token), then:
curl -fsS "http://<host>:8080/<config>/stream/movie/tt0111161.json" | jq '.streams[0]'
curl -sS -o /dev/null -w "%{http_code} %{redirect_url}\n" "http://<host>:8080/<config>/play/<token>"
```

Then in Den: **Settings → Streaming source** → enter `http://<host>:8080`, pick your debrid and paste
the token (or paste the whole `http://<host>:8080/<config>/manifest.json` built at `/configure`).
