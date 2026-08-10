package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/plumbkit/plumb/internal/fsync"
)

// trust.go records, per absolute workspace root, what the user has trusted about
// that workspace's project config (.plumb/config.toml). The record lives in
// plumb's own data dir — never in the project — so a cloned repository can never
// mark itself trusted (the VS Code "workspace trust" pattern). Default-supplied
// and global-config values need no trust; only what a project supplies does.
//
// Trust binding: a record carries a canonical hash of each thing that was
// trusted — the project-supplied task command set (canonicalTaskHash) and the
// project's capability-granting config request (canonicalPolicyHash, see
// project_policy.go). Each gate compares the CURRENT content's hash against the
// recorded one, so an agent that rewrites a trusted `tasks.<lang>` command, or a
// trusted `[lsp.<lang>] command`, after `plumb trust` cannot have the new value
// take effect without a re-prompt (closes the trust TOCTOU). A coarse Trusted
// flag serves the non-task surfaces that share this per-root grant (run_command's
// [[command]] allow-list, execute_shell_command's policy, and the xcode
// auto-build-server), which are gated on the bare boolean.
//
// The coarse flag deliberately does NOT imply either bound grant: the TUI's
// Commands tab sets it on a workspace-scope edit (trusted-by-authorship), and
// that must never be a path to having a repository's argv or git tiers honoured.
//
// Concurrency: TrustStore serialises reads and writes with a mutex; the on-disk
// file is rewritten atomically.
type TrustStore struct {
	mu   sync.Mutex
	path string
}

// trustRecord is the per-root on-disk trust entry. The legacy on-disk format was
// a bare bool per root; those entries are treated as untrusted and re-confirmed
// once on the next `plumb trust` (see load).
type trustRecord struct {
	// Trusted is the coarse grant covering the project's non-task execution
	// surfaces (run_command, execute_shell_command, xcode auto-build-server).
	Trusted bool `json:"trusted"`
	// TaskHash binds trust to the exact project-supplied task command set that was
	// trusted (canonical SHA-256, hex). Empty means the task gate treats the root
	// as untrusted until `plumb trust` records a hash.
	TaskHash string `json:"task_hash,omitempty"`
	// PolicyHash binds trust to the exact capability-granting project config that
	// was trusted — the [git] block and the exec-deciding [lsp.<lang>] fields
	// (canonical SHA-256, hex; see canonicalPolicyHash). Empty means those
	// sections are forced back to the global config, exactly as they were before
	// trust existed.
	PolicyHash string `json:"policy_hash,omitempty"`
}

// TaskCommandSpec identifies one project-supplied task command for trust
// binding: its language, slot, and the exact command string.
type TaskCommandSpec struct {
	Lang    string
	Slot    string
	Command string
}

// NewTrustStore returns a store backed by <DataDir>/trust.json.
func NewTrustStore() *TrustStore {
	return newTrustStoreAt(filepath.Join(DataDir(), "trust.json"))
}

// newTrustStoreAt backs the store with an explicit file — the test seam.
func newTrustStoreAt(path string) *TrustStore {
	return &TrustStore{path: path}
}

// load reads the trust file into records. It tolerates the legacy `map[string]
// bool` format: a legacy boolean entry is dropped (treated as untrusted), so a
// schema migration re-confirms trust exactly once via `plumb trust`.
func (s *TrustStore) load() (map[string]trustRecord, error) {
	data, err := os.ReadFile(s.path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return map[string]trustRecord{}, nil
	default:
		return nil, fmt.Errorf("reading trust store %s: %w", s.path, err)
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing trust store %s: %w", s.path, err)
	}
	out := make(map[string]trustRecord, len(raw))
	for k, v := range raw {
		var rec trustRecord
		if err := json.Unmarshal(v, &rec); err == nil {
			out[k] = rec
			continue
		}
		// Legacy bare-bool entry: treat as untrusted (drop it). Re-running
		// `plumb trust` re-confirms and records the new bound record.
		var b bool
		if err := json.Unmarshal(v, &b); err == nil {
			continue
		}
		return nil, fmt.Errorf("parsing trust store entry %q in %s", k, s.path)
	}
	return out, nil
}

// save persists records atomically.
func (s *TrustStore) save(m map[string]trustRecord) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	return writeJSONAtomic(s.path, m)
}

