# Deploying den-scout

den-scout runs on the Den host as one of the backend stack's Podman Quadlet units, deployed from the
private `den` repo's `deploy/` directory. **The stack mechanics — provisioning, env rendering, releases,
the `den-update` verify-and-pin updater, rollback — are documented once, in that repo's
`deploy/README.md`.** This file covers only what is specific to scout.

The image is built and pushed to `ghcr.io/oxyc/den-scout` by this repo's `docker-publish` workflow, on a
`v*` tag only; the box picks a release up on its next `den-update` run.

## The unit

`deploy/quadlet/den-scout.container` in the den repo:

- **Host port 8080**, LAN http. Den's `NSAllowsLocalNetworking` exempts LAN hosts from the https-only
  addon check, so Den reaches scout directly at `http://<host>:8080` — no certificate involved.
- **Read-only rootfs, all capabilities dropped, no-new-privileges.** The distroless/static image runs as
  uid 65532 with no shell or libc; it needs only outbound https and its listen socket.
- `--memory=256m`. The image sets `GOMEMLIMIT=230MiB` to match (Go's GC is not cgroup-aware); change
  both together.
- Env comes from an env file rendered from the deploy `.env` (`render-env.sh`). **No secrets belong
  there** — the debrid token is per-install, carried in the addon URL. See `README.md` for every variable.
- No HEALTHCHECK. `den-update` probes `/health` and `/manifest.json` over HTTP when there is a new image,
  and not otherwise, so an idle box stays idle.
- On SIGTERM scout drains in-flight requests for up to 8s; the unit's stop timeout leaves headroom.

## The cache directory is not optional

The cache is a `TieredCache` (`internal/scout/diskcache.go`): the byte-bounded in-memory `MemoryCache`
(TTL + LRU, sized by `SCOUT_CACHE_BYTES`, default 48 MiB) in front of a durable disk tier at `CACHE_DIR`,
which the image sets to `/cache`. The unit bind-mounts the host's `/var/lib/den/scout-cache` there, and
that directory must be **owned by uid 65532** (`provision-podman.sh` does it).

With the rootfs read-only, a missing or unwritable mount means `MkdirAll` fails, persistence disables
itself after one log line, and memory keeps serving — so nothing looks wrong, but every redeploy re-pays a
debrid resolve per probed release. Track probes are cached for 30 days precisely so it doesn't. A tmpfs
would survive a restart but not the container recreate an update performs, which is the case the tier
exists for. `scout_cache_persistent` on `/metrics` reads 1 when the tier is writing.

The tier has a real ceiling: expired entries are swept hourly, and the sweep also enforces a 256 MiB byte
budget, evicting oldest-first (a burst of writes brings the sweep forward). A household's installs land in
the low tens of MB. The budget exists because expiry alone is a schedule, not a bound: nothing re-reads a
stream list, so without it an entry occupies the disk until its TTL passes however large the store grows.

A second replica would need a shared cache — the `Cache` interface in `internal/scout/cache.go` is the
seam — but one container is all the box runs.

## Fixed egress IP (Real-Debrid)

Real-Debrid binds an unrestricted link to the IP that requested it, so resolve and playback must leave from
the same address. The box has a single static WAN IP, so all scout egress already does — nothing to
configure. If scout ever moves behind a NAT with a changing IP, give it a fixed-IP egress first (and
allowlist that IP in the RD account if RD tightens this). TorBox and Premiumize are not IP-bound.

## Sealed config keys

Without `CONFIG_KEY` the addon URL carries the debrid token in plain text — anyone who sees the link (a
log, a screenshot, browser history) can read it. `/config-key` 404s until it is set, and `/configure` then
says "⚠︎ Not sealed" on the link it builds. Generate a key with:

```
head -c 32 /dev/urandom | base64
```

Back it up: losing it makes every URL sealed to it undecryptable. To rotate, move the current key into
`CONFIG_KEYS_PREV` (comma-separated) and set a fresh `CONFIG_KEY`; keep the old one there until every
install is re-sealed. The runbook is in `docs/SEALED-CONFIG.md`.

`METRICS_TOKEN` likewise gates `/metrics`; unset, the route 404s. `PUBLIC_BASE_URL` pins the origin used
in `/play` URLs, and is only needed if scout ever sits behind a proxy that rewrites the host.

## Smoke test

```
curl -fsS http://<host>:8080/health                             # {"status":"ok"}
curl -fsS http://<host>:8080/manifest.json | jq .version        # the release you expect
curl -fsS http://<host>:8080/configure | grep "Configure Den Scout"
curl -fsS -H "Authorization: Bearer $METRICS_TOKEN" http://<host>:8080/metrics | grep -E "build_info|cache_persistent"

# build <config> at http://<host>:8080/configure (pick your debrid, paste the token), then:
curl -fsS "http://<host>:8080/<config>/stream/movie/tt0111161.json" | jq '.streams[0]'
curl -sS -o /dev/null -w "%{http_code} %{redirect_url}\n" "http://<host>:8080/<config>/play/<token>"
```

On the host, confirm once after a fresh provision that the disk tier is writing — no "persistence
disabled" line in the journal, and `.ent` files appearing under the cache dir once a title has been opened:

```
journalctl -u den-scout | grep "persistence disabled"     # expect no output
ls /var/lib/den/scout-cache | head
```

Then in Den: **Settings → Streaming source** → enter `http://<host>:8080` as the server, pick your debrid,
paste the token (or paste the whole `http://<host>:8080/<config>/manifest.json` built at `/configure`). A
title should resolve to cached streams and play through the 302 with no CAM/TS/screener rows. In DEBUG you
can instead drop that manifest URL into Den's gitignored `App/Config/dev-addons.json`.
