package cli

// serve_resume_proxy_test.go — what the proxy does with the credential, behind a scripted
// daemon: it announces that it consumes the key, records the credential once a
// conversation is linked, presents a stored one only when a replacement serve is entitled
// to, and leaves a stored entry alone when it cannot tell why a presentation failed.

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// rcLateDaemon is rcDaemon that also discloses late on a "late" tool call: a credential in
// an ordinary tool result, which is how a connection that converged on the C3 retry (no
// initialize left to ride) learns its credential.
func rcLateDaemon(t *testing.T, init, successor, late string) func(e rpcEnvelope, frame []byte) ([][]byte, bool) {
	t.Helper()
	base := rcDaemon(t, "established", init, successor)
	return func(e rpcEnvelope, frame []byte) ([][]byte, bool) {
		if e.isRequest() && toolName(frame) == "late" {
			return [][]byte{rcToolResult(t, e.ID, "late", late)}, false
		}
		return base(e, frame)
	}
}

// presentedKey is the credential key as a member name, which is not the consumer key
// that merely begins with it.
var presentedKey = regexp.MustCompile(`"` + regexp.QuoteMeta(mcp.MetaResumeCredentialKey) + `"`)

func TestResumeProxy_AnnouncesThatItConsumes(t *testing.T) {
	t.Parallel()
	// Daemon one dies on the "crash" call so the handshake is replayed to daemon two.
	d1, p1 := newScriptedDaemon(func(e rpcEnvelope, frame []byte) ([][]byte, bool) {
		if toolName(frame) == "crash" {
			return nil, true
		}
		return rcDaemon(t, "established", "", "")(e, frame)
	})
	d2, p2 := newScriptedDaemon(rcDaemon(t, "restored", "", ""))
	s := startRCServe(t, nil, p1, p2)
	s.call("crash", nil, nil)
	s.call("ping_me", nil, nil)

	for name, d := range map[string]*scriptedDaemon{"the first connect": d1, "the replayed handshake": d2} {
		var init []byte
		for _, f := range d.received() {
			if parseEnvelope(f).Method == "initialize" {
				init = f
				break
			}
		}
		if init == nil {
			t.Fatalf("%s: no initialize reached the daemon", name)
		}
		var req struct {
			Params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		if err := json.Unmarshal(init, &req); err != nil {
			t.Fatal(err)
		}
		if got := string(req.Params.Meta[mcp.MetaResumeCredentialConsumerKey]); got != "1" {
			t.Errorf("%s: the initialize _meta announces the consumer as %q, want the number 1; "+
				"without it the daemon discloses nothing", name, got)
		}
	}
}

// The credential is not keyed to a conversation until session_start links one, and a
// session_start that failed linked nothing.
func TestResumeProxy_RecordsTheCredentialOnceAConversationIsLinked(t *testing.T) {
	t.Parallel()
	store := rcStore(t, t.TempDir(), "scope-a")
	_, p := newScriptedDaemon(rcDaemon(t, "established", rcSecret(1), ""))
	s := startRCServe(t, store, p)

	if store.count() != 0 {
		t.Fatal("the store holds an entry before any conversation is linked")
	}
	s.call("session_start", rcHooked("conv", map[string]any{"workspace": "fail"}), nil)
	s.call("session_start", map[string]any{"workspace": "/somewhere"}, nil) // names no conversation
	if store.count() != 0 {
		t.Fatalf("the store holds %d entries after a failed and an unlinked session_start", store.count())
	}
	s.call("session_start", rcHooked("conv", nil), nil)
	if e, ok := store.load("conv"); !ok || e.Secret != rcSecret(1) || e.Generation != 1 {
		t.Fatalf("store = %+v (found %v); want the disclosed credential keyed to conv", e, ok)
	}
	s.assertClientNeverSaw(rcSecret(1))
}

// The conversation's own session_start is stored under the conversation half of a
// "<conversation>/<agent>" id, which is what the daemon links.
func TestResumeProxy_KeysTheEntryToTheConversationHalf(t *testing.T) {
	t.Parallel()
	store := rcStore(t, t.TempDir(), "scope-a")
	_, p := newScriptedDaemon(rcDaemon(t, "established", rcSecret(1), ""))
	s := startRCServe(t, store, p)
	s.call("session_start", rcHooked("conv/agent-7", nil), nil)
	if _, ok := store.load("conv"); !ok {
		t.Error("no entry under the conversation half of conv/agent-7")
	}
	if _, ok := store.load("conv/agent-7"); ok {
		t.Error("an entry was keyed to the agent half as well")
	}
}

// The replacement serve: it presents what the previous process stored, keeps the successor
// the daemon returns, and presents once.
func TestResumeProxy_AReplacementPresentsAndStoresTheSuccessor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := rcStore(t, dir, "scope-a").put("conv", rcSecret(10)); err != nil { // the previous process's
		t.Fatal(err)
	}
	store := rcStore(t, dir, "scope-a")
	d, p := newScriptedDaemon(rcLateDaemon(t, rcSecret(1), rcSecret(11), rcSecret(12)))
	s := startRCServe(t, store, p)

	s.call("session_start", rcHooked("conv", nil), nil)
	calls := d.calls("session_start")
	if got := presentedCredential(t, calls[0]); got != rcSecret(10) {
		t.Fatalf("the replacement presented %q, want the credential its predecessor stored", got)
	}
	if e, _ := store.load("conv"); e.Secret != rcSecret(11) || e.Generation != 2 {
		t.Fatalf("store = %+v, want the successor at generation 2: the presented credential is spent", e)
	}

	s.call("session_start", rcHooked("conv", nil), nil)
	if got := presentedCredential(t, d.calls("session_start")[1]); got != "" {
		t.Errorf("a second session_start presented %q; a replacement presents once", got)
	}
	// This process owns the entry now, so a later disclosure (a late C3 one) replaces it.
	s.call("late", nil, nil)
	if e, _ := store.load("conv"); e.Secret != rcSecret(12) || e.Generation != 3 {
		t.Errorf("store = %+v, want the late disclosure at generation 3", e)
	}
	s.assertClientNeverSaw(rcSecret(1), rcSecret(10), rcSecret(11), rcSecret(12))
}

