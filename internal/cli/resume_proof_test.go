package cli

// resume_proof_test.go — the hook's proof of a conversation (resume_proof.go): the
// formula, the key file, the hook that writes the proof, and the hook-to-proxy path with
// the real key between them.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// isolateProofKey points the state directory at a fresh temporary one and returns the key
// path. It refuses to go on if the key would land anywhere else: the real one is the
// user's.
func isolateProofKey(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	path := resumeProofKeyPath()
	if !strings.HasPrefix(path, state) {
		t.Fatalf("the proof key path %q is outside the test's state directory %q; refusing to touch it", path, state)
	}
	return path
}

// The formula is a wire contract between the hook and the proxy, and between releases of
// each: a known answer pins it, computed independently of the code under test.
func TestResumeProof_KnownAnswer(t *testing.T) {
	t.Parallel()
	const want = "_lfUzxnpapt8ZD8rheO6B30F-qKEj3e-IKz-2VTuzo4" // HMAC-SHA256(0x5a*32, "resume-v1\x00conv-1"), base64url
	if got := resumeProofFor(rcTestKey, "conv-1"); got != want {
		t.Fatalf("proof = %q, want %q", got, want)
	}
}

func TestResumeProof_VerifiesOnlyItsOwnStampUnderItsOwnKey(t *testing.T) {
	t.Parallel()
	good := resumeProofFor(rcTestKey, "conv-1")
	for name, tc := range map[string]struct {
		key          []byte
		stamp, proof string
		want         bool
	}{
		"the proof for the stamp":       {rcTestKey, "conv-1", good, true},
		"a different stamp":             {rcTestKey, "conv-2", good, false},
		"a subagent's stamp":            {rcTestKey, "conv-1/agent-7", good, false},
		"another key":                   {rcOtherKey, "conv-1", good, false},
		"an empty stamp":                {rcTestKey, "", resumeProofFor(rcTestKey, ""), false},
		"an empty proof":                {rcTestKey, "conv-1", "", false},
		"a proof that is not base64url": {rcTestKey, "conv-1", "!!!" + good, false},
		"a truncated proof":             {rcTestKey, "conv-1", good[:len(good)-2], false},
		"a padded proof":                {rcTestKey, "conv-1", good + "=", false},
		"a proof with trailing text":    {rcTestKey, "conv-1", good + "AAAA", false},
		"a key that is too short":       {rcTestKey[:16], "conv-1", good, false},
		"no key":                        {nil, "conv-1", good, false},
		"the stamp itself as the proof": {rcTestKey, "conv-1", "conv-1", false},
	} {
		if got := verifyResumeProof(tc.key, tc.stamp, tc.proof); got != tc.want {
			t.Errorf("%s: verify = %v, want %v", name, got, tc.want)
		}
	}
}

func TestResumeProofKey_IsMadePrivateAndIsStable(t *testing.T) {
	path := isolateProofKey(t)
	if _, err := loadResumeProofKey(); err == nil {
		t.Fatal("the serve's read made or found a key that nothing created")
	}
	if _, err := os.Lstat(path); err == nil {
		t.Fatal("a read-only load created the key file")
	}
	key, err := ensureResumeProofKey()
	if err != nil || len(key) != resumeProofKeyLen {
		t.Fatalf("ensure = %d bytes, %v", len(key), err)
	}
	if bytes.Equal(key, make([]byte, resumeProofKeyLen)) {
		t.Fatal("the key is all zero")
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("key mode = %v (%v), want 0600", fi.Mode().Perm(), err)
		}
		if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("key directory mode = %v (%v), want 0700", fi.Mode().Perm(), err)
		}
	}
	again, err := ensureResumeProofKey()
	if err != nil || !bytes.Equal(again, key) {
		t.Errorf("a second ensure returned a different key (%v)", err)
	}
	if read, err := loadResumeProofKey(); err != nil || !bytes.Equal(read, key) {
		t.Errorf("the serve's read = %v, %v; want the key the hook made", read, err)
	}
}

