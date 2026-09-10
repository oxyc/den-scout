# GOAL: sealed config-in-URL (get BYOK secrets out of plaintext addon URLs)

**Status:** **den-scout DONE** (`e1d7994`/`dbd257c`) + **den-subtitles DONE** (`1fe426a`) + **den-reel
DONE** (`5e39112`). All three addons seal the config in the browser at `/configure` and resolve sealed
URLs end-to-end; legacy plaintext still works; token never logged; tiny deps only. Interop proven every
direction: **libsodium ↔ Go ↔ Rust** (via the fixed PyNaCl vector) and **JS (browser bundle) → Go and →
Rust**. Activate per deployment by setting `CONFIG_KEY` in each addon's env (the same name in all three;
base64 32-byte X25519 private key; unset = legacy, still works). den-reel additionally moved its former
server-side `TMDB_KEY` into the sealed config (BYOK), keeping the env key only as a migration fallback.
**Remaining:** P3 (optional) den-app minter — the web `/configure` already mints, so this is a nicety;
for den-reel it's what lets the app inject its Keychain TMDB key so the server env key can be dropped.
**Reference impl:** den-scout · **Also:** den-subtitles, den-reel, den-app

## Progress
- [x] **P0** crypto module `seal.go` (crypto_box_seal over nacl/box+blake2b) + the libsodium interop
      gate (`TestSealInteropVector` opens a real PyNaCl ciphertext + matches the derived pubkey) +
      round-trip / fail-closed / rotation tests.
- [x] **P1 (server)** keyring from env (`CONFIG_KEY`/`CONFIG_KEYS_PREV`), version-byte
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

> **P3 is optional and NOT required for the feature.** Every addon's web `/configure` already mints
> sealed URLs, and all three addons (den-scout, den-subtitles, den-reel) are user-installed by pasting a
> manifest URL. So the paste-from-`/configure` flow *is* the sealing — no in-app Swift minter is needed.
> P3 would only save re-typing the TMDB key into den-reel's `/configure` (the app already holds it in the
> Keychain). Skip it unless that convenience is wanted.

## Operator runbook — activating sealing (what to do per deployment)

Sealing is **off until an env key is set**; with no key each addon keeps serving legacy plaintext URLs, so
these steps are safe to do in any order and never break an existing install.

**1. Generate a keypair-seed per addon** (a base64 32-byte X25519 private key):
```
head -c 32 /dev/urandom | base64
```
Do this once per addon. **Back each key up** in the homelab secret store — losing it makes every URL
sealed to it un-decryptable (installs would have to be re-configured).

**2. Set it in the homelab env** for that service, then let it redeploy. Every addon (den-scout,
den-subtitles, den-reel) reads the key as `CONFIG_KEY` and prior keys as `CONFIG_KEYS_PREV`, each from its
own env file, so the keys stay distinct per addon.

**3. Ship each repo** — a `v*` tag builds the image to GHCR, and the box's `den-update` (the den repo's
`deploy/README.md`) verifies and pins it. Setting the env key first means sealing is live the moment the
new image boots.

**4. Re-configure each install** (turns an existing plaintext URL into a sealed one):
open the addon's `/configure`, enter the key(s), copy the new URL (it now shows **🔒 Sealed**), and
replace the old URL in the app under **Settings → Plugins**. For **den-reel** the key to enter is your
**TMDB API key** (plus an optional KinoCheck key).

**5. Drop the den-reel server keys** once its install is re-added sealed: remove `TMDB_KEY` /
`KINOCHECK_KEY` from the den-reel env. They exist only as a migration fallback; after step 4 the key
rides in the (sealed) URL and the server holds nothing.

**Rotation:** move the current key into `CONFIG_KEYS_PREV`, set a fresh `CONFIG_KEY`, redeploy.
Old URLs keep decrypting via the prev list; keep the rotated-out key in `_PREV` until every install is
re-sealed and past the `/config-key` 1-hour cache (see the risk note below), then drop it.
**Rotation is not revocation.** Every URL sealed to the old key keeps working for as long as that key is in
`_PREV`. Dropping it kills every install at once, so it is no way to cut off one leaked URL. For that,
see below.

## Revoking installs (den-scout)

Every config `/configure` builds carries two more sealed fields:
- `iid`, an install id: 16 random bytes from `crypto.getRandomValues`, unpadded base64url, 22 characters.
  Each link gets a fresh one.
- `ep`, the epoch it was minted under. `/config-key` serves the addon's current `CONFIG_EPOCH` as `epoch`.

Both are optional, and they are accepted in plaintext configs too. A config with a malformed `iid` or `ep`
is refused.

- **One install:** add its id to `REVOKED_INSTALLS=<iid>,<iid>,…` and restart. Its config segment is
  refused on every route, and so is every play ticket it already holds. The log line names the reason
  (`install revoked`) and the first six characters of the id. The id of an install that is not revoked is
  never logged.
- **Every install:** raise `CONFIG_EPOCH` and restart. Every config with a lower `ep`, including one with
  no `ep` (minted before this existed), is refused with `install epoch too old`, and so are its tickets.
  Then rebuild the installs you keep at `/configure`. `/config-key` is cached for five minutes, so wait
  that long after the restart before rebuilding, or the new link may still carry the old epoch.

A refused config gets the same answer as an undecodable one (`400`). A config without an `iid` can't be
revoked individually; only the epoch reaches it.

