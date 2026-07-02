# Deploying den-scout on the homelab

den-scout runs as a Docker service **beside the trailer service** (`github.com/oxyc/cameras`
→ `homelab/docker`), fronted by the same Caddy for TLS + a stable domain. Unlike the trailer service
(LAN-only), Scout is reachable over https at a stable name so the Den app's `https`-only addon check
passes, and it egresses from a **fixed IP** so Real-Debrid (which pins tokens to an IP) keeps working.

The image is built + pushed to `ghcr.io/oxyc/den-scout` by this repo's `docker-publish` workflow;
the homelab just pulls a tag.

## 1. Compose service (homelab `docker/compose.yml`)

Added under `profiles: ["scout"]` so a plain `docker compose up` doesn't start it:

```yaml
  den-scout:
    image: ghcr.io/oxyc/den-scout:${DEN_SCOUT_VERSION:-latest}
    container_name: den-scout
    restart: unless-stopped
    profiles: ["scout"]
    environment:
      PORT: "8080"
      SCOUT_SCRAPE_TIMEOUT_MS: "${SCOUT_SCRAPE_TIMEOUT_MS:-8000}"
      SCOUT_LIST_TTL_SECONDS: "${SCOUT_LIST_TTL_SECONDS:-300}"
      SCOUT_MEDIAFUSION_URL: "${SCOUT_MEDIAFUSION_URL:-}"   # base incl. its encrypted-config segment
    read_only: true
    security_opt: ["no-new-privileges:true"]
    cap_drop: ["ALL"]
    mem_limit: 256m
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }   # log rotation
```

- **No secrets in env** — the debrid token is per-install, encoded in the addon URL, never here.
- The image already runs as the non-root `node` user; `read_only` + `no-new-privileges` + `cap_drop`
  harden it further (it only needs outbound https + the listen socket).
- **Cache backend**: the in-container `MemoryCache` (TTL, per-process) is the default and is right
  for a single replica — stream lists live for minutes. If you ever scale to >1 replica, add a
  `redis` service and back the cache with it (the `Cache` seam in `src/cache.ts` is a drop-in); a
  SQLite volume is the alternative. Not needed for the homelab's single container.

## 2. Caddy route (homelab `docker/caddy/Caddyfile`)

```
  @scout host scout.{$CADDY_LOCAL_DOMAIN}
  handle @scout {
    reverse_proxy den-scout:8080
  }
```

Caddy already issues a real wildcard cert for `*.{$CADDY_LOCAL_DOMAIN}` via Cloudflare DNS-01, so
`https://scout.<domain>` gets a valid cert with no ATS exception in Den. Caddy forwards `Host` +
`X-Forwarded-Proto`, and `handleScout` honors them to build correct `https://scout.<domain>/play/…`
URLs.

### Fixed egress IP (Real-Debrid)

RD binds an unrestricted link to the requesting IP. The homelab has a single static WAN IP, so all
Scout egress already comes from one address — nothing extra to configure. If Scout is ever moved
behind a NAT with a changing IP, put it behind the same fixed-IP egress the rest of the box uses
(document the WAN IP in the RD account's allowlist if RD tightens this). TorBox and Premiumize are
not IP-bound.

## 3. `.env` additions (homelab `docker/.env.example`)

```
DEN_SCOUT_VERSION=latest                # ghcr.io/oxyc/den-scout (profile: scout)
# SCOUT_MEDIAFUSION_URL=                 # optional: self-hosted MediaFusion base incl. config segment
```

## 4. Bring it up + smoke test

```
docker compose --profile scout pull
docker compose --profile scout up -d

# health (from the box)
curl -fsS https://scout.<domain>/health                 # {"status":"ok"}

# configure page loads
curl -fsS https://scout.<domain>/configure | grep "Configure Den Scout"

# a configured stream request returns cached streams (build <config> at /configure with a real token)
curl -fsS "https://scout.<domain>/<config>/stream/movie/tt0111161.json" | jq '.streams[0]'

# /play 302s to a debrid link (grab a url from the stream response above)
curl -sS -o /dev/null -w "%{http_code} %{redirect_url}\n" "https://scout.<domain>/<config>/play/<token>"
```

Then in Den: **Settings → Streaming source**, paste `https://scout.<domain>/<config>/manifest.json`
(or build it in the app's Streaming-source screen). A title should resolve to cached streams and play
through the 302 with no CAM/TS/screener rows.
