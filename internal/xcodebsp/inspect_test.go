package xcodebsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectBareXcodeNeedsGuidance(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "App.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	i := Inspect(root)
	if !i.NeedsGuidance() || !strings.Contains(i.GenerateCommand(), "-project") {
		t.Fatalf("inspection = %#v, command = %q", i, i.GenerateCommand())
	}
}

func TestInspectWorkspaceCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "App.xcworkspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	i := Inspect(root)
	if got := i.GenerateCommand(); !strings.Contains(got, "-workspace") || strings.Contains(got, "-project") {
		t.Fatalf("command = %q", got)
	}
}

func TestInspectBuildServerAndSwiftPMSuppressGuidance(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		data string
	}{
		{name: "build server", file: "buildServer.json", data: `{"name":"xcode","argv":["xcode-build-server"],"languages":["swift"]}`},
		{name: "swiftpm", file: "Package.swift", data: "// swift-tools-version: 6.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "App.xcodeproj"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, tc.file), []byte(tc.data), 0o644); err != nil {
				t.Fatal(err)
			}
			if i := Inspect(root); i.NeedsGuidance() {
				t.Fatalf("unexpected guidance: %#v", i)
			}
		})
	}
}

func TestInspectMultipleMarkersIsAmbiguous(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"One.xcodeproj", "Two.xcodeproj"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	i := Inspect(root)
	if !i.Ambiguous() || i.GenerateCommand() != "" || !strings.Contains(i.Hint(), "multiple") {
		t.Fatalf("inspection = %#v, hint = %q", i, i.Hint())
	}
}

func TestInspectMalformedBuildServer(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "App.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "buildServer.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	i := Inspect(root)
	if i.BuildServerErr == nil || !i.NeedsGuidance() {
		t.Fatalf("inspection = %#v", i)
	}
}