What this does not cover:
- **A debrid token that was ever in plaintext** — in a legacy URL, a screenshot or a log — can only be
  killed at the debrid. Regenerate the token in the debrid account's settings. Revoking the install
  stops *this addon* using the URL; the token itself still works against the debrid's API.
- **There is no tool to read an `iid` back out of a sealed URL yet.** Only the addon's key can open it.
  Until there is one, `CONFIG_EPOCH` is the practical lever for a URL whose id nobody recorded.
- **Without `CONFIG_KEY`, `/config-key` answers 404**, so `/configure` can't learn the epoch. Raising
  `CONFIG_EPOCH` on an addon with no key refuses the plaintext links built afterwards as well.
- **A warm stream-list hit is served without opening the config.** For a few minutes after a revocation,
  that install can still read a list it had cached. Every play URL in that list is refused.

## Play tickets (den-scout)

With `CONFIG_KEY` set, a stream list names its play URLs `/p/<ticket>` instead of
`/<config>/play/<token>`, so a stream URL is no longer the whole install credential.
- `ticket = base64url(nonce(24) ‖ XChaCha20-Poly1305(K_play, payload))`, where
  `K_play = HKDF-SHA256(CONFIG_KEY, info "den-scout/play/v1")`.
- The payload is only what one resolve reads: the debrid accounts, the infohash, the file index,
  season/episode, the expiry, `iid` and `ep`. There are no filters, indexers or scope.
- It is stateless. Keys derived from `CONFIG_KEYS_PREV` still open tickets minted before a rotation.
- A scoped (`availability`) config never lists streams, so it never mints a ticket.

**Lifetime.** `PLAY_TICKET_TTL_SECS`, default 86400, is measured from when the list is built. It has to
outlast two things:
- **The list:** a client can first use it up to 3×`LIST_TTL_SECS` after it was built. The server serves a
  complete list as fresh for `LIST_TTL_SECS`, and the device then keeps it `max-age` plus
  `stale-while-revalidate`, one TTL each.
- **The viewing session:** the player may ask the same URL again.

The addon warns at startup when the TTL is not above 3×`LIST_TTL_SECS`. A list the device holds only
through `stale-if-error` (up to a day) can outlive its tickets. That is acceptable, because it only
happens while `/stream` is erroring. An expired ticket answers **410**, meaning: fetch the list again.

**Rollout.** The legacy `/<config>/play` route keeps working, because clients hold cached stream lists
(for up to a day on `stale-if-error`). The route also honours `REVOKED_INSTALLS` and `CONFIG_EPOCH`, and
the first time it serves a request while tickets are on, the addon logs that once. When no client should
hold a pre-ticket list any more, set `LEGACY_PLAY_UNTIL` to an RFC 3339 moment, e.g. a day after the
deploy. Once that passes, the legacy route answers **403**. Unset means it stays open. A value that does
not parse counts as already passed, the same rule den-reel applies to `PLAY_SIGNING_GRACE_UNTIL`. The
variable has no effect without `CONFIG_KEY`, because then there is no other play route.

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
  - `CONFIG_KEY` = current X25519 private key (base64, 32 bytes).
  - `CONFIG_KEYS_PREV` = comma-separated prior private keys (may be empty).
  - Public key = derived from `CONFIG_KEY`; served to `/configure` (embedded) and exposed at
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
- [ ] Load keyring from env (`CONFIG_KEY` + `CONFIG_KEYS_PREV`); fail fast if malformed.
- [ ] `decodeConfig(seg)`: base64url-decode → version-branch → sealed (`Open` over keyring) or legacy JSON.
      Route it everywhere the config path segment is parsed (manifest/stream/play/configure).
- [ ] `GET /config-key` → base64 current pubkey. `/configure` page embeds the pubkey + does the sealing in
      the browser and renders the single `/<sealed>/manifest.json` URL.
- [ ] Never log the segment or the decrypted secret (audit the log sites).
- AC: a URL minted by the updated `/configure` resolves streams end-to-end; a **legacy** plaintext URL
      still resolves (back-compat); the token never appears in logs; `docker-publish` image size delta is
      negligible (report the before/after MB).

### Phase 2 — den-subtitles (Rust) mirror
- [ ] `crypto_box` sealedbox `Open` over a keyring (`CONFIG_KEY` + `CONFIG_KEYS_PREV`); `/config-key`;
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
- **Key loss → every install breaks.** Back up `CONFIG_KEY`; keep it in the homelab secret store,
  not just the container env. Keyring lets you rotate without breakage.
- **JS/Go/Rust crypto mismatch.** The fixed libsodium.js **test vector** in Phase 0 is the gate — do it
  first; every impl must open it.
- **Fail-open on decrypt error.** Must fail **closed** (no config → 400/empty), never fall back to serving
  with an empty/partial config.
- **`/config-key` is cached `public, max-age=3600`.** After a rotation a browser may seal to the *previous*
  pubkey for up to an hour — harmless **as long as the rotated-out key stays in `CONFIG_KEYS_PREV`** (the
  keyring tries prior keys), which the rotation procedure already requires. Don't drop a key from `_PREV`
  until well past the cache TTL.

## Not this / see also
- `FOLLOWUP.md` #13 (debrid file selection) — **done** (commit `cfe8f1f`), separate from this.
- Off-LAN HTTPS + single-replica cache — orthogonal ops items; this goal adds no state so it doesn't
  change them.
