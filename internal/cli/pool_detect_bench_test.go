package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
)

func BenchmarkWorkspacePoolDetect_ProjectLanguage(b *testing.B) {
	root := b.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module benchmark\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	nested := filepath.Join(root, "internal", "pkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		b.Fatal(err)
	}

	cfg := config.Defaults()
	goCfg := cfg.LSP["go"]
	goCfg.Command = "go"
	goCfg.Enabled = true
	cfg.LSP["go"] = goCfg
	pool := newWorkspacePool(context.Background(), cfg)
	root = paths.Canonical(root)

	b.ResetTimer()
	for b.Loop() {
		gotRoot, language, err := pool.Detect(nested)
		if err != nil || gotRoot != root || language != "go" {
			b.Fatalf("Detect() = (%q, %q, %v), want (%q, go, nil)", gotRoot, language, err, root)
		}
	}
}
