package scout

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// heldOnTorBox is the cache truth a probe needs: these hashes are confirmed present on the TorBox
// account, so resolving them is a read rather than an add.
func heldOnTorBox(hashes ...string) CacheTruth {
	truth := CacheTruth{holders: map[string][]DebridService{}, known: map[string]bool{}}
	for _, h := range hashes {
		truth.holders[h] = []DebridService{ServiceTorBox}
		truth.known[h] = true
	}
	return truth
}

// Probing costs a debrid RESOLVE per release, so it must never happen by accident. A caller that hasn't
// wired a probe client gets the old behaviour exactly — the list is built without touching the account.
func TestProbeTop_optIn(t *testing.T) {
	resolves := 0
	h := &handler{deps: Deps{
		Cache: NewMemoryCache(1 << 20),
		MakeStores: func(*Config) []Store {
			resolves++
			return nil
		},
	}}
	streams := []RawStream{{InfoHash: "abc"}}
	h.probeTop(context.Background(), &Config{}, streams, nil, heldOnTorBox("abc"))
	if resolves != 0 || streams[0].Probe != nil {
		t.Fatalf("probed without a client: resolves=%d probe=%v", resolves, streams[0].Probe)
	}
}

// Only the top of the list is probed. The order is the one the viewer sees, so probing all twenty would
// spend a resolve apiece describing releases nobody scrolls to.
func TestProbeTop_capsAtTopN(t *testing.T) {
	streams := make([]RawStream, 20)
	for i := range streams {
		streams[i].InfoHash = "hash"
		streams[i].Cached = true
	}
	cache := NewMemoryCache(1 << 20)
	// Pre-seed every entry so no resolve is attempted, then count how many were consulted.
	for i := range streams {
		cache.Put(probeCacheKey(&streams[i], nil), `{"audioLanguages":["sv"]}`, probeTTL)
	}
	h := &handler{deps: Deps{Cache: cache, ProbeClient: http.DefaultClient,
		MakeStores: func(*Config) []Store { return nil }}}
	h.probeTop(context.Background(), &Config{}, streams, nil, heldOnTorBox("hash"))

	probed := 0
	for i := range streams {
		if streams[i].Probe != nil {
			probed++
		}
	}
	if probed != probeTopN {
		t.Fatalf("probed %d releases, want the top %d only", probed, probeTopN)
	}
}

// A cached probe must not cost a resolve — that is what makes the disk tier worth having across restarts.
func TestProbeTop_cacheHitSkipsResolve(t *testing.T) {
	cache := NewMemoryCache(1 << 20)
	s := RawStream{InfoHash: "cached-hash", Cached: true}
	cache.Put(probeCacheKey(&s, nil), `{"audioLanguages":["it"],"videoCodec":"h264"}`, probeTTL)
	made := 0
	h := &handler{deps: Deps{Cache: cache, ProbeClient: http.DefaultClient,
		MakeStores: func(*Config) []Store { made++; return nil }}}
	streams := []RawStream{s}
	h.probeTop(context.Background(), &Config{}, streams, nil, heldOnTorBox("cached-hash"))

	if made != 0 {
		t.Fatalf("built a store pool despite a cache hit (%d)", made)
	}
	if streams[0].Probe == nil || len(streams[0].Probe.Audio) != 1 || streams[0].Probe.Audio[0] != "it" {
		t.Fatalf("cache hit not applied: %+v", streams[0].Probe)
	}
}

