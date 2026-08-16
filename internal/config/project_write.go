package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// project_write.go implements SPARSE writes to a workspace's project config
// (<workspace>/.plumb/config.toml). Unlike Save (which re-encodes the whole
// resolved Config struct), these helpers touch only the single key the user
// changed, so a per-project override never silently shadows a global value the
// user did not set: a key absent from the file falls through to global/default
// (the "inherit" state), a key present is an explicit override.

// LoadProjectRaw reads the project config into a nested map of only the keys the
// project explicitly sets. Returns an empty (non-nil) map when the file is
// absent. This is the source of truth for the TUI's "overridden vs inherited"
// distinction — drive the inherit annotation off whether a key is present here.
func LoadProjectRaw(workspace string) (map[string]any, error) {
	m := map[string]any{}
	path := ProjectConfigPath(workspace)
	if path == "" {
		return m, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := toml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parsing project config %s: %w", path, err)
		}
	case os.IsNotExist(err):
		// absent → empty map (no overrides)
	default:
		return nil, fmt.Errorf("reading project config %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// ProjectValuePresent reports whether the dotted key path is explicitly set in
// the workspace's project config (i.e. it is an override, not inherited).
// Keys are matched case-insensitively (lookupNested → foldLookup), mirroring
// how go-toml/v2 binds a TOML key to a struct field, so a project supplying
// `[[COMMAND]]` or `[TASKS.go]` reports present under the lowercase path —
// the exact-match miss here was the mechanism of two arbitrary-code-execution
// bypasses (run_command, run_task), closed by deriving provenance from the
// trust spec instead (issue #319 folded the lookup itself; the spec remains
// the provenance source).
//
// STILL never use this to decide whether a security gate applies: the trust
// spec (ProjectPolicyStatus.Asked, ProjectTaskCommands) is computed from the
// same bytes as the loaded config at apply time, so it cannot disagree with
// what actually loaded — a presence check re-reads the file and can. It
// remains the right tool for the TUI and web settings screens' "overridden
// vs inherited" annotation.
func ProjectValuePresent(workspace string, path []string) (bool, error) {
	m, err := LoadProjectRaw(workspace)
	if err != nil {
		return false, err
	}
	return lookupNested(m, path), nil
}

// SetProjectValue writes value at the dotted TOML key path in the project
// config, creating <workspace>/.plumb/ and config.toml on first use. Only the
// touched key is written — the file stays sparse.
func SetProjectValue(workspace string, path []string, value any) error {
	if len(path) == 0 {
		return errors.New("project config: empty key path")
	}
	cfgPath := ProjectConfigPath(workspace)
	if cfgPath == "" {
		return errors.New("project config: no workspace path")
	}
	m, err := LoadProjectRaw(workspace)
	if err != nil {
		return err
	}
	setNested(m, path, value)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fmt.Errorf("creating .plumb dir: %w", err)
	}
	return writeTOMLAtomic(cfgPath, m)
}

// UnsetProjectValue removes the dotted key path from the project config (the
// "inherit" state — the key falls through to global/default). Tables it leaves
// empty are pruned; when the whole file becomes empty it is removed.
func UnsetProjectValue(workspace string, path []string) error {
	if len(path) == 0 {
		return nil
	}
	cfgPath := ProjectConfigPath(workspace)
	if cfgPath == "" {
		return errors.New("project config: no workspace path")
	}
	m, err := LoadProjectRaw(workspace)
	if err != nil {
		return err
	}
	deleteNested(m, path)
	if len(m) == 0 {
		if rmErr := os.Remove(cfgPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("removing empty project config: %w", rmErr)
		}
		return nil
	}
	return writeTOMLAtomic(cfgPath, m)
}

// foldKeys returns every key in m matching want case-insensitively, in a
// DETERMINISTIC order: the exact spelling first, then the rest sorted. TOML
// keys are case-sensitive, so one file can legitimately hold several variants
// of one setting (and plumb's own pre-#319 sparse writer produced exactly that
// by growing a second table); go-toml decodes them all into one struct field,
// with the last in document order winning. Ranging a map directly here would
// pick a variant at random on every call — Go randomises map iteration — so a
// write could land in a different table run to run.
func foldKeys(m map[string]any, want string) []string {
	// strings.ToLower, NOT strings.EqualFold: go-toml/v2 binds a TOML key to a
	// struct field through a map keyed by strings.ToLower(name) (unmarshaler.go
	// byFold), and the two rules disagree in both directions on non-ASCII keys.
	// EqualFold("ſtrict", "strict") is true where the decoder sees two distinct
	// keys — folding there DELETES a key the user still wants — and
	// EqualFold("wrİtes", "writes") is false where the decoder binds them, which
	// is #319's own defect left open. Matching the decoder is the whole point.
	lower := strings.ToLower(want)
	var rest []string
	for k := range m {
		if k == want || strings.ToLower(k) != lower {
			continue
		}
		rest = append(rest, k)
	}
	sort.Strings(rest)
	if _, ok := m[want]; ok {
		return append([]string{want}, rest...)
	}
	return rest
}

// foldLookup returns the key in m that matches want the way go-toml/v2 binds a
// TOML key to a struct field (see foldKeys), so `TASKS` and `tasks` are the
// same setting — preferring the exact spelling when both appear.
// ok is false when no fold variant exists.
func foldLookup(m map[string]any, want string) (key string, ok bool) {
	keys := foldKeys(m, want)
	if len(keys) == 0 {
		return "", false
	}
	return keys[0], true
}

// foldDelete removes every key in m that matches want case-insensitively. The
// raw map can hold two fold variants of one setting as distinct keys (TOML
// keys are case-sensitive), and both decode into the same merged value — so
// "unset this setting" must remove both or lookup would still find it.
func foldDelete(m map[string]any, want string) {
	for _, k := range foldKeys(m, want) {
		delete(m, k)
	}
}

// foldCollapse merges every fold variant of want in m down to a single key and
// returns it, so that after a sparse write exactly one spelling of the setting
// survives. Without it a write can be silently overridden: two variants both
// decode into one field and the LAST one wins, so updating `[GIT]` in a file
// that also says `[git]` would store a value the decoder then discards.
//
// Precedence follows that same last-wins rule, applied to the order plumb will
// re-marshal the file in (go-toml sorts map keys), so the surviving value is
// the one the decoder chooses from the file this write produces. Tables are
// merged recursively; a scalar variant alongside a table is dropped in favour
// of the winner.
//
// What it does NOT promise: that the value in force BEFORE the write survives
// it. That one comes from the variants' order in the source document, which the
// raw map does not carry, so when document order and marshal order disagree a
// write to one key can change another. Pre-existing — re-marshalling reorders
// the variants regardless — and tracked as its own change rather than smuggled
// in here, because fixing it means collapsing in document order at load.
func foldCollapse(m map[string]any, want string) string {
	keys := foldKeys(m, want)
	if len(keys) == 0 {
		return want
	}
	canonical := keys[0]
	if len(keys) == 1 {
		return canonical
	}
	// Merge in marshal order so the last-wins decode is reproduced, then store
	// the result under the canonical spelling and drop the other variants.
	ordered := append([]string(nil), keys...)
	sort.Strings(ordered)
	merged := m[ordered[0]]
	for _, k := range ordered[1:] {
		merged = foldMergeValue(merged, m[k])
	}
	for _, k := range keys {
		delete(m, k)
	}
	m[canonical] = merged
	return canonical
}

// foldCollapseTable is foldCollapse restricted to variants that actually hold a
// TABLE, for the intermediate segments of a write — the only ones that need to
// be descended into. A variant holding something else (most importantly an
// array of tables: `[[command]]` decodes to []any, not map[string]any) is left
// strictly alone and its spelling is not reused, so a sparse write of
// `command.name` cannot replace a VARIANT-spelled allow-list with an empty
// table. The exact spelling is not covered: setNested still writes over a
// `[[command]]` array under the very key it was asked for, which destroys the
// allow-list and leaves a file that fails validation. That needs setNested to
// refuse rather than clobber — an error path it has not got — and no live
// caller reaches it, so it is filed rather than fixed here.
//
// Such a file is already incoherent — `command` cannot be both an array and a
// table — and the caller's own spelling is where the new table goes, matching
// what the pre-#319 code did: adding a key beats deleting one the user never
// asked us to touch.
func foldCollapseTable(m map[string]any, want string) string {
	var tables []string
	for _, k := range foldKeys(m, want) {
		if _, ok := m[k].(map[string]any); ok {
			tables = append(tables, k)
		}
	}
	switch len(tables) {
	case 0:
		return want
	case 1:
		return tables[0]
	}
	canonical := tables[0]
	ordered := append([]string(nil), tables...)
	sort.Strings(ordered)
	merged := m[ordered[0]]
	for _, k := range ordered[1:] {
		merged = foldMergeValue(merged, m[k])
	}
	for _, k := range tables {
		delete(m, k)
	}
	m[canonical] = merged
	return canonical
}

// foldMergeValue merges next over prev with the same semantics go-toml applies
// to two fold variants: two tables merge key-by-key (next winning, and its own
// duplicate keys collapsed in turn), anything else is replaced outright.
func foldMergeValue(prev, next any) any {
	prevTable, prevOK := prev.(map[string]any)
	nextTable, nextOK := next.(map[string]any)
	if !prevOK || !nextOK {
		return next
	}
	for k, v := range nextTable {
		if existing, ok := prevTable[k]; ok {
			prevTable[k] = foldMergeValue(existing, v)
			continue
		}
		prevTable[k] = v
	}
	for _, k := range tableKeys(prevTable) {
		foldCollapse(prevTable, k)
	}
	return prevTable
}

// tableKeys returns m's keys in sorted order, so a collapse pass over a table
// does not itself depend on map iteration order.
func tableKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lookupNested reports whether path resolves to a present leaf in m. Keys are
// matched case-insensitively, mirroring how go-toml/v2 decodes the same file
// into the typed Config: a fold-variant spelling is present in the effective
// config, so it must be present here too.
func lookupNested(m map[string]any, path []string) bool {
	if len(path) == 1 {
		_, ok := foldLookup(m, path[0])
		return ok
	}
	// Every fold variant of this segment is a place the setting can live, and
	// the decoder merges them all — so presence means present under ANY of
	// them, not just the one foldLookup happens to prefer.
	for _, key := range foldKeys(m, path[0]) {
		next, ok := m[key].(map[string]any)
		if !ok {
			continue
		}
		if lookupNested(next, path[1:]) {
			return true
		}
	}
	return false
}

// setNested sets value at path within m, creating intermediate tables. A
// fold-variant spelling of any path segment is written THROUGH (the existing
// key is updated in place) rather than duplicated — writing `git.allow_writes`
// into a file that says [GIT] must not leave two tables that both decode into
// Config.Git.
func setNested(m map[string]any, path []string, value any) {
	for _, k := range path[:len(path)-1] {
		key := foldCollapseTable(m, k)
		next, ok := m[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[key] = next
		}
		m = next
	}
	leaf := path[len(path)-1]
	key := foldCollapse(m, leaf)
	m[key] = value
}

// deleteNested removes path from m and prunes any table it leaves empty. Keys
// are matched case-insensitively, so unsetting a lowercase path removes a
// fold-variant spelling of the same setting.
func deleteNested(m map[string]any, path []string) {
	if len(path) == 1 {
		foldDelete(m, path[0])
		return
	}
	// Descend through EVERY fold variant of this segment: the setting decodes
	// out of all of them, so removing it from only the preferred spelling
	// would leave `[GIT]` holding a key the user just unset via `[git]`.
	for _, key := range foldKeys(m, path[0]) {
		child, ok := m[key].(map[string]any)
		if !ok {
			continue
		}
		deleteNested(child, path[1:])
		if len(child) == 0 {
			delete(m, key)
		}
	}
}
