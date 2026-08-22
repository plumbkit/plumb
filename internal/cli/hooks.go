package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

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
	RunE: func(_ *cobra.Command, _ []string) error { return runHooksStatus() },
}

var hooksInstallCmd = &cobra.Command{
	Use:   "install [client]",
	Short: "Install or refresh plumb's lifecycle hooks",
	Long: `Install plumb's opt-in lifecycle hooks in every registered client, or in the
named one. Two hooks per client: SessionStart states the conversation ID that
session_start records as session_id, and Stop reports unread peer mail.

Claude Code's Stop hook is a background watcher (async + asyncRewake): it wakes
a session that has already gone idle. Codex has no equivalent, so its Stop hook
performs one read-only check as a turn ends — that narrows the end-of-turn race,
it is not push delivery. Both fail open: a missing mailbox, an unavailable
daemon or an ambiguous session all let the turn end normally, and neither ever
carries a message body.

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
script files on disk are left alone.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error { return runHooksUninstall(args) },
}

func init() {
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
	for _, t := range targets {
		path, entries, before, err := hookPlan(t, plumbBin)
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
	report.render(skips)
	return nil
}

// runHooksUninstall removes plumb's hooks. It is deliberately ungated on
// registration: removing a registration and then being unable to remove its
// hooks would be the wrong way round.
func runHooksUninstall(args []string) error {
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
		path, _, before, err := hookPlan(t, plumbBin)
		if err != nil {
			report.clientError(t, err)
			continue
		}
		removed, err := removeHooksAt(path, t.ours)
		if err != nil {
			report.clientError(t, err)
			continue
		}
		report.group(t, path, before, uninstallAction)
		// Every handler plumb owns goes, including one an older plumb installed
		// on an event this version no longer writes. Saying so keeps the count
		// in the output honest when it exceeds the rows above.
		if extra := removed - installedCount(before); extra > 0 {
			report.note(fmt.Sprintf("%s: %d further plumb hook %s removed (installed by an earlier version).",
				t.name, extra, textfmt.Plural(extra, "entry", "entries")))
		}
	}
	report.render(nil)
	return nil
}

// hookPlan resolves one client's config path, the entries this binary would
// write, and their current state on disk — the common preamble of both writers
// and of the status table.
func hookPlan(t hooksTarget, plumbBin string) (path string, entries []hookEntry, before []hookState, err error) {
	path, err = t.pathFn()
	if err != nil {
		return "", nil, nil, fmt.Errorf("locating %s hooks config: %w", t.name, err)
	}
	entries = t.entries(plumbBin)
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

// hookMailReport prefers the stable conversation ID and falls back to cwd only
// when it identifies exactly one live session. Every error is intentionally
// converted to "no report": these hooks are advisory and must fail open.
func hookMailReport(sessionID, cwd string) (mailReport, bool) {
	if id := strings.TrimSpace(sessionID); id != "" {
		if report, err := mailReportFor("external-id", id); err == nil {
			return report, true
		}
	}
	if dir := strings.TrimSpace(cwd); dir != "" {
		if report, err := mailReportFor("workspace", dir); err == nil {
			return report, true
		}
	}
	return mailReport{}, false
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
