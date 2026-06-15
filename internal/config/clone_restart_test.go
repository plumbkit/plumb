package config

import (
	"reflect"
	"testing"
)

// TestCloneConfig_RoundTripsDefaults guards against the empty-non-nil → nil
// slice collapse in cloneConfig/cloneLSPConfig: the defaults use Args:
// []string{}, and append([]string(nil), empty...) returns nil, so a clone
// stopped being DeepEqual to its source — which latched RestartNeeded on every
// fresh daemon (Bug E).
func TestCloneConfig_RoundTripsDefaults(t *testing.T) {
	if !reflect.DeepEqual(defaults, cloneConfig(defaults)) {
		t.Fatal("cloneConfig(defaults) is not DeepEqual to defaults")
	}
	if !RestartSensitiveEqual(defaults, cloneConfig(defaults)) {
		t.Fatal("cloneConfig(defaults) is not restart-equal to defaults")
	}
}

// TestRestartNeeded_FreshStoreIsFalse: a just-started daemon with no config
// change must not report a pending restart.
func TestRestartNeeded_FreshStoreIsFalse(t *testing.T) {
	if NewStore(defaults).RestartNeeded() {
		t.Fatal("fresh store reports RestartNeeded=true with no config change")
	}
}
