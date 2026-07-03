# GOAL: sealed config-in-URL (get BYOK secrets out of plaintext addon URLs)

**Status:** **den-scout DONE** (`e1d7994`/`dbd257c`) + **den-subtitles DONE** (`1fe426a`) + **den-reel
DONE** (`5e39112`). All three addons seal the config in the browser at `/configure` and resolve sealed
URLs end-to-end; legacy plaintext still works; token never logged; tiny deps only. Interop proven every
direction: **libsodium ↔ Go ↔ Rust** (via the fixed PyNaCl vector) and **JS (browser bundle) → Go and →
Rust**. Activate per deployment by setting `SCOUT_CONFIG_KEY` / `SUBS_CONFIG_KEY` / `REEL_CONFIG_KEY`
(base64 32-byte X25519 private key; unset = legacy, still works). den-reel additionally moved its former
server-side `TMDB_KEY` into the sealed config (BYOK), keeping the env key only as a migration fallback.
**Remaining:** P3 (optional) den-app minter — the web `/configure` already mints, so this is a nicety;
for den-reel it's what lets the app inject its Keychain TMDB key so the server env key can be dropped.
**Reference impl:** den-scout · **Also:** den-subtitles, den-reel, den-app

## Progress
- [x] **P0** crypto module `seal.go` (crypto_box_seal over nacl/box+blake2b) + the libsodium interop
      gate (`TestSealInteropVector` opens a real PyNaCl ciphertext + matches the derived pubkey) +
      round-trip / fail-closed / rotation tests.
- [x] **P1 (server)** keyring from env (`SCOUT_CONFIG_KEY`/`SCOUT_CONFIG_KEYS_PREV`), version-byte
      decode branch in `decodeConfig` (sealed `0x01` vs legacy JSON), fail-closed, `GET /config-key`,
      never-log. `TestDecodeConfigSealed` + `TestRoutesSealedConfig` prove a sealed URL resolves
      manifest+streams end-to-end and legacy still resolves.
- [x] **P1 (minter)** `/configure` fetches `/config-key` and seals the config to it (tweetnacl `nacl.box`
      + blakejs blake2b, esbuild IIFE ~23 KB, inlined — no CDN); falls back to plaintext when no key.
      `TestSealJSInteropVector` opens a real browser-bundle-minted segment in Go (JS→Go gate).
- [x] **P2** den-subtitles mirror (Rust `crypto_box` `seal` feature). `seal.rs` keyring + version-branch
      `decode`, `/config-key`, `/configure` seals in-browser (same bundle), fail-closed, never-log.
      Interop gated both ways: opens the same PyNaCl libsodium vector (libsodium→Rust) and a
      browser-bundle-minted segment (JS→Rust). 27 tests + clippy clean. (den-subtitles commit `1fe426a`.)