// A release the store does not hold is never probed at all.
//
// Probing resolves, and resolving a release the account does not hold ADDS it — so this read path was
// queueing up to six torrents per newly-viewed title against a sixty-an-hour ceiling. Browsing spent the
// quota, and the refusals that followed were read as dead releases and blamed on the releases.
func TestProbeTop_uncachedIsNeverProbed(t *testing.T) {
	resolved := make(chan struct{}, 1)
	h := &handler{deps: Deps{
		Cache:       NewMemoryCache(1 << 20),
		ProbeClient: http.DefaultClient,
		MakeStores: func(*Config) []Store {
			return []Store{fakeStore{svc: ServiceTorBox, resolve: func() (string, error) {
				select {
				case resolved <- struct{}{}:
				default:
				}
				return "", nil
			}}}
		},
	}}
	streams := []RawStream{{InfoHash: "uncached-hash", Cached: false}}
	h.probeTop(context.Background(), &Config{}, streams, nil, CacheTruth{})

	if streams[0].Probe != nil {
		t.Fatalf("an uncached release was probed: %+v", streams[0].Probe)
	}
	select {
	case <-resolved:
		t.Fatal("an uncached release was resolved — that adds the torrent and spends the add quota")
	case <-time.After(500 * time.Millisecond):
	}
}

// A HELD release still probes behind the response. Probing costs a ranged read apiece, which put ~12s on
// top of the scrape and timed clients out; the list goes out first and the probe warms the cache for the
// next request.
func TestProbeTop_cachedProbesBehindTheResponse(t *testing.T) {
	resolved := make(chan struct{}, 1)
	h := &handler{deps: Deps{
		Cache:       NewMemoryCache(1 << 20),
		ProbeClient: http.DefaultClient,
		MakeStores: func(*Config) []Store {
			return []Store{fakeStore{svc: ServiceTorBox, resolve: func() (string, error) {
				select {
				case resolved <- struct{}{}:
				default:
				}
				return "", nil // no link → nothing to probe, but the attempt is what's under test
			}}}
		},
	}}
	streams := []RawStream{{InfoHash: "held-hash", Cached: true}}
	h.probeTop(context.Background(), &Config{}, streams, nil, heldOnTorBox("held-hash"))

	// Returned WITHOUT the probe: that is the whole point — the viewer gets the list now.
	if streams[0].Probe != nil {
		t.Fatalf("a held release was probed inline: %+v", streams[0].Probe)
	}
	// …and the work still happens, just not in the client's way.
	select {
	case <-resolved:
	case <-time.After(5 * time.Second):
		t.Fatal("the background probe never ran — the cache would never warm")
	}
}

// The file is the identity, not the request: the same release serves every user of this instance.
func TestProbeCacheKey_identifiesTheFile(t *testing.T) {
	idx := 3
	a := RawStream{InfoHash: "h", FileIdx: &idx}
	b := RawStream{InfoHash: "h", FileIdx: &idx}
	if probeCacheKey(&a, nil) != probeCacheKey(&b, nil) {
		t.Fatal("same file produced different keys")
	}
	other := 4
	c := RawStream{InfoHash: "h", FileIdx: &other}
	if probeCacheKey(&a, nil) == probeCacheKey(&c, nil) {
		t.Fatal("different files in one torrent share a key")
	}
}

// A probed release's own facts replace what the title guessed; an unprobed one is left exactly as it was.
func TestWithProbe_overridesTitleGuesses(t *testing.T) {
	base := StreamAttributes{Codec: strPtr("h264"), AudioChannels: strPtr("2.0")}
	got := withProbe(base, &Probe{VideoCodec: "mpeg4", AudioChannels: "5.1", Audio: []string{"sv"}, DolbyVision: true})
	if *got.Codec != "mpeg4" || *got.AudioChannels != "5.1" || !got.DolbyVision || !got.HDR || !got.Probed {
		t.Fatalf("probe did not override the title: %+v", got)
	}
	if len(got.AudioLanguages) != 1 || got.AudioLanguages[0] != "sv" {
		t.Fatalf("languages missing: %v", got.AudioLanguages)
	}
	untouched := withProbe(base, nil)
	if untouched.Probed || untouched.AudioLanguages != nil || *untouched.Codec != "h264" {
		t.Fatalf("unprobed release was altered: %+v", untouched)
	}
}

