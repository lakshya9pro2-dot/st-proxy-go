package proxy

import (
	"sync"
	"time"
)

// cacheEntry holds a cached value and its expiration time.
type cacheEntry struct {
	value     interface{}
	expiresAt time.Time
}

// MemoryCache provides a fast, thread-safe in-memory cache with TTL.
type MemoryCache struct {
	mu    sync.RWMutex
	items map[string]cacheEntry
}

// NewMemoryCache creates a new MemoryCache instance.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{
		items: make(map[string]cacheEntry),
	}
}

// Get retrieves a value from the cache if it exists and has not expired.
func (c *MemoryCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	entry, found := c.items[key]
	c.mu.RUnlock()

	if !found {
		return nil, false
	}

	if time.Now().After(entry.expiresAt) {
		// Lazily remove expired entry
		c.mu.Lock()
		delete(c.items, key)
		c.mu.Unlock()
		return nil, false
	}

	return entry.value, true
}

// Set stores a value in the cache with the specified TTL.
func (c *MemoryCache) Set(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items[key] = cacheEntry{
		value:     value,
		expiresAt: time.Now().Add(ttl),
	}
}

// Delete removes an entry from the cache.
func (c *MemoryCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// Clear removes all entries from the cache.
func (c *MemoryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]cacheEntry)
}

// call represents an in-flight or completed singleflight request.
type singleflightCall struct {
	wg  sync.WaitGroup
	val interface{}
	err error
}

// SingleflightGroup suppresses duplicate in-flight requests.
type SingleflightGroup struct {
	mu sync.Mutex
	m  map[string]*singleflightCall
}

// NewSingleflightGroup creates a new SingleflightGroup.
func NewSingleflightGroup() *SingleflightGroup {
	return &SingleflightGroup{
		m: make(map[string]*singleflightCall),
	}
}

// Do executes and returns the results of the given function, making
// sure that only one execution is in-flight for a given key at a time.
// If a duplicate comes in, the duplicate caller waits for the original
// to complete and receives the same results.
func (g *SingleflightGroup) Do(key string, fn func() (interface{}, error)) (interface{}, error) {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}

	c := new(singleflightCall)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}
