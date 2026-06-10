package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/redact"
	"github.com/plumbkit/plumb/internal/stats"
)

// episodicWriteTools are the tools that unconditionally mutate files; a call to
// one counts as a write and contributes its target path(s) to the "touched
// files" list. find_replace is NOT here — it is a write only when it actually
// applies (apply==true); its default dry-run is a read (see isEpisodicWrite).
var episodicWriteTools = map[string]bool{
	"write_file": true, "edit_file": true, "delete_file": true,
	"rename_file": true, "copy_file": true, "transaction_apply": true,
	"rename_symbol": true, "replace_symbol_body": true,
	"insert_before_symbol": true, "insert_after_symbol": true,
	"safe_delete_symbol": true,
}

// episodicSymbolTools are LSP/navigation tools whose `name` argument is a symbol
// worth mentioning in the summary.
var episodicSymbolTools = map[string]bool{
	"find_references": true, "find_symbol": true, "get_definition": true,
	"call_hierarchy": true, "type_hierarchy": true, "read_symbol": true,
	"explain_symbol": true,
}

// generateEpisodicSummary builds and persists a rule-based summary of this
// session's recent activity. Called when the session goes idle. No-op when
// generated summaries are disabled for the workspace or there is no activity.
func (s *connSession) generateEpisodicSummary() {
	ws := s.view().acquiredRoot
	if ws == "" {
		return
	}
	mcfg := s.memoryConfig()
	if !mcfg.GeneratedSummaries {
		return
	}
	ro, err := stats.SharedReadOnly()
	if err != nil || ro == nil {
		return
	}
	calls, err := ro.ToolCallsForSession(ws, s.sessID, time.Now().Add(-24*time.Hour))
	if err != nil || len(calls) == 0 {
		return
	}
	summary, touched, readN, writeN := buildEpisodic(calls)
	if summary == "" {
		return
	}
	summary, _ = redact.Redact(summary)
	summary = clampBytes(summary, episodicBudget(mcfg))
	s.statsStore.RecordEpisodic(stats.Episodic{
		Workspace:    ws,
		SessionID:    s.sessID,
		SessionName:  s.view().sessName,
		GeneratedAt:  time.Now(),
		Summary:      summary,
		TouchedFiles: touched,
		ReadCount:    readN,
		WriteCount:   writeN,
	})
}

func episodicBudget(m config.MemoryConfig) int {
	if m.EpisodicBudgetBytes > 0 {
		return m.EpisodicBudgetBytes
	}
	return 1024
}

// buildEpisodic derives a one-or-two sentence summary plus the touched-file list
// and read/write counts from a session's calls. Pure and deterministic — no LLM.
// Single pass: each call's InputJSON is unmarshalled exactly once.
func buildEpisodic(calls []stats.Call) (summary string, touched []string, readN, writeN int) {
	touched, symbols, readN, writeN := tallyEpisodic(calls)
	if readN == 0 && writeN == 0 && len(touched) == 0 {
		return "", nil, 0, 0
	}
	sort.Strings(touched)
	sort.Strings(symbols)
	return renderEpisodic(touched, symbols, readN, writeN), touched, readN, writeN
}

// tallyEpisodic does the single unmarshal pass: each call's InputJSON is decoded
// exactly once, classified read-vs-write, and mined for touched paths (basenames,
// deduped) and symbol names.
func tallyEpisodic(calls []stats.Call) (touched, symbols []string, readN, writeN int) {
	seenFile := map[string]bool{}
	seenSym := map[string]bool{}
	for _, c := range calls {
		var args map[string]any
		_ = json.Unmarshal([]byte(c.InputJSON), &args) // nil map on error; reads below are nil-safe
		if isEpisodicWrite(c.Tool, args) {
			writeN++
			touched = appendTouched(touched, seenFile, args)
		} else {
			readN++
		}
		if episodicSymbolTools[c.Tool] {
			if name, _ := args["name"].(string); name != "" && !seenSym[name] {
				seenSym[name] = true
				symbols = append(symbols, name)
			}
		}
	}
	return touched, symbols, readN, writeN
}

// appendTouched adds the deduped basenames of a write call's paths to touched.
func appendTouched(touched []string, seen map[string]bool, args map[string]any) []string {
	for _, p := range touchedPaths(args) {
		if rel := filepath.Base(p); rel != "." && rel != "/" && rel != "" && !seen[rel] {
			seen[rel] = true
			touched = append(touched, rel)
		}
	}
	return touched
}

// renderEpisodic formats the human-readable summary sentence from the tallies.
func renderEpisodic(touched, symbols []string, readN, writeN int) string {
	var sb strings.Builder
	sb.WriteString("In your last session you ")
	if len(touched) > 0 {
		fmt.Fprintf(&sb, "modified %s", joinBackticked(touched, 4))
		if writeN > 0 {
			fmt.Fprintf(&sb, " (%d write%s)", writeN, plural(writeN))
		}
	} else {
		fmt.Fprintf(&sb, "ran %d read tool call%s", readN, plural(readN))
	}
	if len(symbols) > 0 {
		fmt.Fprintf(&sb, " and looked at %s", joinBackticked(symbols, 3))
	}
	sb.WriteString(".")
	return sb.String()
}

// isEpisodicWrite reports whether a call mutated files. find_replace is a write
// only when dry_run is explicitly false; its default dry-run is a read.
func isEpisodicWrite(tool string, args map[string]any) bool {
	if tool == "find_replace" {
		dryRun, ok := args["dry_run"].(bool)
		return ok && !dryRun
	}
	return episodicWriteTools[tool]
}

// touchedPaths extracts file paths from a write tool's args: the top-level
// file_path/path/from/to, plus each entry of a transaction_apply's operations[]
// array (whose paths are nested, not top-level).
func touchedPaths(args map[string]any) []string {
	out := stringPaths(args)
	if ops, ok := args["operations"].([]any); ok {
		for _, op := range ops {
			if m, ok := op.(map[string]any); ok {
				out = append(out, stringPaths(m)...)
			}
		}
	}
	return out
}

// stringPaths pulls the file_path/path/from/to string values from one arg map.
func stringPaths(m map[string]any) []string {
	var out []string
	for _, key := range []string{"file_path", "path", "from", "to"} {
		if v, ok := m[key].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

func joinBackticked(items []string, max int) string {
	if len(items) > max {
		items = items[:max]
	}
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = "`" + it + "`"
	}
	return strings.Join(quoted, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// clampBytes truncates s so the result is at most budget BYTES, including the
// trailing ellipsis, cut on a UTF-8 rune boundary. A no-op when s already fits
// or budget <= 0. The config knobs are named *_bytes, so a multi-byte (CJK /
// emoji) summary or hint must be measured in bytes, not rune count.
func clampBytes(s string, budget int) string {
	if budget <= 0 || len(s) <= budget {
		return s
	}
	const ell = "…"
	if budget <= len(ell) {
		return truncateToBytes(s, budget) // no room for content + ellipsis
	}
	return truncateToBytes(s, budget-len(ell)) + ell
}

// truncateToBytes returns the longest prefix of s that is at most n bytes and
// ends on a rune boundary.
func truncateToBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n-- // back up off a continuation byte to the rune boundary
	}
	return s[:n]
}
