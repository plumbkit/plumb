package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serve_proxy_restart_message_test.go covers PLAN-459's third acceptance item: the
// error a reconnect owes an interrupted request must NAME the file when the request
// was a mutation_test, because "re-read the file to check whether it landed" is
// useless advice without a path.

// journalEntry writes one mutant-journal entry for path under the state dir the test
// selected, in the shape mutationtest_journal.go writes.
func journalEntry(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "plumb", "mutant-journal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"path": path, "sha_before": "aa", "sha_mutant": "bb", "mode": 420}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	// The file name is the target's digest in production; the reader only cares that
	// it ends in .json.
	if err := os.WriteFile(filepath.Join(dir, "entry.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProxyRestartMessage_IsUnchangedForEveryOtherTool(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	msg := proxyRestartMessage("read_file")
	if strings.Contains(msg, "mutation_test") {
		t.Errorf("only a mutation_test needs the extra sentence:\n%s", msg)
	}
	if !strings.Contains(msg, "outcome is unconfirmed") {
		t.Errorf("the base message must survive:\n%s", msg)
	}
}

func TestProxyRestartMessage_NamesTheMutantPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const target = "/tmp/repo/internal/tools/transaction.go"
	journalEntry(t, target)

	msg := proxyRestartMessage("mutation_test")
	if !strings.Contains(msg, target) {
		t.Errorf("the interrupted mutation_test must name the file:\n%s", msg)
	}
	if !strings.Contains(msg, "matches neither side") {
		t.Errorf("it must say why plumb left the file alone:\n%s", msg)
	}
}

func TestProxyRestartMessage_SaysWhenNothingIsLeftJournalled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	msg := proxyRestartMessage("mutation_test")
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
