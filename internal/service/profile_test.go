package service

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/account"
)

func testUser() account.User {
	return account.User{UID: 501, Home: "/home/tester", Name: "tester"}
}

func testProfile() Profile {
	return Profile{
		Executable: "/opt/plumb/bin/plumb",
		Path:       "/opt/plumb/bin:/usr/bin:/bin",
		Dirs: Dirs{
			Config: "/home/tester/.config/plumb",
			Data:   "/home/tester/.local/share/plumb",
			State:  "/home/tester/.local/state/plumb",
			Log:    "/home/tester/.local/state/plumb",
			Cache:  "/home/tester/.cache/plumb",
		},
		XDG: XDG{ConfigHome: "/data/xdg/config", RuntimeDir: "/data/xdg/run"},
	}
}

func TestProfile_WriteLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "service.toml")
	want := testProfile()
	if err := want.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got != want {
		t.Errorf("round trip changed the profile:\n got %+v\nwant %+v", got, want)
	}

	// Service-owned means private: the file records which binary the account's
	// ONE daemon runs and where it keeps its state.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("profile mode = %04o, want 0600", perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasPrefix(string(data), "# plumb service launch profile.") {
		t.Errorf("the file does not say who owns it:\n%s", data)
	}
	if !strings.Contains(string(data), "plumb service set-*") {
		t.Errorf("the file does not name the way to change it:\n%s", data)
	}
}

// The whole point of the profile: a missing one is NEVER inferred around. A
// loader that returned a default profile here — the calling binary, a PATH from
// the environment, a directory from HOME — would reinstate exactly the defect
// the profile exists to remove.
func TestLoadFile_MissingProfileIsNeverInferred(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.toml")
	got, err := LoadFile(path)
	if err == nil {
		t.Fatalf("LoadFile of a missing profile returned %+v, want a refusal", got)
	}
	if !errors.Is(err, ErrNoProfile) {
		t.Errorf("err = %v, want ErrNoProfile", err)
	}
	if got != (Profile{}) {
		t.Errorf("a missing profile produced %+v, want the zero Profile", got)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal does not name the file it looked for: %v", err)
	}
}

func TestLoadFile_RefusesAnUnusableProfile(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{"no executable", func(p *Profile) { p.Executable = "" }, "executable"},
		{"a relative executable", func(p *Profile) { p.Executable = "plumb" }, "absolute"},
		{"no PATH", func(p *Profile) { p.Path = "" }, "PATH"},
		{"a missing directory", func(p *Profile) { p.Dirs.State = "" }, "state"},
		{"a relative directory", func(p *Profile) { p.Dirs.Data = "share/plumb" }, "absolute"},
		{"a relative XDG override", func(p *Profile) { p.XDG.DataHome = "xdg" }, "XDG_DATA_HOME"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testProfile()
			tt.mutate(&p)
			// Write validates too, so plant the bytes directly: the READER must
			// refuse them as well, because a hand-edited file is the case this
			// guards.
			path := filepath.Join(t.TempDir(), "service.toml")
			data, merr := tomlForTest(p)
			if merr != nil {
				t.Fatalf("encoding the test profile: %v", merr)
			}
			if werr := os.WriteFile(path, data, 0o600); werr != nil {
				t.Fatalf("planting the profile: %v", werr)
			}
			_, err := LoadFile(path)
			if !errors.Is(err, ErrInvalidProfile) {
				t.Fatalf("LoadFile = %v, want ErrInvalidProfile", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the refusal does not name %q: %v", tt.want, err)
			}
		})
	}
}

// tomlForTest renders a profile without validating it, so a test can plant a
// file the writer would have refused.
func tomlForTest(p Profile) ([]byte, error) {
	body, err := marshalUnchecked(p)
	if err != nil {
		return nil, err
	}
	return append([]byte(header), body...), nil
}

// LaunchEnv is the clean environment a managed launch gives the daemon. A client
// variable that is not in it cannot retarget the daemon's files, its language
// servers, or anything else.
func TestLaunchEnv_CarriesTheProfileAndNothingFromTheClient(t *testing.T) {
	for _, kv := range [][2]string{
		{"HOME", "/spoof/home"},
		{"USER", "spoofed"},
		{"PATH", "/spoof/bin"},
		{"XDG_CONFIG_HOME", "/spoof/config"},
		{"XDG_DATA_HOME", "/spoof/data"},
		{"XDG_STATE_HOME", "/spoof/state"},
		{"XDG_CACHE_HOME", "/spoof/cache"},
		{"XDG_RUNTIME_DIR", "/spoof/run"},
		{"PLUMB_ALLOWED_DIRS", "/spoof"},
		{"PLUMB_WORKSPACE", "/spoof/ws"},
		{"LANG", "en_AU.UTF-8"},
		{"LC_ALL", "en_AU.UTF-8"},
		{"LC_MESSAGES", "en_AU.UTF-8"},
	} {
		t.Setenv(kv[0], kv[1])
	}

	p := testProfile()
	env := p.LaunchEnv(testUser())
	got := envMap(env)

	if got["HOME"] != "/home/tester" {
		t.Errorf("HOME = %q, want the account's home", got["HOME"])
	}
	if got["USER"] != "tester" {
		t.Errorf("USER = %q, want the account's login name", got["USER"])
	}
	if got["PATH"] != p.Path {
		t.Errorf("PATH = %q, want the profile's %q", got["PATH"], p.Path)
	}
	if got["XDG_CONFIG_HOME"] != "/data/xdg/config" {
		t.Errorf("XDG_CONFIG_HOME = %q, want the recorded %q", got["XDG_CONFIG_HOME"], "/data/xdg/config")
	}
	if _, ok := got["XDG_DATA_HOME"]; ok {
		t.Error("an XDG variable the profile does NOT record was carried into the daemon")
	}
	if got["XDG_RUNTIME_DIR"] != "/data/xdg/run" {
		t.Errorf("XDG_RUNTIME_DIR = %q, want the profile's recorded %q", got["XDG_RUNTIME_DIR"], "/data/xdg/run")
	}
	if got["LANG"] != "en_AU.UTF-8" || got["LC_ALL"] != "en_AU.UTF-8" {
		t.Error("the launching session's locale must survive: the daemon's children format messages with it")
	}
	for _, name := range []string{"PLUMB_ALLOWED_DIRS", "PLUMB_WORKSPACE", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s leaked from the client into the daemon's environment (%q)", name, got[name])
		}
	}
}

