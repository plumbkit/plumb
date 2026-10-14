package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/service"
)

// withCanonicalService puts this test binary in canonical mode: the account
// seam root (withTempRuntime sets it), the gate on, and a profile recorded at
// the seam's own location. It returns the executable the profile names — a real
// script, so a test can PROVE whether anything started it.
func withCanonicalService(t *testing.T) service.Profile {
	t.Helper()
	withTempRuntime(t)
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "1")
	// Short, so a mutant that spawns something gets caught in a test's lifetime
	// rather than waiting out the launcher's real 30 s.
	t.Setenv("PLUMB_DAEMON_START_TIMEOUT", "2")

	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	script := filepath.Join(dir, "plumb")
	body := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // G306: a test's own script must be executable
		t.Fatalf("writing the stand-in executable: %v", err)
	}

	u, err := account.Current()
	if err != nil {
		t.Fatalf("account.Current: %v", err)
	}
	p := service.Default(u, script, "/profile/bin:/usr/bin")
	writeProfile(t, p)
	return p
}

func writeProfile(t *testing.T, p service.Profile) {
	t.Helper()
	path, err := service.Path()
	if err != nil {
		t.Fatalf("service.Path: %v", err)
	}
	if err := p.Write(path); err != nil {
		t.Fatalf("writing the test profile: %v", err)
	}
}

// The refusal this card is built around: with no profile there is NOTHING to
// infer from, so a canonical cold start fails with the install instruction
// rather than quietly launching the calling binary.
func TestResolveLaunchSpec_CanonicalWithoutProfileRefusesToInfer(t *testing.T) {
	withTempRuntime(t)
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "1")

	spec, err := resolveLaunchSpec()
	if err == nil {
		t.Fatalf("resolveLaunchSpec returned %+v, want a refusal", spec)
	}
	if !errors.Is(err, service.ErrNoProfile) {
		t.Errorf("err = %v, want service.ErrNoProfile", err)
	}
	if !strings.Contains(err.Error(), "plumb service install") {
		t.Errorf("the refusal does not name the remedy: %v", err)
	}
	if spec.Executable != "" {
		t.Errorf("a refused launch still produced an executable (%q): the fallback is the defect", spec.Executable)
	}
	if spec.Env != nil {
		t.Errorf("a refused launch still produced an environment (%v)", spec.Env)
	}
}

// Legacy mode is untouched: the profile is IGNORED — not read, not missed — and
// the launch is this binary with this environment, exactly as every shipped
// build behaves today. The profile used here names an executable that does not
// exist, so a launch that consulted it would fail loudly instead.
func TestResolveLaunchSpec_LegacyIgnoresTheProfile(t *testing.T) {
	withTempRuntime(t)
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "")
	t.Setenv("PLUMB_LAUNCH_SPEC_MARKER", "legacy")
	p := service.Default(account.User{UID: os.Getuid(), Home: t.TempDir()}, "/nonexistent/plumb", "/nonexistent/bin")
	writeProfile(t, p)

	spec, err := resolveLaunchSpec()
	if err != nil {
		t.Fatalf("legacy resolveLaunchSpec: %v", err)
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if spec.Executable != want {
		t.Errorf("legacy launch executable = %q, want this binary %q", spec.Executable, want)
	}
	if !slices.Contains(spec.Env, "PLUMB_LAUNCH_SPEC_MARKER=legacy") {
		t.Error("legacy launch does not inherit this process's environment")
	}
}

// Canonical mode: the PROFILE's executable, and an environment built from the
// profile and the account — never from the client that is launching.
func TestResolveLaunchSpec_CanonicalUsesTheProfileAndIgnoresTheClient(t *testing.T) {
	p := withCanonicalService(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "client-config"))
	t.Setenv("PATH", "/client/bin")
	t.Setenv("HOME", t.TempDir())

	spec, err := resolveLaunchSpec()
	if err != nil {
		t.Fatalf("resolveLaunchSpec: %v", err)
	}
	if spec.Executable != p.Executable {
		t.Errorf("executable = %q, want the profile's %q", spec.Executable, p.Executable)
	}
	env := envSlice(spec.Env)
	u, err := account.Current()
	if err != nil {
		t.Fatalf("account.Current: %v", err)
	}
	if env["HOME"] != u.Home {
		t.Errorf("HOME = %q, want the account's %q", env["HOME"], u.Home)
	}
	if env["PATH"] != p.Path {
		t.Errorf("PATH = %q, want the profile's %q", env["PATH"], p.Path)
	}
	if _, ok := env["XDG_CONFIG_HOME"]; ok {
		t.Error("the client's XDG_CONFIG_HOME reached the daemon's environment")
	}
	// USER comes from the account database, not from the client. It is asserted
	// only where the database names this account, which is every host that can
	// run plumb at all.
	if u.Name != "" && env["USER"] != u.Name {
		t.Errorf("USER = %q, want the account database's %q", env["USER"], u.Name)
	}
}

