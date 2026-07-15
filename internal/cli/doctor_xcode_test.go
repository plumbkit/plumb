package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/xcodebsp"
)

func TestCheckXcodeBuildServerMalformedFails(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "App.xcodeproj"))
	if err := os.WriteFile(filepath.Join(root, "buildServer.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := checkXcodeBuildServer(root)
	if len(checks) != 1 || checks[0].ok || checks[0].fix == "" {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestCheckXcodeBuildServerConfiguredWarnsUntilSemanticProven(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "App.xcodeproj"))
	if err := os.WriteFile(filepath.Join(root, "buildServer.json"), []byte(`{"name":"xcode build server","argv":["xcode-build-server"],"languages":["swift"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := checkXcodeBuildServer(root)
	if len(checks) != 1 || !checks[0].ok || !checks[0].warn {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestCheckXcodeBuildServerIgnoresSwiftPM(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "App.xcodeproj"))
	if err := os.WriteFile(filepath.Join(root, "Package.swift"), []byte("// swift-tools-version: 6.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if checks := checkXcodeBuildServer(root); checks != nil {
		t.Fatalf("checks = %#v", checks)
	}
}

func TestCheckXcodeLifecycleHonestStates(t *testing.T) {
	tests := []struct {
		name  string
		state xcodebsp.State
		ok    bool
		warn  bool
	}{
		{name: "semantic proven", state: xcodebsp.StateSemanticProven, ok: true},
		{name: "warming", state: xcodebsp.StateWarming, ok: true, warn: true},
		{name: "failed", state: xcodebsp.StateFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := checkXcodeLifecycle(xcodebsp.Status{State: tt.state, Detail: tt.name}, "/workspace")
			if len(checks) != 1 || checks[0].ok != tt.ok || checks[0].warn != tt.warn {
				t.Fatalf("checks = %#v", checks)
			}
		})
	}
}
