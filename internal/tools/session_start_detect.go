package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// workspaceScale returns a human-readable file-count summary for the workspace
// identity section, e.g. "~342 files (287 Go)".
func workspaceScale(ws, lang string) string {
	exts, label := langFileProfile(lang)
	total, langCount := countWorkspaceFiles(ws, exts)
	if total == 0 {
		return ""
	}
	if label != "" && langCount > 0 {
		return fmt.Sprintf("~%d files (%d %s)", total, langCount, label)
	}
	return fmt.Sprintf("~%d files", total)
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

// countWorkspaceFiles walks ws and returns the total file count and the count
// of files matching the given extensions. Skips .git, node_modules, vendor,
// dist, build, and hidden directories.
func countWorkspaceFiles(ws string, exts []string) (total, langCount int) {
	extSet := make(map[string]bool, len(exts))
	for _, e := range exts {
		extSet[e] = true
	}
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	}
	_ = filepath.Walk(ws, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			n := info.Name()
			if skipDirs[n] || (strings.HasPrefix(n, ".") && path != ws) {
				return filepath.SkipDir
			}
			return nil
		}
		total++
		if extSet[filepath.Ext(path)] {
			langCount++
		}
		return nil
	})
	return total, langCount
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

// gitBranch returns the current branch name, or "" if not a git repo / git
// is unavailable. Best-effort with a short timeout.
func gitBranch(ws string) string {
	cmd := exec.Command("git", "-C", ws, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRecentCommits returns up to n recent commit subjects in "shortsha subject"
// form. Best-effort; returns nil on any error.
func gitRecentCommits(ws string, n int) []string {
	cmd := exec.Command("git", "-C", ws, "log", fmt.Sprintf("-%d", n), "--pretty=format:%h %s")
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
	cmd := exec.Command("git", "-C", ws, "diff", "--stat", "HEAD")
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

// recentlyModifiedFiles returns up to n workspace-relative file paths sorted
// by mtime (newest first). Skips hidden directories, .git, node_modules,
// vendor — the usual noise.
func recentlyModifiedFiles(ws string, n int) []string {
	type fileEntry struct {
		path string
		mod  int64
	}
	var entries []fileEntry
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, ".idea": true,
	}
	_ = filepath.Walk(ws, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if skipDirs[name] || (strings.HasPrefix(name, ".") && path != ws) {
				return filepath.SkipDir
			}
			return nil
		}
		entries = append(entries, fileEntry{path: path, mod: info.ModTime().UnixNano()})
		return nil
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