// A profile whose executable has gone (an uninstalled build, a deleted Cellar
// directory) refuses BEFORE the child is started, naming the remedy.
func TestResolveLaunchSpec_CanonicalRefusesAMissingExecutable(t *testing.T) {
	p := withCanonicalService(t)
	p.Executable = filepath.Join(t.TempDir(), "gone")
	writeProfile(t, p)

	_, err := resolveLaunchSpec()
	if err == nil {
		t.Fatal("resolveLaunchSpec accepted a profile whose executable is not there")
	}
	if !strings.Contains(err.Error(), "plumb service install") {
		t.Errorf("the refusal does not name the remedy: %v", err)
	}
}

// daemonCommand is what actually gets started, so it is where "the profile's
// executable, the clean environment, both locks on the handover fds" has to be
// true. Nothing is executed here: the command is only built and inspected.
func TestDaemonCommand_UsesTheSpecAndHandsOverBothLocks(t *testing.T) {
	auth := lockFile(t, "authority")
	lifetime := lockFile(t, "lifetime")
	spec := daemonLaunchSpec{
		Executable: "/opt/installed/plumb",
		Env:        []string{"HOME=/home/tester", "PATH=/profile/bin"},
	}
	cmd, logFile, err := daemonCommand(spec, launchLocks{Authority: auth, Lifetime: lifetime})
	if err != nil {
		t.Fatalf("daemonCommand: %v", err)
	}
	defer func() { _ = logFile.Close() }()

	if cmd.Path != spec.Executable {
		t.Errorf("cmd.Path = %q, want the profile's executable %q", cmd.Path, spec.Executable)
	}
	if !slices.Equal(cmd.Args, []string{"/opt/installed/plumb", "daemon"}) {
		t.Errorf("argv = %v, want the profile's executable running `daemon`", cmd.Args)
	}
	if len(cmd.ExtraFiles) != 2 || cmd.ExtraFiles[0] != auth || cmd.ExtraFiles[1] != lifetime {
		t.Errorf("ExtraFiles = %v, want the authority then the lifetime lock", cmd.ExtraFiles)
	}
	for _, want := range []string{
		"HOME=/home/tester", "PATH=/profile/bin",
		"PLUMB_DAEMON_AUTHORITY_FD=3", "PLUMB_DAEMON_LOCK_FD=4",
	} {
		if !slices.Contains(cmd.Env, want) {
			t.Errorf("the child's environment is missing %q: %v", want, cmd.Env)
		}
	}
	// The clean environment is the whole point: the child gets the spec's
	// variables plus the two handovers, and NOT this process's environment.
	if len(cmd.Env) != len(spec.Env)+2 {
		t.Errorf("the child inherited %d variables, want %d (the spec's plus the handovers): %v",
			len(cmd.Env), len(spec.Env)+2, cmd.Env)
	}
	if cmd.Dir != "/" {
		t.Errorf("cmd.Dir = %q, want / so a client's cwd cannot be inherited", cmd.Dir)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Error("the daemon must be detached with Setsid so it outlives the launching serve")
	}
}

func TestDaemonCommand_RefusesHalfTheLockPair(t *testing.T) {
	if _, _, err := daemonCommand(daemonLaunchSpec{Executable: "/bin/true"}, launchLocks{Authority: lockFile(t, "authority")}); err == nil {
		t.Fatal("a launcher holding one lock started a daemon")
	}
}

