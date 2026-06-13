package config

import "testing"

// TestDefaults_GoRootMarkersIncludeGoWork guards that the default Go LSP config
// treats go.work as a root marker alongside go.mod, so a go.work-only workspace
// (e.g. one that mounts a module from a subdirectory or submodule) attaches
// gopls without a forced language.
func TestDefaults_GoRootMarkersIncludeGoWork(t *testing.T) {
	markers := Defaults().LSP["go"].RootMarkers

	var hasMod, hasWork bool
	for _, m := range markers {
		switch m {
		case "go.mod":
			hasMod = true
		case "go.work":
			hasWork = true
		}
	}
	if !hasMod || !hasWork {
		t.Fatalf("Go RootMarkers = %v, want both go.mod and go.work", markers)
	}
}
