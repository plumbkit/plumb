package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The reply half of issue #528: edit_file's `mtime:` line is what an agent
// passes back as expected_mtime, so it must name the version plumb wrote and
// recorded, not whatever the path holds by the time the reply is formatted.
// edit_file formats it after the post-write diagnostics wait, and a re-stat
// there handed the caller an outside writer's mtime. That mtime matched the
// file, changedAtSameMtime could not second-guess it (the recorded read sits at
// plumb's mtime), and the next write went over the outside change.

var replyMtimeRe = regexp.MustCompile(`(?m)^mtime: (\S+)`)

func replyMtime(t *testing.T, out string) string {
	t.Helper()
	m := replyMtimeRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no mtime line in reply:\n%s", out)
	}
	return m[1]
}

// editReplyCases are the two edit_file replies that print an mtime.
var editReplyCases = []struct {
	name    string
	partial bool
}{
	{"edit_file", false},
	{"edit_file apply_partial", true},
}

func editArgs(path string, partial bool, expectedMtime, old, repl string) map[string]any {
	args := map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": old, "new_string": repl}},
	}
	if partial {
		args["apply_partial"] = true
	}
	if expectedMtime != "" {
		args["expected_mtime"] = expectedMtime
	}
	return args
}

// TestEditFile_ReplyMtimeIsTheWrittenVersion: an outsider lands after the
// rename and before the reply is returned. The reply's mtime must be the one
// plumb recorded, and a write guarded by it must be refused. (apply_partial
// formats its header before its first post-write hook, so for it this pins the
// contract rather than reproducing a live window.)
func TestEditFile_ReplyMtimeIsTheWrittenVersion(t *testing.T) {
	for _, c := range editReplyCases {
		for _, m := range outsiderModes {
			t.Run(c.name+"/"+m.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "f.go")
				if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				deps := raceDeps()
				readVia(t, deps.Reads, path)
				hook := onceHook(path, func() { outsiderWrite(t, path, "outsider\n", m.keepMtime) })
				deps.PostWriteNotifyFn = func(_ context.Context, p string) error { hook(p); return nil }
				out, err := NewEditFile(deps).Execute(context.Background(), mustJSON(editArgs(path, c.partial, "", "a", "A")))
				if err != nil {
					t.Fatal(err)
				}
				got := replyMtime(t, out)
				e, _ := deps.Reads.recorded(path)
				if want := e.mtime.Format(time.RFC3339Nano); got != want {
					t.Fatalf("reply mtime %s, want the recorded (written) version %s", got, want)
				}
				deps.PostWriteNotifyFn = nil
				_, err = NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{
					"file_path": path, "content": "clobber\n", "expected_mtime": got,
				}))
				if err == nil || !strings.Contains(err.Error(), "modified since you read it") {
					t.Fatalf("a write guarded by the reply's mtime must be refused over the outside change, got: %v", err)
				}
				if data, _ := os.ReadFile(path); string(data) != "outsider\n" {
					t.Fatalf("outside change was overwritten: %q", data)
				}
			})
		}
	}
}

// TestEditFile_ReplyMtimeRoundTrips is the positive control: with no outsider,
// each reply's mtime is accepted as the next call's expected_mtime (strict mode
// on, no re-read), and a peer write after it is still refused.
func TestEditFile_ReplyMtimeRoundTrips(t *testing.T) {
	t.Setenv("PLUMB_STRICT_EDITS", "1")
	for _, c := range editReplyCases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.go")
			deps := raceDeps()
			ctx := context.Background()
			if _, err := NewWriteFile(deps).Execute(ctx, mustJSON(map[string]any{"file_path": path, "content": "a\nb\nc\n"})); err != nil {
				t.Fatal(err)
			}
			out, err := NewEditFile(deps).Execute(ctx, mustJSON(editArgs(path, c.partial, "", "a", "A")))
			if err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(path); err != nil || replyMtime(t, out) != info.ModTime().Format(time.RFC3339Nano) {
				t.Fatalf("reply mtime %s is not the file's mtime (%v)", replyMtime(t, out), err)
			}
			out, err = NewEditFile(deps).Execute(ctx, mustJSON(editArgs(path, c.partial, replyMtime(t, out), "b", "B")))
			if err != nil || strings.Contains(out, "FAILED") {
				t.Fatalf("an edit guarded by the previous reply's mtime must apply: %v\n%s", err, out)
			}
			mt := replyMtime(t, out)
			peerRewrite(t, path, "A\nB\nPEER\n")
			if _, err := NewWriteFile(deps).Execute(ctx, mustJSON(map[string]any{
				"file_path": path, "content": "x\n", "expected_mtime": mt,
			})); err == nil {
				t.Fatal("a write guarded by a reply's mtime must be refused after a peer write")
			}
		})
	}
}
