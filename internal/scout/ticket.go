package scout

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// Play tickets (docs/SEALED-CONFIG.md, "Play tickets"). With CONFIG_KEY set, a stream list names its play
// URLs `/p/<ticket>` instead of `/<config>/play/<token>`. The legacy URL carries the whole install
// credential, so anyone holding one stream URL could resolve any title on the account, forever. A ticket
// carries only what one resolve needs, for one release, until it expires.
//
//	ticket = base64url(nonce(24) ‖ XChaCha20-Poly1305(K_play, payload))
//	K_play = HKDF-SHA256(ikm = the CONFIG_KEY private key, info = "den-scout/play/v1")
//
// Stateless like the sealed config: nothing is stored, and every key in CONFIG_KEYS_PREV derives a key that
// still opens the tickets minted before a rotation. The HKDF info separates K_play from the X25519 use of
// the same secret.

const (
	playTicketInfo = "den-scout/play/v1"
	// The first path segment of a ticketed play URL. No config segment can be "p" — it does not decode.
	ticketRoute = "p"
	// How long a ticket stays good (PLAY_TICKET_TTL_SECS). It must outlast the stream list that carries it,
	// which a client may first use up to 3×LIST_TTL_SECS after the list was built (a hit served just before
	// the server's freshness ends, then max-age plus stale-while-revalidate on the device), and then the
	// longest viewing session, during which the player may ask the URL again. A day covers both with room.
	defaultPlayTicketTTL = 24 * time.Hour
)

var (
	errTicketBad     = errors.New("ticket: does not open")
	errTicketExpired = errors.New("ticket: expired")
)

// ticketKeys holds one AEAD per keyring key, current first: tickets are minted with the first and opened
// with any.
type ticketKeys struct{ aeads []cipher.AEAD }

// newTicketKeys derives the play keys from the sealing keyring. nil when there is no keyring, which is what
// turns tickets off.
func newTicketKeys(kr *sealKeyring) *ticketKeys {
	if kr == nil || len(kr.keys) == 0 {
		return nil
	}
	t := &ticketKeys{}
	for i := range kr.keys {
		// Neither call can fail here: HKDF only refuses an output longer than 255 hashes, and NewX only a key
		// that is not KeySize bytes.
		k, _ := hkdf.Key(sha256.New, kr.keys[i].priv[:], nil, playTicketInfo, chacha20poly1305.KeySize)
		aead, _ := chacha20poly1305.NewX(k)
		t.aeads = append(t.aeads, aead)
	}
	return t
}

// ticketWire is a ticket's payload: the target a legacy token names, the accounts the resolve uses, the
// expiry, and the install id and epoch revocation checks. Nothing else of the config goes in — no filters,
// indexers. Scope records the admission provenance: availability configs may legitimately lack an
// install id, including when den-remux is authorized to mint tickets for them.
type ticketWire struct {
	playWire             // h, f, s, e
	D        [][2]string `json:"d"` // [service, token] per account, in config order
	X        int64       `json:"x"` // expiry, unix seconds
	I        string      `json:"i,omitempty"`
	P        int         `json:"p,omitempty"`
	Scope    string      `json:"scope,omitempty"`
}

// mint seals a ticket for one release of this config, good until exp.
func (t *ticketKeys) mint(config *Config, target PlayTarget, exp time.Time) string {
	w := ticketWire{
		playWire: playWire{H: target.InfoHash, F: target.FileIdx, S: target.Season, E: target.Episode},
		X:        exp.Unix(), I: config.IID, P: config.Epoch, Scope: config.Scope,
	}
	for _, d := range config.Debrid {
		w.D = append(w.D, [2]string{string(d.Service), d.Token})
	}
	pt, _ := json.Marshal(w)
	nonce := make([]byte, chacha20poly1305.NonceSizeX, chacha20poly1305.NonceSizeX+len(pt)+chacha20poly1305.Overhead)
	_, _ = rand.Read(nonce) // crypto/rand.Read does not return an error; it crashes the process instead
	return b64urlEncode(t.aeads[0].Seal(nonce, nonce, pt, nil))
}

// open authenticates a ticket and returns the config and target it carries. The config holds only what the
// play route reads — the accounts, plus the install id and epoch for admitInstall. errTicketExpired is a
// ticket that opened and has lapsed; anything else that fails is errTicketBad.
func (t *ticketKeys) open(ticket string, now time.Time) (*Config, *PlayTarget, error) {
	// The same ceiling as a config segment: a ticket for the largest config the field caps admit fits under
	// it (TestPlayTicket_theLargestConfigFits), and nothing larger is worth decoding.
	if len(ticket) > maxConfigBlob {
		return nil, nil, errTicketBad
	}
	raw, err := b64urlDecode(ticket)
	if err != nil || len(raw) < chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead {
		return nil, nil, errTicketBad
	}
	nonce, ct := raw[:chacha20poly1305.NonceSizeX], raw[chacha20poly1305.NonceSizeX:]
	var pt []byte
	for _, aead := range t.aeads {
		if pt, err = aead.Open(nil, nonce, ct, nil); err == nil {
			break
		}
	}
	if err != nil {
		return nil, nil, errTicketBad
	}
	var w ticketWire
	if json.Unmarshal(pt, &w) != nil {
		return nil, nil, errTicketBad
	}
	if now.Unix() >= w.X {
		return nil, nil, errTicketExpired
	}
	// Only this process can have written a ticket that authenticates, so these checks are not the defence.
	// They hold the resolve to the same shapes the legacy route validates, whatever a future minter writes.
	h := strings.ToLower(w.H)
	if !infoHashRe.MatchString(h) {
		return nil, nil, errTicketBad
	}
	if w.Scope != "" && w.Scope != scopeAvailability {
		return nil, nil, errTicketBad
	}
	config := &Config{IID: w.I, Epoch: w.P, Scope: w.Scope}
	for _, d := range w.D {
		if !isDebridService(d[0]) || d[1] == "" {
			return nil, nil, errTicketBad
		}
		config.Debrid = append(config.Debrid, DebridAccount{Service: DebridService(d[0]), Token: d[1]})
	}
	if len(config.Debrid) == 0 {
		return nil, nil, errTicketBad
	}
	return config, &PlayTarget{InfoHash: h, FileIdx: w.F, Season: w.S, Episode: w.E}, nil
}
