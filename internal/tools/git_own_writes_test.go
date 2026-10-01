package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// git_own_writes_test.go covers #529 item 1: a file plumb wrote this session and
// then changed on disk through its OWN git tool (a branch switch, a merge) must
// not be reported by the next read as edited by "a peer or external process".
// A real peer's edit, before or after the git operation, must still be.

const peerEditWarning = "changed on disk since plumb last wrote it"

// ownWritesFixture is mergeFixture plus a branch "other" whose init.txt differs
// from main's, and a write tracker that records init.txt as written by plumb
// this session. The recorded mtime is backdated an hour so that git's rewrite of
// the file is unambiguously later, whatever the filesystem's timestamp
// granularity.
func ownWritesFixture(t *testing.T) (repo string, tracker *WriteTracker, tool *Git) {
	t.Helper()
	repo = mergeFixture(t)
	commitFileDirect(t, repo, "other", "init.txt", "other's version\n")
	tracker = NewWriteTracker()
	backdate(t, filepath.Join(repo, "init.txt"), -time.Hour)
	tracker.Record(filepath.Join(repo, "init.txt"))
	tool = NewGit(
		WriteDeps{WorkspaceFn: func(context.Context) string { return repo }, Writes: tracker},
		func() GitPolicy { return GitPolicy{AllowWrites: true} },
	)
	return repo, tracker, tool
}

func backdate(t *testing.T, path string, by time.Duration) {
	t.Helper()
	when := time.Now().Add(by)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func readWarns(t *testing.T, tracker *WriteTracker, path string) bool {
	t.Helper()
	out, err := callReadFileWith(t, NewReadFile(nil).WithWrites(tracker), path)
	if err != nil {
		t.Fatalf("read_file %s: %v", path, err)
	}
	return strings.Contains(out, peerEditWarning)
}

// TestGit_OwnSwitchIsNotReportedAsAPeerEdit is the reported case: a switch
// through the tool rewrites a file plumb wrote, and the next read blamed a peer.
func TestGit_OwnSwitchIsNotReportedAsAPeerEdit(t *testing.T) {
	repo, tracker, tool := ownWritesFixture(t)
	path := filepath.Join(repo, "init.txt")
	if _, err := callGit(t, tool, map[string]any{"subcommand": "switch", "args": []string{"other"}}); err != nil {
		t.Fatalf("switch other: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "other's version\n" {
		t.Fatalf("the switch did not rewrite init.txt (got %q), so this test proves nothing", data)
	}
	if readWarns(t, tracker, path) {
		t.Error("read after plumb's own switch warned of a peer edit")
	}

	// Positive control: a peer's edit AFTER the switch is still reported.
	if err := os.WriteFile(path, []byte("peer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(t, path, time.Hour)
	if !readWarns(t, tracker, path) {
		t.Error("a peer edit after the switch was not reported")
	}
}

// TestGit_PeerEditBeforeTheOpStillWarns: a file the peer edited BEFORE plumb's
// git operation, and which the operation did not touch, keeps its warning — the
// refresh must not launder an edit plumb did not make.
func TestGit_PeerEditBeforeTheOpStillWarns(t *testing.T) {
	repo, tracker, tool := ownWritesFixture(t)
	notes := filepath.Join(repo, "notes.txt") // untracked by git, so no switch touches it
	if err := os.WriteFile(notes, []byte("plumb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(t, notes, -time.Hour)
	tracker.Record(notes)
	if err := os.WriteFile(notes, []byte("peer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callGit(t, tool, map[string]any{"subcommand": "switch", "args": []string{"other"}}); err != nil {
		t.Fatalf("switch other: %v", err)
	}
	if !readWarns(t, tracker, notes) {
		t.Error("a peer edit made before plumb's switch lost its warning")
	}
}

// TestGit_OwnConflictedMergeIsNotReportedAsAPeerEdit: a merge that stops on
// conflicts FAILS, and still rewrote the conflicted file (conflict markers). That
// is plumb's own change too, so the failure path must refresh as well.
func TestGit_OwnConflictedMergeIsNotReportedAsAPeerEdit(t *testing.T) {
	repo, tracker, tool := ownWritesFixture(t)
	path := filepath.Join(repo, "init.txt")
	commitFileDirect(t, repo, "main", "init.txt", "main's version\n")
	backdate(t, path, -time.Hour)
	tracker.Record(path)
	if _, err := callGit(t, tool, map[string]any{"subcommand": "merge", "args": []string{"--no-edit", "other"}}); err == nil {
		t.Fatal("a conflicting merge reported success")
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "<<<<<<<") {
		t.Fatalf("init.txt carries no conflict markers (%q), so this test proves nothing", data)
	}
	if readWarns(t, tracker, path) {
		t.Error("read after plumb's own conflicted merge warned of a peer edit")
	}
}

// TestGit_HookWritesDuringOwnOpStillWarn is the #540 review's N1: a hook runs
// INSIDE the operation's window, so an mtime that moved during it is not proof
// git wrote the file. A write by pre-merge-commit (or post-checkout) to a file
// git did not produce keeps its warning; the file git did produce — init.txt,
// rewritten from the other branch — is still re-recorded.
func TestGit_HookWritesDuringOwnOpStillWarn(t *testing.T) {
	for _, c := range []struct {
		hook string
		args map[string]any
	}{
		{"pre-merge-commit", map[string]any{"subcommand": "merge", "args": []string{"--no-ff", "--no-edit", "other"}}},
		{"post-checkout", map[string]any{"subcommand": "switch", "args": []string{"other"}}},
	} {
		t.Run(c.hook, func(t *testing.T) {
			repo, tracker, tool := ownWritesFixture(t)
			tracked := filepath.Join(repo, "main.txt") // committed; the op leaves it alone
			untracked := filepath.Join(repo, "notes.txt")
			if err := os.WriteFile(untracked, []byte("plumb\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{tracked, untracked} {
				backdate(t, p, -time.Hour)
				tracker.Record(p)
			}
			installHook(t, repo, c.hook, "echo hook >> main.txt\necho hook >> notes.txt\n")
			if _, err := callGit(t, tool, c.args); err != nil {
				t.Fatalf("git %v: %v", c.args, err)
			}
			for _, p := range []string{tracked, untracked} {
				if data, _ := os.ReadFile(p); !strings.Contains(string(data), "hook") {
					t.Fatalf("the %s hook never wrote %s, so this proves nothing", c.hook, p)
				}
				if !readWarns(t, tracker, p) {
					t.Errorf("%s: a write by the %s hook was absorbed as plumb's own", filepath.Base(p), c.hook)
				}
			}
			if readWarns(t, tracker, filepath.Join(repo, "init.txt")) {
				t.Error("init.txt, which git itself rewrote, was reported as a peer edit")
			}
		})
	}
}
