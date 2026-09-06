package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// defaultsPool builds a pool from the SHIPPED default configuration, keeping
// only the named languages and pointing each at a binary that certainly exists
// so the result does not depend on what is installed on the test machine.
//
// Building from config.Defaults() rather than hand-written langConfigs is the
// point: these tests are about which language a REAL plumb resolves for a
// directory, so the markers under test have to be the shipped ones. A
// hand-written fixture would keep passing after someone edited the defaults.
func defaultsPool(t *testing.T, names ...string) *workspacePool {
	t.Helper()
	cfg := config.Defaults()
	for name := range cfg.LSP {
		if !contains(names, name) {
			delete(cfg.LSP, name)
			continue
		}
		c := cfg.LSP[name]
		c.Command = "go" // always present: the toolchain running this test
		c.Enabled = true
		cfg.LSP[name] = c
	}
	for _, name := range names {
		if _, ok := cfg.LSP[name]; !ok {
			t.Fatalf("no [lsp.%s] in the shipped defaults", name)
		}
	}
	return newWorkspacePool(context.Background(), cfg)
}

// TestExtLangAt_GitignoredTreeCastsNoVote is the regression test for the
// reported case: a Python service that gitignores a generated tree of HTML
// reports resolved html, and plumb started an HTML language server for a repo
// whose sources are Python. What a repository excludes from version control is
// not what it is written in, and the sniff could not tell the difference —
// worse, the generated tree is exactly the kind that is large enough to spend
// the file budget before the real sources are reached.
func TestExtLangAt_GitignoredTreeCastsNoVote(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, ".gitignore"), "archive/\n")
	for i := range 3 {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("service%d.py", i)), "x = 1\n")
	}
	mustWrite(t, filepath.Join(dir, "index.html"), "<html></html>\n")
	for i := range 40 {
		mustWrite(t, filepath.Join(dir, "archive", fmt.Sprintf("report%03d.html", i)), "<html></html>\n")
	}

	if got := defaultsPool(t, "python", "html").extLangAt(dir); got != "python" {
		t.Errorf("extLangAt = %q, want python — 40 gitignored HTML reports are not "+
			"what this repo is written in; 3 tracked .py files against 1 tracked .html are", got)
	}
}

// TestExtLangAt_NestedGitignoreIsHonoured: the rules of a subdirectory apply to
// that subdirectory, so a repo whose generated output is excluded from inside
// the directory that produces it is read the same way git reads it. This is the
// half a root-only .gitignore read would miss.
func TestExtLangAt_NestedGitignoreIsHonoured(t *testing.T) {
	dir := freshTempDir(t)
	for i := range 3 {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("service%d.py", i)), "x = 1\n")
	}
	mustWrite(t, filepath.Join(dir, "web", ".gitignore"), "generated/\n")
	for i := range 40 {
		mustWrite(t, filepath.Join(dir, "web", "generated", fmt.Sprintf("page%03d.html", i)), "<html></html>\n")
	}

	if got := defaultsPool(t, "python", "html").extLangAt(dir); got != "python" {
		t.Errorf("extLangAt = %q, want python — web/.gitignore excludes web/generated/, "+
			"so its 40 pages are no more part of this repo than a root-excluded tree is", got)
	}
}

// A negation inside the excluded directory must NOT re-include it — gitignore's
// rule is that git never descends into an excluded directory, so a `!` line
// beneath one has nothing to apply to. Pinned here because the walk is what
// makes that rule hold (ignore.Stack.IsIgnored answers for a walk that prunes),
// and a walk that filtered files instead of pruning directories would quietly
// re-admit every one of these pages.
func TestExtLangAt_NegationInsideAnExcludedTreeDoesNotReadmitIt(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, ".gitignore"), "archive/\n")
	mustWrite(t, filepath.Join(dir, "archive", ".gitignore"), "!*.html\n")
	for i := range 3 {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("service%d.py", i)), "x = 1\n")
	}
	for i := range 40 {
		mustWrite(t, filepath.Join(dir, "archive", fmt.Sprintf("report%03d.html", i)), "<html></html>\n")
	}

	if got := defaultsPool(t, "python", "html").extLangAt(dir); got != "python" {
		t.Errorf("extLangAt = %q, want python — an excluded directory cannot be "+
			"re-included from inside itself", got)
	}
}

// The hardcoded prunes are ADDITIVE with .gitignore, never replaced by it: a
// repository is free not to ignore node_modules (committing it is a real, if
// unfashionable, practice) and the sniff must still not read it as the project's
// language. Pinned because the natural way to write this change — swap skipDir
// for the ignore stack — passes every other test in this file.
func TestExtLangAt_HardcodedPrunesSurviveAGitignorelessRepo(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "app.py"), "x = 1\n")
	for i := range 40 {
		mustWrite(t, filepath.Join(dir, "node_modules", fmt.Sprintf("p%03d.html", i)), "<html></html>\n")
	}

	if got := defaultsPool(t, "python", "html").extLangAt(dir); got != "python" {
		t.Errorf("extLangAt = %q, want python — node_modules is pruned by name, "+
			"with or without a .gitignore", got)
	}
}

// An ignored file must not be CHARGED against the file budget either. A repo
// that excludes far more than it tracks would otherwise exhaust the cap on files
// that contribute no count and report a truncated scan — the same defect in a
// quieter form, and one that changes the tie-break's answer rather than the
// sniff's.
func TestSniffCounts_IgnoredFilesDoNotSpendTheBudget(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, ".gitignore"), "*.log\n")
	mustWrite(t, filepath.Join(dir, "app.py"), "x = 1\n")
	manyFiles(t, dir, "logs", "run", ".log", extScanMaxFiles+500)

	counts, truncated := defaultsPool(t, "python", "html").sniffCounts(dir, extScanDepth, extScanMaxFiles, nil, skipChildDir)
	if truncated {
		t.Errorf("truncated = true (counts=%v) — %d ignored files spent a budget they "+
			"should never have been charged against", counts, extScanMaxFiles+500)
	}
	if counts["python"] != 1 {
		t.Errorf("counts = %v, want one python file", counts)
	}
}
