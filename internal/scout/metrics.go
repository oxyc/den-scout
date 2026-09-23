package scout

import (
	"strconv"
	"strings"
	"sync/atomic"
)

// Counters for the questions this package used to answer only by grepping logs: which indexer is earning
// its slice of the scrape budget, how often a list is served from cache, how many builds come back
// degraded or short, whether the probe cache is warming.
//
// Every one of these facts was already computed and then dropped — scrapeAll knew which scrapers
// answered, buildStreamList knew whether the list was degraded or partial, probeTop knew whether the key
// was warm — so this adds observation, not work.
//
// Package-level rather than threaded through, because the interesting events happen in free functions
// (scrapeAll) as much as on the handler, and passing a collector into all of them would be a much larger
// change than the counters justify. Atomics only: these sit on the request path and must not allocate or
// contend.
var metrics = newMetricSet()

// metricIndexers are the indexer series: every built-in indexer, and one for every household's own sources
// together — their hosts are chosen per install, so they are never a label.
var metricIndexers = append(append([]Indexer(nil), allIndexers...), ownSources)

// The attribute values released streams are counted under. Fixed, for the same reason the indexer series are:
// a label that appears only once something matches it is a label nobody can alert on or divide by. "unknown"
// is one of the values rather than a gap — an unprobed release is most of a real population, and leaving it
// out would flatter every share computed from these.
var (
	metricCodecs      = []string{"h264", "hevc", "av1", "unknown"}
	metricResolutions = []string{"2160p", "1080p", "720p", "480p", "unknown"}
	metricAudioCodecs = []string{
		"eac3", "ac3", "truehd", "dts", "dtshd", "dtshdma", "dtsx", "flac", "aac", "opus", "mp3", "unknown",
	}
)

type metricSet struct {
	listCacheHit   atomic.Int64
	listCacheStale atomic.Int64
	listCacheMiss  atomic.Int64

	buildOK       atomic.Int64
	buildPartial  atomic.Int64
	buildDegraded atomic.Int64

	probeCacheHit  atomic.Int64
	probeCacheMiss atomic.Int64
	// Panics recovered on ANY background goroutine — the probe fan-out, the stale-list rebuild and the
	// account-listing fetch all route here. Deliberately not named for the probe: they share one recover
	// helper, and booking a rebuild's panic to a probe series would ruin the metric an operator alerts on
	// for a parser crash.
	backgroundPanic atomic.Int64

	// Adds scout's own budget refused, by intent: a viewer waiting (the budget spent) or a prefetch (held
	// back by the reserve kept for Play).
	addRefusedPlay     atomic.Int64
	addRefusedPrefetch atomic.Int64

	// Fixed keys, populated once at construction and never written again, so concurrent reads need no
	// lock. Every indexer this build knows about gets an entry whether or not any install names it —
	// a counter that appears only after the first request is a counter you cannot alert on.
	indexerRequests map[Indexer]*atomic.Int64
	indexerFailures map[Indexer]*atomic.Int64
	// Releases each indexer answered with, before dedupe: whether one that answers is also one that adds anything.
	indexerReleases map[Indexer]*atomic.Int64
	// How each configured source figured in a list build's coverage, by outcome. Unlike the request counters
	// this includes the sources that were never asked (misconfigured, quarantined) and answers reused from the
	// indexer answer cache, because coverage does.
	sourceCoverage map[Indexer]map[string]*atomic.Int64

	// Streams as they are SERVED, by the attributes a client ranks on. oxyc/den#26's "practical upshot"
	// table estimates what a modern debrid cache holds and says plainly that it is an estimate — nothing
	// in these repos sampled it. This is that sample, and it is deliberately taken at the point of
	// delivery rather than at the scrape: what matters is the population Den actually offers a viewer,
	// after dedupe and ranking, not the one an indexer happens to hold.
	releaseCodec      map[string]*atomic.Int64
	releaseResolution map[string]*atomic.Int64
	releaseAudio      map[string]*atomic.Int64

	releaseTotal       atomic.Int64
	releaseHDR         atomic.Int64
	releaseDolbyVision atomic.Int64
	releaseCached      atomic.Int64
}

