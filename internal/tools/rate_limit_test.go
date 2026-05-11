package tools

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsUpToLimit(t *testing.T) {
	r := NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !r.Allow() {
			t.Fatalf("Allow #%d = false, want true", i+1)
		}
	}
	if r.Allow() {
		t.Fatal("Allow #4 should have been throttled")
	}
}

func TestRateLimiter_RecoversAfterWindow(t *testing.T) {
	r := NewRateLimiter(2, 50*time.Millisecond)
	if !r.Allow() {
		t.Fatal("Allow #1 = false")
	}
	if !r.Allow() {
		t.Fatal("Allow #2 = false")
	}
	if r.Allow() {
		t.Fatal("Allow #3 should have been throttled")
	}
	time.Sleep(60 * time.Millisecond)
	if !r.Allow() {
		t.Fatal("Allow after window expiry should succeed")
	}
}

func TestRateLimiter_ZeroLimit_Unlimited(t *testing.T) {
	r := NewRateLimiter(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !r.Allow() {
			t.Fatalf("Allow #%d on zero-limit limiter = false", i+1)
		}
	}
}

func TestRateLimiter_NilLimiter_Unlimited(t *testing.T) {
	var r *RateLimiter
	if !r.Allow() {
		t.Fatal("nil limiter should allow")
	}
}
