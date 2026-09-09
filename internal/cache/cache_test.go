package cache_test

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
)

func newCache(t *testing.T) *cache.Cache {
	t.Helper()
	c := cache.New(time.Hour, 0) // slow cleanup and unbounded capacity so tests control expiry themselves
	t.Cleanup(c.Close)
	return c
}

func TestCache_GetSet(t *testing.T) {
	c := newCache(t)
	c.Set("key1", "value1", time.Minute)

	got, ok := c.Get("key1")
	if !ok {
		t.Fatal("expected hit")
	}
	if got != "value1" {
		t.Fatalf("got %v, want value1", got)
	}
}

func TestCache_Miss(t *testing.T) {
	c := newCache(t)
	_, ok := c.Get("nonexistent")
	if ok {
		t.Fatal("expected miss for absent key")
	}
}

func TestCache_Expiry(t *testing.T) {
	c := newCache(t)
	c.Set("short", "val", time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	_, ok := c.Get("short")
	if ok {
		t.Fatal("expected expired entry to miss")
	}
}

func TestCache_Delete(t *testing.T) {
	c := newCache(t)
	c.Set("del", "v", time.Minute)
	c.Delete("del")

	_, ok := c.Get("del")
	if ok {
		t.Fatal("expected miss after delete")
	}
}

func TestCache_InvalidateByPath(t *testing.T) {
	c := newCache(t)
	uri := "file:///project/main.go"
	other := "file:///project/other.go"

	c.Set(uri+":hover:1:2", "h", time.Minute)
	c.Set(uri+":def:3:4", "d", time.Minute)
	c.Set(other+":hover:1:2", "x", time.Minute)

	n := c.InvalidateByPath(uri)
	if n != 2 {
		t.Fatalf("want 2 evictions, got %d", n)
	}
	if _, ok := c.Get(uri + ":hover:1:2"); ok {
		t.Fatal("evicted entry should miss")
	}
	if _, ok := c.Get(uri + ":def:3:4"); ok {
		t.Fatal("evicted entry should miss")
	}
	if _, ok := c.Get(other + ":hover:1:2"); !ok {
		t.Fatal("unrelated entry should survive")
	}
}

func TestCache_Stats(t *testing.T) {
	c := newCache(t)
	c.Set("k", "v", time.Minute)
	c.Get("k")    // hit
	c.Get("k")    // hit
	c.Get("miss") // miss

	stats := c.Stats()
	if stats.Hits != 2 {
		t.Fatalf("hits: got %d, want 2", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Fatalf("misses: got %d, want 1", stats.Misses)
	}
	if stats.Size != 1 {
		t.Fatalf("size: got %d, want 1", stats.Size)
	}
	if stats.MaxSize != 0 {
		t.Fatalf("maxSize: got %d, want 0", stats.MaxSize)
	}
}

func TestCache_ZeroTTL_treatedAsOneHour(t *testing.T) {
	c := newCache(t)
	c.Set("z", "v", 0)

	_, ok := c.Get("z")
	if !ok {
		t.Fatal("zero TTL should store for one hour, not immediately expire")
	}
}

func TestCache_Concurrent(t *testing.T) {
	c := newCache(t)
	const workers = 50
	const opsPerWorker = 100

	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range opsPerWorker {
				key := fmt.Sprintf("key-%d-%d", id, j)
				c.Set(key, j, time.Minute)
				c.Get(key)
				if j%10 == 0 {
					c.Delete(key)
				}
			}
		}(i)
	}
	wg.Wait()
	// No race — verified by go test -race.
}

