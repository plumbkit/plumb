package cli

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

// TestSetupEntryPoints_RecordTheServiceProfile pins PLAN-555: every `plumb setup`
// entry point records the service profile before it does anything else. Three of
// the five were pinned by nothing — deleting the call from claude-desktop, --all
// or the picker left the suite green — and a dropped call re-bricks those installs
// silently: the next canonical cold start finds no profile and refuses.
//
// The record is substituted by one that fails with a sentinel, so an entry point
// that makes the call returns that sentinel before touching any client's
// configuration, and one that does not goes on and returns something else.
func TestSetupEntryPoints_RecordTheServiceProfile(t *testing.T) {
	serviceInstallEnv(t)
	// Every variable a client's config path resolves through, so an entry point
	// that wrote before recording could only write under this test's own HOME.
	for _, v := range []string{"CODEX_HOME", "KIMI_CODE_HOME", "DSH_HOME"} {
		t.Setenv(v, "")
	}
	sentinel := errors.New("the service profile was recorded")

	oldRecord := recordServiceProfileForSetup
	oldUninstall, oldAll, oldProject := setupUninstallFlag, setupAllFlag, setupClaudeCodeProjectFlag
	t.Cleanup(func() {
		recordServiceProfileForSetup = oldRecord
		setupUninstallFlag, setupAllFlag, setupClaudeCodeProjectFlag = oldUninstall, oldAll, oldProject
	})
	setupUninstallFlag, setupClaudeCodeProjectFlag = false, false

	for _, entry := range []struct {
		name string
		run  func() error
	}{
		{"plumb setup claude-code", func() error { return runSetupClaudeCode(nil, nil) }},
		{"plumb setup claude-desktop", func() error { return runSetupClaudeDesktop(nil, nil) }},
		{"plumb setup codex (target-backed)", func() error { return runSetupTarget(codexTarget) }},
		{"plumb setup --all", func() error {
			setupAllFlag = true
			defer func() { setupAllFlag = false }()
			return runSetupAll(setupCmd, nil)
		}},
		{"plumb setup (interactive picker)", runSetupPicker},
	} {
		t.Run(entry.name, func(t *testing.T) {
			home := t.TempDir() // where every client's config would resolve
			t.Setenv("HOME", home)
			calls := 0
			recordServiceProfileForSetup = func() error {
				calls++
				return sentinel
			}
			err := entry.run()
			if !errors.Is(err, sentinel) || calls != 1 {
				t.Fatalf("%s did not record the service profile first (calls=%d, err=%v): a canonical cold start after it would refuse",
					entry.name, calls, err)
			}
			// FIRST, not merely at all: nothing may have been written before the
			// record, so a failed record leaves no client half-configured.
			if written := regularFilesUnder(t, home); len(written) > 0 {
				t.Fatalf("%s wrote %v before recording the service profile", entry.name, written)
			}
		})
	}
}

// regularFilesUnder lists the regular files beneath dir.
func regularFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}
