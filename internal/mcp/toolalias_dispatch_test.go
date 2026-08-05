package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// These tests drive the tool-name alias layer through the FULL tools/call
// dispatch against the REAL survivor tools, so an alias whose adapter does not
// fit the canonical tool's actual schema fails here rather than in the field.

// aliasToolServer registers the real tools that alias canonicals point at. All
// are metadata- and schema-complete with nil dependencies; daemon_info answers
// fully, while file_outline and workspace_symbols are exercised as far as their
// argument guard and their own validation (the language-server dependency is
// reached only where the test says so).
func aliasToolServer() *mcp.Server {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(tools.NewDaemonInfo("", "swift-falcon", "9.9.9", time.Now()))
	s.Register(tools.NewFileOutline(nil, nil, 0, 0))
	s.Register(tools.NewWorkspaceSymbols(stubDocSymbols{}, nil, 0, 0, nil))
	s.Register(tools.NewFindFiles(nil))
	return s
}

// stubDocSymbols is the minimum language server the workspace_symbols alias
// tests need: a documentSymbol answer for the uri-scoped mode and an empty
// workspace/symbol answer for the uri-less one.
type stubDocSymbols struct{ lsp.Client }

func (stubDocSymbols) DocumentSymbols(context.Context, protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	return []protocol.DocumentSymbol{{Name: "Greeter", Kind: protocol.SKClass}}, nil
}

func (stubDocSymbols) WorkspaceSymbols(context.Context, protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return nil, nil
}

// echoArgsTool stands in for a canonical tool so a test can observe exactly
// which arguments the alias adapter handed over.
type echoArgsTool struct{ name string }

func (e *echoArgsTool) Name() string        { return e.name }
func (e *echoArgsTool) Description() string { return "echoes the arguments it received" }
func (e *echoArgsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"uri":{"type":"string"},"path":{"type":"string"},"type":{"type":"string"},"max_depth":{"type":"integer"},"max_results":{"type":"integer"},"include_details":{"type":"boolean"}},"additionalProperties":false}`)
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
	for _, canonical := range []string{"daemon_info", "file_outline", "workspace_symbols", "find_files"} {
		if !listed[canonical] {
			t.Errorf("tools/list is missing the canonical tool %q", canonical)
		}
	}
	for _, alias := range []string{"version", "list_symbols", "find_symbol", "list_directory", "list_files"} {
		if listed[alias] {
			t.Errorf("tools/list advertises the alias %q — aliases must stay hidden", alias)
		}
	}
}

// TestToolsCall_FindSymbolAliasServesFileScopedSearch is the uri-present half of
// the merge: the retired name still runs the single-document engine, now living
// inside workspace_symbols, and says so.
func TestToolsCall_FindSymbolAliasServesFileScopedSearch(t *testing.T) {
	text := callTool(t, aliasToolServer(), "find_symbol", `{"query":"Greeter","uri":"file:///p/main.go"}`)

	const wantNotice = "note: find_symbol is a tool-name alias served by workspace_symbols — call workspace_symbols directly.\n\n"
	if !strings.HasPrefix(text, wantNotice) {
		t.Errorf("result must begin with the alias notice; got:\n%s", text)
	}
	if !strings.Contains(text, `Symbols matching "Greeter" in file:///p/main.go`) {
		t.Errorf("expected the file-scoped engine to have answered; got:\n%s", text)
	}
}

// TestToolsCall_FindSymbolAliasWithoutURISearchesWorkspace pins the upgrade: a
// uri-less find_symbol call used to fail with a redirect naming
// workspace_symbols. It now RUNS the workspace-wide search that redirect
// pointed at — strictly better, and the notice still names the survivor.
func TestToolsCall_FindSymbolAliasWithoutURISearchesWorkspace(t *testing.T) {
	text := callTool(t, aliasToolServer(), "find_symbol", `{"query":"Greeter"}`)

	const wantNotice = "note: find_symbol is a tool-name alias served by workspace_symbols — call workspace_symbols directly.\n\n"
	if !strings.HasPrefix(text, wantNotice) {
		t.Errorf("result must begin with the alias notice; got:\n%s", text)
	}
	if !strings.Contains(text, `No symbols found matching "Greeter"`) {
		t.Errorf("expected the workspace-wide search to have answered; got:\n%s", text)
	}
	if strings.Contains(text, "needs a uri") {
		t.Errorf("the retired uri-less redirect must be gone; got:\n%s", text)
	}
}

