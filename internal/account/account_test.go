package account

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
