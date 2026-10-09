package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serve_proxy_restart_message_test.go covers PLAN-459's third acceptance item: the
// error a reconnect owes an interrupted request must NAME the file when the request
// was a mutation_test, because "re-read the file to check whether it landed" is
// useless advice without a path — and it must word that file's OWN state, because
// "plumb left it exactly as it was" is false of a file that still holds the mutant.

// journalEntry writes one mutant-journal entry for path under the state dir the test
// selected, in the shape mutationtest_journal.go writes: the two sides' digests,
// computed from real bytes because the classifier hashes the file rather than trusting
// the entry. The mutant is left applied, as a killed run leaves it.
func journalEntry(t *testing.T, path, original, mutant string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "plumb", "mutant-journal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{
		"path":       path,
		"sha_before": journalDigest(original),
		"sha_mutant": journalDigest(mutant),
		"mode":       420,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	// Production names the file by the target's digest; two entries in one journal
	// must not overwrite each other here either.
	if err := os.WriteFile(filepath.Join(dir, journalDigest(path)+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(mutant), 0o644); err != nil {
		t.Fatal(err)
	}
}

// journalDigest is the hex sha256 the journal records for a file's content.
func journalDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestProxyRestartMessage_IsUnchangedForEveryOtherTool(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	msg := proxyRestartMessage("read_file", "")
	if strings.Contains(msg, "mutation_test") {
		t.Errorf("only a mutation_test needs the extra sentence:\n%s", msg)
	}
	if !strings.Contains(msg, "outcome is unconfirmed") {
		t.Errorf("the base message must survive:\n%s", msg)
	}
}

// TestProxyRestartMessage_WordsEachJournalState drives the four things a journalled
// file can be. The note used to call every one of them "matches neither side, so plumb
// left it exactly as it was" — including a file that still holds the mutant, which is
// the one case where the caller MUST re-read before retrying.
func TestProxyRestartMessage_WordsEachJournalState(t *testing.T) {
	const original, mutant = "func f() int { return 1 }\n", "func f() int { return 2 }\n"
	const theirs = "func f() int { return 3 } // edited after the run\n"
	cases := []struct {
		name    string
		leave   func(t *testing.T, path string)
		want    string
		notWant string
	}{
		{
			name:    "still applied",
			leave:   func(t *testing.T, _ string) { t.Helper() },
			want:    "a mutant is still applied at",
			notWant: "matches neither side",
		},
		{
			name: "already back",
			leave: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "is already back to its pre-run content",
		},
		{
			name: "a third party's edit",
			leave: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(theirs), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "matches neither side, so plumb left it exactly as it was",
		},
		{
			name: "the file is gone",
			leave: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			want: "no longer exists",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			target := filepath.Join(t.TempDir(), "transaction.go")
			journalEntry(t, target, original, mutant)
			tc.leave(t, target)

			msg := proxyRestartMessage("mutation_test", "")
			if !strings.Contains(msg, target) {
				t.Errorf("the interrupted mutation_test must name the file:\n%s", msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("the state must be worded for what is true of it (%q):\n%s", tc.want, msg)
			}
			if tc.notWant != "" && strings.Contains(msg, tc.notWant) {
				t.Errorf("a file that still holds the mutant is not one plumb left alone (%q):\n%s", tc.notWant, msg)
			}
			if !strings.Contains(msg, "outcome is unconfirmed") {
				t.Errorf("the base sentence must survive:\n%s", msg)
			}
		})
	}
}

// TestProxyRestartMessage_GroupsPathsByState keeps two entries in different states in
// their own sentences — one "the file matches neither side" for both would be false of
// the file still holding its mutant — and keeps the plural wording for entries that
// share a state.
func TestProxyRestartMessage_GroupsPathsByState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const original, mutant = "func f() int { return 1 }\n", "func f() int { return 2 }\n"
	const theirs = "func f() int { return 3 } // edited after the run\n"
	dir := t.TempDir()
	applied, alsoApplied := filepath.Join(dir, "applied.go"), filepath.Join(dir, "also_applied.go")
	edited := filepath.Join(dir, "edited.go")
	journalEntry(t, applied, original, mutant)
	journalEntry(t, alsoApplied, original, mutant)
	journalEntry(t, edited, original, mutant)
	if err := os.WriteFile(edited, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	msg := proxyRestartMessage("mutation_test", "")
	if !strings.Contains(msg, "mutants are still applied at") ||
		!strings.Contains(msg, applied) || !strings.Contains(msg, alsoApplied) {
		t.Errorf("the two still-applied files need one plural sentence naming both:\n%s", msg)
	}
	if !strings.Contains(msg, "the file at "+edited+" matches neither side") {
		t.Errorf("the foreign edit needs its own sentence, naming it:\n%s", msg)
	}
}

func TestProxyRestartMessage_SaysWhenNothingIsLeftJournalled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	msg := proxyRestartMessage("mutation_test", "")
	if !strings.Contains(msg, "nothing is left journalled") {
		t.Errorf("an empty journal is good news and must be told as such:\n%s", msg)
	}
}

// TestCalledTool reads the tool name out of the frame the proxy already parses —
// the whole of what it knows about a request's body.
func TestCalledTool(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{"a tools/call", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mutation_test","arguments":{}}}`, "mutation_test"},
		{"another method", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, ""},
		{"no params", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, ""},
		{"params without a name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{}}}`, ""},
		{"malformed params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"nope"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := calledTool(parseEnvelope([]byte(tc.frame))); got != tc.want {
				t.Errorf("calledTool = %q, want %q", got, tc.want)
			}
		})
	}
}
