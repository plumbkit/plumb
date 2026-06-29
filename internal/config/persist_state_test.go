package config

import "testing"

func TestSessionPersistStateDefaults(t *testing.T) {
	if !defaults.Session.PersistState {
		t.Error("PersistState should default to true")
	}
	if defaults.Session.PersistStateTTLMinutes != 1440 {
		t.Errorf("PersistStateTTLMinutes default = %d, want 1440", defaults.Session.PersistStateTTLMinutes)
	}
}

func TestApplyEnv_PersistSessionStateDisable(t *testing.T) {
	t.Setenv("PLUMB_PERSIST_SESSION_STATE", "0")
	cfg := defaults
	applyEnv(&cfg)
	if cfg.Session.PersistState {
		t.Error("PLUMB_PERSIST_SESSION_STATE=0 should disable PersistState")
	}
}

func TestApplyEnv_PersistSessionStateEnable(t *testing.T) {
	t.Setenv("PLUMB_PERSIST_SESSION_STATE", "1")
	cfg := defaults
	cfg.Session.PersistState = false // simulate a config-file opt-out
	applyEnv(&cfg)
	if !cfg.Session.PersistState {
		t.Error("PLUMB_PERSIST_SESSION_STATE=1 should enable PersistState over a config opt-out")
	}
}
