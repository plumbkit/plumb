package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// This file is the inverse of the registration writers: `plumb setup <client>
// --uninstall` removes plumb's server entry from that client's config —
// and, for a skill-capable client, the skill files plumb itself installed,
// and, for an instructions-capable client, plumb's managed instruction block
// (see removeInstructionsBlock in setup_instructions.go). Every removal
// backs up before writing, preserves sibling entries, and is a no-op when
// plumb is not registered: an uninstall must be as safe to repeat as a
// registration.

var setupUninstallFlag bool

// registerUninstallFlag wires --uninstall onto one setup subcommand. One
// package var serves every subcommand because exactly one of them runs per
// invocation; the bulk parent command never reads it.
func registerUninstallFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&setupUninstallFlag, "uninstall", false,
		"Remove plumb from this client's config (and its plumb-installed skills)")
}

// runSetupTargetOrUninstall is the RunE body shared by every target-backed
// subcommand: --uninstall takes the removal path, anything else registers.
func runSetupTargetOrUninstall(t setupTarget) error {
	if !setupUninstallFlag {
		return runSetupTarget(t)
	}
	return runSetupUninstall(t)
}

// runSetupUninstall removes plumb from every config path the target manages.
func runSetupUninstall(t setupTarget) error {
	paths, err := resolveTargetPaths(t)
	if err != nil {
		return fmt.Errorf("locating %s config: %w", t.name, err)
	}
	return uninstallTargetAt(t, paths, true)
}

// uninstallTargetAt removes plumb's registration from each path, then — when a
// registration was actually removed and the user scope is in play — everything
// else plumb installed for that client: the skill files its sync wrote, and the
// lifecycle hooks `plumb hooks install` wrote. userScoped is false only for a
// project-scoped Claude Code uninstall: both skills and hooks live in the user
// scope, so removing a project registration must not touch them.
//
// Hooks are removed here but never installed here. Consent runs one way: a user
// asks for hooks explicitly, and an uninstall that left them behind would leave
// commands firing on every turn for a client that no longer has plumb.
func uninstallTargetAt(t setupTarget, paths []string, userScoped bool) error {
	PrintLogo()
	if t.outFn == nil {
		return fmt.Errorf("uninstall is not supported for %s", t.name)
	}

	removedAny := false
	lines := make([]string, 0, len(paths))
	for _, cfgPath := range paths {
		removed, err := t.outFn(cfgPath)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s: error: %v", render.ContractPath(cfgPath), err))
			continue
		}
		if removed {
			removedAny = true
			lines = append(lines, render.ContractPath(cfgPath)+": unregistered")
		}
	}

	if removedAny {
		lines = uninstallSideEffectLines(t, removeSkills, lines)
	}

	if !removedAny {
		fmt.Printf("plumb is not registered in %s — nothing to do.\n", t.name)
		for _, cfgPath := range paths {
			fmt.Printf("Config: %s\n", cfgPath)
		}
		return nil
	}

	tui.RebuildStyles()
	fmt.Println(render.ContextBox(tui.MutedStyle.Render("Unregistered from "+t.name+"\n"+strings.Join(lines, "\n")), tui.SepStyle))
	fmt.Printf("\nRestart %s to apply the change.\n", t.name)
	return nil
}

