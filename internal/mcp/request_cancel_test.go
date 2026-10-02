package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// pipedServer runs s.Serve over two pipes: send writes one client frame, and
// lines delivers every frame the server writes. Closing the client side ends
// Serve; the cleanup does that and waits for it.
func pipedServer(t *testing.T, s *mcp.Server) (send func(string), lines <-chan string) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = s.Serve(context.Background(), inR, outW)
		_ = outW.Close()
	}()
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		go func() { //nolint:revive // drain so a blocked server write can finish
			for range ch {
			}
		}()
		<-served
	})
	return func(frame string) {
		t.Helper()
		if _, err := io.WriteString(inW, frame+"\n"); err != nil {
			t.Fatal(err)
		}
	}, ch
}

// cancelWatchTool blocks until its call is cancelled and reports what it saw.
type cancelWatchTool struct {
	started chan struct{}
	saw     chan string
}

func (*cancelWatchTool) Name() string        { return "cancel_watch" }
func (*cancelWatchTool) Description() string { return "waits for its call to be cancelled" }

func (*cancelWatchTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (c *cancelWatchTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	cancelled := mcp.RequestCancelled(ctx)
	c.started <- struct{}{}
	select {
	case <-cancelled:
		c.saw <- "cancelled"
	case <-time.After(400 * time.Millisecond):
		c.saw <- "not cancelled"
	}
	return "done", nil
}

// TestRequestCancelled_FiresOnlyForTheCancelledRequest pins the signal that
// lets a long-running tool give up a call its client has abandoned without
// closing the connection — Claude Code dropping an idle tools/call on a shared
// serve connection, the case ConnectionClosed can never see. The controls
// matter as much as the hit: a cancel for another id, or for the same digits as
// a string id, must not fire, or one subagent's cancel would abandon another's
// call.
func TestRequestCancelled_FiresOnlyForTheCancelledRequest(t *testing.T) {
	tool := &cancelWatchTool{started: make(chan struct{}, 4), saw: make(chan string, 4)}
	s := mcp.New(mcp.ServerInfo{Name: "t", Version: "0"})
	s.Register(tool)
	send, _ := pipedServer(t, s)

	send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"cancel_watch","arguments":{}}}`)
	<-tool.started
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":8}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"7"}}`)
	if got := <-tool.saw; got != "not cancelled" {
		t.Fatalf("a cancel for another request reached this one: tool saw %q", got)
	}

	send(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"cancel_watch","arguments":{}}}`)
	<-tool.started
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":9,"reason":"idle timeout"}}`)
	if got := <-tool.saw; got != "cancelled" {
		t.Fatalf("tool saw %q, want its call reported cancelled", got)
	}
}

// TestRequestCancelled_NilOutsideServe: a ctx that did not come from Serve has
// no request to cancel; nil never fires in a select.
func TestRequestCancelled_NilOutsideServe(t *testing.T) {
	if ch := mcp.RequestCancelled(context.Background()); ch != nil {
		t.Fatalf("RequestCancelled(background) = %v, want nil", ch)
	}
}

// progressTool reports progress 1, 1 (a repeat the spec forbids), then 2, and
// hands its ctx out so the test can try to report after the call returned.
type progressTool struct{ ctx chan context.Context }

func (*progressTool) Name() string                 { return "progress" }
func (*progressTool) Description() string          { return "reports progress" }
func (*progressTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (p *progressTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	mcp.ReportProgress(ctx, 1, 2, "first")
	mcp.ReportProgress(ctx, 1, 2, "repeat")
	mcp.ReportProgress(ctx, 2, 2, "second")
	p.ctx <- ctx
	return "has-progress=" + map[bool]string{true: "yes", false: "no"}[mcp.HasProgress(ctx)], nil
}

// collectUntilResponse reads frames until the response to id arrives, returning
// the progress notifications seen before it.
func collectUntilResponse(t *testing.T, lines <-chan string, id string) (progress []map[string]any, response string) {
	t.Helper()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("server closed before responding")
			}
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params map[string]any  `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatalf("bad frame %q: %v", line, err)
			}
			if msg.Method == "notifications/progress" {
				progress = append(progress, msg.Params)
				continue
			}
			if string(msg.ID) == id {
				return progress, line
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no response within 5s")
		}
	}
}

// TestProgress_SentAgainstTheTokenAndNeverWithoutOne pins both halves of the
// spec rule: progress goes out carrying the client's own token (a number stays
// a number) with strictly increasing values, and a call without a token gets
// none at all. Progress is what keeps Claude Code from dropping a long call at
// its idle window, so the "with" half is the fix and the "without" half is the
// control.
func TestProgress_SentAgainstTheTokenAndNeverWithoutOne(t *testing.T) {
	tool := &progressTool{ctx: make(chan context.Context, 2)}
	s := mcp.New(mcp.ServerInfo{Name: "t", Version: "0"})
	s.Register(tool)
	send, lines := pipedServer(t, s)

	send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"progress","arguments":{},"_meta":{"progressToken":42}}}`)
	progress, resp := collectUntilResponse(t, lines, "1")
	if len(progress) != 2 {
		t.Fatalf("got %d progress notifications, want 2 (the repeated value must be dropped): %v", len(progress), progress)
	}
	for i, p := range progress {
		if p["progressToken"] != float64(42) {
			t.Errorf("notification %d carries token %#v, want the client's 42", i, p["progressToken"])
		}
		if p["progress"] != float64(i+1) || p["total"] != float64(2) {
			t.Errorf("notification %d = %v, want progress %d of 2", i, p, i+1)
		}
	}
	if !strings.Contains(resp, "has-progress=yes") {
		t.Errorf("HasProgress must be true for a call with a token: %s", resp)
	}

	// After the response, nothing more may be sent for that token.
	mcp.ReportProgress(<-tool.ctx, 3, 3, "too late")

	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"progress","arguments":{}}}`)
	progress, resp = collectUntilResponse(t, lines, "2")
	if len(progress) != 0 {
		t.Fatalf("progress was sent after the first call returned, or for a call with no token: %v", progress)
	}
	if !strings.Contains(resp, "has-progress=no") {
		t.Errorf("HasProgress must be false for a call without a token: %s", resp)
	}
}