func newMetricSet() *metricSet {
	m := &metricSet{
		indexerRequests: make(map[Indexer]*atomic.Int64, len(metricIndexers)),
		indexerFailures: make(map[Indexer]*atomic.Int64, len(metricIndexers)),
		indexerReleases: make(map[Indexer]*atomic.Int64, len(metricIndexers)),
		sourceCoverage:  make(map[Indexer]map[string]*atomic.Int64, len(metricIndexers)),
	}
	for _, id := range metricIndexers {
		m.indexerRequests[id] = new(atomic.Int64)
		m.indexerFailures[id] = new(atomic.Int64)
		m.indexerReleases[id] = new(atomic.Int64)
		m.sourceCoverage[id] = fixedSeries(sourceOutcomes)
	}
	m.releaseCodec = fixedSeries(metricCodecs)
	m.releaseResolution = fixedSeries(metricResolutions)
	m.releaseAudio = fixedSeries(metricAudioCodecs)
	return m
}

// fixedSeries is one counter per value, built once so the request path only ever reads this map.
func fixedSeries(values []string) map[string]*atomic.Int64 {
	series := make(map[string]*atomic.Int64, len(values))
	for _, v := range values {
		series[v] = new(atomic.Int64)
	}
	return series
}

// indexerResult records one scrape attempt and the releases it returned. An indexer that is not in the fixed set is
// ignored rather than added, so a caller cannot grow this map at runtime and race the readers.
func (m *metricSet) indexerResult(id Indexer, ok bool, releases int) {
	if c := m.indexerRequests[id]; c != nil {
		c.Add(1)
	}
	if c := m.indexerReleases[id]; c != nil {
		c.Add(int64(releases))
	}
	if !ok {
		if c := m.indexerFailures[id]; c != nil {
			c.Add(1)
		}
	}
}

// coverage records one build's source reports. Ids and outcomes outside the fixed sets are ignored.
func (m *metricSet) coverage(sources []sourceReport) {
	for _, s := range sources {
		if c := m.sourceCoverage[Indexer(s.ID)][s.Outcome]; c != nil {
			c.Add(1)
		}
	}
}

