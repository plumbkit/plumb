package account

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCurrent_RefusesWithoutASeamRootInATestBinary(t *testing.T) {
	// The rule that keeps a test suite off the owner's account: in a test binary
	// with no seam root there is no account resolution at all, because resolving
	// the real one would put the authority lock under the real data directory,
	// where taking it would make the running daemon fail to restart.
	t.Setenv("PLUMB_TEST_ACCOUNT_ROOT", "")
	_, err := Current()
	if err == nil {
		t.Fatal("Current() succeeded in a test binary with no seam root; it must refuse")
	}
	if !errors.Is(err, ErrNoAccount) {
		t.Errorf("error = %v, want it to wrap ErrNoAccount", err)
	}
	if !strings.Contains(err.Error(), "PLUMB_TEST_ACCOUNT_ROOT") {
		t.Errorf("the refusal must name the seam variable, got: %v", err)
	}
}

func TestCurrent_UsesTheSeamRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLUMB_TEST_ACCOUNT_ROOT", dir)
	u, err := Current()
	if err != nil {
		t.Fatalf("Current() with a seam root: %v", err)
	}
	if u.Home != dir {
		t.Errorf("home = %q, want the seam root %q", u.Home, dir)
	}
	if u.UID == 0 && runtime.GOOS != "windows" {
		t.Log("uid 0: running as root, which is normal in a container")
	}
	// Every path the account owns must land inside the seam root, so a test can
	// never touch anything else.
	for name, path := range map[string]string{
		"data": u.DataDir(), "cache": u.CacheDir(), "runtime": u.RuntimeDir(),
		"authority": u.AuthorityLockPath(), "profile": u.ServiceProfilePath(),
	} {
		if !strings.HasPrefix(path, dir+string(filepath.Separator)) {
			t.Errorf("%s path %q escapes the seam root %q", name, path, dir)
		}
	}
}

// The rule the Linux CI leg broke. /run/user/<uid> is a real, usable directory
// there, so the runtime directory resolved to /run/user/<uid>/plumb — outside
// the seam root, where a test binary's daemon could bind its socket in the
// account's own runtime directory. The assertion is the exact seam path rather
// than containment: on darwin the cache fallback is inside the root too, so a
// containment-only check passes with the seam branch deleted, which is how a
// darwin-only run let this through.
//
// /run/user/<uid> cannot be created from a test (and does not exist on darwin at
// all), so the kernel's base is injected instead: runtimeDirUnder is the function
// linuxRuntimeDir calls, and the directory below passes the same checks.
func TestCurrent_SeamRootContainsTheRuntimeDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits and file ownership have no meaning on Windows")
	}
	root := t.TempDir()
	t.Setenv("PLUMB_TEST_ACCOUNT_ROOT", root)
	u, err := Current()
	if err != nil {
		t.Fatalf("Current() with a seam root: %v", err)
	}

	// The shape the Linux runner has: a runtime base it would accept.
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatalf("chmod %s: %v", base, err)
	}
	kernel, ok := runtimeDirUnder(base, u.UID)
	if !ok {
		t.Fatalf("runtimeDirUnder(%q, %d) rejected a 0700 directory owned by this uid; "+
			"the injected base must reproduce the Linux shape", base, u.UID)
	}

	got, want := u.RuntimeDir(), filepath.Join(root, "run", "plumb")
	if got != want {
		t.Errorf("RuntimeDir() = %q, want %q: the seam root must contain the runtime directory "+
			"(the directory Linux would otherwise pick is %q)", got, want, kernel)
	}
}

// The checks linuxRuntimeDir applies to the kernel's runtime base — the XDG
// spec's own, because the daemon socket is exposed to whoever owns that
// directory. They are exercised on every platform because the base is injected:
// on Linux this is the one path a darwin-only run never executes, so it was
// untested on the machine that let the runtime directory escape the seam root.
func TestRuntimeDirUnder_AppliesTheSpecChecksToTheKernelRuntimeBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits and file ownership have no meaning on Windows")
	}
	uid := os.Getuid()

	good := t.TempDir()
	if err := os.Chmod(good, 0o700); err != nil {
		t.Fatalf("chmod %s: %v", good, err)
	}
	// Explicit chmod: a directory's mode is masked by the process umask, so
	// under `umask 077` this one would be created 0700 and correctly ACCEPTED,
	// silently turning the world-readable case into a no-op.
	loose := t.TempDir()
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", loose, err)
	}
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatalf("writing %s: %v", notADir, err)
	}

	tests := []struct {
		name string
		base string
		ok   bool
	}{
		{"a private base this uid owns", good, true},
		{"a base that does not exist", filepath.Join(t.TempDir(), "missing"), false},
		{"a world-readable base", loose, false},
		{"a base that is not a directory", notADir, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := runtimeDirUnder(tt.base, uid)
			if ok != tt.ok {
				t.Fatalf("runtimeDirUnder(%q, %d) ok = %v, want %v", tt.base, uid, ok, tt.ok)
			}
			if ok && got != filepath.Join(tt.base, "plumb") {
				t.Errorf("runtimeDirUnder(%q, %d) = %q, want plumb's directory under it", tt.base, uid, got)
			}
		})
	}
}

