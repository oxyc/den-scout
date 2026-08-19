package scout

import (
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
