package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/redact"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestBuildEpisodic(t *testing.T) {
	calls := []stats.Call{
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/internal/a.go"}`},
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/internal/b.go"}`},
		{Tool: "find_references", InputJSON: `{"name":"UserSession"}`},
		{Tool: "read_file", InputJSON: `{"file_path":"/ws/c.go"}`},
		{Tool: "read_file", InputJSON: `{}`},
	}
	summary, touched, readN, writeN := buildEpisodic(calls)
	if writeN != 2 {
		t.Errorf("writeN = %d, want 2", writeN)
	}
	if readN != 3 {
		t.Errorf("readN = %d, want 3", readN)
	}
	if len(touched) != 2 {
		t.Errorf("touched = %v, want [a.go b.go]", touched)
	}
	for _, want := range []string{"modified", "a.go", "b.go", "UserSession"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q: %s", want, summary)
		}
	}
}

func TestBuildEpisodic_EmptyWhenNoActivity(t *testing.T) {
	if s, _, _, _ := buildEpisodic(nil); s != "" {
		t.Errorf("empty calls should yield empty summary, got %q", s)
	}
}

// TestBuildEpisodic_RedactionComposes proves the pipeline scrubs a secret: a
// symbol name carrying a token must not survive redaction of the summary.
func TestBuildEpisodic_RedactionComposes(t *testing.T) {
	calls := []stats.Call{
		{Tool: "find_symbol", InputJSON: `{"name":"ghp_0123456789abcdefghijklmnopqrstuvwxyz1"}`},
		{Tool: "edit_file", InputJSON: `{"file_path":"/ws/a.go"}`},
	}
	summary, _, _, _ := buildEpisodic(calls)
	cleaned, n := redact.Redact(summary)
	if n == 0 || strings.Contains(cleaned, "ghp_0123456789abcdefghijklmnopqrstuvwxyz1") {
		t.Errorf("secret survived redaction: %s", cleaned)
	}
}

func TestClampRunes(t *testing.T) {
	if got := clampRunes("hello world", 5); got != "hello…" {
		t.Errorf("clampRunes = %q", got)
	}
	if got := clampRunes("short", 0); got != "short" {
		t.Errorf("zero budget should be a no-op, got %q", got)
	}
}
