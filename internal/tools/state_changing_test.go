package tools

import (
	"slices"
	"testing"
)

func TestHistoryClassKeysAreValidAndExemptReasonsNonEmpty(t *testing.T) {
	gate := StateChangingToolNames()
	for k := range historyRecorded {
		if !slices.Contains(gate, k) {
			t.Errorf("historyRecorded has stale tool %q not in StateChangingToolNames", k)
		}
	}
	for k, reason := range historyExempt {
		if !slices.Contains(gate, k) {
			t.Errorf("historyExempt has stale tool %q not in StateChangingToolNames", k)
		}
		if reason == "" {
			t.Errorf("historyExempt entry %q has empty reason", k)
		}
	}
}
