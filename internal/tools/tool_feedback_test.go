package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/toolerror"
)

func TestToolFeedback(t *testing.T) {
	tool := NewToolFeedback()
	run := func(args string) (string, error) {
		return tool.Execute(context.Background(), json.RawMessage(args))
	}
	out, err := run(`{"tool":"context_for_task","missing":"the callers of Ledger.Record"}`)
	if err != nil || !strings.Contains(out, "Noted for context_for_task") || !strings.Contains(out, "plumb stats --context") {
		t.Fatalf("a valid note: %q, %v", out, err)
	}
	for _, c := range []struct{ name, args, want string }{
		{"no tool", `{"missing":"x"}`, "tool is required"},
		{"blank tool", `{"tool":"  ","note":"x"}`, "tool is required"},
		{"nothing said", `{"tool":"git"}`, "say what was missing"},
		{"too long", `{"tool":"git","note":"` + strings.Repeat("a", toolFeedbackMaxBytes+1) + `"}`, "note is 501 bytes"},
		{"not json", `{`, "invalid arguments"},
	} {
		_, err := run(c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.want)
			continue
		}
		if toolerror.KindOf(err) != toolerror.KindInvalidArguments {
			t.Errorf("%s: kind = %q, want invalid arguments", c.name, toolerror.KindOf(err))
		}
	}
}
