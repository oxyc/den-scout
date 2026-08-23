package scout

import (
	"context"
	"errors"
	"testing"
)

// With more than one debrid, WHICH service resolves a release decides whether it plays now or downloads
// first. CacheCheck already learns that and the union threw it away, so the fixed store order could send
// a viewer to download a file sitting cached on their other account.

type namedStore struct {
	svc      DebridService
	cached   map[string]bool
	link     string
	resolves *[]DebridService
}

func (n namedStore) Service() DebridService { return n.svc }
func (n namedStore) CacheCheck(context.Context, []string) (map[string]bool, error) {
	if n.cached == nil {
		return nil, errors.New("no cache api")
	}
	return n.cached, nil
}
func (n namedStore) Resolve(context.Context, ResolveTarget) (string, error) {
	*n.resolves = append(*n.resolves, n.svc)
	if n.link == "" {
		return "", &DeadLinkError{string(n.svc) + " has nothing"}
	}
	return n.link, nil
}
func (n namedStore) Status(context.Context, ResolveTarget) (StoreStatus, bool) {
	return StoreStatus{}, false
}

func TestCachedBy_reportsEachServiceHoldingIt(t *testing.T) {
	var calls []DebridService
	pool := &StorePool{stores: []Store{
		namedStore{svc: ServiceTorBox, cached: map[string]bool{H: false}, resolves: &calls},
		namedStore{svc: ServicePremiumize, cached: map[string]bool{H: true}, resolves: &calls},
	}}
	got := pool.CachedBy(t.Context(), []string{H})
	if len(got[H]) != 1 || got[H][0] != ServicePremiumize {
		t.Fatalf("CachedBy = %v, want [premiumize]", got[H])
	}
	// A store whose cache API errored contributes nothing rather than a false "not cached".
	broken := &StorePool{stores: []Store{namedStore{svc: ServiceRealDebrid, resolves: &calls}}}
	if len(broken.CachedBy(t.Context(), []string{H})) != 0 {
		t.Error("an errored cache check must not claim knowledge")
	}
}

func TestResolvePreferring_triesTheHolderFirst(t *testing.T) {
	var calls []DebridService
	pool := &StorePool{stores: []Store{
		namedStore{svc: ServiceTorBox, resolves: &calls}, // configured first, has nothing
		namedStore{svc: ServicePremiumize, link: "https://pm/x", resolves: &calls},
	}}
	link, err := pool.ResolvePreferring(t.Context(), ResolveTarget{InfoHash: H},
		[]DebridService{ServicePremiumize})
	if err != nil || link != "https://pm/x" {
		t.Fatalf("link = %q, err = %v", link, err)
	}
	if len(calls) != 1 || calls[0] != ServicePremiumize {
		t.Errorf("asked %v — the holder must be tried first, and alone when it succeeds", calls)
	}
}

// The preference is an ordering, not a filter: if the holder fails, the others must still be tried.
func TestResolvePreferring_fallsThroughWhenTheHolderFails(t *testing.T) {
	var calls []DebridService
	pool := &StorePool{stores: []Store{
		namedStore{svc: ServiceTorBox, link: "https://tb/x", resolves: &calls},
		namedStore{svc: ServicePremiumize, resolves: &calls}, // "holds" it but can't resolve
	}}
	link, err := pool.ResolvePreferring(t.Context(), ResolveTarget{InfoHash: H},
		[]DebridService{ServicePremiumize})
	if err != nil || link != "https://tb/x" {
		t.Fatalf("link = %q, err = %v", link, err)
	}
	if len(calls) != 2 || calls[0] != ServicePremiumize || calls[1] != ServiceTorBox {
		t.Errorf("asked %v, want premiumize then torbox", calls)
	}
}

// No preference must behave exactly as before — configured order, deterministically.
func TestResolvePreferring_withoutPreferenceKeepsConfiguredOrder(t *testing.T) {
	var calls []DebridService
	pool := &StorePool{stores: []Store{
		namedStore{svc: ServiceTorBox, resolves: &calls},
		namedStore{svc: ServicePremiumize, link: "https://pm/x", resolves: &calls},
	}}
	if _, err := pool.Resolve(t.Context(), ResolveTarget{InfoHash: H}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != ServiceTorBox {
		t.Errorf("asked %v, want the configured order", calls)
	}
}

// A preference naming a service that isn't configured must not drop the ones that are.
func TestResolvePreferring_ignoresUnknownServices(t *testing.T) {
	var calls []DebridService
	pool := &StorePool{stores: []Store{namedStore{svc: ServiceTorBox, link: "https://tb/x", resolves: &calls}}}
	link, err := pool.ResolvePreferring(t.Context(), ResolveTarget{InfoHash: H},
		[]DebridService{ServicePremiumize})
	if err != nil || link != "https://tb/x" {
		t.Fatalf("link = %q, err = %v", link, err)
	}
}
