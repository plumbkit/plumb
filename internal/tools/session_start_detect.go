package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/ignore"
)

// workspaceScale returns a human-readable file-count summary for the workspace
// identity section, e.g. "~342 files (287 Go)".
func workspaceScale(ws, lang string) string {
	exts, label := langFileProfile(lang)
	total, langCount, truncated := countWorkspaceFiles(ws, exts)
	if total == 0 {
		return ""
	}
	return renderScale(total, langCount, label, truncated)
}

// renderScale formats the Scale line. Split out of workspaceScale so the
// truncation marker can be tested without materialising scaleWalkMaxFiles
// files on disk.
//
// A capped walk renders "~50000+ files", never the bare cap: the number is the
// one fact this line exists to give, and silently reporting a ceiling as if it
// were a count is worse than reporting nothing — an agent reads "50000 files"
// as a measured workspace size and sizes its next move to it.
func renderScale(total, langCount int, label string, truncated bool) string {
	count := fmt.Sprintf("~%d", total)
	if truncated {
		count += "+"
	}
	if label != "" && langCount > 0 {
		return fmt.Sprintf("%s files (%d %s)", count, langCount, label)
	}
	return count + " files"
}

// langFileProfile returns the primary source-file extensions and a short
// display label for a detected language name.
func langFileProfile(lang string) (exts []string, label string) {
	switch lang {
	case "Go":
		return []string{".go"}, "Go"
	case "Python":
		return []string{".py"}, "Python"
	case "TypeScript":
		return []string{".ts", ".tsx"}, "TypeScript"
	case "JavaScript":
		return []string{".js", ".mjs", ".cjs", ".jsx"}, "JavaScript"
	case "JavaScript/TypeScript":
		return []string{".ts", ".js", ".tsx", ".jsx"}, "JS/TS"
	case "Rust":
		return []string{".rs"}, "Rust"
	case "Swift":
		return []string{".swift"}, "Swift"
	case "Zig":
		return []string{".zig"}, "Zig"
	case "Kotlin":
		return []string{".kt", ".kts"}, "Kotlin"
	case "Java (Maven)":
		return []string{".java"}, "Java"
	case "Java/Kotlin (Gradle)":
		return []string{".java", ".kt"}, "Java/Kotlin"
	case "C/C++ (CMake)":
		return []string{".c", ".cpp", ".cc", ".h", ".hpp"}, "C/C++"
	case "Elixir":
		return []string{".ex", ".exs"}, "Elixir"
	case "Ruby":
		return []string{".rb"}, "Ruby"
	default:
		return nil, ""
	}
}

// scaleWalkMaxFiles caps how many files a session_start census walk visits.
//
// Both walks below run on every session_start, before the agent has said
// anything, and neither had a bound: a monorepo or a home-adjacent root made
// the cheapest line in the packet the most expensive call in the session. The
// cap trades an exact count on a huge tree for a bounded one, and the Scale
// line says which it gave (see renderScale).
const scaleWalkMaxFiles = 50000

// errCensusCapped stops a census walk at its file limit. Returned through
// filepath.WalkDir, never to a caller.
var errCensusCapped = errors.New("session_start: census walk capped")

