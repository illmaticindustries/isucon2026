package isureal

import (
	"container/list"
	"sync"
	"time"
)

// Cache only immutable bytes. Authorization, owner status, image validity and
// expiry are checked on every request using the database, never this cache.
type imageBodyKey struct {
	ID        string
	CreatedAt time.Time
	OwnerKey  string
}
type cachedImageBody struct {
	Regional  bool
	Data      []byte
	MediaType string
}
type imageBodyEntry struct {
	key  imageBodyKey
	body cachedImageBody
}
type imageBodyCache struct {
	mu                          sync.Mutex
	generation                  int64
	maxBytes, maxEntries, bytes int
	entries                     map[imageBodyKey]*list.Element
	lru                         *list.List
}

func newImageBodyCache(maxBytes, maxEntries int) *imageBodyCache {
	return &imageBodyCache{maxBytes: maxBytes, maxEntries: maxEntries, entries: make(map[imageBodyKey]*list.Element), lru: list.New()}
}

var imageBodies = newImageBodyCache(128<<20, 4096)

func (c *imageBodyCache) clearLocked() {
	clear(c.entries)
	c.lru.Init()
	c.bytes = 0
}
func (c *imageBodyCache) clear() { c.mu.Lock(); defer c.mu.Unlock(); c.clearLocked() }

// A database epoch increase makes all previous entries unreachable. Delayed
// reads from older snapshots must neither hit nor repopulate the current epoch.
func (c *imageBodyCache) epoch(generation int64) bool {
	if generation < c.generation {
		return false
	}
	if generation > c.generation {
		c.clearLocked()
		c.generation = generation
	}
	return true
}
func (c *imageBodyCache) get(generation int64, key imageBodyKey) (cachedImageBody, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.epoch(generation) {
		return cachedImageBody{}, false
	}
	e, ok := c.entries[key]
	if !ok {
		return cachedImageBody{}, false
	}
	c.lru.MoveToFront(e)
	return e.Value.(imageBodyEntry).body, true
}
func (c *imageBodyCache) put(generation int64, key imageBodyKey, body cachedImageBody) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.epoch(generation) || len(body.Data) > c.maxBytes || c.maxEntries <= 0 {
		return
	}
	if _, ok := c.entries[key]; ok {
		return
	}
	for c.bytes+len(body.Data) > c.maxBytes || len(c.entries) >= c.maxEntries {
		e := c.lru.Back()
		entry := e.Value.(imageBodyEntry)
		c.bytes -= len(entry.body.Data)
		delete(c.entries, entry.key)
		c.lru.Remove(e)
	}
	// Own the stored bytes; callers may reuse their database buffers afterwards.
	body.Data = append([]byte(nil), body.Data...)
	c.entries[key] = c.lru.PushFront(imageBodyEntry{key, body})
	c.bytes += len(body.Data)
	hintWarmImage(key.ID)
}
