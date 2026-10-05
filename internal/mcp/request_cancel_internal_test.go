package mcp

import (
	"context"
	"encoding/json"
	"io"
	"testing"
)

// TestReportProgress_SilentOnceCancelled: a client that has cancelled a call
// has dropped its progress token, so a later notification would only be logged
// there as one for an unknown token. Progress before the cancel still goes out
// (the control).
func TestReportProgress_SilentOnceCancelled(t *testing.T) {
	ss := newServeState(New(ServerInfo{Name: "t", Version: "0"}), io.Discard)
	var sent []string
	notify := func(method string, params any) error {
		m, _ := params.(map[string]any)["message"].(string)
		sent = append(sent, m)
		return nil
	}
	ctx, untrack := ss.trackRequest(context.Background(), float64(5))
	defer untrack()
	ctx, closeProgress := withProgress(withNotifier(ctx, notify), map[string]json.RawMessage{"progressToken": json.RawMessage(`"t"`)})
	defer closeProgress()

	ReportProgress(ctx, 1, 2, "before")
	ss.cancelRequest(cancelFrame("5"))
	ReportProgress(ctx, 2, 2, "after")
	if len(sent) != 1 || sent[0] != "before" {
		t.Fatalf("sent %v, want only the notification from before the cancel", sent)
	}
}

func cancelFrame(id string) []byte {
	return []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + id + `}}`)
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
