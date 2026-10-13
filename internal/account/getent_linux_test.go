//go:build linux

package account

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These two tests are the only place in the tree, or in CI, where getent(1) is
// actually EXECUTED. Everything else about the NSS fallback is checked through
// the injected runner in account_test.go, which pins the rules and not the exec.
//
// Why a build tag and not t.Skip: the tag that made this path unreachable in CI
// is home_linux_pure.go's `linux && !cgo`, the shape goreleaser builds
// (.goreleaser.yml sets CGO_ENABLED=0) and the shape no gate builds — a Linux
// toolchain with a C compiler defaults to CGO_ENABLED=1. A runtime skip would
// therefore report green on exactly the builds this guard exists for. `linux` is
// never skipped anywhere: it compiles on every Linux leg, and runGetent carries
// the same tag in getent_linux.go so these tests can call it there.
//
// What they prove: the exec path runs the real binary, and its outcome is
// classified the way getent.go documents — status 0 with an entry naming this
// uid, status 2 ("no such key") as the name service's refusal. What they do NOT
// prove: that a production binary takes this path, which is a build-tag fact and
// so cannot be asserted from inside the test; and nothing about an account served
// only by LDAP, SSSD or systemd-homed on a machine this suite does not have.

// TestRunGetent_ResolvesThisAccount runs the real getent for THIS uid and
// requires a non-empty absolute home out of it: the assertion the release build's
// platformHome depends on and that nothing else executes.
func TestRunGetent_ResolvesThisAccount(t *testing.T) {
	ctx := t.Context()
	uid := os.Getuid()
	path := getentOnDisk(t)

	// The exec itself, called directly. This is what fails if the binary is named
	// wrongly, the argv is wrong, or the command cannot be run at all — and its
	// status is the only way to tell "no such key" from "could not run", which
	// runGetent's (string, bool) deliberately folds together.
	out, exit, err := execGetentPath(ctx, path, uid)
	if err != nil {
		t.Fatalf("exec of %s failed for uid %d: %v — the NSS fallback cannot run on this host", path, uid, err)
	}
	if exit != 0 && exit != getentKeyNotFound {
		t.Fatalf("%s exited %d for uid %d (output %q): want 0 for an entry or %d for no such key",
			path, exit, uid, out, getentKeyNotFound)
	}

	// The production entry point, called exactly as platformHome calls it.
	got, ok := runGetent(ctx, uid)

	if exit == getentKeyNotFound {
		// The name service answered "no such uid" — a clean refusal, not a broken
		// exec. It is only clean for a uid no source holds, so the file source is
		// checked first: `getent passwd` consults /etc/passwd before any network
		// service, so a uid the file holds MUST resolve here.
		fileHome, inFile := passwdFileHome(t, uid)
		if inFile {
			t.Fatalf("getent %s reported no such uid %d though /etc/passwd holds it with home %q: "+
				"the files source did not answer", path, uid, fileHome)
		}
		if ok {
			t.Fatalf("runGetent(%d) answered %q after %s said the key is not in the database; "+
				"queryGetent must refuse on status %d", uid, got, path, getentKeyNotFound)
		}
		t.Logf("uid %d is in no source this host serves — /etc/passwd does not hold it and %s answered "+
			"status %d — so no home could be asserted; what this run does prove is that the real exec ran "+
			"and that its status was classified as a refusal", uid, path, exit)
		return
	}

	if !ok {
		t.Fatalf("runGetent(%d) refused although %s exited 0 for it: queryGetent must return the answer "+
			"a status of 0 carries", uid, path)
	}
	home, parsed := parsePasswdHome(got, uid)
	if !parsed || home == "" {
		t.Fatalf("parsePasswdHome(%q, %d) = %q, %v: getent's own output for this uid must yield its home",
			got, uid, home, parsed)
	}
	if !filepath.IsAbs(home) {
		t.Fatalf("home = %q for uid %d: want a non-empty absolute path", home, uid)
	}
	// An account the file source can see must come back with the same home from
	// the name service; a mismatch means the argv asked about something else, or
	// the answer was read out of the wrong field.
	if fileHome, inFile := passwdFileHome(t, uid); inFile && fileHome != home {
		t.Errorf("getent %s answered home %q for uid %d, but /etc/passwd holds %q", path, home, uid, fileHome)
	}
}

