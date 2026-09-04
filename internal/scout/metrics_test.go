package scout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The route answers in the exposition format, and reports the facts that used to be log-only.
func TestMetrics_reportsWhatWasPreviouslyLogOnly(t *testing.T) {
	h := NewHandler(testDeps(nil))

	// Drive one cold build and one warm hit, so the counters have something to say.
	path := "/" + validBlob + "/stream/movie/tt1234567.json"
	do(h, path, nil)
	do(h, path, nil)

	rr := do(h, "/metrics", nil)
	if rr.Code != 200 {
		t.Fatalf("/metrics: %d", rr.Code)
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

// The route is unauthenticated, like /health, and must therefore withhold exactly what /health withholds:
// anything that says which debrid services or accounts this install uses. addbudget.go calls that a
// confirmation oracle and refuses to publish it; publishing it one route over would simply move the leak.
func TestMetrics_carriesNoAccountOrServiceDetail(t *testing.T) {
	h := NewHandler(testDeps(nil))
	do(h, "/"+validBlob+"/stream/movie/tt1234567.json", nil)
	body := do(h, "/metrics", nil).Body.String()

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
