package scout

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const metricsToken = "metrics-shared-secret"

var metricsAuth = map[string]string{"authorization": "Bearer " + metricsToken}

// The counters say when this install is being watched and which indexers it uses, so the route is gated
// and OFF entirely when no token is configured — 404, not an empty 200, so an unconfigured deployment
// does not advertise that there is something here.
func TestMetrics_isGated(t *testing.T) {
	off := NewHandler(testDeps(nil))
	if rr := do(off, "/metrics", nil); rr.Code != 404 {
		t.Errorf("no token configured: %d, want 404", rr.Code)
	}

	on := NewHandler(testDeps(func(d *Deps) { d.MetricsToken = metricsToken }))
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no header", nil, 404},
		{"wrong token", map[string]string{"authorization": "Bearer nope"}, 404},
		{"no bearer prefix", map[string]string{"authorization": metricsToken}, 200},
		{"correct", metricsAuth, 200},
	} {
		if rr := do(on, "/metrics", tc.headers); rr.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, rr.Code, tc.want)
		}
	}
}

// sampleValue pulls one series' value out of the exposition text, so a test can assert on the NUMBER
// rather than on the series existing — render() emits every series unconditionally, so presence proves
// only that render ran.
func sampleValue(t *testing.T, body, series string) int {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, series+" ") {
			var v int
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, series+" "), "%d", &v); err != nil {
				t.Fatalf("unparseable sample %q", line)
			}
			return v
		}
	}
	t.Fatalf("no series %q in:\n%s", series, body)
	return 0
}

// The route answers in the exposition format, and the numbers actually move — asserted as DELTAS,
// because the counters are process-wide and every other test in this package feeds them too.
func TestMetrics_reportsWhatWasPreviouslyLogOnly(t *testing.T) {
	h := NewHandler(testDeps(func(d *Deps) { d.MetricsToken = metricsToken }))
	before := do(h, "/metrics", metricsAuth).Body.String()

	// One cold build (a miss) then a warm hit, so each counter has a known increment.
	path := "/" + validBlob + "/stream/movie/tt1234567.json"
	do(h, path, nil)
	do(h, path, nil)

	rr := do(h, "/metrics", metricsAuth)
	if rr.Code != 200 {
		t.Fatalf("/metrics: %d", rr.Code)
	}
	after := rr.Body.String()
	for _, want := range []struct {
		series string
		delta  int
	}{
		{`scout_list_cache_total{result="miss"}`, 1},
		{`scout_list_cache_total{result="hit"}`, 1},
		{`scout_list_builds_total{result="ok"}`, 1},
		{`scout_indexer_requests_total{indexer="torrentio"}`, 1},
	} {
		got := sampleValue(t, after, want.series) - sampleValue(t, before, want.series)
		if got != want.delta {
			t.Errorf("%s moved by %d, want %d", want.series, got, want.delta)
		}
	}
	// A counter for something that did NOT happen must stay put.
	if d := sampleValue(t, after, `scout_list_builds_total{result="degraded"}`) -
		sampleValue(t, before, `scout_list_builds_total{result="degraded"}`); d != 0 {
		t.Errorf("a healthy build counted as degraded (delta %d)", d)
	}
	if ct := rr.Header().Get("content-type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	if cc := rr.Header().Get("cache-control"); cc != noStore {
		t.Errorf("cache-control = %q, want no-store", cc)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"scout_list_cache_total{result=\"hit\"}",
		"scout_list_cache_total{result=\"miss\"}",
		"scout_list_builds_total{result=\"ok\"}",
		"scout_indexer_requests_total{indexer=\"torrentio\"}",
		"scout_indexer_failures_total{indexer=\"torrentio\"}",
		"scout_probe_cache_total{result=\"hit\"}",
		"scout_add_budget_remaining",
		"scout_cache_persistent",
		"# TYPE scout_list_cache_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing series %q", want)
		}
	}
	// Every indexer is present from the start — a series that only appears after the first failure is one
	// nobody can alert on.
	for _, id := range allIndexers {
		if !strings.Contains(body, "scout_indexer_requests_total{indexer=\""+string(id)+"\"}") {
			t.Errorf("indexer %s has no series before it is used", id)
		}
	}
}

// The stale path has its own counter, and it is the one the stale-while-revalidate work exists to make
// visible — how often viewers are being served a list while it is rebuilt behind them.
func TestMetrics_countsStaleHits(t *testing.T) {
	cache := &recordingCache{Cache: NewMemoryCache(1 << 20)}
	h := NewHandler(testDeps(func(d *Deps) {
		d.Cache = cache
		d.MetricsToken = metricsToken
	}))
	path := "/" + validBlob + "/stream/movie/tt1234567.json"
	do(h, path, nil)

	before := sampleValue(t, do(h, "/metrics", metricsAuth).Body.String(), `scout_list_cache_total{result="stale"}`)

	key := cache.lastKey()
	held, ok := cache.Get(key)
	if !ok {
		t.Fatal("nothing cached")
	}
	complete, _, etag, body := splitCached(held)
	cache.Put(key, joinCached(complete, time.Now().Add(-time.Second).Unix(), etag, body), time.Minute)
	do(h, path, nil)

	after := sampleValue(t, do(h, "/metrics", metricsAuth).Body.String(), `scout_list_cache_total{result="stale"}`)
	if after-before != 1 {
		t.Errorf("stale hits moved by %d, want 1", after-before)
	}
}

// The route is unauthenticated, like /health, and must therefore withhold exactly what /health withholds:
// anything that says which debrid services or accounts this install uses. addbudget.go calls that a
// confirmation oracle and refuses to publish it; publishing it one route over would simply move the leak.
func TestMetrics_carriesNoAccountOrServiceDetail(t *testing.T) {
	h := NewHandler(testDeps(func(d *Deps) { d.MetricsToken = metricsToken }))
	do(h, "/"+validBlob+"/stream/movie/tt1234567.json", nil)
	body := do(h, "/metrics", metricsAuth).Body.String()

	// Only the SAMPLES are data. HELP text is documentation and says "accounts" on purpose.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		for _, forbidden := range []string{
			string(ServiceTorBox), string(ServiceRealDebrid), string(ServicePremiumize),
			"tb-secret", "tt1234567", validBlob,
		} {
			if strings.Contains(line, forbidden) {
				t.Errorf("/metrics sample discloses %q: %s", forbidden, line)
			}
		}
	}
}

// The disk tier disables itself silently — one log line, then memory keeps serving — so the gauge is the
// only thing that separates "persisting" from "quietly re-paying a debrid resolve per probe on every
// redeploy". Three states, because "the backend does not report" is not the same as "it is off".
func TestMetrics_cachePersistenceGauge(t *testing.T) {
	if got := cachePersistentGauge(NewMemoryCache(1 << 20)); got != -1 {
		t.Errorf("a backend with no durable tier reported %d, want -1", got)
	}
	working := NewTieredCache(1<<20, t.TempDir())
	if got := cachePersistentGauge(working); got != 1 {
		t.Errorf("a writable tier reported %d, want 1", got)
	}
	// A file where the directory should be: MkdirAll fails, persistence disables itself.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cachePersistentGauge(NewTieredCache(1<<20, blocked)); got != 0 {
		t.Errorf("an unwritable tier reported %d, want 0", got)
	}
}
