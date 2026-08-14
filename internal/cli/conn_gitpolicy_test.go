package cli

import (
	"reflect"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// TestGitPolicyFrom_CarriesEveryGitField guards the one crossing between the
// [git] config block and the git tool's live policy. A field added to GitConfig
// and to GitPolicy but forgotten HERE is silently inert: the config parses, the
// trust disclosure lists it, `plumb config show` reports it in effect, and the
// tool never sees it. That failure mode is invisible to every other test, so it
// is checked structurally — set every GitConfig field to a distinctive non-zero
// value, then assert no GitPolicy field came out zero.
func TestGitPolicyFrom_CarriesEveryGitField(t *testing.T) {
	in := config.GitConfig{
		AllowWrites:       true,
		AllowDestructive:  true,
		AllowPush:         true,
		ProtectedBranches: []string{"main"},
		CommitTrailer:     true,
		Env:               map[string]string{"GOWORK": "off"},
		WriteTimeout:      config.Duration{Duration: 7 * time.Minute},
	}
	// If GitConfig grows a field, this fails until the fixture above covers it,
	// which is the prompt to decide whether it should cross into GitPolicy.
	inV := reflect.ValueOf(in)
	for i := range inV.NumField() {
		if inV.Field(i).IsZero() {
			t.Fatalf("fixture leaves config.GitConfig.%s at its zero value — extend it, "+
				"or the corresponding policy field is not really being checked", inV.Type().Field(i).Name)
		}
	}

	got := gitPolicyFrom(in)
	gotV := reflect.ValueOf(got)
	for i := range gotV.NumField() {
		if gotV.Field(i).IsZero() {
			t.Errorf("tools.GitPolicy.%s was not carried across from [git]", gotV.Type().Field(i).Name)
		}
	}

	if got.Env["GOWORK"] != "off" {
		t.Errorf("Env = %v, want the configured git child environment", got.Env)
	}
	// Unwrapped, not merely non-zero: a config.Duration carrying the wrong number
	// would still pass the zero-value sweep above, and the bound this feeds is
	// what decides when plumb kills a commit mid-hook.
	if got.WriteTimeout != 7*time.Minute {
		t.Errorf("WriteTimeout = %s, want the configured 7m", got.WriteTimeout)
	}
}
