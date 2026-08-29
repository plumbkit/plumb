package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestTaskSlotNamePattern_MatchesConfig pins this package's slot-name guard
// against the config layer's.
//
// The two exist separately on purpose: tasks.go does not import config (the
// resolver bridges it), so the tool can be tested without a config layer. That
// separation is only safe while the two agree — a tool that accepts a name
// config will reject, or vice versa, moves the refusal to a confusing place.
// Six copies of the slot LIST is what made the vocabulary closed in the first
// place; this test is the cheap guard against the same drift returning as two
// copies of the grammar.
func TestTaskSlotNamePattern_MatchesConfig(t *testing.T) {
	cases := []string{
		"build", "check", "typecheck", "audit", "e2e", "a", "a-b", "a_b", "x9",
		"", "Check", "9check", "check!", "che ck", "-check", "_check",
		strings.Repeat("c", 32), strings.Repeat("c", 33),
	}
	for _, name := range cases {
		tool := taskSlotName.MatchString(name)
		cfg := config.ValidTaskSlotName(name)
		if tool != cfg {
			t.Errorf("slot name %q: tools accepts=%v, config accepts=%v — the two guards have drifted", name, tool, cfg)
		}
	}
}

// TestMutationTest_SlotArgsAcceptProjectDefinedSlots covers the same opening for
// mutation_test that TestRunTask_ProjectDefinedSlotReachesResolver covers for
// run_task: its test_task/compile_task named the built-in five as a closed set,
// so a project whose compile gate is its own slot could not point at it.
//
// These live here rather than in mutationtest_test.go's arg-validation table
// because they are about the slot-name grammar this file already pins — and
// that table's file sits at its size cap.
func TestMutationTest_SlotArgsAcceptProjectDefinedSlots(t *testing.T) {
	tool := NewMutationTest(WriteDeps{}, nil)
	call := func(args map[string]any) error {
		t.Helper()
		args["mutants"] = []map[string]any{{"file_path": "f", "old_string": "a", "new_string": "b"}}
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		_, execErr := tool.Execute(context.Background(), raw)
		return execErr
	}

	// Malformed names stay refused, by this package, as input hygiene.
	for _, bad := range []map[string]any{
		{"test_task": "Deploy It"},
		{"compile_task": "9build"},
	} {
		if err := call(bad); err == nil || !strings.Contains(err.Error(), "is not a valid slot name") {
			t.Errorf("args %v: want a slot-name refusal, got %v", bad, err)
		}
	}

	// A well-formed project-defined name is the workspace's business: it must get
	// past validation rather than being refused against a closed set. With no
	// resolver wired the call still fails, but on the LATER, different ground of
	// there being no task commands for the session — which is what proves
	// validation let it through.
	for _, ok := range []map[string]any{
		{"test_task": "check"},
		{"compile_task": "typecheck"},
	} {
		err := call(ok)
		if err == nil {
			t.Errorf("args %v: expected the no-resolver error", ok)
			continue
		}
		if strings.Contains(err.Error(), "is not a valid slot name") {
			t.Errorf("args %v: a project-defined slot was refused as malformed: %v", ok, err)
		}
	}
}
