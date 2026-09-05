package mcp

// server_serve.go — the serve loop: reading framed messages off the transport,
// dispatching each one, and writing responses back under the write deadline.
//
// Split from server.go, which owns the Server type, its registry and its
// configuration. This file owns the per-connection RUN, and the state that only
// exists while a connection is being served (serveState).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

type deadlineWriter interface {
	SetWriteDeadline(time.Time) error
}

// serveState holds the mutable per-Serve-call state shared across the scan
// goroutine, request dispatcher, and response writer.
//
// Concurrency: enc/wd are written through wrMu; broken is read and written only
// under wrMu. cancel is set once before any goroutine starts and only read
// afterwards.
type serveState struct {
	s            *Server
	enc          *json.Encoder
	wd           deadlineWriter // nil when the transport has no SetWriteDeadline
	writeTimeout time.Duration
	cancel       context.CancelFunc // tears the connection down on a fatal write error
	wrMu         sync.Mutex
	broken       bool // a write failed; further writes are no-ops (guarded by wrMu)
	wg           sync.WaitGroup
}

func newServeState(s *Server, w io.Writer) *serveState {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	ss := &serveState{s: s, enc: enc, writeTimeout: s.WriteTimeout}
	if dw, ok := w.(deadlineWriter); ok {
		ss.wd = dw
	}
	return ss
}

// encode writes one message, bounding the write with a deadline when the
// transport supports it. Caller must hold wrMu. The deadline is cleared after
// the write so an idle connection between replies carries none.
func (ss *serveState) encode(v any) error {
	if ss.wd != nil && ss.writeTimeout > 0 {
		_ = ss.wd.SetWriteDeadline(time.Now().Add(ss.writeTimeout))
		defer func() { _ = ss.wd.SetWriteDeadline(time.Time{}) }()
	}
	return ss.enc.Encode(v)
}

// fail marks the connection broken and cancels Serve. A write error on the
// socket (including a write-deadline timeout) is not recoverable for this
// connection: tearing it down lets the resilient proxy reconnect, where leaving
// it up would wedge wrMu and hang every later reply. Caller must hold wrMu.
//
// A lapsed write deadline is the wedge this guards against, so it logs at WARN.
// Any other write error means the client/proxy disconnected mid-write (broken
// pipe, reset, EOF) — expected churn, logged at Debug so it does not drown the
// log on every routine disconnect.
func (ss *serveState) fail(err error) {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		slog.Warn("mcp: response write timed out — closing connection", "err", err, "timeout", ss.writeTimeout)
	} else {
		slog.Debug("mcp: write failed — closing connection", "err", err)
	}
	ss.broken = true
	if ss.cancel != nil {
		ss.cancel()
	}
}

func (ss *serveState) write(resp mcpResponse) {
	ss.wrMu.Lock()
	defer ss.wrMu.Unlock()
	if ss.broken {
		return
	}
	if err := ss.encode(resp); err != nil {
		ss.fail(err)
	}
}

// dispatchMessage handles one inbound message in a wg.Go goroutine.
func (ss *serveState) dispatchMessage(ctx context.Context, data []byte, initOnce *sync.Once) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mcp: handler panic", "err", r)
			// Best-effort: try to send an error response so the client
			// doesn't hang waiting for a reply that will never come.
			var req mcpRequest
			if json.Unmarshal(data, &req) == nil && req.ID != nil {
				ss.write(errResp(req.ID, -32603, fmt.Sprintf("internal error: %v", r)))
			}
		}
	}()

	// Peek at method before full handling (needed for post-init hook).
	var peek struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(data, &peek)

	resp, isRequest := ss.s.handle(ctx, data)
	if !isRequest {
		if peek.Method == "notifications/roots/listChanged" && ss.s.OnRootsChanged != nil {
			go safeRun("OnRootsChanged", func() { ss.s.OnRootsChanged(ctx, ss.makeRequest) })
		}
		return
	}
	ss.write(resp)

	if peek.Method == "initialize" && resp.Error == nil {
		initOnce.Do(func() {
			// The negotiation hook fires inside the once-guard so a client that
			// re-sends initialize cannot double-record (or double-log) it.
			// Synchronous, before OnInit. The parse is repeated here rather than
			// plumbed out of handleInitialize: it is one unmarshal, and the
			// negotiation is a pure function of the same params.
			if ss.s.OnProtocolNegotiated != nil {
				var initReq mcpRequest
				if json.Unmarshal(data, &initReq) == nil {
					offered, clientCaps := clientProtocolParams(initReq.Params)
					ss.s.OnProtocolNegotiated(ctx, offered, negotiateProtocolVersion(offered), clientCaps)
				}
			}
			if ss.s.OnInit != nil {
				go safeRun("OnInit", func() { ss.s.OnInit(ctx, ss.makeRequest, ss.notify) })
			}
		})
	}
}

// startScanGoroutine spawns the reader goroutine and returns a channel that
// delivers one scanLine per inbound message until the reader is exhausted or
// ctx is cancelled.
func startScanGoroutine(ctx context.Context, reader *bufio.Reader) <-chan scanLine {
	ch := make(chan scanLine)
	go func() {
		defer close(ch)
		for {
			b, tooLarge, err := readMessageLine(reader, maxMessageBytes)
			if err != nil {
				if errors.Is(err, io.EOF) && len(b) == 0 && !tooLarge {
					return
				}
				select {
				case ch <- scanLine{data: b, err: err, tooLarge: tooLarge}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case ch <- scanLine{data: b, tooLarge: tooLarge}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// Serve reads newline-delimited JSON-RPC 2.0 messages from r and writes
// responses to w until r is exhausted or ctx is cancelled. Each request is
// handled concurrently; Serve waits for all in-flight handlers before returning.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	// A fatal write error cancels this derived context so the loop below returns
	// and the connection is torn down, rather than wedging on a held wrMu.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ss := newServeState(s, w)
	ss.cancel = cancel
	scanCh := startScanGoroutine(ctx, bufio.NewReader(r))
	var initOnce sync.Once

	for {
		select {
		case <-ctx.Done():
			ss.wg.Wait()
			return ctx.Err()
		case line, ok := <-scanCh:
			if !ok {
				ss.wg.Wait()
				return nil
			}
			data := line.data
			if line.tooLarge {
				ss.write(errResp(extractID(data), codeInvalidRequest, fmt.Sprintf("message exceeds %d byte limit", maxMessageBytes)))
				continue
			}
			if line.err != nil {
				ss.wg.Wait()
				return line.err
			}
			ss.wg.Go(func() { ss.dispatchMessage(ctx, data, &initOnce) })
		}
	}
}

// safeRun calls f and recovers from any panic, logging it with a stack trace.
// Use for goroutines that must not crash the daemon (OnInit, OnRootsChanged, …).
