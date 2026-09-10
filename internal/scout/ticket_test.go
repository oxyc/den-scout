package scout

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	testIID  = b64urlEncode(bytesSeq(0x00, installIDBytes))
	otherIID = b64urlEncode(bytesSeq(0x10, installIDBytes))
)

// Unterminated, so a test can append iid/ep/scope before closing it.
const ticketCfg = `{"debrid":[{"service":"torbox","token":"tb-secret"}],"indexers":["torrentio"],"cachedOnly":true`

func ticketKeyring(t *testing.T) *sealKeyring {
	t.Helper()
	kr, err := parseSealKeyring(vecPrivB64, "")
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func otherKeyring(t *testing.T, prev string) *sealKeyring {
	t.Helper()
	kr, err := parseSealKeyring(base64.StdEncoding.EncodeToString(bytesSeq(0x40, 32)), prev)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

// streamURLs builds a movie stream list for a config segment and returns its play URLs.
func streamURLs(t *testing.T, h http.Handler, config string) []string {
	t.Helper()
	rr := do(h, "/"+config+"/stream/movie/tt1234567.json", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("stream: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Streams []struct {
			URL string `json:"url"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Streams) == 0 {
		t.Fatal("no streams")
	}
	var out []string
	for _, s := range body.Streams {
		out = append(out, s.URL)
	}
	return out
}

func pathOf(t *testing.T, u string) string {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return p.Path
}

// forgetLogged lets a test see a logLimited line another test (or an earlier -count run) already wrote.
func forgetLogged(key string) {
	limitedLog.mu.Lock()
	delete(limitedLog.last, key)
	limitedLog.mu.Unlock()
}

// A ticket opens to exactly what was minted: the accounts, the target, and the install id and epoch.
func TestPlayTicket_roundTrip(t *testing.T) {
	tk := newTicketKeys(ticketKeyring(t))
	config := &Config{Debrid: []DebridAccount{{ServiceTorBox, "tb"}, {ServiceRealDebrid, "rd"}}, IID: testIID, Epoch: 3}
	target := PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(4), Season: intp(1), Episode: intp(2)}
	gotConfig, gotTarget, err := tk.open(tk.mint(config, target, time.Now().Add(time.Hour)), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotConfig, config) {
		t.Errorf("config: got %+v, want %+v", gotConfig, config)
	}
	if !reflect.DeepEqual(*gotTarget, target) {
		t.Errorf("target: got %+v, want %+v", *gotTarget, target)
	}
}

// Anything that is not a ticket this addon minted, unaltered, is refused.
func TestPlayTicket_refusesWhatDoesNotOpen(t *testing.T) {
	tk := newTicketKeys(ticketKeyring(t))
	config := &Config{Debrid: []DebridAccount{{ServiceTorBox, "tb"}}}
	target := PlayTarget{InfoHash: repeat("a", 40)}
	exp := time.Now().Add(time.Hour)
	good := tk.mint(config, target, exp)
	raw, _ := b64urlDecode(good)
	flipped := func(i int) string {
		b := append([]byte(nil), raw...)
		b[i] ^= 1
		return b64urlEncode(b)
	}
	for name, ticket := range map[string]string{
		"a flipped nonce byte":      flipped(0),
		"a flipped ciphertext byte": flipped(len(raw) / 2),
		"a flipped tag byte":        flipped(len(raw) - 1),
		"truncated":                 good[:len(good)-4],
		"not base64":                "!!!",
		"empty":                     "",
		"over the size cap":         repeat("A", maxConfigBlob+4),
		"another key's ticket":      newTicketKeys(otherKeyring(t, "")).mint(config, target, exp),
		"a legacy play token":       encodePlayToken(target),
	} {
		if _, _, err := tk.open(ticket, time.Now()); !errors.Is(err, errTicketBad) {
			t.Errorf("%s: err = %v, want errTicketBad", name, err)
		}
	}
}

// An expired ticket is 410 on the route, probe or not, so the client fetches the list again.
func TestPlayTicket_anExpiredTicketIsGone(t *testing.T) {
	kr := ticketKeyring(t)
	tk := newTicketKeys(kr)
	config, _ := decodeConfig(kr, sealedConfig(t, kr, ticketCfg+`}`))
	expired := tk.mint(config, PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)}, time.Now().Add(-time.Second))
	if _, _, err := tk.open(expired, time.Now()); !errors.Is(err, errTicketExpired) {
		t.Fatalf("err = %v, want errTicketExpired", err)
	}
	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr }))
	for _, q := range []string{"", "?probe=1", "?fresh=1"} {
		if rr := do(h, "/p/"+expired+q, nil); rr.Code != http.StatusGone {
			t.Errorf("/p/<expired>%s: %d, want 410", q, rr.Code)
		}
	}
}

// A key rotated into CONFIG_KEYS_PREV still opens the tickets it minted, and stops once it is dropped.
func TestPlayTicket_opensWithAPriorKey(t *testing.T) {
	ticket := newTicketKeys(ticketKeyring(t)).mint(&Config{Debrid: []DebridAccount{{ServiceTorBox, "tb"}}},
		PlayTarget{InfoHash: repeat("a", 40)}, time.Now().Add(time.Hour))
	if _, _, err := newTicketKeys(otherKeyring(t, vecPrivB64)).open(ticket, time.Now()); err != nil {
		t.Errorf("a ticket from the prior key: %v", err)
	}
	if _, _, err := newTicketKeys(otherKeyring(t, "")).open(ticket, time.Now()); !errors.Is(err, errTicketBad) {
		t.Errorf("with the prior key dropped: err = %v, want errTicketBad", err)
	}
}

// The largest config the field caps admit still mints a ticket under the size cap open() enforces.
func TestPlayTicket_theLargestConfigFits(t *testing.T) {
	var accounts []DebridAccount
	for i := 0; i < maxDebridAccounts; i++ {
		accounts = append(accounts, DebridAccount{ServicePremiumize, repeat("t", 512)})
	}
	tk := newTicketKeys(ticketKeyring(t))
	ticket := tk.mint(&Config{Debrid: accounts, IID: testIID, Epoch: maxConfigEpoch},
		PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(9999), Season: intp(99), Episode: intp(999)},
		time.Now().Add(time.Hour))
	if _, _, err := tk.open(ticket, time.Now()); err != nil {
		t.Errorf("the largest legitimate ticket (%d bytes) does not open under the %d cap: %v",
			len(ticket), maxConfigBlob, err)
	}
	t.Logf("largest legitimate ticket: %d bytes, against a %d cap", len(ticket), maxConfigBlob)
}

// With a key set, every play URL in a list is a ticket, carries neither the config nor the token, and plays.
func TestPlayTicket_aStreamListNamesTicketsThatPlay(t *testing.T) {
	kr := ticketKeyring(t)
	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr }))
	seg := sealedConfig(t, kr, ticketCfg+`,"iid":"`+testIID+`"}`)
	urls := streamURLs(t, h, seg)
	for _, u := range urls {
		if p := pathOf(t, u); !strings.HasPrefix(p, "/p/") || strings.Count(p, "/") != 2 ||
			strings.Contains(u, seg) || strings.Contains(u, "tb-secret") {
			t.Errorf("play url: %s", u)
		}
	}
	play := pathOf(t, urls[0])
	rr := do(h, play, nil)
	if rr.Code != http.StatusFound || !strings.HasPrefix(rr.Header().Get("location"), "https://cdn.torbox/") {
		t.Fatalf("GET /p/<ticket>: %d location=%q", rr.Code, rr.Header().Get("location"))
	}
	if rr := doMethod(h, http.MethodHead, play, nil); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("HEAD /p/<ticket>: %d, want 405", rr.Code)
	}
}

// Without CONFIG_KEY nothing changes: the list names the legacy route and /p/ does not exist.
func TestPlayTicket_noConfigKeyKeepsTheLegacyURL(t *testing.T) {
	h := NewHandler(testDeps(nil))
	play := pathOf(t, streamURLs(t, h, validBlob)[0])
	if !strings.HasPrefix(play, "/"+validBlob+"/play/") {
		t.Errorf("play url without a key: %s", play)
	}
	if rr := do(h, play, nil); rr.Code != http.StatusFound {
		t.Errorf("legacy play: %d, want 302", rr.Code)
	}
	ticket := newTicketKeys(ticketKeyring(t)).mint(&Config{Debrid: []DebridAccount{{ServiceTorBox, "tb"}}},
		PlayTarget{InfoHash: repeat("a", 40)}, time.Now().Add(time.Hour))
	if rr := do(h, "/p/"+ticket, nil); rr.Code != http.StatusNotFound {
		t.Errorf("/p/ without a key: %d, want 404", rr.Code)
	}
}

// The scoped config is refused before any list is built, so it never mints a ticket.
func TestPlayTicket_theScopedConfigNeverMintsOne(t *testing.T) {
	kr := ticketKeyring(t)
	scoped := sealedConfig(t, kr, ticketCfg+`,"scope":"availability"}`)
	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr }))
	for _, path := range []string{"/" + scoped + "/stream/movie/tt1234567.json",
		"/" + scoped + "/stream/movie/tt1234567.json?debug=1"} {
		if rr := do(h, path, nil); rr.Code != http.StatusForbidden || strings.Contains(rr.Body.String(), "/p/") {
			t.Errorf("scoped stream: %d %s", rr.Code, rr.Body.String())
		}
	}
}

// A revoked install is refused on every route that takes its config, on the tickets it already holds and on
// the legacy play route — each with the answer an undecodable config gets there. Other installs carry on.
func TestRevokedInstall_isRefusedEverywhere(t *testing.T) {
	kr := ticketKeyring(t)
	revoked := sealedConfig(t, kr, ticketCfg+`,"iid":"`+testIID+`"}`)
	live := sealedConfig(t, kr, ticketCfg+`,"iid":"`+otherIID+`"}`)
	// Minted before the revocation, as a client would still hold them.
	before := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr }))
	revokedTicket := pathOf(t, streamURLs(t, before, revoked)[0])
	liveTicket := pathOf(t, streamURLs(t, before, live)[0])

	forgetLogged("install-revoked")
	out := captureLog(t)
	h := NewHandler(testDeps(func(d *Deps) {
		d.SealKeyring = kr
		d.RevokedInstalls = map[string]bool{testIID: true}
	}))
	token := encodePlayToken(PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)})
	for path, want := range map[string]int{
		"/" + revoked + "/manifest.json":               http.StatusBadRequest,
		"/" + revoked + "/stream/movie/tt7654321.json": http.StatusBadRequest,
		"/" + revoked + "/play/" + token:               http.StatusBadRequest,
		revokedTicket:                                  http.StatusBadRequest,
		"/" + live + "/manifest.json":                  http.StatusOK,
		"/" + live + "/play/" + token:                  http.StatusFound,
		liveTicket:                                     http.StatusFound,
	} {
		name := strings.NewReplacer(revoked, "<revoked>", live, "<live>").Replace(path)
		if rr := do(h, path, nil); rr.Code != want {
			t.Errorf("%s: %d, want %d", name, rr.Code, want)
		}
	}
	if rr := postJSON(h, "/"+revoked+"/availability", `{"ids":[]}`); rr.Code != http.StatusBadRequest {
		t.Errorf("revoked availability: %d, want 400", rr.Code)
	}
	logs := out.String()
	if !strings.Contains(logs, "config refused: install revoked (iid "+testIID[:6]+"…)") {
		t.Errorf("no revocation line in:\n%s", logs)
	}
	if strings.Contains(logs, testIID) || strings.Contains(logs, otherIID[:6]) {
		t.Errorf("the log carries more of an install id than it may:\n%s", logs)
	}
}

// A config without an install id cannot be revoked by id, and keeps working while CONFIG_EPOCH is 0.
func TestRevokedInstall_aConfigWithoutAnIDStillWorks(t *testing.T) {
	h := NewHandler(testDeps(func(d *Deps) { d.RevokedInstalls = map[string]bool{testIID: true} }))
	play := pathOf(t, streamURLs(t, h, validBlob)[0])
	if rr := do(h, play, nil); rr.Code != http.StatusFound {
		t.Errorf("legacy play: %d, want 302", rr.Code)
	}
}

// CONFIG_EPOCH refuses every config, and every ticket, minted under an older epoch — an unstamped one
// included — and /config-key hands /configure the epoch to stamp.
func TestConfigEpoch_refusesWhatWasMintedBeforeIt(t *testing.T) {
	kr := ticketKeyring(t)
	old := sealedConfig(t, kr, ticketCfg+`,"ep":1}`)
	current := sealedConfig(t, kr, ticketCfg+`,"ep":2}`)
	unstamped := sealedConfig(t, kr, ticketCfg+`}`)
	before := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr }))
	oldTicket := pathOf(t, streamURLs(t, before, old)[0])

	forgetLogged("install-epoch")
	out := captureLog(t)
	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr; d.ConfigEpoch = 2 }))
	for path, want := range map[string]int{
		"/" + old + "/manifest.json":                   http.StatusBadRequest,
		"/" + old + "/stream/movie/tt7654321.json":     http.StatusBadRequest,
		"/" + unstamped + "/manifest.json":             http.StatusBadRequest,
		oldTicket:                                      http.StatusBadRequest,
		"/" + current + "/manifest.json":               http.StatusOK,
		"/" + current + "/stream/movie/tt7654321.json": http.StatusOK,
	} {
		name := strings.NewReplacer(old, "<ep1>", current, "<ep2>", unstamped, "<unstamped>").Replace(path)
		if rr := do(h, path, nil); rr.Code != want {
			t.Errorf("%s: %d, want %d", name, rr.Code, want)
		}
	}
	// Whichever refused config the map yields first writes the line; logLimited holds back the rest.
	if !regexp.MustCompile(`config refused: install epoch too old \([01] < CONFIG_EPOCH 2\)`).MatchString(out.String()) {
		t.Errorf("no epoch line in:\n%s", out)
	}
	if rr := do(h, "/config-key", nil); !strings.Contains(rr.Body.String(), `"epoch":2`) {
		t.Errorf("/config-key: %s", rr.Body.String())
	}
}

// The install id and epoch are strictly validated; a config carrying a bad one is refused, not trimmed.
func TestConfig_installIDAndEpoch(t *testing.T) {
	const account = `{"debrid":[{"service":"torbox","token":"t"}],`
	c, ok := decodeConfig(nil, blob(account+`"iid":"`+testIID+`","ep":7}`))
	if !ok || c.IID != testIID || c.Epoch != 7 {
		t.Fatalf("got %+v ok=%v", c, ok)
	}
	if c, ok := decodeConfig(nil, blob(account+`"cachedOnly":true}`)); !ok || c.IID != "" || c.Epoch != 0 {
		t.Errorf("absent: got %+v ok=%v", c, ok)
	}
	for name, extra := range map[string]string{
		"a short id":                `"iid":"` + testIID[:21] + `"`,
		"a long id":                 `"iid":"` + testIID + `A"`,
		"a padded id":               `"iid":"` + testIID + `=="`,
		"standard base64":           `"iid":"+/` + testIID[2:] + `"`,
		"a non-canonical id":        `"iid":"` + testIID[:21] + `B"`,
		"an empty id":               `"iid":""`,
		"a numeric id":              `"iid":5`,
		"a negative epoch":          `"ep":-1`,
		"a fractional epoch":        `"ep":1.5`,
		"a string epoch":            `"ep":"1"`,
		"an epoch past the ceiling": `"ep":1e12`,
	} {
		if _, ok := decodeConfig(nil, blob(account+extra+`}`)); ok {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The TV's full config and the browser's separately-minted scoped config differ in scope, install id and
// epoch, and must still share verdicts. A different account must not.
func TestVerdictPrefix_ignoresScopeInstallAndEpoch(t *testing.T) {
	kr := ticketKeyring(t)
	tvSeg := sealedConfig(t, kr, ticketCfg+`,"iid":"`+testIID+`","ep":3}`)
	browserSeg := sealedConfig(t, kr, ticketCfg+`,"scope":"availability","iid":"`+otherIID+`"}`)
	tv, _ := decodeConfig(kr, tvSeg)
	browser, _ := decodeConfig(kr, browserSeg)
	if verdictPrefix(tv) != verdictPrefix(browser) {
		t.Error("the TV's and the browser's configs key their verdicts apart")
	}
	another, _ := decodeConfig(kr, sealedConfig(t, kr, strings.Replace(ticketCfg, "tb-secret", "another", 1)+`}`))
	if verdictPrefix(another) == verdictPrefix(tv) {
		t.Error("the prefix no longer tells accounts apart")
	}

	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr }))
	streamURLs(t, h, tvSeg)
	if got := availabilityOf(t, h, browserSeg, "tt1234567"); got != "available" {
		t.Errorf("the browser's scoped config should read the TV's verdict: %q", got)
	}
}

// The old route stays open until LEGACY_PLAY_UNTIL passes — and only while tickets are on, since without
// them it is the only play route there is.
func TestLegacyPlay_closesAtLegacyPlayUntil(t *testing.T) {
	kr := ticketKeyring(t)
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)})
	for name, c := range map[string]struct {
		kr    *sealKeyring
		until time.Time
		want  int
	}{
		"no deadline":                  {kr, time.Time{}, http.StatusFound},
		"before the deadline":          {kr, time.Now().Add(time.Hour), http.StatusFound},
		"after the deadline":           {kr, time.Now().Add(-time.Second), http.StatusForbidden},
		"a passed deadline, no ticket": {nil, time.Now().Add(-time.Second), http.StatusFound},
	} {
		h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = c.kr; d.LegacyPlayUntil = c.until }))
		if rr := do(h, path, nil); rr.Code != c.want {
			t.Errorf("%s: %d, want %d", name, rr.Code, c.want)
		}
	}
}

func TestLegacyPlay_isLoggedOnceWhileTicketsAreOn(t *testing.T) {
	path := "/" + validBlob + "/play/" + encodePlayToken(PlayTarget{InfoHash: repeat("a", 40), FileIdx: intp(0)})
	out := captureLog(t)
	do(NewHandler(testDeps(nil)), path, nil)
	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = ticketKeyring(t) }))
	do(h, path, nil)
	do(h, path, nil)
	if n := strings.Count(out.String(), "serving a legacy /<config>/play URL"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, out)
	}
}

func TestSettings_revocationAndTickets(t *testing.T) {
	out := captureLog(t)
	env := map[string]string{
		"CONFIG_KEY":           vecPrivB64,
		"REVOKED_INSTALLS":     " " + testIID + ", not-an-id ,," + otherIID,
		"CONFIG_EPOCH":         "3",
		"PLAY_TICKET_TTL_SECS": "7200",
		"LEGACY_PLAY_UNTIL":    "2026-09-17T12:00:00+02:00",
	}
	s := SettingsFromEnv(func(k string) string { return env[k] })
	if !reflect.DeepEqual(s.RevokedInstalls, []string{testIID, otherIID}) || s.ConfigEpoch != 3 ||
		s.PlayTicketTTL != 2*time.Hour || !s.LegacyPlayUntil.Equal(time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("settings: %+v", s)
	}
	if !strings.Contains(out.String(), "skipping a malformed REVOKED_INSTALLS entry") || strings.Contains(out.String(), "not-an-id") {
		t.Errorf("the malformed entry: %s", out)
	}
	if line := StartupSummary(s, true); !strings.Contains(line, " revoked=2 epoch=3 ") {
		t.Errorf("startup line: %s", line)
	}
	d := BuildDeps(s, nil, NewMemoryCache(1<<10))
	if !d.RevokedInstalls[testIID] || !d.RevokedInstalls[otherIID] || len(d.RevokedInstalls) != 2 ||
		d.ConfigEpoch != 3 || d.PlayTicketTTL != 2*time.Hour || !d.LegacyPlayUntil.Equal(s.LegacyPlayUntil) {
		t.Errorf("deps: %+v", d)
	}

	defaults := SettingsFromEnv(func(string) string { return "" })
	if defaults.RevokedInstalls != nil || defaults.ConfigEpoch != 0 || defaults.PlayTicketTTL != defaultPlayTicketTTL ||
		!defaults.LegacyPlayUntil.IsZero() {
		t.Errorf("defaults: %+v", defaults)
	}

	malformed := map[string]string{"CONFIG_KEY": vecPrivB64, "CONFIG_EPOCH": "two", "LEGACY_PLAY_UNTIL": "next tuesday"}
	m := SettingsFromEnv(func(k string) string { return malformed[k] })
	if m.ConfigEpoch != 0 {
		t.Errorf("a malformed CONFIG_EPOCH: %d, want 0", m.ConfigEpoch)
	}
	if m.LegacyPlayUntil.IsZero() || !m.LegacyPlayUntil.Before(time.Now()) {
		t.Errorf("a malformed LEGACY_PLAY_UNTIL must read as passed: %v", m.LegacyPlayUntil)
	}

	noKey := map[string]string{"LEGACY_PLAY_UNTIL": "2020-01-01T00:00:00Z"}
	if u := SettingsFromEnv(func(k string) string { return noKey[k] }).LegacyPlayUntil; !u.IsZero() {
		t.Errorf("LEGACY_PLAY_UNTIL without a key: %v, want unset", u)
	}

	short := map[string]string{"CONFIG_KEY": vecPrivB64, "PLAY_TICKET_TTL_SECS": "900", "LIST_TTL_SECS": "300"}
	BuildDeps(SettingsFromEnv(func(k string) string { return short[k] }), nil, NewMemoryCache(1<<10))
	if !strings.Contains(out.String(), "PLAY_TICKET_TTL_SECS (15m0s) is not above 3×LIST_TTL_SECS (15m0s)") {
		t.Errorf("no warning for a TTL the list outlives:\n%s", out)
	}
}
