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

// dshPlumbRowID is the patch row that registers the plumb MCP server in a
// DeepSeek Harness user patch layer ($DSH_HOME/cordis.patch.yml). dsh mounts
// that layer over every profile — web, headless, and any custom ones — so one
// row makes plumb's tools available in all of them.
const dshPlumbRowID = "mcp-plumb"

// dshMCPClientPlugin is the dsh plugin that bridges external MCP servers' tools
// onto the harness tool registry. It is a direct dependency of the dsh app
// manifest, so the harness's maintained profiles/node_modules fallback resolves
// it from any profile without a pnpm install.
const dshMCPClientPlugin = "@deepseek-ai/dsh-mcp-client"

// DSHConfigPath returns the DeepSeek Harness home-level user patch layer
// ($DSH_HOME/cordis.patch.yml, or ~/.dsh/cordis.patch.yml when the variable is
// unset or blank). DSH_HOME mirrors dsh's own resolver, which treats empty and
// whitespace-only values as unset. The harness applies this layer to every
// profile, so it is the one write that registers plumb everywhere.
func DSHConfigPath() (string, error) {
	if home := os.Getenv("DSH_HOME"); strings.TrimSpace(home) != "" {
		return filepath.Join(home, "cordis.patch.yml"), nil
	}
	return homeRelConfigPath(".dsh", "cordis.patch.yml")
}

// dshInstalled reports whether DeepSeek Harness looks installed: its home
// directory ($DSH_HOME, or ~/.dsh) exists. dsh creates that directory on its
// first launch, but the home-level cordis.patch.yml only appears once a patch
// entry is configured — so config-file presence alone cannot tell "installed,
// no plumb row yet" from "not installed". Mirrors kimiCodeInstalled.
func dshInstalled() bool {
	if home := os.Getenv("DSH_HOME"); strings.TrimSpace(home) != "" {
		return dirExists(home)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return dirExists(filepath.Join(home, ".dsh"))
}

// dshSetupNote is the setupTarget.note hook for DeepSeek Harness.
func dshSetupNote() string {
	return "Every dsh profile (web, headless, custom) picks this up — tools appear as mcp__plumb__*. " +
		"The bridge plugin ships with dsh, so no pnpm install is needed."
}

// setupDSHInto registers plumb in a DeepSeek Harness user patch layer as a
// stdio MCP server row for the dsh-mcp-client plugin. The patch file is a
// top-level YAML LIST of loader entries, so unlike every other target there is
// no server map to merge into: the walk finds the active mcp-plumb row (nested
// under an insert entry — the only form dsh honours for adding rows) and
// repoints its config in place (preserving any extra keys such as env or
// toolCallTimeoutMs), or appends a fresh insert entry. Every other entry —
// comments and !!js expressions included — is preserved verbatim through the
// node round-trip.
func setupDSHInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	doc, isNew, err := readDSHPatch(cfgPath)
	if err != nil {
		return false, nil, err
	}
	list := doc.Content[0]

	if row := findDSHPlumbRow(list); row != nil {
		added, err := repointDSHPlumbRow(cfgPath, doc, row, plumbBin, isNew)
		return added, nil, err
	}

	if !isNew {
		if err := backupFile(cfgPath); err != nil {
			return false, nil, fmt.Errorf("backing up %s: %w", cfgPath, err)
		}
	}
	list.Content = append(list.Content, dshInsertPlumbEntry(plumbBin))
	if err := writeDSHPatch(cfgPath, doc); err != nil {
		return false, nil, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, nil, nil
}

// repointDSHPlumbRow updates an existing active plumb row's config at plumbBin,
// backing up the file first when it is not new. added is false (no write) when
// the row already launches plumbBin.
func repointDSHPlumbRow(cfgPath string, doc *yaml.Node, row *yaml.Node, plumbBin string, isNew bool) (added bool, err error) {
	if sameDSHPlumbRow(row, plumbBin) {
		return false, nil
	}
	if cfg := mappingValue(row, "config"); cfg != nil && cfg.Kind != yaml.MappingNode {
		return false, fmt.Errorf("config of %s row in %s is not a mapping — will not overwrite", dshPlumbRowID, cfgPath)
	}
	if !isNew {
		if err := backupFile(cfgPath); err != nil {
			return false, fmt.Errorf("backing up %s: %w", cfgPath, err)
		}
	}
	setDSHPlumbRow(row, plumbBin)
	if err := writeDSHPatch(cfgPath, doc); err != nil {
		return false, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, nil
}

// findDSHPlumbRow returns the active plumb row node — one nested under an
// insert patch entry, the only form dsh honours for adding rows. A bare
// id-keyed row is inert (dsh warns and skips it at boot), so it is deliberately
// not found. Returns nil when no active row exists.
func findDSHPlumbRow(list *yaml.Node) *yaml.Node {
	for _, entry := range list.Content {
		insert := patchInsert(entry)
		if insert == nil {
			continue
		}
		for _, candidate := range insert.Content {
			if candidate.Kind == yaml.MappingNode && isDSHPlumbRow(candidate) {
				return candidate
			}
		}
	}
	return nil
}

// patchInsert returns the insert sequence of a patch entry, or nil when the
// entry is not a mapping or carries no insert key.
func patchInsert(entry *yaml.Node) *yaml.Node {
	if entry.Kind != yaml.MappingNode {
		return nil
	}
	insert := mappingValue(entry, "insert")
	if insert == nil || insert.Kind != yaml.SequenceNode {
		return nil
	}
	return insert
}

// dshCommandExtractor reads the launch binary back from a DeepSeek Harness
// user patch layer: the config.command of the active mcp-plumb row, i.e. one
// nested under an insert entry (a bare id-keyed row is inert — dsh warns and
// skips it at boot). registered is false when no active row exists or its
// command is empty or not a plain string. Inspection-only — it never creates
// the file or its parent directory.
func dshCommandExtractor(cfgPath string) (binPath string, registered bool, err error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", false, err
	}
	doc, err := parseDSHPatch(cfgPath, data)
	if err != nil {
		return "", false, err
	}
	row := findDSHPlumbRow(doc.Content[0])
	if row == nil {
		return "", false, nil
	}
	cmd := mappingValue(mappingValue(row, "config"), "command")
	if cmd == nil || cmd.Value == "" {
		return "", false, nil
	}
	return cmd.Value, true, nil
}

// readDSHPatch reads a DeepSeek Harness user patch layer as a YAML document
// node whose root is the entry list. A missing file yields an empty list with
// isNew=true (and the parent directory created, mirroring
// readOrInitClaudeConfig); a present file that is not a YAML list is an error —
// dsh itself refuses such a file at boot, so plumb must not rewrite it into a
// shape the harness would reject.
func readDSHPatch(path string) (doc *yaml.Node, isNew bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, fmt.Errorf("creating directory: %w", err)
		}
		return dshEmptyPatchDoc(), true, nil
	}
	if err != nil {
		return nil, false, err
	}
	doc, err = parseDSHPatch(path, data)
	if err != nil {
		return nil, false, err
	}
	return doc, false, nil
}

