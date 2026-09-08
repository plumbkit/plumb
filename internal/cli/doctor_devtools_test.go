package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/quality"
)

// The Dev Tools section exists because a missing golangci-lint used to be
// invisible: the post-write [quality] analyser skips silently, so on a machine
// where the binary sat in ~/go/bin outside the daemon's PATH the findings simply
// never appeared and nothing said why. It now reports the configured list, which
// covers the second silent shape: an entry plumb does not recognise at all.

func TestAnalyserResult_OKNamesLanguageAndPath(t *testing.T) {
	// ruff, not golangci-lint: the language ("go") is a substring of the binary
	// name "golangci-lint", so a Contains check on that row passes even with the
	// language prefix deleted. "python" appears nowhere in "/usr/bin/ruff".
	st := quality.ClassifyEntry("ruff", nil)
	st.Status, st.Binary = quality.EntryOK, "/usr/bin/ruff"

	got := analyserResult(st, true)
	if !got.ok || got.warn {
		t.Errorf("found: ok=%v warn=%v, want ok=true warn=false", got.ok, got.warn)
	}
	if got.name != "analyser ruff" {
		t.Errorf("name = %q", got.name)
	}
	if !strings.Contains(got.detail, "/usr/bin/ruff") {
		t.Errorf("detail should name the resolved path, got %q", got.detail)
	}
	// The language is what makes a list of several analysers readable, and it is
	// the thing a user cannot infer from a name they do not recognise.
	if !strings.HasPrefix(got.detail, "python ") {
		t.Errorf("detail should lead with the language, got %q", got.detail)
	}
	if got.fix != "" {
		t.Errorf("a clean pass needs no fix hint, got %q", got.fix)
	}
}

// Missing is a WARNING, never a failure: plumb works without it (writes still
// succeed), and doctor's exit code is reserved for things that are broken.
func TestAnalyserResult_MissingIsWarningNotFailure(t *testing.T) {
	got := analyserResult(quality.EntryState{
		Entry:  "ruff",
		Status: quality.EntryBinaryMissing,
		Reason: "executable not found on PATH",
	}, true)
	if !got.ok {
		t.Error("a missing optional tool must not fail doctor's exit code")
	}
	if !got.warn {
		t.Error("a missing tool must warn — silence is the bug this check exists to fix")
	}
	if !strings.Contains(got.detail, "will not run") {
		t.Errorf("detail must say what stops working, got %q", got.detail)
	}
	if !strings.Contains(got.fix, "quality.bin") {
		t.Errorf("a missing BINARY must point at the override that fixes it, got %q", got.fix)
	}
}

// An unsupported tool must NOT be sent to "install it": installing eslint does
// not make plumb run eslint, and a fix hint that does not fix anything is worse
// than none.
func TestAnalyserResult_UnsupportedIsNotAnInstallHint(t *testing.T) {
	got := analyserResult(quality.EntryState{
		Entry:  "eslint",
		Status: quality.EntryUnsupported,
		Reason: "recognised, but plumb has no adapter for it yet",
	}, true)
	if strings.Contains(got.fix, "install") {
		t.Errorf("an unsupported tool must not be reported as an install problem, got %q", got.fix)
	}
	if !strings.Contains(got.fix, "analysers") {
		t.Errorf("fix should point at the config key to change, got %q", got.fix)
	}
}

// [quality] is opt-in and off by default. Warning about an unusable entry in a
// block that is not running would put a yellow row in front of every user who
// has never touched this config.
func TestAnalyserResult_NoWarningWhileQualityIsOff(t *testing.T) {
	st := quality.EntryState{Entry: "eslint", Status: quality.EntryUnsupported, Reason: "no adapter"}
	if got := analyserResult(st, false); got.warn {
		t.Error("a broken analyser entry must not warn while [quality] enabled = false")
	}
	if got := analyserResult(st, true); !got.warn {
		t.Error("the same entry must warn once [quality] is switched on")
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

// Host-independent: whatever this machine has installed, and whatever the
// user's config lists, the section always reports the clipboard row and one row
// per configured analyser. The clipboard row is the invariant — it must survive
// an unloadable config, which is the path that returns early.
func TestCheckDevTools_AlwaysReportsClipboardAndOneRowPerAnalyser(t *testing.T) {
	got := checkDevTools()
	if len(got) == 0 {
		t.Fatal("Dev Tools reported nothing at all")
	}
	names := make([]string, 0, len(got))
	analysers := 0
	for _, r := range got {
		names = append(names, r.name)
		if strings.HasPrefix(r.name, "analyser ") {
			analysers++
		}
	}
	if !containsString(names, "clipboard") {
		t.Errorf("Dev Tools missing the clipboard row; got %v", names)
	}
	if analysers != len(got)-1 {
		t.Errorf("every non-clipboard row must be an analyser row; got %v", names)
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