// The daemon does not say WHY it did not rotate: refused as superseded or revoked, or unable
// to finish (a live session still holds the predecessor's ID). Overwriting the stored entry
// with this process's fresh credential would trade a retry that can still restore the
// predecessor's mail and threads for one that restores a name-only fork.
func TestResumeProxy_ARefusedPresentationLeavesTheStoredEntryAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := rcStore(t, dir, "scope-a").put("conv", rcSecret(10)); err != nil {
		t.Fatal(err)
	}
	store := rcStore(t, dir, "scope-a")
	d, p := newScriptedDaemon(rcLateDaemon(t, rcSecret(1), "", rcSecret(3)))
	s := startRCServe(t, store, p)

	s.call("session_start", rcHooked("conv", nil), nil)
	if got := presentedCredential(t, d.calls("session_start")[0]); got != rcSecret(10) {
		t.Fatalf("precondition: the replacement presented %q, want the stored credential", got) // positive control
	}
	if e, _ := store.load("conv"); e.Secret != rcSecret(10) || e.Generation != 1 {
		t.Fatalf("store = %+v after a presentation that earned no successor; the predecessor's entry must stand", e)
	}
	// Not even this process's later disclosures (a daemon restart re-mints) displace it.
	s.call("late", nil, nil)
	if e, _ := store.load("conv"); e.Secret != rcSecret(10) {
		t.Errorf("store = %+v; the entry this process was refused on was overwritten", e)
	}
}

// A process that presented, and whose session_start then failed, has used its one attempt.
func TestResumeProxy_APresentationIsAttemptedOncePerConversation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := rcStore(t, dir, "scope-a").put("conv", rcSecret(10)); err != nil {
		t.Fatal(err)
	}
	d, p := newScriptedDaemon(rcDaemon(t, "established", rcSecret(1), ""))
	s := startRCServe(t, rcStore(t, dir, "scope-a"), p)
	s.call("session_start", rcHooked("conv", map[string]any{"workspace": "fail"}), nil)
	s.call("session_start", rcHooked("conv", nil), nil)
	calls := d.calls("session_start")
	if presentedCredential(t, calls[0]) != rcSecret(10) || presentedCredential(t, calls[1]) != "" {
		t.Errorf("presentations = %q then %q, want the credential once and then nothing",
			presentedCredential(t, calls[0]), presentedCredential(t, calls[1]))
	}
}

