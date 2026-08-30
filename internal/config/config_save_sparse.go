package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/pelletier/go-toml/v2"
)

// config_save_sparse.go — the SPARSE global save: persist only the keys a
// mutation actually changed, so a single-settings write never materialises
// compiled-in defaults (or active PLUMB_* env overrides) into config.toml as if
// the user had chosen them.
//
// Why this exists: Save re-encodes the WHOLE Config struct, so changing one
// global setting writes every field back — and an explicitly-written value
// out-ranks the compiled-in default forever after, which freezes any default
// plumb adds later (empty slices written as `= []`; non-empty ones pinned by
// value, like git.protected_branches and quality.analysers). omitempty cannot
// fix this: it only suppresses the empty case, and go-toml/v2 truncates a
// pre-populated slice on the first decoded [[command]] — only not writing the
// untouched keys can. The writer is a diff-based helper rather than a
// SetGlobalValue call at each settings row because the TUI's persist API is a
// whole-config mutation closure with no key path, and every settings row
// funnels through it.

// SaveSparse applies mutate to the loaded global config and persists ONLY the
// keys the mutation actually changed (added, altered, or removed), leaving
// every other key in the file — and every key it never had — untouched. It is
// the sparse counterpart of Save: the same load (compiled defaults + the
// on-disk file, WITHOUT the PLUMB_* env overlay), the same refusal to clobber
// an unparseable file, the same atomic write — but a single-settings edit no
// longer writes the rest of the struct. The config file (and its parent
// directory) is created on the first actual change; a mutation that changes
// nothing writes nothing.
func SaveSparse(mutate func(*Config)) error {
	if mutate == nil {
		return errors.New("global config: nil mutation")
	}
	cfg, err := loadForSave()
	if err != nil {
		return err
	}
	before, err := configTOMLMap(cfg)
	if err != nil {
		return fmt.Errorf("encoding config for sparse diff: %w", err)
	}
	mutate(&cfg)
	after, err := configTOMLMap(cfg)
	if err != nil {
		return fmt.Errorf("encoding config for sparse diff: %w", err)
	}
	sets, dels := diffTOMLMaps(before, after)
	if len(sets) == 0 && len(dels) == 0 {
		return nil // nothing changed — do not create or rewrite the file
	}
	cfgPath := GlobalConfigPath()
	if cfgPath == "" {
		return errors.New("writing config: no config path could be resolved")
	}
	m, err := loadGlobalRaw()
	if err != nil {
		return err
	}
	for _, p := range dels {
		deleteNested(m, p)
	}
	for _, s := range sets {
		if err := setNested(m, s.path, s.value); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	return writeTOMLAtomic(cfgPath, m)
}

// configTOMLMap round-trips cfg through the same encoder the config file uses,
// yielding the nested key map whose re-encode is byte-identical to what Save
// would write. Diffing these maps (instead of the structs directly) means the
// sparse write's values are exactly the TOML the full encode would have
// produced for the changed keys, with no field-to-key mapping to maintain.
func configTOMLMap(cfg Config) (map[string]any, error) {
	data, err := toml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// tomlSet is one leaf key a sparse write must set, with the round-tripped
// value to write.
type tomlSet struct {
	path  []string
	value any
}

// diffTOMLMaps compares two config-as-TOML-map trees and returns the leaf
// paths whose value changed (with the new value) and the paths that
// disappeared. A key present before and absent after — an omitempty field
// cleared to its zero value — must be deleted from the file, not silently left
// stale. Nested tables are walked key-by-key, so an edit inside one section
// writes only that leaf; a slice (including an array of tables) is one leaf.
func diffTOMLMaps(before, after map[string]any) (sets []tomlSet, dels [][]string) {
	diffTOML(nil, before, after, &sets, &dels)
	return sets, dels
}

func diffTOML(prefix []string, before, after map[string]any, sets *[]tomlSet, dels *[][]string) {
	for k, afterV := range after {
		path := append(slices.Clone(prefix), k)
		beforeV, ok := before[k]
		if !ok {
			*sets = append(*sets, tomlSet{path: path, value: afterV})
			continue
		}
		beforeTable, beforeIsTable := beforeV.(map[string]any)
		afterTable, afterIsTable := afterV.(map[string]any)
		if beforeIsTable && afterIsTable {
			diffTOML(path, beforeTable, afterTable, sets, dels)
			continue
		}
		if !reflect.DeepEqual(beforeV, afterV) {
			*sets = append(*sets, tomlSet{path: path, value: afterV})
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			*dels = append(*dels, append(slices.Clone(prefix), k))
		}
	}
}
