package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// connWatchTool blocks until the connection that issued the call closes, and
// reports what it saw. started is closed once Execute is running, so the test
// can close the connection while the call is genuinely in flight.
type connWatchTool struct {
	started chan struct{}
	saw     chan string
}

func (*connWatchTool) Name() string                 { return "conn_watch" }
func (*connWatchTool) Description() string          { return "waits for its connection to close" }
func (*connWatchTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (c *connWatchTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	gone := mcp.ConnectionClosed(ctx)
	close(c.started)
	select {
	case <-gone:
		c.saw <- "closed"
	case <-time.After(10 * time.Second):
		c.saw <- "still open after 10s"
	}
	return "", nil
}

// TestConnectionClosed_FiresWhenTheClientStopsSending pins the signal a
// long-running tool (mutation_test) uses to give up a daemon-wide resource when
// the agent that asked for it has gone. Serve waits for in-flight handlers
// before it returns and leaves their ctx live, so without this signal a call
// whose client crashed runs to completion — for a mutation run, hours of holding
// a slot nobody will ever read the result of.
func TestConnectionClosed_FiresWhenTheClientStopsSending(t *testing.T) {
	tool := &connWatchTool{started: make(chan struct{}), saw: make(chan string, 1)}
	s := mcp.New(mcp.ServerInfo{Name: "t", Version: "0"})
	s.Register(tool)

	pr, pw := io.Pipe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = s.Serve(context.Background(), pr, io.Discard)
	}()
	if _, err := io.WriteString(pw, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"conn_watch","arguments":{}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	<-tool.started

	// Positive control: while the client is still connected the signal must NOT
	// fire, or a tool watching it would abandon every call it is given.
	select {
	case got := <-tool.saw:
		t.Fatalf("the connection is still open, but the tool saw %q", got)
	case <-time.After(100 * time.Millisecond):
	}

	_ = pw.Close() // the client goes away mid-call
	select {
	case got := <-tool.saw:
		if got != "closed" {
			t.Fatalf("tool saw %q, want the connection reported closed", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tool never learned its connection had closed")
	}
	<-served
}

// TestConnectionClosed_NilOutsideServe: a ctx that did not come from Serve has
// no connection to watch. nil is the safe answer — a nil channel never fires in
// a select, so a tool called in-process (tests, nested calls) behaves exactly
// as it did before the signal existed.
func TestConnectionClosed_NilOutsideServe(t *testing.T) {
	if ch := mcp.ConnectionClosed(context.Background()); ch != nil {
		t.Fatalf("ConnectionClosed(background) = %v, want nil", ch)
	}
}