// `plumb serve --attach-only` must dial and NEVER spawn. The profile's
// executable is a script that touches a marker, so a launch would leave
// evidence — this is the positive-control shape of the test, not an absence.
func TestAttachOnly_NeverStartsADaemon(t *testing.T) {
	p := withCanonicalService(t)
	marker := spawnMarker(t, p)

	socket := filepath.Join(t.TempDir(), "plumb.sock")
	conn, err := attachOnly(context.Background(), socket)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("attachOnly connected to a socket that does not exist")
	}
	if err == nil {
		t.Fatal("attachOnly must fail when no daemon is running")
	}
	if !errors.Is(err, errNoDaemonToAttach) {
		t.Errorf("err = %v, want errNoDaemonToAttach", err)
	}
	if !strings.Contains(err.Error(), "attach-only") {
		t.Errorf("the failure does not explain that attach-only never starts one: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("attachOnly STARTED a daemon: the profile's executable ran")
	}
}

// The resilient proxy redials through dialOrAttach, so --attach-only has to hold
// on that path too: a reconnect that quietly started a daemon would defeat the
// flag exactly when a script depends on it.
func TestDialOrAttach_AttachOnlyNeverSpawnsOnARedial(t *testing.T) {
	p := withCanonicalService(t)
	marker := spawnMarker(t, p)

	conn, err := dialOrAttach(context.Background(), filepath.Join(t.TempDir(), "plumb.sock"), true)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("dialOrAttach connected to a socket that does not exist")
	}
	if !errors.Is(err, errNoDaemonToAttach) {
		t.Errorf("err = %v, want errNoDaemonToAttach", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("--attach-only spawned a daemon on a redial")
	}
}