// TestNew_NonPositiveInterval_NoPanic guards the cleanup-goroutine fix: a zero
// or negative cleanup interval must not panic (time.NewTicker panics on a
// non-positive interval from the background goroutine, which would crash the
// whole process). A misconfigured `[cache] ttl = "0s"` or a test pool that
// leaves cacheTTL unset reaches cache.New with 0; lazy expiry on Get still
// works without the proactive cleanup loop.
func TestNew_NonPositiveInterval_NoPanic(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		c := cache.New(interval, 0) // must not panic
		c.Set("k", "v", time.Minute)
		if got, ok := c.Get("k"); !ok || got != "v" {
			t.Fatalf("interval %v: Get = (%v, %v), want (v, true)", interval, got, ok)
		}
		c.Close()
	}
}

func TestCache_MaxSize_Unbounded(t *testing.T) {
	c := cache.New(time.Hour, 0)
	defer c.Close()

	for i := range 500 {
		c.Set(fmt.Sprintf("item-%d", i), i, time.Hour)
	}
	if stats := c.Stats(); stats.Size != 500 {
		t.Fatalf("unbounded cache should retain all items; got size %d, want 500", stats.Size)
	}
}

// shardCollisions finds n distinct keys that map to the same shard index in a 16-shard Cache.
func shardCollisions(n int) []string {
	// Simple polynomial hash matching shardIndex: h = h*31 + byte
	shardOf := func(k string) int {
		var h uint32
		for i := range len(k) {
			h = h*31 + uint32(k[i])
		}
		return int(h % 16)
	}
	targetShard := 0
	var res []string
	for i := 0; len(res) < n; i++ {
		candidate := fmt.Sprintf("k-shard-test-%d", i)
		if shardOf(candidate) == targetShard {
			res = append(res, candidate)
		}
	}
	return res
}

func TestCache_MaxSize_PerShardBudget(t *testing.T) {
	keys := shardCollisions(2)
	k1, k2 := keys[0], keys[1]

	// maxSize = 16 => shardBudget = ceil(16/16) = 1
	c := cache.New(time.Hour, 16)
	defer c.Close()

	c.Set(k1, "v1", time.Hour)
	if got, ok := c.Get(k1); !ok || got != "v1" {
		t.Fatalf("expected k1 hit, got (%v, %v)", got, ok)
	}

	// Setting k2 in the same shard should evict k1
	c.Set(k2, "v2", time.Hour)
	if _, ok := c.Get(k1); ok {
		t.Fatal("expected k1 to be evicted when shard budget of 1 is reached")
	}
	if got, ok := c.Get(k2); !ok || got != "v2" {
		t.Fatalf("expected k2 hit, got (%v, %v)", got, ok)
	}

	// Updating k2 in place should keep size at 1 and not evict k2
	c.Set(k2, "v2-updated", time.Hour)
	if got, ok := c.Get(k2); !ok || got != "v2-updated" {
		t.Fatalf("expected k2 update hit, got (%v, %v)", got, ok)
	}
	if stats := c.Stats(); stats.Size != 1 {
		t.Fatalf("expected size 1 after update, got %d", stats.Size)
	}
}

func TestCache_MaxSize_ExpiredFirst(t *testing.T) {
	keys := shardCollisions(3)
	k1, k2, k3 := keys[0], keys[1], keys[2]

	// maxSize = 32 => shardBudget = ceil(32/16) = 2
	c := cache.New(time.Hour, 32)
	defer c.Close()

	// k1 has short TTL; k2 has long TTL
	c.Set(k1, "v1", 10*time.Millisecond)
	c.Set(k2, "v2", time.Hour)

	time.Sleep(20 * time.Millisecond) // k1 expires

	// Adding k3 when k1 is expired: eviction should prune expired k1, retaining k2
	c.Set(k3, "v3", time.Hour)

	if _, ok := c.Get(k1); ok {
		t.Fatal("expired k1 should not be in cache")
	}
	if got, ok := c.Get(k2); !ok || got != "v2" {
		t.Fatalf("k2 should survive because expired k1 is evicted first; got (%v, %v)", got, ok)
	}
	if got, ok := c.Get(k3); !ok || got != "v3" {
		t.Fatalf("k3 should be stored; got (%v, %v)", got, ok)
	}
}

