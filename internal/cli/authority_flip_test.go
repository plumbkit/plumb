package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/service"
)

// The PLAN-518 default flip's launch half.
//
// Two properties are pinned here that the flip turns from "reachable only in a
// test" into production behaviour: a managed launch takes the ACCOUNT AUTHORITY
// in every mode, opted out included, and it tells the daemon it starts which
// mode it resolved — because the daemon resolves the runtime directory its
// lifetime lock lives in BEFORE it reads any configuration, so a daemon left to
// infer the mode from the environment would look for the handed-over lock in a
// different directory and refuse to start.

// The opt-out is not a disarming: it must not stop the account authority being
// taken, in the launcher or in a directly started daemon.
func TestLaunchLocks_TakeTheAccountAuthorityInEveryMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate string
	}{
		{name: "account authority on", gate: "1"},
		{name: "account authority off (the opt-out)", gate: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempRuntime(t)
			unsetInheritedLockEnv(t)
			t.Setenv("PLUMB_ACCOUNT_AUTHORITY", tc.gate)

			locks, err := acquireLaunchLocks()
			if err != nil {
				t.Fatalf("a managed launch could not take its locks: %v", err)
			}
			if locks.Authority == nil {
				t.Fatal("the launch took no account authority: one daemon per account must not be configurable")
			}
			if locks.Lifetime == nil {
				t.Fatal("the launch took no runtime lifetime lock")
			}
			// A daemon (or a second launch) beside the holder must lose, in both
			// modes: the fence is the authority, not the mode.
			if _, err := daemonLaunchLocks(); !errors.Is(err, errDaemonAlreadyRunning) {
				t.Errorf("a daemon started beside a launch lock = %v, want errDaemonAlreadyRunning", err)
			}
			if _, err := acquireAuthority(); !errors.Is(err, errDaemonAlreadyRunning) {
				t.Errorf("a second authority acquisition = %v, want errDaemonAlreadyRunning", err)
			}
			locks.closeAll()
			freeLaunchLocks(t)
		})
	}
}

// TestDaemonCommand_StampsTheLaunchersAuthorityMode is the handover of the
// DECISION, not just of the locks. The child must be told the mode its launcher
// resolved, and any value it inherited must be replaced in place: os.Getenv
// resolves the first match, so an appended variable would lose to the inherited
// one in the legacy branch, where the environment is this process's own.
func TestDaemonCommand_StampsTheLaunchersAuthorityMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gate      string
		inherited []string
	}{
		{name: "canonical, nothing inherited", gate: "1"},
		{name: "legacy opt-out, nothing inherited", gate: "0"},
		{name: "canonical, a stale inherited value", gate: "1", inherited: []string{"PLUMB_ACCOUNT_AUTHORITY=false"}},
		{name: "legacy opt-out, a stale inherited value", gate: "0", inherited: []string{"PLUMB_ACCOUNT_AUTHORITY=true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempRuntime(t)
			t.Setenv("PLUMB_ACCOUNT_AUTHORITY", tc.gate)
			want := "PLUMB_ACCOUNT_AUTHORITY=" + boolText(paths.AuthorityEnabled())

			spec := daemonLaunchSpec{
				Executable: "/opt/installed/plumb",
				Env:        append([]string{"HOME=/home/tester"}, tc.inherited...),
			}
			cmd, logFile, err := daemonCommand(spec, launchLocks{
				Authority: lockFile(t, "authority"),
				Lifetime:  lockFile(t, "lifetime"),
			})
			if err != nil {
				t.Fatalf("daemonCommand: %v", err)
			}
			defer func() { _ = logFile.Close() }()

			got := 0
			for _, v := range cmd.Env {
				if strings.HasPrefix(v, "PLUMB_ACCOUNT_AUTHORITY=") {
					got++
					if v != want {
						t.Errorf("the daemon was told %q, want %q (what this launcher resolved)", v, want)
					}
				}
			}
			if got != 1 {
				t.Errorf("the daemon's environment carries %d authority-mode entries, want exactly 1: %v", got, cmd.Env)
			}
		})
	}
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// `plumb setup` is the one command every non-Homebrew install runs, so it is
// where a first-run profile has to come from: a canonical cold start with no
// profile REFUSES rather than guessing which binary is the daemon.
func TestSetupRecordsTheServiceProfileWhenTheAuthorityIsOn(t *testing.T) {
	serviceInstallEnv(t) // the account seam, the gate on, a PATH worth recording
	if err := ensureServiceProfileForSetup(); err != nil {
		t.Fatalf("ensureServiceProfileForSetup: %v", err)
	}
	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	p, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("plumb setup did not record a service profile: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if p.Executable != exe {
		t.Errorf("profile executable = %q, want the installing binary %q", p.Executable, exe)
	}
}

