package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/stats"
)

var (
	historyFlagWorkspace string
	historyFlagAll       bool
	historyFlagSession   string
	historyFlagAgent     string
	historyFlagTool      string
	historyFlagFile      string
	historyFlagSince     string
	historyFlagUntil     string
	historyFlagLimit     int
	historyFlagJSON      bool

	historyFlagPruneBefore    string
	historyFlagPruneWorkspace string
	historyFlagPruneYes       bool
	historyFlagPruneVacuum    bool
)

// daemonAlive reports whether the plumb daemon is running. Package-level var as a test seam.
var daemonAlive = func() bool {
	return socketAlive(daemonSocketPath())
}

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Review the diffs of every file write plumb made",
	RunE:  runHistoryList,
}

var historyShowCmd = &cobra.Command{
	Use:   "show <seq|call_id>",
	Short: "Show diffs and call metadata for a sequence number or call ID",
	Args:  cobra.ExactArgs(1),
	RunE:  runHistoryShow,
}

var historyPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Delete write history older than a given date or age",
	RunE:  runHistoryPrune,
}

func init() {
	historyCmd.Flags().StringVar(&historyFlagWorkspace, "workspace", "", "filter to a workspace path (default: current workspace)")
	historyCmd.Flags().BoolVar(&historyFlagAll, "all", false, "list changes across all workspaces")
	historyCmd.Flags().StringVar(&historyFlagSession, "session", "", "filter to a session ID")
	historyCmd.Flags().StringVar(&historyFlagAgent, "agent", "", "filter to a logical agent")
	historyCmd.Flags().StringVar(&historyFlagTool, "tool", "", "filter to a tool name")
	historyCmd.Flags().StringVar(&historyFlagFile, "file", "", "filter to a file path")
	historyCmd.Flags().StringVar(&historyFlagSince, "since", "", "only changes newer than T (RFC 3339, YYYY-MM-DD, 2h, 7d)")
	historyCmd.Flags().StringVar(&historyFlagUntil, "until", "", "only changes older than T")
	historyCmd.Flags().IntVar(&historyFlagLimit, "limit", 50, "maximum number of changes to list")
	historyCmd.Flags().BoolVar(&historyFlagJSON, "json", false, "render as JSON")

	historyShowCmd.Flags().BoolVar(&historyFlagJSON, "json", false, "render as JSON")

	historyPruneCmd.Flags().StringVar(&historyFlagPruneBefore, "before", "", "delete changes older than T (required)")
	historyPruneCmd.Flags().StringVar(&historyFlagPruneWorkspace, "workspace", "", "filter prune to a workspace path")
	historyPruneCmd.Flags().BoolVar(&historyFlagPruneYes, "yes", false, "confirm deletion without prompting")
	historyPruneCmd.Flags().BoolVar(&historyFlagPruneVacuum, "vacuum", false, "reclaim file space (refused while daemon is running)")

	historyCmd.AddCommand(historyShowCmd, historyPruneCmd)
}

// parseWhen parses a date, age (e.g. 7d, 2h), or RFC 3339 timestamp.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty time string")
	}
	if strings.HasSuffix(s, "d") {
		nStr := strings.TrimSuffix(s, "d")
		if n, err := strconv.ParseInt(nStr, 10, 64); err == nil && n > 0 {
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			d = -d
		}
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q: expected RFC 3339, YYYY-MM-DD, or age like 2h or 7d", s)
}

func resolveHistoryWorkspace() (string, error) {
	if historyFlagAll {
		return "", nil
	}
	if historyFlagWorkspace != "" {
		return paths.Canonical(historyFlagWorkspace), nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}
	return resolveCLIWorkspace(".", cfg)
}

func runHistoryList(_ *cobra.Command, _ []string) error {
	ws, err := resolveHistoryWorkspace()
	if err != nil {
		return err
	}

	r, err := history.OpenReadOnly()
	if err != nil {
		return fmt.Errorf("opening history db: %w", err)
	}
	if r == nil {
		if historyFlagJSON {
			fmt.Println("[]")
			return nil
		}
		printCLIDiagnostic(os.Stdout, cliDiagnostic{
			Kind:  "info",
			Title: "No write history recorded yet",
			Body:  "No write history recorded yet. Make some file writes first.",
		})
		return nil
	}
	defer r.Close()

	filter, err := buildHistoryFilter(ws)
	if err != nil {
		return err
	}

	entries, err := r.List(filter)
	if err != nil {
		return fmt.Errorf("listing history: %w", err)
	}

	if historyFlagJSON {
		return renderListJSON(entries)
	}

	renderListText(entries)
	return nil
}

