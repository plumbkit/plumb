package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/service"
)

// `plumb service env` and install's seeding (PLAN-554), and the invariant the
// whole section rests on: no entry can move the service.

// fakeShell is the configuring shell's environment, for the capture paths.
func fakeShell(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func loadTestProfile(t *testing.T) service.Profile {
	t.Helper()
	p, err := service.Load()
	if err != nil {
		t.Fatalf("service.Load: %v", err)
	}
	return p
}

// installClean installs a profile and empties its [service.env], so a test starts
// from a known section whatever the test process's own environment holds.
func installClean(t *testing.T) string {
	t.Helper()
	serviceInstallEnv(t)
	if err := installServiceProfile(false); err != nil {
		t.Fatalf("installServiceProfile: %v", err)
	}
	path, err := service.Path()
	if err != nil {
		t.Fatal(err)
	}
	p := loadTestProfile(t)
	p.Env = service.Env{Seeded: true}
	if err := p.Write(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServiceEnvSet_RecordsALiteralAndACapturedSymbol(t *testing.T) {
	installClean(t)
	shell := fakeShell(map[string]string{"SSH_AUTH_SOCK": "/agent.sock"})
	if err := setServiceEnv("GOPRIVATE=github.com/acme/*", shell); err != nil {
		t.Fatalf("set literal: %v", err)
	}
	if err := setServiceEnv("SSH_AUTH_SOCK", shell); err != nil {
		t.Fatalf("set symbol: %v", err)
	}
	if err := setServiceEnv("HTTPS_PROXY", shell); err != nil {
		t.Fatalf("set uncaptured symbol: %v", err)
	}
	env := loadTestProfile(t).Env
	want := map[string]service.EnvEntry{
		"GOPRIVATE":     {Name: "GOPRIVATE", Value: "github.com/acme/*"},
		"SSH_AUTH_SOCK": {Name: "SSH_AUTH_SOCK", Value: "/agent.sock", Inherit: true, Captured: true},
		"HTTPS_PROXY":   {Name: "HTTPS_PROXY", Inherit: true},
	}
	for name, w := range want {
		if got, ok := env.Lookup(name); !ok || got != w {
			t.Errorf("%s = %+v (recorded %v), want %+v", name, got, ok, w)
		}
	}
}

// TestServiceEnvSet_RefusesADeniedName: refused with the reason, and the file is
// not touched.
func TestServiceEnvSet_RefusesADeniedName(t *testing.T) {
	path := installClean(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"HOME=/x", "PATH=/x", "XDG_CONFIG_HOME=/x", "PLUMB_ACCOUNT_AUTHORITY=0", "LOGNAME", "TSM_ORIG_XDG_DATA_HOME=/x", "NOT-A-NAME=1"} {
		err := setServiceEnv(arg, fakeShell(nil))
		if err == nil {
			t.Errorf("env set %s was accepted", arg)
			continue
		}
		name, _, _ := strings.Cut(arg, "=")
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal for %s does not name it: %v", arg, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a refused env set changed the profile")
	}
}

func TestServiceEnvUnsetAndRefresh(t *testing.T) {
	installClean(t)
	if err := setServiceEnv("GOPROXY=https://literal", fakeShell(nil)); err != nil {
		t.Fatal(err)
	}
	if err := setServiceEnv("SSL_CERT_FILE", fakeShell(map[string]string{"SSL_CERT_FILE": "/old.pem"})); err != nil {
		t.Fatal(err)
	}
	// The certificate moved: refresh re-captures the symbol, and leaves the
	// literal — the user's own value — exactly as it was.
	if err := refreshServiceEnv(fakeShell(map[string]string{"SSL_CERT_FILE": "/new.pem", "GOPROXY": "from-shell"})); err != nil {
		t.Fatal(err)
	}
	env := loadTestProfile(t).Env
	if e, _ := env.Lookup("SSL_CERT_FILE"); e.Value != "/new.pem" || !e.Captured {
		t.Errorf("refresh did not re-capture SSL_CERT_FILE: %+v", e)
	}
	if e, _ := env.Lookup("GOPROXY"); e.Value != "https://literal" || e.Inherit {
		t.Errorf("refresh touched the literal GOPROXY: %+v", e)
	}
	if err := unsetServiceEnv("GOPROXY"); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadTestProfile(t).Env.Lookup("GOPROXY"); ok {
		t.Error("unset left GOPROXY recorded")
	}
	if err := unsetServiceEnv("GOPROXY"); err == nil {
		t.Error("unsetting a name that is not recorded must say so")
	}
}

// TestServiceInstall_SeedsOnceAndPreserves: the first install seeds the default
// set from the installing shell; a later install never replaces an entry the
// user set, and never seeds back a name the user unset.
func TestServiceInstall_SeedsOnceAndPreserves(t *testing.T) {
	serviceInstallEnv(t)
	for _, name := range service.DefaultEnvNames() {
		t.Setenv(name, "")
	}
	t.Setenv("SSH_AUTH_SOCK", "/install/agent.sock")
	t.Setenv("GOPRIVATE", "github.com/install/*")
	if err := installServiceProfile(false); err != nil {
		t.Fatal(err)
	}
	env := loadTestProfile(t).Env
	if !env.Seeded {
		t.Fatal("the first install did not record that it seeded")
	}
	for _, name := range service.DefaultEnvNames() {
		if _, ok := env.Lookup(name); !ok {
			t.Errorf("the first install did not seed %s", name)
		}
	}
	if e, _ := env.Lookup("SSH_AUTH_SOCK"); e.Value != "/install/agent.sock" || !e.Captured {
		t.Errorf("SSH_AUTH_SOCK was not captured from the installing shell: %+v", e)
	}
	if e, _ := env.Lookup("HTTPS_PROXY"); e.Captured {
		t.Errorf("HTTPS_PROXY was empty in the installing shell but is captured: %+v", e)
	}

	// The user overrides one and drops another; an upgrade's install runs again in
	// a shell where both differ.
	if err := setServiceEnv("GOPRIVATE=github.com/mine/*", fakeShell(nil)); err != nil {
		t.Fatal(err)
	}
	if err := unsetServiceEnv("SSH_AUTH_SOCK"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOPRIVATE", "github.com/upgrade/*")
	t.Setenv("SSH_AUTH_SOCK", "/upgrade/agent.sock")
	if err := installServiceProfile(false); err != nil {
		t.Fatal(err)
	}
	env = loadTestProfile(t).Env
	if e, _ := env.Lookup("GOPRIVATE"); e.Value != "github.com/mine/*" {
		t.Errorf("install replaced the user's GOPRIVATE: %+v", e)
	}
	if _, ok := env.Lookup("SSH_AUTH_SOCK"); ok {
		t.Error("install seeded back SSH_AUTH_SOCK, which the user unset")
	}
}

// TestServiceInstall_SeedsAProfileFromBeforeTheSection: a profile written before
// [service.env] existed is seeded by the next install (the upgrade), once.
func TestServiceInstall_SeedsAProfileFromBeforeTheSection(t *testing.T) {
	path := installClean(t)
	p := loadTestProfile(t)
	p.Env = service.Env{}
	if err := p.Write(path); err != nil {
		t.Fatal(err)
	}
	// The user recorded one before the section was ever seeded: the seeding must
	// preserve it, not replace it with the installing shell's value.
	if err := setServiceEnv("GOPRIVATE=github.com/mine/*", fakeShell(nil)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOPRIVATE", "github.com/upgrade/*")
	t.Setenv("GONOSUMDB", "github.com/acme")
	if err := installServiceProfile(false); err != nil {
		t.Fatal(err)
	}
	env := loadTestProfile(t).Env
	if e, _ := env.Lookup("GONOSUMDB"); e.Value != "github.com/acme" {
		t.Errorf("the upgrade did not seed a pre-[service.env] profile: %+v", e)
	}
	if e, _ := env.Lookup("GOPRIVATE"); e.Value != "github.com/mine/*" || e.Inherit {
		t.Errorf("the first seeding replaced the user's GOPRIVATE: %+v", e)
	}
}

const serviceEnvHelperEnv = "PLUMB_TEST_SERVICE_ENV_HELPER"

// TestHelperServiceEnvResolvedPaths is not a test: run as a child under a managed
// launch's environment, it prints every location the daemon resolves, and the
// variables a [service.env] entry should (or should not) have put there.
func TestHelperServiceEnvResolvedPaths(t *testing.T) {
	if os.Getenv(serviceEnvHelperEnv) != "1" {
		t.Skip("helper process")
	}
	auth, err := authorityPath()
	fmt.Printf("authority=%s err=%v\n", auth, err)
	for _, kv := range [][2]string{
		{"runtime", plumbRuntimeDir()},
		{"socket", daemonSocketPath()},
		{"ctrl", daemonCtrlSocketPath()},
		{"pid", daemonPIDPath()},
		{"lock", daemonLockPath()},
		{"log", daemonLogPath()},
		{"config", paths.ConfigDir()},
		{"data", paths.DataDir()},
		{"state", paths.StateDir()},
		{"logdir", paths.LogDir()},
		{"cache", paths.CacheDir()},
	} {
		fmt.Printf("%s=%s\n", kv[0], kv[1])
	}
	for _, name := range []string{"GOPROXY", "SSH_AUTH_SOCK", "HTTPS_PROXY", "CLIENT_ONLY"} {
		v, ok := os.LookupEnv(name)
		fmt.Printf("env.%s=%q set=%v\n", name, v, ok)
	}
}

// resolveInChild runs the helper under the environment a managed launch builds
// from the account's recorded profile, and returns what it printed.
func resolveInChild(t *testing.T, u account.User) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperServiceEnvResolvedPaths$", "-test.count=1", "-test.v")
	cmd.Env = append(loadTestProfile(t).LaunchEnv(u), serviceEnvHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	var keep []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, "=") && !strings.HasPrefix(line, "=== ") && !strings.HasPrefix(line, "--- ") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// TestServiceEnv_NoEntryCanMoveTheService is the card's acceptance invariant: no
// key in [service.env] can alter the authority directory, the socket and lock
// paths, or the daemon's runtime/data/state/log/cache directories. The profile is
// hand-edited with a hostile value for every name that steers resolution (the
// deny list, which the reader must drop) and for the names a corporate machine
// sets (which must pass and move nothing), and a child started with the launch
// environment resolves exactly what it resolved without them. The same child
// shows the entries that SHOULD arrive did, and that a value only the spawning
// client had did not.
func TestServiceEnv_NoEntryCanMoveTheService(t *testing.T) {
	path := installClean(t)
	u, err := account.Current()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIENT_ONLY", "from-the-spawning-client")
	t.Setenv("HTTPS_PROXY", "from-the-spawning-client")
	baseline := resolveInChild(t, u)
	for _, key := range []string{"authority=", "socket=", "lock=", "runtime=", "config=", "cache="} {
		if !strings.Contains(baseline, key) {
			t.Fatalf("the helper did not report %s: the comparison below would be vacuous\n%s", key, baseline)
		}
	}

	hostile := []string{
		"HOME", "USER", "LOGNAME", "PATH", "TMPDIR",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
		"PLUMB_TEST_ACCOUNT_ROOT", "PLUMB_ACCOUNT_AUTHORITY", "PLUMB_SEPARATE_ENV",
		"TSM_ORIG_XDG_CONFIG_HOME", "TSM_ORIG_XDG_DATA_HOME", "TSM_ORIG_XDG_STATE_HOME", "TSM_ORIG_XDG_CACHE_HOME", "TSM_ORIG_XDG_RUNTIME_DIR",
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// installClean recorded an empty but SEEDED section; the hand-edit replaces it.
	data = []byte(strings.Split(string(data), "\n[service]")[0])
	var b strings.Builder
	b.Write(data)
	b.WriteString("\n[service.env]\nGOPROXY = \"https://literal.example\"\n")
	b.WriteString("SSH_AUTH_SOCK = { inherit = true, value = \"/captured/agent.sock\" }\n")
	b.WriteString("HTTPS_PROXY = { inherit = true }\n")
	for _, name := range hostile {
		fmt.Fprintf(&b, "%s = %q\n", name, t.TempDir())
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := resolveInChild(t, u)

	for line := range strings.SplitSeq(baseline, "\n") {
		if strings.HasPrefix(line, "env.") {
			continue
		}
		if !strings.Contains(got, line) {
			t.Errorf("a [service.env] entry moved the service: %q is gone\nbaseline:\n%s\nwith entries:\n%s", line, baseline, got)
		}
	}
	for _, want := range []string{
		`env.GOPROXY="https://literal.example" set=true`,
		`env.SSH_AUTH_SOCK="/captured/agent.sock" set=true`,
		`env.HTTPS_PROXY="" set=false`,
		`env.CLIENT_ONLY="" set=false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the child's environment lacks %s:\n%s", want, got)
		}
	}
}