// TestToolsCall_ListDirectoryAliasListsOneLevelWithDetails drives the alias
// against the REAL find_files: the retired name still renders the detailed
// single-level listing, which only happens if the adapter's forced max_depth,
// type, and include_details all fit find_files' actual schema.
func TestToolsCall_ListDirectoryAliasListsOneLevelWithDetails(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "deep.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	text := callTool(t, aliasToolServer(), "list_directory", fmt.Sprintf(`{"path":%q}`, dir))

	const wantNotice = "note: list_directory is a tool-name alias served by find_files — call find_files directly.\n\n"
	if !strings.HasPrefix(text, wantNotice) {
		t.Errorf("result must begin with the alias notice; got:\n%s", text)
	}
	for _, want := range []string{"[DIR]  sub", "[FILE] top.go", "1 directory, 1 file"} {
		if !strings.Contains(text, want) {
			t.Errorf("detailed listing missing %q; got:\n%s", want, text)
		}
	}
	if strings.Contains(text, "deep.go") {
		t.Errorf("list_directory is one level only; got:\n%s", text)
	}
}

// TestToolsCall_ListFilesAliasRenamesRoot proves the root → path rename against
// the real find_files schema: find_files declares no "root", so an unadapted
// call would be rejected by the argument guard before the walk ever ran.
func TestToolsCall_ListFilesAliasRenamesRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	text := callTool(t, aliasToolServer(), "list_files", fmt.Sprintf(`{"root":%q,"pattern":"*.go"}`, dir))

	const wantNotice = "note: list_files is a tool-name alias served by find_files — call find_files directly.\n\n"
	if !strings.HasPrefix(text, wantNotice) {
		t.Errorf("result must begin with the alias notice; got:\n%s", text)
	}
	if !strings.Contains(text, "main.go") {
		t.Errorf("expected the walk to have answered; got:\n%s", text)
	}
}

// TestToolsCall_ListFilesAliasPinsTheOldDepthDefault guards the one silent
// widening the rename could have caused: list_files stopped at depth 8,
// find_files descends without limit, so the adapter must inject the old
// default when the caller did not set one.
func TestToolsCall_ListFilesAliasPinsTheOldDepthDefault(t *testing.T) {
	s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.Register(&echoArgsTool{name: "find_files"})

	text := callTool(t, s, "list_files", `{"root":"/p"}`)
	if !strings.Contains(text, `"max_depth":8`) {
		t.Errorf("adapter must pin list_files' default depth; got:\n%s", text)
	}
	if strings.Contains(text, `"root"`) {
		t.Errorf("root must be renamed away before dispatch; got:\n%s", text)
	}
}

