# Deploying den-scout on the homelab

den-scout runs as a Docker service **beside the trailer service** (`github.com/oxyc/cameras`
→ `homelab/docker`). The image is built + pushed to `ghcr.io/oxyc/den-scout` by this repo's
`docker-publish` workflow; the homelab just pulls a tag.

**LAN-only for now (like the trailer service).** Den's `NSAllowsLocalNetworking` exempts LAN hosts
from the https-only addon check, so Den reaches Scout directly at `http://<lxc-ip>:8080` — no cert
needed. Egress is the box's single fixed WAN IP, which is what Real-Debrid pins tokens to, so RD
resolve works. The Caddy `scout.<domain>` route (below) is the eventual **public-https** path and is
harmless to leave configured meanwhile; you only need it when Den runs off-LAN.

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

# --- LAN (now) — <lxc-ip> is the Docker LXC's IP, e.g. 192.168.86.193 ---
curl -fsS http://<lxc-ip>:8080/health                        # {"status":"ok"}
curl -fsS http://<lxc-ip>:8080/configure | grep "Configure Den Scout"

# build <config> at http://<lxc-ip>:8080/configure (pick your debrid, paste the token) then:
curl -fsS "http://<lxc-ip>:8080/<config>/stream/movie/tt0111161.json" | jq '.streams[0]'
curl -sS -o /dev/null -w "%{http_code} %{redirect_url}\n" "http://<lxc-ip>:8080/<config>/play/<token>"

# --- Public https (later, via Caddy) — same paths under https://scout.<domain> ---
```

Then in Den: **Settings → Streaming source** → enter `http://<lxc-ip>:8080` as the server, pick your
debrid, paste the token (or paste the whole `http://<lxc-ip>:8080/<config>/manifest.json` built at
`/configure`). A title should resolve to cached streams and play through the 302 with no CAM/TS/screener
rows. In DEBUG you can instead drop that manifest URL into Den's gitignored `App/Config/dev-addons.json`,
right next to the trailer service's `http://<lxc-ip>:8092/manifest.json`.