// IsTrusted reports whether root has a coarse trust grant. It backs the non-task
// execution surfaces (run_command, execute_shell_command, xcode) that share this
// per-root grant. A read error fails closed (untrusted). The task gate uses
// IsTrustedForTasks, not this, so a task-command change never silently re-enables
// a project command through this coarse boolean.
func (s *TrustStore) IsTrusted(root string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return false
	}
	return m[canonRoot(root)].Trusted
}

// IsTrustedForTasks reports whether root's project-supplied task commands are
// trusted AND unchanged since trust was recorded: the recorded TaskHash must
// match the canonical hash of cmds. A read error, an absent record, or a hash
// mismatch (any add/remove/modify of a task command) all fail closed.
func (s *TrustStore) IsTrustedForTasks(root string, cmds []TaskCommandSpec) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return false
	}
	rec, ok := m[canonRoot(root)]
	if !ok || rec.TaskHash == "" {
		return false
	}
	return rec.TaskHash == canonicalTaskHash(cmds)
}

// SetTrusted records (trusted=true) or clears (false) the coarse grant for root,
// persisting the change atomically.
//
// Setting reads the existing record and flips only Trusted, so BOTH bound hashes
// survive a coarse re-grant (e.g. the TUI Commands tab marking a workspace
// trusted-by-authorship). Assigning a fresh trustRecord here instead would
// silently revoke the task and capability bindings — the easy mistake this
// comment exists to prevent. Clearing removes the whole record, so revocation is
// total.
func (s *TrustStore) SetTrusted(root string, trusted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	key := canonRoot(root)
	if trusted {
		rec := m[key]
		rec.Trusted = true
		m[key] = rec
	} else {
		delete(m, key)
	}
	return s.save(m)
}

// IsTrustedForPolicy reports whether root's capability-granting project config
// is trusted AND unchanged since trust was recorded: the recorded PolicyHash must
// match the canonical hash of spec. A read error, an absent record, an absent
// hash, or a hash mismatch (any add/remove/modify of a [git] field or an
// exec-deciding [lsp.<lang>] field) all fail closed.
//
// It never consults the coarse Trusted flag, so no other surface that grants
// trust can incidentally have a repository's argv honoured.
func (s *TrustStore) IsTrustedForPolicy(root string, spec ProjectPolicySpec) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return false
	}
	rec, ok := m[canonRoot(root)]
	if !ok || rec.PolicyHash == "" {
		return false
	}
	return rec.PolicyHash == canonicalPolicyHash(spec)
}

// SetTrustedForProject grants trust for root and binds it to everything the
// project supplies that needs approval: the task command set (cmds) and the
// capability-granting config request (spec). One record write, so a crash can
// never leave a root half-trusted, and one setter, so a caller cannot record a
// task grant while forgetting the policy grant. The coarse Trusted flag is set
// too, covering the shared non-task surfaces.
func (s *TrustStore) SetTrustedForProject(root string, cmds []TaskCommandSpec, spec ProjectPolicySpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	key := canonRoot(root)
	m[key] = trustRecord{
		Trusted:    true,
		TaskHash:   canonicalTaskHash(cmds),
		PolicyHash: canonicalPolicyHash(spec),
	}
	return s.save(m)
}