// TestRunGetent_RefusesAKeyTheNameServiceDoesNotHold pins the status-2 rule
// against the real binary: a getent that RAN and reported the key absent has
// answered, so the lookup ends as a refusal there rather than asking the second
// copy of the binary (getent.go, queryGetent).
//
// The uid is one no account database holds, so this asserts the classification
// rather than anything about this host's accounts.
func TestRunGetent_RefusesAKeyTheNameServiceDoesNotHold(t *testing.T) {
	ctx := t.Context()
	path := getentOnDisk(t)

	// A valid uid_t, so getent parses the argument instead of rejecting it, that
	// no account database on a test machine assigns.
	const unassigned = 1<<31 - 1
	if uid := os.Getuid(); uid == unassigned {
		t.Fatalf("this test is running as uid %d, the one it assumes is unassigned", unassigned)
	}

	out, exit, err := execGetentPath(ctx, path, unassigned)
	if err != nil {
		t.Fatalf("exec of %s failed: %v", path, err)
	}
	if exit != getentKeyNotFound {
		t.Fatalf("%s exited %d for uid %d (output %q): want %d for no such key",
			path, exit, unassigned, out, getentKeyNotFound)
	}
	if got, ok := runGetent(ctx, unassigned); ok {
		t.Errorf("runGetent(%d) = %q, true; want a refusal: status %d ends the lookup",
			unassigned, got, getentKeyNotFound)
	}
}

// TestPlatformHome_ResolvesThisAccount is the guard on the body that SHIPS.
//
// runGetent's tests prove the exec works; this proves the release build's own
// entry point reaches the account database and returns what account.go's
// contract needs. Which body answers depends on how the test binary was built —
// home_linux_pure.go under CGO_ENABLED=0, home_linux_cgo.go under cgo — and that
// is exactly the point: `make test-pure` runs this same test in the release's
// mode, so the body that Linux releases actually execute is EXECUTED by a gate
// rather than only compiled by one. Before that target existed, neither happened.
//
// It calls platformHome directly rather than account.Current() because Current
// requires a seam root in a test binary by design (seam_off.go); the account
// lookup is what is under test here, not the seam.
func TestPlatformHome_ResolvesThisAccount(t *testing.T) {
	uid := os.Getuid()
	home, ok := platformHome(uid)
	if !ok {
		// A refusal is a real failure for the account this test is already
		// running as: the file source holds it, or getent asks the same name
		// service that authenticated this very process.
		t.Fatalf("platformHome(%d) refused: this account cannot resolve its own home, so no "+
			"daemon could start for it", uid)
	}
	if home == "" || !filepath.IsAbs(home) {
		t.Fatalf("platformHome(%d) = %q: want a non-empty absolute path", uid, home)
	}
	// Whichever source answered must agree with /etc/passwd when the file holds
	// this uid: a disagreement means the body read the wrong field, or asked
	// about an account other than the one it was given.
	if fileHome, inFile := passwdFileHome(t, uid); inFile && fileHome != home {
		t.Errorf("platformHome(%d) = %q, but /etc/passwd holds %q for it", uid, home, fileHome)
	}
}

// TestPlatformHome_RefusesAKeyNoSourceHolds pins the fail-closed half for the
// shipped body: an account no database holds has no home, and the lookup must
// refuse rather than fall back to the environment. The uid is the one
// TestRunGetent_RefusesAKeyTheNameServiceDoesNotHold already establishes that no
// source on this host serves.
func TestPlatformHome_RefusesAKeyNoSourceHolds(t *testing.T) {
	const unassigned = 1<<31 - 1
	if uid := os.Getuid(); uid == unassigned {
		t.Fatalf("this test is running as uid %d, the one it assumes is unassigned", unassigned)
	}
	if home, ok := platformHome(unassigned); ok {
		t.Errorf("platformHome(%d) = %q, true; want a refusal: no account database holds this uid",
			unassigned, home)
	}
}

// getentOnDisk returns the first of getentPaths that exists, and fails when none
// does: a host without getent(1) is one where the CGO_ENABLED=0 build cannot
// resolve an NSS account at all — the refusal in account.go names libc-bin for
// exactly that reason — so passing here would hide the failure this guard is for.
func getentOnDisk(t *testing.T) string {
	t.Helper()
	for _, path := range getentPaths {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	t.Fatalf("none of %v exists on this host: the NSS fallback cannot run, so this test would prove nothing", getentPaths)
	return ""
}

// passwdFileHome reports the home /etc/passwd holds for uid, and whether the file
// holds uid at all. It parses the file here rather than through parsePasswdHome so
// that the test's cross-check does not rest on the parser under test.
func passwdFileHome(t *testing.T, uid int) (string, bool) {
	t.Helper()
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Fatalf("reading /etc/passwd: %v", err)
	}
	want := strconv.Itoa(uid)
	for line := range strings.SplitSeq(string(data), "\n") {
		// login:passwd:uid:gid:gecos:home:shell — uid is field 2, home field 5.
		f := strings.Split(line, ":")
		if len(f) >= 6 && f[2] == want && f[5] != "" {
			return f[5], true
		}
	}
	return "", false
}
