package tui

import (
	"fmt"
	"sort"
	"strings"
)

// keyAction is a stable, rebindable TUI action. Its string value is the name a
// user writes under [ui.keys] in the global config. This file is the single
// source of truth for the rebindable action set and their default keys.
type keyAction string

const (
	actQuit        keyAction = "quit"
	actNavUp       keyAction = "nav_up"
	actNavDown     keyAction = "nav_down"
	actPageUp      keyAction = "page_up"
	actPageDown    keyAction = "page_down"
	actSectionMenu keyAction = "section_menu"
	actPanelNext   keyAction = "panel_next"
	actPanelPrev   keyAction = "panel_prev"
	actRefresh     keyAction = "refresh"
	actHelp        keyAction = "help"
	actRename      keyAction = "rename"
	actFilter      keyAction = "filter"
)

// keyDefault pairs a rebindable action with its default key. The default key is
// exactly the string the switch-case handlers already match on, so an
// un-rebound action normalises to itself and the existing cases keep working
// with no change beyond the normalise call at the switch head.
type keyDefault struct {
	action keyAction
	key    string
}

// defaultKeymap is the canonical default binding for every rebindable action,
// in a stable order. The keys here MUST match the literal cases in the key
// handlers (model_keys.go, model_keys_logs.go, dashboard.go,
// model_settings_keys.go); TestKeymapDefaultsMatchHandlers guards that.
var defaultKeymap = []keyDefault{
	{actQuit, "ctrl+q"},
	{actNavUp, "up"},
	{actNavDown, "down"},
	{actPageUp, "pgup"},
	{actPageDown, "pgdown"},
	{actSectionMenu, "/"},
	{actPanelNext, "tab"},
	{actPanelPrev, "shift+tab"},
	{actRefresh, "a"},
	{actHelp, "ctrl+h"},
	{actRename, "r"},
	{actFilter, "f"},
}

// reservedKeys are keys wired to fixed, non-rebindable behaviours. A rebinding
// that targets one is reported and ignored, so the user cannot silently shadow
// interrupt/close/select, the vim-style nav aliases, the section shortcuts, or
// the always-on theme picker. The value is a short reason shown in the warning.
var reservedKeys = map[string]string{
	"ctrl+c":    "interrupt / quit confirmation",
	"esc":       "back / close",
	"enter":     "select / open",
	" ":         "toggle / activate",
	"j":         "nav down (vim alias)",
	"k":         "nav up (vim alias)",
	"[":         "resize column",
	"]":         "resize column",
	"ctrl+t":    "theme picker",
	"c":         "copy to clipboard",
	"G":         "jump to latest (logs)",
	"backspace": "edit filter",
	"ctrl+1":    "jump to section",
	"ctrl+2":    "jump to section",
	"ctrl+3":    "jump to section",
	"ctrl+4":    "jump to section",
	"ctrl+5":    "jump to section",
	"alt+1":     "jump to section",
	"alt+2":     "jump to section",
	"alt+3":     "jump to section",
	"alt+4":     "jump to section",
	"alt+5":     "jump to section",
}

// keymap is a resolved, immutable binding table built once at TUI start. It is
// consulted at each key-handler switch head via normalise, which rewrites the
// pressed key into the canonical default key of whatever action it is now bound
// to, so the existing string cases dispatch unchanged. A default key displaced
// by a rebinding (its action moved elsewhere and nothing took its place) is
// rewritten to "" so it dispatches nothing — deterministic, per the design.
//
// Concurrency: read-only after resolveKeymap returns.
type keymap struct {
	owner    map[string]keyAction // pressed key -> action it is bound to
	canon    map[keyAction]string // action -> canonical default key
	resolved map[keyAction]string // action -> resolved (possibly rebound) key
}

// defaultKeys returns the resolved keymap with no overrides applied.
func defaultKeys() keymap {
	km, _ := resolveKeymap(nil)
	return km
}

