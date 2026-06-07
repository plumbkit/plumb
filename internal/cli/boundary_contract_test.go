package cli

import (
	"os"
	"strings"
	"testing"
)

// knownToolGuarding maps each tools.New* constructor used in registerAllTools
// to its boundary-guard category. This is the D10 contract from the
// path-access design doc (docs/internal/path-access-design.md).
//
// Categories:
//
//	guard     — direct .WithBoundary(boundary or writeBoundary) in the registration
//	writedeps — guarded via WriteDeps.Boundary (constructor receives wd as first arg)
//	proxy     — guarded at the sessionProxy/sessionInv layer (setBoundaryGuard)
//	none      — no path input at all; boundary guard not applicable
var knownToolGuarding = map[string]string{
	// Direct .WithBoundary guard
	"NewFileOutline":           "guard",
	"NewDiagnosticsWithOpener": "guard",
	"NewListFiles":             "guard",
	"NewListDirectory":         "guard",
	"NewReadFile":              "guard",
	"NewReadSymbol":            "guard",
	"NewReadMultipleFiles":     "guard",
	"NewSearchInFiles":         "guard",
	"NewFindFiles":             "guard",
	"NewFileDiff":              "guard",
	"NewRenameSymbol":          "guard",
	"NewListMemories":          "guard",
	"NewReadMemory":            "guard",
	"NewWriteMemory":           "guard",
	"NewDeleteMemory":          "guard",
	"NewSearchMemories":        "guard",
	"NewRelevantMemories":      "guard",
	"NewTopologyStatus":        "guard",
	"NewWorkspaceSessions":     "guard",
	// Guarded via WriteDeps.Boundary (wd carries the write boundary guard)
	"NewWriteFile":        "writedeps",
	"NewEditFile":         "writedeps",
	"NewDeleteFile":       "writedeps",
	"NewRenameFile":       "writedeps",
	"NewCopyFile":         "writedeps",
	"NewTransactionApply": "writedeps",
	"NewGit":              "writedeps",
	"NewGitInit":          "writedeps",
	"NewFindReplace":      "writedeps",
	// Guarded at the proxy routing layer (setBoundaryGuard on sessionProxy/sessionInv)
	"NewFindSymbol":         "proxy",
	"NewWorkspaceSymbols":   "proxy",
	"NewGetDefinition":      "proxy",
	"NewExplainSymbol":      "proxy",
	"NewListSymbols":        "proxy",
	"NewFindReferences":     "proxy",
	"NewCallHierarchy":      "proxy",
	"NewTypeHierarchy":      "proxy",
	"NewInsertBeforeSymbol": "proxy",
	"NewInsertAfterSymbol":  "proxy",
	"NewReplaceSymbolBody":  "proxy",
	"NewSafeDeleteSymbol":   "proxy",
	// No path input — guard not applicable
	"NewVersion":          "none",
	"NewDaemonInfoFunc":   "none",
	"NewRenameSession":    "none",
	"NewSessionStart":     "none", // workspace arg is for deliberate re-pinning, not file access
	"NewTopologySearch":   "none",
	"NewTopologyExplore":  "none",
	"NewTopologyImpact":   "none",
	"NewTopologyAffected": "none", // queries the topology DB by symbol/file name; no direct FS access
	"NewTopologyRoutes":   "none",
	"NewStructuralQuery":  "none", // queries the topology DB; reads bodies only under the pinned workspace root, no user path input
}

// TestBoundaryGuardWiringComplete is the D10 registration-time contract test
// from the path-access design doc. It scans registerAllTools in conn_register.go
// and asserts:
//  1. Every tools.New* constructor is classified in knownToolGuarding.
//  2. Tools classified as "guard" have .WithBoundary in their registration line.
//  3. Tools classified as "writedeps" receive wd as their first constructor arg.
//
// A developer adding a new path-bearing tool to registerAllTools will see this
// test fail with "unknown tool" — the signal to add the correct guard and update
// the map. Mirrors TestInputSchemasDeclareAdditionalProperties in internal/tools,
// but guards the wiring layer rather than the schema layer.
func TestBoundaryGuardWiringComplete(t *testing.T) {
	src, err := os.ReadFile("conn_register.go")
	if err != nil {
		t.Fatalf("reading conn_register.go: %v", err)
	}
	body := registerAllToolsBody(string(src))
	if body == "" {
		t.Fatal("could not locate registerAllTools in conn_register.go — was it renamed?")
	}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "srv.Register(tools.New") {
			continue
		}
		name := extractToolName(trimmed)
		if name == "" {
			t.Errorf("could not parse tool name from: %s", trimmed)
			continue
		}
		cat, known := knownToolGuarding[name]
		if !known {
			t.Errorf("unknown tool %q in registerAllTools — "+
				"classify it in knownToolGuarding (boundary_contract_test.go) "+
				"with the correct category: guard/writedeps/proxy/none", name)
			continue
		}
		switch cat {
		case "guard":
			if !strings.Contains(trimmed, ".WithBoundary(") {
				t.Errorf("tool %s (category=guard) has no .WithBoundary in its registration:\n  %s",
					name, trimmed)
			}
		case "writedeps":
			if !strings.Contains(trimmed, "(wd") {
				t.Errorf("tool %s (category=writedeps) does not receive wd in its registration:\n  %s",
					name, trimmed)
			}
		}
	}
}

// registerAllToolsBody extracts the source text of the registerAllTools method
// from conn.go by finding the function signature and the next top-level function.
func registerAllToolsBody(src string) string {
	const sig = "func (s *connSession) registerAllTools"
	start := strings.Index(src, sig)
	if start < 0 {
		return ""
	}
	rest := src[start+len(sig):]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		return src[start:]
	}
	return src[start : start+len(sig)+end]
}

// extractToolName pulls the "New..." constructor name from a srv.Register line.
// Example input:  srv.Register(tools.NewReadFile(s.readTracker).WithBoundary(boundary)...)
// Example output: NewReadFile
func extractToolName(line string) string {
	rest := strings.TrimPrefix(line, "srv.Register(tools.")
	idx := strings.IndexAny(rest, "(.")
	if idx < 0 {
		return ""
	}
	return rest[:idx]
}