// censusWalk walks ws top-down and calls visit for every file that survives
// three filters, reporting whether it stopped at maxFiles.
//
// The filters are ADDITIVE and applied in this order:
//
//  1. skipDirs — the hardcoded floor. A directory named here is pruned whether
//     or not any ignore file mentions it.
//  2. the dot-directory rule — same floor, same reason.
//  3. .gitignore / .ignore, via ignore.Stack loaded per directory.
//
// gitignore is a supplement to the floor, not a replacement for it: a
// repository that tracks its vendor/ tree (many do) must still not have it
// counted as workspace scale, and a workspace with no ignore file at all must
// behave exactly as it did before this walk learned to read them.
//
// The walk PRUNES excluded directories with fs.SkipDir rather than filtering
// their files, which is what ignore.Stack.IsIgnored is contracted for — see the
// CONTRACT note on that method. Filtering instead of pruning would both cost
// the descent and get the answer wrong, because a file under an excluded
// directory is usually matched by no rule of its own.
//
// This deliberately does not reuse tools.walk: that walker hides every dotfile
// (the census counts them, they are workspace scale) and has no notion of the
// hardcoded floor above.
func censusWalk(ws string, skipDirs map[string]bool, maxFiles int, visit func(path string, d fs.DirEntry)) (truncated bool) {
	root := filepath.Clean(ws)
	// One stack per directory, keyed by absolute path. WalkDir is depth-first
	// and pre-order, so a directory's parent is always already loaded, and a
	// pruned directory never has children to look it up.
	stacks := make(map[string]ignore.Stack)
	seen := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if path == root {
				var st ignore.Stack
				stacks[root] = st.Load(root)
				return nil
			}
			name := d.Name()
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			parent := stacks[filepath.Dir(path)]
			if parent.IsIgnored(path, true) {
				return fs.SkipDir
			}
			stacks[path] = parent.Load(path)
			return nil
		}
		if stacks[filepath.Dir(path)].IsIgnored(path, false) {
			return nil
		}
		visit(path, d)
		seen++
		if maxFiles > 0 && seen >= maxFiles {
			return errCensusCapped
		}
		return nil
	})
	return errors.Is(err, errCensusCapped)
}

// censusSkipDirs is the hardcoded floor for the Scale walk: pruned whatever
// the ignore files say. Dot-directories are pruned by censusWalk itself, so
// .git is listed for the record rather than for effect.
var censusSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
}

// countWorkspaceFiles walks ws and returns the total file count, the count of
// files matching the given extensions, and whether the walk hit
// scaleWalkMaxFiles. Skips the censusSkipDirs floor, hidden directories, and
// anything .gitignore / .ignore excludes.
func countWorkspaceFiles(ws string, exts []string) (total, langCount int, truncated bool) {
	extSet := make(map[string]bool, len(exts))
	for _, e := range exts {
		extSet[e] = true
	}
	truncated = censusWalk(ws, censusSkipDirs, scaleWalkMaxFiles, func(path string, _ fs.DirEntry) {
		total++
		if extSet[filepath.Ext(path)] {
			langCount++
		}
	})
	return total, langCount, truncated
}

// detectLanguageInfo returns a human-readable language label and, when a plumb
// LSP adapter exists for it, that adapter's config key ([lsp.<key>]). Both are
// "" when no root marker matches; the key alone is "" for a recognised language
// plumb has no server for (C/C++, Elixir, Ruby). The key lets session_start
// name the exact knob to enable when a server is expected but not attached.
func detectLanguageInfo(ws string) (label, key string) {
	markers := []struct {
		file  string
		label string
		key   string
	}{
		{"go.mod", "Go", "go"},
		{"tsconfig.json", "TypeScript", "typescript"},
		{"jsconfig.json", "JavaScript", "typescript"},
		{"package.json", "JavaScript/TypeScript", "typescript"},
		{"Cargo.toml", "Rust", "rust"},
		{"pyproject.toml", "Python", "python"},
		{"setup.py", "Python", "python"},
		{"Package.swift", "Swift", "swift"},
		{"build.zig", "Zig", "zig"},
		{"pom.xml", "Java (Maven)", "java"},
		{"settings.gradle.kts", "Kotlin", "kotlin"},
		{"build.gradle.kts", "Kotlin", "kotlin"},
		{"build.gradle", "Java/Kotlin (Gradle)", "java"},
		{"CMakeLists.txt", "C/C++ (CMake)", ""},
		{"mix.exs", "Elixir", ""},
		{"Gemfile", "Ruby", ""},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(ws, m.file)); err == nil {
			return m.label, m.key
		}
	}
	return "", ""
}

