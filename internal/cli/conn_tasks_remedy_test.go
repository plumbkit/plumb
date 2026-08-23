package cli

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// shippedOperand returns the value plumb's own {target:<D>} placeholder declares
// for (lang, slot) — the ONE element a remedy is allowed to replace. Derived
// from config.Defaults() rather than spelled as a literal, so a change to a
// shipped command is caught here instead of silently invalidating the test.
func shippedOperand(t *testing.T, lang, slot string) string {
	t.Helper()
	def, err := config.ParseTaskCommand(config.DefaultTaskCommand(lang, slot))
	if err != nil || def == nil {
		t.Fatalf("no shipped %s command for %s", slot, lang)
	}
	_, value, ok := soleDefaultedPlaceholder(def)
	if !ok {
		t.Fatalf("the shipped %s command for %s carries no defaulted placeholder", slot, lang)
	}
	return value
}

// TestTargetPlaceholderRefusal_RemedyKeepsTheCallersOwnCommand is the fix for
// the remedy that cost the caller their flags.
//
// Reconciliation means the command that IS the expanded shipped default no
// longer reaches this refusal at all — so the only commands that can reach it
// are commands that DIFFER from the default, which is exactly the population for
// which "set the slot to plumb's default" throws work away. Following the old
// advice turned `go test -race ./...` into `go test {target:./...}` and dropped
// the race detector from every later run, silently.
//
// Nothing here is asserted against a hand-written expected command. The
// properties are relationships the remedy must satisfy whatever its wording:
// following it must not change an unscoped run, must make the refused target
// land, and must not cost the caller a single argument they wrote.
func TestTargetPlaceholderRefusal_RemedyKeepsTheCallersOwnCommand(t *testing.T) {
	ws := t.TempDir()
	for _, tc := range []struct{ name, lang, stored, target string }{
		{"the race detector and count flag survive", "go", "go test -race -count=1 ./...", "./internal/cli"},
		{"a build tag written after the operand survives", "go", "go test ./... -tags=integration", "./internal/cli"},
		{"the caller's own test runner survives", "go", "gotestsum ./...", "./internal/cli"},
		{"a command with no extra flags lands on the shipped spelling", "go", expandShippedDefault(t, "go", "test"), "./internal/tools"},
		{"an empty default is appended, since there is no operand to replace", "python", "pytest --cov=src", "tests/test_x.py"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const slot = "test"
			shipped := config.DefaultTaskCommand(tc.lang, slot)
			fixed, ok := targetPlaceholderRemedy(tc.stored, shipped)
			if !ok {
				t.Fatalf("no remedy derived for stored %q against shipped %q", tc.stored, shipped)
			}
			storedArgv, err := config.ParseTaskCommand(tc.stored)
			if err != nil {
				t.Fatal(err)
			}
			remedied := config.TasksConfig{Test: fixed}

			// 1. Following the remedy must not change what an UNSCOPED run does.
			//    A remedy that quietly alters the caller's everyday command is the
			//    same defect in a new place.
			unscoped, err := buildTaskSteps(remedied, tc.lang, slot, "")
			if err != nil || len(unscoped) == 0 {
				t.Fatalf("the remedied command %q does not run unscoped: %v", fixed, err)
			}
			if !slices.Equal(unscoped[0], storedArgv) {
				t.Errorf("the remedy changed the unscoped run: %q builds %v, but the caller's %q builds %v",
					fixed, unscoped[0], tc.stored, storedArgv)
			}

			// 2. It must make the target that was refused actually land.
			scoped, err := buildTaskSteps(remedied, tc.lang, slot, tc.target)
			if err != nil || len(scoped) == 0 {
				t.Fatalf("the remedied command %q still refuses a target: %v", fixed, err)
			}
			if !slices.Contains(scoped[0], tc.target) {
				t.Errorf("the remedied command dropped the target: %v", scoped[0])
			}

			// 3. Every argument the caller wrote must survive into the scoped run,
			//    except the one plumb's own placeholder stands in for. This is what
			//    "keep your flags" means, asserted rather than trusted.
			operand := shippedOperand(t, tc.lang, slot)
			for _, arg := range storedArgv {
				if arg == operand {
					continue
				}
				if !slices.Contains(scoped[0], arg) {
					t.Errorf("the remedy cost the caller %q: %q scopes to %v", arg, fixed, scoped[0])
				}
			}

			// 4. Both directions of the message, in one build. The refusal must
			//    carry the derived command, and may quote the shipped default as the
			//    value to set ONLY when that default IS the caller's own command with
			//    the placeholder put back — which is precisely the case the old
			//    wording got right and every other case it got wrong.
			msg := targetPlaceholderRefusal(ws, config.TasksConfig{Test: tc.stored}, tc.lang, slot).Error()
			if !strings.Contains(msg, fixed) {
				t.Errorf("the refusal must carry the derived command %q; got: %s", fixed, msg)
			}
			quotesDefault := strings.Contains(msg, strconv.Quote(shipped))
			if want := fixed == shipped; quotesDefault != want {
				t.Errorf("refusal quotes plumb's own default %q as the value to set = %v, want %v — "+
					"naming the default for a command that is not the caller's own is what costs "+
					"them their flags: %s", shipped, quotesDefault, want, msg)
			}
		})
	}
}

