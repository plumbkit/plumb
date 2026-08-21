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

func TestClipboardToolResult_FoundNamesTheToolAndPath(t *testing.T) {
	got := clipboardToolResult("wl-copy", "/usr/bin/wl-copy", "")
	if !got.ok || got.warn {
		t.Errorf("found: ok=%v warn=%v, want ok=true warn=false", got.ok, got.warn)
	}
	if got.name != "clipboard" {
		t.Errorf("the row name must be stable across platforms so --json consumers see one key, got %q", got.name)
	}
	if !strings.Contains(got.detail, "wl-copy") {
		t.Errorf("detail should name the helper, got %q", got.detail)
	}
	if got.fix != "" {
		t.Errorf("a resolved helper needs no fix hint, got %q", got.fix)
	}
}

func TestClipboardToolResult_MissingHelperWarnsWithInstallFix(t *testing.T) {
	got := clipboardToolResult("", "", "install wl-clipboard")
	if !got.ok {
		t.Error("a missing clipboard helper must not fail doctor's exit code — OSC 52 still works")
	}
	if !got.warn {
		t.Error("a missing helper must warn")
	}
	if !strings.Contains(got.detail, "OSC 52") {
		t.Errorf("detail must say what happens instead, got %q", got.detail)
	}
	if got.fix != "install wl-clipboard" {
		t.Errorf("fix must carry the hint through verbatim, got %q", got.fix)
	}
}

// A headless box (SSH, bare TTY) has nothing to install, and a warning on every
// SSH session is how a warning gets ignored.
func TestClipboardToolResult_NoDisplayIsNotAWarning(t *testing.T) {
	got := clipboardToolResult("", "", "")
	if !got.ok || got.warn {
		t.Errorf("headless: ok=%v warn=%v, want ok=true warn=false", got.ok, got.warn)
	}
	if got.fix != "" {
		t.Errorf("nothing to fix on a headless box, got %q", got.fix)
	}
}

// Host-independent: whatever this machine has installed, the section reports
// both tools.
func TestCheckDevTools_ReportsBothTools(t *testing.T) {
	got := checkDevTools()
	if len(got) != 2 {
		t.Fatalf("want 2 dev-tool rows, got %d", len(got))
	}
	names := []string{got[0].name, got[1].name}
	for _, want := range []string{"golangci-lint", "clipboard"} {
		if !containsString(names, want) {
			t.Errorf("Dev Tools missing %q; got %v", want, names)
		}
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
