package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
)

// progress.go is the server side of notifications/progress for tools/call. A
// client that wants progress for a request puts a progressToken in the request's
// `_meta`; the server may then send notifications/progress carrying that token
// until it sends the response, and must send none without one.
//
// It matters most for a call that runs for minutes: Claude Code abandons a
// tools/call that has sent neither a response nor progress for its idle window
// (30 min by default), and a progress notification resets that window.

// notifierCtxKey carries the connection's NotifyFn to handleToolsCall, which
// builds the per-call reporter from it. Set only by Serve's dispatcher.
type notifierCtxKey struct{}

func withNotifier(ctx context.Context, notify NotifyFn) context.Context {
	return context.WithValue(ctx, notifierCtxKey{}, notify)
}

func notifierFrom(ctx context.Context) NotifyFn {
	n, _ := ctx.Value(notifierCtxKey{}).(NotifyFn)
	return n
}

// progressCtxKey carries one call's progressReporter.
type progressCtxKey struct{}

// progressReporter sends one call's progress notifications.
//
// Concurrency: mu serialises every send with close, so once close returns no
// notification for this token can still be in flight — none may follow the
// response. last enforces the spec's strictly increasing progress.
type progressReporter struct {
	token  json.RawMessage
	notify NotifyFn
	mu     sync.Mutex
	last   float64
	sent   bool
	closed bool
}

// withProgress attaches a reporter when the call asked for progress (a non-null
// progressToken in its `_meta`) and arrived through Serve. The returned close
// func must run before the response is written.
func withProgress(ctx context.Context, meta map[string]json.RawMessage) (context.Context, func()) {
	token := meta["progressToken"]
	notify := notifierFrom(ctx)
	if len(token) == 0 || string(token) == "null" || notify == nil {
		return ctx, func() {}
	}
	r := &progressReporter{token: token, notify: notify}
	return context.WithValue(ctx, progressCtxKey{}, r), r.close
}

func (r *progressReporter) close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

// HasProgress reports whether the client asked for progress on this call, so a
// tool can tell a call kept alive by ReportProgress from one that is not.
func HasProgress(ctx context.Context) bool {
	r, _ := ctx.Value(progressCtxKey{}).(*progressReporter)
	return r != nil
}

// ReportProgress sends a notifications/progress for the current call: progress
// so far, the total when known (0 omits it), and a short human-readable message
// ("" omits it). It is a no-op when the client asked for no progress, after the
// call has returned, and for a progress value that does not exceed the last one
// sent. A failed send is logged, never returned: progress is advisory and must
// not fail the work it describes.
func ReportProgress(ctx context.Context, progress, total float64, message string) {
	r, _ := ctx.Value(progressCtxKey{}).(*progressReporter)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || (r.sent && progress <= r.last) {
		return
	}
	params := map[string]any{"progressToken": r.token, "progress": progress}
	if total > 0 {
		params["total"] = total
	}
	if message != "" {
		params["message"] = message
	}
	if err := r.notify("notifications/progress", params); err != nil {
		slog.Debug("mcp: progress notification failed", "err", err)
		return
	}
	r.last, r.sent = progress, true
}
