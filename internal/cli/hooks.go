package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// `plumb hooks` is the lifecycle-hook counterpart of `plumb setup` and
// `plumb skills`: bare it reports, and the two writers install and remove.
//
//	plumb hooks                    # read-only status, per client, per hook
//	plumb hooks install [client]   # install or refresh
//	plumb hooks uninstall [client] # remove plumb's handlers, and only those
//
// The split mirrors `plumb skills` deliberately. Hooks execute commands with
// the user's credentials, so installation is always an explicit, consented
// step: nothing here runs from `plumb setup`, from project config, or from a
// repository a user cloned. Removal is the one direction that also happens
// elsewhere — `plumb setup <client> --uninstall` calls it, because hooks left
// pointing at a deregistered plumb are dead weight.
//
// The per-client data lives in hooks_clients.go; the writers and the ownership
// rules they depend on live there too.

var hooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Show which plumb lifecycle hooks are installed per client",
	Long: `Show, for every client plumb ships lifecycle hooks for (claude-code, codex),
whether each hook is installed, missing, or stale relative to what this binary
would write. Read-only.

` + "`plumb hooks install [client]`" + ` installs or refreshes them;
` + "`plumb hooks uninstall [client]`" + ` removes them again. A client whose
config does not register plumb is shown as unregistered — hooks are only
installed where plumb is registered, since the linkage they supply and the
mailbox they probe both need plumb's tool surface to be reachable.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error { return runHooksStatus(cmd) },
}

var hooksInstallCmd = &cobra.Command{
	Use:   "install [client]",
	Short: "Install or refresh plumb's lifecycle hooks",
	Long: `Install plumb's opt-in lifecycle hooks in every registered client, or in the
named one. SessionStart states the conversation ID that session_start records
as session_id, and Stop reports unread peer mail. Claude Code gets a third,
PreToolUse, which stamps a per-agent identity onto every plumb call so
subagents multiplexed over one connection keep their own workspace pin and
trackers instead of being refused as unattributable.

Claude Code's Stop hook is a background watcher (async + asyncRewake): it wakes
a session that has already gone idle. Codex has no equivalent, so its Stop hook
performs one read-only check as a turn ends — that narrows the end-of-turn race,
it is not push delivery. All of them fail open: a missing mailbox, an
unavailable daemon, an ambiguous session or a daemon that predates the identity
channel all leave the turn and the call exactly as the client sent them, and
none ever carries a message body.

Existing entries are merged, never replaced wholesale: hooks the user wrote
survive, the file is backed up first, and re-running refreshes plumb's own
entries after the binary moves.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error { return runHooksInstall(args) },
}

