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
	for _, c := range allSetupClients() {
		rows, didChange := refreshClient(c, plumbBin, bulkRegistersMissing())
		if didChange {
			changed++
		}
		t.NextGroup()
		for _, r := range rows {
			t.Row(r.name, statusStyle(r.status).Render(r.status), r.detail)
		}
	}
	fmt.Println(t.Render())

	printSetupAllSummary(changed)
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
func printSetupAllSummary(changed int) {
	if changed == 0 {
		fmt.Println("\nNo changes — every installed client already has this binary registered.")
	} else {
		fmt.Printf("\nUpdated %d client(s). Restart them to apply.\n", changed)
	}
}

// clientRow is one row in the `plumb setup --all` table: a client name (blank on
// the continuation rows of a multi-path client, so the paths group visually), a
// short status word, and the config path or error the status refers to.
type clientRow struct {
	name   string
	status string
	detail string
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
		return []clientRow{{name: c.name, status: "error", detail: err.Error()}}, false
	}

	for i, cfgPath := range paths {
		status, detail, didChange := refreshClientAt(c, cfgPath, plumbBin, installMissing)
		if didChange {
			changed = true
		}
		name := c.name
		if i > 0 {
			name = ""
		}
		rows = append(rows, clientRow{name: name, status: status, detail: detail})
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

// refreshClientAt is refreshClient's single-path body. It returns a short status
// word plus a detail cell — the contracted config path, or the error text when
// status is "error". A config-present client without a plumb entry is only
// registered ("registered") when installMissing is set; otherwise it is reported
// "not registered" and left untouched. A repointed existing entry reports
// "updated". An absent config means "not installed" — unless the target's
// installedFn detects the client anyway, in which case it is treated as
// installed-but-unregistered (and installMissing creates the config).
func refreshClientAt(c setupTarget, cfgPath, plumbBin string, installMissing bool) (status, detail string, changed bool) {
	detail = render.ContractPath(cfgPath)
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if c.installedFn == nil || !c.installedFn() {
			return "not installed", detail, false
		}
		// Installed client whose config does not exist yet (Kimi Code, DeepSeek
		// Harness) — fall through as installed-but-unregistered.
	}
	hadPlumb := clientHasPlumb(c, cfgPath)
	if !hadPlumb && !installMissing {
		return "not registered", detail, false
	}
	added, _, err := c.intoFn(cfgPath, plumbBin)
	if err != nil {
		return "error", err.Error(), false
	}
	if !added {
		return "already current", detail, false
	}
	if hadPlumb {
		return "updated", detail, true
	}
	return "registered", detail, true
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