func buildHistoryFilter(ws string) (history.Filter, error) {
	filter := history.Filter{
		Workspace: ws,
		All:       historyFlagAll,
		SessionID: historyFlagSession,
		Agent:     historyFlagAgent,
		Tool:      historyFlagTool,
		File:      historyFlagFile,
		Limit:     historyFlagLimit,
	}

	now := time.Now()
	if historyFlagSince != "" {
		t, err := parseWhen(historyFlagSince, now)
		if err != nil {
			return filter, fmt.Errorf("invalid --since: %w", err)
		}
		filter.Since = t
	}
	if historyFlagUntil != "" {
		t, err := parseWhen(historyFlagUntil, now)
		if err != nil {
			return filter, fmt.Errorf("invalid --until: %w", err)
		}
		filter.Until = t
	}
	return filter, nil
}

type historyListJSONEntry struct {
	Seq          int64     `json:"seq"`
	At           time.Time `json:"at"`
	CallID       string    `json:"call_id"`
	Workspace    string    `json:"workspace"`
	Path         string    `json:"path"`
	From         string    `json:"from,omitempty"`
	Kind         string    `json:"kind"`
	Op           string    `json:"op"`
	Tool         string    `json:"tool"`
	SessionID    string    `json:"session_id,omitempty"`
	SessionName  string    `json:"session_name,omitempty"`
	LogicalAgent string    `json:"logical_agent,omitempty"`
	ClientName   string    `json:"client_name,omitempty"`
	Added        int       `json:"added"`
	Removed      int       `json:"removed"`
	Redactions   int       `json:"redactions,omitempty"`
	Content      string    `json:"content"`
	RevertsSeq   int64     `json:"reverts_seq,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	GapBefore    bool      `json:"gap_before,omitempty"`
	GapDropped   bool      `json:"gap_dropped,omitempty"`
}

func toJSONEntry(e history.Entry) historyListJSONEntry {
	return historyListJSONEntry{
		Seq:          e.Seq,
		At:           e.At,
		CallID:       e.CallID,
		Workspace:    e.Workspace,
		Path:         e.Path,
		From:         e.From,
		Kind:         string(e.Kind),
		Op:           string(e.Op),
		Tool:         e.Tool,
		SessionID:    e.SessionID,
		SessionName:  e.SessionName,
		LogicalAgent: e.LogicalAgent,
		ClientName:   e.ClientName,
		Added:        e.Added,
		Removed:      e.Removed,
		Redactions:   e.Redactions,
		Content:      string(e.Content),
		RevertsSeq:   e.RevertsSeq,
		Reason:       e.Reason,
		GapBefore:    e.GapBefore,
		GapDropped:   e.GapDropped,
	}
}

func renderListJSON(entries []history.Entry) error {
	rows := make([]historyListJSONEntry, len(entries))
	for i, e := range entries {
		rows[i] = toJSONEntry(e)
	}
	out, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("marshalling history: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

func renderListText(entries []history.Entry) {
	for _, e := range entries {
		fmt.Println(formatListEntry(e))
		if e.GapBefore {
			gapLine := "  ⋯ unrecorded change (outside plumb's write tools)"
			if e.GapDropped {
				gapLine += "; history dropped rows in this interval"
			}
			fmt.Println(gapLine)
		}
	}
}

func formatListEntry(e history.Entry) string {
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
	return fmt.Sprintf("%s  %-10s  %-17s  %-15s  %s  %s%s", ts, op, e.Tool, sess, p, diffStats, content)
}

func runHistoryShow(_ *cobra.Command, args []string) error {
	target := args[0]

	r, err := history.OpenReadOnly()
	if err != nil {
		return fmt.Errorf("opening history db: %w", err)
	}
	if r == nil {
		return errors.New("no write history database found")
	}
	defer r.Close()

	entries, diffs, callID, err := fetchShowEntries(r, target)
	if err != nil {
		return err
	}

	callSummary, haveCall := lookupCallSummary(callID)

	if historyFlagJSON {
		return renderShowJSON(entries, diffs, haveCall, callSummary)
	}

	renderShowText(entries, diffs, haveCall, callSummary)
	return nil
}

func fetchShowEntries(r *history.Reader, target string) ([]history.Entry, []string, string, error) {
	if seq, err := strconv.ParseInt(target, 10, 64); err == nil {
		e, diff, err := r.Get(seq)
		if err != nil {
			return nil, nil, "", fmt.Errorf("history entry %d: %w", seq, err)
		}
		return []history.Entry{e}, []string{diff}, e.CallID, nil
	}
	entries, diffs, err := r.ByCall(target)
	if err != nil {
		return nil, nil, "", fmt.Errorf("history by call %s: %w", target, err)
	}
	if len(entries) == 0 {
		return nil, nil, "", fmt.Errorf("no history entries found for call %s", target)
	}
	return entries, diffs, target, nil
}

func lookupCallSummary(callID string) (stats.CallSummary, bool) {
	if callID == "" {
		return stats.CallSummary{}, false
	}
	sdb, err := stats.OpenReadOnly()
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

type historyShowJSONEntry struct {
	historyListJSONEntry
	Diff string             `json:"diff,omitempty"`
	Call *stats.CallSummary `json:"call,omitempty"`
}

func renderShowJSON(entries []history.Entry, diffs []string, haveCall bool, call stats.CallSummary) error {
	items := make([]historyShowJSONEntry, len(entries))
	for i, e := range entries {
		items[i] = historyShowJSONEntry{
			historyListJSONEntry: toJSONEntry(e),
			Diff:                 diffs[i],
		}
		if haveCall {
			items[i].Call = &call
		}
	}
	out, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("marshalling history show: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

func renderShowText(entries []history.Entry, diffs []string, haveCall bool, call stats.CallSummary) {
	if haveCall {
		status := "success"
		if !call.Success {
			status = "failed: " + call.ErrorMsg
		}
		sess := ""
		if call.SessionName != "" {
			sess = "  session " + call.SessionName
		}
		fmt.Printf("%s (%dms, %s)%s\n\n", call.Tool, call.DurationMs, status, sess)
	} else {
		fmt.Println("(call metadata unavailable: stats.db has no row for this call)")
		fmt.Println()
	}

	for i, e := range entries {
		path := e.Path
		fromPath := path
		if e.From != "" {
			fromPath = e.From
		}
		fmt.Printf("--- a/%s\n", fromPath)
		fmt.Printf("+++ b/%s\n", path)
		if e.Content == history.ContentDiff {
			if diffs[i] != "" {
				fmt.Println(diffs[i])
			}
		} else {
			fmt.Printf("[%s]\n", e.Content)
		}
		if i < len(entries)-1 {
			fmt.Println()
		}
	}
}

func runHistoryPrune(_ *cobra.Command, _ []string) error {
	if historyFlagPruneBefore == "" {
		return errors.New("--before is required")
	}

	before, err := parseWhen(historyFlagPruneBefore, time.Now())
	if err != nil {
		return fmt.Errorf("invalid --before: %w", err)
	}

	if historyFlagPruneVacuum && daemonAlive() {
		return errors.New("history prune --vacuum needs exclusive access: stop the daemon first (plumb stop)")
	}

	if err := confirmPrune(before); err != nil {
		return err
	}

	ws := historyFlagPruneWorkspace
	if ws != "" {
		ws = paths.Canonical(ws)
	}

	res, err := history.Prune(history.DBPath(), before, ws)
	if err != nil {
		return fmt.Errorf("prune history: %w", err)
	}

	if historyFlagPruneVacuum {
		if err := history.Vacuum(history.DBPath()); err != nil {
			return fmt.Errorf("vacuum history: %w", err)
		}
	}

	fmt.Printf("Pruned %d changes, %d paths, %d workspaces\n", res.Changes, res.Paths, res.Workspaces)
	return nil
}

func confirmPrune(before time.Time) error {
	if historyFlagPruneYes {
		return nil
	}
	if !stdinIsTerminal() {
		return errors.New("refusing to prune history without confirmation: stdin is not a terminal. Re-run with --yes to prune without asking")
	}
	confirmed, err := runYesNoSelector(func(cursor int) string {
		prompt := fmt.Sprintf("Delete write history before %s?", before.Format("2006-01-02 15:04:05"))
		if cursor == 0 {
			return prompt + "\n> Yes   No"
		}
		return prompt + "\n  Yes > No"
	})
	if err != nil {
		return err
	}
	if !confirmed {
		return errors.New("prune aborted")
	}
	return nil
}