// Each of these is a session_start the proxy must not attach a stored credential to. A
// conversation id and a stamp are strings a model can type, so the only thing that
// entitles a presentation is the hook's proof, verified against the stamp.
func TestResumeProxy_PresentsOnlyWhenEntitled(t *testing.T) {
	t.Parallel()
	const conv = "conv"
	type scenario struct {
		name     string
		recovery string // what the daemon's initialize result says; "" is an old daemon
		scope    string // the scope the running proxy's store is bound to
		args     map[string]any
		key      func() ([]byte, error) // the proxy's key reader; nil is the test key
		seed     bool
		noStore  bool
		noKey    bool
		want     bool
	}
	hooked := rcHooked(conv, nil)
	withProof := func(stamp, proof string) map[string]any {
		return map[string]any{"session_id": conv, mcp.ArgLogicalAgentKey: stamp, mcp.ArgHookProofKey: proof}
	}
	for _, c := range []scenario{
		{name: "an entitled, hooked replacement (the positive control)", recovery: "established", scope: "scope-a", args: hooked, seed: true, want: true},
		{
			name: "hooked, under the declared stamp key", recovery: "established", scope: "scope-a", seed: true, want: true,
			args: map[string]any{"session_id": conv, mcp.ArgLogicalAgentDeclaredKey: conv, mcp.ArgHookProofKey: resumeProofFor(rcTestKey, conv)},
		},
		// The reviewer's repros: the model typed the conversation, with or without a stamp.
		{name: "an unhooked client that names the conversation", recovery: "established", scope: "scope-a", args: map[string]any{"session_id": conv}, seed: true},
		{name: "a typed stamp equal to the conversation, with no proof", recovery: "established", scope: "scope-a", args: map[string]any{"session_id": conv, mcp.ArgLogicalAgentKey: conv}, seed: true},
		{name: "a typed stamp in the declared key, with no proof", recovery: "established", scope: "scope-a", args: map[string]any{"session_id": conv, mcp.ArgLogicalAgentDeclaredKey: conv}, seed: true},
		{name: "a proof for a different stamp", recovery: "established", scope: "scope-a", args: withProof(conv, resumeProofFor(rcTestKey, "other")), seed: true},
		{name: "a proof made with another user's key", recovery: "established", scope: "scope-a", args: withProof(conv, resumeProofFor(rcOtherKey, conv)), seed: true},
		{name: "an invalid proof", recovery: "established", scope: "scope-a", args: withProof(conv, "not-a-proof"), seed: true},
		{name: "a proof that is not base64url", recovery: "established", scope: "scope-a", args: withProof(conv, "!!!"+resumeProofFor(rcTestKey, conv)), seed: true},
		{name: "an empty proof", recovery: "established", scope: "scope-a", args: withProof(conv, ""), seed: true},
		{name: "a proof but no stamp", recovery: "established", scope: "scope-a", args: map[string]any{"session_id": conv, mcp.ArgHookProofKey: resumeProofFor(rcTestKey, conv)}, seed: true},
		{name: "a valid proof for another conversation's stamp", recovery: "established", scope: "scope-a", args: withProof("other", resumeProofFor(rcTestKey, "other")), seed: true},
		{
			name: "a proxy that cannot read the proof key", recovery: "established", scope: "scope-a", args: hooked, seed: true,
			key: func() ([]byte, error) { return nil, os.ErrNotExist },
		},
		{name: "a proxy with no way to verify a proof", recovery: "established", scope: "scope-a", args: hooked, seed: true, noKey: true},
		{name: "no stored entry for the conversation", recovery: "established", scope: "scope-a", args: hooked},
		{name: "the daemon restored the identity from the proxy credential", recovery: "restored", scope: "scope-a", args: hooked, seed: true},
		{name: "a daemon that predates the identity snapshot", recovery: "", scope: "scope-a", args: hooked, seed: true},
		{name: "a degraded daemon connection", recovery: "degraded", scope: "scope-a", args: hooked, seed: true},
		{name: "stamped as a subagent of the conversation", recovery: "established", scope: "scope-a", args: rcHooked(conv+"/agent-1", nil), seed: true},
		{name: "an entry written for a different daemon", recovery: "established", scope: "scope-b", args: hooked, seed: true},
		{name: "no session_id at all", recovery: "established", scope: "scope-a", args: map[string]any{}, seed: true},
		{name: "a proxy with no store", recovery: "established", scope: "scope-a", args: hooked, seed: true, noStore: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if c.seed {
				if _, err := rcStore(t, dir, "scope-a").put(conv, rcSecret(10)); err != nil {
					t.Fatal(err)
				}
			}
			var store *resumeStore
			if !c.noStore {
				store = rcStore(t, dir, c.scope)
			}
			d, p := newScriptedDaemon(rcDaemon(t, c.recovery, rcSecret(1), ""))
			s := startRCServe(t, store, p)
			if c.key != nil {
				s.h.proxy.rc.proofKey = c.key
			}
			if c.noKey {
				s.h.proxy.rc.proofKey = nil
			}
			s.call("session_start", c.args, nil)
			got := presentedCredential(t, d.calls("session_start")[0])
			if c.want && got != rcSecret(10) {
				t.Fatalf("presented %q, want the stored credential", got)
			}
			if !c.want && got != "" {
				t.Fatalf("presented a credential (%q…) to a connection it must not go to", got[:9])
			}
		})
	}
}

