package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// This file is `plumb hooks`'s client registry — the lifecycle-hook counterpart
// of setup_clients.go's setupTarget. A client differs only in where its hook
// config lives, which handlers plumb wants installed there, and how a handler
// plumb wrote is recognised again afterwards; the merge, the removal and the
// status classification are shared, so supporting another client is a registry
// row rather than a new code path.
//
// Both supported clients happen to share one container shape:
//
//	{"hooks": {"<Event>": [ {"hooks": [ <handler>, … ]} ]}}
//
// Claude Code's settings.json carries much else besides and Codex's hooks.json
// is a file of its own, but the sub-tree plumb touches is identical — and every
// writer here edits ONLY the handlers it owns inside it. A user's own hooks, on
// the same events, in the same file, survive an install, a refresh and an
// uninstall untouched. That is the property that made this a design task rather
// than a file write, and it is what every test in hooks_clients_test.go pins.

const (
	hookStateInstalled = "installed"
	hookStateMissing   = "missing"
	hookStateStale     = "stale"
)

// The hidden verbs the installed hooks run. They double as the ownership
// marker: see plumbHookCommand.
const (
	claudeHookVerb = "hooks run-claude"
	codexHookVerb  = "hooks run-codex"
)

// hookEntry is one handler plumb wants installed for a client: the event in
// that client's own vocabulary, a short label for the status table, and the
// rendered handler object.
type hookEntry struct {
	event   string
	label   string
	handler map[string]any
}

// hooksTarget describes one `plumb hooks <verb> [client]` row.
//
// setup names the MCP registration these hooks depend on: hooks install only
// where plumb is registered, because session linkage is context with no
// receiver and a mailbox probe could only point the agent at a tool surface its
// client cannot reach. ours is the ownership test — the one thing an uninstall
// must get exactly right.
type hooksTarget struct {
	use     string
	name    string
	setup   setupTarget
	pathFn  func() (string, error)
	entries func(plumbBin string) []hookEntry
	ours    func(handler map[string]any) bool
	notes   []string
}

// hooksTargets is the display order for every command that walks clients. It is
// a function rather than a package var so it cannot depend on the
// initialisation order of the setupTarget vars it embeds.
func hooksTargets() []hooksTarget {
	return []hooksTarget{claudeCodeHooksTarget, codexHooksTarget}
}

var claudeCodeHooksTarget = hooksTarget{
	use:     "claude-code",
	name:    "Claude Code",
	setup:   claudeCodeTarget,
	pathFn:  claudeSettingsPath,
	entries: claudeHookEntries,
	ours:    claudeHookOwned,
	notes: []string{
		"Claude Code hot-reloads settings.json, so sessions already running pick these up without a restart.",
		"The Stop hook stays silent and costs nothing unless `plumb mail` reports unread peer messages for that session.",
	},
}

var codexHooksTarget = hooksTarget{
	use:     "codex",
	name:    "Codex",
	setup:   codexTarget,
	pathFn:  codexHooksPath,
	entries: codexHookEntries,
	ours:    codexHookOwned,
	notes: []string{
		"Use /hooks in Codex to review and trust the two Plumb command hooks.",
		"The Stop hook checks only as a turn ends; Codex cannot wake an already-idle session.",
	},
}

// findHooksTarget resolves a command-line client name against the registry.
func findHooksTarget(name string) (hooksTarget, bool) {
	for _, t := range hooksTargets() {
		if strings.EqualFold(t.use, name) || strings.EqualFold(t.name, name) {
			return t, true
		}
	}
	return hooksTarget{}, false
}

func hooksClientNames() string {
	names := make([]string, 0, len(hooksTargets()))
	for _, t := range hooksTargets() {
		names = append(names, t.use)
	}
	return strings.Join(names, ", ")
}

// claudeSettingsPath returns the user-scoped Claude Code settings file. Hooks
// live in ~/.claude/settings.json, while the MCP registration lives in
// ~/.claude.json (claudeCodeConfigPath): two files, two purposes, and an
// installer that confused them would write hooks the client never reads.
func claudeSettingsPath() (string, error) {
	return homeRelConfigPath(".claude", "settings.json")
}

// codexHooksPath follows CodexConfigPath's CODEX_HOME precedence, but hooks
// have their own JSON file so one representation per config layer remains
// possible.
func codexHooksPath() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "hooks.json"), nil
	}
	return homeRelConfigPath(".codex", "hooks.json")
}

// plumbHookCommand renders the command a hook handler runs.
//
// The command string is also the ownership marker, deliberately: it is stable
// across binary paths and upgrades (the verb never moves), it needs no key of
// plumb's own invention inside a schema plumb does not own, and it cannot be
// confused with a user's hook that happens to carry a familiar label. The
// binary is quoted because these command lines are handed to a shell.
func plumbHookCommand(plumbBin, verb string) string {
	return strconv.Quote(plumbBin) + " " + verb
}

