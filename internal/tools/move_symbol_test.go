package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// writeInDir writes name under dir and returns its absolute path and file:// URI,
// so two fixtures can share one directory (move_symbol requires same-directory).
func writeInDir(t *testing.T, dir, name, content string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture %s: %v", name, err)
	}
	return path, "file://" + path
}

const moveSrc = "package demo\n\n// Foo does foo.\nfunc Foo() int { return 1 }\n\nfunc Bar() int { return 2 }\n"

// fooBarSymbols is the mock document-symbol tree matching moveSrc's two funcs.
func fooBarSymbols() []protocol.DocumentSymbol {
	return []protocol.DocumentSymbol{
		symbolAt("Foo", 3, 3, 27),
		symbolAt("Bar", 5, 5, 27),
	}
}

func moveArgs(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

func TestMoveSymbol_DryRunPreviewShowsBothFiles(t *testing.T) {
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", moveSrc)
	dstPath, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")

	mock := &mockLSP{docSymbols: fooBarSymbols()}
	tool := tools.NewMoveSymbol(mock, 0)
	out, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri":      srcURI,
		"name_path":       "Foo",
		"destination_uri": dstURI,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"DRY RUN", "Would move \"Foo\"", "src.go", "dst.go", "-func Foo() int", "+func Foo() int"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run preview missing %q:\n%s", want, out)
		}
	}
	// Neither file may change on a dry run.
	if got, _ := os.ReadFile(srcPath); string(got) != moveSrc {
		t.Errorf("source changed during dry run:\n%s", got)
	}
	if got, _ := os.ReadFile(dstPath); string(got) != "package demo\n\nfunc Keep() {}\n" {
		t.Errorf("destination changed during dry run:\n%s", got)
	}
}

func TestMoveSymbol_ApplyMovesWithDocComment(t *testing.T) {
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", moveSrc)
	dstPath, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")

	mock := &mockLSP{docSymbols: fooBarSymbols()}
	tool := tools.NewMoveSymbol(mock, 0)
	dryRun := false
	if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri":      srcURI,
		"name_path":       "Foo",
		"destination_uri": dstURI,
		"dry_run":         &dryRun,
	})); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	src, _ := os.ReadFile(srcPath)
	if strings.Contains(string(src), "func Foo() int") {
		t.Errorf("Foo not removed from source:\n%s", src)
	}
	if !strings.Contains(string(src), "func Bar() int") {
		t.Errorf("Bar wrongly removed from source:\n%s", src)
	}
	// Removing Foo (with its doc comment) leaves a run of 4 consecutive
	// newlines between "package demo" and "func Bar" — assert it collapsed to
	// exactly one blank line rather than merely checking containment.
	wantSrc := "package demo\n\nfunc Bar() int { return 2 }\n"
	if string(src) != wantSrc {
		t.Errorf("source not normalised at the removal seam:\ngot:  %q\nwant: %q", src, wantSrc)
	}
	dst, _ := os.ReadFile(dstPath)
	for _, want := range []string{"func Keep() {}", "// Foo does foo.", "func Foo() int { return 1 }"} {
		if !strings.Contains(string(dst), want) {
			t.Errorf("destination missing %q:\n%s", want, dst)
		}
	}
}

// TestMoveSymbol_ApplyTrimsTrailingNewlineWhenLastDeclRemoved covers the other
// half of removal-seam normalisation: removing the file's LAST declaration
// must not leave a dangling blank line before EOF.
func TestMoveSymbol_ApplyTrimsTrailingNewlineWhenLastDeclRemoved(t *testing.T) {
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", moveSrc)
	dstPath := filepath.Join(dir, "new.go")
	dstURI := "file://" + dstPath

	mock := &mockLSP{docSymbols: fooBarSymbols()}
	tool := tools.NewMoveSymbol(mock, 0)
	dryRun := false
	if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri":         srcURI,
		"name_path":          "Bar",
		"destination_uri":    dstURI,
		"create_destination": true,
		"dry_run":            &dryRun,
	})); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	wantSrc := "package demo\n\n// Foo does foo.\nfunc Foo() int { return 1 }\n"
	if got, _ := os.ReadFile(srcPath); string(got) != wantSrc {
		t.Errorf("source not trimmed to a single trailing newline after removing the last declaration:\ngot:  %q\nwant: %q", got, wantSrc)
	}
}