// parseDSHPatch parses patch data as a YAML document whose root must be an
// entry list. It is the inspection-only counterpart to readDSHPatch — it
// creates nothing. Errors carry the "will not overwrite" contract of the other
// readOrInit* helpers. A blank file parses as an empty list rather than an
// error, so plumb can repair a file dsh's own boot would refuse.
func parseDSHPatch(path string, data []byte) (*yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return dshEmptyPatchDoc(), nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s as a YAML patch list: %w — will not overwrite", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s is not a YAML list — dsh would refuse it at boot, so plumb will not overwrite it", path)
	}
	return &doc, nil
}

// dshEmptyPatchDoc builds the document node for a fresh patch layer: an empty
// entry list.
func dshEmptyPatchDoc() *yaml.Node {
	return &yaml.Node{
		Kind:    yaml.DocumentNode,
		Content: []*yaml.Node{{Kind: yaml.SequenceNode}},
	}
}

// writeDSHPatch writes a patch document node back to path atomically, through
// the shared setup writer. The node round-trip preserves every comment and
// !!js tag in entries plumb does not own, and marshalYAMLLike keeps their
// indentation too — a plain yaml.Marshal re-indents the whole layer to 4
// spaces, which is how plumb has corrupted a client's config before (see
// marshalYAMLLike).
func writeDSHPatch(path string, doc *yaml.Node) error {
	// Read failure is not fatal: an absent or unreadable file reveals no indent
	// and marshalYAMLLike falls back to the default.
	existing, _ := os.ReadFile(path)
	data, err := marshalYAMLLike(doc, existing)
	if err != nil {
		return err
	}
	return fsync.AtomicWrite(path, data, setupWriteOptions(".plumb_setup_*.yml"))
}