// resolveKeymap builds the binding table from [ui.keys] overrides (action name
// -> key). It returns the resolved keymap plus a list of human-readable startup
// warnings for unknown actions, empty keys, collisions with a fixed key, and
// two actions colliding on one key (deterministic: overrides are applied in
// action-name order and the later one is ignored). Defaults fill every action
// left unconfigured.
func resolveKeymap(overrides map[string]string) (keymap, []string) {
	canon := make(map[keyAction]string, len(defaultKeymap))
	resolved := make(map[keyAction]string, len(defaultKeymap))
	for _, d := range defaultKeymap {
		canon[d.action] = d.key
		resolved[d.action] = d.key
	}

	explicit := make(map[keyAction]bool)
	var warnings []string
	for _, name := range sortedOverrideNames(overrides) {
		key := strings.TrimSpace(overrides[name])
		act := keyAction(name)
		if _, ok := canon[act]; !ok {
			warnings = append(warnings, fmt.Sprintf("[ui.keys] unknown action %q ignored (valid actions: %s)", name, actionList()))
			continue
		}
		if key == "" {
			warnings = append(warnings, fmt.Sprintf("[ui.keys] %s has an empty key; keeping the default %q", name, canon[act]))
			continue
		}
		if reason, ok := reservedKeys[key]; ok {
			warnings = append(warnings, fmt.Sprintf("[ui.keys] %s = %q collides with a fixed key (%s); binding ignored", name, key, reason))
			continue
		}
		if other, ok := explicitOwnerOf(resolved, explicit, key); ok && other != act {
			warnings = append(warnings, fmt.Sprintf("[ui.keys] %s and %s are both bound to %q; keeping %s, ignoring %s", other, name, key, other, name))
			continue
		}
		resolved[act] = key
		explicit[act] = true
	}

	owner, shadowWarnings := buildOwners(resolved, explicit)
	warnings = append(warnings, shadowWarnings...)
	return keymap{owner: owner, canon: canon, resolved: resolved}, warnings
}

// buildOwners assigns each key an owning action, letting an explicit override
// win over another action's passive default and reporting the shadowed default.
func buildOwners(resolved map[keyAction]string, explicit map[keyAction]bool) (map[string]keyAction, []string) {
	owner := make(map[string]keyAction, len(defaultKeymap))
	// Passive defaults first (in stable order): only actions not explicitly
	// rebound keep their default key as an owner.
	for _, d := range defaultKeymap {
		if !explicit[d.action] {
			owner[resolved[d.action]] = d.action
		}
	}
	// Explicit overrides second, winning any tie against a passive default.
	var warnings []string
	for _, d := range defaultKeymap {
		if !explicit[d.action] {
			continue
		}
		key := resolved[d.action]
		if prev, ok := owner[key]; ok && prev != d.action {
			warnings = append(warnings, fmt.Sprintf("[ui.keys] %s = %q shadows the default binding of %s (%s left with no key)", d.action, key, prev, prev))
		}
		owner[key] = d.action
	}
	return owner, warnings
}

// normalise rewrites a pressed key into the canonical default key of the action
// it is bound to, so the existing switch-case handlers dispatch unchanged. A
// key that is some action's default but is no longer owned (displaced by a
// rebinding) returns "" and dispatches nothing; any other key passes through.
func (k keymap) normalise(key string) string {
	if act, ok := k.owner[key]; ok {
		return k.canon[act]
	}
	if k.isDefaultKey(key) {
		return ""
	}
	return key
}

// isDefaultKey reports whether key is the default binding of some action.
func (k keymap) isDefaultKey(key string) bool {
	for _, c := range k.canon {
		if c == key {
			return true
		}
	}
	return false
}

// display returns the resolved key for an action, for footer/help rendering.
// A zero-value keymap (constructed outside resolveKeymap, e.g. in tests) falls
// back to the compiled default so the UI still shows a sensible binding.
func (k keymap) display(action keyAction) string {
	if key, ok := k.resolved[action]; ok {
		return key
	}
	for _, d := range defaultKeymap {
		if d.action == action {
			return d.key
		}
	}
	return ""
}

// explicitOwnerOf finds the action whose explicitly-set key equals key.
func explicitOwnerOf(resolved map[keyAction]string, explicit map[keyAction]bool, key string) (keyAction, bool) {
	for _, d := range defaultKeymap {
		if explicit[d.action] && resolved[d.action] == key {
			return d.action, true
		}
	}
	return "", false
}

// sortedOverrideNames returns the override action names in a stable order, so a
// duplicate-key collision deterministically ignores the later (alphabetically
// greater) action.
func sortedOverrideNames(overrides map[string]string) []string {
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// actionList is the comma-separated set of valid action names, for warnings.
func actionList() string {
	names := make([]string, 0, len(defaultKeymap))
	for _, d := range defaultKeymap {
		names = append(names, string(d.action))
	}
	return strings.Join(names, ", ")
}
