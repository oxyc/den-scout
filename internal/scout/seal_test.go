package scout

import (
	"encoding/base64"
	"testing"
)

// A fixed libsodium crypto_box_seal vector, generated with PyNaCl (SealedBox) — see
// docs/SEALED-CONFIG.md. This is the cross-language interop GATE: the Go `open` MUST decrypt a
// ciphertext produced by a real libsodium binding (the same one /configure uses via libsodium.js).
const (
	vecPrivB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=" // recipient X25519 private key (seed 00..1f)
	vecPubB64  = "j0DFrbaPJWJK5bIU6nZ6bslNgp09e14a0bpvPiE4KF8=" // its public key
	vecCTB64   = "YVIKGV1+YCwzCoPC0WNrcle9bYR0iWhBDAsy2ylVhGmDWneqpfb/Oug5izEfwx2Q9j8UDKM2XF/u+Q9Sg1jqQeMb5RNWpLZk+81tVixiI5qFpc/zNGAwfTJSMVj+B48nCbgqRk0rqQzqniVKm1d85g=="
	vecPlain   = `{"debrid":[{"service":"realdebrid","token":"SEALED-VECTOR-OK"}]}`
)

func TestSealInteropVector(t *testing.T) {
	kr, err := parseSealKeyring(vecPrivB64, "")
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	// Our derived public key must match libsodium's.
	if got := kr.currentPubBase64(); got != vecPubB64 {
		t.Fatalf("derived pub %q, libsodium says %q", got, vecPubB64)
	}
	ct, _ := base64.StdEncoding.DecodeString(vecCTB64)
	pt, err := kr.open(ct)
	if err != nil {
		t.Fatalf("open libsodium vector: %v", err)
	}
	if string(pt) != vecPlain {
		t.Fatalf("plaintext = %q, want %q", pt, vecPlain)
	}
}

func TestSealRoundTrip(t *testing.T) {
	kr, err := parseSealKeyring(vecPrivB64, "")
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte(`{"hello":"world"}`)
	ct, err := seal(&kr.keys[0].pub, msg)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := kr.open(ct)
	if err != nil || string(pt) != string(msg) {
		t.Fatalf("round-trip: pt=%q err=%v", pt, err)
	}
}

func TestSealFailsClosed(t *testing.T) {
	kr, _ := parseSealKeyring(vecPrivB64, "")
	ct, _ := seal(&kr.keys[0].pub, []byte("secret"))

	// Tampered ciphertext → error, no plaintext (never fall through to a partial config).
	bad := append([]byte(nil), ct...)
	bad[len(bad)-1] ^= 0xff
	if pt, err := kr.open(bad); err == nil || pt != nil {
		t.Errorf("tampered blob opened: pt=%q err=%v", pt, err)
	}
	// Too short.
	if _, err := kr.open([]byte("nope")); err == nil {
		t.Error("short blob should fail")
	}
	// Wrong key (different recipient) → cannot open.
	other, _ := parseSealKeyring(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if _, err := other.open(ct); err == nil {
		t.Error("wrong-key open should fail")
	}
}

func TestSealKeyringRotation(t *testing.T) {
	// A blob sealed to an OLD key must still open once that key is demoted to the "prev" list.
	oldKR, _ := parseSealKeyring(vecPrivB64, "")
	ct, _ := seal(&oldKR.keys[0].pub, []byte("rotate-me"))

	newCurrent := base64.StdEncoding.EncodeToString(bytesSeq(0x40, 32)) // a different current key
	kr, err := parseSealKeyring(newCurrent, vecPrivB64)                 // old key kept as prev
	if err != nil {
		t.Fatal(err)
	}
	pt, err := kr.open(ct)
	if err != nil || string(pt) != "rotate-me" {
		t.Fatalf("rotation open: pt=%q err=%v", pt, err)
	}
	if len(kr.keys) != 2 {
		t.Fatalf("keyring size = %d, want 2 (current + prev)", len(kr.keys))
	}
}

func TestDecodeConfigSealed(t *testing.T) {
	kr, _ := parseSealKeyring(vecPrivB64, "")
	cfgJSON := []byte(`{"debrid":[{"service":"realdebrid","token":"rd-secret"}],"resultCap":20}`)
	sealed, _ := seal(&kr.keys[0].pub, cfgJSON)
	seg := b64urlEncode(append([]byte{sealedVersion}, sealed...)) // the URL path segment

	// Sealed segment decrypts + validates to the same config a plaintext one would.
	c, ok := decodeConfig(kr, seg)
	if !ok || len(c.Debrid) != 1 || c.Debrid[0].Token != "rd-secret" {
		t.Fatalf("sealed decode: %+v ok=%v", c, ok)
	}
	// Fail CLOSED: sealed segment but no keyring configured.
	if _, ok := decodeConfig(nil, seg); ok {
		t.Error("sealed blob with no keyring must fail closed")
	}
	// Fail CLOSED: sealed to a different recipient key.
	other, _ := parseSealKeyring(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if _, ok := decodeConfig(other, seg); ok {
		t.Error("sealed to a different key must fail closed")
	}
	// Legacy plaintext still decodes even when a keyring is present (back-compat).
	if _, ok := decodeConfig(kr, blob(`{"debrid":[{"service":"torbox","token":"t"}]}`)); !ok {
		t.Error("legacy plaintext must still decode with a keyring set")
	}
}

func bytesSeq(start byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}
