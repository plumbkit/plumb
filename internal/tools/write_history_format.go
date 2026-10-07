package tools

import (
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// Rendering of write_history results: list lines, show output, and the
// response-budget and truncation notices.

func formatWriteHistoryResult(res writeHistoryResult) string {
	if res.Mode == "show" {
		return formatShowResult(res)
	}
	return formatListResult(res)
}

func formatListResult(res writeHistoryResult) string {
	if len(res.Entries) == 0 {
		if res.Offset > 0 {
			return fmt.Sprintf("No write history at offset %d in workspace %s (past the last match).", res.Offset, res.Workspace)
		}
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
	if res.Limit > 0 && len(res.Entries) == res.Limit {
		fmt.Fprintf(&sb, "\n… more may exist: next page offset=%d\n", res.Offset+len(res.Entries))
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
	if res.CallID != "" {
		if len(res.Entries) == 0 {
			fmt.Fprintf(&sb, "Offset %d is past the end of call %s (%d change(s)).", res.Offset, res.CallID, res.Total)
			return sb.String()
		}
		fmt.Fprintf(&sb, "Call %s: changes %d–%d of %d\n\n", res.CallID, res.Offset+1, res.Offset+len(res.Entries), res.Total)
	}

	// The diff budget counts diff bytes only, so one change always gets its
	// full max_diff_bytes (which never exceeds the budget) and headers do not
	// eat it. The whole response is capped too: a call of thousands of withheld
	// or binary rows spends no diff bytes but prints a header for each.
	diffSpent := 0

	for i, e := range res.Entries {
		if diffSpent >= maxTotalShowBytes || sb.Len() >= maxShowOutputBytes {
			omitted := len(res.Entries) - i
			fmt.Fprintf(&sb, "… (%d change(s) omitted to stay within the response budget; continue with offset=%d, or show one with seq)\n", omitted, res.Offset+i)
			break
		}

		fromPath := e.Path
		if e.From != "" {
			fromPath = e.From
		}
		fmt.Fprintf(&sb, "#%d %s %s\n", e.Seq, e.Op, e.Path)
		fmt.Fprintf(&sb, "--- a/%s\n", fromPath)
		fmt.Fprintf(&sb, "+++ b/%s\n", e.Path)
		if e.Content == history.ContentDiff {
			diff, err := res.diffAt(i)
			if err != nil {
				fmt.Fprintf(&sb, "[diff unavailable: %v]\n", err)
			} else {
				diffSpent += formatShowDiff(&sb, diff, e.Seq, res.MaxDiffBytes, maxTotalShowBytes-diffSpent)
			}
		} else {
			fmt.Fprintf(&sb, "[%s]\n", e.Content)
		}
		if i < len(res.Entries)-1 {
			sb.WriteByte('\n')
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (res writeHistoryResult) diffAt(i int) (string, error) {
	if res.DiffFor != nil {
		return res.DiffFor(i)
	}
	if i < len(res.Diffs) {
		return res.Diffs[i], nil
	}
	return "", nil
}

// formatShowDiff writes diff, cut on a UTF-8 boundary at the smaller of
// maxDiffBytes and avail (what is left of the response budget), with a notice
// that names the cap that applied and how to see more. It returns the diff
// bytes written.
func formatShowDiff(sb *strings.Builder, diff string, seq int64, maxDiffBytes, avail int) int {
	if diff == "" {
		return 0
	}
	limit := min(maxDiffBytes, avail)
	notice := ""
	if len(diff) > limit {
		diff, _ = textfmt.ClampBytesKept(diff, limit)
		switch {
		case avail < maxDiffBytes:
			notice = fmt.Sprintf("… (diff truncated: response budget reached; show this change alone with seq=%d)", seq)
		case maxDiffBytes < maxAllowedDiffBytes:
			notice = fmt.Sprintf("… (diff truncated at max_diff_bytes=%d; raise it, up to %d, to see more)", maxDiffBytes, maxAllowedDiffBytes)
		default:
			notice = fmt.Sprintf("… (diff truncated at the %d-byte maximum; `plumb history show %d` prints it in full)", maxAllowedDiffBytes, seq)
		}
	}
	sb.WriteString(diff)
	if !strings.HasSuffix(diff, "\n") {
		sb.WriteByte('\n')
	}
	if notice != "" {
		sb.WriteString(notice)
		sb.WriteByte('\n')
	}
	return len(diff)
}
