package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
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

// servedFixture pins a session to a fresh workspace and makes it receive a waiting
// message of about the size a connection appends to a result, returning how many
// bytes that enrichment adds.
func servedFixture(t *testing.T, chatBudget int) (s *connSession, srv *mcp.Server, enrichment int) {
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
	seedMessage(t, s, root, "bob", s.view().sessName, strings.Repeat("deploy is blocked, stop what you are doing. ", 60))
	// Measure the enrichment the hook adds, then rewind so the real call sees the
	// message as unseen (a preview is offered once).
	extra := len(s.enrichToolOutput(context.Background(), "context_for_task", json.RawMessage(`{}`), ""))
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
		s, srv, extra := servedFixture(t, 600)
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

// The reserve is a contract on the enrichment, not something the tool can enforce:
// an enrichment larger than the reserve does push a full pack past max_bytes. The
// control proves the test above would notice, and states the limit.
func TestContextForTask_AnEnrichmentLargerThanTheReserveExceedsMaxBytes(t *testing.T) {
	s, srv, extra := servedFixture(t, 1500)
	if extra <= contextReserveForTest {
		t.Fatalf("control: the enrichment is %d B, not larger than the %d B reserve", extra, contextReserveForTest)
	}
	args := longSeeds()
	args["max_bytes"] = 1536
	if served := servedText(t, s, srv, args); len(served) <= 1536 {
		t.Errorf("a %d B enrichment on a full pack stayed within max_bytes (%d B), so the test above cannot tell a missing reserve from a present one", extra, len(served))
	}
}
