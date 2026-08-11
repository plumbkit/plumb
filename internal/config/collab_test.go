package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCollabProject(t *testing.T, body string) string {
	t.Helper()
	ws := t.TempDir()
	plumbDir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plumbDir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestCollab_Defaults(t *testing.T) {
	d := Defaults()
	if !d.Collab.PeerAwareness {
		t.Error("collab.peer_awareness should default to true")
	}
	if d.Collab.HintBudgetBytes != 512 {
		t.Errorf("collab.hint_budget_bytes default = %d, want 512", d.Collab.HintBudgetBytes)
	}
	if d.Collab.Intents {
		t.Error("collab.intents should default to false (opt-in)")
	}
	if !d.Collab.Mailbox {
		t.Error("collab.mailbox should default to TRUE — same-project agent messaging is on by default")
	}
	if d.Collab.CrossProject {
		t.Error("collab.cross_project should default to false — reaching another project is opt-in")
	}
	if d.Collab.MaxExchanges != 10 {
		t.Errorf("collab.max_exchanges default = %d, want 10", d.Collab.MaxExchanges)
	}
	if d.Collab.ChatBudgetBytes != 2048 {
		t.Errorf("collab.chat_budget_bytes default = %d, want 2048", d.Collab.ChatBudgetBytes)
	}
	if d.Collab.MaxWaitSeconds != 55 {
		t.Errorf("collab.max_wait_seconds default = %d, want 55", d.Collab.MaxWaitSeconds)
	}
	if d.Collab.IntentTTLMinutes != 120 {
		t.Errorf("collab.intent_ttl_minutes default = %d, want 120", d.Collab.IntentTTLMinutes)
	}
	if d.Collab.KnowledgeHandoff {
		t.Error("collab.knowledge_handoff should default to false (opt-in)")
	}
}

// TestLoadProject_CollabChannelSwitchesNeedTrust is the untrusted half of the
// boundary.
//
// A project's .plumb/config.toml is an untrusted surface — a cloned repository
// ships it — and each of these four switches opens a cross-agent CHANNEL. A
// channel a repository can open is a channel it can use: a payload that has
// already steered one agent through some other file in the repo can leave
// instructions for the next session. cross_project is the plainest case, because
// its own contract is that receiving is the RECIPIENT's decision, which holds
// only while the recipient's project file cannot set it unasked.
//
// So an UNAPPROVED request is refused in both directions: a project can neither
// enable a channel the user left off nor disable one they left on. The trusted
// half is TestLoadProject_TrustedCollabChannelSwitchesAreHonoured.
func TestLoadProject_CollabChannelSwitchesNeedTrust(t *testing.T) {
	tempTrustStore(t) // an empty store: nothing is trusted
	const allOn = "[collab]\nintents = true\nmailbox = true\ncross_project = true\nknowledge_handoff = true\n"
	const allOff = "[collab]\nintents = false\nmailbox = false\ncross_project = false\nknowledge_handoff = false\n"

	t.Run("a project cannot enable a channel the user left off", func(t *testing.T) {
		base := Defaults()
		base.Collab.Intents = false
		base.Collab.Mailbox = false
		base.Collab.CrossProject = false
		base.Collab.KnowledgeHandoff = false

		got, err := LoadProject(base, writeCollabProject(t, allOn))
		if err != nil {
			t.Fatal(err)
		}
		if got.Collab.CrossProject {
			t.Error("a cloned repo enabling cross_project would defeat the guarantee that " +
				"another project cannot inject text into this one uninvited")
		}
		if got.Collab.Mailbox || got.Collab.Intents || got.Collab.KnowledgeHandoff {
			t.Errorf("a project must not open a channel the user switched off: "+
				"mailbox=%v intents=%v knowledge_handoff=%v",
				got.Collab.Mailbox, got.Collab.Intents, got.Collab.KnowledgeHandoff)
		}
	})

	t.Run("a project cannot disable a channel either", func(t *testing.T) {
		// Forcing to the global value is symmetric on purpose. A project silently
		// closing a channel would make peer coordination fail in ways neither agent
		// could explain, and the switch is the user's to hold in both directions.
		base := Defaults()
		base.Collab.Intents = true
		base.Collab.Mailbox = true
		base.Collab.CrossProject = true
		base.Collab.KnowledgeHandoff = true

		got, err := LoadProject(base, writeCollabProject(t, allOff))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Collab.Intents || !got.Collab.Mailbox || !got.Collab.CrossProject || !got.Collab.KnowledgeHandoff {
			t.Error("the channel switches must take the global value in both directions")
		}
	})
}

// TestLoadProject_CollabTuningStaysProjectOverridable is the other half of the
// contract, and the reason the fix above is scoped to the switches alone: tuning
// cannot OPEN anything. A project saying "smaller hints here" is a legitimate
// per-project preference; one saying "open a cross-agent channel here" is not.
func TestLoadProject_CollabTuningStaysProjectOverridable(t *testing.T) {
	base := Defaults()
	ws := writeCollabProject(t, "[collab]\nintent_ttl_minutes = 30\nhint_budget_bytes = 256\n"+
		"max_exchanges = 3\nchat_budget_bytes = 512\nmax_wait_seconds = 10\npeer_awareness = false\n")
	got, err := LoadProject(base, ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"intent_ttl_minutes", got.Collab.IntentTTLMinutes, 30},
		{"hint_budget_bytes", got.Collab.HintBudgetBytes, 256},
		{"max_exchanges", got.Collab.MaxExchanges, 3},
		{"chat_budget_bytes", got.Collab.ChatBudgetBytes, 512},
		{"max_wait_seconds", got.Collab.MaxWaitSeconds, 10},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d — tuning must stay project-overridable", c.name, c.got, c.want)
		}
	}
	if got.Collab.PeerAwareness {
		t.Error("peer_awareness must stay project-overridable: it surfaces what the daemon " +
			"already observed in this project and opens no channel")
	}
}

