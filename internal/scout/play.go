package scout

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
)

// PlayTarget is what /play resolves: an infohash + either the exact file, or series coords to pick
// from a pack. The debrid token is NOT here — it rides in the config blob on the URL path.
type PlayTarget struct {
	InfoHash string
	FileIdx  *int
	Season   *int
	Episode  *int
}

// playWire fixes field order (h,f,s,e) + omitempty so the encoded token matches the TS encoder.
type playWire struct {
	H string `json:"h"`
	F *int   `json:"f,omitempty"`
	S *int   `json:"s,omitempty"`
	E *int   `json:"e,omitempty"`
}

var infoHashRe = regexp.MustCompile(`^[a-z0-9]{32,40}$`)

func encodePlayToken(t PlayTarget) string {
	b, _ := json.Marshal(playWire{H: t.InfoHash, F: t.FileIdx, S: t.Season, E: t.Episode})
	return b64urlEncode(b)
}

// decodePlayToken decodes + validates a token, or ok=false (→ 400). Infohash must be 32–40 hex/base32.
func decodePlayToken(tok string) (*PlayTarget, bool) {
	data, err := b64urlDecode(tok)
	if err != nil {
		return nil, false
	}
	var raw struct {
		H string   `json:"h"`
		F *float64 `json:"f"`
		S *float64 `json:"s"`
		E *float64 `json:"e"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil, false
	}
	h := strings.ToLower(raw.H)
	if !infoHashRe.MatchString(h) {
		return nil, false
	}
	return &PlayTarget{InfoHash: h, FileIdx: nonNegInt(raw.F), Season: nonNegInt(raw.S), Episode: nonNegInt(raw.E)}, true
}

// nonNegInt returns *int for a non-negative integer JSON number, else nil (dropped, like the TS guard).
func nonNegInt(v *float64) *int {
	if v == nil || !isFinite(*v) || *v < 0 || *v != math.Trunc(*v) {
		return nil
	}
	n := int(*v)
	return &n
}
