package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// These tests drive the tool-name alias layer through the FULL tools/call
// dispatch against the REAL survivor tools, so an alias whose adapter does not
// fit the canonical tool's actual schema fails here rather than in the field.

// aliasToolServer registers the real tools that alias canonicals point at.
// Both are metadata- and schema-complete with nil dependencies; daemon_info
// answers fully, and file_outline is exercised only as far as its argument
// guard (its language-server dependency is never reached).
func aliasToolServer() *mcp.Server {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(tools.NewDaemonInfo("", "swift-falcon", "9.9.9", time.Now()))
	s.Register(tools.NewFileOutline(nil, nil, 0, 0))
	return s
}

// echoArgsTool stands in for a canonical tool so a test can observe exactly
// which arguments the alias adapter handed over.
type echoArgsTool struct{ name string }

func (e *echoArgsTool) Name() string        { return e.name }
func (e *echoArgsTool) Description() string { return "echoes the arguments it received" }
func (e *echoArgsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"uri":{"type":"string"}},"additionalProperties":false}`)
}

func (e *echoArgsTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	return "args=" + string(args), nil
}

// TestToolsCall_VersionAliasServedByDaemonInfo is the merge's contract: the old
// name still answers, the notice names the survivor, and the answer is a
// superset — the go runtime and os/arch rows the retired tool reported are in
// daemon_info's output.
func TestToolsCall_VersionAliasServedByDaemonInfo(t *testing.T) {
	text := callTool(t, aliasToolServer(), "version", `{}`)

	const wantNotice = "note: version is a tool-name alias served by daemon_info — call daemon_info directly.\n\n"
	if !strings.HasPrefix(text, wantNotice) {
		t.Errorf("result must begin with the alias notice; got:\n%s", text)
	}
	for _, want := range []string{
		"daemon version: 9.9.9",
		"go runtime:     " + runtime.Version(),
		fmt.Sprintf("os/arch:        %s/%s", runtime.GOOS, runtime.GOARCH),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("daemon_info output missing %q; got:\n%s", want, text)
		}
	}
}

// TestToolsCall_ListSymbolsAliasFitsFileOutlineSchema proves the adapter
// against the REAL file_outline schema: include_signatures has no counterpart
// there, so an unadapted call would be rejected by the argument guard. The
// outline then fails on the missing file — which is the point: the call reached
// the tool's own logic, not the guard.
func TestToolsCall_ListSymbolsAliasFitsFileOutlineSchema(t *testing.T) {
	text := callTool(t, aliasToolServer(), "list_symbols",
		`{"uri":"/no/such/dir/main.go","include_signatures":true}`)

	if strings.Contains(text, "include_signatures") {
		t.Errorf("include_signatures must be dropped before validation; got:\n%s", text)
	}
	if !strings.Contains(text, "file_outline:") {
		t.Errorf("expected the canonical tool to have run; got:\n%s", text)
	}
}

// TestToolsCall_ListSymbolsAliasNoticeAndArgs pins the success path: the notice
// leads the result and the canonical tool receives exactly the adapted
// arguments.
func TestToolsCall_ListSymbolsAliasNoticeAndArgs(t *testing.T) {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(&echoArgsTool{name: "file_outline"})

	text := callTool(t, s, "list_symbols", `{"uri":"/p/main.go","include_signatures":true}`)

	const wantNotice = "note: list_symbols is a tool-name alias served by file_outline — call file_outline directly.\n\n"
	if !strings.HasPrefix(text, wantNotice) {
		t.Errorf("result must begin with the alias notice; got:\n%s", text)
	}
	if !strings.Contains(text, `args={"uri":"/p/main.go"}`) {
		t.Errorf("canonical tool did not receive the adapted arguments; got:\n%s", text)
	}
}

// TestToolsList_OmitsAliases is the unadvertised half of the contract: an alias
// is callable but never appears in tools/list, so it costs no schema budget and
// no client ever learns the retired name from plumb.
func TestToolsList_OmitsAliases(t *testing.T) {
	resps := serveOn(t, aliasToolServer(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listed := map[string]bool{}
	for _, tl := range resultByID(t, resps, 1)["tools"].([]any) {
		listed[tl.(map[string]any)["name"].(string)] = true
	}
	for _, canonical := range []string{"daemon_info", "file_outline"} {
		if !listed[canonical] {
			t.Errorf("tools/list is missing the canonical tool %q", canonical)
		}
	}
	for _, alias := range []string{"version", "list_symbols"} {
		if listed[alias] {
			t.Errorf("tools/list advertises the alias %q — aliases must stay hidden", alias)
		}
	}
}

// TestToolsCall_UnknownTool_DidYouMean covers the rejection path: a near-miss of
// a registered name OR of an alias gets a suggestion, and genuine garbage gets
// the bare error rather than a nonsense guess.
func TestToolsCall_UnknownTool_DidYouMean(t *testing.T) {
	tests := []struct {
		name    string
		called  string
		wantMsg string
	}{
		{"typo of a registered tool", "file_outlin", `unknown tool: file_outlin; did you mean "file_outline"?`},
		{"typo of an alias", "versio", `unknown tool: versio; did you mean "version"?`},
		{"nothing close enough", "qqqqqqqqqqqqqqqq", "unknown tool: qqqqqqqqqqqqqqqq"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, tt.called)
			resps := serveOn(t, aliasToolServer(), req)
			errObj, _ := resps[0]["error"].(map[string]any)
			if errObj == nil {
				t.Fatalf("expected an error response, got: %v", resps[0])
			}
			if got, _ := errObj["message"].(string); got != tt.wantMsg {
				t.Errorf("error message = %q, want %q", got, tt.wantMsg)
			}
		})
	}
}