func TestValidateCollab_NegativeTTLRejected(t *testing.T) {
	ws := writeCollabProject(t, "[collab]\nintent_ttl_minutes = -5\n")
	if _, err := LoadProject(Defaults(), ws); err == nil {
		t.Fatal("expected validation error for negative collab.intent_ttl_minutes")
	}
}

// TestLoadProject_CollabOverridesBothDirections asserts the generated_summaries
// precedent for the new [collab] bool: a project may DISABLE peer_awareness under
// a global opt-in, and ENABLE it under a global opt-out. Both win over global.
func TestLoadProject_CollabOverridesBothDirections(t *testing.T) {
	t.Run("project off under global on", func(t *testing.T) {
		base := Defaults() // peer_awareness = true
		ws := writeCollabProject(t, "[collab]\npeer_awareness = false\n")
		got, err := LoadProject(base, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got.Collab.PeerAwareness {
			t.Error("project peer_awareness=false must override global true")
		}
	})

	t.Run("project on under global off", func(t *testing.T) {
		base := Defaults()
		base.Collab.PeerAwareness = false // global opt-out
		ws := writeCollabProject(t, "[collab]\npeer_awareness = true\n")
		got, err := LoadProject(base, ws)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Collab.PeerAwareness {
			t.Error("project peer_awareness=true must override global false")
		}
	})

	t.Run("absent key keeps global", func(t *testing.T) {
		base := Defaults()
		base.Collab.PeerAwareness = false
		ws := writeCollabProject(t, "[collab]\nhint_budget_bytes = 256\n")
		got, err := LoadProject(base, ws)
		if err != nil {
			t.Fatal(err)
		}
		if got.Collab.PeerAwareness {
			t.Error("absent peer_awareness must keep the global value (false)")
		}
		if got.Collab.HintBudgetBytes != 256 {
			t.Errorf("hint_budget_bytes = %d, want 256", got.Collab.HintBudgetBytes)
		}
	})
}

func TestValidateCollab_NegativeBudgetRejected(t *testing.T) {
	ws := writeCollabProject(t, "[collab]\nhint_budget_bytes = -1\n")
	if _, err := LoadProject(Defaults(), ws); err == nil {
		t.Fatal("expected validation error for negative collab.hint_budget_bytes")
	}
}

// TestLoadProject_TrustedCollabChannelSwitchesAreHonoured is the capability half
// of the boundary: a user who has approved this project's exact request gets it.
//
// This is what makes per-workspace chat settings legitimate. "Per workspace" and
// "the repository decides" are different things — the repo may ASK, and the
// answer is the user's, recorded out of band in plumb's data dir where a clone
// cannot forge it, and bound to the exact content approved.
func TestLoadProject_TrustedCollabChannelSwitchesAreHonoured(t *testing.T) {
	s := tempTrustStore(t)
	ws := projectConfigWorkspace(t, "[collab]\nmailbox = true\ncross_project = true\n"+
		"intents = true\nknowledge_handoff = true\n")
	trustWorkspace(t, s, ws)

	base := Defaults()
	base.Collab.Mailbox = false
	base.Collab.CrossProject = false
	base.Collab.Intents = false
	base.Collab.KnowledgeHandoff = false

	got, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if !got.Collab.CrossProject {
		t.Error("an approved project asking for cross_project must get it — that is the whole point of trust")
	}
	if !got.Collab.Mailbox || !got.Collab.Intents || !got.Collab.KnowledgeHandoff {
		t.Errorf("approved switches not honoured: mailbox=%v intents=%v knowledge_handoff=%v",
			got.Collab.Mailbox, got.Collab.Intents, got.Collab.KnowledgeHandoff)
	}
}

// TestCollabPolicySpec_GatesSwitchesAndFreesTuning pins WHICH keys reach the
// trust gate. Getting this wrong in either direction is a real defect: a gated
// key missing from the spec is honoured with no approval and no disclosure, and
// a free key in the spec makes a project demand `plumb trust` to set a byte
// budget.
func TestCollabPolicySpec_GatesSwitchesAndFreesTuning(t *testing.T) {
	raw := map[string]any{"collab": map[string]any{
		"mailbox": true, "cross_project": true, "intents": true, "knowledge_handoff": true,
		"peer_awareness": false, "hint_budget_bytes": int64(256), "intent_ttl_minutes": int64(30),
		"max_exchanges": int64(3), "chat_budget_bytes": int64(512), "max_wait_seconds": int64(10),
	}}
	got := map[string]bool{}
	for _, k := range projectPolicySpecFrom(raw).Keys() {
		got[k] = true
	}
	for _, k := range []string{"collab.mailbox", "collab.cross_project", "collab.intents", "collab.knowledge_handoff"} {
		if !got[k] {
			t.Errorf("%s must be gated on trust — it opens a cross-agent channel", k)
		}
	}
	for _, k := range []string{
		"collab.peer_awareness", "collab.hint_budget_bytes", "collab.intent_ttl_minutes",
		"collab.max_exchanges", "collab.chat_budget_bytes", "collab.max_wait_seconds",
	} {
		if got[k] {
			t.Errorf("%s must NOT need trust — it opens nothing, and demanding approval to tune a size is friction for no safety", k)
		}
	}
}

// TestCollabPolicySpec_UnknownKeyIsGated is the reason the free list is an
// ALLOW-list. The channel switches were project-settable in the first place
// because [collab] was added and nobody re-derived which of its keys grant
// capability. A key plumb does not recognise must therefore fail CLOSED, so the
// next field added forces that question instead of defaulting to free.
func TestCollabPolicySpec_UnknownKeyIsGated(t *testing.T) {
	raw := map[string]any{"collab": map[string]any{"some_future_switch": true}}
	spec := projectPolicySpecFrom(raw)
	if len(spec) != 1 || spec[0].Key != "collab.some_future_switch" {
		t.Fatalf("an unrecognised [collab] key must be gated; spec = %v", spec.Keys())
	}
	if spec[0].Warning(Defaults()) == "" {
		t.Error("a gated unknown key must still warn, so `plumb trust` cannot approve it silently")
	}
}

// TestCollabPolicySpec_CaseInsensitiveKeysAreGated closes the hole that made the
// [lsp] free list an allow-list: go-toml/v2 binds a TOML key to a struct field
// case-insensitively, so `Cross_Project = true` reaches CrossProject. An
// exact-match gate would let it through unseen — absent from the spec, the
// disclosure, and the trust hash.
func TestCollabPolicySpec_CaseInsensitiveKeysAreGated(t *testing.T) {
	raw := map[string]any{"collab": map[string]any{"Cross_Project": true, "MAILBOX": true}}
	if got := len(projectPolicySpecFrom(raw)); got != 2 {
		t.Errorf("case-variant channel switches must still be gated; got %d entries", got)
	}
	// ...and a case-variant FREE key must still be recognised as free.
	freeRaw := map[string]any{"collab": map[string]any{"Hint_Budget_Bytes": int64(256)}}
	if got := projectPolicySpecFrom(freeRaw); len(got) != 0 {
		t.Errorf("a case-variant tuning key must stay free; got %v", got.Keys())
	}
}

// TestLoadProject_CaseVariantTableCannotBypassTheGate reproduces the bypass an
// independent review found in the first version of this change.
//
// go-toml/v2 folds a TABLE name to a struct field exactly as it folds a field
// name, so `[COLLAB]` decodes into Config.Collab — but the spec was extracted
// with an exact `raw["collab"]` lookup, so those keys reached the merged config
// while being absent from the spec, the disclosure, and the policy hash.
//
// It was invisible while nothing else in the file was gated, because an empty
// spec still forces everything back. It became live the moment ANY key was
// trusted, since that skips forceCapabilityFieldsToBase wholesale.
func TestLoadProject_CaseVariantTableCannotBypassTheGate(t *testing.T) {
	s := tempTrustStore(t)
	ws := projectConfigWorkspace(t, "[collab]\nintents = true\n\n[COLLAB]\ncross_project = true\nknowledge_handoff = true\n")
	trustWorkspace(t, s, ws) // the user approves whatever this file asks for

	spec, err := ProjectPolicySpecFor(ws)
	if err != nil {
		t.Fatal(err)
	}
	keys := strings.Join(spec.Keys(), ",")
	for _, want := range []string{"collab.cross_project", "collab.knowledge_handoff"} {
		if !strings.Contains(keys, want) {
			t.Errorf("%s came from a case-variant table and must still be disclosed and hashed; spec = %s", want, keys)
		}
	}
}

// TestTrustGrant_CaseVariantTableAppendedLaterLapsesTheGrant is the TOCTOU half.
// A repository trusted for one thing must not be able to append a channel switch
// afterwards and have it honoured under the old grant.
func TestTrustGrant_CaseVariantTableAppendedLaterLapsesTheGrant(t *testing.T) {
	s := tempTrustStore(t)
	ws := projectConfigWorkspace(t, "[git]\nallow_push = true\n")
	trustWorkspace(t, s, ws) // approved: [git] only

	// The repository rewrites itself, adding a channel switch in a case variant.
	writeProjectConfig(t, ws, "[git]\nallow_push = true\n\n[COLLAB]\ncross_project = true\n")

	base := Defaults()
	base.Collab.CrossProject = false
	got, err := LoadProject(base, ws)
	if err != nil {
		t.Fatal(err)
	}
	if got.Collab.CrossProject {
		t.Error("appending a channel switch after approval must lapse the grant, not ride on it")
	}
}
