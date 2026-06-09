package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/redact"
	"github.com/plumbkit/plumb/internal/stats"
)

// episodicWriteTools are the tools that mutate files; a call to one counts as a
// write and contributes its target path to the "touched files" list.
var episodicWriteTools = map[string]bool{
	"write_file": true, "edit_file": true, "delete_file": true,
	"rename_file": true, "copy_file": true, "transaction_apply": true,
	"rename_symbol": true, "replace_symbol_body": true,
	"insert_before_symbol": true, "insert_after_symbol": true,
	"safe_delete_symbol": true, "find_replace": true,
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
	mcfg := s.memoryConfigFor(ws)
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
	summary = clampRunes(summary, episodicBudget(mcfg))
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
func buildEpisodic(calls []stats.Call) (summary string, touched []string, readN, writeN int) {
	touched = episodicTouchedFiles(calls)
	symbols := episodicSymbols(calls)
	readN, writeN = episodicCounts(calls)
	if readN == 0 && writeN == 0 && len(touched) == 0 {
		return "", nil, 0, 0
	}

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
	return sb.String(), touched, readN, writeN
}

func episodicCounts(calls []stats.Call) (readN, writeN int) {
	for _, c := range calls {
		if episodicWriteTools[c.Tool] {
			writeN++
		} else {
			readN++
		}
	}
	return readN, writeN
}

func episodicTouchedFiles(calls []stats.Call) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range calls {
		if !episodicWriteTools[c.Tool] {
			continue
		}
		for _, p := range pathsFromArgs(c.InputJSON) {
			rel := filepath.Base(p)
			if rel == "" || seen[rel] {
				continue
			}
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

func episodicSymbols(calls []stats.Call) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range calls {
		if !episodicSymbolTools[c.Tool] {
			continue
		}
		if name := stringField(c.InputJSON, "name"); name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// pathsFromArgs extracts file path arguments (file_path, path, from, to) from a
// tool's JSON input. Best-effort: malformed JSON yields nothing.
func pathsFromArgs(inputJSON string) []string {
	var m map[string]any
	if json.Unmarshal([]byte(inputJSON), &m) != nil {
		return nil
	}
	var out []string
	for _, key := range []string{"file_path", "path", "from", "to"} {
		if v, ok := m[key].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

func stringField(inputJSON, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(inputJSON), &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
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

func clampRunes(s string, budget int) string {
	if budget <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= budget {
		return s
	}
	return string(r[:budget]) + "…"
}
