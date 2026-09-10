package scout

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// /health's flip is logged as a change of state: one line going degraded however many builds keep
// failing, and one coming back however many succeed.
func TestHealthFlipIsLoggedOnceEachWay(t *testing.T) {
	out := captureLog(t)
	var healthy atomic.Bool
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				if healthy.Load() {
					return testSeeds(), nil
				}
				return nil, context.Canceled
			}}}
		}
	}))
	build := func(i int) { do(h, "/"+validBlob+"/stream/movie/tt9"+strconv.Itoa(i)+".json", nil) }
	for i := 0; i < scrapeFailThreshold+3; i++ {
		build(i) // distinct titles, so each one builds
	}
	if n := strings.Count(out.String(), "health: ok → degraded (indexers): "+indexersDownDetail); n != 1 {
		t.Errorf("going degraded logged %d times, want 1:\n%s", n, out)
	}
	healthy.Store(true)
	build(80)
	build(81)
	if n := strings.Count(out.String(), "health: degraded → ok"); n != 1 {
		t.Errorf("recovering logged %d times, want 1:\n%s", n, out)
	}
}

// The startup line says what is running without saying anything secret.
func TestStartupSummary(t *testing.T) {
	env := map[string]string{
		"MEDIAFUSION_URL": "https://mf.self/SECRET-CONFIG",
		"CONFIG_KEY":      "key-secret",
		"METRICS_TOKEN":   "metrics-secret",
		"CACHE_DIR":       "/cache",
	}
	s := SettingsFromEnv(func(k string) string { return env[k] })
	line := StartupSummary(s, true)
	for _, want := range []string{"den-scout " + manifestVersion + " listening on :8080 — ",
		"indexers=torrentio,mediafusion", "overrides=mediafusion", "disabled=torz", "minting=off",
		"cache=/cache cache_persistent=on", "sealed=on", "metrics=on", "log_requests=off"} {
		if !strings.Contains(line, want) {
			t.Errorf("startup line lacks %q: %s", want, line)
		}
	}
	for _, leak := range []string{"SECRET-CONFIG", "mf.self", "key-secret", "metrics-secret"} {
		if strings.Contains(line, leak) {
			t.Errorf("startup line carries %q: %s", leak, line)
		}
	}
	if line := StartupSummary(s, false); !strings.Contains(line, "cache_persistent=off") {
		t.Errorf("an unwritable cache should say so: %s", line)
	}
}

// One condition, many occurrences: one line a minute, and the next line admits what it held back.
func TestLogLimited(t *testing.T) {
	out := captureLog(t)
	for i := 0; i < 3; i++ {
		logLimited("test-condition", "upstream down (%d)", i)
	}
	if got := strings.Count(out.String(), "upstream down"); got != 1 {
		t.Fatalf("three occurrences inside a minute wrote %d lines, want 1:\n%s", got, out)
	}
	// A different condition is not held back by this one.
	logLimited("test-other-condition", "another thing")
	if !strings.Contains(out.String(), "another thing") {
		t.Errorf("an unrelated condition was suppressed:\n%s", out)
	}
	// Once the minute has passed the condition speaks again, and says how many it swallowed.
	limitedLog.mu.Lock()
	e := limitedLog.last["test-condition"]
	e.at = e.at.Add(-logInterval)
	limitedLog.last["test-condition"] = e
	limitedLog.mu.Unlock()
	logLimited("test-condition", "upstream down (%d)", 3)
	if !strings.Contains(out.String(), "upstream down (3) (2 more like this since the last line)") {
		t.Errorf("the next line should report the two held back:\n%s", out)
	}
}