// legacyClaudeHookScripts are the two hand-written shell scripts plumb's own
// documentation told users to create before this command existed
// (skills/plumb-chat/references/idle-agent-wake-hook.md). Their settings.json
// entries count as plumb's, so an install REPLACES them in place — leaving one
// pair of hooks rather than two that both fire — and an uninstall clears them.
// Only the settings entry is touched: the .sh files on disk are the user's and
// are never removed.
var legacyClaudeHookScripts = []string{"plumb-session-link.sh", "plumb-mail-wake.sh"}

func claudeHookOwned(h map[string]any) bool {
	cmd, _ := h["command"].(string)
	if strings.Contains(cmd, claudeHookVerb) {
		return true
	}
	for _, script := range legacyClaudeHookScripts {
		if strings.Contains(cmd, script) {
			return true
		}
	}
	return false
}

// codexHookOwned matches the command first and the statusMessage second: the
// first Codex installer marked its entries by status line, so hooks it wrote
// are still recognised — and therefore refreshed and removable — by this one.
func codexHookOwned(h map[string]any) bool {
	if cmd, _ := h["command"].(string); strings.Contains(cmd, codexHookVerb) {
		return true
	}
	switch h["statusMessage"] {
	case codexSessionHookStatus, codexMailboxHookStatus:
		return true
	}
	return false
}

// readHookConfig reads a client's hook config. An absent file is an empty
// config (isNew), and an unparseable one is an error: plumb will not overwrite
// JSON it cannot read back.
func readHookConfig(path string) (cfg map[string]any, isNew bool, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path comes from the client registry, not from input
	if os.IsNotExist(err) {
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	cfg = map[string]any{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, false, fmt.Errorf("parsing %s as JSON: %w — will not overwrite", path, err)
		}
	}
	return cfg, false, nil
}

// hookMap returns cfg's "hooks" object, or a fresh one. A "hooks" key that is
// not an object is an error rather than something to replace: plumb did not
// write it, so plumb does not get to redefine it.
func hookMap(cfg map[string]any) (map[string]any, error) {
	if existing, ok := cfg["hooks"]; ok {
		hooks, ok := existing.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("hooks must be an object, got %T", existing)
		}
		return hooks, nil
	}
	return map[string]any{}, nil
}

// installHooksAt merges entries into path, returning whether anything changed.
// Re-running is a no-op once the file matches, which is what makes this safe to
// call from `plumb setup` flows and from a shell loop alike.
func installHooksAt(path string, entries []hookEntry, ours func(map[string]any) bool) (bool, error) {
	cfg, isNew, err := readHookConfig(path)
	if err != nil {
		return false, err
	}
	hooks, err := hookMap(cfg)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}

	changed := false
	for _, e := range entries {
		did, err := upsertHook(hooks, e, ours)
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		changed = changed || did
	}
	if !changed {
		return false, nil
	}

	cfg["hooks"] = hooks
	if isNew {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
		}
	} else if err := backupFile(path); err != nil {
		return false, fmt.Errorf("backing up %s: %w", path, err)
	}
	if err := writeJSON(path, cfg); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, nil
}

// upsertHook installs one entry into the hooks map in memory.
//
// An existing plumb handler on that event is REPLACED where it stands: that is
// how a re-run refreshes the binary path after an upgrade, and how a legacy
// script entry migrates to the built-in verb without the user's other handlers
// changing position. An event whose value is not an array of groups is refused
// rather than overwritten — a shape plumb cannot have written is a shape plumb
// must not silently redefine.
func upsertHook(hooks map[string]any, e hookEntry, ours func(map[string]any) bool) (bool, error) {
	existing, ok := hooks[e.event]
	if !ok {
		hooks[e.event] = []any{map[string]any{"hooks": []any{e.handler}}}
		return true, nil
	}
	groups, ok := existing.([]any)
	if !ok {
		return false, fmt.Errorf("%s must be an array of hook groups, got %T — refusing to change it", e.event, existing)
	}
	for _, groupAny := range groups {
		group, ok := groupAny.(map[string]any)
		if !ok {
			continue
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		for i, handlerAny := range handlers {
			handler, ok := handlerAny.(map[string]any)
			if !ok || !ours(handler) {
				continue
			}
			if reflect.DeepEqual(handler, e.handler) {
				return false, nil
			}
			handlers[i] = e.handler
			group["hooks"] = handlers
			return true, nil
		}
	}
	hooks[e.event] = append(groups, map[string]any{"hooks": []any{e.handler}})
	return true, nil
}

// removeHooksAt takes plumb's handlers back out of path and reports how many
// went. It removes every handler ours matches on every event — including one
// left behind by an older plumb that installed a hook this version no longer
// writes — so an uninstall cannot leave orphans pointing at a binary that is
// about to stop being registered.
//
// A group left with no handlers goes with them (a group matches nothing once
// it is empty, and plumb only ever creates single-handler groups of its own); a
// hooks map left empty loses its key entirely, restoring the pre-plumb shape of
// a file plumb created. Nothing is written when nothing matched.
func removeHooksAt(path string, ours func(map[string]any) bool) (int, error) {
	cfg, isNew, err := readHookConfig(path)
	if err != nil || isNew {
		return 0, err
	}
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		return 0, nil
	}

	removed := 0
	for event, value := range hooks {
		groups, ok := value.([]any)
		if !ok {
			continue
		}
		keptGroups, n := groupsWithout(groups, ours)
		removed += n
		if len(keptGroups) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = keptGroups
	}
	if removed == 0 {
		return 0, nil
	}

	if len(hooks) == 0 {
		delete(cfg, "hooks")
	} else {
		cfg["hooks"] = hooks
	}
	if err := backupFile(path); err != nil {
		return 0, fmt.Errorf("backing up %s: %w", path, err)
	}
	// A config left holding nothing at all was a file plumb created for its own
	// hooks (Codex's hooks.json is the clear case): removing it restores the
	// pre-plumb shape rather than leaving an empty object behind — the same call
	// `plumb setup --uninstall` makes about a server map it emptied. The backup
	// above means the removal is still recoverable.
	if len(cfg) == 0 {
		if err := os.Remove(path); err != nil {
			return 0, fmt.Errorf("removing %s: %w", path, err)
		}
		return removed, nil
	}
	if err := writeJSON(path, cfg); err != nil {
		return 0, fmt.Errorf("writing %s: %w", path, err)
	}
	return removed, nil
}

