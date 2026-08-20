package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// This file holds the `plumb setup --all` sweep engine — runSetupAll, the
// per-client refresh helpers it drives, and the trailing summary. It is split
// from setup_clients.go, which keeps the target registry and the per-client
// Into writers, so both files stay under the size cap.

// runSetupAll sweeps every client under --all: it repoints existing plumb
// registrations at the current binary and registers installed-but-unregistered
// clients (config file present but no plumb entry) — the bulk repair for a
// moved or rebuilt binary, and the fix for the mismatched-binary warnings
// `plumb doctor` reports, as well as the one-shot first-time setup in a single
// command. --repair and --install-missing are deprecated hidden aliases of
// --all with the same effect; any of the three flags triggers the bulk run,
// and bare `plumb setup` (no flags) opens the interactive picker on a
// terminal (setup_interactive.go) or prints help without one.
func runSetupAll(cmd *cobra.Command, _ []string) error {
	if !setupRepairFlag && !setupAllFlag && !setupInstallMissingFlag {
		if stdinIsTerminal() && stdoutIsTerminal() {
			return runSetupPicker()
		}
		return cmd.Help()
	}
	PrintLogo()

	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	tui.RebuildStyles()
	fmt.Println(tui.MutedStyle.Render("Current binary: " + plumbBin))
	fmt.Println()

	t := render.NewGroupedTable(tui.SepStyle, tui.HintStyle, "Client", "Status", "Config")
	changed := 0
	var failures []setupFailure
	for _, c := range allSetupClients() {
		rows, didChange := refreshClient(c, plumbBin, bulkRegistersMissing())
		if didChange {
			changed++
		}
		failures = appendSetupFailures(failures, c.name, rows)
		t.NextGroup()
		for _, r := range rows {
			t.Row(r.name, statusStyle(r.status).Render(r.status), render.ShortenPath(r.detail, setupPathWidth))
		}
	}
	fmt.Println(t.Render())

	printSetupAllSummary(changed, len(failures))
	printSetupFailures(failures)
	return nil
}

// bulkSetupRunning reports whether this invocation is a bulk --all sweep (or
// one of its deprecated aliases, --repair and --install-missing) rather than a
// named client subcommand. The three flags are only ever set by `plumb setup`
// itself; a subcommand run cannot see them, so their state is a reliable
// signal.
//
// It exists for the --lean clients: a sweep carries no --lean state, so it must
// preserve whatever allowlist is on disk rather than read its own silence as
// "the user wants the full surface back" and strip a key they set on purpose.
func bulkSetupRunning() bool {
	return setupRepairFlag || setupAllFlag || setupInstallMissingFlag
}

// bulkRegistersMissing reports whether the bulk run registers
// installed-but-unregistered clients. --all does, and --repair and
// --install-missing are deprecated aliases of --all, so each of the three
// flags turns it on.
func bulkRegistersMissing() bool {
	return setupAllFlag || setupInstallMissingFlag || setupRepairFlag
}

// printSetupAllSummary prints the trailing summary line for the bulk run.
// failed is how many clients printSetupFailures will name below it, so a sweep
// that could not read a client stops short of claiming every one is current.
func printSetupAllSummary(changed, failed int) {
	switch {
	case changed > 0:
		fmt.Printf("\nUpdated %d client(s). Restart them to apply.\n", changed)
	case failed > 0:
		fmt.Println("\nNo changes — every client plumb could read already has this binary registered.")
	default:
		fmt.Println("\nNo changes — every installed client already has this binary registered.")
	}
}

// setupFailure is one client's failure, held out of the status table so its
// reason prints below it. A parser message names the file, the line and the
// syntax it choked on, which is far wider than any config path — inlined in the
// Config column it stretched every row past the terminal width and wrapped the
// table into unreadability.
type setupFailure struct {
	client string // the client's display name; the table row itself may have blanked it
	err    error
}

// appendSetupFailures appends one setupFailure per errored row in rows. The
// name is taken from client rather than the row because a multi-path client
// blanks it on every row but the first.
func appendSetupFailures(dst []setupFailure, client string, rows []clientRow) []setupFailure {
	for _, r := range rows {
		if r.err != nil {
			dst = append(dst, setupFailure{client: client, err: r.err})
		}
	}
	return dst
}

// printSetupFailures prints the reasons held back from the table — last, after
// the summary, because they are the only part of the report the reader has to
// act on. Home directories are contracted to ~ so the paths quoted inside an
// error read the same as the ones in the table above. Nothing is printed when
// there were no failures.
func printSetupFailures(failures []setupFailure) {
	if len(failures) == 0 {
		return
	}
	fmt.Printf("\n%s\n", tui.WarnStyle.Render(fmt.Sprintf("%d error(s):", len(failures))))
	for _, f := range failures {
		fmt.Printf("  %s: %s\n", f.client, render.ContractHome(f.err.Error()))
	}
}