// The opt-out is left alone: in legacy mode the profile is ignored entirely, so
// writing one would hand the user a file nothing reads.
func TestSetupLeavesTheProfileAloneWhenTheAuthorityIsOff(t *testing.T) {
	withTempRuntime(t)
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "0")
	if err := ensureServiceProfileForSetup(); err != nil {
		t.Fatalf("ensureServiceProfileForSetup: %v", err)
	}
	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	if _, err := service.LoadFile(path); !errors.Is(err, service.ErrNoProfile) {
		t.Errorf("an opted-out setup wrote %s (LoadFile = %v); the profile is ignored in legacy mode", path, err)
	}
}

// The end-to-end wiring, through the command body every target-backed subcommand
// runs. A registration is what a `go install` user actually executes, so the
// profile write has to be on THIS path and not only on `plumb service install`.
func TestSetupTargetRecordsTheServiceProfile(t *testing.T) {
	serviceInstallEnv(t)
	t.Setenv("HOME", t.TempDir()) // where CodexConfigPath resolves
	if err := runSetupTarget(codexTarget); err != nil {
		t.Fatalf("runSetupTarget(codex): %v", err)
	}
	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	if _, err := service.LoadFile(path); err != nil {
		t.Fatalf("`plumb setup codex` did not leave a service profile: %v", err)
	}
}

// TestInstallPathsRecordTheServiceProfile pins the two install paths that do not
// go through `plumb setup`: `make install` and the Homebrew formula's
// post_install. Neither can be executed from here (the first installs to a
// prefix, the second only exists inside a release), so their recipes are pinned
// by text — the same way the build-tag guards are — because a recipe that
// silently stopped recording the profile would brick the next cold start on a
// non-Homebrew install and nothing else in the suite would notice.
func TestInstallPathsRecordTheServiceProfile(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	recipe := targetRecipe(string(makefile), "install:")
	if recipe == "" {
		t.Fatal("the Makefile has no install: target to check")
	}
	if !strings.Contains(recipe, "service install") {
		t.Errorf("the install: target does not record the service profile:\n%s", recipe)
	}

	release, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yml"))
	if err != nil {
		t.Fatalf("reading .goreleaser.yml: %v", err)
	}
	post := blockAfter(string(release), "post_install: |")
	if post == "" {
		t.Fatal("the brews block has no post_install: Homebrew's upgrade path would leave no profile behind")
	}
	if !strings.Contains(post, "service") || !strings.Contains(post, "install") {
		t.Errorf("the post_install does not run `plumb service install`:\n%s", post)
	}
	if !strings.Contains(post, `"#{bin}/plumb"`) {
		t.Errorf("the post_install does not invoke the installed binary:\n%s", post)
	}
}

// targetRecipe returns the lines of a Make target's recipe, up to the next
// target or variable definition.
func targetRecipe(makefile, target string) string {
	lines := strings.Split(makefile, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, target) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	var out []string
	for _, l := range lines[start:] {
		if l != "" && !strings.HasPrefix(l, "\t") && !strings.HasPrefix(l, "#") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// blockAfter returns the indented block that follows a YAML key line.
func blockAfter(doc, key string) string {
	lines := strings.Split(doc, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), key) {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		var out []string
		for _, b := range lines[i+1:] {
			if strings.TrimSpace(b) == "" {
				out = append(out, b)
				continue
			}
			if len(b)-len(strings.TrimLeft(b, " ")) <= indent {
				break
			}
			out = append(out, b)
		}
		return strings.Join(out, "\n")
	}
	return ""
}
