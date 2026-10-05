package config

import (
	"strings"
	"testing"
)

// The trust warning for a project's [history] enabled describes the change it
// makes to the user's global setting. Asking for what is already in force
// changes nothing and warns about nothing; a value that is not a bool is
// rejected by LoadProject anyway, but the warning must not claim it switches
// history on.
func TestHistoryEnabledWarningNamesOnlyARealChange(t *testing.T) {
	for _, tc := range []struct {
		value  any
		global bool
		want   string // substring, or "" for no warning
	}{
		{false, true, "switches write history off"},
		{true, false, "switches write history on"},
		{false, false, ""},
		{true, true, ""},
		{"yes", true, "not a bool"},
	} {
		got := historyEnabledWarning(tc.value, tc.global)
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("value %v, global %v: warning %q, want %q", tc.value, tc.global, got, tc.want)
		}
	}
}

// PLUMB_RELAY_WRITE_DIFF turns an off-by-default setting ON, so only an
// explicit yes may: a value plumb does not recognise must not switch the agent
// into repeating every diff.
func TestRelayWriteDiffEnvOnlyAnExplicitYesTurnsItOn(t *testing.T) {
	for _, v := range []string{"False", "FALSE", "off", "disabled", "0", "no"} {
		t.Setenv("PLUMB_RELAY_WRITE_DIFF", v)
		cfg := Defaults()
		applyEnv(&cfg)
		if cfg.Edits.RelayWriteDiff {
			t.Errorf("PLUMB_RELAY_WRITE_DIFF=%q turned the relay on", v)
		}
	}
	t.Setenv("PLUMB_RELAY_WRITE_DIFF", "1")
	cfg := Defaults()
	applyEnv(&cfg)
	if !cfg.Edits.RelayWriteDiff {
		t.Error("PLUMB_RELAY_WRITE_DIFF=1 did not turn the relay on")
	}
}