// TestToolsCall_UnknownTool_DidYouMean covers the rejection path: a near-miss of
// a registered name OR of an alias gets a suggestion, and genuine garbage gets
// the bare error rather than a nonsense guess.
//
// A typo of an ALIAS is suggested as its canonical. Aliases are matched (so the
// hint fires at all) but never named: pointing a caller at a tool that appears
// in no tool list, and that plumb would only redirect anyway, teaches the wrong
// name at the one moment they are reading for the right one.
func TestToolsCall_UnknownTool_DidYouMean(t *testing.T) {
	tests := []struct {
		name    string
		called  string
		wantMsg string
	}{
		{"typo of a registered tool", "file_outlin", `unknown tool: file_outlin; did you mean "file_outline"?`},
		{"typo of an alias suggests its canonical", "versio", `unknown tool: versio; did you mean "daemon_info"?`},
		{"typo of a retired list tool suggests the survivor", "list_file", `unknown tool: list_file; did you mean "find_files"?`},
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

// TestToolsCall_AliasNoticeOnTheErrorPath is the failure half of the alias
// contract. An aliased call that errors reports the SURVIVOR's name in the
// message ("daemon_info: unknown parameter …") — a tool the caller never
// invoked. Without the notice there is nothing in the response connecting the
// two names, and the error reads as plumb answering something else entirely.
func TestToolsCall_AliasNoticeOnTheErrorPath(t *testing.T) {
	text := callTool(t, aliasToolServer(), "version", `{"bogus_param":1}`)

	const wantNotice = "note: version is a tool-name alias served by daemon_info — call daemon_info directly.\n\n"
	if !strings.HasPrefix(text, wantNotice) {
		t.Errorf("an errored aliased call must still carry the alias notice; got:\n%s", text)
	}
	if !strings.Contains(text, "error: ") {
		t.Fatalf("expected an error result; got:\n%s", text)
	}
	if !strings.Contains(text, "bogus_param") {
		t.Errorf("the real rejection must survive the notice prepend; got:\n%s", text)
	}
}

// TestToolsCall_ListAliasesLiftTheResultCap guards the second silent clip the
// fold could have caused: NEITHER retired tool capped its output —
// list_directory read a whole directory and list_files walked eight levels,
// both returning everything they found — while find_files stops at 500 results
// by default. Both adapters carry the schema maximum so a big listing still
// comes back whole.
func TestToolsCall_ListAliasesLiftTheResultCap(t *testing.T) {
	for _, alias := range []struct{ name, args string }{
		{"list_directory", `{"path":"/p"}`},
		{"list_files", `{"root":"/p"}`},
	} {
		t.Run(alias.name, func(t *testing.T) {
			s := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
			s.Register(&echoArgsTool{name: "find_files"})

			text := callTool(t, s, alias.name, alias.args)
			if !strings.Contains(text, `"max_results":5000`) {
				t.Errorf("adapter must lift the result cap for a previously uncapped listing; got:\n%s", text)
			}
		})
	}
}

// TestToolsCall_ListDirectoryAliasHonoursAnExplicitDepth is the set-if-absent
// policy end to end: the adapter's max_depth:1 is a DEFAULT. A caller who names
// a depth is deliberately asking for more than the retired tool could give, and
// gets it.
func TestToolsCall_ListDirectoryAliasHonoursAnExplicitDepth(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "deep.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	text := callTool(t, aliasToolServer(), "list_directory", fmt.Sprintf(`{"path":%q,"max_depth":2}`, dir))
	if !strings.Contains(text, "deep.go") {
		t.Errorf("an explicit max_depth must win over the adapter's default; got:\n%s", text)
	}
}

// TestToolsCall_ListFilesAliasHonoursAnExplicitType is the same policy on the
// other listing alias: list_files could only ever list files, so type:"dir" is
// unambiguously the caller reaching past it.
func TestToolsCall_ListFilesAliasHonoursAnExplicitType(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	text := callTool(t, aliasToolServer(), "list_files", fmt.Sprintf(`{"root":%q,"type":"dir"}`, dir))
	if !strings.Contains(text, "sub") {
		t.Errorf("type:\"dir\" must survive the adapter; got:\n%s", text)
	}
	if strings.Contains(text, "top.go") {
		t.Errorf("type:\"dir\" must exclude files; got:\n%s", text)
	}
}

// TestToolsCall_ListFilesAliasRejectsRootAndPathTogether pins the collision
// policy: two supplied values for one slot is a caller mistake the argument
// guard should state, not something the adapter quietly picks a winner for.
func TestToolsCall_ListFilesAliasRejectsRootAndPathTogether(t *testing.T) {
	dir := t.TempDir()
	text := callTool(t, aliasToolServer(), "list_files",
		fmt.Sprintf(`{"root":%q,"path":%q}`, dir, t.TempDir()))

	if !strings.Contains(text, "unknown parameter") || !strings.Contains(text, "root") {
		t.Errorf("expected an honest unknown-parameter rejection naming root; got:\n%s", text)
	}
}

// TestToolsCall_ListFilesAliasZeroDepthStaysShallow is the inversion the alias
// exists to prevent: find_files reads max_depth:0 as UNLIMITED, so passing the
// old caller's 0 straight through would answer a whole-tree walk to a request
// for the shallowest listing there is.
func TestToolsCall_ListFilesAliasZeroDepthStaysShallow(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b", "c", "d", "e", "f", "g", "h", "i")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "buried.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	text := callTool(t, aliasToolServer(), "list_files", fmt.Sprintf(`{"root":%q,"max_depth":0}`, dir))
	if !strings.Contains(text, "top.go") {
		t.Errorf("the listing must still answer; got:\n%s", text)
	}
	if strings.Contains(text, "buried.go") {
		t.Errorf("max_depth:0 must not invert into an unlimited walk; got:\n%s", text)
	}
}

// TestToolsCall_ListDirectoryAliasOnAFileSaysSo is F1's alias half. find_files
// has always answered a file path by listing its PARENT, while the retired
// list_directory hard-errored ("is not a directory"). Keeping the walk is fine;
// keeping it SILENT is not — an old caller who pointed at a file would read the
// parent's contents as that file's directory listing with nothing to say
// otherwise.
func TestToolsCall_ListDirectoryAliasOnAFileSaysSo(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	text := callTool(t, aliasToolServer(), "list_directory", fmt.Sprintf(`{"path":%q}`, file))
	if !strings.Contains(text, "is a file — listing its parent directory") {
		t.Errorf("a file path must be announced, not silently redirected; got:\n%s", text)
	}
	if !strings.Contains(text, "main.go") {
		t.Errorf("the parent listing must still be rendered; got:\n%s", text)
	}
}