// mappingValue returns the value node for key in a mapping node, or nil when
// the key is absent or the node is not a mapping.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMappingValue sets key in a mapping node to node v, keeping the key's
// position when it is already present and appending the pair otherwise.
func setMappingValue(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
}

// isDSHPlumbRow reports whether a patch entry is the plumb row (its id).
func isDSHPlumbRow(entry *yaml.Node) bool {
	id := mappingValue(entry, "id")
	return id != nil && id.Value == dshPlumbRowID
}

// sameDSHPlumbRow reports whether an existing plumb row already launches
// plumbBin over stdio — the idempotence check. A row with a different command,
// a non-stdio transport, or different args is not "already registered" and
// gets repointed.
func sameDSHPlumbRow(entry *yaml.Node, plumbBin string) bool {
	cfg := mappingValue(entry, "config")
	if cfg == nil || cfg.Kind != yaml.MappingNode {
		return false
	}
	if command := mappingValue(cfg, "command"); command == nil || command.Value != plumbBin {
		return false
	}
	if transport := mappingValue(cfg, "transport"); transport == nil || transport.Value != "stdio" {
		return false
	}
	args := mappingValue(cfg, "args")
	if args == nil || args.Kind != yaml.SequenceNode || len(args.Content) != 1 || args.Content[0].Value != "serve" {
		return false
	}
	return true
}

// setDSHPlumbRow repoints an existing plumb row's config at plumbBin, merging
// the canonical fields onto whatever the row already carries so user-added
// keys survive a re-register or a plumb setup --all repoint.
func setDSHPlumbRow(entry *yaml.Node, plumbBin string) {
	cfg := mappingValue(entry, "config")
	if cfg == nil || cfg.Kind != yaml.MappingNode {
		cfg = &yaml.Node{Kind: yaml.MappingNode}
		setMappingValue(entry, "config", cfg)
	}
	setMappingValue(cfg, "serverName", yamlScalar("plumb"))
	setMappingValue(cfg, "transport", yamlScalar("stdio"))
	setMappingValue(cfg, "command", yamlScalar(plumbBin))
	setMappingValue(cfg, "args", yamlServeArgs())
}

// dshPlumbRowNode builds the patch row registering plumb as a stdio MCP
// server: a row for the in-box dsh-mcp-client plugin. The model sees the
// bridged tools under the mcp__plumb__* namespace.
func dshPlumbRowNode(plumbBin string) *yaml.Node {
	cfg := &yaml.Node{Kind: yaml.MappingNode}
	setMappingValue(cfg, "serverName", yamlScalar("plumb"))
	setMappingValue(cfg, "transport", yamlScalar("stdio"))
	setMappingValue(cfg, "command", yamlScalar(plumbBin))
	setMappingValue(cfg, "args", yamlServeArgs())

	row := &yaml.Node{Kind: yaml.MappingNode}
	setMappingValue(row, "id", yamlScalar(dshPlumbRowID))
	setMappingValue(row, "name", yamlScalar(dshMCPClientPlugin))
	setMappingValue(row, "config", cfg)
	return row
}

// dshInsertPlumbEntry builds the patch entry that INSERTS the plumb row. dsh
// patch semantics only add new rows through the insert form: a bare id-keyed
// entry targets a row that already exists and is skipped with a warning when
// none does — which is why the shipped bundle patches all use insert lists.
func dshInsertPlumbEntry(plumbBin string) *yaml.Node {
	entry := &yaml.Node{Kind: yaml.MappingNode}
	setMappingValue(entry, "insert", &yaml.Node{
		Kind:    yaml.SequenceNode,
		Content: []*yaml.Node{dshPlumbRowNode(plumbBin)},
	})
	return entry
}

// yamlScalar builds a plain scalar node.
func yamlScalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}

// yamlServeArgs builds the args sequence the plumb row launches with.
func yamlServeArgs() *yaml.Node {
	return &yaml.Node{
		Kind:    yaml.SequenceNode,
		Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "serve"}},
	}
}
