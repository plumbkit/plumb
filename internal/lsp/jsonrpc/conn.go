// Package jsonrpc implements a JSON-RPC 2.0 client with LSP content framing
// (Content-Length headers over stdio).
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Caller abstracts the JSON-RPC connection so adapters can be tested with a
// mock without spawning a real process.
// Concurrency: all methods must be safe for concurrent use.
type Caller interface {
	Call(ctx context.Context, method string, params, result any) error
	Notify(ctx context.Context, method string, params any) error
	SetNotificationHandler(fn func(method string, params json.RawMessage))
	Close() error
}

// ─── wire types ──────────────────────────────────────────────────────────────

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *wireError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// ─── pending ─────────────────────────────────────────────────────────────────

type pending struct {
	ch  chan wireMessage
	ctx context.Context //nolint:containedctx
}

// ─── Conn ────────────────────────────────────────────────────────────────────

// Conn is a JSON-RPC 2.0 connection using LSP content framing.
// Concurrency: all exported methods are safe for concurrent use.
type Conn struct {
	wrMu   sync.Mutex
	writer io.Writer

	nextID  atomic.Int64
	pending sync.Map // int64 → *pending

	notifyMu sync.RWMutex
	onNotify func(method string, params json.RawMessage)

	done chan struct{}
	once sync.Once
}

// NewConn creates a Conn over r/w and starts the read loop.
// The caller owns the lifecycle of r and w; close them after calling Close.
func NewConn(r io.Reader, w io.Writer) *Conn {
	c := &Conn{
		writer: w,
		done:   make(chan struct{}),
	}
	go c.readLoop(bufio.NewReader(r))
	return c
}

// SetNotificationHandler registers fn to be called (in its own goroutine) for
// every server-initiated notification. Only one handler is active at a time.
func (c *Conn) SetNotificationHandler(fn func(method string, params json.RawMessage)) {
	c.notifyMu.Lock()
	c.onNotify = fn
	c.notifyMu.Unlock()
}

// Call sends a request and blocks until a response arrives or ctx is cancelled.
// result is JSON-decoded from the response; pass nil if not needed.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)

	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("jsonrpc call %s: marshaling params: %w", method, err)
	}

	ch := make(chan wireMessage, 1)
	c.pending.Store(id, &pending{ch: ch, ctx: ctx})
	defer c.pending.Delete(id)

	if err := c.send(wireMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  encoded,
	}); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("jsonrpc: connection closed")
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 && string(resp.Result) != "null" {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("jsonrpc call %s: decoding result: %w", method, err)
			}
		}
		return nil
	}
}

// Notify sends a notification (no ID, no response expected).
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("jsonrpc notify %s: marshaling params: %w", method, err)
	}
	_ = ctx // notifications don't block
	return c.send(wireMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  encoded,
	})
}

// Close signals the connection to stop. It does not close the underlying
// io.Reader/Writer — the caller is responsible for that.
func (c *Conn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *Conn) send(msg wireMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("jsonrpc: marshaling message: %w", err)
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))

	c.wrMu.Lock()
	defer c.wrMu.Unlock()
	if _, err := io.WriteString(c.writer, header); err != nil {
		return fmt.Errorf("jsonrpc: writing header: %w", err)
	}
	if _, err := c.writer.Write(data); err != nil {
		return fmt.Errorf("jsonrpc: writing body: %w", err)
	}
	return nil
}

func (c *Conn) readLoop(r *bufio.Reader) {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		msg, err := readMessage(r)
		if err != nil {
			// EOF or broken pipe — signal done so pending calls unblock.
			c.once.Do(func() { close(c.done) })
			return
		}
		c.dispatch(msg)
	}
}

func (c *Conn) dispatch(msg wireMessage) {
	if msg.ID != nil {
		if v, ok := c.pending.Load(*msg.ID); ok {
			p := v.(*pending)
			select {
			case p.ch <- msg:
			case <-p.ctx.Done():
			case <-c.done:
			}
		}
		return
	}
	// Server-initiated notification.
	if msg.Method == "" {
		return
	}
	c.notifyMu.RLock()
	fn := c.onNotify
	c.notifyMu.RUnlock()
	if fn != nil {
		go fn(msg.Method, msg.Params)
	}
}

// readMessage reads one LSP-framed JSON-RPC message from r.
func readMessage(r *bufio.Reader) (wireMessage, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return wireMessage{}, fmt.Errorf("reading header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			n, err := strconv.Atoi(strings.TrimPrefix(line, "Content-Length: "))
			if err != nil {
				return wireMessage{}, fmt.Errorf("parsing Content-Length: %w", err)
			}
			length = n
		}
	}
	if length < 0 {
		return wireMessage{}, fmt.Errorf("missing Content-Length header")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return wireMessage{}, fmt.Errorf("reading body: %w", err)
	}
	var msg wireMessage
	if err := json.Unmarshal(buf, &msg); err != nil {
		return wireMessage{}, fmt.Errorf("parsing message body: %w", err)
	}
	return msg, nil
}
