package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// servedText runs context_for_task through a real mcp.Server tools/call, with the
// connection's real EnrichToolOutput hook attached, and returns the text a client
// receives. The tool is the object registerAllTools built.
func servedText(t *testing.T, s *connSession, srv *mcp.Server, args map[string]any) string {
	t.Helper()
	tool, ok := srv.Lookup("context_for_task")
	if !ok {
		t.Fatal("context_for_task is not registered")
	}
	served := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	served.Register(tool)
	served.EnrichToolOutput = s.enrichToolOutput
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"context_for_task","arguments":%s}}`, raw)
	var out bytes.Buffer
	if err := served.Serve(context.Background(), strings.NewReader(frame+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", out.String(), err)
	}
	if resp.Error != nil || resp.Result.IsError || len(resp.Result.Content) == 0 {
		t.Fatalf("the call failed: %s", out.String())
	}
	return resp.Result.Content[0].Text
}

// servedFixture pins a session to a fresh workspace and makes it receive waiting
// messages of about the size a connection appends to a result, returning how many
// bytes that enrichment adds on a tool whose result is not budgeted.
func servedFixture(t *testing.T, chatBudget, messages int) (s *connSession, srv *mcp.Server, enrichment int) {
	t.Helper()
	s, srv = buildTestConnSession(t)
	root := freshTempDir(t)
	mustWrite(t, root+"/main.go", "package main\n\nfunc main() {}\n")
	s.collabPool = newCollabPool()
	t.Cleanup(s.collabPool.closeAll)
	s.mutate(func(v *sessionView) {
		v.acquiredRoot = root
		v.policy = s.buildPathPolicy(v)
		v.collab = config.CollabConfig{Mailbox: true, ChatBudgetBytes: chatBudget}
	})
	for range messages {
		seedMessage(t, s, root, "bob", s.view().sessName, strings.Repeat("deploy is blocked, stop what you are doing. ", 60))
	}
	// Measure the enrichment the hook adds to a tool nothing bounds, then rewind so
	// the real call sees the messages as unseen (a preview is offered once).
	extra := len(s.enrichToolOutput(context.Background(), "run_task", json.RawMessage(`{}`), ""))
	s.chatWatch.reset()
	return s, srv, extra
}

// longSeeds fills a pack to its budget with unresolved selectors, which render as
// one line each whatever the index holds, so the output is as large as the budget
// lets it be.
func longSeeds() map[string]any {
	symbols := make([]string, 0, 8)
	for i := range 8 {
		symbols = append(symbols, fmt.Sprintf("Selector%d_%s", i, strings.Repeat("x", 90)))
	}
	return map[string]any{"files": []string{"main.go"}, "symbols": symbols}
}

// S15: max_bytes bounds the WHOLE served response. The pack renders into
// max_bytes minus the 1024 B reserve for the connection layer's own appended text;
// with a ~1 KiB message preview appended through the real hook, the served text
// still fits.
func TestContextForTask_ServedBytesStayWithinMaxBytesWithAnEnrichment(t *testing.T) {
	for _, maxBytes := range []int{1536, 2048, 4000, 12000} {
		s, srv, extra := servedFixture(t, 600, 1)
		if extra < 900 || extra > 1024 {
			t.Fatalf("control: the enrichment is %d B; this test needs about 1 KiB (900..1024)", extra)
		}
		args := longSeeds()
		args["max_bytes"] = maxBytes
		served := servedText(t, s, srv, args)
		if !strings.Contains(served, "[Messages") {
			t.Fatalf("max_bytes %d: the message preview was not appended, so nothing was tested:\n%s", maxBytes, served)
		}
		if len(served) > maxBytes {
			t.Errorf("max_bytes %d: the client received %d bytes (%d of pack + %d appended)", maxBytes, len(served), len(served)-extra, extra)
		}
		// The pack really was cut to its budget, so the margin above is the reserve
		// and not slack: a pack well under budget would pass for any reserve.
		if pack := len(served) - extra; maxBytes <= 2048 && pack < maxBytes-contextReserveForTest-250 {
			t.Errorf("max_bytes %d: the pack is only %d bytes, so it did not fill its budget", maxBytes, pack)
		}
	}
}

// contextReserveForTest mirrors tools' 1024 B reserve. It is spelled out here
// because the contract under test is the number a client is told, not a constant
// the tool could change under the test.
const contextReserveForTest = 1024

// S3: the reserve is held to, not merely hoped for. A message preview larger than the
// reserve (the default chat budget is 2 KiB, so one ordinary waiting message is) is
// shortened to fit, and when even short bodies cannot fit it becomes a one-line pointer
// at check_messages; either way the served text stays within max_bytes and the agent
// still learns a message is waiting. Positive control: the same enrichment appended to a
// tool nothing bounds is not shortened, so only context_for_task is affected.
func TestContextForTask_AnEnrichmentLargerThanTheReserveIsHeldToIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages int
		want     []string // what the client must still be told
		not      []string
	}{
		{"one long message: a shortened preview", 1, []string{"[Messages", "from bob", "check_messages"}, []string{"the preview is left out"}},
		{"several long messages: still told, still within max_bytes", 5, []string{"[Messages", "check_messages"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, maxBytes := range []int{1536, 4000, 12000} {
				s, srv, extra := servedFixture(t, 1500, tc.messages)
				if extra <= contextReserveForTest {
					t.Fatalf("control: the unbounded enrichment is %d B, not larger than the %d B reserve", extra, contextReserveForTest)
				}
				args := longSeeds()
				args["max_bytes"] = maxBytes
				served := servedText(t, s, srv, args)
				if len(served) > maxBytes {
					t.Errorf("max_bytes %d: the client received %d bytes with a %d B enrichment waiting", maxBytes, len(served), extra)
				}
				tail := served[max(0, len(served)-1100):]
				for _, w := range tc.want {
					if !strings.Contains(served, w) {
						t.Errorf("max_bytes %d: the client is not told %q:\n%s", maxBytes, w, tail)
					}
				}
				for _, n := range tc.not {
					if strings.Contains(served, n) {
						t.Errorf("max_bytes %d: the result holds %q:\n%s", maxBytes, n, tail)
					}
				}
			}
		})
	}

	// Control: the unbounded enrichment on this fixture is what the bounded one is cut
	// from, so the tool is the only thing that changed it.
	s, _, extra := servedFixture(t, 1500, 1)
	if bounded := len(s.enrichToolOutput(context.Background(), "context_for_task", json.RawMessage(`{}`), "")); bounded == 0 || bounded > contextReserveForTest || bounded >= extra {
		t.Errorf("the enrichment on context_for_task is %d B, want it shortened from %d B to within %d B", bounded, extra, contextReserveForTest)
	}
}

// previewWithin shortens before it gives up, points before it says nothing, and never
// touches an unbounded room.
func TestPreviewWithin(t *testing.T) {
	grows := func(budget int) string { return strings.Repeat("x", budget+300) } // a preview that scales with its bodies
	fixed := func(int) string { return strings.Repeat("x", 5000) }              // one that no body budget can shrink
	if got := previewWithin(grows, 2048, unboundedRoom, 3); len(got) != 2048+300 {
		t.Errorf("an unbounded room cut the preview to %d B", len(got))
	}
	if got := previewWithin(grows, 2048, 1024, 3); len(got) != 512+300 {
		t.Errorf("a 1024 B room gave %d B, want the first halving that fits (512 B bodies = 812 B)", len(got))
	}
	got := previewWithin(fixed, 2048, 1024, 3)
	if want := tools.RenderWaitingPointer(3); got != want || len(got) > 1024 {
		t.Errorf("a preview nothing can shrink gave %q, want the pointer %q", got, want)
	}
	if got := previewWithin(fixed, 2048, 50, 3); got != "" {
		t.Errorf("a room too small even for the pointer gave %q, want nothing", got)
	}
}

// The room is spent in order and a spent room says nothing: fitRoom passes an
// unbounded block through, clamps one to what is left on a UTF-8 boundary, and gives
// nothing once nothing is left.
func TestFitRoom(t *testing.T) {
	block := strings.Repeat("é", 100) // 200 bytes
	unbounded, spent, small := unboundedRoom, 0, 60
	if got := fitRoom(block, &unbounded); got != block || unbounded != unboundedRoom {
		t.Errorf("an unbounded room changed the block or itself: %d B, room %d", len(got), unbounded)
	}
	if got := fitRoom(block, &spent); got != "" || spent != 0 {
		t.Errorf("a spent room gave %d B, room %d", len(got), spent)
	}
	got := fitRoom(block, &small)
	if len(got) == 0 || len(got) > 60 || !utf8.ValidString(got) || small != 60-len(got) {
		t.Errorf("a 60 B room gave %d B (valid UTF-8 %v) and left %d", len(got), utf8.ValidString(got), small)
	}
}
