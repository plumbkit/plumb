package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/stats"
)

// workspace_sessions_git.go — commit attribution in the workspace_sessions
// recent-writes feed. A successful plumb-mediated commit renders as a full
// attribution line (session, short SHA, subject, repository) instead of the
// bare tool-only line other entries get, so a peer's commit is traceable to
// the session that authored it. The data comes from the stats tool_calls row
// the daemon already records for every git call — input_json carries the
// subcommand and repo, output_text (selected for git rows only, see
// RecentWritesByWorkspace) carries the "<short-sha> <subject>" the tool
// reported — so there is no parallel store.

// writeGitWriteEntry renders a git feed entry: the full attribution line for
// a successful commit, the bare tool-only line otherwise.
func writeGitWriteEntry(sb *strings.Builder, w stats.RecentCall, workspace, age string) {
	sha, subject, ok := gitCommitAttribution(w)
	if !ok {
		fmt.Fprintf(sb, "  %-20s  %-18s  (%s ago)\n", w.SessionName, w.Tool, age)
		return
	}
	fmt.Fprintf(sb, "  %-20s  %-18s  %s %s  [repo: %s]  (%s ago)\n",
		w.SessionName, "git commit", sha, subject, gitCommitRepo(w, workspace), age)
}

// gitCommitAttribution recovers the commit identity recorded for a successful
// plumb-mediated commit. The git tool reports a commit as "<short-sha>
// <subject>" (formatGitCommitResult) on the LAST line of its output (a
// cross-session guard warning, when present, leads the output), and the stats
// feed carries that output for git rows — so the attribution needs no store
// of its own. ok is false for a non-commit subcommand, a failed call, or an
// unrecognised output shape; the caller then renders the bare line.
func gitCommitAttribution(w stats.RecentCall) (sha, subject string, ok bool) {
	if !w.Success || gitInputField(w.InputJSON, "subcommand") != "commit" {
		return "", "", false
	}
	sha, subject, found := strings.Cut(lastNonEmptyLine(w.OutputText), " ")
	if !found || !isCommitSHA(sha) || subject == "" {
		return "", "", false
	}
	return sha, subject, true
}

// gitCommitRepo renders the repository a recorded commit targeted: the call's
// repo argument, relative to the workspace when it sits inside it. An absent
// repo key means the call defaulted to the workspace root, rendered as ".".
func gitCommitRepo(w stats.RecentCall, workspace string) string {
	repo := gitInputField(w.InputJSON, "repo")
	if repo == "" {
		return "."
	}
	if rel, err := filepath.Rel(workspace, repo); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return repo
}

// gitInputField extracts a top-level string field from a recorded git call's
// raw input JSON; "" when the key is absent, not a string, or the JSON does
// not parse.
func gitInputField(raw, key string) string {
	if raw == "" {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	s, _ := jsonStringField(m, key)
	return s
}

// lastNonEmptyLine returns the final line of s with trailing blank lines
// skipped, or "" when s holds nothing but whitespace.
func lastNonEmptyLine(s string) string {
	s = strings.TrimRight(s, " \t\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// isCommitSHA reports whether s looks like a git commit prefix (7–40
// lowercase hex characters) — the shape formatGitCommitResult leads with.
func isCommitSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
