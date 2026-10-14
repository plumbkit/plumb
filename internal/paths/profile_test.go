package paths

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/service"
)

// withServiceProfile isolates this test the way the daemon's account is
// isolated: a seam root that is also HOME, every XDG variable cleared, and — in
// the canonical tests — a profile recorded at the seam's own location.
func withServiceProfile(t *testing.T, canonical bool) (account.User, service.Profile) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PLUMB_TEST_ACCOUNT_ROOT", root)
	for _, v := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(v, "")
	}
	if canonical {
		t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "1")
	} else {
		t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "")
	}
	u, err := account.Current()
	if err != nil {
		t.Fatalf("account.Current: %v", err)
	}

	p := service.Default(u, "/installed/plumb", "/installed/bin:/usr/bin")
	p = p.Adopt(u, service.XDG{
		ConfigHome: filepath.Join(root, "adopted", "config"),
		DataHome:   filepath.Join(root, "adopted", "data"),
		StateHome:  filepath.Join(root, "adopted", "state"),
		CacheHome:  filepath.Join(root, "adopted", "cache"),
		RuntimeDir: filepath.Join(root, "adopted", "run"),
	})
	// The adopted runtime base must pass the reader's own XDG checks (a directory
	// this uid owns, mode 0700), as a real adopted one does (PLAN-550).
	if err := os.MkdirAll(p.XDG.RuntimeDir, 0o700); err != nil {
		t.Fatalf("creating the adopted runtime base: %v", err)
	}
	if err := os.Chmod(p.XDG.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	if err := p.Write(path); err != nil {
		t.Fatalf("writing the test profile: %v", err)
	}
	return u, p
}

// TestRuntimeDir_RefusesAnUnusableRecordedBase pins the reader's re-check of a
// recorded XDG_RUNTIME_DIR (PLAN-550). The writers refuse such a base; a profile
// edited by hand must not get one past every reader, because the daemon binds its
// sockets under it. A base another user could write (0777), and one that does
// not exist, are both refused: the runtime directory is the account's own, and
// the REST of the profile still holds — a bad runtime base does not move the
// config directory too.
func TestRuntimeDir_RefusesAnUnusableRecordedBase(t *testing.T) {
	for name, spoil := range map[string]func(t *testing.T, base string){
		"world-writable": func(t *testing.T, base string) {
			t.Helper()
			if err := os.Chmod(base, 0o777); err != nil { //nolint:gosec // G302: the unsafe mode IS the case under test
				t.Fatal(err)
			}
		},
		"missing": func(t *testing.T, base string) {
			t.Helper()
			if err := os.RemoveAll(base); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			u, p := withServiceProfile(t, true)
			if got, want := RuntimeDir(), service.DirUnder(p.XDG.RuntimeDir); got != want {
				t.Fatalf("control: a usable recorded base must be used: runtime dir = %q, want %q", got, want)
			}
			spoil(t, p.XDG.RuntimeDir)
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			runtimeBaseWarn = sync.Once{}
			_ = RuntimeDir()
			if got := RuntimeDir(); got != u.RuntimeDir() {
				t.Errorf("runtime dir = %q, want the account's own %q: the recorded base %s is not usable", got, u.RuntimeDir(), p.XDG.RuntimeDir)
			}
			// The refusal is said, once per process, with the remedy.
			if n := strings.Count(logs.String(), "is not a usable runtime directory"); n != 1 {
				t.Errorf("the refusal was logged %d times, want once:\n%s", n, logs.String())
			}
			if !strings.Contains(logs.String(), "set-xdg --runtime-dir=") {
				t.Errorf("the warning must name the remedy:\n%s", logs.String())
			}
			if got := ConfigDir(); got != p.Dirs.Config {
				t.Errorf("config dir = %q, want the profile's %q: a bad runtime base must not move the rest", got, p.Dirs.Config)
			}
		})
	}
}

// Legacy mode is every shipped build until the flip: the profile is IGNORED
// entirely, so its recorded directories must not appear anywhere.
func TestDirs_LegacyIgnoresTheServiceProfile(t *testing.T) {
	_, p := withServiceProfile(t, false)

	for _, role := range service.Roles() {
		got := roleDir(role)
		want := envDir(role)
		if got != want {
			t.Errorf("%s dir = %q, want the environment's %q in legacy mode", role, got, want)
		}
		if got == p.Dirs.Dir(role) && want != p.Dirs.Dir(role) {
			t.Errorf("%s dir = %q: legacy mode read the service profile", role, got)
		}
	}
	if got, want := RuntimeDir(), filepath.Join(runtimeBase(), appDir); got != want {
		t.Errorf("runtime dir = %q, want the environment's %q in legacy mode", got, want)
	}
}

