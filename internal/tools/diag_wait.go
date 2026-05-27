package tools

import (
	"sync"
	"time"
)

// DiagWaitEstimator adapts the post-write diagnostics wait to how quickly the
// language server actually re-publishes diagnostics for this connection.
//
// The configured post_write_diagnostics_ms is treated as a ceiling. Once the
// server's typical write→publish latency is known, the effective wait shrinks
// to a small multiple of it (bounded below by a floor), so a clean write — one
// the server never re-publishes for — stops paying the full ceiling waiting for
// a notification that will never arrive. This is the bulk of edit_file's
// steady-state latency.
//
// Tradeoff (latency vs. immediacy, never persistently wrong): if the server
// re-publishes a *changed* diagnostic set later than the shrunk window, that one
// write response shows the pre-write snapshot; the next diagnostics or edit call
// reflects the change. With no samples yet (cold start) the full ceiling is
// used, so a warming language server is never under-waited.
//
// Concurrency: safe for concurrent use; all state is mutex-guarded. A nil
// *DiagWaitEstimator is valid and behaves as "no adaptation" (window returns the
// ceiling, record is a no-op), so test setups can leave it unset.
type DiagWaitEstimator struct {
	mu    sync.Mutex
	ewma  time.Duration
	count int
}

// NewDiagWaitEstimator returns a ready-to-use estimator with no samples.
func NewDiagWaitEstimator() *DiagWaitEstimator { return &DiagWaitEstimator{} }

const (
	diagWaitAlpha = 0.3                   // EWMA smoothing factor for new samples
	diagWaitK     = 3                     // effective window = K × typical publish latency
	diagWaitFloor = 75 * time.Millisecond // never wait less than this once adapting
)

// record folds an observed write→publish latency into the EWMA. nil-safe;
// non-positive samples are ignored.
func (e *DiagWaitEstimator) record(d time.Duration) {
	if e == nil || d <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.count == 0 {
		e.ewma = d
	} else {
		e.ewma = time.Duration(diagWaitAlpha*float64(d) + (1-diagWaitAlpha)*float64(e.ewma))
	}
	e.count++
}

// window returns the effective wait for a write, bounded by [floor, ceiling].
// Before any sample is recorded it returns the ceiling unchanged. nil-safe.
func (e *DiagWaitEstimator) window(ceiling time.Duration) time.Duration {
	if e == nil {
		return ceiling
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.count == 0 {
		return ceiling
	}
	w := diagWaitK * e.ewma
	if w < diagWaitFloor {
		w = diagWaitFloor
	}
	if w > ceiling {
		w = ceiling
	}
	return w
}
