package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/stats"
)

var writeHistorySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "mode": {
      "type": "string",
      "enum": ["list", "show"],
      "description": "Operation mode: 'list' (default) queries the change timeline; 'show' inspects diffs and tool-call metadata for a specific seq or call_id."
    },
    "seq": {
      "type": "integer",
      "description": "Sequence number of a change to show (show mode)."
    },
    "call_id": {
      "type": "string",
      "description": "Tool call ID to show all associated changes and call metadata (show mode)."
    },
    "session": {
      "type": "string",
      "description": "Filter by session ID, or 'self' to restrict to the calling session's writes (list mode)."
    },
    "agent": {
      "type": "string",
      "description": "Filter by logical agent name (list mode)."
    },
    "tool": {
      "type": "string",
      "description": "Filter by tool name (e.g. 'edit_file', 'write_file') (list mode)."
    },
    "file": {
      "type": "string",
      "description": "Filter by file path (workspace-relative or absolute) (list mode)."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum changes to list (default 20, max 100) (list mode)."
    },
    "offset": {
      "type": "integer",
      "description": "Pagination offset (default 0) (list mode)."
    },
    "max_diff_bytes": {
      "type": "integer",
      "description": "Cap on diff bytes returned per change (default 32768 [32 KiB], max 131072 [128 KiB]) (show mode)."
    },
    "workspace": {
      "type": "string",
      "description": "Workspace root path. Defaults to the pinned workspace."
    }
  },
  "additionalProperties": false
}`)

const (
	defaultHistoryLimit = 20
	maxHistoryLimit     = 100
	defaultMaxDiffBytes = 32 * 1024  // 32 KiB
	maxAllowedDiffBytes = 128 * 1024 // 128 KiB
	diffTruncatedNotice = "\n… (diff truncated; use max_diff_bytes to expand)"
)

// WriteHistory enables read-only inspection of recorded write-diff history.
// Concurrency: Execute is safe for concurrent use.
type WriteHistory struct {
	ws         WorkspaceFn
	selfSessID func() string
	guard      BoundaryGuard
	contested  ContestedFn
	openReader func() (*history.Reader, error)
	openStats  func() (*stats.DB, error)
}

// NewWriteHistory creates a write_history tool instance.
func NewWriteHistory() *WriteHistory {
	return &WriteHistory{
		openReader: history.OpenReadOnly,
		openStats:  stats.OpenReadOnly,
	}
}

// WithWorkspace wires the pinned-workspace accessor. Nil-safe.
func (t *WriteHistory) WithWorkspace(ws WorkspaceFn) *WriteHistory {
	t.ws = ws
	return t
}

// WithSelfSession wires the current session ID accessor. Nil-safe.
func (t *WriteHistory) WithSelfSession(fn func() string) *WriteHistory {
	t.selfSessID = fn
	return t
}

// WithBoundary wires the read boundary guard. Nil-safe.
func (t *WriteHistory) WithBoundary(guard BoundaryGuard) *WriteHistory {
	t.guard = guard
	return t
}

// WithContested wires the contested pin detector. Nil-safe.
func (t *WriteHistory) WithContested(fn ContestedFn) *WriteHistory {
	t.contested = fn
	return t
}

func (t *WriteHistory) withOpeners(openReader func() (*history.Reader, error), openStats func() (*stats.DB, error)) *WriteHistory {
	t.openReader = openReader
	t.openStats = openStats
	return t
}

func (t *WriteHistory) Name() string                 { return "write_history" }
func (t *WriteHistory) InputSchema() json.RawMessage { return writeHistorySchema }
func (t *WriteHistory) Description() string {
	return "Reviews file write history recorded across plumb write tools. Supports listing recent changes with session, agent, tool, and file filters, and showing full diffs and tool-call metadata by sequence number or call ID."
}

type writeHistoryArgs struct {
	Mode         string `json:"mode"`
	Seq          int64  `json:"seq"`
	CallID       string `json:"call_id"`
	Session      string `json:"session"`
	Agent        string `json:"agent"`
	Tool         string `json:"tool"`
	File         string `json:"file"`
	Limit        int    `json:"limit"`
	Offset       int    `json:"offset"`
	MaxDiffBytes int    `json:"max_diff_bytes"`
	Workspace    string `json:"workspace"`
}

func parseWriteHistoryArgs(raw json.RawMessage) (writeHistoryArgs, error) {
	var a writeHistoryArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return a, fmt.Errorf("write_history: unmarshal arguments: %w", err)
		}
	}
	return a, nil
}

func (a *writeHistoryArgs) validate() error {
	if a.Seq != 0 || a.CallID != "" {
		if a.Mode == "" {
			a.Mode = "show"
		}
	}
	if a.Mode == "" {
		a.Mode = "list"
	}
	if a.Mode != "list" && a.Mode != "show" {
		return errors.New("mode must be 'list' or 'show'")
	}
	if a.Mode == "show" {
		if err := a.validateShow(); err != nil {
			return err
		}
	}
	return a.validateLimits()
}

func (a *writeHistoryArgs) validateShow() error {
	if a.Seq == 0 && a.CallID == "" {
		return errors.New("show mode requires 'seq' or 'call_id'")
	}
	if a.Seq != 0 && a.CallID != "" {
		return errors.New("cannot specify both 'seq' and 'call_id'")
	}
	return nil
}

func (a *writeHistoryArgs) validateLimits() error {
	if a.Limit <= 0 {
		a.Limit = defaultHistoryLimit
	}
	if a.Limit > maxHistoryLimit {
		a.Limit = maxHistoryLimit
	}
	if a.Offset < 0 {
		return errors.New("offset must be non-negative")
	}
	if a.MaxDiffBytes <= 0 {
		a.MaxDiffBytes = defaultMaxDiffBytes
	}
	if a.MaxDiffBytes > maxAllowedDiffBytes {
		a.MaxDiffBytes = maxAllowedDiffBytes
	}
	return nil
}

type writeHistoryResult struct {
	Mode         string
	Workspace    string
	Entries      []history.Entry
	Diffs        []string
	CallSummary  stats.CallSummary
	HaveCall     bool
	Offset       int
	MaxDiffBytes int
	Empty        bool
	EmptyMsg     string
}

// Execute performs the write_history request according to the thin-orchestrator pattern.
func (t *WriteHistory) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := parseWriteHistoryArgs(raw)
	if err != nil {
		return "", err
	}
	if err := args.validate(); err != nil {
		return "", fmt.Errorf("write_history: %w", err)
	}
	res, err := t.run(ctx, args)
	if err != nil {
		return "", fmt.Errorf("write_history: %w", err)
	}
	return formatWriteHistoryResult(res), nil
}

func (t *WriteHistory) resolveAndCheckWorkspace(ctx context.Context, explicit string) (string, error) {
	ws := resolveWorkspace(ctx, explicit, t.ws)
	if ws == "" {
		return "", noWorkspaceError()
	}
	if err := t.guard.check(ctx, ws); err != nil {
		return "", fmt.Errorf("workspace boundary: %w", err)
	}
	return paths.Canonical(ws), nil
}

func (t *WriteHistory) run(ctx context.Context, a writeHistoryArgs) (writeHistoryResult, error) {
	ws, err := t.resolveAndCheckWorkspace(ctx, a.Workspace)
	if err != nil {
		return writeHistoryResult{}, err
	}

	readerFn := t.openReader
	if readerFn == nil {
		readerFn = history.OpenReadOnly
	}
	rdr, err := readerFn()
	if err != nil {
		return writeHistoryResult{}, fmt.Errorf("opening history database: %w", err)
	}
	if rdr == nil {
		return writeHistoryResult{
			Mode:      a.Mode,
			Workspace: ws,
			Empty:     true,
			EmptyMsg:  "No write history recorded yet. Make file writes through plumb first.",
		}, nil
	}
	defer rdr.Close()

	if a.Mode == "show" {
		return t.runShow(rdr, ws, a)
	}
	return t.runList(ctx, rdr, ws, a)
}

func (t *WriteHistory) resolveSessionFilter(filterSession string) (string, error) {
	if filterSession == "self" {
		if t.selfSessID == nil || t.selfSessID() == "" {
			return "", errors.New("cannot filter by session 'self': calling session ID is unknown")
		}
		return t.selfSessID(), nil
	}
	return filterSession, nil
}

func (t *WriteHistory) resolveFileFilter(ctx context.Context, file string) (string, error) {
	if file == "" {
		return "", nil
	}
	resolved, err := resolvePath(ctx, file, t.ws, t.contested)
	if err != nil {
		return "", fmt.Errorf("resolving file: %w", err)
	}
	if err := t.guard.check(ctx, resolved); err != nil {
		return "", fmt.Errorf("file boundary: %w", err)
	}
	return paths.Canonical(resolved), nil
}

func (t *WriteHistory) runList(ctx context.Context, rdr *history.Reader, ws string, a writeHistoryArgs) (writeHistoryResult, error) {
	sess, err := t.resolveSessionFilter(a.Session)
	if err != nil {
		return writeHistoryResult{}, err
	}
	filePath, err := t.resolveFileFilter(ctx, a.File)
	if err != nil {
		return writeHistoryResult{}, err
	}

	filter := history.Filter{
		Workspace: ws,
		SessionID: sess,
		Agent:     a.Agent,
		Tool:      a.Tool,
		File:      filePath,
		Limit:     a.Limit,
		Offset:    a.Offset,
	}

	entries, err := rdr.List(filter)
	if err != nil {
		return writeHistoryResult{}, fmt.Errorf("listing history: %w", err)
	}

	return writeHistoryResult{
		Mode:      "list",
		Workspace: ws,
		Entries:   entries,
		Offset:    a.Offset,
	}, nil
}

func (t *WriteHistory) runShow(rdr *history.Reader, ws string, a writeHistoryArgs) (writeHistoryResult, error) {
	if a.Seq != 0 {
		return t.runShowSeq(rdr, ws, a.Seq, a.MaxDiffBytes)
	}
	return t.runShowCall(rdr, ws, a.CallID, a.MaxDiffBytes)
}

func (t *WriteHistory) runShowSeq(rdr *history.Reader, ws string, seq int64, maxDiffBytes int) (writeHistoryResult, error) {
	e, diff, err := rdr.Get(seq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return writeHistoryResult{}, fmt.Errorf("history entry %d not found", seq)
		}
		return writeHistoryResult{}, fmt.Errorf("history entry %d: %w", seq, err)
	}
	if ws != "" && paths.Canonical(e.Workspace) != ws {
		return writeHistoryResult{}, fmt.Errorf("history entry %d belongs to a different workspace", seq)
	}

	summary, haveCall := t.lookupStatsCall(e.CallID)
	return writeHistoryResult{
		Mode:         "show",
		Workspace:    ws,
		Entries:      []history.Entry{e},
		Diffs:        []string{diff},
		CallSummary:  summary,
		HaveCall:     haveCall,
		MaxDiffBytes: maxDiffBytes,
	}, nil
}

func (t *WriteHistory) runShowCall(rdr *history.Reader, ws string, callID string, maxDiffBytes int) (writeHistoryResult, error) {
	entries, diffs, err := rdr.ByCall(callID)
	if err != nil {
		return writeHistoryResult{}, fmt.Errorf("history by call %s: %w", callID, err)
	}
	if len(entries) == 0 {
		return writeHistoryResult{}, fmt.Errorf("no history entries found for call %s", callID)
	}
	for _, e := range entries {
		if ws != "" && paths.Canonical(e.Workspace) != ws {
			return writeHistoryResult{}, fmt.Errorf("history entries for call %s belong to a different workspace", callID)
		}
	}

	summary, haveCall := t.lookupStatsCall(callID)
	return writeHistoryResult{
		Mode:         "show",
		Workspace:    ws,
		Entries:      entries,
		Diffs:        diffs,
		CallSummary:  summary,
		HaveCall:     haveCall,
		MaxDiffBytes: maxDiffBytes,
	}, nil
}

func (t *WriteHistory) lookupStatsCall(callID string) (stats.CallSummary, bool) {
	if callID == "" {
		return stats.CallSummary{}, false
	}
	statsFn := t.openStats
	if statsFn == nil {
		statsFn = stats.OpenReadOnly
	}
	sdb, err := statsFn()
	if err != nil || sdb == nil {
		return stats.CallSummary{}, false
	}
	defer sdb.Close()
	s, ok, err := sdb.CallByID(callID)
	if err != nil || !ok {
		return stats.CallSummary{}, false
	}
	return s, true
}

func formatWriteHistoryResult(res writeHistoryResult) string {
	if res.Empty {
		return res.EmptyMsg
	}
	if res.Mode == "show" {
		return formatShowResult(res)
	}
	return formatListResult(res)
}

func formatListResult(res writeHistoryResult) string {
	if len(res.Entries) == 0 {
		return fmt.Sprintf("No write history found matching query in workspace %s.", res.Workspace)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Write history (%d change(s), offset %d):\n\n", len(res.Entries), res.Offset)
	for _, e := range res.Entries {
		sb.WriteString(formatHistoryLine(e))
		sb.WriteByte('\n')
		if e.GapBefore {
			gap := "  ⋯ unrecorded change (outside plumb's write tools)"
			if e.GapDropped {
				gap += "; history dropped rows in this interval"
			}
			sb.WriteString(gap)
			sb.WriteByte('\n')
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatHistoryLine(e history.Entry) string {
	ts := e.At.Local().Format("2006-01-02 15:04:05.000")
	op := string(e.Op)
	if e.Kind == history.KindDir {
		op += " dir"
	}
	sess := e.SessionName
	if sess == "" {
		if len(e.SessionID) > 8 {
			sess = e.SessionID[:8]
		} else {
			sess = e.SessionID
		}
	}
	if sess == "" {
		sess = "-"
	}
	if e.LogicalAgent != "" {
		sess += "/" + e.LogicalAgent
	}
	p := e.Path
	if e.From != "" {
		p = e.From + " → " + e.Path
	}
	diffStats := fmt.Sprintf("+%d -%d", e.Added, e.Removed)
	content := ""
	if e.Content != history.ContentDiff && e.Content != "" {
		content = fmt.Sprintf("  [%s]", e.Content)
	}
	return fmt.Sprintf("#%d  %s  %-10s  %-17s  %-15s  %s  %s%s",
		e.Seq, ts, op, e.Tool, sess, p, diffStats, content)
}

func formatShowResult(res writeHistoryResult) string {
	var sb strings.Builder
	if res.HaveCall {
		status := "success"
		if !res.CallSummary.Success {
			status = "failed: " + res.CallSummary.ErrorMsg
		}
		sess := ""
		if res.CallSummary.SessionName != "" {
			sess = "  session " + res.CallSummary.SessionName
		}
		fmt.Fprintf(&sb, "%s (%dms, %s)%s\n\n", res.CallSummary.Tool, res.CallSummary.DurationMs, status, sess)
	} else {
		sb.WriteString("(call metadata unavailable)\n\n")
	}

	for i, e := range res.Entries {
		fromPath := e.Path
		if e.From != "" {
			fromPath = e.From
		}
		fmt.Fprintf(&sb, "#%d %s %s\n", e.Seq, e.Op, e.Path)
		fmt.Fprintf(&sb, "--- a/%s\n", fromPath)
		fmt.Fprintf(&sb, "+++ b/%s\n", e.Path)
		if e.Content == history.ContentDiff {
			diff := ""
			if i < len(res.Diffs) {
				diff = res.Diffs[i]
			}
			formatShowDiff(&sb, diff, res.MaxDiffBytes)
		} else {
			fmt.Fprintf(&sb, "[%s]\n", e.Content)
		}
		if e.GapBefore {
			sb.WriteString("  ⋯ unrecorded change before this edit\n")
		}
		if i < len(res.Entries)-1 {
			sb.WriteByte('\n')
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatShowDiff(sb *strings.Builder, diff string, maxBytes int) {
	if len(diff) > maxBytes {
		diff = diff[:maxBytes] + diffTruncatedNotice
	}
	if diff == "" {
		return
	}
	sb.WriteString(diff)
	if !strings.HasSuffix(diff, "\n") {
		sb.WriteByte('\n')
	}
}
