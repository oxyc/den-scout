package scout

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strings"
)

// etagHex is an 8-hex FNV-1a-32 of s — matches the TS ETag format ("[0-9a-f]{8}").
func etagHex(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// keyHash is a collision-resistant digest for cache keys (audit #7 — the key gates cross-config
// data, so a 32-bit FNV was unsafe). 128 bits of SHA-256 is ample.
func keyHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// b64urlDecode decodes a base64url string, padded or not (lenient, like the TS decoder).
func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func b64urlEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
