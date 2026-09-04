package scout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TieredCache is the in-memory cache backed by a disk store, so a restart doesn't cold-start.
//
// The container is redeployed often — an image push restarts it — and everything the memory cache held is
// gone with it: stream lists, and now the track probes, which cost a debrid resolve each to rebuild.
// Mirrors den-subtitles: memory is the hot tier (bounded, LRU), disk is the durable one. `Put` writes
// through; a memory miss reads back lazily and repopulates.
//
// Disk is BEST-EFFORT throughout. An unwritable directory disables persistence and says so once; memory
// keeps serving, because a cache that fails closed on a permissions problem would take the service with it.
type TieredCache struct {
	mem  *MemoryCache
	dir  string
	once sync.Once
	off  bool
	mu   sync.Mutex
}

func NewTieredCache(maxBytes int, dir string) *TieredCache {
	c := &TieredCache{mem: NewMemoryCache(maxBytes), dir: dir}
	if dir == "" {
		c.off = true
		return c
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("den-scout: cache dir %s not writable (%v) — persistence disabled, memory still serving", dir, err)
		c.off = true
	}
	return c
}

func (c *TieredCache) Get(key string) (string, bool) {
	if v, ok := c.mem.Get(key); ok {
		return v, true
	}
	if c.disabled() {
		return "", false
	}
	raw, err := os.ReadFile(c.path(key))
	if err != nil {
		return "", false
	}
	// "<unix-expiry>\n<value>". Wall-clock on disk on purpose: a monotonic deadline means nothing to the
	// process that reads the file after a restart.
	nl := bytes.IndexByte(raw, '\n')
	if nl <= 0 {
		return "", false
	}
	expires, err := strconv.ParseInt(string(raw[:nl]), 10, 64)
	if err != nil {
		return "", false
	}
	if time.Now().Unix() >= expires {
		_ = os.Remove(c.path(key))
		return "", false
	}
	value := string(raw[nl+1:])
	c.mem.Put(key, value, time.Until(time.Unix(expires, 0)))
	return value, true
}

func (c *TieredCache) Put(key, value string, ttl time.Duration) {
	c.mem.Put(key, value, ttl)
	if c.disabled() {
		return
	}
	body := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10) + "\n" + value
	// Written via a temp file and renamed: a half-written entry read after a crash would decode as
	// garbage, and this cache's whole point is surviving restarts.
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		c.disable("create temp", err)
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		c.disable("write", err)
		return
	}
	if err := tmp.Close(); err != nil {
		c.disable("close", err)
		return
	}
	if err := os.Rename(tmp.Name(), c.path(key)); err != nil {
		c.disable("rename", err)
	}
}

// How often expired entries are swept, and how long a leftover temp file may linger before it counts as
// abandoned. Hourly is far more often than space needs and rare enough to be invisible: the sweep is a
// small read per file, and the store holds thousands, not millions.
const (
	SweepInterval = time.Hour
	tempFileGrace = time.Hour
)

// Sweep removes expired entries and abandoned temp files, and reports how many it deleted.
//
// Expiry was enforced only on READ — which means only for a key someone asks for again. Every key nobody
// re-reads was written once and kept forever: a stream list for a title watched once, a one-minute
// refusal, a fifteen-second torrent-miss, a probe for a release that never comes back up the ranking. On
// a homelab box that is unbounded growth with no ceiling and nothing reporting it.
//
// Best-effort like the rest of the disk tier: an unreadable directory, or a file that vanishes underneath
// us, is skipped rather than fatal.
// Deliberately NOT gated on `disabled()`. `disable` is set for the process lifetime by one failed write,
// and it also short-circuits Get before the on-read expiry removal — so a single ENOSPC or permissions
// blip used to turn off BOTH ways an entry is ever reclaimed, leaving the store to grow untouched. The
// directory may still be readable when it is not writable, and reaping is exactly what helps if the
// problem was space.
func (c *TieredCache) Sweep() int {
	if c.dir == "" {
		return 0
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return 0
	}
	now := time.Now()
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".ent"):
			if !c.entryExpired(filepath.Join(c.dir, name), now) {
				continue
			}
		case strings.HasPrefix(name, ".tmp-"):
			// A temp file outlives its Put only if the process died between CreateTemp and rename. The
			// grace period keeps the sweep from racing a write that is legitimately in flight.
			info, err := e.Info()
			if err != nil || now.Sub(info.ModTime()) < tempFileGrace {
				continue
			}
		default:
			continue // not ours
		}
		if os.Remove(filepath.Join(c.dir, name)) == nil {
			removed++
		}
	}
	return removed
}

// entryExpired reads just the expiry line. A file that cannot be read or parsed counts as expired — Get
// cannot serve it either way, so keeping it only keeps a file nothing can ever use.
func (c *TieredCache) entryExpired(path string, now time.Time) bool {
	f, err := os.Open(path)
	if err != nil {
		return false // it may already be gone; don't claim a deletion we didn't make
	}
	defer func() { _ = f.Close() }()
	// The expiry is a unix timestamp on the first line; 32 bytes is more than enough to reach the newline.
	head := make([]byte, 32)
	n, _ := f.Read(head)
	nl := strings.IndexByte(string(head[:n]), '\n')
	if nl <= 0 {
		return true
	}
	expires, err := strconv.ParseInt(string(head[:nl]), 10, 64)
	if err != nil {
		return true
	}
	return now.Unix() >= expires
}

// SweepEvery runs Sweep on a ticker until ctx ends. Started once from main, so the goroutine lives as
// long as the process and there is nothing to stop but the context.
func (c *TieredCache) SweepEvery(ctx context.Context, every time.Duration) {
	if c.dir == "" {
		return
	}
	// Once at startup: a redeploy is the most likely moment for the store to be holding a backlog of
	// entries that expired while the process was not running.
	if n := c.Sweep(); n > 0 {
		log.Printf("den-scout: swept %d expired cache entries at startup", n)
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := c.Sweep(); n > 0 {
				log.Printf("den-scout: swept %d expired cache entries", n)
			}
		}
	}
}

// path hashes the key: cache keys carry config blobs and ids, which are neither filename-safe nor short.
func (c *TieredCache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, base64.RawURLEncoding.EncodeToString(sum[:])+".ent")
}

// Persistent reports whether the durable tier is still writing. The failure it exposes is silent by
// design — one log line and then memory keeps serving — so without a way to ask, an operator learns that
// persistence died only by noticing probes never survive a redeploy.
func (c *TieredCache) Persistent() bool { return !c.disabled() }

func (c *TieredCache) disabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.off
}

// disable reports the first failure and stops trying. Repeating it per entry would fill the log with the
// same fact.
func (c *TieredCache) disable(op string, err error) {
	c.mu.Lock()
	c.off = true
	c.mu.Unlock()
	c.once.Do(func() {
		log.Printf("den-scout: cache %s failed (%v) — persistence disabled, memory still serving", op, err)
	})
}

// writeFile is a test seam kept next to the cache so the unwritable-directory case can be built without
// importing os into the test.
func writeFile(path, body string) error { return os.WriteFile(path, []byte(body), 0o644) }
