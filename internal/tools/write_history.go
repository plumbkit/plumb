package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
      "description": "Filter by session ID, or 'self' for the caller's own writes: this session, and on a connection shared by several agents only this agent's (list mode)."
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
      "description": "Maximum changes to list (default 20) or to show for a call_id (default all), max 100."
    },
    "offset": {
      "type": "integer",
      "description": "Pagination offset (default 0)."
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
	maxTotalShowBytes   = 128 * 1024 // 128 KiB aggregate budget for call diff output
)

// WriteHistory enables read-only inspection of recorded write-diff history.
// Concurrency: Execute is safe for concurrent use.
type WriteHistory struct {
	ws         WorkspaceFn
	selfSessID func() string
	selfAgent  func(ctx context.Context) string
	sensitive  func(ctx context.Context, path, from string) bool
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

// WithSelfAgent wires the caller's logical agent exactly as history attributes
// it (empty unless the connection is shared by several agents), so
// session:"self" on a shared connection means this agent, not every agent on
// it. Nil-safe.
func (t *WriteHistory) WithSelfAgent(fn func(ctx context.Context) string) *WriteHistory {
	t.selfAgent = fn
	return t
}

// WithSensitive wires the connection's sensitive-change decision (the one the
// write tools' response diffs and the recorder use). A row recorded as a diff
// is re-asked at read time: the globs may have grown since it was written, and
// before 0.22.0 a project config could empty the global list, so history.db can
// hold a diff of a file that is sensitive now. This tool's output is a
// transcript, which leaves the machine; history.db does not. Nil-safe.
func (t *WriteHistory) WithSensitive(fn func(ctx context.Context, path, from string) bool) *WriteHistory {
	t.sensitive = fn
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
	if a.Limit < 0 {
		return errors.New("limit must be non-negative")
	}
	if a.Mode == "list" && a.Limit == 0 {
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
	Mode      string
	Workspace string
	Entries   []history.Entry
	// Diffs holds one diff per entry (seq mode). DiffFor, when set, fetches the
	// i-th entry's diff instead (call_id mode), so only the diffs the response
	// budget will actually print are read and decompressed.
	Diffs        []string
	DiffFor      func(i int) (string, error)
	CallID       string
	CallSummary  stats.CallSummary
	HaveCall     bool
	Offset       int
	Limit        int
	Total        int // call_id mode: the call's changes in this workspace
	MaxDiffBytes int
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
	out, err := t.run(ctx, args)
	if err != nil {
		return "", fmt.Errorf("write_history: %w", err)
	}
	return out, nil
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

// run formats the result before it returns: in call_id mode the formatter
// fetches diffs through the reader (DiffFor), which must still be open.
func (t *WriteHistory) run(ctx context.Context, a writeHistoryArgs) (string, error) {
	ws, err := t.resolveAndCheckWorkspace(ctx, a.Workspace)
	if err != nil {
		return "", err
	}

	readerFn := t.openReader
	if readerFn == nil {
		readerFn = history.OpenReadOnly
	}
	rdr, err := readerFn()
	if err != nil {
		return "", fmt.Errorf("opening history database: %w", err)
	}
	if rdr == nil {
		return "No write history recorded yet. Make file writes through plumb first.", nil
	}
	defer rdr.Close()

	var res writeHistoryResult
	if a.Mode == "show" {
		res, err = t.runShow(ctx, rdr, ws, a)
	} else {
		res, err = t.runList(ctx, rdr, ws, a)
	}
	if err != nil {
		return "", err
	}
	return formatWriteHistoryResult(res), nil
}

// withholdIfSensitive downgrades a row recorded as a diff to the sensitive
// marker when the change is sensitive under the CURRENT globs (WithSensitive).
// It reports whether the row's diff may be shown.
func (t *WriteHistory) withholdIfSensitive(ctx context.Context, e *history.Entry) bool {
	if e.Content != history.ContentDiff {
		return false
	}
	if t.sensitive != nil && t.sensitive(ctx, e.Path, e.From) {
		e.Content = history.ContentSensitive
		return false
	}
	return true
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

func (t *WriteHistory) resolveFileFilter(ctx context.Context, ws, file string) (string, error) {
	if file == "" {
		return "", nil
	}
	wsFn := t.ws
	if ws != "" {
		wsFn = func(context.Context) string { return ws }
	}
	resolved, err := resolvePath(ctx, file, wsFn, t.contested)
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
	agent := a.Agent
	if a.Session == "self" && agent == "" && t.selfAgent != nil {
		// One connection session carries every agent on a shared connection;
		// the recorder attributes each row to its logical agent, so "self"
		// narrows to that agent's rows too.
		agent = t.selfAgent(ctx)
	}
	filePath, err := t.resolveFileFilter(ctx, ws, a.File)
	if err != nil {
		return writeHistoryResult{}, err
	}

	filter := history.Filter{
		Workspace: ws,
		SessionID: sess,
		Agent:     agent,
		Tool:      a.Tool,
		File:      filePath,
		Limit:     a.Limit,
		Offset:    a.Offset,
	}

	entries, err := rdr.List(filter)
	if err != nil {
		return writeHistoryResult{}, fmt.Errorf("listing history: %w", err)
	}
	for i := range entries {
		t.withholdIfSensitive(ctx, &entries[i])
	}

	return writeHistoryResult{
		Mode:      "list",
		Workspace: ws,
		Entries:   entries,
		Offset:    a.Offset,
		Limit:     a.Limit,
	}, nil
}

func (t *WriteHistory) runShow(ctx context.Context, rdr *history.Reader, ws string, a writeHistoryArgs) (writeHistoryResult, error) {
	if a.Seq != 0 {
		return t.runShowSeq(ctx, rdr, ws, a.Seq, a.MaxDiffBytes)
	}
	return t.runShowCall(ctx, rdr, ws, a.CallID, a.MaxDiffBytes, a.Limit, a.Offset)
}

func (t *WriteHistory) runShowSeq(ctx context.Context, rdr *history.Reader, ws string, seq int64, maxDiffBytes int) (writeHistoryResult, error) {
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
	if !t.withholdIfSensitive(ctx, &e) {
		diff = ""
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

func (t *WriteHistory) runShowCall(ctx context.Context, rdr *history.Reader, ws string, callID string, maxDiffBytes int, limit int, offset int) (writeHistoryResult, error) {
	entries, err := rdr.CallEntries(callID)
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

	total := len(entries)
	start := min(offset, total)
	end := total
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	entries = entries[start:end]
	for i := range entries {
		t.withholdIfSensitive(ctx, &entries[i])
	}

	summary, haveCall := t.lookupStatsCall(callID)
	return writeHistoryResult{
		Mode:    "show",
		Entries: entries,
		DiffFor: func(i int) (string, error) {
			_, diff, err := rdr.Get(entries[i].Seq)
			return diff, err
		},
		Workspace:    ws,
		CallID:       callID,
		CallSummary:  summary,
		HaveCall:     haveCall,
		Offset:       offset, // == start unless past the end, where it is what was asked
		Total:        total,
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
