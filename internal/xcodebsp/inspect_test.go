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
		{name: "build server", file: "buildServer.json", data: "{\"name\":\"xcode build server\",\"argv\":[\"/opt/homebrew/bin/xcode-build-server\"],\"languages\":[\"swift\"]}"},
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

func TestInspectRejectsForeignBuildServer(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "App.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := "{\"name\":\"other-bsp\",\"argv\":[\"xcode-build-server\"],\"languages\":[\"swift\"]}"
	if err := os.WriteFile(filepath.Join(root, "buildServer.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if i := Inspect(root); i.BuildServerOK || i.BuildServerErr == nil {
		t.Fatalf("inspection = %#v", i)
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

func TestSelectMarkerPrecedence(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"App.xcodeproj", "App.xcworkspace"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker, err := Inspect(root).SelectMarker()
	if err != nil {
		t.Fatal(err)
	}
	if marker.Flag != "-workspace" || !strings.HasSuffix(marker.Path, ".xcworkspace") {
		t.Fatalf("marker = %#v", marker)
	}
}

func TestSelectMarkerFallsBackToSoleProject(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"One.xcworkspace", "Two.xcworkspace", "App.xcodeproj"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker, err := Inspect(root).SelectMarker()
	if err != nil {
		t.Fatal(err)
	}
	if marker.Flag != "-project" || !strings.HasSuffix(marker.Path, ".xcodeproj") {
		t.Fatalf("marker = %#v", marker)
	}
}

func TestSchemesAndSelectScheme(t *testing.T) {
	for _, tc := range []struct {
		name     string
		marker   Marker
		json     string
		explicit string
		want     string
		wantErr  bool
	}{
		{name: "project sole", marker: Marker{Flag: "-project"}, json: "{\"project\":{\"schemes\":[\"App\"]}}", want: "App"},
		{name: "workspace explicit", marker: Marker{Flag: "-workspace"}, json: "{\"workspace\":{\"schemes\":[\"App\",\"Tests\"]}}", explicit: "Tests", want: "Tests"},
		{name: "missing explicit", marker: Marker{Flag: "-project"}, json: "{\"project\":{\"schemes\":[\"App\"]}}", explicit: "Missing", wantErr: true},
		{name: "ambiguous", marker: Marker{Flag: "-project"}, json: "{\"project\":{\"schemes\":[\"App\",\"Tests\"]}}", wantErr: true},
		{name: "empty", marker: Marker{Flag: "-project"}, json: "{\"project\":{\"schemes\":[]}}", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schemes, err := Schemes(tc.marker, []byte(tc.json))
			if err != nil {
				t.Fatal(err)
			}
			got, err := SelectScheme(schemes, tc.explicit)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SelectScheme(%v, %q) = %q, nil", schemes, tc.explicit, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("SelectScheme(%v, %q) = %q, %v; want %q", schemes, tc.explicit, got, err, tc.want)
			}
		})
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
