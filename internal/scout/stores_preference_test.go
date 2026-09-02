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
	refuse   bool // the store faulted or throttled US, rather than the release being dead
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
	if n.refuse {
		return "", &StoreUnavailableError{n.svc, "throttled"}
	}
	if n.link == "" {
		return "", &DeadLinkError{string(n.svc) + " has nothing"}
	}
	return n.link, nil
}
func (n namedStore) Status(context.Context, ResolveTarget) (StoreStatus, bool) {
	return StoreStatus{}, false
}

func TestCacheTruth_reportsEachServiceHoldingIt(t *testing.T) {
	var calls []DebridService
	pool := &StorePool{stores: []Store{
		namedStore{svc: ServiceTorBox, cached: map[string]bool{H: false}, resolves: &calls},
		namedStore{svc: ServicePremiumize, cached: map[string]bool{H: true}, resolves: &calls},
	}}
	truth, _ := pool.CacheCheck(t.Context(), []string{H})
	if got := truth.HeldBy(H); len(got) != 1 || got[0] != ServicePremiumize {
		t.Fatalf("HeldBy = %v, want [premiumize]", got)
	}
	// Cached is the union — SOMEONE holds it — while HeldBy says who. Conflating the two sent the probe
	// path to resolve a Premiumize-only release against TorBox, which adds it.
	if !truth.Cached(H) {
		t.Error("a release one store holds is cached")
	}

	// A store whose cache API errored contributes nothing rather than a false "not cached".
	broken := &StorePool{stores: []Store{namedStore{svc: ServiceTorBox, resolves: &calls}}}
	brokenTruth, _ := broken.CacheCheck(t.Context(), []string{H})
	if brokenTruth.Cached(H) || brokenTruth.Known(H) {
		t.Error("an errored cache check must not claim knowledge")
	}
}

// A hash a store could not check is UNKNOWN, not "not cached". They were the same value, and the
// difference decides whether a cached-only list silently drops a playable release, and whether pressing
// play pays an add for a torrent the account already holds.
func TestCacheTruth_separatesUnknownFromNotCached(t *testing.T) {
	var calls []DebridService
	// The store answered for one hash and not the other — what a partially-failed batch looks like.
	other := repeat("b", 40)
	pool := &StorePool{stores: []Store{
		namedStore{svc: ServiceTorBox, cached: map[string]bool{H: false}, resolves: &calls},
	}}
	truth, ok := pool.CacheCheck(t.Context(), []string{H, other})
	if !ok {
		t.Fatal("a store that answered at all is not a total outage")
	}
	if !truth.Known(H) || truth.Cached(H) {
		t.Errorf("H was answered for, and the answer was no: known=%v cached=%v", truth.Known(H), truth.Cached(H))
	}
	if truth.Known(other) {
		t.Error("a hash the store never reported on must read as unknown, not as not-cached")
	}
}

// The probe path must never reach a store that would ADD the release.
func TestResolveCachedOnly_neverTouchesANonHolder(t *testing.T) {
	var calls []DebridService
	pool := &StorePool{stores: []Store{
		namedStore{svc: ServiceTorBox, resolves: &calls}, // configured first, does NOT hold it
		namedStore{svc: ServicePremiumize, link: "https://pm/x", resolves: &calls},
	}}
	link, err := pool.ResolveCachedOnly(t.Context(), ResolveTarget{InfoHash: H}, []DebridService{ServicePremiumize})
	if err != nil || link != "https://pm/x" {
		t.Fatalf("resolve from the holder: %q %v", link, err)
	}
	if len(calls) != 1 || calls[0] != ServicePremiumize {
		t.Errorf("stores called: %v — TorBox does not hold this, and asking it would add the torrent", calls)
	}

	// With no known holder it refuses outright rather than shopping around.
	calls = nil
	if _, err := pool.ResolveCachedOnly(t.Context(), ResolveTarget{InfoHash: H}, nil); err == nil {
		t.Error("no holder should be a refusal")
	}
	if len(calls) != 0 {
		t.Errorf("asked %v with no holder known", calls)
	}

	// A holder that is configured but not in the pool leaves nothing to ask — a dead link, not a crash.
	absent := &StorePool{stores: []Store{namedStore{svc: ServiceTorBox, resolves: &calls}}}
	if _, err := absent.ResolveCachedOnly(t.Context(), ResolveTarget{InfoHash: H},
		[]DebridService{ServicePremiumize}); err == nil {
		t.Error("a holder that is not in the pool cannot resolve")
	}

	// A store REFUSING us (throttled, faulting) outranks "dead" as the explanation — the same rule
	// ResolvePreferring follows, because a refusal is not evidence that a release does not exist.
	refusing := &StorePool{stores: []Store{
		namedStore{svc: ServiceTorBox, resolves: &calls, refuse: true},
	}}
	_, err = refusing.ResolveCachedOnly(t.Context(), ResolveTarget{InfoHash: H}, []DebridService{ServiceTorBox})
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) {
		t.Errorf("a refusal must surface as unavailable, not as a dead link: %v", err)
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
