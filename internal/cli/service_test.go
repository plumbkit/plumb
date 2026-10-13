package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/service"
)

// serviceInstallEnv is the environment `plumb service install` runs in for
// these tests: the account seam, the gate on, and a PATH worth recording.
func serviceInstallEnv(t *testing.T) account.User {
	t.Helper()
	withTempRuntime(t)
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "1")
	t.Setenv("PATH", "/install/bin:/usr/bin")
	for _, v := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(v, "")
	}
	u, err := account.Current()
	if err != nil {
		t.Fatalf("account.Current: %v", err)
	}
	return u
}

func TestServiceInstall_RecordsTheAccountProfile(t *testing.T) {
	u := serviceInstallEnv(t)
	if err := runServiceInstall(nil, nil); err != nil {
		t.Fatalf("runServiceInstall: %v", err)
	}
	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	got, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if got.Executable != exe {
		t.Errorf("executable = %q, want the installing binary %q", got.Executable, exe)
	}
	if got.Path != "/install/bin:/usr/bin" {
		t.Errorf("path = %q, want the installing environment's PATH", got.Path)
	}
	want := service.Default(u, exe, "/install/bin:/usr/bin")
	if got.Dirs != want.Dirs {
		t.Errorf("dirs = %+v, want the account standard directories %+v", got.Dirs, want.Dirs)
	}
	if got.XDG != (service.XDG{}) {
		t.Errorf("XDG = %+v, want nothing adopted: this environment sets no override", got.XDG)
	}
}

// The reviewer's binding requirement: an install is what a package manager runs
// after EVERY upgrade, so it must update the executable and touch nothing else.
// A user's adopted directories, PATH and XDG overrides are theirs.
func TestServiceInstall_IsIdempotentAndPreservesServiceSettings(t *testing.T) {
	serviceInstallEnv(t)
	if err := runServiceInstall(nil, nil); err != nil {
		t.Fatalf("first install: %v", err)
	}
	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}

	// The user changes two settings through the supported commands.
	if err := setProfilePath("/user/bin:/usr/bin"); err != nil {
		t.Fatalf("setProfilePath: %v", err)
	}
	configBase := t.TempDir()
	if err := setXDGProfile(map[string]string{"XDG_CONFIG_HOME": configBase}); err != nil {
		t.Fatalf("setXDGProfile: %v", err)
	}
	before, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	// An upgrade: the recorded executable is the PREVIOUS install's path.
	before.Executable = "/old/cellar/plumb"
	if err := before.Write(path); err != nil {
		t.Fatalf("writing the pre-upgrade profile: %v", err)
	}

	if err := runServiceInstall(nil, nil); err != nil {
		t.Fatalf("install after the upgrade: %v", err)
	}
	after, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if after.Executable != exe {
		t.Errorf("executable = %q, want the newly installed binary %q", after.Executable, exe)
	}
	if after.Path != before.Path {
		t.Errorf("path = %q, want the user's %q: an install must not reset it", after.Path, before.Path)
	}
	if after.XDG != before.XDG {
		t.Errorf("XDG = %+v, want the user's %+v: an install must not reset it", after.XDG, before.XDG)
	}
	if after.Dirs != before.Dirs {
		t.Errorf("dirs = %+v, want the user's %+v: an install must not reset them", after.Dirs, before.Dirs)
	}

	// And running it again, with nothing to update, changes not one byte.
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := runServiceInstall(nil, nil); err != nil {
		t.Fatalf("second install: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("a second install rewrote the profile; it must be idempotent")
	}
}

func TestServiceSetXDG_RecordsAndDropsAnOverride(t *testing.T) {
	u := serviceInstallEnv(t)
	if err := runServiceInstall(nil, nil); err != nil {
		t.Fatalf("runServiceInstall: %v", err)
	}
	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	original, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	base := t.TempDir()
	if err := setXDGProfile(map[string]string{"XDG_STATE_HOME": base}); err != nil {
		t.Fatalf("setXDGProfile: %v", err)
	}
	adopted, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if adopted.XDG.StateHome != base {
		t.Errorf("XDG_STATE_HOME = %q, want %q", adopted.XDG.StateHome, base)
	}
	if want := service.DirUnder(base); adopted.Dirs.State != want {
		t.Errorf("state dir = %q, want %q — the override must move the directory it governs", adopted.Dirs.State, want)
	}
	if adopted.Executable != original.Executable || adopted.Path != original.Path || adopted.Dirs.Config != original.Dirs.Config {
		t.Errorf("set-xdg changed settings it does not own:\n got %+v\nwant executable/path/config as %+v", adopted, original)
	}

	// Dropping it puts the role back on the account standard path.
	if err := setXDGProfile(map[string]string{"XDG_STATE_HOME": ""}); err != nil {
		t.Fatalf("setXDGProfile (drop): %v", err)
	}
	dropped, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if dropped.XDG.StateHome != "" {
		t.Errorf("XDG_STATE_HOME = %q, want it dropped", dropped.XDG.StateHome)
	}
	if dropped.Dirs.State != u.StateDir() {
		t.Errorf("state dir = %q, want the account's %q", dropped.Dirs.State, u.StateDir())
	}
	if dropped.Path != original.Path || dropped.Executable != original.Executable {
		t.Error("dropping an override changed unrelated settings")
	}
}

func TestMutateProfile_RefusesWithoutAProfile(t *testing.T) {
	serviceInstallEnv(t)
	err := setProfilePath("/usr/bin")
	if err == nil {
		t.Fatal("a set-* without a profile must refuse")
	}
	if !strings.Contains(err.Error(), "plumb service install") {
		t.Errorf("the refusal does not name the command that creates one: %v", err)
	}
	if err := setXDGProfile(map[string]string{"XDG_CONFIG_HOME": ""}); err == nil {
		t.Fatal("set-xdg without a profile must refuse")
	}
}