// setupPathWidth caps the Config column. A grouped table sizes each column to
// its widest cell, so the one deeply nested config on the list (Kimi Work's
// bundled kernel home, three times the width of any other) would otherwise set
// the table's width for every client. Shortening is display-only: the paths
// quoted inside an error keep their full form in the block below the table,
// which is where a reader copies one from.
const setupPathWidth = 60

// clientRow is one row in the `plumb setup --all` table: a client name (blank on
// the continuation rows of a multi-path client, so the paths group visually), a
// short status word, and the config path the status refers to.
//
// err is set exactly when status is "error", and carries the reason the detail
// cell deliberately does not: an error is reported in the table as a status
// against its config path like any other, and its text prints below the table
// (printSetupFailures) so a long message cannot stretch the Config column.
// detail is empty on the one error that has no path to report — a client whose
// config path would not resolve at all.
type clientRow struct {
	name   string
	status string
	detail string
	err    error
}

// refreshClient repoints one client's plumb registration at plumbBin. It repoints
// a client that already references plumb; when installMissing is set it also
// registers plumb in an installed client whose config file exists but has no plumb
// entry. It never fabricates a config for a client with no config file at all —
// unless the target's installedFn vouches that the client is installed anyway
// (Kimi Code's mcp.json only exists once an MCP server is configured), in which
// case installMissing creates the config fresh.
// Returns one table row per managed config path and whether any of them changed. A
// client with pathsFn set (currently only Claude Desktop) yields one row per
// resolved path, not just one.
func refreshClient(c setupTarget, plumbBin string, installMissing bool) (rows []clientRow, changed bool) {
	if c.intoFn == nil {
		return []clientRow{{name: c.name, status: "skipped", detail: "no updater"}}, false
	}
	paths, err := resolveTargetPaths(c)
	if err != nil {
		return []clientRow{{name: c.name, status: "error", err: err}}, false
	}

	for i, cfgPath := range paths {
		row, didChange := refreshClientAt(c, cfgPath, plumbBin, installMissing)
		if didChange {
			changed = true
		}
		if i == 0 {
			row.name = c.name
		}
		rows = append(rows, row)
	}
	return rows, changed
}

// resolveTargetPaths returns every config path refreshClient should manage for
// c: c.pathsFn's full list when set, otherwise the single c.pathFn path.
func resolveTargetPaths(c setupTarget) ([]string, error) {
	if c.pathsFn != nil {
		return c.pathsFn()
	}
	p, err := c.pathFn()
	if err != nil {
		return nil, err
	}
	return []string{p}, nil
}

// refreshClientAt is refreshClient's single-path body. It returns the row for
// this config path with its name left blank for the caller to fill, and whether
// the path changed. A config-present client without a plumb entry is only
// registered ("registered") when installMissing is set; otherwise it is reported
// "not registered" and left untouched. A repointed existing entry reports
// "updated". An absent config means "not installed" — unless the target's
// installedFn detects the client anyway, in which case it is treated as
// installed-but-unregistered (and installMissing creates the config). A writer
// that fails reports "error" against the same path every other status uses,
// with the reason on the row's err for printing below the table.
func refreshClientAt(c setupTarget, cfgPath, plumbBin string, installMissing bool) (row clientRow, changed bool) {
	row.detail = render.ContractPath(cfgPath)
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if c.installedFn == nil || !c.installedFn() {
			row.status = "not installed"
			return row, false
		}
		// Installed client whose config does not exist yet (Kimi Code, DeepSeek
		// Harness) — fall through as installed-but-unregistered.
	}
	hadPlumb := clientHasPlumb(c, cfgPath)
	if !hadPlumb && !installMissing {
		row.status = "not registered"
		return row, false
	}
	added, _, err := c.intoFn(cfgPath, plumbBin)
	switch {
	case err != nil:
		row.status, row.err = "error", err
		return row, false
	case !added:
		row.status = "already current"
		return row, false
	case hadPlumb:
		row.status = "updated"
	default:
		row.status = "registered"
	}
	return row, true
}

// clientHasPlumb reports whether cfgPath already registers a plumb server, using
// the structured extractor when available and falling back to a substring scan.
func clientHasPlumb(c setupTarget, cfgPath string) bool {
	if c.extractFn != nil {
		if _, registered, err := c.extractFn(cfgPath); err == nil {
			return registered
		}
	}
	data, err := os.ReadFile(cfgPath)
	return err == nil && strings.Contains(string(data), "plumb")
}
