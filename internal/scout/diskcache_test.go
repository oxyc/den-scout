package scout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The point of the durable tier is a redeploy: the container restarts on every image push, and a probe
// costs a debrid resolve to rebuild. A fresh cache over the same directory must still know what the old
// one learned.
func TestTieredCache_survivesRestart(t *testing.T) {
	dir := t.TempDir()
	first := NewTieredCache(1<<20, dir)
	first.Put("tracks:abc", `{"audioLanguages":["swe"]}`, time.Hour)

	second := NewTieredCache(1<<20, dir) // a "restart": nothing in memory
	got, ok := second.Get("tracks:abc")
	if !ok || got != `{"audioLanguages":["swe"]}` {
		t.Fatalf("after restart got %q ok=%v, want the stored value", got, ok)
	}
}

// Expiry is wall-clock on disk: a monotonic deadline means nothing to the process that reads the file.
func TestTieredCache_expiredOnDiskIsNotServed(t *testing.T) {
	dir := t.TempDir()
	NewTieredCache(1<<20, dir).Put("k", "v", -time.Second) // already expired
	if got, ok := NewTieredCache(1<<20, dir).Get("k"); ok {
		t.Fatalf("served expired entry %q", got)
	}
}

// An unwritable directory must not take the service down with it — memory keeps serving.
func TestTieredCache_unwritableDirStillServesFromMemory(t *testing.T) {
	// A path under a FILE can never be created.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := writeFile(file, "x"); err != nil {
		t.Fatal(err)
	}
	c := NewTieredCache(1<<20, filepath.Join(file, "cache"))
	c.Put("k", "v", time.Hour)
	if got, ok := c.Get("k"); !ok || got != "v" {
		t.Fatalf("memory tier stopped serving: got %q ok=%v", got, ok)
	}
}

// Expiry was enforced only on READ, so a key nobody asks for again was written once and kept forever —
// a stream list for a title watched once, a one-minute refusal, a fifteen-second torrent-miss. On a
// homelab box that is unbounded growth with no ceiling and nothing reporting it.
func TestTieredCache_sweepReclaimsWhatNobodyReadsBack(t *testing.T) {
	dir := t.TempDir()
	// A tiny memory tier, so nothing is served from memory and the disk state is what's under test.
	c := NewTieredCache(1, dir)
	for i := 0; i < 20; i++ {
		c.Put(fmt.Sprintf("dead-%d", i), "v", time.Millisecond)
	}
	c.Put("alive", "v", time.Hour)
	time.Sleep(10 * time.Millisecond)

	if got := c.Sweep(); got != 20 {
		t.Errorf("swept %d entries, want the 20 expired ones", got)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Errorf("%d files left on disk, want only the unexpired one", len(files))
	}
	// And the survivor is still readable — a sweep that took the live entry would be worse than none.
	if got, ok := NewTieredCache(1<<20, dir).Get("alive"); !ok || got != "v" {
		t.Errorf("the sweep removed a live entry: %q ok=%v", got, ok)
	}
}

// A temp file only outlives its Put if the process died mid-write. Old ones are abandoned and swept;
// fresh ones may belong to a write still in flight, so they are left alone.
func TestTieredCache_sweepClearsAbandonedTempFilesOnly(t *testing.T) {
	dir := t.TempDir()
	c := NewTieredCache(1<<20, dir)

	stale := filepath.Join(dir, ".tmp-crashed")
	if err := writeFile(stale, "half"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * tempFileGrace)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, ".tmp-inflight")
	if err := writeFile(fresh, "writing"); err != nil {
		t.Fatal(err)
	}

	c.Sweep()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("an abandoned temp file survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("the sweep raced a write that was still in flight")
	}
}

// Keys carry config blobs and ids; they are neither filename-safe nor short.
func TestTieredCache_hashesAwkwardKeys(t *testing.T) {
	dir := t.TempDir()
	c := NewTieredCache(1<<20, dir)
	key := "list:" + string(make([]byte, 300)) + "/../../etc/passwd?x=1"
	c.Put(key, "v", time.Hour)
	if got, ok := NewTieredCache(1<<20, dir).Get(key); !ok || got != "v" {
		t.Fatalf("awkward key round-trip failed: %q ok=%v", got, ok)
	}
}

// SweepEvery sweeps once at startup — a redeploy is the likeliest moment for a backlog of entries that
// expired while the process was not running — then on its ticker, and stops when the context ends.
func TestTieredCache_sweepEveryRunsAndStops(t *testing.T) {
	dir := t.TempDir()
	c := NewTieredCache(1, dir)
	c.Put("dead", "v", time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.SweepEvery(ctx, time.Millisecond) }()

	deadline := time.After(2 * time.Second)
	for {
		if files, _ := os.ReadDir(dir); len(files) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("SweepEvery never swept the expired entry")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("SweepEvery ignored its cancelled context")
	}

	// A cache with persistence off has nothing to sweep and must return rather than tick forever.
	off := NewTieredCache(1<<20, "")
	returned := make(chan struct{})
	go func() { defer close(returned); off.SweepEvery(context.Background(), time.Hour) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Error("SweepEvery span a ticker for a cache with no disk tier")
	}
}