// labelForLSPKey maps an [lsp.<key>] config key to the human-readable language
// label used in the session_start identity line. It is the inverse of the key
// column in detectLanguageInfo, for when the attached primary has no root marker
// (e.g. swift forced on an Xcode app) and must still display a name.
func labelForLSPKey(key string) string {
	switch key {
	case "go":
		return "Go"
	case "python":
		return "Python"
	case "typescript":
		return "TypeScript"
	case "javascript":
		return "JavaScript"
	case "rust":
		return "Rust"
	case "swift":
		return "Swift"
	case "zig":
		return "Zig"
	case "kotlin":
		return "Kotlin"
	case "java":
		return "Java"
	case "html":
		return "HTML"
	default:
		return key
	}
}

// joinLanguageLabels maps a set of [lsp.<key>] language keys to their display
// labels and joins them, e.g. ["swift","zig"] → "Swift, Zig". Used for the
// multi-language identity line of a monorepo workspace root.
func joinLanguageLabels(keys []string) string {
	labels := make([]string, 0, len(keys))
	for _, k := range keys {
		labels = append(labels, labelForLSPKey(k))
	}
	return strings.Join(labels, ", ")
}

// gitBranch returns the current branch name, or "" if not a git repo / git
// is unavailable. Best-effort with a short timeout.
func gitBranch(ws string) string {
	cmd := exec.Command("git", gitNoOptionalLocks, "-C", ws, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRecentCommits returns up to n recent commit subjects in "shortsha subject"
// form. Best-effort; returns nil on any error.
func gitRecentCommits(ws string, n int) []string {
	cmd := exec.Command("git", gitNoOptionalLocks, "-C", ws, "log", fmt.Sprintf("-%d", n), "--pretty=format:%h %s")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// gitWorkingTreeSummary returns a compact `git diff --stat HEAD` of the
// uncommitted changes to tracked files (staged + unstaged vs HEAD), capped to
// maxLines lines. Empty when the tree is clean or not a git repo. Lets an agent
// see *what* was already changed at orientation instead of guessing from a bare
// file list (from dogfooding feedback).
func gitWorkingTreeSummary(ws string, maxLines int) string {
	cmd := exec.Command("git", gitNoOptionalLocks, "-C", ws, "diff", "--stat", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > maxLines {
		// Keep the last line (the "N files changed" summary) and the first
		// maxLines-1 file rows.
		summary := lines[len(lines)-1]
		lines = append(lines[:maxLines-1], "… "+summary)
	}
	return strings.Join(lines, "\n")
}

// gitSubmodules returns the workspace-relative paths of the repo's git
// submodules, sorted, or nil when there are none / not a git repo / git is
// unavailable. Read straight from .gitmodules via `git config`, so it reports
// configured submodules even before they are initialised. Best-effort. A
// submodule is a separate repository, so a git command in the superproject
// cannot stage its file contents — session_start surfaces them and the
// repo-targeting rule up front.
func gitSubmodules(ws string) []string {
	gitmodules := filepath.Join(ws, ".gitmodules")
	if _, err := os.Stat(gitmodules); err != nil {
		return nil
	}
	cmd := exec.Command("git", gitNoOptionalLocks, "-C", ws, "config", "--file", gitmodules, "--get-regexp", `^submodule\..*\.path$`)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var paths []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		// Each line is "submodule.<name>.path <path>"; the path is everything
		// after the first space.
		if _, after, ok := strings.Cut(line, " "); ok {
			if p := strings.TrimSpace(after); p != "" {
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// recentSkipDirs is the hardcoded floor for the recent-files walk. Same set as
// censusSkipDirs plus .idea, kept separate so neither list silently inherits a
// change made for the other.
var recentSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, ".idea": true,
}

// recentlyModifiedFiles returns up to n workspace-relative file paths sorted
// by mtime (newest first). Skips hidden directories, the recentSkipDirs floor,
// and anything .gitignore / .ignore excludes — a generated tree's build
// artefacts are the newest files in most workspaces and were, until this walk
// read the ignore files, the whole list.
//
// The walk stops at scaleWalkMaxFiles like the census does, which bounds the
// entry slice as well as the traversal. On a workspace past that cap the five
// newest come from the files visited before it, not from the whole tree: a
// bounded walk that answers approximately beats an unbounded one that may not
// return in time to answer at all.
func recentlyModifiedFiles(ws string, n int) []string {
	type fileEntry struct {
		path string
		mod  int64
	}
	var entries []fileEntry
	_ = censusWalk(ws, recentSkipDirs, scaleWalkMaxFiles, func(path string, d fs.DirEntry) {
		info, err := d.Info()
		if err != nil {
			return
		}
		entries = append(entries, fileEntry{path: path, mod: info.ModTime().UnixNano()})
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod > entries[j].mod })
	if len(entries) > n {
		entries = entries[:n]
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		rel, err := filepath.Rel(ws, e.path)
		if err != nil {
			rel = e.path
		}
		out = append(out, rel)
	}
	return out
}

// isClaudeCode reports whether fn identifies the MCP client as Claude Code.
// Matches "claude-code" exactly or "claude-code/<version>" — a bare HasPrefix
// would falsely match names like "claude-codegen".
func isClaudeCode(fn func() string) bool {
	if fn == nil {
		return false
	}
	n := strings.ToLower(fn())
	return n == "claude-code" || strings.HasPrefix(n, "claude-code/")
}

// isKimiCode reports whether fn identifies the MCP client as Kimi Code.
// Matches "kimi-code" exactly or "kimi-code/<version>", mirroring isClaudeCode:
// a bare HasPrefix on "kimi-code" would falsely match a name like
// "kimi-codegen".
//
// Bare "kimi" is deliberately NOT matched, even though clientcaps.Lookup
// accepts it as an alias. The two predicates answer different questions:
// Lookup is picking the closest capability profile and errs towards a useful
// match, whereas this one gates a block of prose written specifically for the
// Kimi Code CLI (its native edit tools, its mcp.json allowlist). Showing that
// to some other "kimi*" product would be wrong advice, and the cost of not
// matching is only a missing hint.
func isKimiCode(fn func() string) bool {
	if fn == nil {
		return false
	}
	n := strings.ToLower(fn())
	return n == "kimi-code" || strings.HasPrefix(n, "kimi-code/")
}

// clientSideAllowlistCapable reports whether the connected client can filter
// plumb's tools in its OWN MCP config — the `plumb setup <client> --lean`
// allowlist (see clientcaps.Capabilities.ClientSideAllowlist).
//
// Guidance for such a client must name only lean-set tools. plumb cannot see
// whether the allowlist is in force, so anything outside that set may already
// have been removed client-side, before a call could reach plumb. Unlike the
// lean PROFILE, there is no "hidden but callable by name" escape hatch.
//
// Unlike isKimiCode this goes through clientcaps.Lookup, aliases and all: the
// question is a capability of the product, not a licence to show one product's
// prose to another, so a sibling build that resolves to the same entry should
// get the same restraint.
func clientSideAllowlistCapable(fn func() string) bool {
	if fn == nil {
		return false
	}
	return clientcaps.Lookup(fn()).ClientSideAllowlist
}

// isClaudeDesktop reports whether fn identifies the MCP client as Claude Desktop.
// Claude Desktop identifies itself as "claude-ai" (e.g. "claude-ai 0.1.0") over
// MCP, not "claude-desktop" — both are matched so the guidance fires regardless
// of which name a build reports.
func isClaudeDesktop(fn func() string) bool {
	if fn == nil {
		return false
	}
	n := strings.ToLower(fn())
	return n == "claude-ai" || strings.HasPrefix(n, "claude-ai/") ||
		n == "claude-desktop" || strings.HasPrefix(n, "claude-desktop/")
}

// sameDir reports whether paths a and b refer to the same directory on the
// filesystem, using os.SameFile for identity (handles symlinks and macOS
// firmlinks such as /var→/private/var). Falls back to filepath.Clean
// string comparison when either path cannot be stat'd (e.g. not-yet-created
// directory in a test).
func sameDir(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(ia, ib)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