// Several hooks run at once on the first call of a session; they must all end up with
// the one key that landed, and leave no temporary file behind.
func TestResumeProofKey_RacingCreatorsAgree(t *testing.T) {
	path := isolateProofKey(t)
	const n = 16
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys[i], errs[i] = ensureResumeProofKey()
		}()
	}
	close(start)
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("creator %d: %v", i, errs[i])
		}
		if !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("creator %d holds a different key from creator 0: the proofs they make cannot all verify", i)
		}
	}
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != resumeProofKeyFile {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("the key directory holds %v, want only %s", names, resumeProofKeyFile)
	}
}

func TestResumeProofKey_RefusesAKeyThatIsNotPrivateOrNotAKey(t *testing.T) {
	path := isolateProofKey(t)
	if _, err := ensureResumeProofKey(); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(path)

	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadResumeProofKey(); err == nil {
			t.Error("a world-readable key was accepted: it is not a secret any more")
		}
		if _, err := ensureResumeProofKey(); err == nil {
			t.Error("ensure accepted (or replaced) a world-readable key")
		}
		if now, _ := os.ReadFile(path); !bytes.Equal(now, good) {
			t.Error("ensure overwrote a key it refused")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, good[:resumeProofKeyLen-1], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadResumeProofKey(); err == nil {
		t.Error("a key of the wrong length was accepted")
	}

	// A symbolic link is not the key file, whatever it points at.
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if _, err := loadResumeProofKey(); err == nil {
		t.Error("a symbolic link was followed to a key")
	}
}

// hookedSessionStart runs the real PreToolUse core for a main-thread session_start of
// conv and returns the arguments the hook leaves for the client to send.
func hookedSessionStart(t *testing.T, daemon identityProbeRecord, agent, typed string) map[string]json.RawMessage {
	t.Helper()
	in := claudeHookInput{
		SessionID: "conv-1", AgentID: agent, ToolName: "mcp__plumb__session_start",
		ToolInput: json.RawMessage(`{"workspace":"/w"` + typed + `}`),
	}
	out, ok := claudePreToolUseOutput(in, noEnv, func() identityProbeRecord { return daemon })
	if !ok {
		t.Fatal("the hook left session_start unstamped")
	}
	args, _ := updatedInputOf(t, out)
	return args
}

func proofDaemon() identityProbeRecord {
	return identityProbeRecord{DaemonVersion: "0.21.0", DeclaredKey: true, HookProof: true}
}

func TestClaudePreToolUse_SessionStartCarriesAProofOfItsStamp(t *testing.T) {
	isolateProofKey(t)
	for name, tc := range map[string]struct{ agent, stamp string }{
		"the main thread": {"", "conv-1"},
		"a subagent":      {"agent-7", "conv-1/agent-7"},
	} {
		args := hookedSessionStart(t, proofDaemon(), tc.agent, "")
		var proof string
		if err := json.Unmarshal(args[mcp.ArgHookProofKey], &proof); err != nil || proof == "" {
			t.Fatalf("%s: no proof in %v", name, args)
		}
		key, err := loadResumeProofKey()
		if err != nil {
			t.Fatalf("%s: the serve cannot read the key the hook made: %v", name, err)
		}
		if !verifyResumeProof(key, tc.stamp, proof) {
			t.Errorf("%s: the proof does not verify against its own stamp %q", name, tc.stamp)
		}
		if string(args[mcp.ArgLogicalAgentDeclaredKey]) != `"`+tc.stamp+`"` || string(args["workspace"]) != `"/w"` {
			t.Errorf("%s: the stamp or the caller's own arguments were disturbed: %v", name, args)
		}
	}
}

func TestClaudePreToolUse_AddsNoProofUnlessTheDaemonListsIt(t *testing.T) {
	path := isolateProofKey(t)
	for name, daemon := range map[string]identityProbeRecord{
		"a daemon that does not list the proof key": {DaemonVersion: "0.21.0", DeclaredKey: true},
		"an old daemon": {DaemonVersion: "0.20.3"},
	} {
		args := hookedSessionStart(t, daemon, "", "")
		if _, has := args[mcp.ArgHookProofKey]; has {
			t.Errorf("%s: a proof went to a daemon that would reject it as an unknown parameter", name)
		}
		if len(args[mcp.ArgLogicalAgentDeclaredKey])+len(args[mcp.ArgLogicalAgentKey]) == 0 {
			t.Errorf("%s: the stamp is gone too: %v", name, args)
		}
	}
	if _, err := os.Lstat(path); err == nil {
		t.Error("the hook made a key for a call it was never going to prove")
	}
}

// Like the stamp, the proof is the hook's: a typed one never survives, and no other tool
// gets one.
func TestClaudePreToolUse_TypedProofIsReplacedAndOtherToolsGetNone(t *testing.T) {
	isolateProofKey(t)
	typed := `,"` + mcp.ArgHookProofKey + `":"typed-by-a-model"`
	for _, daemon := range []identityProbeRecord{proofDaemon(), {DaemonVersion: "0.21.0", DeclaredKey: true}} {
		args := hookedSessionStart(t, daemon, "", typed)
		if strings.Contains(string(args[mcp.ArgHookProofKey]), "typed-by-a-model") {
			t.Errorf("daemon %+v: a typed proof survived the hook", daemon)
		}
	}
	in := claudeHookInput{SessionID: "conv-1", ToolName: "mcp__plumb__read_file", ToolInput: json.RawMessage(`{"file_path":"/w/a.go"}`)}
	out, _ := claudePreToolUseOutput(in, noEnv, func() identityProbeRecord { return proofDaemon() })
	if args, _ := updatedInputOf(t, out); len(args[mcp.ArgHookProofKey]) != 0 {
		t.Errorf("a non-session_start call carries a proof: %v", args)
	}
}

// Fail open: a key that cannot be made costs the proof, never the stamp or the call.
func TestClaudePreToolUse_FailsOpenWhenTheKeyCannotBeMade(t *testing.T) {
	path := isolateProofKey(t)
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(path), []byte("a file where the directory should be"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := hookedSessionStart(t, proofDaemon(), "", "")
	if _, has := args[mcp.ArgHookProofKey]; has {
		t.Error("a proof was made with no key")
	}
	if string(args["session_id"]) != `"conv-1"` || string(args[mcp.ArgLogicalAgentDeclaredKey]) != `"conv-1"` {
		t.Errorf("the stamp was lost with the proof: %v", args)
	}
}

// The path the feature exists for, with the real key between the two halves: what the
// hook writes is what the proxy verifies and presents on, and the proof goes no further.
func TestHookToProxy_TheHooksProofEntitlesAPresentation(t *testing.T) {
	isolateProofKey(t)
	dir := t.TempDir()
	if _, err := rcStore(t, dir, "scope-a").put("conv-1", rcSecret(10)); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{}
	for k, v := range hookedSessionStart(t, proofDaemon(), "", "") {
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			t.Fatal(err)
		}
		args[k] = decoded
	}
	d, p := newScriptedDaemon(rcDaemon(t, "established", rcSecret(1), ""))
	s := startRCServe(t, rcStore(t, dir, "scope-a"), p)
	s.h.proxy.rc.proofKey = loadResumeProofKey // the production reader, not the test key
	s.call("session_start", args, nil)

	frame := d.calls("session_start")[0]
	if got := presentedCredential(t, frame); got != rcSecret(10) {
		t.Fatalf("a call the real hook stamped and proved presented %q, want the stored credential", got)
	}
	if strings.Contains(string(frame), mcp.ArgHookProofKey) {
		t.Errorf("the proof reached the daemon:\n%s", frame)
	}

	// The same call with the proof taken off is a typed claim, and is refused.
	d2, p2 := newScriptedDaemon(rcDaemon(t, "established", rcSecret(1), ""))
	s2 := startRCServe(t, rcStore(t, dir, "scope-a"), p2)
	s2.h.proxy.rc.proofKey = loadResumeProofKey
	delete(args, mcp.ArgHookProofKey)
	s2.call("session_start", args, nil)
	if got := presentedCredential(t, d2.calls("session_start")[0]); got != "" {
		t.Errorf("the same call without its proof presented %q…", got[:9])
	}
}