func TestCurrent_RefusesARelativeSeamRoot(t *testing.T) {
	t.Setenv("PLUMB_TEST_ACCOUNT_ROOT", "relative/dir")
	if _, err := Current(); err == nil {
		t.Error("a relative seam root must be refused, not resolved against the working directory")
	}
}

func TestParseTestRoot(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/run/user/501/plumb-test", "/run/user/501/plumb-test"},
		{"  /tmp/x  ", "/tmp/x"},
		{"", ""},
		{"relative", ""},
		{"   ", ""},
	}
	for _, tt := range tests {
		if got := ParseTestRoot(tt.in); got != tt.want {
			t.Errorf("ParseTestRoot(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParsePasswdHome(t *testing.T) {
	data := strings.Join([]string{
		"# a comment line with:colons:in:it",
		"root:x:0:0:root:/root:/bin/sh",
		"alice:x:501:20:Alice:/Users/alice:/bin/zsh",
		"nohome:x:502:20::/home/nohome:/bin/sh",
		"emptyhome:x:503:20:Empty::/bin/sh",
		"short:x:504",
		"textuid:x:notanumber:20:x:/home/x:/bin/sh",
		"",
	}, "\n")

	tests := []struct {
		name string
		uid  int
		want string
		ok   bool
	}{
		{"an ordinary account", 501, "/Users/alice", true},
		{"root", 0, "/root", true},
		{"an account with no gecos", 502, "/home/nohome", true},
		{"an empty home field", 503, "", false},
		{"a uid that is not there", 999, "", false},
		{"a line with too few fields is skipped", 504, "", false},
		{"a non-numeric uid field never matches", 505, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parsePasswdHome(data, tt.uid)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parsePasswdHome(uid %d) = %q, %v; want %q, %v", tt.uid, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestUserPaths_SeparateTheAuthorityFromTheRuntimeDirectory(t *testing.T) {
	u := User{UID: 501, Home: "/home/alice"}

	// The authority must not live in the runtime directory: the runtime
	// directory is removable (a tmpfs, or a directory somebody deletes), and a
	// lock file that vanished with it would let a second daemon take "the"
	// authority from scratch.
	if strings.HasPrefix(u.AuthorityLockPath(), u.RuntimeDir()+string(filepath.Separator)) {
		t.Errorf("the authority lock %q must live outside the runtime directory %q",
			u.AuthorityLockPath(), u.RuntimeDir())
	}
	if !strings.HasPrefix(u.AuthorityLockPath(), u.DataDir()+string(filepath.Separator)) {
		t.Errorf("the authority lock %q must live under the data directory %q",
			u.AuthorityLockPath(), u.DataDir())
	}
	if u.ServiceProfilePath() == u.AuthorityLockPath() {
		t.Error("the profile and the authority lock are different files")
	}

	wantData := filepath.Join("/home/alice", ".local", "share", "plumb")
	wantCache := filepath.Join("/home/alice", ".cache", "plumb")
	if runtime.GOOS == "darwin" {
		wantData = filepath.Join("/home/alice", "Library", "Application Support", "plumb")
		wantCache = filepath.Join("/home/alice", "Library", "Caches", "plumb")
	}
	if u.DataDir() != wantData {
		t.Errorf("DataDir() = %q, want %q", u.DataDir(), wantData)
	}
	if got := u.CacheDir(); got != wantCache {
		t.Errorf("CacheDir() = %q, want %q", got, wantCache)
	}
}

// The rule a CGO_ENABLED=0 Linux release binary depends on: an account served by
// NSS (LDAP, SSSD, Active Directory, systemd-homed, NIS) has no /etc/passwd
// entry, and authorityPath() is consulted unconditionally on the way to starting
// a daemon — so before this fallback existed, such a host could not run one at
// all. The runner is injected because the production caller is Linux-only.
func TestLookupHome_FallsBackToGetentForAnAccountAbsentFromPasswd(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/sh\nalice:x:501:20:Alice:/home/alice:/bin/zsh\n"
	var asked int
	getent := func(_ context.Context, uid int) (string, bool) {
		asked = uid
		return "bob:x:502:20:Bob:/home/bob:/bin/bash\n", true
	}

	home, ok := lookupHome(context.Background(), passwd, 502, getent)
	if !ok {
		t.Fatal("an account absent from /etc/passwd but present in getent must resolve")
	}
	if home != "/home/bob" {
		t.Errorf("home = %q, want /home/bob from the getent entry", home)
	}
	if asked != 502 {
		t.Errorf("getent was asked for uid %d, want the uid passed to lookupHome (502)", asked)
	}

	// What the resolved home is FOR: every directory the daemon's locks, sockets
	// and metadata live in is built from it, so this is the difference between a
	// daemon on an NSS host and none at all.
	u := User{UID: 502, Home: home}
	wantData := filepath.Join(home, ".local", "share", "plumb")
	wantCache := filepath.Join(home, ".cache", "plumb")
	if runtime.GOOS == "darwin" {
		wantData = filepath.Join(home, "Library", "Application Support", "plumb")
		wantCache = filepath.Join(home, "Library", "Caches", "plumb")
	}
	if got := u.DataDir(); got != wantData {
		t.Errorf("DataDir() = %q, want %q", got, wantData)
	}
	if got := u.CacheDir(); got != wantCache {
		t.Errorf("CacheDir() = %q, want %q", got, wantCache)
	}
	if got := u.AuthorityLockPath(); got != filepath.Join(wantData, "daemon.authority.lock") {
		t.Errorf("AuthorityLockPath() = %q, want it under %q", got, wantData)
	}
}

// getent is the FALLBACK, not a second opinion: a uid the passwd file holds must
// be answered from the file, without an NSS round trip that could be slow or, on
// a misconfigured host, answer differently.
func TestLookupHome_DoesNotAskGetentWhenPasswdAnswers(t *testing.T) {
	called := false
	getent := func(context.Context, int) (string, bool) {
		called = true
		return "bob:x:502:20:Bob:/home/elsewhere:/bin/bash\n", true
	}

	home, ok := lookupHome(context.Background(), "bob:x:502:20:Bob:/home/bob:/bin/bash\n", 502, getent)
	if !ok || home != "/home/bob" {
		t.Fatalf("lookupHome = %q, %v; want /home/bob from the passwd file", home, ok)
	}
	if called {
		t.Error("getent was consulted for a uid the passwd file already answered")
	}
}

// A uid field that is not the one asked for is a refusal, not a fallback: getent
// was asked about exactly one account, so a line naming a different uid is an
// answer to a different question, and reading a home out of it would be the
// guess this package exists to refuse.
func TestLookupHome_RefusesAGetentAnswerNamingAnotherUID(t *testing.T) {
	getent := func(context.Context, int) (string, bool) {
		return "bob:x:999:20:Bob:/home/bob:/bin/bash\n", true
	}

	home, ok := lookupHome(context.Background(), "root:x:0:0:root:/root:/bin/sh\n", 502, getent)
	if ok {
		t.Fatalf("lookupHome resolved %q from a getent answer naming uid 999", home)
	}
}

// The rule for a getent that RAN and said the key is not there: that is the name
// service answering the question it was asked, so the lookup ends as a refusal.
// Treating it as a failure to run and putting the question to the other copy of
// the binary is a second round trip to the same database — spent on the way to
// starting a daemon, where a slow directory server is exactly the worry — and it
// can only ever come back with the same answer.
func TestQueryGetent_TreatsKeyNotFoundAsARefusalRatherThanATryAgain(t *testing.T) {
	const entry = "bob:x:502:20:Bob:/home/bob:/bin/bash\n"
	// A binary that is not installed: ENOENT, not a status, which is what tells
	// the lookup to try the other path.
	notInstalled := errors.New("fork/exec /usr/bin/getent: no such file or directory")

	// paths is the script each getent path follows, in order; the lookup must
	// never run more of them than that.
	type outcome struct {
		out  string
		exit int
		err  error
	}
	tests := []struct {
		name      string
		paths     []outcome
		want      string
		ok        bool
		wantAsked int
	}{
		{"the first path answers", []outcome{{out: entry}, {out: "other:x:9:9:x:/elsewhere:/bin/sh\n"}}, entry, true, 1},
		{"the key is not in the database", []outcome{{exit: getentKeyNotFound}, {out: entry}}, "", false, 1},
		{"the first path is not installed, the second answers", []outcome{{err: notInstalled}, {out: entry}}, entry, true, 2},
		{"neither path is installed", []outcome{{err: notInstalled}, {err: notInstalled}}, "", false, 2},
		{"a usage error is not the key's absence", []outcome{{exit: 1}, {out: entry}}, entry, true, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []string
			run := func(_ context.Context, path string, _ int) (string, int, error) {
				i := len(asked)
				asked = append(asked, path)
				if i >= len(tt.paths) {
					t.Fatalf("getent path %d (%s) was run, but the lookup must have ended after %d", i+1, path, len(tt.paths))
				}
				return tt.paths[i].out, tt.paths[i].exit, tt.paths[i].err
			}

			home, ok := queryGetent(context.Background(), 502, []string{"getent-first", "getent-second"}, run)
			if ok != tt.ok || home != tt.want {
				t.Errorf("queryGetent = %q, %v; want %q, %v", home, ok, tt.want, tt.ok)
			}
			if len(asked) != tt.wantAsked {
				t.Errorf("getent was run %d times (%v), want %d: "+
					"the key's absence must end the lookup, and a binary that cannot run must not", len(asked), asked, tt.wantAsked)
			}
		})
	}
}

// getent is never found through PATH: it decides the account, and so where the
// daemon's authority lock, sockets and metadata live, and PATH is environment.
// A bare name here is the whole retargeting defect in one word.
func TestGetentPaths_AreAbsoluteAndTriedInOrder(t *testing.T) {
	if len(getentPaths) == 0 {
		t.Fatal("no getent path to try: the NSS fallback could never run")
	}
	for i, path := range getentPaths {
		if !filepath.IsAbs(path) {
			t.Errorf("getentPaths[%d] = %q, want an absolute path: PATH must not decide this lookup", i, path)
		}
		if filepath.Base(path) != "getent" {
			t.Errorf("getentPaths[%d] = %q, want the getent binary", i, path)
		}
	}
}

// Only BOTH sources failing is a refusal, and that refusal names the remedy. The
// uid is one no account database can hold: the assertion is about lookup's own
// wiring, so it must not depend on which source answered — or on which platform
// this test runs on.
func TestLookup_RefusesWhenNeitherPasswdNorGetentAnswers(t *testing.T) {
	const nowhere = 1 << 30
	_, err := lookup(nowhere)
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("lookup(%d) = %v, want it to wrap ErrNoAccount", nowhere, err)
	}
	// The remedy, not only the fact: the message has to say what to change. Both
	// halves of it — add the account to the database, or run plumb as a user it
	// knows — are asserted, because a refusal that only reports the failure
	// leaves the reader with a host that cannot start a daemon and no way out.
	for _, want := range []string{
		"getent",
		"run plumb as an account the OS account database can resolve",
		"needs an entry in one of them",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name the remedy (%q) for the build that ships, got: %v", want, err)
		}
	}
}

// NSS can block on an unreachable directory server rather than fail, and this
// lookup runs on the way to starting a daemon, so the fallback carries a
// deadline: a source that misses it is a source that did not answer.
func TestLookupHome_BoundsTheGetentLookup(t *testing.T) {
	var (
		deadline time.Time
		hasLimit bool
	)
	getent := func(ctx context.Context, _ int) (string, bool) {
		deadline, hasLimit = ctx.Deadline()
		return "", false
	}

	if home, ok := lookupHome(context.Background(), "", 501, getent); ok {
		t.Fatalf("lookupHome resolved %q from a source that answered nothing", home)
	}
	if !hasLimit {
		t.Fatal("the getent lookup ran with no deadline; NSS can block indefinitely")
	}
	if left := time.Until(deadline); left <= 0 || left > getentTimeout {
		t.Errorf("the getent deadline is %s away, want more than 0 and at most %s", left, getentTimeout)
	}
}
