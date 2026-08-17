package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
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
		// Collapse fold variants in DOCUMENT order here, not in setNested: the raw
		// map does not carry the variants' source order, and collapsing only the
		// path being written lets duplicate variants elsewhere keep flipping on the
		// next unrelated write (PLAN-330).
		order, err := buildFoldOrder(data)
		if err != nil {
			return nil, fmt.Errorf("scanning project config %s: %w", path, err)
		}
		collapseFolds(m, order)
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
	if err := setNested(m, path, value); err != nil {
		return err
	}
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
// Tables are merged recursively; a scalar variant alongside a table is dropped
// in favour of the winner. The merge runs in sort.Strings order, so on its own
// it reproduces the decode of the file the write produces, not the value in
// force beforehand — LoadProjectRaw collapses in document order first, making
// this a single-variant no-op for project config, and leaving the sorted merge
// only for maps not collapsed at load (loadGlobalRaw, ApplyKeyToRaw).
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
// table. The exact spelling is covered by setNested, which refuses to descend
// through a segment that holds a non-table value rather than clobbering it.
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

// mergeValue merges next over prev the way go-toml merges two fold variants in
// document order: two tables merge key-by-key (next winning on conflict),
// anything else is replaced outright. It is foldMergeValue without the trailing
// collapse — collapseFolds drives that itself in document order, and folding
// here would merge in sort.Strings order, re-introducing the very bug this file
// fixes.
func mergeValue(prev, next any) any {
	prevTable, prevOK := prev.(map[string]any)
	nextTable, nextOK := next.(map[string]any)
	if !prevOK || !nextOK {
		return next
	}
	for k, v := range nextTable {
		if existing, ok := prevTable[k]; ok {
			prevTable[k] = mergeValue(existing, v)
			continue
		}
		prevTable[k] = v
	}
	return prevTable
}

// foldOrder records the document order of fold-variant key spellings at every
// level of the TOML document, so collapseFolds can merge variants the way the
// decoder does — last in DOCUMENT order wins — instead of the sort.Strings order
// the raw map would otherwise force. Keyed by strings.ToLower(name), matching
// foldKeys.
type foldOrder struct {
	variants map[string][]string // lowercased key -> spellings in first-appearance document order
	children map[string]*foldOrder
}

func newFoldOrder() *foldOrder {
	return &foldOrder{
		variants: map[string][]string{},
		children: map[string]*foldOrder{},
	}
}

// buildFoldOrder walks data with go-toml's unstable parser, recording the order
// each key spelling first appears at each level. LoadProjectRaw already read and
// decoded these exact bytes, so a successful decode guarantees this walk
// succeeds; the parser error path is still surfaced rather than silently
// skipping the collapse.
func buildFoldOrder(data []byte) (*foldOrder, error) {
	var p unstable.Parser
	p.Reset(data)
	root := newFoldOrder()
	var current []string // table context set by the most recent header
	for p.NextExpression() {
		expr := p.Expression()
		var path []string
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			path = exprKeyPath(expr)
			current = path
		case unstable.KeyValue:
			path = append(append([]string{}, current...), exprKeyPath(expr)...)
		default:
			continue
		}
		recordFoldOrder(root, path)
	}
	if err := p.Error(); err != nil {
		return nil, err
	}
	return root, nil
}

// exprKeyPath returns the decoded key segments of a Table, ArrayTable or
// KeyValue expression.
func exprKeyPath(expr *unstable.Node) []string {
	var segs []string
	for it := expr.Key(); it.Next(); {
		segs = append(segs, string(it.Node().Data))
	}
	return segs
}

// recordFoldOrder records path's segments in first-appearance document order at
// each level of the order tree.
func recordFoldOrder(root *foldOrder, path []string) {
	cur := root
	for _, seg := range path {
		lower := strings.ToLower(seg)
		if !slices.Contains(cur.variants[lower], seg) {
			cur.variants[lower] = append(cur.variants[lower], seg)
		}
		child, ok := cur.children[lower]
		if !ok {
			child = newFoldOrder()
			cur.children[lower] = child
		}
		cur = child
	}
}

// collapseFolds merges every fold variant of m in document order (last wins),
// recursively, so the surviving value is what the decoder chose from the
// original bytes — not what sort.Strings order would have chosen on re-marshal.
func collapseFolds(m map[string]any, order *foldOrder) {
	if order == nil {
		return
	}
	for _, spellings := range order.variants {
		var present []string
		for _, s := range spellings {
			if _, ok := m[s]; ok {
				present = append(present, s)
			}
		}
		if len(present) < 2 {
			continue
		}
		// Never merge array values: an array-of-tables ([[command]]) decodes to
		// []any just like a plain list, but the decoder APPENDS array-of-tables
		// across fold variants — replacing one here would silently drop entries.
		if hasArrayVariant(m, present) {
			continue
		}
		merged := m[present[0]]
		for _, k := range present[1:] {
			merged = mergeValue(merged, m[k])
		}
		for _, k := range present {
			delete(m, k)
		}
		m[present[0]] = merged
	}
	for k, v := range m {
		child, ok := v.(map[string]any)
		if !ok {
			continue
		}
		collapseFolds(child, order.children[strings.ToLower(k)])
	}
}

// hasArrayVariant reports whether any of present names an array value in m.
func hasArrayVariant(m map[string]any, present []string) bool {
	for _, k := range present {
		if _, ok := m[k].([]any); ok {
			return true
		}
	}
	return false
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
//
// It REFUSES to overwrite an intermediate segment that already holds a non-table
// value: writing `command.name` must not replace an exact-spelled `[[command]]`
// allow-list with an empty table, which would destroy the list and leave a
// config that fails validation.
func setNested(m map[string]any, path []string, value any) error {
	for _, k := range path[:len(path)-1] {
		key := foldCollapseTable(m, k)
		next, ok := m[key].(map[string]any)
		if !ok {
			if _, exists := m[key]; exists {
				return fmt.Errorf("cannot set %q: %q already exists and is not a table", strings.Join(path, "."), key)
			}
			next = map[string]any{}
			m[key] = next
		}
		m = next
	}
	leaf := path[len(path)-1]
	key := foldCollapse(m, leaf)
	m[key] = value
	return nil
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
