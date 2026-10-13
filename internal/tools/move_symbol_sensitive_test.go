package tools_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

const moveSecretSrc = "package demo\n\n// Foo does foo.\nfunc Foo() string { return \"TOPSECRET\" }\n\nfunc Bar() int { return 2 }\n"

// moveSecretSymbols matches moveSecretSrc: Foo's line is 40 characters long.
func moveSecretSymbols() []protocol.DocumentSymbol {
	return []protocol.DocumentSymbol{
		symbolAt("Foo", 3, 3, 40),
		symbolAt("Bar", 5, 5, 27),
	}
}

// TestMoveSymbol_SensitiveSourceIsWithheldFromResponseAndHistory moves a symbol
// out of a file matching a sensitive glob into an ordinary file: the destination's
// name alone would show the text, so the source must be judged with it. The
// ordinary-source control proves the marker is the gate working, not a default.
func TestMoveSymbol_SensitiveSourceIsWithheldFromResponseAndHistory(t *testing.T) {
	cases := []struct {
		name      string
		srcName   string
		sensitive bool
	}{
		{"sensitive source", "credentials.go", true},
		{"ordinary source", "plain.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, srcURI := writeInDir(t, dir, tc.srcName, moveSecretSrc)
			dstPath, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")

			var mu sync.Mutex
			var changes []history.Change
			deps := tools.WriteDeps{
				ShowWriteDiff: true,
				SensitivePathFn: func(_ context.Context, path, from string) bool {
					return history.IsSensitiveChange([]string{"credentials.*"}, dir, path, from)
				},
				HistoryFn: func(_ context.Context, c history.Change) {
					mu.Lock()
					defer mu.Unlock()
					changes = append(changes, c)
				},
			}
			tool := tools.NewMoveSymbol(&mockLSP{docSymbols: moveSecretSymbols()}, 0).WithWriteDeps(deps)
			dryRun := false
			out, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
				"source_uri":      srcURI,
				"name_path":       "Foo",
				"destination_uri": dstURI,
				"dry_run":         &dryRun,
			}))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got, _ := os.ReadFile(dstPath); !strings.Contains(string(got), "TOPSECRET") {
				t.Fatalf("the move did not land:\n%s", got)
			}

			var dstRow *history.Change
			mu.Lock()
			for i := range changes {
				if changes[i].Path == dstPath {
					dstRow = &changes[i]
				}
			}
			mu.Unlock()
			if dstRow == nil {
				t.Fatalf("no history row for the destination: %+v", changes)
			}

			shown := strings.Contains(out, "TOPSECRET")
			if tc.sensitive == shown {
				t.Errorf("sensitive=%v but response shows content=%v:\n%s", tc.sensitive, shown, out)
			}
			if got := strings.Contains(dstRow.From, "credentials.go"); got != tc.sensitive {
				t.Errorf("destination row From = %q, want the source recorded=%v", dstRow.From, tc.sensitive)
			}
			if !tc.sensitive && dstRow.From == "" {
				t.Errorf("destination row lost its From")
			}
			stored := history.IsSensitiveChange([]string{"credentials.*"}, dir, dstRow.Path, dstRow.From)
			if stored != tc.sensitive {
				t.Errorf("store classifies the destination row sensitive=%v, want %v", stored, tc.sensitive)
			}
		})
	}
}
