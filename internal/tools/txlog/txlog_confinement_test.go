package txlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// txlog_confinement_test.go — the replay trust boundary.
//
// <workspace>/.plumb/tx-log/ is an ordinary directory inside the workspace, so a
// cloned repository ships one and the daemon replays it on attach with no user
// action. Every test here asserts that a manifest cannot make the replay write
// somewhere the session may not write, or with mode bits it chose.
//
// Against the pre-fix code these payloads created files outside the workspace,
// including a 0o777 shell script; see FuzzScanReplay's recorded corpus.

// pastStart is any instant before the liveCutoff the tests pass, so every
// fixture manifest is treated as a recoverable orphan rather than a live
// transaction.
var pastStart = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// confinedTo is the test stand-in for a session's PathPolicy: it admits a path
// only when it resolves inside root. The real policy additionally admits
// configured extra roots and --allow-dir grants, which is what
// TestScan_RestoresGuardAdmittedPathOutsideWorkspace exercises.
func confinedTo(root string) PathGuard {
	roots := []string{filepath.Clean(root)}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		if c := filepath.Clean(resolved); c != roots[0] {
			roots = append(roots, c)
		}
	}
	return func(path string) error {
		clean := filepath.Clean(path)
		for _, r := range roots {
			rel, err := filepath.Rel(r, clean)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil
			}
		}
		return fmt.Errorf("path %q is outside %q", path, root)
	}
}

// writeOrphan builds an orphaned tx-log directory: a manifest dated in the past
// plus one "<n>-before" snapshot per op, so Scan treats it as recoverable.
func writeOrphan(t *testing.T, ws string, ops []opMeta, snapshots map[int]string) string {
	t.Helper()
	dir := filepath.Join(ws, ".plumb", "tx-log", "orphan")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	data, err := json.Marshal(txManifest{TxID: "orphan", StartedAt: pastStart, Workspace: ws, Ops: ops})
	if err != nil {
		t.Fatalf("marshalling manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	for n, content := range snapshots {
		snap := filepath.Join(dir, strconv.Itoa(n)+"-before")
		if err := os.WriteFile(snap, []byte(content), 0o600); err != nil {
			t.Fatalf("writing snapshot %s: %v", snap, err)
		}
	}
	return dir
}

// TestScan_RefusesPathOutsideTheSessionPolicy is the core regression test: a
// manifest naming a path the guard rejects must not be written, while a
// legitimate op in the SAME manifest is still restored — a refusal must skip one
// entry, not abandon the recovery.
func TestScan_RefusesPathOutsideTheSessionPolicy(t *testing.T) {
	ws := initWorkspace(t)
	outside := filepath.Join(t.TempDir(), "victim.txt")
	inside := filepath.Join(ws, "restored.txt")

	writeOrphan(t, ws,
		[]opMeta{
			{N: 0, Path: outside, Perm: 0o644, Snapshotted: true},
			{N: 1, Path: inside, Perm: 0o644, Snapshotted: true},
		},
		map[int]string{0: "pwned\n", 1: "original\n"})

	Scan(ws, time.Now(), confinedTo(ws))

	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		got, _ := os.ReadFile(outside)
		t.Errorf("replay wrote outside the session's allowed roots: %s (content %q)", outside, got)
	}
	if got, _ := os.ReadFile(inside); string(got) != "original\n" {
		t.Errorf("the admitted op in the same manifest was not restored: %q", got)
	}
}

// TestScan_RefusesRelativePath pins the sub-case a relative path represents:
// os.WriteFile would anchor it to the daemon's working directory, which belongs
// to whichever client spawned the singleton process, not to this workspace.
//
// The guard here deliberately ADMITS EVERYTHING, so the only thing that can
// refuse the write is admitReplayPath's own absolute-path check. With
// confinedTo(ws) instead, this test could not fail: filepath.Rel of an absolute
// root against a relative path errors, so the guard would reject the payload
// whether or not the check under test existed — and the test would keep passing
// after the check was deleted. Verified by deleting it and re-running.
func TestScan_RefusesRelativePath(t *testing.T) {
	ws := initWorkspace(t)
	writeOrphan(t, ws,
		[]opMeta{{N: 0, Path: "escape-rel.txt", Perm: 0o644, Snapshotted: true}},
		map[int]string{0: "rel\n"})

	Scan(ws, time.Now(), func(string) error { return nil })

	// The daemon's cwd is the test's cwd here; a leaked write lands beside the
	// package sources.
	if _, err := os.Stat("escape-rel.txt"); err == nil {
		_ = os.Remove("escape-rel.txt")
		t.Error("replay wrote a relative manifest path against the process working directory")
	}
}

