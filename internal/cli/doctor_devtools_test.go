package cli

import (
	"strings"
	"testing"
)

// The Dev Tools section exists because a missing golangci-lint used to be
// invisible: the post-write [quality] analyser skips silently, so on a machine
// where the binary sat in ~/go/bin outside the daemon's PATH the findings simply
// never appeared and nothing said why.

func TestGolangciLintResult_Found(t *testing.T) {
	got := golangciLintResult("/usr/bin/golangci-lint", true)
	if !got.ok || got.warn {
		t.Errorf("found: ok=%v warn=%v, want ok=true warn=false", got.ok, got.warn)
	}
	if got.name != "golangci-lint" {
		t.Errorf("name = %q", got.name)
	}
	if !strings.Contains(got.detail, "golangci-lint") {
		t.Errorf("detail should name the resolved path, got %q", got.detail)
	}
	if got.fix != "" {
		t.Errorf("a clean pass needs no fix hint, got %q", got.fix)
	}
}

// Missing is a WARNING, never a failure: plumb works without it (writes still
// succeed), and doctor's exit code is reserved for things that are broken.
func TestGolangciLintResult_MissingIsWarningNotFailure(t *testing.T) {
	got := golangciLintResult("", false)
	if !got.ok {
		t.Error("a missing optional tool must not fail doctor's exit code")
	}
	if !got.warn {
		t.Error("a missing tool must warn — silence is the bug this check exists to fix")
	}
	if !strings.Contains(got.detail, "disabled") {
		t.Errorf("detail must say what stops working, got %q", got.detail)
	}
	if got.fix == "" {
		t.Error("a warning must carry an actionable fix hint")
	}
}

// Both the human and the --json path must run the same checks. They were
// declared as two separate lists, so a new section could appear in one and be
// silently missing from the other.
func TestDoctorSections_SingleSourceOfTruthIncludesDevTools(t *testing.T) {
	titles := make([]string, 0, 8)
	for _, s := range doctorSections(t.TempDir()) {
		titles = append(titles, s.title)
		if s.run == nil {
			t.Errorf("section %q has a nil run func", s.title)
		}
	}
	for _, want := range []string{"Daemon", "Language Servers", "MCP Clients", "Configuration", "Dev Tools", "Integrations", "Data", "Indexing"} {
		if !containsString(titles, want) {
			t.Errorf("doctorSections missing %q; got %v", want, titles)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
