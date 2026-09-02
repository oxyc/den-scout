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
func (h *handler) probeTop(ctx context.Context, config *Config, streams []RawStream, sid *StreamID,
	truth CacheTruth) {
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
		// Only probe what the store ALREADY holds. Probing resolves, and resolving a release the account
		// does not hold ADDS it — so this read path was queueing up to six torrents per newly-viewed
		// title, against a sixty-an-hour ceiling. Browsing did it; opening a ten-episode season did it
		// sixty times. That is where the quota went, and the refusals it produced were then read as dead
		// releases and blamed on the releases.
		//
		// Nothing is lost: probe facts are keyed by infohash and kept for a month, so a release is probed
		// for free the first time it is listed after it becomes cached. It also self-heals during a
		// cache-check outage — everything reads as not-held, so nothing is probed, which is precisely
		// when touching the debrid is least wise.
		if !s.Cached {
			continue
		}
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
		// WHICH services hold it, not just that one does. `s.Cached` is the union across accounts, so on a
		// two-account install a release only the second holds still reads as cached here — and resolving
		// it through the pool would reach the first account, which adds it. Carrying the holders lets the
		// probe resolve read-only.
		holders := truth.HeldBy(s.InfoHash)
		if len(holders) == 0 {
			continue
		}
		pending = append(pending, probeJob{
			key:     key,
			holders: holders,
			target: ResolveTarget{
				InfoHash: s.InfoHash, FileIdx: s.FileIdx, Season: seasonOf(sid), Episode: episodeOf(sid),
				// Enforced by the store, not promised by this caller. Picking only held releases was not
				// enough: TorBox's cache check says what TORBOX has, not what this ACCOUNT has, so a
				// "held" release with no warm resolve entry still went to createtorrent.
				NoAdd: true,
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
	key string
	// The services confirmed to hold this release. The probe resolves against these ONLY — anything else
	// would fetch rather than read, which is an add against the quota.
	holders []DebridService
	target  ResolveTarget
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
					link, err := pool.ResolveCachedOnly(gctx, job.target, job.holders)
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