// Another conversation's typed claim must not get a credential filed under it either:
// the credential a connection is issued is its own, and a model that names conversation A
// links nothing, so what the daemon discloses to B's connection never lands in A's entry.
func TestResumeProxy_AnUnverifiedClaimFilesNothingUnderTheConversation(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		"an unhooked client":                   {"session_id": "conv-a"},
		"a typed stamp":                        {"session_id": "conv-a", mcp.ArgLogicalAgentKey: "conv-a"},
		"a proof for the wrong stamp":          {"session_id": "conv-a", mcp.ArgLogicalAgentKey: "conv-a", mcp.ArgHookProofKey: resumeProofFor(rcTestKey, "conv-b")},
		"a proof made with another user's key": {"session_id": "conv-a", mcp.ArgLogicalAgentKey: "conv-a", mcp.ArgHookProofKey: resumeProofFor(rcOtherKey, "conv-a")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if _, err := rcStore(t, dir, "scope-a").put("conv-a", rcSecret(10)); err != nil { // conversation A's entry
				t.Fatal(err)
			}
			store := rcStore(t, dir, "scope-a")
			d, p := newScriptedDaemon(rcLateDaemon(t, rcSecret(1), rcSecret(2), rcSecret(3))) // B's own credentials
			s := startRCServe(t, store, p)

			s.call("session_start", args, nil)
			s.call("late", nil, nil)
			if got := presentedCredential(t, d.calls("session_start")[0]); got != "" {
				t.Errorf("presented A's credential %q… on a claim nothing verified", got[:9])
			}
			if e, ok := store.load("conv-a"); !ok || e.Secret != rcSecret(10) || e.Generation != 1 {
				t.Errorf("A's entry = %+v (found %v); B's credentials were filed under it", e, ok)
			}
			if store.count() != 1 {
				t.Errorf("the store holds %d entries, want only A's", store.count())
			}
			s.assertClientNeverSaw(rcSecret(1), rcSecret(2), rcSecret(3), rcSecret(10))
		})
	}
}

// The proof is for the proxy: whatever its verdict, it never reaches the daemon, and the
// rewrite that removes it takes nothing else.
func TestResumeProxy_TheHookProofIsStrippedBeforeTheDaemon(t *testing.T) {
	t.Parallel()
	good := resumeProofFor(rcTestKey, "conv")
	for name, args := range map[string]map[string]any{
		"a verified proof":        rcHooked("conv", map[string]any{"workspace": "/w", "name": "n"}),
		"an invalid proof":        {"session_id": "conv", mcp.ArgLogicalAgentKey: "conv", mcp.ArgHookProofKey: "garbage", "workspace": "/w", "name": "n"},
		"a proof for another one": {"session_id": "conv", mcp.ArgLogicalAgentKey: "conv", mcp.ArgHookProofKey: resumeProofFor(rcTestKey, "x"), "workspace": "/w", "name": "n"},
		"a proof and no stamp":    {"session_id": "conv", mcp.ArgHookProofKey: good, "workspace": "/w", "name": "n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if _, err := rcStore(t, dir, "scope-a").put("conv", rcSecret(10)); err != nil {
				t.Fatal(err)
			}
			d, p := newScriptedDaemon(rcDaemon(t, "established", rcSecret(1), ""))
			s := startRCServe(t, rcStore(t, dir, "scope-a"), p)
			s.call("session_start", args, nil)

			frame := d.calls("session_start")[0]
			if strings.Contains(string(frame), mcp.ArgHookProofKey) || strings.Contains(string(frame), good) {
				t.Fatalf("the proof reached the daemon:\n%s", frame)
			}
			var req struct {
				Params struct {
					Arguments map[string]json.RawMessage `json:"arguments"`
				} `json:"params"`
			}
			if err := json.Unmarshal(frame, &req); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"session_id", "workspace", "name"} {
				if _, ok := req.Params.Arguments[k]; !ok {
					t.Errorf("the strip lost the %q argument: %s", k, frame)
				}
			}
			if _, ok := args[mcp.ArgLogicalAgentKey]; ok {
				if string(req.Params.Arguments[mcp.ArgLogicalAgentKey]) != `"conv"` {
					t.Errorf("the strip lost the stamp: %s", frame)
				}
			}
			// The proof alone is stripped for a verified claim too, and the credential the
			// proxy attaches in its place still goes out: the rewrite did not undo itself.
			if name == "a verified proof" && presentedCredential(t, frame) != rcSecret(10) {
				t.Errorf("the verified claim was not presented after the proof was stripped: %s", frame)
			}
		})
	}
}