// render writes the Prometheus text exposition format by hand — the format is a dozen lines of printing,
// and a client library would be the first runtime dependency this binary has.
//
// WHAT IS DELIBERATELY ABSENT: anything keyed by debrid account or service. addbudget.go withholds
// per-account detail from /health for a reason that applies here word for word — this route is
// unauthenticated, every other route is protected by an unguessable config segment, and on a single-
// install box a per-service counter says which services that install uses. The add-budget gauge is the
// same aggregate /health already publishes.
//
// Indexer names ARE published. They are not a credential: the set is compiled into this binary, listed
// in the README, and shared by every install running the defaults, and "torrentio failed 40 times" says
// nothing about anybody's account. Being able to see which indexer is failing is the main reason this
// endpoint exists.
// cachePersistent: 1 when the durable tier is writing, 0 when it disabled itself, -1 when the backend
// does not report. Worth a series of its own because that failure is otherwise SILENT — one log line at
// startup and then a service that looks perfectly healthy while re-paying a debrid resolve per probed
// release on every redeploy. A monitor can see it now.
func (m *metricSet) render(cachePersistent int) string {
	var b strings.Builder
	b.Grow(2048)

	// Which release is answering, in the same shape every den addon publishes (<addon>_build_info), so one
	// query across the stack shows what each box is running without asking each manifest.
	b.WriteString("# HELP scout_build_info The running den-scout version.\n")
	b.WriteString("# TYPE scout_build_info gauge\n")
	b.WriteString(`scout_build_info{version="` + manifestVersion + `"} 1` + "\n")

	counter(&b, "scout_list_cache_total", "Stream-list cache lookups by outcome.",
		[][2]string{
			{`result="hit"`, num(m.listCacheHit.Load())},
			{`result="stale"`, num(m.listCacheStale.Load())},
			{`result="miss"`, num(m.listCacheMiss.Load())},
		})
	counter(&b, "scout_list_builds_total", "Stream-list builds by outcome.",
		[][2]string{
			{`result="ok"`, num(m.buildOK.Load())},
			{`result="partial"`, num(m.buildPartial.Load())},
			{`result="degraded"`, num(m.buildDegraded.Load())},
		})
	counter(&b, "scout_probe_cache_total", "Track-probe cache lookups by outcome.",
		[][2]string{
			{`result="hit"`, num(m.probeCacheHit.Load())},
			{`result="miss"`, num(m.probeCacheMiss.Load())},
		})
	counter(&b, "scout_background_panics_total", "Panics recovered on a background goroutine (probe fan-out, stale list rebuild, account listing fetch).",
		[][2]string{{"", num(m.backgroundPanic.Load())}})

	reqs := make([][2]string, 0, len(metricIndexers))
	fails := make([][2]string, 0, len(metricIndexers))
	releases := make([][2]string, 0, len(metricIndexers))
	for _, id := range metricIndexers {
		label := `indexer="` + string(id) + `"`
		reqs = append(reqs, [2]string{label, num(m.indexerRequests[id].Load())})
		fails = append(fails, [2]string{label, num(m.indexerFailures[id].Load())})
		releases = append(releases, [2]string{label, num(m.indexerReleases[id].Load())})
	}
	counter(&b, "scout_indexer_requests_total", "Scrape attempts per indexer.", reqs)
	counter(&b, "scout_indexer_failures_total", "Scrape attempts that did not answer, per indexer.", fails)
	counter(&b, "scout_indexer_releases_total", "Releases each indexer answered with, before dedupe.", releases)

	cov := make([][2]string, 0, len(metricIndexers)*len(sourceOutcomes))
	for _, id := range metricIndexers {
		for _, outcome := range sourceOutcomes {
			cov = append(cov, [2]string{`indexer="` + string(id) + `",outcome="` + outcome + `"`,
				num(m.sourceCoverage[id][outcome].Load())})
		}
	}
	counter(&b, "scout_source_coverage_total", "Configured sources in each list build's coverage, by outcome.", cov)

	counter(&b, "scout_released_streams_total", "Streams served to a client, the denominator for the shares below.",
		[][2]string{{"", num(m.releaseTotal.Load())}})
	counter(&b, "scout_released_video_codec_total", "Streams served, by video codec.", series(m.releaseCodec, metricCodecs, "codec"))
	counter(&b, "scout_released_resolution_total", "Streams served, by resolution.",
		series(m.releaseResolution, metricResolutions, "resolution"))
	counter(&b, "scout_released_audio_codec_total", "Streams served, by source audio codec.",
		series(m.releaseAudio, metricAudioCodecs, "codec"))
	counter(&b, "scout_released_feature_total", "Streams served carrying a feature, as a share of scout_released_streams_total.",
		[][2]string{
			{`feature="hdr"`, num(m.releaseHDR.Load())},
			{`feature="dolby_vision"`, num(m.releaseDolbyVision.Load())},
			{`feature="cached"`, num(m.releaseCached.Load())},
		})

	// The tightest remaining debrid add allowance, aggregated across accounts exactly as /health reports
	// it. -1 when nothing has been spent yet, which is distinct from 0 (spent out).
	left, accounts := globalAddBudget.lowest()
	if accounts == 0 {
		left = -1
	}
	b.WriteString("# HELP scout_add_budget_remaining Smallest remaining hourly add allowance across accounts (-1 = none spent).\n")
	b.WriteString("# TYPE scout_add_budget_remaining gauge\n")
	b.WriteString("scout_add_budget_remaining " + num(int64(left)) + "\n")

	// The same allowance as each intent sees it: a prefetch stops the reserve short of zero.
	prefetchLeft := left
	if left >= 0 {
		prefetchLeft = max(left-globalAddBudget.playReserve(), 0)
	}
	b.WriteString("# HELP scout_add_budget_remaining_by_intent Smallest remaining hourly add allowance across accounts, " +
		"as a viewer waiting (play) and a prefetch see it (-1 = none spent).\n")
	b.WriteString("# TYPE scout_add_budget_remaining_by_intent gauge\n")
	b.WriteString(`scout_add_budget_remaining_by_intent{intent="play"} ` + num(int64(left)) + "\n")
	b.WriteString(`scout_add_budget_remaining_by_intent{intent="prefetch"} ` + num(int64(prefetchLeft)) + "\n")
	counter(&b, "scout_add_budget_refusals_total", "Adds scout's own hourly budget refused, by intent.",
		[][2]string{
			{`intent="play"`, num(m.addRefusedPlay.Load())},
			{`intent="prefetch"`, num(m.addRefusedPrefetch.Load())},
		})

	b.WriteString("# HELP scout_cache_persistent Durable cache tier writing (1), disabled (0), not reported (-1).\n")
	b.WriteString("# TYPE scout_cache_persistent gauge\n")
	b.WriteString("scout_cache_persistent " + num(int64(cachePersistent)) + "\n")

	return b.String()
}

