package scout

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

// Sealed config-in-URL (see docs/SEALED-CONFIG.md). The config path segment can carry a config JSON
// sealed to the addon's X25519 key instead of plaintext base64, so a BYOK secret is never plaintext in
// the URL and never stored server-side — the addon decrypts per request and forgets it.
//
// Primitive: libsodium crypto_box_seal — anonymous sealed box (X25519 + XSalsa20-Poly1305, a fresh
// ephemeral sender key per message, nonce = blake2b_24(eph_pub ‖ recipient_pub), not transmitted).
// Chosen so it interops byte-for-byte with libsodium.js (/configure) and the Rust `crypto_box` crate
// (den-subtitles). Pure Go, no cgo.

const (
	// sealedVersion is the first byte of a DECODED config segment marking a sealed blob. A legacy
	// plaintext segment decodes to JSON whose first byte is '{' (0x7b), so the two never collide.
	sealedVersion  = 0x01
	x25519KeySize  = 32
	sealEphPubSize = 32
	sealOverhead   = sealEphPubSize + box.Overhead // 32 (eph pub) + 16 (Poly1305 tag)
)

// sealKeypair is a recipient X25519 keypair; pub is derived from priv.
type sealKeypair struct {
	priv [x25519KeySize]byte
	pub  [x25519KeySize]byte
}

// sealKeyring holds the current key first, then any prior keys (tried in order on open) so the addon can
// rotate its keypair without breaking installs whose URL was sealed to an older key.
type sealKeyring struct{ keys []sealKeypair }

func newSealKeypair(priv [x25519KeySize]byte) sealKeypair {
	kp := sealKeypair{priv: priv}
	p, _ := curve25519.X25519(priv[:], curve25519.Basepoint)
	copy(kp.pub[:], p)
	return kp
}

// sealNonce = blake2b_24(ephemeralPub ‖ recipientPub) — the libsodium crypto_box_seal nonce derivation.
func sealNonce(ephPub, recipientPub *[x25519KeySize]byte) [24]byte {
	h, _ := blake2b.New(24, nil)
	_, _ = h.Write(ephPub[:])
	_, _ = h.Write(recipientPub[:])
	var n [24]byte
	copy(n[:], h.Sum(nil))
	return n
}

// seal encrypts plaintext to recipientPub (crypto_box_seal). Output = eph_pub(32) ‖ box(plaintext).
func seal(recipientPub *[x25519KeySize]byte, plaintext []byte) ([]byte, error) {
	ephPub, ephPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	nonce := sealNonce(ephPub, recipientPub)
	out := make([]byte, 0, sealEphPubSize+len(plaintext)+box.Overhead)
	out = append(out, ephPub[:]...)
	return box.Seal(out, plaintext, &nonce, recipientPub, ephPriv), nil
}

// open decrypts a crypto_box_seal ciphertext with the keyring (current key first, then prior keys). Fails
// closed: returns an error and no plaintext if no key opens it (never a partial/empty config).
func (kr *sealKeyring) open(sealed []byte) ([]byte, error) {
	if len(sealed) < sealOverhead {
		return nil, errors.New("sealed: too short")
	}
	var ephPub [x25519KeySize]byte
	copy(ephPub[:], sealed[:sealEphPubSize])
	ct := sealed[sealEphPubSize:]
	for i := range kr.keys {
		k := &kr.keys[i]
		nonce := sealNonce(&ephPub, &k.pub)
		if pt, ok := box.Open(nil, ct, &nonce, &ephPub, &k.priv); ok {
			return pt, nil
		}
	}
	return nil, errors.New("sealed: no key opened it")
}

// currentPubBase64 is the current recipient public key (std base64), served to /configure + /config-key
// so the browser (or den-app) can seal to it. Empty when no keyring is configured.
func (kr *sealKeyring) currentPubBase64() string {
	if kr == nil || len(kr.keys) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(kr.keys[0].pub[:])
}

// parseSealKeyring builds a keyring from a current base64 private key + comma-separated prior keys. Empty
// current → nil keyring (sealed URLs disabled; legacy plaintext still works). A malformed key is an error.
func parseSealKeyring(current, prev string) (*sealKeyring, error) {
	current = strings.TrimSpace(current)
	if current == "" {
		return nil, nil
	}
	kr := &sealKeyring{}
	add := func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		raw, err := decodeKeyB64(s)
		if err != nil || len(raw) != x25519KeySize {
			return errors.New("sealed: bad config key (want base64 32-byte X25519 private key)")
		}
		var priv [x25519KeySize]byte
		copy(priv[:], raw)
		kr.keys = append(kr.keys, newSealKeypair(priv))
		return nil
	}
	if err := add(current); err != nil {
		return nil, err
	}
	for _, p := range strings.Split(prev, ",") {
		if err := add(p); err != nil {
			return nil, err
		}
	}
	return kr, nil
}

// decodeKeyB64 accepts std/url base64 with or without padding.
func decodeKeyB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("bad base64")
}
