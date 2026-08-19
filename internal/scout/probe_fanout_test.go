package scout

import (
	"context"
	"net/http"
	"testing"
)

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
	h.probeTop(context.Background(), &Config{}, streams, nil)
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
	}
	cache := NewMemoryCache(1 << 20)
	// Pre-seed every entry so no resolve is attempted, then count how many were consulted.
	for i := range streams {
		cache.Put(probeCacheKey(&streams[i], nil), `{"audioLanguages":["sv"]}`, probeTTL)
	}
	h := &handler{deps: Deps{Cache: cache, ProbeClient: http.DefaultClient,
		MakeStores: func(*Config) []Store { return nil }}}
	h.probeTop(context.Background(), &Config{}, streams, nil)

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
	s := RawStream{InfoHash: "cached-hash"}
	cache.Put(probeCacheKey(&s, nil), `{"audioLanguages":["it"],"videoCodec":"h264"}`, probeTTL)
	made := 0
	h := &handler{deps: Deps{Cache: cache, ProbeClient: http.DefaultClient,
		MakeStores: func(*Config) []Store { made++; return nil }}}
	streams := []RawStream{s}
	h.probeTop(context.Background(), &Config{}, streams, nil)

	if made != 0 {
		t.Fatalf("built a store pool despite a cache hit (%d)", made)
	}
	if streams[0].Probe == nil || len(streams[0].Probe.Audio) != 1 || streams[0].Probe.Audio[0] != "it" {
		t.Fatalf("cache hit not applied: %+v", streams[0].Probe)
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

// A probe that couldn't read a field must not blank what the title supplied.
func TestWithProbe_emptyFieldsDoNotErase(t *testing.T) {
	base := StreamAttributes{Codec: strPtr("hevc"), AudioChannels: strPtr("7.1")}
	got := withProbe(base, &Probe{Container: "avi"})
	if *got.Codec != "hevc" || *got.AudioChannels != "7.1" {
		t.Fatalf("empty probe erased title data: %+v", got)
	}
}
