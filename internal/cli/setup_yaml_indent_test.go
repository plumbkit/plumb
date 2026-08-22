package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// plumb registers one entry in a client's YAML config and must leave the rest
// of the file alone. yaml.Marshal always emits 4-space indentation, so writing
// a 2-space config re-indented every line plumb does not own — which is how a
// real ~/.hermes/config.yaml ended up with `plugins.enabled` at two depths and
// stopped parsing. These tests pin the indent-preserving writer that fixes it.

func TestDetectYAMLIndent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"two-space config", "a:\n  b: 1\n  c:\n    - x\n", 2},
		{"four-space config", "a:\n    b: 1\n    c:\n        - x\n", 4},
		{"flat file reveals nothing", "a: 1\nb: 2\n", 0},
		{"empty file reveals nothing", "", 0},
		{"deeply nested first line still reports one level", "a:\n        b:\n  c: 1\n", 2},
		{"comment indentation is ignored", "a:\n   # aligned note\n  b: 1\n", 2},
		{"tab-indented lines are ignored", "a:\n\tb: 1\n  c: 2\n", 2},
		{"an absurd run falls back to nothing", "a:\n" + strings.Repeat(" ", 12) + "b: 1\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectYAMLIndent([]byte(tc.in)); got != tc.want {
				t.Errorf("detectYAMLIndent(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestMarshalYAMLLike_MatchesTheExistingIndent(t *testing.T) {
	v := map[string]any{"outer": map[string]any{"inner": "v"}}

	two, err := marshalYAMLLike(v, []byte("a:\n  b: 1\n"))
	if err != nil {
		t.Fatalf("marshalYAMLLike: %v", err)
	}
	if !strings.Contains(string(two), "\n  inner:") {
		t.Errorf("a 2-space file must stay 2-space, got:\n%s", two)
	}

	four, err := marshalYAMLLike(v, []byte("a:\n    b: 1\n"))
	if err != nil {
		t.Fatalf("marshalYAMLLike: %v", err)
	}
	if !strings.Contains(string(four), "\n    inner:") {
		t.Errorf("a 4-space file must stay 4-space, got:\n%s", four)
	}

	// A file plumb creates has no indent to copy and must look as it always
	// has, or every existing fixture and user config changes shape on upgrade.
	fresh, err := marshalYAMLLike(v, nil)
	if err != nil {
		t.Fatalf("marshalYAMLLike: %v", err)
	}
	plain, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if string(fresh) != string(plain) {
		t.Errorf("a new file must match yaml.Marshal exactly:\ngot  %q\nwant %q", fresh, plain)
	}
}

// The regression proper: register plumb in a 2-space config and every line
// plumb does not own must come back byte-identical.
func TestWriteYAML_LeavesForeignLinesUntouched(t *testing.T) {
	const original = "" +
		"model: some-model\n" +
		"plugins:\n" +
		"  enabled:\n" +
		"    - herdr-agent-state\n" +
		"routing:\n" +
		"  channels:\n" +
		"    telegram:\n" +
		"      - hermes-telegram\n"

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	m, _, err := readOrInitYAMLConfig(path)
	if err != nil {
		t.Fatalf("readOrInitYAMLConfig: %v", err)
	}
	m["mcp_servers"] = map[string]any{"plumb": map[string]any{"command": "/opt/plumb"}}
	if err := writeYAML(path, m); err != nil {
		t.Fatalf("writeYAML: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Compared as WHOLE lines, never with strings.Contains: a re-indented
	// "    enabled:" contains "  enabled:" as a substring, so a Contains check
	// passes under exactly the bug this test exists to catch.
	got := make(map[string]bool)
	for line := range strings.SplitSeq(string(after), "\n") {
		got[line] = true
	}
	for line := range strings.SplitSeq(original, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !got[line] {
			t.Errorf("plumb restyled a line it does not own:\nwant line %q\ngot file:\n%s", line, after)
		}
	}

	// And the file must still parse — the corruption this guards against was
	// only visible on the NEXT read, not the write.
	var check map[string]any
	if err := yaml.Unmarshal(after, &check); err != nil {
		t.Fatalf("the rewritten config no longer parses: %v\n%s", err, after)
	}
	if _, ok := check["mcp_servers"]; !ok {
		t.Error("plumb's own entry did not survive the write")
	}
}