func TestMoveSymbol_CreateDestination(t *testing.T) {
	dir := t.TempDir()
	_, srcURI := writeInDir(t, dir, "src.go", moveSrc)
	dstPath := filepath.Join(dir, "new.go")
	dstURI := "file://" + dstPath

	mock := &mockLSP{docSymbols: fooBarSymbols()}
	tool := tools.NewMoveSymbol(mock, 0)
	dryRun := false
	if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri":         srcURI,
		"name_path":          "Bar",
		"destination_uri":    dstURI,
		"create_destination": true,
		"dry_run":            &dryRun,
	})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	dst, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("destination not created: %v", err)
	}
	if !strings.HasPrefix(string(dst), "package demo\n") {
		t.Errorf("created Go destination not seeded with package clause:\n%s", dst)
	}
	if !strings.Contains(string(dst), "func Bar() int { return 2 }") {
		t.Errorf("created destination missing moved declaration:\n%s", dst)
	}
}

func TestMoveSymbol_Refusals(t *testing.T) {
	tests := []struct {
		name    string
		docSyms []protocol.DocumentSymbol
		mutate  func(dir string, args map[string]any) // adjust fixture/args per case
		wantErr string
	}{
		{
			name:    "missing destination without create flag",
			docSyms: fooBarSymbols(),
			mutate: func(dir string, args map[string]any) {
				args["destination_uri"] = "file://" + filepath.Join(dir, "absent.go")
			},
			wantErr: "does not exist",
		},
		{
			name:    "cross-directory",
			docSyms: fooBarSymbols(),
			mutate: func(dir string, args map[string]any) {
				args["destination_uri"] = "file://" + filepath.Join(dir, "sub", "dst.go")
			},
			wantErr: "cross-directory",
		},
		{
			name:    "symbol not found",
			docSyms: fooBarSymbols(),
			mutate: func(dir string, args map[string]any) {
				args["name_path"] = "Nope"
			},
			wantErr: "not found",
		},
		{
			name:    "ambiguous name",
			docSyms: []protocol.DocumentSymbol{symbolAt("Foo", 3, 3, 27), symbolAt("Foo", 5, 5, 27)},
			mutate:  func(dir string, args map[string]any) {},
			wantErr: "ambiguous",
		},
		{
			name:    "same file",
			docSyms: fooBarSymbols(),
			mutate: func(dir string, args map[string]any) {
				args["destination_uri"] = args["source_uri"]
			},
			wantErr: "same file",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, srcURI := writeInDir(t, dir, "src.go", moveSrc)
			_, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")
			args := map[string]any{
				"source_uri":      srcURI,
				"name_path":       "Foo",
				"destination_uri": dstURI,
			}
			tc.mutate(dir, args)
			tool := tools.NewMoveSymbol(&mockLSP{docSymbols: tc.docSyms}, 0)
			_, err := tool.Execute(context.Background(), moveArgs(t, args))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestMoveSymbol_RefusesCrossPackage(t *testing.T) {
	dir := t.TempDir()
	_, srcURI := writeInDir(t, dir, "src.go", moveSrc)
	// Same directory, different package clause (the _test-package case).
	_, dstURI := writeInDir(t, dir, "dst.go", "package demo_test\n\nfunc Keep() {}\n")

	tool := tools.NewMoveSymbol(&mockLSP{docSymbols: fooBarSymbols()}, 0)
	_, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri":      srcURI,
		"name_path":       "Foo",
		"destination_uri": dstURI,
	}))
	if err == nil || !strings.Contains(err.Error(), "cross-package") {
		t.Fatalf("want cross-package refusal, got %v", err)
	}
}