- [ ] **P3 (optional)** den-app in-app sealing — an *alternative* minter (the web `/configure` on both
      addons already mints sealed URLs, so this isn't required). When the app builds an addon URL: fetch
      the addon's `GET /config-key`, and seal `{secrets, prefs}` to it with crypto_box_seal
      (`seg = base64url(0x01 ‖ eph_pub ‖ box)`, nonce = `blake2b_24(eph_pub‖recipient_pub)`). CryptoKit
      has Curve25519 (X25519) but not XSalsa20-Poly1305/blake2b, so use a swift crypto_box/sodium package
      or port the ~40-line seal. Verify with the same vector approach (mint in Swift → open in a Go/Rust
      test). Lives in the den-app repo (the "Add Den plugins" flow) — coordinate with the app agent.
- [ ] **P4** rollout / migration.



## Goal (one line)
Keep the single paste-one-URL Stremio install flow, but make the config bytes in the URL **ciphertext
sealed to the addon's key** — so a BYOK secret (Real-Debrid token, OpenSubtitles key, LLM key) is never
plaintext in the URL, never stored server-side, and readable only by the addon process at the instant it
resolves. Stateless: no database, no container bloat.

## Why (problem)
Today the whole config — **including the debrid token** — rides base64url in the addon URL path
(`/<base64url(JSON)>/manifest.json`). The URL *is* the credential: anyone who sees it (a log, the Caddy
proxy, a screenshot, browser history, a shared file) can extract and spend the token. Same issue in
den-subtitles (OpenSubtitles + LLM keys).

## Design (the shape)
The config stays in the URL path (so the install is still one URL). We only change the **bytes**:
- `/configure` holds the addon's **X25519 public key**. The browser encrypts `{secrets, prefs}` to that
  key (libsodium **crypto_box_seal** — anonymous sealed box) and shows one URL: `/<sealed>/manifest.json`.
- Every request, the addon **decrypts with its private key**, uses the secret, **stores nothing**.
- A stock Stremio client sees an opaque path segment either way — protocol-compatible, UX unchanged.

Because the browser seals **before** forming the URL, even the `/configure` request never sees plaintext.

### Honest limit (not zero-knowledge)
Server-side resolution requires the addon to decrypt-to-use, so this is **"end-to-end from the browser to
the addon *process*"**, not "the server can never read it." True zero-knowledge needs on-device
resolution (VortX's model) = the App-Store surface the neutral-addon pivot exists to avoid. Out of scope.

## Crypto (pinned, for cross-language interop + small deps)
- **Primitive:** libsodium `crypto_box_seal` / `crypto_box_seal_open` (X25519 + XSalsa20-Poly1305,
  ephemeral sender key per message, nonce = `blake2b(eph_pub ‖ recipient_pub)` — not transmitted).
  Chosen because it interops identically across:
  - **JS** (`/configure`): a ~23 KB esbuild IIFE of tweetnacl (`nacl.box`) + blakejs (`blake2b`) that
    reimplements `crypto_box_seal` — inlined into the page, not container weight. (Interop with libsodium
    is proven by the JS→Go and JS→Rust vectors, so the hand-assembled seal is byte-identical.)
  - **Go** (den-scout): ~30 lines over `golang.org/x/crypto/nacl/box` + `.../blake2b` (pure Go, **no cgo**).
  - **Rust** (den-subtitles): `crypto_box` crate `sealedbox` (pure Rust).
- **URL segment wire format** (base64url, no padding):
  - `seg = base64url( version_byte ‖ payload )`
  - `version 0x01` (sealed): payload = `eph_pub(32) ‖ ciphertext(plaintext+16 MAC)`.
  - **Legacy `v0`:** the existing plaintext blob is `base64url(JSON)` whose first decoded byte is `{` (0x7b).
    Discriminate: base64url-decode the segment; if `[0]==0x01` → sealed → decrypt; else parse as legacy JSON.
    (Never emit a version byte for legacy; only read it.)
- **Keyring / rotation:** the addon holds a **current** private key + **prior** keys; decrypt tries each in
  order. So rotating the keypair doesn't break existing installs (old URLs decrypt with an old key).
  - `SCOUT_CONFIG_KEY` = current X25519 private key (base64, 32 bytes).
  - `SCOUT_CONFIG_KEYS_PREV` = comma-separated prior private keys (may be empty).
  - Public key = derived from `SCOUT_CONFIG_KEY`; served to `/configure` (embedded) and exposed at
    `GET /config-key` (base64 pubkey) so den-app can seal in-app later.
- **Payload plaintext** = the same validated config JSON as today (`SingularityConfig`-equivalent), i.e. the
  secret lives *inside* the sealed payload; nothing is split out.

## Non-goals
- No database / persistent store (the whole point vs an opaque `configId`).
- No change to the resolve/rank logic — only where the config is parsed.
- Not zero-knowledge (see honest limit).
- Don't drop legacy plaintext support until installs have migrated.

## Phases & acceptance criteria

### Phase 0 — crypto module (den-scout, reference)
- [ ] `internal/scout/seal.go`: `Seal(pub, plaintext) []byte`, `Open(keyring, seg) ([]byte, error)`
      implementing crypto_box_seal semantics over `nacl/box` + `blake2b`.
- [ ] Round-trip + wrong-key + tampered-ciphertext tests, **plus a fixed test vector from
      libsodium.js** (encrypt in JS, paste the bytes into a Go test) to prove cross-language interop.
- AC: `go test` round-trips; the libsodium.js vector opens; a tampered blob fails closed.

### Phase 1 — den-scout wiring (reference)
- [ ] Load keyring from env (`SCOUT_CONFIG_KEY` + `SCOUT_CONFIG_KEYS_PREV`); fail fast if malformed.
- [ ] `decodeConfig(seg)`: base64url-decode → version-branch → sealed (`Open` over keyring) or legacy JSON.
      Route it everywhere the config path segment is parsed (manifest/stream/play/configure).
- [ ] `GET /config-key` → base64 current pubkey. `/configure` page embeds the pubkey + does the sealing in
      the browser and renders the single `/<sealed>/manifest.json` URL.
- [ ] Never log the segment or the decrypted secret (audit the log sites).
- AC: a URL minted by the updated `/configure` resolves streams end-to-end; a **legacy** plaintext URL
      still resolves (back-compat); the token never appears in logs; `docker-publish` image size delta is
      negligible (report the before/after MB).

### Phase 2 — den-subtitles (Rust) mirror
- [ ] `crypto_box` sealedbox `Open` over a keyring (`SUBS_CONFIG_KEY` + `_PREV`); `/config-key`;
      `/configure` seals in-browser (share the same JS).
- [ ] Version-branch decode in `userconfig.rs`; legacy plaintext still accepted; never log.
- AC: OpenSubtitles + LLM keys resolve from a sealed URL; legacy still works; no plaintext in logs.

### Phase 3 — den-app (Swift), optional but nice
- [ ] Fetch the addon's `/config-key`, seal `{secret, prefs}` in-app (a Swift crypto_box_seal:
      CryptoKit `Curve25519` + a small XSalsa20-Poly1305, or a vetted SwiftPM sealed-box), so the app can
      generate a private URL without the web `/configure`.
- AC: the app can add a plugin by entering a token in-app and producing a sealed URL itself.

### Phase 4 — rollout
- [ ] Both addons accept sealed + legacy indefinitely; docs updated; a one-tap "re-configure privately"
      path for existing installs (later).
- AC: no existing install breaks; new installs are sealed by default.

## Risks & mitigations
- **Key loss → every install breaks.** Back up `SCOUT_CONFIG_KEY`; keep it in the homelab secret store,
  not just the container env. Keyring lets you rotate without breakage.
- **JS/Go/Rust crypto mismatch.** The fixed libsodium.js **test vector** in Phase 0 is the gate — do it
  first; every impl must open it.
- **Fail-open on decrypt error.** Must fail **closed** (no config → 400/empty), never fall back to serving
  with an empty/partial config.
- **`/config-key` is cached `public, max-age=3600`.** After a rotation a browser may seal to the *previous*
  pubkey for up to an hour — harmless **as long as the rotated-out key stays in `*_CONFIG_KEYS_PREV`** (the
  keyring tries prior keys), which the rotation procedure already requires. Don't drop a key from `_PREV`
  until well past the cache TTL.

## Not this / see also
- `FOLLOWUP.md` #13 (debrid file selection) — **done** (commit `cfe8f1f`), separate from this.
- Off-LAN HTTPS + single-replica cache — orthogonal ops items; this goal adds no state so it doesn't
  change them.