func TestCache_MaxSize_LRURecency(t *testing.T) {
	keys := shardCollisions(3)
	k1, k2, k3 := keys[0], keys[1], keys[2]

	// maxSize = 32 => shardBudget = 2
	c := cache.New(time.Hour, 32)
	defer c.Close()

	c.Set(k1, "v1", time.Hour)
	time.Sleep(2 * time.Millisecond)
	c.Set(k2, "v2", time.Hour)
	time.Sleep(2 * time.Millisecond)

	// k1 is oldest accessed; setting k3 should evict k1
	c.Set(k3, "v3", time.Hour)

	if _, ok := c.Get(k1); ok {
		t.Fatal("k1 should be evicted as oldest access")
	}
	if _, ok := c.Get(k2); !ok {
		t.Fatal("k2 should survive")
	}
	if _, ok := c.Get(k3); !ok {
		t.Fatal("k3 should survive")
	}
}

func TestCache_MaxSize_GetUpdatesRecency(t *testing.T) {
	keys := shardCollisions(3)
	k1, k2, k3 := keys[0], keys[1], keys[2]

	// maxSize = 32 => shardBudget = 2
	c := cache.New(time.Hour, 32)
	defer c.Close()

	c.Set(k1, "v1", time.Hour)
	time.Sleep(2 * time.Millisecond)
	c.Set(k2, "v2", time.Hour)
	time.Sleep(2 * time.Millisecond)

	// Get(k1) updates its lastAccess
	if _, ok := c.Get(k1); !ok {
		t.Fatal("expected k1 hit")
	}
	time.Sleep(2 * time.Millisecond)

	// Now k2 has the oldest lastAccess; setting k3 should evict k2, retaining k1 and k3
	c.Set(k3, "v3", time.Hour)

	if _, ok := c.Get(k2); ok {
		t.Fatal("k2 should be evicted because k1's recency was updated by Get")
	}
	if _, ok := c.Get(k1); !ok {
		t.Fatal("k1 should survive")
	}
	if _, ok := c.Get(k3); !ok {
		t.Fatal("k3 should survive")
	}
}

func TestCache_MaxSize_ConcurrentSingleShardOverfill(t *testing.T) {
	const (
		maxSize      = 64
		budget       = (maxSize + 16 - 1) / 16 // ceil(64/16) = 4
		workers      = 10
		readers      = 10
		opsPerWorker = 200
		numKeys      = 50
	)
	keys := shardCollisions(numKeys)

	c := cache.New(time.Hour, maxSize)
	defer c.Close()

	if stats := c.Stats(); stats.MaxSize != maxSize {
		t.Fatalf("stats.MaxSize: got %d, want %d", stats.MaxSize, maxSize)
	}

	stopSampler := make(chan struct{})
	var sampleWg sync.WaitGroup
	sampleWg.Add(1)
	go func() {
		defer sampleWg.Done()
		for {
			select {
			case <-stopSampler:
				return
			default:
			}
			if sz := c.Stats().Size; sz > budget {
				t.Errorf("in-flight cache size %d exceeded single-shard budget %d", sz, budget)
				return
			}
			runtime.Gosched()
		}
	}()

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range opsPerWorker {
				key := keys[(worker*opsPerWorker+i)%len(keys)]
				c.Set(key, fmt.Sprintf("val-%d-%d", worker, i), time.Hour)
			}
		}(w)
	}

	for r := range readers {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			for i := range opsPerWorker {
				key := keys[(reader*opsPerWorker+i)%len(keys)]
				c.Get(key)
			}
		}(r)
	}

	wg.Wait()
	close(stopSampler)
	sampleWg.Wait()

	if sz := c.Stats().Size; sz > budget {
		t.Fatalf("final cache size %d exceeded single-shard budget %d", sz, budget)
	}
}