var hooksUninstallCmd = &cobra.Command{
	Use:   "uninstall [client]",
	Short: "Remove plumb's lifecycle hooks",
	Long: `Remove plumb's lifecycle hooks from every client that has them, or from the
named one. Only plumb's own handlers go — hooks the user wrote on the same
events survive, the file is backed up first, and a client plumb has no hooks in
is a no-op. Hooks installed by an earlier plumb, including the hand-installed
shell scripts plumb's own recipe documented, are recognised and removed too;
script files on disk are left alone.

--only context removes just the advisory context-hint handlers
(UserPromptSubmit, SubagentStart) and leaves session linkage, mailbox and
identity hooks in place. The SessionStart handler also carries the session
linkage, so it stays; [context] hints = false or PLUMB_CONTEXT_HINTS=off
silences the hint it adds.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error { return runHooksUninstall(args) },
}

// hooksUninstallOnly narrows `plumb hooks uninstall` to one group of handlers.
var hooksUninstallOnly string

// hooksInstallContext opts a client into the advisory context-hint handlers.
var hooksInstallContext bool

func init() {
	hooksUninstallCmd.Flags().StringVar(&hooksUninstallOnly, "only", "",
		"remove only this group of plumb's handlers: context")
	hooksInstallCmd.Flags().BoolVar(&hooksInstallContext, "context", false,
		"also install the experimental advisory context-hint handlers (then turn hints on with [context] hints = true or PLUMB_CONTEXT_HINTS=on)")
	hooksCmd.AddCommand(hooksInstallCmd, hooksUninstallCmd, hooksRunCodexCmd, hooksRunClaudeCmd)
}

// resolveHooksTargets turns an optional client argument into the set to act on.
// A named client is an error when it is unknown, or — for install — when plumb
// is not registered in it; the sweep skips an unregistered client with a note
// instead, exactly as `plumb skills sync` does.
func resolveHooksTargets(args []string, requireRegistration bool) (targets []hooksTarget, skips []string, err error) {
	if len(args) == 1 {
		t, ok := findHooksTarget(args[0])
		if !ok {
			return nil, nil, fmt.Errorf("unknown hooks client %q — supported: %s", args[0], hooksClientNames())
		}
		if requireRegistration && !plumbRegisteredIn(t.setup) {
			return nil, nil, fmt.Errorf("plumb is not registered in %s — run `plumb setup %s` first, then re-run `plumb hooks install %s`",
				t.name, t.setup.use, t.use)
		}
		return []hooksTarget{t}, nil, nil
	}
	for _, t := range hooksTargets() {
		if requireRegistration && !plumbRegisteredIn(t.setup) {
			skips = append(skips, fmt.Sprintf("Skipping %s — plumb is not registered (`plumb setup %s`).", t.name, t.setup.use))
			continue
		}
		targets = append(targets, t)
	}
	return targets, skips, nil
}

// runHooksInstall installs or refreshes hooks, reporting the action taken per
// hook. The action comes from the state BEFORE the write, so "installed",
// "updated" and "current" say what actually happened rather than restating the
// end state three times.
func runHooksInstall(args []string) error {
	PrintLogo()
	targets, skips, err := resolveHooksTargets(args, true)
	if err != nil {
		return err
	}
	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	report := newHookReport()
	if _, err := ensureResumeProofKey(); err != nil {
		// Not fatal: the identity hook still stamps, and makes the key on its first
		// run if it can. Without it a replacement serve resumes by name only.
		report.note(fmt.Sprintf("Could not create the resume proof key (%v); a replacement serve will resume by name only.", err))
	}
	for _, t := range targets {
		path, entries, before, err := hookPlan(t, plumbBin, hooksInstallContext)
		if err != nil {
			report.clientError(t, err)
			continue
		}
		if _, err := installHooksAt(path, entries, t.ours); err != nil {
			report.clientError(t, err)
			continue
		}
		report.group(t, path, before, installAction)
		// The notes print on the no-op path too. Codex re-hashes and un-trusts a
		// hook whose command changes, so the user re-running install after a Codex
		// upgrade needs the `/hooks` reminder most precisely when plumb writes
		// nothing — the same call `plumb setup` makes about its own per-client note.
		report.note(t.notes...)
	}
	report.render(skips, nil)
	return nil
}

// runHooksUninstall removes plumb's hooks. It is deliberately ungated on
// registration: removing a registration and then being unable to remove its
// hooks would be the wrong way round.
func runHooksUninstall(args []string) error {
	only, err := hooksUninstallScope(hooksUninstallOnly)
	if err != nil {
		return err
	}
	PrintLogo()
	targets, _, err := resolveHooksTargets(args, false)
	if err != nil {
		return err
	}
	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	report := newHookReport()
	for _, t := range targets {
		path, _, before, err := hookPlan(t, plumbBin, false)
		if err != nil {
			report.clientError(t, err)
			continue
		}
		before = only.states(before)
		removed, err := removeHooksAt(path, only.ownership(t.ours))
		if err != nil {
			report.clientError(t, err)
			continue
		}
		report.group(t, path, before, uninstallAction)
		if only.events != nil {
			report.note(fmt.Sprintf("%s: the SessionStart hook stays (it also links the session); "+
				"set [context] hints = false or PLUMB_CONTEXT_HINTS=off to silence its hint.", t.name))
		}
		// Every handler plumb owns goes, including one an older plumb installed
		// on an event this version no longer writes. Saying so keeps the count
		// in the output honest when it exceeds the rows above.
		if extra := removed - installedCount(before); extra > 0 {
			report.note(fmt.Sprintf("%s: %d further plumb hook %s removed (installed by an earlier version).",
				t.name, extra, textfmt.Plural(extra, "entry", "entries")))
		}
	}
	report.render(nil, nil)
	return nil
}

// hookPlan resolves one client's config path, the entries this binary would
// write, and their current state on disk — the common preamble of both writers
// and of the status table.
//
// The opt-in context-hint entries are part of the plan when withContext asks
// for them or when any of them is already installed: present ones are kept
// fresh and reported, absent ones are neither written nor listed as missing.
func hookPlan(t hooksTarget, plumbBin string, withContext bool) (path string, entries []hookEntry, before []hookState, err error) {
	path, err = t.pathFn()
	if err != nil {
		return "", nil, nil, fmt.Errorf("locating %s hooks config: %w", t.name, err)
	}
	entries = t.entries(plumbBin)
	if t.contextEntries != nil {
		ctx := t.contextEntries(plumbBin)
		if !withContext {
			states, err := hookStatesAt(path, ctx, t.ours)
			if err != nil {
				return "", nil, nil, err
			}
			withContext = slices.ContainsFunc(states, func(s hookState) bool { return s.state != hookStateMissing })
		}
		if withContext {
			entries = append(entries, ctx...)
		}
	}
	before, err = hookStatesAt(path, entries, t.ours)
	if err != nil {
		return "", nil, nil, err
	}
	return path, entries, before, nil
}

func installedCount(states []hookState) int {
	n := 0
	for _, s := range states {
		if s.state != hookStateMissing {
			n++
		}
	}
	return n
}

// Shared hook runtime — used by both clients' hidden run verbs.

// hookMailReport observes only the daemon's established live recipient. An
// unavailable or older daemon allows completion; offline mail is never a fallback.
func hookMailReport(sessionID, _ string) (mailReport, bool) {
	reply, ok := probeHookMailbox(sessionID, false)
	return reply.Report, ok
}

// hookWakeProbe is hookMailReport plus the one extra fact an adaptive wake
// window needs: how many OTHER live sessions share this session's workspace.
//
// Peer presence is what justifies holding a watcher open past the base window.
// Nobody outside this workspace can write to this mailbox uninvited, so a
// session with no peer has nothing to wait for and keeps the short window and
// its cost; only a workspace where a peer could actually send pays for the long
// one. Like the mail report, the answer is a COUNT: it names no peer and
// discloses nothing about one beyond existence.
//
// Kept separate from hookMailReport because Codex's hook shares that probe and
// has no adaptive window, and because mailReport's field set is a deliberate
// disclosure surface serialised by `plumb mail --json` — widening it here would
// change a published shape for a caller that never asked.
func hookWakeProbe(sessionID, cwd string) (mailReport, int, bool) {
	report, ok := hookMailReport(sessionID, cwd)
	if !ok {
		return report, 0, false
	}
	return report, hookPeerCount(report.Workspace, report.Session), true
}

// hookPeerCount counts the live sessions rooted at exactly this workspace, other
// than self. Exact-root equality rather than the nearest-root rule mailReportFor
// resolves with: a session on a parent or child root is a different workspace
// for mail purposes, and counting it would extend a watcher for a peer that
// cannot reach it. Every failure reads as zero peers, which costs only the
// extension — the fail-open direction this whole path is built on.
func hookPeerCount(workspace, self string) int {
	if strings.TrimSpace(workspace) == "" {
		return 0
	}
	all, err := session.List()
	if err != nil {
		return 0
	}
	root := filepath.Clean(workspace)
	n := 0
	for _, s := range all {
		if s.Folder == "" || (self != "" && s.Name == self) {
			continue
		}
		if filepath.Clean(s.Folder) == root {
			n++
		}
	}
	return n
}

// sessionLinkageSentence states the conversation id as a fact and names the
// parameter that records it. The phrasing is deliberate: context injected by a
// hook should read as a factual statement rather than an out-of-band
// instruction, which a client may surface to the user instead of acting on.
func sessionLinkageSentence(id, subject string) string {
	quoted := strconv.Quote(id)
	return fmt.Sprintf(
		"Plumb session linkage: this %s has id %s. The plumb session_start tool records it via its "+
			"session_id parameter, which is what lets plumb mail address this session: session_start({session_id: %s}).",
		subject, quoted, quoted)
}