// The probe resolves ONLY against a store that holds the release — never through the pool.
//
// `Cached` is the union across accounts, so with two configured, a release only the SECOND holds still
// reads as cached. Resolving that through the pool reaches the first account in priority order, and an
// account that does not hold a torrent does not fetch it — it ADDS it. Six probes per newly-viewed title
// against a sixty-an-hour ceiling is a browse that spends the evening's quota with nobody pressing play.
// Invisible on a single-account install, which is why the existing guard looked sufficient.
func TestProbeTop_resolvesOnlyAgainstAHoldingStore(t *testing.T) {
	var asked []DebridService
	var mu sync.Mutex
	record := func(svc DebridService) func() (string, error) {
		return func() (string, error) {
			mu.Lock()
			asked = append(asked, svc)
			mu.Unlock()
			return "", nil
		}
	}
	h := &handler{deps: Deps{
		Cache:       NewMemoryCache(1 << 20),
		ProbeClient: http.DefaultClient,
		MakeStores: func(*Config) []Store {
			return []Store{
				fakeStore{svc: ServiceTorBox, resolve: record(ServiceTorBox)},
				fakeStore{svc: ServicePremiumize, resolve: record(ServicePremiumize)},
			}
		},
	}}
	// Held on Premiumize only — TorBox is configured first and must never be asked.
	truth := CacheTruth{
		holders: map[string][]DebridService{"pm-only": {ServicePremiumize}},
		known:   map[string]bool{"pm-only": true},
	}
	streams := []RawStream{{InfoHash: "pm-only", Cached: true}}
	h.probeTop(context.Background(), &Config{}, streams, nil, truth)

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		ran := len(asked) > 0
		mu.Unlock()
		if ran {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the background probe never ran")
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(100 * time.Millisecond) // leave room for a wrong second call to happen

	mu.Lock()
	defer mu.Unlock()
	for _, svc := range asked {
		if svc == ServiceTorBox {
			t.Fatalf("probed through TorBox, which does not hold this — that ADDS the torrent. asked=%v", asked)
		}
	}
}

// A probe that couldn't read a field must not blank what the title supplied.
func TestWithProbe_emptyFieldsDoNotErase(t *testing.T) {
	base := StreamAttributes{Codec: strPtr("hevc"), AudioChannels: strPtr("7.1")}
	got := withProbe(base, &Probe{Container: "avi"})
	if *got.Codec != "hevc" || *got.AudioChannels != "7.1" {
		t.Fatalf("empty probe erased title data: %+v", got)
	}
}

// The probe marks its target NoAdd, so the store refuses rather than queueing.
//
// Choosing only held releases was not enough, and this is the assertion that says why: TorBox's cache
// check reports what TORBOX has, not what this ACCOUNT has, so a "held" release with no warm resolve
// entry went straight to createtorrent. Six adds per newly-viewed title, from a read path, which is the
// bug the holder-scoping was supposed to have ended.
func TestProbeTop_marksItsTargetNoAdd(t *testing.T) {
	seen := make(chan ResolveTarget, 1)
	h := &handler{deps: Deps{
		Cache:       NewMemoryCache(1 << 20),
		ProbeClient: http.DefaultClient,
		MakeStores: func(*Config) []Store {
			return []Store{capturingStore{svc: ServiceTorBox, seen: seen}}
		},
	}}
	streams := []RawStream{{InfoHash: "held-hash", Cached: true}}
	h.probeTop(context.Background(), &Config{}, streams, nil, heldOnTorBox("held-hash"))

	select {
	case target := <-seen:
		if !target.NoAdd {
			t.Error("the probe resolved without NoAdd — the store is free to queue the torrent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the background probe never ran")
	}
}

type capturingStore struct {
	svc  DebridService
	seen chan ResolveTarget
}

func (c capturingStore) Service() DebridService { return c.svc }
func (c capturingStore) CacheCheck(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (c capturingStore) Resolve(_ context.Context, t ResolveTarget) (string, error) {
	select {
	case c.seen <- t:
	default:
	}
	return "", nil
}
func (c capturingStore) Status(context.Context, ResolveTarget) (StoreStatus, bool) {
	return StoreStatus{}, false
}
