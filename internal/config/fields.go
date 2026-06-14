package config

import (
	"strings"
	"sync"
)

// fields.go is the single source of truth for per-config-field metadata:
// description, reload tier, type, and agent-write safety class. It is consumed
// by the TUI Settings screen, `plumb config show`, the per-key validator, and
// the agent-writable-config tool — so a field's description and reload tier are
// defined once, in the Application layer, never duplicated in Presentation.
//
// Concurrency: the registry is immutable after package init; the lookup map is
// built once under sync.Once. RegisterEnumValues is the one mutable seam (a
// higher layer supplying dynamic enum members, e.g. tui supplying theme names);
// it is guarded by its own mutex.

// ReloadTier classifies when a change to a setting takes effect. The
// ReloadRestart set must stay in lock-step with the fields
// RestartSensitiveEqual compares (log format + cache).
type ReloadTier int

const (
	ReloadLive        ReloadTier = iota // applies to running sessions immediately
	ReloadNextSession                   // applies on next attach / new session
	ReloadRestart                       // needs a daemon restart
)

func (t ReloadTier) String() string {
	switch t {
	case ReloadLive:
		return "live"
	case ReloadNextSession:
		return "next-session"
	case ReloadRestart:
		return "restart"
	default:
		return "unknown"
	}
}

// FieldType is the value shape of a config field, driving coercion and
// validation of an externally-supplied value (e.g. a JSON value from the
// agent-config tool, where numbers arrive as float64 and durations as strings).
type FieldType int

const (
	FieldBool FieldType = iota
	FieldInt
	FieldDuration
	FieldEnum
	FieldString
	FieldList // []string
)

func (t FieldType) String() string {
	switch t {
	case FieldBool:
		return "bool"
	case FieldInt:
		return "int"
	case FieldDuration:
		return "duration"
	case FieldEnum:
		return "enum"
	case FieldString:
		return "string"
	case FieldList:
		return "list"
	default:
		return "unknown"
	}
}

// SafetyClass governs whether the agent-writable-config tool may write a field.
// The zero value is SafetyDenied — a field is never agent-writable unless it is
// explicitly opted in, so a newly-added field fails closed.
type SafetyClass int

const (
	SafetyDenied  SafetyClass = iota // never agent-writable (default — fail closed)
	SafetyAllowed                    // on the agent allowlist
)

// Field describes one configuration field.
type Field struct {
	// Key is the dotted TOML path. For per-language families it is the TEMPLATE
	// form with a literal "<lang>" segment, e.g. "tasks.<lang>.build".
	Key           string
	Type          FieldType
	Description   string
	ReloadTier    ReloadTier
	Safety        SafetyClass
	AllowedValues []string // for FieldEnum; may be augmented at runtime via RegisterEnumValues
	Min, Max      *int64   // optional inclusive bounds for FieldInt; nil = unbounded
	// PerLanguage marks a keyed sub-table family ([lsp.<lang>].*, [tasks.<lang>].*).
	PerLanguage bool
}

const langPlaceholder = "<lang>"

// perLanguageFamilies are the dotted-key prefixes whose second segment is a
// language id, normalised to "<lang>" for registry lookup.
var perLanguageFamilies = map[string]bool{"lsp": true, "tasks": true}

// Registry returns the immutable set of known config fields. Callers must not
// mutate the returned slice.
func Registry() []Field {
	return registryData
}

var (
	fieldMapOnce sync.Once
	fieldMap     map[string]Field
)

func buildFieldMap() {
	fieldMap = make(map[string]Field, len(registryData))
	for _, f := range registryData {
		fieldMap[f.Key] = f
	}
}

// Lookup resolves a concrete dotted key to its field, normalising a
// per-language family key ("tasks.go.build") to its template
// ("tasks.<lang>.build"). The second return value reports whether it is known.
func Lookup(key string) (Field, bool) {
	fieldMapOnce.Do(buildFieldMap)
	f, ok := fieldMap[normaliseFamilyKey(key)]
	return f, ok
}

// normaliseFamilyKey rewrites the language segment of a per-language family key
// to the "<lang>" placeholder. Non-family keys pass through unchanged.
func normaliseFamilyKey(key string) string {
	parts := strings.Split(key, ".")
	if len(parts) >= 3 && perLanguageFamilies[parts[0]] && parts[1] != langPlaceholder {
		parts[1] = langPlaceholder
		return strings.Join(parts, ".")
	}
	return key
}

// keyPath splits a concrete dotted key into the path SetProjectValue expects.
// Unlike Lookup, it does NOT normalise the language segment — the write targets
// the concrete language.
func keyPath(key string) []string {
	return strings.Split(key, ".")
}

// --- dynamic enum members (the tui → config seam for theme names) -----------

var (
	dynamicEnumsMu sync.RWMutex
	dynamicEnums   = map[string][]string{}
)

// RegisterEnumValues supplies enum members for a field whose option set lives
// above the config layer (e.g. tui registers the available theme names for
// "ui.theme"). Safe for concurrent use; later registrations replace earlier.
func RegisterEnumValues(key string, values []string) {
	dynamicEnumsMu.Lock()
	defer dynamicEnumsMu.Unlock()
	dynamicEnums[key] = append([]string(nil), values...)
}

// EnumValues returns the effective allowed values for a field: dynamically
// registered values when present, otherwise the field's static AllowedValues.
// An empty result means "membership unchecked" (any non-empty string accepted).
func EnumValues(f Field) []string {
	dynamicEnumsMu.RLock()
	defer dynamicEnumsMu.RUnlock()
	if v, ok := dynamicEnums[f.Key]; ok && len(v) > 0 {
		return v
	}
	return f.AllowedValues
}
