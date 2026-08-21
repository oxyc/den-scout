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

// How long the whole fan-out may take. It runs BEHIND the response now, so this bounds the background
// work rather than the client's wait — see probeTop.
const probeBudget = 12 * time.Second

// Probe results never change for a given file, so they are cached hard. The tier is disk-backed, which is
// what makes a redeploy cheap.
const probeTTL = 720 * time.Hour

// probeTop attaches track details to the first few ranked releases, and never delays the reply.
//
// Only the CACHE is consulted on the request path — a map lookup, no network. Anything not yet known is
// probed behind the response, so the next request for the title has it. Probing inline cost a debrid
// resolve plus a ranged read per release, up to `probeBudget` on top of the scrape; measured end to end,
// a cold stream list took ~28s and clients timed out waiting for it. This file already said a late list
// is worse than one without track details — it just wasn't true of the code.
//
// The trade is that the first ask for a title is described by the indexer alone. Every later one, for
// every user of this instance, has the probe facts.
//
// Best-effort by construction: a release that can't be resolved, a server that ignores Range, a container
// nobody parses — all leave the entry exactly as the indexer described it.
func (h *handler) probeTop(ctx context.Context, config *Config, streams []RawStream, sid *StreamID) {
	// Opt-in: no client, no probing. Probing costs a debrid RESOLVE per release, so it must never happen
	// by accident — a caller that hasn't asked for it (a test, an embedder) gets the old behaviour
	// exactly, and the stream list is built without touching the debrid account at all.
	if h.deps.ProbeClient == nil || h.deps.MakeStores == nil {
		return
	}
	var pending []probeJob
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
		pending = append(pending, probeJob{
			key: key,
			target: ResolveTarget{
				InfoHash: s.InfoHash, FileIdx: s.FileIdx, Season: seasonOf(sid), Episode: episodeOf(sid),
			},
		})
	}
	if len(pending) > 0 {
		h.probeBehind(config, pending)
	}
}

// probeJob is a copy, deliberately: the background work must not reach into the response's slice, which
// the caller is free to marshal and hand to the client the moment probeTop returns.
type probeJob struct {
	key    string
	target ResolveTarget
}

// probeBehind warms the probe cache after the reply has gone out.
//
// Its own context, not the request's: the request is over, so inheriting it would cancel every probe
// immediately. Deduped through the handler's singleflight so a title several clients ask for at once is
// probed once, not once per request.
func (h *handler) probeBehind(config *Config, jobs []probeJob) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), probeBudget)
		defer cancel()
		pool := &StorePool{stores: h.deps.MakeStores(config)}
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(3) // gentle on the debrid account: a handful of resolves, not a burst
		for _, job := range jobs {
			g.Go(func() error {
				_, _, _ = h.sf.Do("probe:"+job.key, func() (any, error) {
					link, err := pool.Resolve(gctx, job.target)
					if err != nil || link == "" {
						return nil, nil
					}
					p, err := ProbeTracks(gctx, h.deps.ProbeClient, link)
					if err != nil {
						return nil, nil
					}
					if body, err := json.Marshal(p); err == nil {
						h.deps.Cache.Put(job.key, string(body), probeTTL)
					}
					return nil, nil
				})
				return nil
			})
		}
		_ = g.Wait()
	}()
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