func TestLaunchEnv_OmitsUSERWhenTheAccountHasNoName(t *testing.T) {
	p := testProfile()
	env := envMap(p.LaunchEnv(account.User{UID: 501, Home: "/home/tester"}))
	if _, ok := env["USER"]; ok {
		t.Error("a nameless account must not produce a USER variable")
	}
	if env["HOME"] != "/home/tester" {
		t.Errorf("HOME = %q, want the account's home", env["HOME"])
	}
}

// The seam variables are carried so a launcher and its child resolve the SAME
// account in a container harness. They are named explicitly rather than by
// copying the environment, which is what keeps every other client variable out.
func TestLaunchEnv_CarriesTheAccountSeam(t *testing.T) {
	t.Setenv("PLUMB_TEST_ACCOUNT_ROOT", "/seam")
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "1")
	t.Setenv("PLUMB_SEPARATE_ENV", "vm")
	env := envMap(testProfile().LaunchEnv(testUser()))
	if env["PLUMB_TEST_ACCOUNT_ROOT"] != "/seam" || env["PLUMB_ACCOUNT_AUTHORITY"] != "1" || env["PLUMB_SEPARATE_ENV"] != "vm" {
		t.Errorf("the account seam did not cross into the daemon's environment: %v", env)
	}
}

func TestAdopt_RecomputesTheDirectoriesItGoverns(t *testing.T) {
	u := testUser()
	p := Default(u, "/opt/plumb", "/usr/bin")
	if p.Dirs.Config != u.ConfigDir() || p.Dirs.Data != u.DataDir() {
		t.Fatalf("Default() = %+v, want the account's standard directories", p.Dirs)
	}

	p = p.Adopt(u, XDG{ConfigHome: "/xdg/config"})
	if want := DirUnder("/xdg/config"); p.Dirs.Config != want {
		t.Errorf("adopted config dir = %q, want %q", p.Dirs.Config, want)
	}
	if p.Dirs.Data != u.DataDir() {
		t.Errorf("data dir = %q, want the account default: an unadopted role follows the account", p.Dirs.Data)
	}

	// Dropping the override puts the role back on the account standard path.
	p = p.Adopt(u, XDG{})
	if p.Dirs.Config != u.ConfigDir() {
		t.Errorf("config dir = %q, want the account default %q", p.Dirs.Config, u.ConfigDir())
	}
	if p.XDG != (XDG{}) {
		t.Errorf("XDG = %+v, want no recorded overrides", p.XDG)
	}
}

// On macOS plumb's logs live under ~/Library/Logs, which no XDG variable governs
// — so an adopted XDG_STATE_HOME must not drag the log directory with it.
func TestAdopt_LogFollowsStateOnlyWhereXDGGovernsIt(t *testing.T) {
	u := testUser()
	p := Default(u, "/opt/plumb", "/usr/bin")
	p = p.Adopt(u, XDG{StateHome: "/xdg/state"})
	if runtime.GOOS == "darwin" {
		if p.Dirs.Log != u.LogDir() {
			t.Errorf("log dir = %q, want the macOS log directory %q", p.Dirs.Log, u.LogDir())
		}
		return
	}
	if want := DirUnder("/xdg/state"); p.Dirs.Log != want {
		t.Errorf("log dir = %q, want %q", p.Dirs.Log, want)
	}
}

func TestRole_EveryRecordedRoleResolves(t *testing.T) {
	p := testProfile()
	for _, role := range Roles() {
		if p.Dirs.Dir(role) == "" {
			t.Errorf("role %q has no directory in a complete profile", role)
		}
	}
	if p.Dirs.Dir(Role("nonsense")) != "" {
		t.Error("an unknown role must resolve to nothing")
	}
}

func TestVars_RecordsOnlyWhatWasAdopted(t *testing.T) {
	var x XDG
	x = x.With("XDG_CONFIG_HOME", "/xdg/config")
	x = x.With("XDG_RUNTIME_DIR", "/xdg/run")
	x = x.With("NOT_AN_XDG_VARIABLE", "/nope")
	got := map[string]string{}
	for _, v := range x.Vars() {
		if v.Value != "" {
			got[v.Name] = v.Value
		}
	}
	if len(got) != 2 || got["XDG_CONFIG_HOME"] != "/xdg/config" || got["XDG_RUNTIME_DIR"] != "/xdg/run" {
		t.Errorf("recorded variables = %v, want exactly the two adopted ones", got)
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	return out
}
