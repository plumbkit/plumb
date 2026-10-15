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

// The pack's pointer to tool_feedback takes only room nothing else wanted: it is
// there when the pack has room, and a tight pack never loses a line to it.
func TestContextForTask_FeedbackLineOnlyInSpareRoom(t *testing.T) {
	s := newShop(t)
	roomy, err := s.run(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}, "max_bytes": 32000})
	if err != nil || !strings.Contains(roomy, contextFeedbackLine) {
		t.Fatalf("a roomy pack lacks the feedback line (%v):\n%s", err, roomy)
	}
	for mb := contextMinMaxBytes; mb <= 6000; mb += 97 {
		pack := s.collect(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}, "max_bytes": mb})
		without := packLines(pack.lines(), pack.budget()).Text
		res := renderContext(pack)
		with := res.Text
		if res.Omitted && strings.Contains(with, contextFeedbackLine) {
			t.Fatalf("max_bytes %d: a pack that lost lines also carries the feedback line:\n%s", mb, with)
		}
		if !strings.HasPrefix(with, without) {
			t.Fatalf("max_bytes %d: the feedback line changed the pack itself:\n--- without\n%s\n--- with\n%s", mb, without, with)
		}
		if len(with) > pack.budget() {
			t.Fatalf("max_bytes %d: %d bytes, over the %d-byte pack budget", mb, len(with), pack.budget())
		}
	}
}
