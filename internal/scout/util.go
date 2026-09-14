package scout

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strings"
)

// etagHex is a 16-hex FNV-1a-64 of s. 64 bits, because the ETag is the only thing standing between a
// revalidating client and a 304 for a body it does not hold; at 32 bits a collision between two versions of
// a list is within reach of an ordinary cache's lifetime.
func etagHex(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%016x", h.Sum64())
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
