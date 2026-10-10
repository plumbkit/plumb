package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type codexSpecific struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func decodeCodexOutput(t *testing.T, out map[string]any) codexSpecific {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc codexSpecific
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// The Codex adapter asks nothing outside a plumb workspace. Inside one it sends
// the prompt's selectors (never the prompt) with Codex's own turn id, and
// answers UserPromptSubmit and SubagentStart with additionalContext; no hint is
// no output at all.
func TestCodexHookOutput_ContextHints(t *testing.T) {
	_, sub := hintWorkspace(t)
	var asked []contextHintRequest
	ask := func(r contextHintRequest) string { asked = append(asked, r); return "HINT\n" }

	// Empty cwd, not t.TempDir(): see TestClaudeContextHintOutput.
	if out := codexHookOutput(codexHookInput{Event: "UserPromptSubmit", SessionID: "thr-1", Prompt: "`Cart.Total`"}, nil, nil, ask); out != nil || len(asked) != 0 {
		t.Fatalf("outside a workspace: output %v after %d asks, want none", out, len(asked))
	}

	in := codexHookInput{Event: "UserPromptSubmit", CWD: sub, SessionID: "thr-1", TurnID: "turn-9", Prompt: "secret plan for `Cart.Total`"}
	doc := decodeCodexOutput(t, codexHookOutput(in, nil, nil, ask))
	if doc.HookSpecificOutput.HookEventName != "UserPromptSubmit" || doc.HookSpecificOutput.AdditionalContext != "HINT" {
		t.Fatalf("UserPromptSubmit output = %+v", doc)
	}
	req := asked[0]
	want := contextHintRequest{Host: "codex", Event: "UserPromptSubmit", SessionID: "thr-1", TurnID: "turn-9", Selectors: []string{"Cart.Total"}}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("request = %+v, want %+v", req, want)
	}
	if b, _ := json.Marshal(req); strings.Contains(string(b), "secret plan") {
		t.Fatalf("the prompt text left the hook: %s", b)
	}

	sa := decodeCodexOutput(t, codexHookOutput(codexHookInput{Event: "SubagentStart", CWD: sub, SessionID: "thr-1", AgentID: "a1", TurnID: "turn-9"}, nil, nil, ask))
	if sa.HookSpecificOutput.HookEventName != "SubagentStart" || sa.HookSpecificOutput.AdditionalContext != "HINT" {
		t.Fatalf("SubagentStart output = %+v", sa)
	}
	if last := asked[len(asked)-1]; last.AgentID != "a1" || last.Selectors != nil {
		t.Errorf("SubagentStart request = %+v, want the child's agent id and no selectors", last)
	}

	silent := func(contextHintRequest) string { return "" }
	for _, ev := range []string{"UserPromptSubmit", "SubagentStart"} {
		if out := codexHookOutput(codexHookInput{Event: ev, CWD: sub, SessionID: "thr-1"}, nil, nil, silent); out != nil {
			t.Errorf("%s with no hint = %v, want no output", ev, out)
		}
	}
}

// A SessionStart hint joins the linkage sentence; without one, the output is
// exactly what it was before hints existed.
func TestCodexHookOutput_SessionStartKeepsLinkage(t *testing.T) {
	_, sub := hintWorkspace(t)
	in := codexHookInput{Event: "SessionStart", CWD: sub, SessionID: "thr-1", Source: "resume"}
	plain := codexHookResult(in, nil, nil)
	if got := codexHookOutput(in, nil, nil, func(contextHintRequest) string { return "" }); !reflect.DeepEqual(got, plain) {
		t.Fatalf("no hint changed SessionStart: %v, want %v", got, plain)
	}
	var req contextHintRequest
	doc := decodeCodexOutput(t, codexHookOutput(in, nil, nil, func(r contextHintRequest) string { req = r; return "HINT\n" }))
	linkage := sessionLinkageSentence("thr-1", "Codex conversation")
	if doc.HookSpecificOutput.AdditionalContext != linkage+"\n\nHINT" {
		t.Fatalf("SessionStart context = %q", doc.HookSpecificOutput.AdditionalContext)
	}
	if req.Source != "resume" || req.Host != "codex" {
		t.Errorf("SessionStart request = %+v", req)
	}
	// No session id: no linkage, so nothing to attach a hint to, and no ask.
	asked := false
	if out := codexHookOutput(codexHookInput{Event: "SessionStart", CWD: sub}, nil, nil, func(contextHintRequest) string { asked = true; return "HINT" }); out != nil || asked {
		t.Errorf("SessionStart without an id = %v (asked %v), want nothing", out, asked)
	}
}

