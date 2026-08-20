package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/plumbkit/plumb/internal/fsync"
)

// This file holds the YAML serialisation side of `plumb setup` — reading a
// client's config and putting it back. It is split from setup_helpers.go, which
// keeps the JSON and TOML equivalents plus the path resolvers, so both files
// stay under the size cap.
//
// The governing rule for everything here: plumb owns ONE entry in a config file
// somebody else wrote. Every other byte is theirs, and a write that restyles it
// is a bug even when the result still parses — see marshalYAMLLike.

// readOrInitYAMLConfig reads cfgPath as YAML into a generic map.
// isNew is true when the file did not exist.
func readOrInitYAMLConfig(path string) (m map[string]any, isNew bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, fmt.Errorf("creating directory: %w", err)
		}
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return map[string]any{}, false, nil
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing %s as YAML: %w — will not overwrite", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, false, nil
}

// defaultYAMLIndent is what plumb encodes with when the file it is writing
// reveals no indentation of its own — a new or flat config. It is yaml.v3's own
// default, so a file plumb creates looks exactly as it always has.
const defaultYAMLIndent = 4

// maxDetectableYAMLIndent bounds what detectYAMLIndent will believe. A run
// wider than this is more likely a wrapped value or an aligned comment block
// than a nesting level.
const maxDetectableYAMLIndent = 10

// detectYAMLIndent reports the width of one nesting level in data, as the
// smallest positive leading-space run across its content lines, or 0 when data
// reveals none.
//
// The minimum rather than the first line's: a file can open with a deeply
// nested line (a list under a mapping, a block scalar's body), and one level is
// the smallest step any line takes. Comment lines are skipped — their
// indentation is free-form and routinely does not line up with the data around
// them — as are tab-indented lines, which YAML does not permit as indentation.
func detectYAMLIndent(data []byte) int {
	smallest := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "\t") {
			continue
		}
		body := strings.TrimLeft(line, " ")
		if body == "" || strings.HasPrefix(body, "#") {
			continue
		}
		if n := len(line) - len(body); n > 0 && (smallest == 0 || n < smallest) {
			smallest = n
		}
	}
	if smallest > maxDetectableYAMLIndent {
		return 0
	}
	return smallest
}

// marshalYAMLLike encodes v as YAML indented to match existing — the current
// contents of the file about to be overwritten — falling back to
// defaultYAMLIndent when existing reveals no indentation.
//
// Why this is not plain yaml.Marshal: that always emits 4-space indentation, so
// registering plumb in a 2-space config silently re-indented the WHOLE file.
// The client's own writer then did not recognise its own keys at their new
// depth and appended a second copy beside them, leaving a config neither tool
// could parse (observed on a real ~/.hermes/config.yaml, whose `plugins.enabled`
// ended up present twice at two depths). The damage surfaces on the NEXT read,
// which is why the write looked clean for days.
func marshalYAMLLike(v any, existing []byte) ([]byte, error) {
	indent := detectYAMLIndent(existing)
	if indent == 0 {
		indent = defaultYAMLIndent
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(indent)
	if err := enc.Encode(v); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeYAML writes m to path as YAML, creating the file if needed, matching the
// indentation already in the file. It writes to a temp file in the same
// directory and renames atomically.
func writeYAML(path string, m map[string]any) error {
	// Read failure is not fatal here: an absent or unreadable file simply
	// reveals no indent, and marshalYAMLLike falls back to the default.
	existing, _ := os.ReadFile(path)
	data, err := marshalYAMLLike(m, existing)
	if err != nil {
		return err
	}

	return fsync.AtomicWrite(path, data, setupWriteOptions(".plumb_setup_*.yaml"))
}
