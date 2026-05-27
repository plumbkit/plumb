package tools

import (
	"testing"
	"time"
)

func TestDiagWaitEstimator_NoSamplesReturnsCeiling(t *testing.T) {
	e := NewDiagWaitEstimator()
	if got := e.window(300 * time.Millisecond); got != 300*time.Millisecond {
		t.Fatalf("window with no samples = %v, want ceiling 300ms", got)
	}
}

func TestDiagWaitEstimator_NilSafe(t *testing.T) {
	var e *DiagWaitEstimator
	e.record(50 * time.Millisecond) // must not panic
	if got := e.window(200 * time.Millisecond); got != 200*time.Millisecond {
		t.Fatalf("nil estimator window = %v, want ceiling 200ms", got)
	}
}

func TestDiagWaitEstimator_ShrinksTowardObservedLatency(t *testing.T) {
	e := NewDiagWaitEstimator()
	for range 20 {
		e.record(40 * time.Millisecond)
	}
	// EWMA of a constant 40ms is 40ms → window = 3×40 = 120ms, below the ceiling.
	got := e.window(300 * time.Millisecond)
	if got != 120*time.Millisecond {
		t.Fatalf("window = %v, want 120ms (3×40ms)", got)
	}
}

func TestDiagWaitEstimator_RespectsFloor(t *testing.T) {
	e := NewDiagWaitEstimator()
	for range 20 {
		e.record(5 * time.Millisecond) // 3×5 = 15ms is below the floor
	}
	if got := e.window(300 * time.Millisecond); got != diagWaitFloor {
		t.Fatalf("window = %v, want floor %v", got, diagWaitFloor)
	}
}

func TestDiagWaitEstimator_NeverExceedsCeiling(t *testing.T) {
	e := NewDiagWaitEstimator()
	for range 20 {
		e.record(500 * time.Millisecond) // 3×500 well over the ceiling
	}
	if got := e.window(200 * time.Millisecond); got != 200*time.Millisecond {
		t.Fatalf("window = %v, want clamped to ceiling 200ms", got)
	}
}