// series renders one fixed set of counters in a stable order, so a scrape's output does not reorder between
// reads the way a map's iteration would.
func series(counters map[string]*atomic.Int64, values []string, label string) [][2]string {
	out := make([][2]string, 0, len(values))
	for _, v := range values {
		out = append(out, [2]string{label + `="` + v + `"`, num(counters[v].Load())})
	}
	return out
}

// release records one stream as it goes out. Called per stream on the response path, so it only reads the
// fixed maps and adds to atomics — no allocation, no lock, nothing that can grow at runtime.
func (m *metricSet) release(a StreamAttributes) {
	m.releaseTotal.Add(1)
	bump(m.releaseCodec, deref(a.Codec))
	bump(m.releaseResolution, deref(a.Resolution))
	bump(m.releaseAudio, deref(a.AudioCodec))
	if a.HDR {
		m.releaseHDR.Add(1)
	}
	if a.DolbyVision {
		m.releaseDolbyVision.Add(1)
	}
	// Three answers, and only one of them counts: a nil Cached means nobody could ask, which is not "no".
	if a.Cached != nil && *a.Cached {
		m.releaseCached.Add(1)
	}
}

// bump counts a value against its series, or against "unknown" when the value is empty or not one this
// build knows — so a spelling nobody anticipated lands somewhere visible instead of vanishing.
func bump(counters map[string]*atomic.Int64, value string) {
	if c := counters[value]; c != nil {
		c.Add(1)
		return
	}
	if c := counters["unknown"]; c != nil {
		c.Add(1)
	}
}

// persistenceReporter is the optional half of the Cache seam: a backend that has a durable tier can say
// whether it is actually working. Optional because MemoryCache has nothing to report and a test cache
// should not have to grow a method to be usable.
type persistenceReporter interface{ Persistent() bool }

// cachePersistentGauge maps a cache onto the gauge's three states.
func cachePersistentGauge(c Cache) int {
	r, ok := c.(persistenceReporter)
	if !ok {
		return -1
	}
	if r.Persistent() {
		return 1
	}
	return 0
}

// counter writes one HELP/TYPE block and its samples. An empty label string means an unlabelled series.
func counter(b *strings.Builder, name, help string, samples [][2]string) {
	b.WriteString("# HELP " + name + " " + help + "\n")
	b.WriteString("# TYPE " + name + " counter\n")
	for _, s := range samples {
		b.WriteString(name)
		if s[0] != "" {
			b.WriteString("{" + s[0] + "}")
		}
		b.WriteString(" " + s[1] + "\n")
	}
}

func num(v int64) string { return strconv.FormatInt(v, 10) }
