package mcp

import "context"

// connClosedCtxKey is the context key under which Serve publishes its
// connection-closed signal. Unexported, so nothing outside this package can
// forge one; the only reader is ConnectionClosed.
type connClosedCtxKey struct{}

// withConnectionClosed derives a request ctx carrying the connection's
// closed signal. Serve is the only caller.
func withConnectionClosed(ctx context.Context, closed <-chan struct{}) context.Context {
	return context.WithValue(ctx, connClosedCtxKey{}, closed)
}

// ConnectionClosed returns a channel that is closed once the connection a
// request arrived on has stopped delivering requests — its reader hit EOF or
// failed — or Serve itself is ending. It returns nil for a ctx that did not
// come from Serve (tests, in-process nested calls); a nil channel never fires in
// a select, so a watcher behaves exactly as it did without one.
//
// Serve waits for in-flight handlers before returning, and leaves their ctx
// live: a request already accepted is not cancelled because its client went
// away. That is right for a quick call, but a tool that holds a daemon-wide
// resource for minutes (mutation_test's single run slot) must not keep holding
// it for a client that can never read the result. Such a tool watches this
// signal and cancels itself. In production an EOF here means the client is
// gone, not half-closed: the serve proxy closes its whole daemon socket when its
// own client closes stdin, and nothing in plumb half-closes a connection.
func ConnectionClosed(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(connClosedCtxKey{}).(<-chan struct{})
	return ch
}
