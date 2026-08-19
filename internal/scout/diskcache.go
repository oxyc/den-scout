package scout

import (
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
	nl := strings.IndexByte(string(raw), '\n')
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

// path hashes the key: cache keys carry config blobs and ids, which are neither filename-safe nor short.
func (c *TieredCache) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, base64.RawURLEncoding.EncodeToString(sum[:])+".ent")
}

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