// The real stdin path decodes Codex 0.161.0's UserPromptSubmit payload (snake
// case, extra fields) and prints the hint document.
func TestRunCodexHookIO_UserPromptSubmitPayload(t *testing.T) {
	_, sub := hintWorkspace(t)
	payload, _ := json.Marshal(map[string]any{
		"session_id": "thr-1", "turn_id": "turn-3", "cwd": sub, "hook_event_name": "UserPromptSubmit",
		"model": "gpt-5", "permission_mode": "default", "transcript_path": nil,
		"prompt": "look at `internal/cart/cart.go`",
	})
	var got contextHintRequest
	var out bytes.Buffer
	if err := runCodexHookIOWithHints(bytes.NewReader(payload), &out, nil, nil, func(r contextHintRequest) string { got = r; return "HINT" }); err != nil {
		t.Fatal(err)
	}
	if got.TurnID != "turn-3" || !reflect.DeepEqual(got.Selectors, []string{"internal/cart/cart.go"}) {
		t.Fatalf("request = %+v", got)
	}
	var doc codexSpecific
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.HookSpecificOutput.AdditionalContext != "HINT" {
		t.Fatalf("stdout = %q (%v)", out.String(), err)
	}
	// The test-only runner asks for no hints at all.
	out.Reset()
	if err := runCodexHookIOWith(bytes.NewReader(payload), &out, nil, nil); err != nil || out.Len() != 0 {
		t.Fatalf("hint-less runner printed %q (%v)", out.String(), err)
	}
}

// The production entry point, runCodexHookIO, is wired to the real asker: the
// request reaches the daemon's control socket and the daemon's hint comes back
// as the event's additionalContext.
func TestRunCodexHookIO_HintReachesTheDaemon(t *testing.T) {
	probeTestEnv(t)
	_, sub := hintWorkspace(t)
	got := make(chan string, 1)
	fakeCtrlDaemon(t, func(c net.Conn, line string) {
		got <- line
		_, _ = c.Write([]byte(`ok {"outcome":"emitted","text":"DAEMON HINT\n"}` + "\n"))
	})
	payload, _ := json.Marshal(map[string]any{
		"session_id": "thr-1", "turn_id": "turn-3", "cwd": sub, "hook_event_name": "UserPromptSubmit",
		"prompt": "fix `Cart.Total`",
	})
	var out bytes.Buffer
	if err := runCodexHookIO(bytes.NewReader(payload), &out); err != nil {
		t.Fatal(err)
	}
	var req contextHintRequest
	select {
	case line := <-got:
		if json.Unmarshal([]byte(strings.TrimPrefix(line, ctrlContextHintCommand)), &req) != nil || req.Host != "codex" || req.TurnID != "turn-3" {
			t.Fatalf("daemon received %q", line)
		}
	default:
		t.Fatal("runCodexHookIO asked the daemon nothing")
	}
	var doc codexSpecific
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.HookSpecificOutput.AdditionalContext != "DAEMON HINT" {
		t.Fatalf("stdout = %q (%v)", out.String(), err)
	}
}

// Codex's hint handlers are opt-in exactly as Claude Code's: absent from a plain
// plan, added by --context, kept by a later plain plan, and removed alone by
// `uninstall --only context`.
func TestCodexContextEntries_OptInAndScopedUninstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	target := codexHooksTarget
	target.pathFn = func() (string, error) { return path, nil }
	isHint := func(e hookEntry) bool { return slices.Contains(contextHintEvents, e.event) }

	_, plain, _, err := hookPlan(target, "/opt/plumb", false)
	if err != nil || slices.ContainsFunc(plain, isHint) {
		t.Fatalf("plain plan has hint entries (%v)", err)
	}
	_, opted, _, _ := hookPlan(target, "/opt/plumb", true)
	if n := len(opted); n != 4 || !slices.ContainsFunc(opted, isHint) {
		t.Fatalf("--context plan has %d entries, want 4 including the hints", n)
	}
	for _, e := range opted {
		if !codexHookOwned(e.event, e.handler) {
			t.Errorf("%s handler is not recognised as plumb's", e.event)
		}
		if isHint(e) && e.handler["statusMessage"] != nil {
			t.Errorf("%s carries a statusMessage Codex would show on every prompt", e.event)
		}
	}
	if _, err := installHooksAt(path, opted, target.ours); err != nil {
		t.Fatal(err)
	}
	if _, again, _, err := hookPlan(target, "/opt/plumb", false); err != nil || len(again) != 4 {
		t.Fatalf("plain plan after opting in kept %d entries (%v), want 4", len(again), err)
	}

	scope, _ := hooksUninstallScope("context")
	if _, err := removeHooksAt(path, scope.ownership(target.ours)); err != nil {
		t.Fatal(err)
	}
	states, err := hookStatesAt(path, opted, target.ours)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		installed := s.state == hookStateInstalled
		if isHint(s.entry) == installed {
			t.Errorf("after uninstall --only context, %s state = %q", s.entry.event, s.state)
		}
	}
}
