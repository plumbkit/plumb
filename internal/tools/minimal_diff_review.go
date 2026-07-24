package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/minchange"
	"github.com/plumbkit/plumb/internal/tokenise"
	"github.com/plumbkit/plumb/internal/topology"
)

// maxReviewDiffBytes bounds the diff text fed to the analyser, so a huge diff
// cannot blow the review's memory or runtime. Findings past the cut are simply
// not seen — the response says so.
const maxReviewDiffBytes = 1 * 1024 * 1024 // 1 MiB

var minimalDiffReviewSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "base_ref": {
      "type": "string",
      "description": "Git ref to diff against (default HEAD, i.e. review uncommitted changes)."
    },
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Restrict the review to these paths (workspace-relative or absolute). Strongly recommended in a shared worktree so unrelated peer-agent edits are excluded."
    },
    "mode": {
      "type": "string",
      "enum": ["changed", "staged"],
      "description": "changed (default) reviews the working tree vs base_ref (all uncommitted changes); staged reviews only the index vs base_ref."
    },
    "max_findings": {
      "type": "integer",
      "description": "Cap on findings returned (default 20, max 100)."
    },
    "include_suggestions": {
      "type": "boolean",
      "description": "Include a concrete smaller-alternative line per finding (default true)."
    }
  },
  "additionalProperties": false
}`)

// MinimalDiffReview is an advisory diff-review tool: it flags places a change
// may have been over-built. It is read-only and never blocks a write.
//
// Concurrency: Execute is safe for concurrent use.
type MinimalDiffReview struct {
	storeFn func() *topology.Store
	ws      WorkspaceFn
	guard   BoundaryGuard // rejects a files entry outside the pinned workspace
}

// NewMinimalDiffReview returns a new MinimalDiffReview tool. storeFn supplies the
// topology store (may return nil when indexing is disabled — the approximate
// checks then degrade and say so).
func NewMinimalDiffReview(storeFn func() *topology.Store) *MinimalDiffReview {
	return &MinimalDiffReview{storeFn: storeFn}
}

// WithWorkspace wires the pinned-workspace accessor, used to locate the git
// repository and to bound the review to the workspace. Nil-safe.
func (t *MinimalDiffReview) WithWorkspace(ws WorkspaceFn) *MinimalDiffReview {
	t.ws = ws
	return t
}

// WithBoundary wires the workspace boundary guard so a files entry pointing
// outside the pinned workspace is refused. Nil-safe.
func (t *MinimalDiffReview) WithBoundary(guard BoundaryGuard) *MinimalDiffReview {
	t.guard = guard
	return t
}

func (*MinimalDiffReview) Name() string                 { return "minimal_diff_review" }
func (*MinimalDiffReview) InputSchema() json.RawMessage { return minimalDiffReviewSchema }
func (*MinimalDiffReview) Description() string {
	return "Advisory review of a diff for signs of over-building — findings NEVER block a write, they are hints. " +
		"Deterministic, no LLM: it flags a single-use abstraction, a thin forwarding wrapper, a new dependency with a well-known stdlib equivalent, a possible duplicate helper, and a logic change with no accompanying test change. " +
		"Evidence is asymmetric: a check stays silent unless it can point at concrete evidence and (where defensible) a smaller alternative, so silence is NOT proof a change is minimal. " +
		"Findings are labelled by confidence: high = proven from the diff text; low = leans on the topology index, which is approximate (its call graph is intra-file — unlike find_references' exact cross-file lookup) and may be a few edits stale. " +
		"Reviews the working-tree diff vs base_ref (default HEAD); pass `files` to scope it to your change in a shared worktree. Degrades cleanly outside a git repository."
}

type minimalDiffReviewArgs struct {
	BaseRef            string   `json:"base_ref"`
	Files              []string `json:"files"`
	Mode               string   `json:"mode"`
	MaxFindings        int      `json:"max_findings"`
	IncludeSuggestions *bool    `json:"include_suggestions"`
}

func (t *MinimalDiffReview) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseMinimalDiffReviewArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	ws := ""
	if t.ws != nil {
		ws = t.ws()
	}
	if ws == "" {
		return "", UnattachedWorkspaceError{Path: "minimal_diff_review"}
	}
	for _, f := range a.Files {
		if err := t.guard.check(resolvePath(f, t.ws)); err != nil {
			return "", fmt.Errorf("minimal_diff_review: %w", err)
		}
	}
	repoRoot, gitErr := findGitRoot(ws)
	if gitErr != nil {
		return "minimal_diff_review: not a git repository — this tool reviews a git diff, so it has nothing to analyse here.\n" +
			"Run it inside a git working tree (init one with git_init if you intend to track this project).", nil
	}
	diffText, err := t.gitDiff(ctx, repoRoot, ws, a)
	if err != nil {
		return "", err
	}
	report := t.review(ctx, diffText, a)
	return formatReview(report, a, len(diffText) >= maxReviewDiffBytes), nil
}

func parseMinimalDiffReviewArgs(raw json.RawMessage) (minimalDiffReviewArgs, error) {
	var a minimalDiffReviewArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return a, fmt.Errorf("minimal_diff_review: invalid arguments: %w", err)
		}
	}
	if a.Mode == "" {
		a.Mode = "changed"
	}
	if a.BaseRef == "" {
		a.BaseRef = "HEAD"
	}
	return a, nil
}

func (a *minimalDiffReviewArgs) validate() error {
	if a.Mode != "changed" && a.Mode != "staged" {
		return fmt.Errorf("minimal_diff_review: mode must be \"changed\" or \"staged\", got %q", a.Mode)
	}
	// base_ref sits before the "--" pathspec separator in the git argv, so a
	// dash-leading value would be parsed as a git option (e.g. --output writes
	// a file, --ext-diff runs a command) rather than a revision.
	if strings.HasPrefix(a.BaseRef, "-") {
		return fmt.Errorf("minimal_diff_review: base_ref must be a revision, not an option (got %q)", a.BaseRef)
	}
	return nil
}

// gitDiff runs the git diff for the requested scope and returns its text,
// bounded to maxReviewDiffBytes. Pathspecs are limited to the requested files,
// or to the workspace root when none are given, so the review never reaches
// changes outside the pinned workspace even when the repo root is an ancestor.
func (t *MinimalDiffReview) gitDiff(ctx context.Context, repoRoot, ws string, a minimalDiffReviewArgs) (string, error) {
	argv := []string{"--no-pager", "diff", "--no-color", "-U3"}
	if a.Mode == "staged" {
		argv = append(argv, "--cached")
	}
	argv = append(argv, a.BaseRef, "--")
	if len(a.Files) > 0 {
		for _, f := range a.Files {
			argv = append(argv, resolvePath(f, t.ws))
		}
	} else {
		argv = append(argv, ws)
	}
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			msg := strings.TrimSpace(string(exitErr.Stderr))
			if msg == "" {
				msg = err.Error()
			}
			return "", fmt.Errorf("minimal_diff_review: git diff: %s", msg)
		}
		return "", fmt.Errorf("minimal_diff_review: running git diff: %w", err)
	}
	text := string(out)
	// `git diff` omits untracked files, but a brand-new uncommitted file is
	// exactly where over-building shows up, so synthesise a new-file diff for
	// each untracked file in scope (changed mode only — untracked files are by
	// definition not staged).
	if a.Mode == "changed" && len(text) < maxReviewDiffBytes {
		text += t.untrackedDiffs(ctx, repoRoot, ws, a, maxReviewDiffBytes-len(text))
	}
	if len(text) > maxReviewDiffBytes {
		text = text[:maxReviewDiffBytes]
	}
	return text, nil
}

// untrackedDiffs synthesises a new-file unified diff for every untracked,
// non-ignored file in scope, up to budget bytes. It reads each file directly
// (read-only — it never touches the index), so a newly-created but unstaged file
// is still reviewed. Binary and oversized files are skipped.
func (t *MinimalDiffReview) untrackedDiffs(ctx context.Context, repoRoot, ws string, a minimalDiffReviewArgs, budget int) string {
	argv := []string{"-C", repoRoot, "ls-files", "--others", "--exclude-standard", "-z", "--"}
	if len(a.Files) > 0 {
		for _, f := range a.Files {
			argv = append(argv, resolvePath(f, t.ws))
		}
	} else {
		argv = append(argv, ws)
	}
	out, err := exec.CommandContext(ctx, "git", argv...).Output()
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || sb.Len() >= budget {
			break
		}
		syn := synthesiseNewFileDiff(repoRoot, rel)
		if syn == "" || sb.Len()+len(syn) > budget {
			continue
		}
		sb.WriteString(syn)
	}
	return sb.String()
}

// maxUntrackedFileBytes caps a single synthesised untracked file so one large
// generated file cannot dominate the review budget.
const maxUntrackedFileBytes = 128 * 1024

// synthesiseNewFileDiff builds the new-file unified diff ParseUnifiedDiff
// expects for one untracked file, reading its current content. Returns "" for a
// binary, oversized, or unreadable file.
func synthesiseNewFileDiff(repoRoot, rel string) string {
	content, err := os.ReadFile(filepath.Join(repoRoot, rel)) //nolint:gosec // G304: repoRoot is a resolved git root; rel comes from git ls-files, not agent input
	if err != nil || len(content) > maxUntrackedFileBytes || isProbablyBinary(content) {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	var sb strings.Builder
	fmt.Fprintf(&sb, "diff --git a/%s b/%s\n", rel, rel)
	sb.WriteString("new file mode 100644\n")
	fmt.Fprintf(&sb, "--- /dev/null\n+++ b/%s\n", rel)
	fmt.Fprintf(&sb, "@@ -0,0 +1,%d @@\n", len(lines))
	for _, ln := range lines {
		sb.WriteString("+" + ln + "\n")
	}
	return sb.String()
}

// isProbablyBinary reports whether content looks binary (contains a NUL in the
// first scanned window), so binary files are skipped rather than mangled.
func isProbablyBinary(content []byte) bool {
	window := content
	if len(window) > 8000 {
		window = window[:8000]
	}
	for _, b := range window {
		if b == 0 {
			return true
		}
	}
	return false
}

// review parses the diff and runs the analyser with topology-backed deps wired
// to this session's store.
func (t *MinimalDiffReview) review(ctx context.Context, diffText string, a minimalDiffReviewArgs) minchange.Report {
	diff := minchange.ParseUnifiedDiff(diffText)
	include := true
	if a.IncludeSuggestions != nil {
		include = *a.IncludeSuggestions
	}
	deps := minchange.Deps{}
	if t.storeFn != nil && t.storeFn() != nil {
		deps.CallerCount = t.callerCount
		deps.SimilarSymbols = t.similarSymbols
	}
	return minchange.Analyze(ctx, diff, deps, minchange.Options{
		MaxFindings:        a.MaxFindings,
		IncludeSuggestions: include,
		ScopedToFiles:      len(a.Files) > 0,
		DiffTruncated:      len(diffText) >= maxReviewDiffBytes,
	})
}

// callerCount returns an approximate intra-file caller count for name via the
// topology call graph, plus the single call site when the count is one. found is
// false when the symbol is not in the index.
func (t *MinimalDiffReview) callerCount(ctx context.Context, name string) (int, minchange.SymbolRef, bool) {
	store := t.storeFn()
	if store == nil {
		return 0, minchange.SymbolRef{}, false
	}
	res, err := store.Impact(ctx, name, topology.ImpactOpts{
		Depth:     1,
		MaxNodes:  64,
		MaxBytes:  100000,
		EdgeKinds: []string{"calls"},
	})
	if err != nil || res == nil || res.DependedOnBy == nil {
		return 0, minchange.SymbolRef{}, false
	}
	callers := callableNodes(res.DependedOnBy.Nodes)
	count := len(callers)
	if count == 1 {
		return 1, nodeToSymbolRef(callers[0]), true
	}
	return count, minchange.SymbolRef{}, true
}

// callableNodes keeps only nodes that can be a call site (functions, methods,
// tests), filtering out non-call inward neighbours.
func callableNodes(nodes []topology.Node) []topology.Node {
	out := make([]topology.Node, 0, len(nodes))
	for _, n := range nodes {
		switch n.Kind {
		case topology.KindFunction, topology.KindMethod, topology.KindTest:
			out = append(out, n)
		}
	}
	return out
}

// similarSymbols finds indexed free functions in other files whose tokenised
// name is within an edit distance of one of name's tokenised form — a possible
// duplicate. Single-token names are ignored (too generic to flag usefully).
func (t *MinimalDiffReview) similarSymbols(ctx context.Context, name, excludeFile string) []minchange.SymbolRef {
	store := t.storeFn()
	if store == nil {
		return nil
	}
	tok := tokenise.SplitIdentifier(name)
	if len(strings.Fields(tok)) < 2 {
		return nil
	}
	results, err := store.Search(ctx, tok, topology.SearchOpts{Limit: 20})
	if err != nil {
		return nil
	}
	var out []minchange.SymbolRef
	for _, r := range results {
		n := r.Node
		if n.Kind != topology.KindFunction || samePath(n.Path, excludeFile) {
			continue
		}
		if levenshtein(tok, tokenise.SplitIdentifier(n.Name)) > 1 {
			continue
		}
		out = append(out, nodeToSymbolRef(n))
		if len(out) >= 2 {
			break
		}
	}
	return out
}

// samePath reports whether two workspace-relative-ish paths refer to the same
// file, tolerating one being a suffix of the other (absolute vs relative).
func samePath(a, b string) bool {
	a, b = strings.TrimPrefix(a, "./"), strings.TrimPrefix(b, "./")
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}

func nodeToSymbolRef(n topology.Node) minchange.SymbolRef {
	return minchange.SymbolRef{Name: n.Name, Path: n.Path, Line: n.StartLine, Kind: string(n.Kind)}
}

// formatReview renders the report as a bounded, human-readable advisory block.
func formatReview(r minchange.Report, a minimalDiffReviewArgs, diffCapped bool) string {
	var sb strings.Builder
	sb.WriteString("minimal_diff_review — advisory (findings never block writes)\n")
	fmt.Fprintf(&sb, "scope: mode=%s, base_ref=%s, %d file(s) reviewed\n", a.Mode, a.BaseRef, r.FilesReviewed)
	sb.WriteString("source: git diff + topology index. Evidence is asymmetric — silence is not proof a change is minimal.\n")
	if diffCapped {
		fmt.Fprintf(&sb, "note: the diff exceeded %d bytes and was truncated before analysis.\n", maxReviewDiffBytes)
	}
	sb.WriteString("\n")

	if len(r.Findings) == 0 {
		sb.WriteString("findings: none — no over-building signals in the reviewed scope.\n")
	} else {
		fmt.Fprintf(&sb, "findings (%d):\n", len(r.Findings))
		for _, f := range r.Findings {
			writeFinding(&sb, f)
		}
	}
	if r.Truncated {
		sb.WriteString("\n[truncated: max_findings reached — raise max_findings to see the rest]\n")
	}

	if len(r.NotChecked) > 0 {
		sb.WriteString("\nnot analysed / limits:\n")
		for _, n := range r.NotChecked {
			fmt.Fprintf(&sb, "  - %s\n", n)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// writeFinding renders one finding.
func writeFinding(sb *strings.Builder, f minchange.Finding) {
	loc := ""
	if f.File != "" {
		loc = " — " + f.File
		if f.Line > 0 {
			loc += fmt.Sprintf(":%d", f.Line)
		}
	}
	fmt.Fprintf(sb, "  [%s] %s (confidence %s)%s\n",
		strings.ToUpper(string(f.Severity)), f.Kind, f.Confidence, loc)
	fmt.Fprintf(sb, "    why: %s\n", f.Rationale)
	fmt.Fprintf(sb, "    evidence: %s\n", f.Evidence)
	if f.Alternative != "" {
		fmt.Fprintf(sb, "    smaller alternative: %s\n", f.Alternative)
	}
}
