package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConsentProjectConfig(t *testing.T, ws, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTargetAllowsCrossProject_NoProjectConfigFallsBackToBase is the common
// case: a workspace with no .plumb/config.toml inherits the caller's own
// resolved default.
func TestTargetAllowsCrossProject_NoProjectConfigFallsBackToBase(t *testing.T) {
	ws := t.TempDir()
	base := Defaults()
	base.Collab.CrossProject = true
	if !TargetAllowsCrossProject(base, ws) {
		t.Error("a workspace with no project config must inherit base's cross_project default")
	}
	base.Collab.CrossProject = false
	if TargetAllowsCrossProject(base, ws) {
		t.Error("a workspace with no project config must inherit base's cross_project default (false case)")
	}
}

// TestTargetAllowsCrossProject_UntrustedRequestStaysClosed is the security
// contract this whole feature depends on: a project asking for cross_project
// in its own (untrusted) config.toml must not grant it to itself.
func TestTargetAllowsCrossProject_UntrustedRequestStaysClosed(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	writeConsentProjectConfig(t, ws, "[collab]\ncross_project = true\n")

	if TargetAllowsCrossProject(Defaults(), ws) {
		t.Error("an untrusted project config must not be able to grant itself cross_project")
	}
}

// TestTargetAllowsCrossProject_TrustedRequestIsHonoured is the positive half:
// once the user has run `plumb trust` over this exact request, it takes
// effect.
func TestTargetAllowsCrossProject_TrustedRequestIsHonoured(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	writeConsentProjectConfig(t, ws, "[collab]\ncross_project = true\n")

	spec, err := ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatal(err)
	}
	cmds, err := ProjectTaskCommands(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewTrustStore().SetTrustedForProject(ws, cmds, spec); err != nil {
		t.Fatal(err)
	}

	if !TargetAllowsCrossProject(Defaults(), ws) {
		t.Error("a trusted project config's cross_project request must be honoured")
	}
}

// TestTargetAllowsCrossProject_EmptyWorkspaceIsClosed: the callers this
// exists for have no single recipient to ask, so an unresolved workspace must
// never read as consenting.
func TestTargetAllowsCrossProject_EmptyWorkspaceIsClosed(t *testing.T) {
	base := Defaults()
	base.Collab.CrossProject = true
	if TargetAllowsCrossProject(base, "") {
		t.Error("an empty workspace must never report consent, regardless of base's default")
	}
}

// TestTargetAllowsCrossProject_UnparseableConfigFailsClosed: a project config
// that will not parse must not silently fall back to "consenting".
func TestTargetAllowsCrossProject_UnparseableConfigFailsClosed(t *testing.T) {
	ws := t.TempDir()
	writeConsentProjectConfig(t, ws, "[collab\ncross_project = true\n")

	base := Defaults()
	base.Collab.CrossProject = true
	if TargetAllowsCrossProject(base, ws) {
		t.Error("an unparseable project config must fail closed, not fall back to base's true default")
	}
}
