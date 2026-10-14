package cli

import (
	"errors"
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
	t.Setenv("HOME", t.TempDir()) // where every client's config would resolve
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
		})
	}
}
