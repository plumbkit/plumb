// Package cache provides a session-scoped, sharded, TTL-aware cache for LSP
// query results.
//
// Key convention: callers prefix keys with the document URI so that
// InvalidateByPath can evict all results for a given file.
// Example: "file:///project/main.go:hover:10:5"
//
// Concurrency: all exported methods are safe for concurrent use.
package cache

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const numShards = 16

type entry struct {
	value     any
	expiresAt time.Time
}

type shard struct {
	mu      sync.RWMutex
	entries map[string]entry
}

// Stats reports cache health metrics.
type Stats struct {
	Size   int
	Hits   int64
	Misses int64
}

// Cache is a sharded TTL cache backed by in-memory maps.
// Concurrency: all exported methods are safe for concurrent use.
type Cache struct {
	shards [numShards]shard
	hits   atomic.Int64
	misses atomic.Int64
	stopCh chan struct{}
	once   sync.Once
}

// New creates a Cache. When cleanupInterval is positive it starts a background
// goroutine that proactively evicts expired entries every cleanupInterval. A
// non-positive interval disables proactive eviction — entries still expire
// lazily on Get, and Set clamps a non-positive TTL to one hour — which also
// avoids the time.NewTicker panic on a zero or negative interval (e.g. a
// misconfigured `[cache] ttl = "0s"` or a test pool that leaves cacheTTL unset).
// Call Close when done.
func New(cleanupInterval time.Duration) *Cache {
	c := &Cache{stopCh: make(chan struct{})}
	for i := range c.shards {
		c.shards[i].entries = make(map[string]entry)
	}
	if cleanupInterval > 0 {
		go c.cleanupLoop(cleanupInterval)
	}
	return c
}

// Get returns the value for key and true if present and not expired.
func (c *Cache) Get(key string) (any, bool) {
	s := c.shard(key)
	s.mu.RLock()
	e, ok := s.entries[key]
	s.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return e.value, true
}

// Set stores value under key with the given TTL. A zero TTL is treated as
// 1 hour to avoid accidental indefinite storage.
func (c *Cache) Set(key string, value any, ttl time.Duration) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	s := c.shard(key)
	s.mu.Lock()
	s.entries[key] = entry{value: value, expiresAt: time.Now().Add(ttl)}
	s.mu.Unlock()
}

// Delete removes key from the cache. It is a no-op if the key is absent.
func (c *Cache) Delete(key string) {
	s := c.shard(key)
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

// InvalidateByPath removes all entries whose key contains uri and returns the
// count of evicted entries. This is O(total entries) but only occurs on file
// change notifications.
func (c *Cache) InvalidateByPath(uri string) int {
	var count int
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		for k := range s.entries {
			if strings.Contains(k, uri) {
				delete(s.entries, k)
				count++
			}
		}
		s.mu.Unlock()
	}
	return count
}

// Stats returns a snapshot of hit/miss counters and the current live entry count.
func (c *Cache) Stats() Stats {
	now := time.Now()
	var size int
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.RLock()
		for _, e := range s.entries {
			if now.Before(e.expiresAt) {
				size++
			}
		}
		s.mu.RUnlock()
	}
	return Stats{
		Size:   size,
		Hits:   c.hits.Load(),
		Misses: c.misses.Load(),
	}
}

// Close stops the background cleanup goroutine.
func (c *Cache) Close() {
	c.once.Do(func() { close(c.stopCh) })
}

func (c *Cache) shard(key string) *shard {
	return &c.shards[shardIndex(key)]
}

func (c *Cache) cleanupLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.evictExpired()
		case <-c.stopCh:
			return
		}
	}
}

func (c *Cache) evictExpired() {
	now := time.Now()
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		for k, e := range s.entries {
			if now.After(e.expiresAt) {
				delete(s.entries, k)
			}
		}
		s.mu.Unlock()
	}
}

// shardIndex maps a key to a shard index using a simple polynomial hash.
func shardIndex(key string) int {
	var h uint32
	for i := range len(key) {
		h = h*31 + uint32(key[i])
	}
	return int(h % numShards)
}
