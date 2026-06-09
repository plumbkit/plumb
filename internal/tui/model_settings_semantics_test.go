package tui

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

func TestSemanticsRows_MaskedKeyAndTab(t *testing.T) {
	cfg := config.Defaults()
	cfg.Semantics.APIKey = "sk-supersecret-leak-me"

	items := buildSettingItems(cfg)
	sem := filterSettingsByTab(items, settingsTabSemantics)
	if len(sem) != 8 {
		t.Fatalf("expected 8 semantics rows, got %d", len(sem))
	}

	// The raw key must never appear in any rendered row value.
	for _, it := range items {
		if strings.Contains(it.value, "supersecret") {
			t.Fatalf("api key leaked into a settings row value: %q", it.value)
		}
	}

	// The api-key row is masked and present in the Semantics tab.
	var found bool
	for _, it := range sem {
		if it.key == skSemAPIKey {
			found = true
			if !strings.Contains(it.value, "set in config") {
				t.Errorf("api key row should show a mask, got %q", it.value)
			}
		}
	}
	if !found {
		t.Error("api key row not found in the Semantics tab")
	}
}

func TestStringField_SemanticsMapping(t *testing.T) {
	c := &config.Config{}
	if p := stringField(c, skSemModel); p == nil {
		t.Error("skSemModel should map to a string field")
	} else {
		*p = "voyage-code-3"
		if c.Semantics.Model != "voyage-code-3" {
			t.Error("stringField did not point at Semantics.Model")
		}
	}
	if stringField(c, skStrict) != nil {
		t.Error("a non-string key must map to nil")
	}
}
