package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/memory"
)

// session_start_brief.go implements the `detail: "brief"` orientation
// packet: a ≤1.5 KB subset of the full packet for a caller that just needs
// cheap re-orientation — a subagent, or a session resuming under the same
// session_id. See SessionStart.executeBrief for the exact field list and
// worth-it strategy §4.1, §5 W1-2 / PLAN-356 for the rationale.

// briefOrientationFooter is the exact closing line the card pins: it tells
// the caller how to get the complete packet, so a brief response never reads
// as "this is everything there is".
const briefOrientationFooter = `brief orientation — call session_start({detail:"full"}) for the complete packet` + "\n"

// maxListedBriefNames caps how many peer or memory names the brief packet
// spells out before falling back to a "+N more" tail — keeping every brief
// field's size bounded regardless of how many peers or memories a workspace
// accumulates.
const maxListedBriefNames = 8

// resolveDetail decides which orientation packet this call renders. An
// explicit `detail` argument always wins ("brief" or "full" — validated here
// since it is caller-supplied input, unlike autoBrief which is derived);
// otherwise the default is "full", flipped to "brief" only when autoBrief is
// true. autoBrief is the caller's signal that this session_id was already
// seen by this daemon within the last 24h (see the WithExternalID /
// session.FindEnded grace window) — a resumed conversation re-orienting
// itself, not a first bootstrap. First contact always defaults to full:
// bootstrap discoverability (PR #189) must never thin out.
func resolveDetail(raw json.RawMessage, autoBrief bool) (string, error) {
	var a struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("session_start: invalid arguments: %w", err)
	}
	switch a.Detail {
	case "":
		if autoBrief {
			return "brief", nil
		}
		return "full", nil
	case "brief", "full":
		return a.Detail, nil
	default:
		return "", fmt.Errorf("session_start: detail must be %q or %q, got %q", "brief", "full", a.Detail)
	}
}

// executeBrief renders the brief orientation packet: workspace path,
// language, branch, a one-line git policy, a diagnostics COUNT (never
// bodies), the active-peer count and names, memory NAMES only (no
// descriptions or sizes), the edit-lane rule for clients that need it, and
// the closing pointer to the full packet. Everything else in the full packet
// (context.md, commits, working-tree diffstat, tool stats, the tokens
// banner, the full client guidance block, submodules, tasks, the episodic
// summary, messages, collab policy) is full-only — per the card's Do NOT,
// brief never grows a second "sections" knob to pick among them.
func (t *SessionStart) executeBrief(ws, lang string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Workspace: %s\n\n", ws)
	if lang != "" {
		fmt.Fprintf(&sb, "Language: %s\n", lang)
	}
	branch := gitBranch(ws)
	if branch != "" {
		fmt.Fprintf(&sb, "Branch:   %s\n", branch)
	}
	if t.gitPolicyFn != nil && branch != "" {
		fmt.Fprintf(&sb, "Git:      %s\n", briefGitPolicy(t.gitPolicyFn()))
	}
	fmt.Fprintf(&sb, "Diagnostics: %d\n", t.diagnosticsCount())
	if n, names := t.briefPeers(ws); n > 0 {
		fmt.Fprintf(&sb, "Peers (%d): %s\n", n, joinBriefNames(names))
	}
	sb.WriteString(briefMemoriesLine(ws))
	if clientHasNativeEditConflict(t.clientNameFn) {
		sb.WriteString("\n" + strings.TrimRight(nativeEditLaneWarning, "\n") + "\n")
	}
	sb.WriteString("\n" + briefOrientationFooter)
	return sb.String()
}

// briefGitPolicy renders the one-line git policy summary the brief packet
// carries in place of formatGitPolicy's full multi-line body.
func briefGitPolicy(p GitPolicy) string {
	if !p.AllowWrites {
		return "read-only (commits disabled)"
	}
	return fmt.Sprintf("writes on, destructive %s, push %s", gitGateLabel(p.AllowDestructive), gitGateLabel(p.AllowPush))
}

// diagnosticsCount returns the number of active errors and warnings with no
// per-diagnostic detail — the brief packet reports the count so a caller
// knows to reach for `diagnostics` (or session_start with detail:"full")
// instead of paying for every message body up front.
func (t *SessionStart) diagnosticsCount() int {
	if t.diag == nil {
		return 0
	}
	n := 0
	for _, diags := range t.diag.AllDiagnostics() {
		for _, d := range diags {
			if d.Severity <= protocol.SevWarning {
				n++
			}
		}
	}
	return n
}

// briefPeers returns the active-peer count and names for the brief packet,
// reusing the same [collab] peer_awareness gate and session listing as the
// full digest (writeSessionPeers/activePeers) but skipping the areas-touched
// lookup — that is a stats + topology query the brief packet does not pay
// for.
func (t *SessionStart) briefPeers(ws string) (int, []string) {
	if t.collabFn == nil {
		return 0, nil
	}
	if enabled, _ := t.collabFn(); !enabled {
		return 0, nil
	}
	peers := t.activePeers(ws)
	if len(peers) == 0 {
		return 0, nil
	}
	names := make([]string, 0, len(peers))
	for _, p := range peers {
		names = append(names, p.Name)
	}
	return len(peers), names
}

// briefMemoriesLine renders the memory NAMES-only line: user-authored memory
// names (capped, same shape as the full packet's list), plus a bare
// generated count — never descriptions or byte sizes, which is what keeps
// this line short regardless of how detailed the full memory listing gets.
func briefMemoriesLine(ws string) string {
	mems, err := memory.List(ws)
	if err != nil || len(mems) == 0 {
		return "Memories: none\n"
	}
	var user, generated []memory.Memory
	for _, m := range mems {
		if m.UserAuthored() {
			user = append(user, m)
		} else {
			generated = append(generated, m)
		}
	}
	if len(user) == 0 {
		return fmt.Sprintf("Memories: 0 user (%d generated)\n", len(generated))
	}
	names := make([]string, len(user))
	for i, m := range user {
		names[i] = m.Name
	}
	line := fmt.Sprintf("Memories (%d user", len(user))
	if len(generated) > 0 {
		line += fmt.Sprintf(", %d generated", len(generated))
	}
	line += "): " + joinBriefNames(names) + "\n"
	return line
}

// joinBriefNames renders a comma-joined name list capped at
// maxListedBriefNames, appending a "+N more" tail — the same shape the full
// packet uses (writeUserMemoryList, formatPeerDigest) so a brief field never
// grows unbounded with workspace size.
func joinBriefNames(names []string) string {
	if len(names) <= maxListedBriefNames {
		return strings.Join(names, ", ")
	}
	shown := names[:maxListedBriefNames]
	return fmt.Sprintf("%s, +%d more", strings.Join(shown, ", "), len(names)-maxListedBriefNames)
}