// Canonical mode resolves every global directory through the profile: that is
// what makes a client's HOME or XDG unable to retarget anything.
func TestDirs_CanonicalResolvesThroughTheProfile(t *testing.T) {
	u, p := withServiceProfile(t, true)

	for _, role := range service.Roles() {
		if got := roleDir(role); got != p.Dirs.Dir(role) {
			t.Errorf("%s dir = %q, want the profile's %q", role, got, p.Dirs.Dir(role))
		}
	}
	// A profile is not a no-op here: these tests would pass vacuously if the
	// adopted directories happened to be the account's own.
	if p.Dirs.Config == u.ConfigDir() {
		t.Fatal("the test profile records the account default; it cannot tell the two apart")
	}
	if got, want := RuntimeDir(), service.DirUnder(p.XDG.RuntimeDir); got != want {
		t.Errorf("runtime dir = %q, want the adopted %q", got, want)
	}
}

// A profile that records no on-disk location for a role leaves the account
// standard path in place — that is the profile's documented DEFAULT, not an
// inference. Only a LAUNCH refuses without a profile, because only a launch
// needs to name a binary.
func TestDirs_CanonicalWithoutAProfileUsesTheAccountDefaults(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PLUMB_TEST_ACCOUNT_ROOT", root)
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "1")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "client", "config"))

	u, err := account.Current()
	if err != nil {
		t.Fatalf("account.Current: %v", err)
	}
	if _, err := service.Path(); err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	if got := ConfigDir(); got != u.ConfigDir() {
		t.Errorf("config dir = %q, want the account's %q — and never the client's %q",
			got, u.ConfigDir(), envDir(service.RoleConfig))
	}
}

// The divergence report is what tells a user their environment was ignored: it
// names both answers, so the remedy can name the directory to adopt.
func TestClientEnvDivergence_NamesBothAnswers(t *testing.T) {
	_, p := withServiceProfile(t, true)
	clientConfig := filepath.Join(t.TempDir(), "client-config")
	t.Setenv("XDG_CONFIG_HOME", clientConfig)

	got := ClientEnvDivergence()
	var config *Divergence
	for i := range got {
		if got[i].Role == string(service.RoleConfig) {
			config = &got[i]
		}
	}
	if config == nil {
		t.Fatalf("no config divergence reported for an XDG_CONFIG_HOME the service ignores: %v", got)
	}
	if want := service.DirUnder(clientConfig); config.Env != want {
		t.Errorf("reported client directory = %q, want %q", config.Env, want)
	}
	if config.Service != p.Dirs.Config {
		t.Errorf("reported service directory = %q, want the profile's %q", config.Service, p.Dirs.Config)
	}
}

func TestClientEnvDivergence_IsEmptyInLegacyMode(t *testing.T) {
	withServiceProfile(t, false)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "client-config"))
	if got := ClientEnvDivergence(); len(got) != 0 {
		t.Errorf("legacy mode reported %d divergences: %v", len(got), got)
	}
}

// roleDir is the public accessor for a role, so one loop can check them all.
func roleDir(role service.Role) string {
	switch role {
	case service.RoleConfig:
		return ConfigDir()
	case service.RoleData:
		return DataDir()
	case service.RoleState:
		return StateDir()
	case service.RoleLog:
		return LogDir()
	case service.RoleCache:
		return CacheDir()
	default:
		return ""
	}
}

// TestEnvVarForRole_NamesTheVariableThatMovesARole pins the mapping the
// installer adopts by: a role with no XDG variable must not silently acquire
// one, and a role with one must name the variable that actually moves it.
func TestEnvVarForRole_NamesTheVariableThatMovesARole(t *testing.T) {
	want := map[service.Role]string{
		service.RoleConfig: "XDG_CONFIG_HOME",
		service.RoleData:   "XDG_DATA_HOME",
		service.RoleState:  "XDG_STATE_HOME",
		service.RoleCache:  "XDG_CACHE_HOME",
	}
	if runtime.GOOS == "darwin" {
		want[service.RoleLog] = ""
	} else {
		want[service.RoleLog] = "XDG_STATE_HOME"
	}
	for role, name := range want {
		if got := EnvVarForRole(role); got != name {
			t.Errorf("EnvVarForRole(%s) = %q, want %q", role, got, name)
		}
	}
}

// The installer must never adopt a variable that is not an absolute path: a
// relative one would resolve against whichever process read the profile.
func TestRoleEnvOverride_RefusesARelativeValue(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	if _, _, ok := RoleEnvOverride(service.RoleConfig); ok {
		t.Error("a relative XDG_CONFIG_HOME was offered for adoption")
	}
	abs := filepath.Join(t.TempDir(), "config")
	t.Setenv("XDG_CONFIG_HOME", abs)
	name, value, ok := RoleEnvOverride(service.RoleConfig)
	if !ok || name != "XDG_CONFIG_HOME" || value != abs {
		t.Errorf("RoleEnvOverride = %q, %q, %v; want the absolute override", name, value, ok)
	}
}
