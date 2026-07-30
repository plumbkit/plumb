package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// Per-tool wiring of the cold-server EMPTY-result caveat. A zero-value mockLSP
// replies successfully to everything with nothing in it — exactly what a server
// that has completed enough of its handshake to answer, but has not indexed,
// does. Without the caveat these tools hand back a confident negative that an
// agent acts on: "no references" → delete the symbol, "clean" → ship the change.

func warming(d time.Duration) tools.LSPWarmupFn {
	return func(string) (bool, time.Duration) { return true, d }
}

func ready() tools.LSPWarmupFn {
	return func(string) (bool, time.Duration) { return false, 0 }
}

func positionArgs() json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 2, "character": 5})
	return raw
}

func TestFindReferences_EmptyResultWhileWarming_IsCaveated(t *testing.T) {
	tool := tools.NewFindReferences(&mockLSP{}, nil, 0, 0).WithLSPWarmup(warming(3 * time.Second))
	out, err := tool.Execute(context.Background(), positionArgs())
	if err != nil {
		t.Fatalf("an empty result must not be an error: %v", err)
	}
	if !strings.Contains(out, "No references found") {
		t.Fatalf("lost the negative result: %q", out)
	}
	if !strings.Contains(out, "NOT evidence of absence") {
		t.Errorf("a warming server's empty reference set must be caveated: %q", out)
	}
}

func TestFindReferences_EmptyResultOnReadyServer_StaysPlain(t *testing.T) {
	tool := tools.NewFindReferences(&mockLSP{}, nil, 0, 0).WithLSPWarmup(ready())
	out, err := tool.Execute(context.Background(), positionArgs())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "still warming") {
		t.Errorf("a ready server's genuine empty result must stay plain: %q", out)
	}
}

func TestTypeHierarchy_EmptyResultWhileWarming_IsCaveated(t *testing.T) {
	tool := tools.NewTypeHierarchy(&mockLSP{}, 0).WithLSPWarmup(warming(3 * time.Second))
	out, err := tool.Execute(context.Background(), positionArgs())
	if err != nil {
		t.Fatalf("an empty result must not be an error: %v", err)
	}
	if !strings.Contains(out, "No type hierarchy item") || !strings.Contains(out, "NOT evidence of absence") {
		t.Errorf("a warming server's empty type hierarchy must be caveated: %q", out)
	}
}

func TestCallHierarchy_EmptyResultWhileWarming_IsCaveated(t *testing.T) {
	tool := tools.NewCallHierarchy(&mockLSP{}, 0).WithLSPWarmup(warming(3 * time.Second))
	out, err := tool.Execute(context.Background(), positionArgs())
	if err != nil {
		t.Fatalf("an empty result must not be an error: %v", err)
	}
	if !strings.Contains(out, "No call hierarchy item") || !strings.Contains(out, "NOT evidence of absence") {
		t.Errorf("a warming server's empty call hierarchy must be caveated: %q", out)
	}
}

// diagnostics is the worst cold-LSP case: it neither fails nor says "not
// found" — it reports CLEAN, which an agent reads as "my change compiles".
func TestDiagnostics_CleanReportWhileWarming_IsLabelledIncomplete(t *testing.T) {
	tool := tools.NewDiagnostics(&trackedCleanDiags{}).WithLSPWarmup(warming(5 * time.Second))
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "No issues found") {
		t.Fatalf("lost the clean report: %q", out)
	}
	if !strings.Contains(out, "INCOMPLETE") {
		t.Errorf("a clean report from a warming server must be labelled incomplete: %q", out)
	}
}

func TestDiagnostics_CleanReportOnReadyServer_StaysPlain(t *testing.T) {
	tool := tools.NewDiagnostics(&trackedCleanDiags{}).WithLSPWarmup(ready())
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if strings.Contains(out, "INCOMPLETE") || strings.Contains(out, "still warming") {
		t.Errorf("a ready server's clean report must stay plain: %q", out)
	}
}

// TestDiagnostics_MultiURI_WarmingBehindLaterURI_IsLabelled: a batch can span
// languages and roots, so it can span SERVERS. Probing only uris[0] — ready —
// would hand back a report that reads complete while the server behind a later
// URI is still indexing.
func TestDiagnostics_MultiURI_WarmingBehindLaterURI_IsLabelled(t *testing.T) {
	warmingOnlyForGo := func(uri string) (bool, time.Duration) {
		return strings.HasSuffix(uri, ".go"), 3 * time.Second
	}
	tool := tools.NewDiagnostics(&trackedCleanDiags{}).WithLSPWarmup(warmingOnlyForGo)
	args, _ := json.Marshal(map[string]any{"uris": []string{"file:///p/a.ts", "file:///p/b.go"}})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "INCOMPLETE") {
		t.Errorf("a warming server behind a later URI must still label the report: %q", out)
	}
}

// trackedCleanDiags reports every file as tracked with no diagnostics — the
// "analysed and clean" shape, which is indistinguishable from "warming and
// silent" without the warm-up probe.
type trackedCleanDiags struct{}

func (*trackedCleanDiags) Diagnostics(string) []protocol.Diagnostic         { return nil }
func (*trackedCleanDiags) AllDiagnostics() map[string][]protocol.Diagnostic { return nil }
func (*trackedCleanDiags) Tracked(string) bool                              { return true }
func (*trackedCleanDiags) WaitDiagnostics(context.Context, string) ([]protocol.Diagnostic, error) {
	return nil, nil
}
