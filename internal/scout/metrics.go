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

type metricSet struct {
	listCacheHit   atomic.Int64
	listCacheStale atomic.Int64
	listCacheMiss  atomic.Int64

	buildOK       atomic.Int64
	buildPartial  atomic.Int64
	buildDegraded atomic.Int64

	probeCacheHit  atomic.Int64
	probeCacheMiss atomic.Int64
	// Panics recovered on ANY background goroutine — the probe fan-out and the stale-list rebuild both
	// route here. Deliberately not named for the probe: the two share one recover helper, and booking a
	// rebuild's panic to a probe series would ruin the metric an operator alerts on for a parser crash.
	backgroundPanic atomic.Int64

	// Fixed keys, populated once at construction and never written again, so concurrent reads need no
	// lock. Every indexer this build knows about gets an entry whether or not any install names it —
	// a counter that appears only after the first request is a counter you cannot alert on.
	indexerRequests map[Indexer]*atomic.Int64
	indexerFailures map[Indexer]*atomic.Int64
}

func newMetricSet() *metricSet {
	m := &metricSet{
		indexerRequests: make(map[Indexer]*atomic.Int64, len(allIndexers)),
		indexerFailures: make(map[Indexer]*atomic.Int64, len(allIndexers)),
	}
	for _, id := range allIndexers {
		m.indexerRequests[id] = new(atomic.Int64)
		m.indexerFailures[id] = new(atomic.Int64)
	}
	return m
}

// indexerResult records one scrape attempt. An indexer that is not in the fixed set is ignored rather
// than added, so a caller cannot grow this map at runtime and race the readers.
func (m *metricSet) indexerResult(id Indexer, ok bool) {
	if c := m.indexerRequests[id]; c != nil {
		c.Add(1)
	}
	if !ok {
		if c := m.indexerFailures[id]; c != nil {
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
	counter(&b, "scout_background_panics_total", "Panics recovered on a background goroutine (probe fan-out, stale list rebuild).",
		[][2]string{{"", num(m.backgroundPanic.Load())}})

	reqs := make([][2]string, 0, len(allIndexers))
	fails := make([][2]string, 0, len(allIndexers))
	for _, id := range allIndexers {
		label := `indexer="` + string(id) + `"`
		reqs = append(reqs, [2]string{label, num(m.indexerRequests[id].Load())})
		fails = append(fails, [2]string{label, num(m.indexerFailures[id].Load())})
	}
	counter(&b, "scout_indexer_requests_total", "Scrape attempts per indexer.", reqs)
	counter(&b, "scout_indexer_failures_total", "Scrape attempts that did not answer, per indexer.", fails)

	// The tightest remaining debrid add allowance, aggregated across accounts exactly as /health reports
	// it. -1 when nothing has been spent yet, which is distinct from 0 (spent out).
	left, accounts := globalAddBudget.lowest()
	if accounts == 0 {
		left = -1
	}
	b.WriteString("# HELP scout_add_budget_remaining Smallest remaining hourly add allowance across accounts (-1 = none spent).\n")
	b.WriteString("# TYPE scout_add_budget_remaining gauge\n")
	b.WriteString("scout_add_budget_remaining " + num(int64(left)) + "\n")

	b.WriteString("# HELP scout_cache_persistent Durable cache tier writing (1), disabled (0), not reported (-1).\n")
	b.WriteString("# TYPE scout_cache_persistent gauge\n")
	b.WriteString("scout_cache_persistent " + num(int64(cachePersistent)) + "\n")

	return b.String()
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
