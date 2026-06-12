package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// detectTestPoolSwift builds a pool with go (go.mod) and swift (Package.swift +
// the *.xcodeproj/*.xcworkspace globs) to exercise glob root-marker matching.
func detectTestPoolSwift() *workspacePool {
	return &workspacePool{
		entries:  make(map[poolKey]*poolEntry),
		baseCtx:  context.Background(),
		cacheTTL: time.Minute,
		langs: []langConfig{
			{name: "go", cfg: config.LSPConfig{RootMarkers: []string{"go.mod"}, Enabled: true}},
			{name: "swift", cfg: config.LSPConfig{
				RootMarkers: []string{"Package.swift", "*.xcodeproj", "*.xcworkspace"},
				Enabled:     true,
			}},
		},
	}
}

// TestDetect_XcodeProjMarker: an Xcode app — a *.xcodeproj directory with no
// SwiftPM Package.swift — resolves as swift via the glob root marker.
func TestDetect_XcodeProjMarker(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, "NoCaps.xcodeproj"))

	_, lang, err := detectTestPoolSwift().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if lang != "swift" {
		t.Errorf("language: got %s, want swift (*.xcodeproj glob marker)", lang)
	}
}

// TestDetect_XcodeProjFromSubdir: the .xcodeproj sits at the project root while
// sources live in a subdirectory; the strong glob marker is found ancestrally.
func TestDetect_XcodeProjFromSubdir(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, "App.xcodeproj"))
	sub := filepath.Join(root, "Sources", "Views")
	mustMkdir(t, sub)

	gotRoot, lang, err := detectTestPoolSwift().Detect(sub)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if lang != "swift" {
		t.Errorf("language: got %s, want swift (ancestral *.xcodeproj)", lang)
	}
	if gotRoot != root {
		t.Errorf("root: got %s, want %s", gotRoot, root)
	}
}

// TestDetect_XcworkspaceMarker: a *.xcworkspace also resolves as swift.
func TestDetect_XcworkspaceMarker(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, "MyApp.xcworkspace"))

	_, lang, err := detectTestPoolSwift().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if lang != "swift" {
		t.Errorf("language: got %s, want swift (*.xcworkspace glob marker)", lang)
	}
}
