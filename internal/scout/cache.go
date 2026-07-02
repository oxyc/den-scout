package scout

import (
	"container/list"
	"sync"
	"time"
)

// Cache seam — stream list + per-hash truth. The default backend is an in-memory TTL + LRU cache
// bounded by BYTES (audit #1: a count cap let a large resultCap × many titles blow the heap and OOM
// the container). Thread-safe.
type Cache interface {
	Get(key string) (string, bool)
	Put(key, value string, ttl time.Duration)
}

type cacheEntry struct {
	key     string
	value   string
	expires time.Time
	size    int
}

type MemoryCache struct {
	mu       sync.Mutex
	ll       *list.List // front = most-recently-used
	items    map[string]*list.Element
	bytes    int
	maxBytes int
	now      func() time.Time
}

func NewMemoryCache(maxBytes int) *MemoryCache {
	return &MemoryCache{
		ll:       list.New(),
		items:    make(map[string]*list.Element),
		maxBytes: maxBytes,
		now:      time.Now,
	}
}

func (c *MemoryCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	e := el.Value.(*cacheEntry)
	if c.now().After(e.expires) {
		c.remove(el)
		return "", false
	}
	c.ll.MoveToFront(el) // LRU touch
	return e.value, true
}

func (c *MemoryCache) Put(key, value string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := len(key) + len(value)
	if el, ok := c.items[key]; ok {
		e := el.Value.(*cacheEntry)
		c.bytes += size - e.size
		e.value, e.size, e.expires = value, size, c.now().Add(ttl)
		c.ll.MoveToFront(el)
	} else {
		e := &cacheEntry{key: key, value: value, size: size, expires: c.now().Add(ttl)}
		c.items[key] = c.ll.PushFront(e)
		c.bytes += size
	}
	for c.bytes > c.maxBytes && c.ll.Len() > 0 {
		c.remove(c.ll.Back())
	}
}

func (c *MemoryCache) remove(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.bytes -= e.size
}