// TestTargetPlaceholderRemedy_DeclinesWhenItWouldHaveToGuess is the other
// direction: a stored command that never spells the shipped default's operand
// (`make test`) offers no element that can be identified as the scope, so no
// remedy is derived and the caller gets the generic "add a placeholder" advice
// instead of a guess about where their target belongs.
func TestTargetPlaceholderRemedy_DeclinesWhenItWouldHaveToGuess(t *testing.T) {
	shipped := config.DefaultTaskCommand("go", "test")
	if _, ok := targetPlaceholderRemedy("make test", shipped); ok {
		t.Error("a command that does not spell the default's operand must not be rewritten by guess")
	}
	// A slot plumb ships no placeholder for has nothing to derive from either.
	if _, ok := targetPlaceholderRemedy("golangci-lint run", config.DefaultTaskCommand("go", "lint")); ok {
		t.Error("a slot with no shipped placeholder must derive no remedy")
	}
	// And the generic remedy is what the caller then sees.
	msg := targetPlaceholderRefusal(t.TempDir(), config.TasksConfig{Test: "make test"}, "go", "test").Error()
	if !strings.Contains(msg, "add a {target} placeholder") {
		t.Errorf("with no derivable remedy the refusal must still say what to do; got: %s", msg)
	}
}

// TestSoleDefaultedPlaceholder_OnlyADefaultedSingletonReconciles pins the
// predicate reconciliation rests on, directly.
//
// It had no direct fixture, and every shipped default happens to carry a
// DEFAULTED placeholder — so the bare-{target} and multiple-placeholder guards
// were unreachable through the defaults and a mutant that deleted them survived.
// A bare {target} has no expanded spelling to compare a stored command against,
// so accepting one would let reconciliation fire on an equivalence it cannot
// establish; two placeholders make "the operand" meaningless.
func TestSoleDefaultedPlaceholder_OnlyADefaultedSingletonReconciles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		argv    []string
		wantIdx int
		wantVal string
		wantOK  bool
	}{
		{"a defaulted placeholder", []string{"go", "test", "{target:./...}"}, 2, "./...", true},
		{"an empty default", []string{"pytest", "{target:}"}, 1, "", true},
		{"a BARE placeholder has no expanded spelling to compare against", []string{"npm", "test", "{target}"}, 0, "", false},
		{"a bare placeholder alongside a defaulted one", []string{"go", "test", "{target}", "{target:./...}"}, 0, "", false},
		{"two defaulted placeholders leave no single operand", []string{"go", "test", "{target:./...}", "{target:x}"}, 0, "", false},
		{"no placeholder at all", []string{"golangci-lint", "run"}, -1, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, value, ok := soleDefaultedPlaceholder(tc.argv)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (argv %v)", ok, tc.wantOK, tc.argv)
			}
			if !ok {
				return
			}
			if idx != tc.wantIdx || value != tc.wantVal {
				t.Errorf("idx, value = %d, %q; want %d, %q", idx, value, tc.wantIdx, tc.wantVal)
			}
		})
	}
}
