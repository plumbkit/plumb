package mcp

import (
	"context"
	"io"
	"testing"
)

func cancelFrame(id string) []byte {
	return []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + id + `}}`)
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestCancelRequest_DuplicateCancelIsHarmless: a client may send the same
// cancel twice. Called directly, outside dispatchMessage's recover, so a
// double close would fail the test instead of being swallowed as a logged panic.
func TestCancelRequest_DuplicateCancelIsHarmless(t *testing.T) {
	ss := newServeState(New(ServerInfo{Name: "t", Version: "0"}), io.Discard)
	ctx, untrack := ss.trackRequest(context.Background(), float64(9))
	defer untrack()
	ss.cancelRequest(cancelFrame("9"))
	ss.cancelRequest(cancelFrame("9"))
	if !isClosed(RequestCancelled(ctx)) {
		t.Fatal("the cancelled request's signal never fired")
	}
}

// TestTrackRequest_ReusedIDKeepsTheNewerEntry: a client that reuses an id while
// the first request runs must still be able to cancel the second, so the first
// request finishing must not untrack the second's entry. And once both finish
// nothing is left behind.
func TestTrackRequest_ReusedIDKeepsTheNewerEntry(t *testing.T) {
	ss := newServeState(New(ServerInfo{Name: "t", Version: "0"}), io.Discard)
	_, untrackFirst := ss.trackRequest(context.Background(), float64(7))
	second, untrackSecond := ss.trackRequest(context.Background(), float64(7))
	untrackFirst()
	ss.cancelRequest(cancelFrame("7"))
	if !isClosed(RequestCancelled(second)) {
		t.Fatal("the first request's untrack removed the second's entry, so its cancel was lost")
	}
	untrackSecond()

	_, untrack := ss.trackRequest(context.Background(), "x")
	untrack()
	if n := len(ss.inflight); n != 0 {
		t.Fatalf("%d entries left in flight after every request finished", n)
	}
}