// TestScan_NilGuardFailsClosed proves the absent-policy case refuses rather than
// replaying unchecked. A caller with no boundary policy to consult is exactly
// the situation in which an unchecked replay is least defensible.
func TestScan_NilGuardFailsClosed(t *testing.T) {
	ws := initWorkspace(t)
	target := filepath.Join(ws, "in-workspace.txt")
	writeOrphan(t, ws,
		[]opMeta{{N: 0, Path: target, Perm: 0o644, Snapshotted: true}},
		map[int]string{0: "restored\n"})

	Scan(ws, time.Now(), nil)

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("a nil guard must refuse every op, even one inside the workspace")
	}
}

// TestScan_RestoresGuardAdmittedPathOutsideWorkspace is the test that pins the
// DESIGN, not just the fix. transaction_apply legitimately writes to configured
// extra roots and --allow-dir grants, so crash recovery must still restore them.
// A fix that checked workspace containment instead of consulting the session's
// policy would pass every other test in this file and silently break that.
func TestScan_RestoresGuardAdmittedPathOutsideWorkspace(t *testing.T) {
	ws := initWorkspace(t)
	extraRoot := t.TempDir() // stands in for a configured extra root
	target := filepath.Join(extraRoot, "lib.go")

	writeOrphan(t, ws,
		[]opMeta{{N: 0, Path: target, Perm: 0o644, Snapshotted: true}},
		map[int]string{0: "package lib\n"})

	// A guard admitting both roots, as the real PathPolicy would.
	wsGuard, extraGuard := confinedTo(ws), confinedTo(extraRoot)
	Scan(ws, time.Now(), func(path string) error {
		if err := wsGuard(path); err == nil {
			return nil
		}
		return extraGuard(path)
	})

	if got, _ := os.ReadFile(target); string(got) != "package lib\n" {
		t.Errorf("a path the session's policy admits must still be recoverable, got %q", got)
	}
}

// TestScan_DoesNotHonourManifestModeBits pins the second half of the payload.
// os.WriteFile applies perm only when it CREATES the file, so a manifest naming
// a file that does not exist is the one case its mode bits take effect — the
// recorded corpus entry pairs perm 0o777 with a shell script.
func TestScan_DoesNotHonourManifestModeBits(t *testing.T) {
	ws := initWorkspace(t)
	target := filepath.Join(ws, "new-exec")
	writeOrphan(t, ws,
		[]opMeta{{N: 0, Path: target, Perm: 0o777, Snapshotted: true}},
		map[int]string{0: "#!/bin/sh\necho pwned\n"})

	Scan(ws, time.Now(), confinedTo(ws))

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("expected the in-workspace op to be restored: %v", err)
	}
	if perm := info.Mode().Perm(); perm != replayPerm {
		t.Errorf("replay honoured the manifest's mode bits: got %#o, want %#o", perm, replayPerm)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Error("replay created an executable file from an untrusted manifest")
	}
}

// TestRollback_UsesInMemoryManifestNotDisk mutation-proofs the split between the
// two paths. Rollback is exempt from the guard precisely because it replays what
// this process recorded in memory; if it were still parsing the on-disk file, a
// corrupt manifest would stop it restoring anything — and the exemption would be
// unsound, because that file is attacker-writable.
func TestRollback_UsesInMemoryManifestNotDisk(t *testing.T) {
	ws := initWorkspace(t)
	target := filepath.Join(ws, "file.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := Begin(ws)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := l.Record(target, []byte("original"), 0o644); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := os.WriteFile(target, []byte("mid-transaction"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Corrupt the on-disk manifest: only a disk-reading rollback notices.
	if err := os.WriteFile(filepath.Join(l.dir, "manifest.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	l.Rollback()

	if got, _ := os.ReadFile(target); string(got) != "original" {
		t.Errorf("Rollback must replay its in-memory ops regardless of the on-disk manifest, got %q", got)
	}
}
