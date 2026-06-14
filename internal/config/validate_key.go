package config

import (
	"fmt"
	"slices"
	"time"
)

// validate_key.go is the per-key validation + coercion gateway used by the
// agent-writable-config tool. It checks one externally-supplied key/value
// against the registry's type, enum, and range metadata, coercing the value
// (JSON numbers arrive as float64, durations as strings) into the form the TOML
// map wants. The authoritative whole-config invariant check stays validate().

// ValidateKeyValue reports whether value is an acceptable assignment to the
// dotted config key, per the registry. An unknown key is rejected.
func ValidateKeyValue(key string, value any) error {
	f, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("unknown config key %q", key)
	}
	_, err := coerceValue(f, value)
	return err
}

// ApplyKeyToRaw coerces value to the field's type and sets it at the dotted key
// in a sparse nested map (LoadProjectRaw shape), so a batch can be staged and
// validated before any disk write. An unknown or ill-typed value is rejected
// and the map is left untouched.
func ApplyKeyToRaw(m map[string]any, key string, value any) error {
	f, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("unknown config key %q", key)
	}
	coerced, err := coerceValue(f, value)
	if err != nil {
		return err
	}
	setNested(m, keyPath(key), coerced)
	return nil
}

// coerceValue converts an externally-supplied value into the Go type the field
// expects, enforcing enum membership and integer bounds.
func coerceValue(f Field, value any) (any, error) {
	switch f.Type {
	case FieldBool:
		return coerceBool(f, value)
	case FieldInt:
		return coerceInt(f, value)
	case FieldString:
		return coerceString(f, value)
	case FieldEnum:
		return coerceEnum(f, value)
	case FieldDuration:
		return coerceDuration(f, value)
	case FieldList:
		return coerceList(f, value)
	default:
		return nil, fmt.Errorf("%s: unsupported field type", f.Key)
	}
}

func coerceBool(f Field, value any) (any, error) {
	b, ok := value.(bool)
	if !ok {
		return nil, fmt.Errorf("%s: expected a boolean, got %T", f.Key, value)
	}
	return b, nil
}

func coerceInt(f Field, value any) (any, error) {
	n, err := asInt64(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Key, err)
	}
	if f.Min != nil && n < *f.Min {
		return nil, fmt.Errorf("%s: must be >= %d, got %d", f.Key, *f.Min, n)
	}
	if f.Max != nil && n > *f.Max {
		return nil, fmt.Errorf("%s: must be <= %d, got %d", f.Key, *f.Max, n)
	}
	return n, nil
}

func coerceString(f Field, value any) (any, error) {
	s, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s: expected a string, got %T", f.Key, value)
	}
	return s, nil
}

func coerceEnum(f Field, value any) (any, error) {
	s, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s: expected a string, got %T", f.Key, value)
	}
	allowed := EnumValues(f)
	if len(allowed) > 0 && !slices.Contains(allowed, s) {
		return nil, fmt.Errorf("%s: %q is not one of %v", f.Key, s, allowed)
	}
	return s, nil
}

func coerceDuration(f Field, value any) (any, error) {
	s, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s: expected a duration string like \"30s\", got %T", f.Key, value)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid duration %q: %w", f.Key, s, err)
	}
	if d < 0 {
		return nil, fmt.Errorf("%s: duration must be non-negative", f.Key)
	}
	return s, nil // store the canonical string; Duration.UnmarshalText parses it on load
}

func coerceList(f Field, value any) (any, error) {
	switch v := value.(type) {
	case []string:
		return slices.Clone(v), nil
	case []any:
		out := make([]string, 0, len(v))
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s: list element %d is %T, want string", f.Key, i, e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s: expected a list of strings, got %T", f.Key, value)
	}
}

// asInt64 accepts the integer-ish shapes an external value may take: a JSON
// number (float64 with no fractional part), or a native int/int64.
func asInt64(value any) (int64, error) {
	switch n := value.(type) {
	case float64:
		if n != float64(int64(n)) {
			return 0, fmt.Errorf("expected a whole number, got %v", n)
		}
		return int64(n), nil
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	default:
		return 0, fmt.Errorf("expected an integer, got %T", value)
	}
}
