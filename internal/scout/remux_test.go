package scout

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRemuxTicket_preservesScopedAdmissionAndRevocation(t *testing.T) {
	kr := ticketKeyring(t)
	headers := map[string]string{"X-Den-Remux-Key": "remux-secret"}
	for _, iid := range []string{"", testIID} {
		t.Run("iid="+iid, func(t *testing.T) {
			cfg := ticketCfg + `,"scope":"availability"`
			if iid != "" {
				cfg += `,"iid":"` + iid + `"`
			}
			scoped := sealedConfig(t, kr, cfg+`}`)
			deps := testDeps(func(d *Deps) {
				d.SealKeyring, d.RemuxKey, d.RequireInstallID = kr, "remux-secret", true
			})
			h := NewHandler(deps)
			path := "/" + scoped + "/stream/movie/tt1234567.json"
			if rr := do(h, path, nil); rr.Code != http.StatusForbidden {
				t.Fatalf("scoped list without remux key: %d", rr.Code)
			}
			rr := do(h, path, headers)
			var list streamsResponse
			if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &list) != nil || len(list.Streams) == 0 {
				t.Fatalf("authorized list: %d %s", rr.Code, rr.Body.String())
			}
			play := pathOf(t, list.Streams[0].URL)
			if rr := do(h, play, headers); rr.Code != http.StatusFound {
				t.Fatalf("fresh scoped ticket: %d %s", rr.Code, rr.Body.String())
			}
			deps.ConfigEpoch = 1
			if rr := do(NewHandler(deps), play, headers); rr.Code != http.StatusBadRequest {
				t.Errorf("old epoch ticket: %d", rr.Code)
			}
			if iid != "" {
				deps.ConfigEpoch = 0
				deps.RevokedInstalls = map[string]bool{iid: true}
				if rr := do(NewHandler(deps), play, headers); rr.Code != http.StatusBadRequest {
					t.Errorf("revoked scoped ticket: %d", rr.Code)
				}
			}
		})
	}
}

// An availability-scoped config lists and plays only for den-remux (REMUX_KEY in X-Den-Remux-Key), and a
// list built for den-remux is never what a request without the key gets from the cache.
func TestRemuxKey_letsAScopedConfigListAndPlay(t *testing.T) {
	kr := ticketKeyring(t)
	scoped := sealedConfig(t, kr, ticketCfg+`,"scope":"availability"}`)
	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr; d.RemuxKey = "remux-secret" }))
	path := "/" + scoped + "/stream/movie/tt7654321.json"
	withKey := map[string]string{"X-Den-Remux-Key": "remux-secret"}

	if rr := do(h, path, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("scoped, no key: %d, want 403", rr.Code)
	}
	rr := do(h, path, withKey)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "/p/") {
		t.Fatalf("scoped + key: %d %s, want a list of tickets", rr.Code, rr.Body.String())
	}
	if rr := do(h, path, nil); rr.Code != http.StatusForbidden {
		t.Errorf("scoped, no key, after a keyed build: %d, want 403 (the keyed list is cached apart)", rr.Code)
	}
	if rr := do(h, path, map[string]string{"X-Den-Remux-Key": "wrong"}); rr.Code != http.StatusForbidden {
		t.Errorf("scoped, wrong key: %d, want 403", rr.Code)
	}
}

// With REMUX_KEY unset the header means nothing, and a full config never needed it.
func TestRemuxKey_offByDefault(t *testing.T) {
	kr := ticketKeyring(t)
	scoped := sealedConfig(t, kr, ticketCfg+`,"scope":"availability"}`)
	full := sealedConfig(t, kr, ticketCfg+`}`)
	h := NewHandler(testDeps(func(d *Deps) { d.SealKeyring = kr }))
	anyKey := map[string]string{"X-Den-Remux-Key": ""}
	if rr := do(h, "/"+scoped+"/stream/movie/tt7654321.json", anyKey); rr.Code != http.StatusForbidden {
		t.Errorf("scoped with REMUX_KEY unset: %d, want 403", rr.Code)
	}
	if rr := do(h, "/"+full+"/stream/movie/tt7654321.json", nil); rr.Code != http.StatusOK {
		t.Errorf("full config: %d, want 200", rr.Code)
	}
}
