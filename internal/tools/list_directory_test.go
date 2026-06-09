package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/tools"
)

func TestListDirectory_SymlinkShowsTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	tool := tools.NewListDirectory(nil)
	args, _ := json.Marshal(map[string]any{"path": dir})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("list_directory: %v", err)
	}
	if !strings.Contains(out, "[LINK]") {
		t.Errorf("expected a [LINK] entry, got:\n%s", out)
	}
	if !strings.Contains(out, "link.txt -> real.txt") {
		t.Errorf("expected symlink target annotation, got:\n%s", out)
	}
}
