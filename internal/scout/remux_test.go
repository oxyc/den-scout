package scout

import (
	"net/http"
	"strings"
	"testing"
)

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