// uninstallSideEffectLines appends the skill-removal and instructions-block-
// removal report lines onto lines, returning the extended slice — factored
// out of uninstallTargetAt to keep that function under the project's
// cyclomatic-complexity budget. Only called once something was actually
// unregistered (see uninstallTargetAt), so the removals here are
// unconditional on that account.
//
// removeSkills gates everything user-scoped, not just skills: hooks and the
// instructions block live in the user scope too, so a project-scoped Claude
// Code uninstall must leave all three alone.
func uninstallSideEffectLines(t setupTarget, removeSkills bool, lines []string) []string {
	if removeSkills && t.skillsDirFn != nil {
		if dir, err := t.skillsDirFn(); err == nil {
			removed, kept := removePlumbSkills(dir)
			if len(removed) > 0 {
				lines = append(lines, fmt.Sprintf("skills removed: %d from %s", len(removed), render.ContractPath(dir)))
			}
			if len(kept) > 0 {
				lines = append(lines, "skills left in place (not plumb's): "+strings.Join(kept, ", "))
			}
		}
	}

	lines = append(lines, removePlumbHooksFor(t)...)

	if t.instructionsFn != nil {
		instrLines, err := removeInstructionsBlock(t)
		if err != nil {
			lines = append(lines, fmt.Sprintf("instructions: error: %v", err))
		} else {
			lines = append(lines, instrLines...)
		}
	}
	return lines
// removePlumbHooksFor takes plumb's lifecycle hooks back out of a client whose
// registration has just been removed, returning the report lines for the
// uninstall box. A client with no hooks pack, an unreadable hooks config, or no
// plumb hooks in it contributes nothing: an uninstall reports what it did, and
// a hook that was never there is not news.
func removePlumbHooksFor(t setupTarget) []string {
	h, ok := findHooksTarget(t.use)
	if !ok {
		return nil
	}
	path, err := h.pathFn()
	if err != nil {
		return nil
	}
	removed, err := removeHooksAt(path, h.ours)
	if err != nil {
		return []string{fmt.Sprintf("hooks: error: %v", err)}
	}
	if removed == 0 {
		return nil
	}
	return []string{fmt.Sprintf("hooks removed: %d from %s", removed, render.ContractPath(path))}
}

// removeServerEntry is mergeServerEntry's inverse: it deletes the "plumb" key
// from serversKey in cfgPath, backing up first and preserving every sibling.
// removed is false — and nothing is written — when the file does not exist,
// holds no plumb entry, or the servers key is not an object (a shape plumb
// cannot have registered into, so an uninstall must not "fix" it either). A
// servers key left empty is dropped entirely: for the clients whose config
// plumb created from scratch (Kimi Code's mcp.json is the clear case) that
// restores the pre-plumb shape, and no client reads an empty server map
// differently from a missing key.
func removeServerEntry(
	cfgPath, serversKey string,
	read func(string) (map[string]any, bool, error),
	write func(string, map[string]any) error,
) (removed bool, err error) {
	if _, err := os.Stat(cfgPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	cfg, _, err := read(cfgPath)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", cfgPath, err)
	}
	servers, ok := cfg[serversKey].(map[string]any)
	if !ok {
		return false, nil
	}
	if _, ok := servers["plumb"]; !ok {
		return false, nil
	}
	if err := backupFile(cfgPath); err != nil {
		return false, fmt.Errorf("backing up %s: %w", cfgPath, err)
	}
	delete(servers, "plumb")
	if len(servers) == 0 {
		delete(cfg, serversKey)
	}
	if err := write(cfgPath, cfg); err != nil {
		return false, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, nil
}

// removeServerEntryFrom builds an outFn for a client whose servers live under
// serversKey with the given serialisation — the removal counterpart of
// mapCommandExtractor's wiring.
func removeServerEntryFrom(
	serversKey string,
	read func(string) (map[string]any, bool, error),
	write func(string, map[string]any) error,
) func(string) (bool, error) {
	return func(cfgPath string) (bool, error) {
		return removeServerEntry(cfgPath, serversKey, read, write)
	}
}

// Shared outFns for the clients that funnel through removeServerEntry, keyed
// by the servers map each client reads (see the registry in setup_clients.go).
var (
	removeMcpServersJSON = removeServerEntryFrom("mcpServers", readOrInitClaudeConfig, writeJSON)
	removeMcpJSON        = removeServerEntryFrom("mcp", readOrInitClaudeConfig, writeJSON)
	removeCodexTOML      = removeServerEntryFrom("mcp_servers", readOrInitCodexConfig, writeTOML)
	removeGooseYAML      = removeServerEntryFrom("extensions", readOrInitYAMLConfig, writeYAML)
	removeHermesYAML     = removeServerEntryFrom("mcp_servers", readOrInitYAMLConfig, writeYAML)
)

// setupZCodeOut removes plumb from ZCode's nested mcp.servers map. It mirrors
// setupZCodeInto's refusal to touch anything beyond the servers entry — the
// file also carries hooks and plugin state. An mcp.servers left empty is
// dropped (removeServerEntry's reasoning), but "mcp" itself stays: it is
// ZCode's own section, not one plumb introduced.
func setupZCodeOut(cfgPath string) (bool, error) {
	if _, err := os.Stat(cfgPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	cfg, _, err := readOrInitClaudeConfig(cfgPath)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", cfgPath, err)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		return false, nil
	}
	servers, ok := mcp["servers"].(map[string]any)
	if !ok {
		return false, nil
	}
	if _, ok := servers["plumb"]; !ok {
		return false, nil
	}
	if err := backupFile(cfgPath); err != nil {
		return false, fmt.Errorf("backing up %s: %w", cfgPath, err)
	}
	delete(servers, "plumb")
	if len(servers) == 0 {
		delete(mcp, "servers")
	}
	if err := writeJSON(cfgPath, cfg); err != nil {
		return false, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, nil
}

// setupDSHOut removes the mcp-plumb row from a DeepSeek Harness user patch
// layer. Within its insert entry only the plumb row goes — siblings survive —
// and the entry itself is dropped when plumb was its only content AND it
// carried nothing but the insert (an entry with other keys keeps them). Bare
// id-keyed mcp-plumb rows are inert (dsh skips them at boot) but still plumb's
// debris, so they go too. Everything else — comments, !!js expressions,
// unrelated entries — rides the node round-trip unchanged.
func setupDSHOut(cfgPath string) (bool, error) {
	if _, err := os.Stat(cfgPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return false, err
	}
	doc, err := parseDSHPatch(cfgPath, data)
	if err != nil {
		return false, err
	}
	list := doc.Content[0]

	insertEntry, row := findDSHPlumbInsert(list)
	bareRows := dshBarePlumbRows(list)
	if row == nil && len(bareRows) == 0 {
		return false, nil
	}
	if err := backupFile(cfgPath); err != nil {
		return false, fmt.Errorf("backing up %s: %w", cfgPath, err)
	}
	if row != nil {
		insertSeq := patchInsert(insertEntry)
		insertSeq.Content = withoutNodes(insertSeq.Content, []*yaml.Node{row})
		// A mapping node's Content holds key/value pairs, so len 2 is exactly
		// one key — an insert-only entry whose insert is now empty.
		if len(insertSeq.Content) == 0 && len(insertEntry.Content) == 2 {
			list.Content = withoutNodes(list.Content, []*yaml.Node{insertEntry})
		}
	}
	list.Content = withoutNodes(list.Content, bareRows)
	if err := writeDSHPatch(cfgPath, doc); err != nil {
		return false, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, nil
}

// findDSHPlumbInsert locates the insert entry carrying the active plumb row —
// the same active-form-only rule as findDSHPlumbRow — returning both the entry
// (so its sequence can be edited) and the row.
func findDSHPlumbInsert(list *yaml.Node) (insertEntry, row *yaml.Node) {
	for _, entry := range list.Content {
		insert := patchInsert(entry)
		if insert == nil {
			continue
		}
		for _, candidate := range insert.Content {
			if candidate.Kind == yaml.MappingNode && isDSHPlumbRow(candidate) {
				return entry, candidate
			}
		}
	}
	return nil, nil
}

// dshBarePlumbRows collects the inert top-level mcp-plumb rows — entries that
// are themselves plumb rows rather than carrying one inside an insert.
func dshBarePlumbRows(list *yaml.Node) []*yaml.Node {
	var rows []*yaml.Node
	for _, entry := range list.Content {
		if entry.Kind == yaml.MappingNode && isDSHPlumbRow(entry) {
			rows = append(rows, entry)
		}
	}
	return rows
}

// withoutNodes returns nodes minus drop, matching by identity.
func withoutNodes(nodes, drop []*yaml.Node) []*yaml.Node {
	kept := make([]*yaml.Node, 0, len(nodes))
	for _, n := range nodes {
		skip := false
		for _, d := range drop {
			if n == d {
				skip = true
				break
			}
		}
		if !skip {
			kept = append(kept, n)
		}
	}
	return kept
}

// setupAntigravityOut reverses setupAntigravityInto: the target's standalone
// mcp/plumb.json — a file plumb owns outright — is backed up and deleted.
// Legacy-era flat mcp_config.json files under ~/.gemini are no longer plumb's
// to manage (Antigravity no longer reads them), so an uninstall leaves them
// alone.
func setupAntigravityOut(cfgPath string) (bool, error) {
	return removeOwnedFile(cfgPath)
}

// removeOwnedFile backs up and deletes a file plumb owns outright (the
// Antigravity standalone configs). Absent is a no-op.
func removeOwnedFile(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := backupFile(path); err != nil {
		return false, fmt.Errorf("backing up %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("removing %s: %w", path, err)
	}
	return true, nil
}

// removePlumbSkills deletes the skill directories plumb's sync installed under
// dir — but only those still carrying plumb's provenance marker or plumb's
// exact content, so a directory the user rewrote into their own skill is left
// alone and reported instead. Every removed file is backed up first, matching
// the sync's own overwrite policy.
func removePlumbSkills(dir string) (removed, kept []string) {
	for _, s := range embeddedSkills() {
		skillDir := filepath.Join(dir, s.Name)
		data, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
		if err != nil {
			continue
		}
		_, marked := skillMarkerVersion(string(data))
		ours := stripSkillMarker(string(data)) == s.Content
		if !marked && !ours {
			kept = append(kept, s.Name)
			continue
		}
		if backupSkillDir(skillDir) != nil || os.RemoveAll(skillDir) != nil {
			// A skill that cannot be backed up or removed stays put and is
			// reported as kept — surviving is the safety property that matters;
			// the reason is diagnosable from the filesystem.
			kept = append(kept, s.Name)
			continue
		}
		removed = append(removed, s.Name)
	}
	return removed, kept
}

// backupSkillDir copies dir's contents to a sibling "<dir>.<timestamp>.bak"
// directory, so the backup survives the RemoveAll that follows — backupFile's
// per-file .bak convention would land inside the tree being deleted.
func backupSkillDir(dir string) error {
	stamp := time.Now().Format("20060102-150405")
	dst := dir + "." + stamp + ".bak"
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is inside the skills dir plumb manages
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600) //nolint:gosec // G304: target is derived from the skills dir plumb manages
	})
}
