package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
)

// request_cancel.go is the server side of notifications/cancelled: the client
// telling us it has abandoned one request it sent, by that request's id.
//
// A client abandons a CALL far more often than it closes its connection: Claude
// Code sends this when the user interrupts a running tool, or when the SDK's own
// request timeout fires, and keeps the connection open. On a shared serve
// connection every subagent's calls ride one connection that outlives all of
// them. So ConnectionClosed alone cannot tell a long-running tool that nobody is
// waiting for its answer any more — this signal can. (Claude Code's 30-minute
// IDLE abort sends no cancel: it just stops waiting. Progress is what prevents
// that one; see progress.go.)
//
// Like ConnectionClosed it is OPT-IN: the request ctx is not cancelled, a tool
// that holds something worth giving back watches RequestCancelled and decides.
// Cancelling the ctx itself would reach every tool, and a tool that hands its
// ctx to work meant to outlive the call would lose that work to a cancel the
// spec only asks it to honour on a best-effort basis.

// requestCancelledCtxKey is the context key under which dispatchMessage
// publishes a request's cancelled signal. Unexported, so only this package can
// set one.
type requestCancelledCtxKey struct{}

// RequestCancelled returns a channel that is closed once the client has sent
// notifications/cancelled for the request ctx belongs to. It returns nil for a
// ctx that did not come from Serve; a nil channel never fires in a select.
func RequestCancelled(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(requestCancelledCtxKey{}).(chan struct{})
	return ch
}

// requestKey renders a JSON-RPC id as a map key. The id and a cancel's
// requestId decode through the same `any` path (a number becomes float64, a
// string stays a string), so re-marshalling both yields the same key, and `1`
// and `"1"` stay distinct as the spec requires.
func requestKey(id any) string {
	b, err := json.Marshal(id)
	if err != nil {
		return ""
	}
	return string(b)
}

// trackRequest registers an in-flight request and returns its ctx carrying the
// cancelled signal, plus the untrack func the dispatcher defers. A client that
// reuses an id while the first request is still running replaces the entry; the
// first request's untrack then leaves the newer entry alone.
func (ss *serveState) trackRequest(ctx context.Context, id any) (context.Context, func()) {
	key := requestKey(id)
	if key == "" {
		return ctx, func() {}
	}
	ch := make(chan struct{})
	ss.inflightMu.Lock()
	if ss.inflight == nil {
		ss.inflight = make(map[string]chan struct{})
	}
	ss.inflight[key] = ch
	ss.inflightMu.Unlock()
	untrack := func() {
		ss.inflightMu.Lock()
		if ss.inflight[key] == ch {
			delete(ss.inflight, key)
		}
		ss.inflightMu.Unlock()
	}
	return context.WithValue(ctx, requestCancelledCtxKey{}, ch), untrack
}

// cancelRequest services one notifications/cancelled. The entry is deleted
// under the lock before its channel is closed, so a duplicate cancel finds
// nothing and the channel is closed exactly once. A cancel for an id that has
// already finished, or was never seen, is ignored, as the spec requires.
//
// The cancelled request is still answered when its handler returns, although
// the spec says a receiver SHOULD NOT respond. That is deliberate: the serve
// proxy forgets a request only when the daemon answers it, so an unanswered
// one would sit in its outstanding set and be error-synthesised on the next
// reconnect. A client that has forgotten the id drops the answer.
func (ss *serveState) cancelRequest(data []byte) {
	var n struct {
		Params struct {
			RequestID any    `json:"requestId"`
			Reason    string `json:"reason"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &n); err != nil || n.Params.RequestID == nil {
		return
	}
	key := requestKey(n.Params.RequestID)
	ss.inflightMu.Lock()
	ch := ss.inflight[key]
	delete(ss.inflight, key)
	ss.inflightMu.Unlock()
	if ch == nil {
		slog.Debug("mcp: cancel for a request not in flight", "request_id", key)
		return
	}
	slog.Info("mcp: client cancelled a request", "request_id", key, "reason", n.Params.Reason)
	close(ch)
}