// The divergence report is what makes "we ignored your environment" honest. A
// client whose XDG config directory holds a real plumb config gets a WARNING
// naming the remedy that adopts it; every other divergence is information.
func TestClientEnvWarnings_NameTheAdoptionRemedyForARealConfig(t *testing.T) {
	withCanonicalService(t)
	clientConfig := filepath.Join(t.TempDir(), "client-config")
	if err := os.MkdirAll(clientConfig, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(clientConfig, "plumb"), 0o700); err != nil {
		t.Fatalf("MkdirAll plumb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clientConfig, "plumb", "config.toml"), []byte("[ui]\n"), 0o600); err != nil {
		t.Fatalf("writing the client's config: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", clientConfig)

	warnings := clientEnvWarnings()
	var configLine *envWarning
	for i := range warnings {
		if warnings[i].Role == string(service.RoleConfig) {
			configLine = &warnings[i]
		}
	}
	if configLine == nil {
		t.Fatalf("no divergence reported for an XDG_CONFIG_HOME that holds a plumb config: %v", warnings)
	}
	if !configLine.Warn {
		t.Error("a client directory that holds a real config must be warned about, not merely logged")
	}
	if !strings.Contains(configLine.Msg, "plumb service set-xdg --config-home") {
		t.Errorf("the warning does not name the remedy: %s", configLine.Msg)
	}
	if !strings.Contains(configLine.Msg, clientConfig) {
		t.Errorf("the warning does not name the directory to adopt: %s", configLine.Msg)
	}
}

// An ordinary client — one whose XDG bases are the account's own — is told
// nothing about the config directory it uses, because nothing diverged. (The
// runtime directory is exempt: under the test seam it is the seam's own, which
// is a property of the seam rather than of a real client's environment.)
func TestClientEnvWarnings_AreSilentWhenTheClientAgrees(t *testing.T) {
	withCanonicalService(t)
	u, err := account.Current()
	if err != nil {
		t.Fatalf("account.Current: %v", err)
	}
	for _, v := range [][2]string{
		{"XDG_CONFIG_HOME", filepath.Dir(u.ConfigDir())},
		{"XDG_DATA_HOME", filepath.Dir(u.DataDir())},
		{"XDG_STATE_HOME", filepath.Dir(u.StateDir())},
		{"XDG_CACHE_HOME", filepath.Dir(u.CacheDir())},
	} {
		t.Setenv(v[0], v[1])
	}
	for _, w := range clientEnvWarnings() {
		if w.Role == string(service.RoleConfig) {
			t.Errorf("a client whose XDG_CONFIG_HOME agrees with the service was told its config diverged: %s", w.Msg)
		}
	}
}

func lockFile(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), name)
	if err != nil {
		t.Fatalf("creating the %s lock stand-in: %v", name, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// spawnMarker returns the path a profile's stand-in executable touches when it
// runs, so a test can tell "nothing was started" from "nothing was observed".
func spawnMarker(t *testing.T, p service.Profile) string {
	t.Helper()
	// The script is written by withCanonicalService beside its own marker.
	dir := filepath.Dir(p.Executable)
	return filepath.Join(dir, "spawned")
}

func envSlice(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	return out
}

// The REAL exec path: not the command plumb would build, but the process it
// actually starts. The profile's executable is a shell script that records what
// it was handed, so the evidence is the child's own view of its argv, its
// environment and its inherited fds — and nothing daemon-shaped is started on
// this host.
func TestStartDaemonProcess_ExecsTheProfileExecutableWithTheCleanEnvironment(t *testing.T) {
	p := withCanonicalService(t)
	dir := filepath.Dir(p.Executable)
	report := filepath.Join(dir, "child.txt")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  echo \"argv=$0 $*\"\n" +
		"  echo \"HOME=${HOME-unset}\"\n" +
		"  echo \"USER=${USER-unset}\"\n" +
		"  echo \"PATH=${PATH-unset}\"\n" +
		"  echo \"XDG_CONFIG_HOME=${XDG_CONFIG_HOME-unset}\"\n" +
		"  echo \"fd3=$([ -e /dev/fd/3 ] && echo open || echo closed)\"\n" +
		"  echo \"fd4=$([ -e /dev/fd/4 ] && echo open || echo closed)\"\n" +
		"} > \"" + report + "\"\n"
	if err := os.WriteFile(p.Executable, []byte(script), 0o755); err != nil { //nolint:gosec // G306: a test's own script must be executable
		t.Fatalf("writing the stand-in executable: %v", err)
	}
	// A client environment that must not reach the child.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "client-config"))

	spec, err := resolveLaunchSpec()
	if err != nil {
		t.Fatalf("resolveLaunchSpec: %v", err)
	}
	if spec.Executable != p.Executable {
		// Never exec whatever else the spec named: in a test binary that is this
		// test binary, and running it with "daemon" would run the suite.
		t.Skipf("the launch spec names %q rather than the profile's executable; not executing it from a test", spec.Executable)
	}

	locks := launchLocks{Authority: lockFile(t, "authority"), Lifetime: lockFile(t, "lifetime")}
	if err := startDaemonProcess(locks); err != nil {
		t.Fatalf("startDaemonProcess: %v", err)
	}

	var lines []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(report); err == nil && len(data) > 0 {
			lines = strings.Split(strings.TrimSpace(string(data)), "\n")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(lines) == 0 {
		t.Fatal("the profile's executable was never run: no report file appeared")
	}
	got := map[string]string{}
	for _, ln := range lines {
		if k, v, ok := strings.Cut(ln, "="); ok {
			got[k] = v
		}
	}
	u, err := account.Current()
	if err != nil {
		t.Fatalf("account.Current: %v", err)
	}
	if want := p.Executable + " daemon"; got["argv"] != want {
		t.Errorf("argv = %q, want %q", got["argv"], want)
	}
	if got["HOME"] != u.Home {
		t.Errorf("child HOME = %q, want the account's %q", got["HOME"], u.Home)
	}
	if got["PATH"] != p.Path {
		t.Errorf("child PATH = %q, want the profile's %q", got["PATH"], p.Path)
	}
	if got["XDG_CONFIG_HOME"] != "unset" {
		t.Errorf("child XDG_CONFIG_HOME = %q, want it unset: the profile records no such override", got["XDG_CONFIG_HOME"])
	}
	if got["fd3"] != "open" || got["fd4"] != "open" {
		t.Errorf("handover fds: fd3=%s fd4=%s, want both open (authority on 3, lifetime on 4)", got["fd3"], got["fd4"])
	}
}

// F1: `set-xdg --runtime-dir` must refuse a base the install path refuses. The
// daemon binds its sockets there, so a base this uid does not privately own would
// expose them — and a set-* must not be the way around the install-time rule.
func TestServiceSetXDG_RefusesAnUnusableRuntimeBase(t *testing.T) {
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

	exposed := t.TempDir()
	if err := os.Chmod(exposed, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	u, err := account.Current()
	if err != nil {
		t.Fatalf("account.Current: %v", err)
	}
	if account.UsableRuntimeBase(exposed, u.UID) {
		t.Fatalf("%s is usable as a runtime base; the test cannot tell the two cases apart", exposed)
	}
	err = setXDGProfile(map[string]string{"XDG_RUNTIME_DIR": exposed})
	if err == nil {
		t.Fatal("set-xdg accepted a world-readable runtime base")
	}
	if !strings.Contains(err.Error(), "0700") {
		t.Errorf("the refusal does not say what a runtime base must be: %v", err)
	}
	after, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if after != before {
		t.Errorf("a refused set-xdg changed the profile:\n got %+v\nwant %+v", after, before)
	}

	private := t.TempDir()
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if err := setXDGProfile(map[string]string{"XDG_RUNTIME_DIR": private}); err != nil {
		t.Fatalf("set-xdg refused a private runtime base: %v", err)
	}
	recorded, err := service.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if recorded.XDG.RuntimeDir != private {
		t.Errorf("XDG_RUNTIME_DIR = %q, want %q", recorded.XDG.RuntimeDir, private)
	}
	if got, want := paths.RuntimeDir(), service.DirUnder(private); got != want {
		t.Errorf("RuntimeDir() = %q, want the adopted %q", got, want)
	}
}

// F2: a daemon that is STARTING is not an absent daemon. The launcher records
// what it is starting through the held authority fd, and `--attach-only` waits on
// that record instead of failing at once — without taking the lock, which would
// make every concurrent launcher wait out its whole deadline.
func TestAttachOnly_WaitsWhileADaemonIsStarting(t *testing.T) {
	withCanonicalService(t)
	t.Setenv("PLUMB_DAEMON_START_TIMEOUT", "5s")

	socket := shortSocketPath(t)
	authority, err := authorityPath()
	if err != nil {
		t.Fatalf("authorityPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(authority), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := os.OpenFile(authority, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening the authority lock: %v", err)
	}
	// The record a launcher publishes while its child is coming up: THIS live
	// pid, and the socket the daemon will bind.
	if err := publishAuthority(f, authorityMetadata{PID: os.Getpid(), Version: "test", Socket: socket}); err != nil {
		t.Fatalf("publishAuthority: %v", err)
	}
	_ = f.Close()

	// The socket appears only after the client has been waiting: exactly the
	// window the metadata now covers.
	ready := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, lerr := net.Listen("unix", socket)
		if lerr == nil {
			close(ready)
			<-time.After(5 * time.Second)
			_ = ln.Close()
		}
	}()

	conn, err := attachOnly(context.Background(), socket)
	if err != nil {
		t.Fatalf("attachOnly gave up while a daemon was starting: %v", err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case <-ready:
	default:
		t.Fatal("attachOnly returned before the socket existed: it did not wait for the starting daemon")
	}
}

// The other half of F2: a record that names a process which is NOT alive (a
// daemon that exited) must not make --attach-only wait. Otherwise every scripted
// client would hang for the whole budget after a clean stop.
func TestAttachOnly_DoesNotWaitForADeadHoldersRecord(t *testing.T) {
	withCanonicalService(t)
	t.Setenv("PLUMB_DAEMON_START_TIMEOUT", "5s")

	authority, err := authorityPath()
	if err != nil {
		t.Fatalf("authorityPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(authority), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := os.OpenFile(authority, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening the authority lock: %v", err)
	}
	// A pid that cannot be alive: nothing is running under it on any supported OS.
	if err := publishAuthority(f, authorityMetadata{PID: 1 << 30, Version: "test", Socket: filepath.Join(t.TempDir(), "plumb.sock")}); err != nil {
		t.Fatalf("publishAuthority: %v", err)
	}
	_ = f.Close()

	start := time.Now()
	_, err = attachOnly(context.Background(), shortSocketPath(t))
	if !errors.Is(err, errNoDaemonToAttach) {
		t.Fatalf("err = %v, want errNoDaemonToAttach", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("attachOnly waited %s on a dead holder's record; it must fail at once", elapsed)
	}
}

// F2, the writer's half: the launcher must actually publish the starting record,
// or the reader above is testing a record nothing produces.
func TestDaemonLauncher_PublishesAStartingRecord(t *testing.T) {
	withCanonicalService(t)
	auth := lockFile(t, "authority")
	if err := syscall.Flock(int(auth.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("taking the authority lock: %v", err)
	}
	lifetime := lockFile(t, "lifetime")
	socket := shortSocketPath(t)
	l := newDaemonLauncher(socket)
	var seen authorityMetadata
	l.start = func(launchLocks) error {
		// Read the record from the lock file the launcher holds — the same bytes
		// another process reads through the path — mid-launch, before any child
		// exists: this is the state a starting daemon leaves behind.
		m, ok := readAuthority(auth.Name())
		if !ok {
			t.Error("no authority record was published while the launch was in progress")
			return errors.New("stop")
		}
		seen = m
		return errors.New("stop") // never start a daemon from a test
	}
	var st launchState
	if err := l.launch(context.Background(), launchLocks{Authority: auth, Lifetime: lifetime}, &st); err == nil {
		t.Fatal("the fake start refused, so launch should have reported an error")
	}
	if seen.PID != os.Getpid() {
		t.Errorf("the starting record names pid %d, want this live process (%d)", seen.PID, os.Getpid())
	}
	if seen.Socket != socket {
		t.Errorf("the starting record names socket %q, want %q", seen.Socket, socket)
	}
	if n, ok := readAuthority(auth.Name()); ok {
		t.Errorf("a failed launch left a record behind: %+v", n)
	}
}

// F3: the over-long-socket hint must name a setting that can actually move the
// runtime directory. In canonical mode no environment variable can — the profile
// records it — so telling the user to point $HOME somewhere shorter is a remedy
// that provably changes nothing.
func TestSocketPathLengthHint_NamesTheLeverThatCanMoveTheSocket(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d/", maxUnixSocketPath), "plumb.sock")

	withTempRuntime(t)
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "")
	legacy := socketPathLengthHint(long)
	if !strings.Contains(legacy, socketPathShortenLever()) {
		t.Errorf("legacy hint does not name the environment lever %q: %s", socketPathShortenLever(), legacy)
	}

	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "1")
	canonical := socketPathLengthHint(long)
	if !strings.Contains(canonical, "plumb service set-xdg --runtime-dir") {
		t.Errorf("canonical hint does not name the profile remedy: %s", canonical)
	}
	if strings.Contains(canonical, "$HOME") || strings.Contains(canonical, socketPathShortenLever()) {
		t.Errorf("canonical hint points at an environment variable that cannot move the socket: %s", canonical)
	}
	if strings.Contains(legacy, "plumb service set-xdg") {
		t.Errorf("legacy hint offers a remedy from the other mode: %s", legacy)
	}
}

// F5: the resilient proxy's redial closure is what makes `--attach-only` hold
// after a reconnect. Driving the very deps the proxy is built with is the only
// way to pin that wiring: a stub that reconnects must not start a daemon the
// flag forbade.
func TestServeProxyDeps_RedialNeverSpawnsUnderAttachOnly(t *testing.T) {
	p := withCanonicalService(t)
	marker := spawnMarker(t, p)
	socket := filepath.Join(t.TempDir(), "plumb.sock")

	deps := serveProxyDeps(socket, true, []string{"/tmp"}, "", nil)
	if deps.dial == nil {
		t.Fatal("the proxy has no redial closure")
	}
	conn, err := deps.dial(context.Background())
	if conn != nil {
		_ = conn.Close()
		t.Fatal("the redial connected to a socket that does not exist")
	}
	if !errors.Is(err, errNoDaemonToAttach) {
		t.Errorf("redial err = %v, want errNoDaemonToAttach: the closure must never spawn under --attach-only", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the proxy's redial STARTED a daemon under --attach-only")
	}
	if deps.proxySessionID == "" || deps.heartbeatInterval <= 0 {
		t.Error("the deps are not the complete set the proxy runs with")
	}
}

// shortSocketPath returns a socket path short enough for sun_path. t.TempDir()
// on darwin is far too long for one, and a test whose listener silently fails to
// bind proves nothing about waiting.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	// /tmp directly, not t.TempDir(): on darwin t.TempDir() lives under
	// /var/folders/… and a socket path there exceeds sun_path (103 bytes), so the
	// listener silently fails to bind and the test proves nothing.
	path := filepath.Join("/tmp", fmt.Sprintf("p528-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(path) })
	if len(path) > maxUnixSocketPath {
		t.Fatalf("the test's socket path is %d bytes, over sun_path's %d", len(path), maxUnixSocketPath)
	}
	return path
}
