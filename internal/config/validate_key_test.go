package config

import "testing"

func TestValidateKeyValue(t *testing.T) {
	cases := []struct {
		key     string
		value   any
		wantErr bool
	}{
		{"edits.strict", true, false},
		{"edits.strict", "yes", true}, // wrong type
		{"log_level", "warn", false},
		{"log_level", "verbose", true},                      // not in enum
		{"edits.rate_limit_per_minute", float64(50), false}, // JSON number
		{"edits.rate_limit_per_minute", float64(-1), true},  // below Min 0
		{"edits.rate_limit_per_minute", float64(1.5), true}, // not whole
		{"lsp_query.timeout", "30s", false},
		{"lsp_query.timeout", "nonsense", true},
		{"topology.exclude_patterns", []any{"vendor/**", "dist/**"}, false},
		{"topology.exclude_patterns", []any{"ok", 5}, true}, // non-string element
		{"tasks.go.build", "go build ./...", false},
		{"unknown.key", "x", true},
	}
	for _, c := range cases {
		err := ValidateKeyValue(c.key, c.value)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateKeyValue(%q, %v) err=%v, wantErr=%v", c.key, c.value, err, c.wantErr)
		}
	}
}

func TestApplyKeyToRaw_PerLanguageTaskKey(t *testing.T) {
	m := map[string]any{}
	if err := ApplyKeyToRaw(m, "tasks.go.test", "go test ./..."); err != nil {
		t.Fatalf("ApplyKeyToRaw: %v", err)
	}
	tasks, ok := m["tasks"].(map[string]any)
	if !ok {
		t.Fatalf("tasks table not created: %#v", m)
	}
	goTasks, ok := tasks["go"].(map[string]any)
	if !ok {
		t.Fatalf("tasks.go table not created: %#v", tasks)
	}
	if goTasks["test"] != "go test ./..." {
		t.Errorf("tasks.go.test = %v, want the command", goTasks["test"])
	}
}

func TestApplyKeyToRaw_CoercesJSONNumber(t *testing.T) {
	m := map[string]any{}
	if err := ApplyKeyToRaw(m, "edits.rate_limit_per_minute", float64(42)); err != nil {
		t.Fatalf("ApplyKeyToRaw: %v", err)
	}
	edits := m["edits"].(map[string]any)
	if got, ok := edits["rate_limit_per_minute"].(int64); !ok || got != 42 {
		t.Errorf("rate_limit_per_minute = %v (%T), want int64 42", edits["rate_limit_per_minute"], edits["rate_limit_per_minute"])
	}
}
