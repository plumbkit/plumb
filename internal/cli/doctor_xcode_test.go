package cli

import (
	"os"
	"path/filepath"
	"testing"
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

func TestCheckXcodeBuildServerValidPasses(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "App.xcodeproj"))
	if err := os.WriteFile(filepath.Join(root, "buildServer.json"), []byte(`{"name":"xcode","argv":["xcode-build-server"],"languages":["swift"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := checkXcodeBuildServer(root)
	if len(checks) != 1 || !checks[0].ok || checks[0].warn {
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
