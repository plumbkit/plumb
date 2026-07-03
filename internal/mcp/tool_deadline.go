package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ExecTimeoutBounded is an optional Tool capability. A tool that implements it
// is run under the server's per-tool execution deadline (ToolExecTimeout): the
// dispatcher offloads Execute to a child goroutine and returns a fast, actionable
// error if the deadline elapses, so a blocking filesystem syscall on a slow or
// unresponsive mount cannot hang the call until the MCP client's own multi-minute
// timeout. Tools that already self-bound — the LSP tools, search_in_files,
// find_files — do NOT implement this; they manage their own deadline internally.
// The marker method carries no behaviour; its presence is the opt-in.
type ExecTimeoutBounded interface {
	ExecTimeoutBounded()
}

// execTool runs t.Execute, bounding it with s.ToolExecTimeout when t opts in via
// ExecTimeoutBounded. The bound is applied only when the timeout is positive and
// ctx does not already carry a deadline (a caller-supplied deadline already
// bounds the work). A bounded call runs on a child goroutine so a wedged syscall
// cannot hold the dispatcher.
//
// The timer is the authoritative "deadline exceeded" signal: when it fires first
// execTool returns the actionable message and cancels execCtx so a ctx-honouring
// tool unwinds (a blocked syscall is orphaned and reaps when it returns). It
// deliberately does NOT then read the tool's own result — a ctx-honouring tool
// propagates a bare "context deadline exceeded", and surfacing that instead of
// the actionable text would defeat the purpose (and races the timer under load).
func (s *Server) execTool(ctx context.Context, t Tool, name string, args json.RawMessage) (string, error) {
	if _, ok := t.(ExecTimeoutBounded); !ok || s.ToolExecTimeout <= 0 {
		return t.Execute(ctx, args)
	}
	if _, ok := ctx.Deadline(); ok {
		return t.Execute(ctx, args)
	}

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := t.Execute(execCtx, args)
		done <- result{text, err}
	}()

	timer := time.NewTimer(s.ToolExecTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.text, r.err
	case <-timer.C:
		cancel() // unwind a ctx-honouring tool; a blocked syscall is orphaned
		return "", fmt.Errorf("%s: exceeded its %s execution deadline — the target path may be "+
			"on a slow or unresponsive filesystem (a stalled network, iCloud, or FUSE mount); "+
			"the call was abandoned. Raise PLUMB_TOOL_EXEC_TIMEOUT or check the mount", name, s.ToolExecTimeout)
	case <-ctx.Done():
		return "", ctx.Err() // parent cancelled (daemon shutdown / idle eviction)
	}
}
