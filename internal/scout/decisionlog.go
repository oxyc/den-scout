package scout

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Decision logging: a line that, read from the box journal alone, explains AND reproduces a reported
// problem — "no sources", "wrong release", "it stalled" — without a client in the loop. Every such line
// shares one shape, the convention every den service's logs use:
//
//	event=<token> outcome=<served|skipped|refused|degraded|fallback> reason=<text> \
//	    upstream=<indexer/debrid name, when one is involved> dur_ms=<n> rid=<id> [identity…]
//
// event/outcome/reason/dur_ms/rid are always present (upstream only when an external system is
// involved); the identity fields that follow — title id, season/episode, release name, infohash,
// file index/size, runner-ups — are gated by LOG_IDENTITY as one unit, built here rather than at
// each call site, so no line can forget the gate.

// logIdentity mirrors Settings.LogIdentity (LOG_IDENTITY, default on). Set once at startup from
// BuildDeps, the same pattern as EnableIndexerConfigMinting / SetPlayReserve — never from a request.
var logIdentity = true

// SetLogIdentity turns the identity fields on new decision lines on or off. Called once at startup.
func SetLogIdentity(on bool) { logIdentity = on }

// decisionLine composes the shared shape. identity is whatever the caller already built — this is the
// one place that decides whether it survives into the line, so LOG_IDENTITY has exactly one choke
// point rather than one per call site that each has to remember it.
func decisionLine(event, outcome, reason, upstream string, dur time.Duration, rid, identity string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "event=%s outcome=%s reason=%s", event, outcome, reason)
	if upstream != "" {
		fmt.Fprintf(&b, " upstream=%s", upstream)
	}
	fmt.Fprintf(&b, " dur_ms=%d", dur.Milliseconds())
	if rid != "" {
		b.WriteString(" rid=")
		b.WriteString(rid)
	}
	if logIdentity && identity != "" {
		b.WriteByte(' ')
		b.WriteString(identity)
	}
	return b.String()
}

// maxLoggedTitle bounds a release name in a log line — it comes from an indexer, unbounded, and a
// log line must stay one line.
const maxLoggedTitle = 140

// truncateForLog cuts a string for a log line, the way storeErrorDetail already does for a debrid
// error body.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// logRankDecision is the per-list-build decision line: title id, how many candidates were in
// contention, the chosen release and its runner-ups, and the top drop reasons — everything ?debug=1
// used to require a client to ask for, now on every build, rate-limited like any other decision line.
func logRankDecision(sid *StreamID, candidates int, ranked []RawStream, dbg *rankDebug, dur time.Duration, rid string) {
	outcome, reason := "served", fmt.Sprintf("picked from %d candidates", candidates)
	if len(ranked) == 0 {
		outcome, reason = "skipped", fmt.Sprintf("no survivors among %d candidates", candidates)
	}
	logLimited("rank-decision", decisionLine("rank", outcome, reason, "", dur, rid, rankIdentity(sid, ranked, dbg)))
}

// rankRunnerUps: "the chosen first release and the next 2-3 runner-ups".
const rankRunnerUps = 3

// rankIdentity is the rank decision line's identity suffix: title id, the chosen release and its
// runner-ups, and the top drop reasons — one unit, gated by LOG_IDENTITY like every other identity
// suffix here.
func rankIdentity(sid *StreamID, ranked []RawStream, dbg *rankDebug) string {
	var b strings.Builder
	fmt.Fprintf(&b, "imdb=%s type=%s ", sid.IMDb, sid.Type)
	if sid.HasEp {
		fmt.Fprintf(&b, "season=%d episode=%d ", sid.Season, sid.Episode)
	}
	if drops := topDropReasons(dbg.DroppedBy, 3); drops != "" {
		fmt.Fprintf(&b, "top_drops=%s ", drops)
	}
	for i := 0; i < len(ranked) && i <= rankRunnerUps; i++ {
		label := "picked"
		if i > 0 {
			label = fmt.Sprintf("runnerup%d", i)
		}
		fmt.Fprintf(&b, "%s=%s ", label, formatPick(ranked[i]))
	}
	return strings.TrimSuffix(b.String(), " ")
}

// scrapeIdentity is the identity suffix for a per-indexer scrape decision line: the title being asked
// about. Built unconditionally and gated by decisionLine.
func scrapeIdentity(q scrapeQuery) string {
	var b strings.Builder
	if q.IMDb != "" {
		fmt.Fprintf(&b, "imdb=%s ", q.IMDb)
	}
	if q.Type != "" {
		fmt.Fprintf(&b, "type=%s ", q.Type)
	}
	if q.HasEp {
		fmt.Fprintf(&b, "season=%d episode=%d ", q.Season, q.Episode)
	}
	return strings.TrimSuffix(b.String(), " ")
}

// resolveIdentity is the identity suffix for a play/probe/cancel decision line: the release a route is
// acting on. Built unconditionally and gated by decisionLine, like every other identity suffix here.
func resolveIdentity(rt ResolveTarget) string {
	var b strings.Builder
	if rt.IMDb != "" {
		fmt.Fprintf(&b, "imdb=%s ", rt.IMDb)
	}
	if rt.Season != nil {
		fmt.Fprintf(&b, "season=%d ", *rt.Season)
	}
	if rt.Episode != nil {
		fmt.Fprintf(&b, "episode=%d ", *rt.Episode)
	}
	if rt.FileIdx != nil {
		fmt.Fprintf(&b, "file_idx=%d ", *rt.FileIdx)
	}
	if rt.InfoHash != "" {
		fmt.Fprintf(&b, "hash=%s ", shortHash(rt.InfoHash))
	}
	if rt.Title != "" {
		fmt.Fprintf(&b, "release=%q ", truncateForLog(rt.Title, maxLoggedTitle))
	}
	return strings.TrimSuffix(b.String(), " ")
}

// playDecision is a play/probe/cancel decision line: the shared shape plus the release it names.
func playDecision(event, outcome, reason, upstream string, dur time.Duration, rid string, rt ResolveTarget) string {
	return decisionLine(event, outcome, reason, upstream, dur, rid, resolveIdentity(rt))
}

// topDropReasons renders a drop-count map as the "top reasons" a decision line reports: the n largest
// counts, highest first, ties broken by name so the line is deterministic. "" when nothing was dropped.
func topDropReasons(counts map[string]int, n int) string {
	if len(counts) == 0 {
		return ""
	}
	type kv struct {
		reason string
		n      int
	}
	all := make([]kv, 0, len(counts))
	for k, v := range counts {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].reason < all[j].reason
	})
	if len(all) > n {
		all = all[:n]
	}
	parts := make([]string, len(all))
	for i, e := range all {
		parts[i] = fmt.Sprintf("%s:%d", e.reason, e.n)
	}
	return strings.Join(parts, ",")
}

// formatPick renders one served release for the rank decision line: label, short hash, size, cached —
// the identity of a specific pick, which is why it rides inside the same LOG_IDENTITY gate as the rest.
func formatPick(s RawStream) string {
	cached := "uncached"
	if s.Cached {
		cached = "cached"
	}
	sizeGB := "unknown"
	if s.SizeBytes != nil {
		sizeGB = fmt.Sprintf("%.1f", float64(*s.SizeBytes)/float64(gib))
	}
	return fmt.Sprintf("%q(hash=%s,size_gb=%s,%s)", truncateForLog(s.Title, maxLoggedTitle), shortHash(s.InfoHash), sizeGB, cached)
}
