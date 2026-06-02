package cache_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/cache"
)

func newCache(t *testing.T) *cache.Cache {
	t.Helper()
	c := cache.New(time.Hour) // slow cleanup so tests control expiry themselves
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
		c := cache.New(interval) // must not panic
		c.Set("k", "v", time.Minute)
		if got, ok := c.Get("k"); !ok || got != "v" {
			t.Fatalf("interval %v: Get = (%v, %v), want (v, true)", interval, got, ok)
		}
		c.Close()
	}
}