func TestSetExecutable_RefusesAPathThatCannotBeRun(t *testing.T) {
	serviceInstallEnv(t)
	if err := runServiceInstall(nil, nil); err != nil {
		t.Fatalf("runServiceInstall: %v", err)
	}
	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	before, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	dir := t.TempDir()
	notExecutable := filepath.Join(dir, "plumb")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatalf("writing the non-executable: %v", err)
	}
	for _, arg := range []string{"relative/plumb", filepath.Join(dir, "gone"), dir, notExecutable} {
		if err := setProfileExecutable(arg); err == nil {
			t.Errorf("set-executable accepted %q", arg)
		}
	}
	// Nothing was recorded by the refusals.
	after, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if after != before {
		t.Errorf("a refused set-executable changed the profile:\n got %+v\nwant %+v", after, before)
	}

	executable := filepath.Join(dir, "real-plumb")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: a test's own script must be executable
		t.Fatalf("writing the executable: %v", err)
	}
	if err := setProfileExecutable(executable); err != nil {
		t.Fatalf("set-executable refused a runnable absolute path: %v", err)
	}
	updated, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if updated.Executable != executable {
		t.Errorf("executable = %q, want %q", updated.Executable, executable)
	}
	if updated.Path != before.Path || updated.Dirs != before.Dirs {
		t.Error("set-executable changed settings it does not own")
	}
}

// Adoption is what keeps a real XDG setup: the installer records an override
// only when it names an existing plumb directory that the account default does
// not already cover. A temp directory a session manager invented holds no plumb
// state, so it is never adopted — and neither is an override that changes
// nothing.
func TestAdoptXDG_AdoptsOnlyARealPlumbSetup(t *testing.T) {
	u := serviceInstallEnv(t)
	def := service.Default(u, "/installed/plumb", "/usr/bin").Dirs

	adopted := t.TempDir()
	if err := os.MkdirAll(service.DirUnder(adopted), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	empty := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", adopted)
	t.Setenv("XDG_DATA_HOME", empty)
	t.Setenv("XDG_CACHE_HOME", filepath.Dir(u.CacheDir())) // resolves to the account default

	got := adoptXDG(u, def)
	if got.ConfigHome != adopted {
		t.Errorf("XDG_CONFIG_HOME = %q, want the real setup %q", got.ConfigHome, adopted)
	}
	if got.DataHome != "" {
		t.Errorf("XDG_DATA_HOME = %q, want nothing: that directory holds no plumb state", got.DataHome)
	}
	if got.CacheHome != "" {
		t.Errorf("XDG_CACHE_HOME = %q, want nothing: it resolves where the account already is", got.CacheHome)
	}

	// The runtime directory is adopted on the XDG spec's own terms, because a
	// runtime directory is empty by design.
	run := t.TempDir()
	if err := os.Chmod(run, 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", run)
	if got := adoptXDG(u, def); got.RuntimeDir != run {
		t.Errorf("XDG_RUNTIME_DIR = %q, want the usable runtime base %q", got.RuntimeDir, run)
	}
	if err := os.Chmod(run, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if got := adoptXDG(u, def); got.RuntimeDir != "" {
		t.Errorf("XDG_RUNTIME_DIR = %q, want nothing: a world-readable base is not a runtime directory", got.RuntimeDir)
	}
}

// The command surface itself: the subcommands a user types, and the flag-to-XDG
// mapping behind `set-xdg`. It is driven through the real cobra command so a
// renamed or unwired flag fails here rather than in a user's shell.
func TestServiceCmd_HasItsSubcommands(t *testing.T) {
	names := make([]string, 0, len(serviceCmd.Commands()))
	for _, c := range serviceCmd.Commands() {
		names = append(names, c.Name())
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"install", "show", "set-executable", "set-path", "set-xdg"} {
		if !strings.Contains(joined, want) {
			t.Errorf("subcommand %q is not registered; got %v", want, names)
		}
	}
	registered := false
	for _, c := range rootCmd.Commands() {
		if c.Name() == "service" {
			registered = true
		}
	}
	if !registered {
		t.Error("`plumb service` is not registered on the root command")
	}
	for _, f := range serviceSetXDGFlags {
		if serviceVarFor(f.name) == "" {
			t.Errorf("flag --%s records no XDG variable", f.name)
		}
	}
}

func TestServiceSetXDG_FlagWiring(t *testing.T) {
	u := serviceInstallEnv(t)
	if err := runServiceInstall(nil, nil); err != nil {
		t.Fatalf("runServiceInstall: %v", err)
	}
	base := t.TempDir()

	// Driven through the ROOT command, because that is how a user reaches it:
	// cobra resolves the subcommand from the root's arguments, and the root's own
	// pre-run (theme, logo) then runs exactly as it does in a shell.
	rootCmd.SetArgs([]string{"service", "set-xdg", "--state-home", base})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("plumb service set-xdg --state-home: %v", err)
	}

	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	got, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got.XDG.StateHome != base {
		t.Errorf("XDG_STATE_HOME = %q, want %q", got.XDG.StateHome, base)
	}
	if want := service.DirUnder(base); got.Dirs.State != want {
		t.Errorf("state dir = %q, want %q", got.Dirs.State, want)
	}
	if got.Path == "" {
		t.Errorf("set-xdg left the PATH empty; it must not touch an unrelated setting (%v)", u.UID)
	}
}