// A connection linked to one conversation is not handed another's credential by a later call.
func TestResumeProxy_ALinkedConnectionDoesNotPresentForAnotherConversation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := rcStore(t, dir, "scope-a").put("conv-b", rcSecret(10)); err != nil {
		t.Fatal(err)
	}
	d, p := newScriptedDaemon(rcDaemon(t, "established", rcSecret(1), ""))
	s := startRCServe(t, rcStore(t, dir, "scope-a"), p)
	s.call("session_start", rcHooked("conv-a", nil), nil)
	s.call("session_start", rcHooked("conv-b", nil), nil)
	if got := presentedCredential(t, d.calls("session_start")[1]); got != "" {
		t.Errorf("a connection already linked to conv-a presented conv-b's credential %q…", got[:9])
	}
}

// The proxy is the sole presenter: a credential a client put in the request _meta is not forwarded.
func TestResumeProxy_AClientSuppliedCredentialIsNotForwarded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := rcStore(t, dir, "scope-a").put("conv", rcSecret(10)); err != nil {
		t.Fatal(err)
	}
	attacker := map[string]any{mcp.MetaResumeCredentialKey: rcSecret(66), mcp.MetaLogicalAgentKey: "conv"}

	// No stored entry for this conversation: the client's value is removed, nothing replaces it.
	d, p := newScriptedDaemon(rcDaemon(t, "established", rcSecret(1), ""))
	s := startRCServe(t, rcStore(t, dir, "scope-a"), p)
	s.call("session_start", map[string]any{"session_id": "unstored"}, attacker)
	frame := d.calls("session_start")[0]
	if got := presentedCredential(t, frame); got != "" {
		t.Errorf("the client's credential reached the daemon: %q…", got[:9])
	}
	if meta := requestMeta(t, frame); string(meta[mcp.MetaLogicalAgentKey]) != `"conv"` {
		t.Errorf("the rewrite lost the client's other _meta: %v", meta)
	}

	// With a stored entry the daemon is given the STORED credential, not the client's.
	d2, p2 := newScriptedDaemon(rcDaemon(t, "established", rcSecret(1), ""))
	s2 := startRCServe(t, rcStore(t, dir, "scope-a"), p2)
	s2.call("session_start", rcHooked("conv", nil), attacker)
	if got := presentedCredential(t, d2.calls("session_start")[0]); got != rcSecret(10) {
		t.Errorf("presented %q, want the stored credential in place of the client's", got)
	}
}

// An old daemon: it discloses nothing and ignores what it does not know. The proxy stores
// nothing, never names the credential key in a request, and the flag is the only thing
// it said. (Its answer to a presentation is the same: an unknown request _meta key is read
// as a map and ignored.)
func TestResumeProxy_AnOldDaemonIsInert(t *testing.T) {
	t.Parallel()
	store := rcStore(t, t.TempDir(), "scope-a")
	d, p := newScriptedDaemon(rcDaemon(t, "established", "", ""))
	s := startRCServe(t, store, p)
	s.call("session_start", rcHooked("conv", nil), nil)
	s.call("whatever", nil, nil)
	if store.count() != 0 {
		t.Errorf("the store holds %d entries from a daemon that disclosed nothing", store.count())
	}
	for _, f := range d.received() {
		if presentedKey.Match(f) {
			t.Errorf("a request to a daemon that disclosed nothing names the credential key:\n%s", f)
		}
	}
	s.assertClientNeverSaw()
}
