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
   filters — rides in the addon path. Build it at `/configure`. It is a bearer credential; the Den
   app stores it in the Keychain and never logs it. (A future hardening swaps it for an opaque
   `configId`; the decode/validate seam in `internal/scout/config.go` + `internal/scout/play.go` is
   the only thing that changes.)
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

## Routes

```
GET /                                   configure page
GET /configure                          configure page
GET /health                             { status: "ok" }
GET /manifest.json                      unconfigured manifest (configurationRequired)
GET /<config>/manifest.json             configured manifest
GET /<config>/stream/<movie|series>/<id>.json   ranked, clean, cached streams
GET /<config>/play/<token>              302 → cached debrid link
```

`<id>` is `tt…` (movie) or `tt…:S:E` (series episode). Scout advertises `idPrefixes: ["tt"]` because
Den bridges TMDB → IMDb before it asks for streams.

## Run

A single static Go binary, no runtime dependencies.

```
go run ./cmd/den-scout            # serves :8080
go test ./...                     # all tests
go test -cover ./internal/...     # coverage (gated ≥90% on the logic package)
go build -o den-scout ./cmd/den-scout
```

Docker (what the homelab runs) — distroless/static, ~15 MB, non-root:

```
docker build -t den-scout .
docker run -p 8080:8080 den-scout
```

### Config (env, no secrets)

den-scout holds **no** debrid secret — the token is per-install, in the addon URL, not the server.
Env only tunes runtime behavior (see `.env.example`): `PORT`, `SCOUT_SCRAPE_TIMEOUT_MS`,
`SCOUT_LIST_TTL_SECONDS`, `SCOUT_CACHE_BYTES` (in-memory list-cache byte budget), `SCOUT_PUBLIC_URL`
(the external origin used to mint `/play` URLs; when set, `X-Forwarded-*`/`Host` are ignored), and
per-indexer base-URL overrides `SCOUT_{TORRENTIO,COMET,MEDIAFUSION,TORZ}_URL` (point MediaFusion at a
base that includes its encrypted-config segment).

## Architecture

The core is `NewHandler(Deps)` (`internal/scout/handler.go`), an `http.Handler` where `Deps` injects
the HTTP client, cache backend, and scraper/store factories — which is also how the tests drive
everything against fixtures with a mock `doer`. `cmd/den-scout` is a thin `main()` that wires the
env-configured deps and calls `ListenAndServe`; `den-scout -healthcheck` probes `/health` and exits
0/1 for the container HEALTHCHECK (no second runtime process).

User-supplied `excludeRegex` runs on Go's stdlib `regexp` (RE2 — linear-time, no catastrophic
backtracking). The internal quality/season patterns that need lookaround use `dlclark/regexp2`;
user input is never routed through it.

## Deploy

Homelab (Docker beside the trailer service, Caddy TLS, fixed egress IP for Real-Debrid): see
`DEPLOY.md`.
