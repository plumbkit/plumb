package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

// agent_write.go applies a batch of agent config writes ATOMICALLY: the whole
// batch is validated against a candidate merged config first, and only on
// success is it written — in a single rewrite of the sparse project file, so a
// bad batch is a pure no-op and a partial file never exists. Every written key
// is stamped in the provenance sidecar.

// AgentApplyBatch validates pairs (every key must be agent-writable and pass the
// whole-config validation merged onto base) and, on success, writes them to the
// workspace's project config in one atomic rewrite, stamping provenance. Returns
// the sorted list of changed keys.
func AgentApplyBatch(base Config, workspace string, pairs map[string]any, prov ProvenanceEntry) ([]string, error) {
	if workspace == "" {
		return nil, errors.New("agent config: no workspace")
	}
	if len(pairs) == 0 {
		return nil, errors.New("agent config: empty batch")
	}
	keys := sortedKeys(pairs)
	// Fail closed before touching anything: every key must be on the allowlist.
	for _, k := range keys {
		if !IsAgentWritable(k) {
			return nil, fmt.Errorf("agent config: %q is not an agent-writable key", k)
		}
	}
	raw, err := LoadProjectRaw(workspace)
	if err != nil {
		return nil, err
	}
	prior := priorProjectValues(raw, keys) // capture before staging mutates raw
	for _, k := range keys {
		if err := ApplyKeyToRaw(raw, k, pairs[k]); err != nil {
			return nil, err
		}
	}
	if err := validateStaged(base, raw); err != nil {
		return nil, err
	}
	cfgPath := ProjectConfigPath(workspace)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return nil, fmt.Errorf("creating .plumb dir: %w", err)
	}
	if err := writeTOMLAtomic(cfgPath, raw); err != nil {
		return nil, err
	}
	for _, k := range keys {
		e := prov
		if pv, ok := prior[k]; ok {
			e.Previous = &pv
		}
		_ = RecordAgentWrite(workspace, k, e) // best-effort; the config write already succeeded
	}
	return keys, nil
}

// validateStaged materialises the candidate config (staged overrides merged onto
// base, exactly as LoadProject would resolve it) and runs the authoritative
// whole-config validation.
func validateStaged(base Config, staged map[string]any) error {
	data, err := toml.Marshal(staged)
	if err != nil {
		return fmt.Errorf("encoding staged config: %w", err)
	}
	candidate := cloneConfig(base)
	if err := toml.Unmarshal(data, &candidate); err != nil {
		return fmt.Errorf("merging staged config: %w", err)
	}
	applyEnv(&candidate)
	normaliseConfig(&candidate)
	return validate(candidate)
}

// priorProjectValues stringifies the existing project-level value of each key
// (if the project already overrode it), for the provenance "previous" field.
func priorProjectValues(raw map[string]any, keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := getNested(raw, keyPath(k)); ok {
			out[k] = fmt.Sprintf("%v", v)
		}
	}
	return out
}

// getNested returns the value at path within m, if present. Keys are matched
// case-insensitively, for the same reason lookupNested is (#319): go-toml
// decodes a fold-variant spelling into the same field, so a `[TASKS.go]` table
// holds a real prior value. An exact-match miss here is not cosmetic — this
// feeds priorProjectValues, which stamps the agent-write provenance record
// with what the key USED to be, and setNested writes through that same fold
// variant. Missing it would record "no previous value" for a write that in
// fact overwrote one, and cost `plumb config unset` its one-step revert.
func getNested(m map[string]any, path []string) (any, bool) {
	if len(path) == 1 {
		key, ok := foldLookup(m, path[0])
		if !ok {
			return nil, false
		}
		return m[key], true
	}
	for _, key := range foldKeys(m, path[0]) {
		next, ok := m[key].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := getNested(next, path[1:]); ok {
			return v, true
		}
	}
	return nil, false
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