// canonicalTaskHash computes a stable, ordering-independent SHA-256 (hex) of a
// task command set. Each command is rendered as its three fields
// length-prefixed and concatenated (encodeField), which is injective by
// construction — decoding does not rely on a separator byte, so no content any
// field could contain (a literal "\n", "\x1f", or digit run) can shift a field
// or record boundary and re-partition one command set into another. The
// per-command encodings are sorted and concatenated (again no separator
// needed: each encoding is self-delimiting, so record boundaries survive
// concatenation) before hashing. Two sets with the same commands in a
// different order hash identically; any add, remove, or modification of a
// command changes the hash. The empty set has a fixed, non-empty hash.
func canonicalTaskHash(cmds []TaskCommandSpec) string {
	lines := make([]string, 0, len(cmds))
	for _, c := range cmds {
		lines = append(lines, encodeField(c.Lang)+encodeField(c.Slot)+encodeField(c.Command))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return hex.EncodeToString(sum[:])
}

// policyHashDomain separates the policy hash's input space from the task hash's,
// so no task command set can ever produce a digest that satisfies the policy gate
// (or the reverse). It is a constant prefix, so it does not disturb the
// injectivity of what follows.
const policyHashDomain = "plumb.project-policy.v1\x00"

// canonicalPolicyHash computes a stable, ordering-independent SHA-256 (hex) of a
// capability-granting project config request. Each entry is rendered as its
// length-prefixed key (encodeField, the same netstring encoding canonicalTaskHash
// uses) followed by its self-delimiting value encoding (encodePolicyValue). Both
// halves are self-delimiting, so an entry's encoding can be parsed back without
// a separator byte, and no content any key or value could contain — a literal
// ':', a digit run, a newline — can shift a field or record boundary and
// re-partition one request into another. The per-entry encodings are sorted and
// concatenated (again self-delimiting, so record boundaries survive) before
// hashing.
//
// Two requests with the same keys and values hash identically regardless of the
// order the TOML declared them; any add, remove, or modification of a key or a
// value changes the hash. The empty spec has a fixed, non-empty hash, so a
// project that asks for nothing can still be recorded as trusted without that
// record being mistakable for a grant over some other content.
func canonicalPolicyHash(spec ProjectPolicySpec) string {
	lines := make([]string, 0, len(spec))
	for _, e := range spec {
		lines = append(lines, encodeField(e.Key)+encodePolicyValue(e.Value))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(policyHashDomain + strings.Join(lines, "")))
	return hex.EncodeToString(sum[:])
}

// encodePolicyValue renders a decoded TOML value as a self-delimiting string: a
// one-byte type tag followed by exactly one netstring (encodeField). A reader
// consumes the tag, then reads the netstring's declared length, so any
// concatenation of these encodings parses back uniquely — which is what makes
// canonicalPolicyHash injective for the nested values TOML allows (an argv is an
// array, an env or initialization_options table is a map).
//
// The tag also separates the TYPE: `true` (bool) and "true" (string) are
// different requests and must not share a digest. Arrays and tables recurse, so
// nesting depth and shape are part of the encoding rather than something a
// flattened rendering could disguise. Floats use the exact hexadecimal form, the
// only decimal-free round-trip for a float64.
func encodePolicyValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "n" + encodeField("")
	case string:
		return "s" + encodeField(t)
	case bool:
		return "b" + encodeField(strconv.FormatBool(t))
	case int64:
		return "i" + encodeField(strconv.FormatInt(t, 10))
	case int:
		return "i" + encodeField(strconv.Itoa(t))
	case float64:
		return "f" + encodeField(strconv.FormatFloat(t, 'x', -1, 64))
	case []any:
		var b strings.Builder
		for _, e := range t {
			b.WriteString(encodePolicyValue(e))
		}
		return "a" + encodeField(b.String())
	case map[string]any:
		return "m" + encodeField(encodePolicyTable(t))
	default:
		// TOML datetimes and anything a future decoder introduces. Typed so two
		// different Go types can never collapse onto one encoding.
		return "?" + encodeField(fmt.Sprintf("%T\x00%v", v, v))
	}
}

// encodePolicyTable renders a table's entries key-sorted, each as a netstring key
// followed by its self-delimiting value — so the encoding is ordering-independent
// and unambiguous at every nesting level.
func encodePolicyTable(t map[string]any) string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(encodeField(k))
		b.WriteString(encodePolicyValue(t[k]))
	}
	return b.String()
}

// encodeField renders s as a length-prefixed field ("<byte length>:<s>"), a
// netstring-style encoding: a reader knows exactly how many bytes belong to s
// from the prefix, so no byte s contains (including a literal ':' or a digit)
// can be misread as a boundary. This is what makes canonicalTaskHash's
// concatenation injective without needing a reserved separator byte.
func encodeField(s string) string {
	return strconv.Itoa(len(s)) + ":" + s
}

// canonRoot returns the absolute, cleaned form of root used as the map key.
func canonRoot(root string) string {
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return filepath.Clean(root)
}

// writeJSONAtomic marshals v to JSON and writes it atomically (temp file in the
// target dir + rename). Shared by the trust store and the provenance sidecar.
// The temp file is fsynced before the rename and the directory after it
// (best-effort), so a crash cannot silently drop a trust grant or revocation.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding json: %w", err)
	}
	return fsync.AtomicWrite(path, data, fsync.Options{
		TempPattern: ".plumb-*.json.tmp",
		Label:       "config",
	})
}