func TestMoveSymbol_RefusesDifferingBuildTags(t *testing.T) {
	dir := t.TempDir()
	_, srcURI := writeInDir(t, dir, "src.go", "//go:build linux\n\npackage demo\n\nfunc Foo() int { return 1 }\n")
	_, dstURI := writeInDir(t, dir, "dst.go", "//go:build darwin\n\npackage demo\n\nfunc Keep() {}\n")

	// "Foo" now sits on line 4 (0-based) because of the leading build-tag
	// comment and blank line.
	tool := tools.NewMoveSymbol(&mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 4, 4, 27)}}, 0)
	_, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri":      srcURI,
		"name_path":       "Foo",
		"destination_uri": dstURI,
	}))
	if err == nil || !strings.Contains(err.Error(), "build constraint") {
		t.Fatalf("want build-constraint refusal, got %v", err)
	}
}

func TestMoveSymbol_AllowsMoveWhenNeitherFileHasBuildTags(t *testing.T) {
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", moveSrc)
	_, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")

	mock := &mockLSP{docSymbols: fooBarSymbols()}
	tool := tools.NewMoveSymbol(mock, 0)
	dryRun := false
	if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri":      srcURI,
		"name_path":       "Foo",
		"destination_uri": dstURI,
		"dry_run":         &dryRun,
	})); err != nil {
		t.Fatalf("expected move to proceed when neither file has build tags, got: %v", err)
	}
	if got, _ := os.ReadFile(srcPath); strings.Contains(string(got), "func Foo() int") {
		t.Errorf("Foo not removed from source:\n%s", got)
	}
}

func TestMoveSymbol_TreeSitterFallback(t *testing.T) {
	ws := t.TempDir()
	src := "package demo\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n"
	srcPath := filepath.Join(ws, "src.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(ws, "dst.go")
	if err := os.WriteFile(dstPath, []byte("package demo\n\nfunc Keep() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tool := tools.NewMoveSymbol(brokenLSP(), 0).
		WithTopologyFallback(func() *topology.Store { return store })
	dryRun := false
	out, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri":      "file://" + srcPath,
		"name_path":       "Beta",
		"destination_uri": "file://" + dstPath,
		"dry_run":         &dryRun,
	}))
	if err != nil {
		t.Fatalf("expected tree-sitter fallback to move, got: %v", err)
	}
	if !strings.Contains(out, "topology fallback") {
		t.Errorf("expected fallback banner:\n%s", out)
	}
	srcNow, _ := os.ReadFile(srcPath)
	if strings.Contains(string(srcNow), "func Beta() int") {
		t.Errorf("Beta not removed from source:\n%s", srcNow)
	}
	dstNow, _ := os.ReadFile(dstPath)
	if !strings.Contains(string(dstNow), "func Beta() int") {
		t.Errorf("Beta not appended to destination:\n%s", dstNow)
	}
}

func TestMoveSymbol_RefusesDestinationOutsideWorkspace(t *testing.T) {
	ws := t.TempDir()
	_, srcURI := writeInDir(t, ws, "src.go", moveSrc)
	outside := t.TempDir() // a sibling root, not under ws
	dstURI := "file://" + filepath.Join(outside, "dst.go")

	guard := tools.NewPathPolicy(ws, []tools.AllowedRoot{
		{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"},
	}).WriteGuard()
	tool := tools.NewMoveSymbol(&mockLSP{docSymbols: fooBarSymbols()}, 0).
		WithWriteDeps(tools.WriteDeps{Boundary: guard})

	_, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri":      srcURI,
		"name_path":       "Foo",
		"destination_uri": dstURI,
	}))
	if err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("want workspace boundary refusal, got %v", err)
	}
}