// groupsWithout filters one event's hook groups, returning the groups to keep
// and how many handlers went. A group whose handlers were all plumb's is
// dropped: an empty group matches nothing, and plumb only ever creates
// single-handler groups of its own. Anything this cannot read as a group — a
// shape plumb did not write — is kept exactly as it is.
func groupsWithout(groups []any, ours func(map[string]any) bool) ([]any, int) {
	removed := 0
	keptGroups := make([]any, 0, len(groups))
	for _, groupAny := range groups {
		group, isGroup := groupAny.(map[string]any)
		if !isGroup {
			keptGroups = append(keptGroups, groupAny)
			continue
		}
		handlers, hasHandlers := group["hooks"].([]any)
		if !hasHandlers {
			keptGroups = append(keptGroups, groupAny)
			continue
		}
		kept := make([]any, 0, len(handlers))
		for _, handlerAny := range handlers {
			if handler, isHandler := handlerAny.(map[string]any); isHandler && ours(handler) {
				removed++
				continue
			}
			kept = append(kept, handlerAny)
		}
		if len(kept) == 0 {
			continue
		}
		group["hooks"] = kept
		keptGroups = append(keptGroups, group)
	}
	return keptGroups, removed
}

// hookState is one entry's classification for the status table and for the
// per-hook action lines the writers print.
type hookState struct {
	entry  hookEntry
	state  string
	detail string
}

// hookStatesAt classifies every entry plumb would install against what is on
// disk: missing (nothing of plumb's on that event), installed (byte-identical
// to what this binary writes), or stale (plumb's, but written by a different
// binary path or an older entry shape — including a legacy script hook).
func hookStatesAt(path string, entries []hookEntry, ours func(map[string]any) bool) ([]hookState, error) {
	cfg, isNew, err := readHookConfig(path)
	if err != nil {
		return nil, err
	}
	var hooks map[string]any
	if !isNew {
		hooks, _ = cfg["hooks"].(map[string]any)
	}

	out := make([]hookState, 0, len(entries))
	for _, e := range entries {
		found := findHookHandler(hooks, e.event, ours)
		switch {
		case found == nil:
			out = append(out, hookState{entry: e, state: hookStateMissing})
		case reflect.DeepEqual(found, e.handler):
			out = append(out, hookState{entry: e, state: hookStateInstalled})
		default:
			out = append(out, hookState{entry: e, state: hookStateStale, detail: staleHookDetail(found, e.handler)})
		}
	}
	return out, nil
}

// findHookHandler returns plumb's handler on one event, or nil. A nil hooks map
// (no config, or no "hooks" key) simply finds nothing.
func findHookHandler(hooks map[string]any, event string, ours func(map[string]any) bool) map[string]any {
	groups, ok := hooks[event].([]any)
	if !ok {
		return nil
	}
	for _, groupAny := range groups {
		group, ok := groupAny.(map[string]any)
		if !ok {
			continue
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		for _, handlerAny := range handlers {
			if handler, ok := handlerAny.(map[string]any); ok && ours(handler) {
				return handler
			}
		}
	}
	return nil
}

// staleHookDetail says why an installed entry is not the one this binary would
// write. A different command is the case that matters and the one a reader can
// act on — a moved binary, or a legacy script hook awaiting migration — so it
// is quoted verbatim rather than summarised.
func staleHookDetail(found, want map[string]any) string {
	foundCmd, _ := found["command"].(string)
	wantCmd, _ := want["command"].(string)
	if foundCmd != wantCmd {
		return "runs " + foundCmd
	}
	return "entry differs from this binary's — install refreshes it"
}
