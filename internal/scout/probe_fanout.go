package scout

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"
)

// How many of the ranked releases get probed. The list is already in the order the client will show, so
// the ones a viewer actually considers are at the top — probing all twenty would spend a debrid resolve
// apiece to describe releases nobody scrolls to.
const probeTopN = 6

// How long the whole fan-out may take. A stream list that arrives late is worse than one without track
// details, so this is a budget the list can afford to lose entirely.
const probeBudget = 12 * time.Second

// Probe results never change for a given file, so they are cached hard. The tier is disk-backed, which is
// what makes a redeploy cheap.
const probeTTL = 720 * time.Hour

// probeTop fills in track details for the first few ranked releases, in place.
//
// Best-effort by construction: a release that can't be resolved, a server that ignores Range, a container
// nobody parses — all leave the entry exactly as the indexer described it. Nothing here can fail the
// stream list, which is why every error path just returns.
func (h *handler) probeTop(ctx context.Context, config *Config, streams []RawStream, sid *StreamID) {
	// Opt-in: no client, no probing. Probing costs a debrid RESOLVE per release, so it must never happen
	// by accident — a caller that hasn't asked for it (a test, an embedder) gets the old behaviour
	// exactly, and the stream list is built without touching the debrid account at all.
	if h.deps.ProbeClient == nil || h.deps.MakeStores == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()

	var pool *StorePool
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(3) // gentle on the debrid account: a handful of resolves, not a burst
	for i := range streams {
		if i >= probeTopN {
			break
		}
		s := &streams[i]
		if s.InfoHash == "" {
			continue
		}
		key := probeCacheKey(s, sid)
		if raw, ok := h.deps.Cache.Get(key); ok {
			var p Probe
			if json.Unmarshal([]byte(raw), &p) == nil {
				s.Probe = &p
			}
			continue
		}
		if pool == nil {
			pool = &StorePool{stores: h.deps.MakeStores(config)}
		}
		g.Go(func() error {
			link, err := pool.Resolve(gctx, ResolveTarget{
				InfoHash: s.InfoHash, FileIdx: s.FileIdx, Season: seasonOf(sid), Episode: episodeOf(sid),
			})
			if err != nil || link == "" {
				return nil
			}
			p, err := ProbeTracks(gctx, h.deps.ProbeClient, link)
			if err != nil {
				return nil
			}
			s.Probe = &p
			if body, err := json.Marshal(p); err == nil {
				h.deps.Cache.Put(key, string(body), probeTTL)
			}
			return nil
		})
	}
	_ = g.Wait()
}

// probeCacheKey identifies the FILE, not the request: the same release serves every user of this instance
// and every episode request that maps onto it.
func probeCacheKey(s *RawStream, sid *StreamID) string {
	key := "probe:v1:" + s.InfoHash
	if s.FileIdx != nil {
		key += ":" + strconv.Itoa(*s.FileIdx)
	}
	if sid != nil && sid.HasEp {
		key += ":" + strconv.Itoa(sid.Season) + ":" + strconv.Itoa(sid.Episode)
	}
	return key
}

func seasonOf(sid *StreamID) *int {
	if sid == nil || !sid.HasEp {
		return nil
	}
	s := sid.Season
	return &s
}

func episodeOf(sid *StreamID) *int {
	if sid == nil || !sid.HasEp {
		return nil
	}
	e := sid.Episode
	return &e
}
