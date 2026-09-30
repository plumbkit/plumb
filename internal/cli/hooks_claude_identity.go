package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/plumbkit/plumb/internal/fsync"
	"github.com/plumbkit/plumb/internal/mcp"
)

// The PreToolUse identity hook — Claude Code's half of the per-agent identity
// contract (PLAN-400 / design doc §4.2).
//
// On a shared `plumb serve` connection the daemon can only tell agents apart by
// an identity each call carries, and Claude Code's transport carries none: its
// per-call `_meta` holds a tool-use id and a progress token, nothing agent
// scoped. What Claude Code DOES offer is a settings-level PreToolUse hook that
// fires inside subagents too, whose stdin names the conversation
// (`session_id`) and, in a subagent, the agent (`agent_id`), and whose
// `updatedInput` replaces the tool's arguments. So the identity rides inside
// `arguments` (under mcp.ArgLogicalAgentDeclaredKey when the daemon lists it in
// its identity-keys answer, else mcp.ArgLogicalAgentKey; identityStampKey
// picks), and the daemon lifts it out again
// before any tool or schema sees it (internal/mcp/argidentity.go).
//
// Identity shape: the main thread is `<session_id>` — the SAME value the
// SessionStart linkage sentence tells the agent to pass as
// session_start.session_id, so the two channels cannot fork one agent into two
// shards — and a subagent is `<session_id>/<agent_id>`. `/` never appears in
// a Claude Code id and is refused by every session name, so the split is
// unambiguous (see linkageIDOf).
//
// Failure policy, as for every hook in this package: fail OPEN. No output and
// exit 0 leaves the call exactly as the client would have sent it. The one
// failure that must be prevented is stamping a daemon that predates the
// channel — it would reject every stamped call as an unknown parameter — so
// the stamp is gated on what the running daemon reports (claudeIdentityDaemon).

const (
	// claudeIdentityMatcher is the PreToolUse matcher the installer writes. It
	// covers plumb's own server name only; a plugin-scoped registration
	// (`mcp__plugin_<p>_plumb__*`) is not matched, by design — the hook does
	// not know which server the user meant that to be.
	claudeIdentityMatcher = "mcp__plumb__.*"
	claudeIdentityPrefix  = "mcp__plumb__"
	// claudeIdentityKillSwitch disables the stamp for users who run their own
	// linkage scheme (the dsh plugin precedent).
	claudeIdentityKillSwitch = "PLUMB_IDENTITY_HOOK"
)

// claudeIdentity derives the logical-agent id from the hook input.
func claudeIdentity(sessionID, agentID string) string {
	sessionID = strings.TrimSpace(sessionID)
	agentID = strings.TrimSpace(agentID)
	if sessionID == "" {
		return ""
	}
	if agentID == "" {
		return sessionID
	}
	return sessionID + "/" + agentID
}

// claudePreToolUseOutput builds the hook's stdout document for one PreToolUse
// event, or reports false when the call must be left untouched. It is pure:
// the environment and the daemon check are injected so every branch is
// testable without a daemon.
//
// The matcher is not trusted: users edit settings.json, and a hook that
// stamped a non-plumb tool would hand a foreign server an argument it never
// declared. The stamp also rewrites session_start's own session_id to the
// derived identity, so a subagent never needs to remember one and a
// model-typed value ("subagent-7") cannot become a phantom third identity that
// clobbers the connection's linkage.
func claudePreToolUseOutput(input claudeHookInput, env func(string) string, daemon func() identityProbeRecord) (map[string]any, bool) {
	if strings.EqualFold(strings.TrimSpace(env(claudeIdentityKillSwitch)), "off") {
		return nil, false
	}
	if !strings.HasPrefix(input.ToolName, claudeIdentityPrefix) {
		return nil, false
	}
	id := claudeIdentity(input.SessionID, input.AgentID)
	if id == "" {
		return nil, false
	}
	args, ok := decodeHookToolInput(input.ToolInput)
	if !ok {
		return nil, false
	}
	key := identityStampKey(daemon)
	if key == "" {
		return nil, false
	}
	// The hook owns the identity: drop whatever the model typed under EITHER
	// key before stamping one. The daemon prefers the reverse-DNS key, so a
	// typed value left beside a plumb_agent stamp would outrank it and become a
	// phantom identity; an older daemon would also reject a typed plumb_agent.
	delete(args, mcp.ArgLogicalAgentKey)
	delete(args, mcp.ArgLogicalAgentDeclaredKey)
	args[key] = json.RawMessage(mustJSONString(id))
	if input.ToolName == claudeIdentityPrefix+"session_start" {
		args["session_id"] = json.RawMessage(mustJSONString(id))
	}
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse",
			"updatedInput":  orderedRawObject(args),
		},
	}, true
}

// decodeHookToolInput parses tool_input into raw values so every original key
// is echoed byte-for-byte — updatedInput REPLACES the whole input, so a key
// dropped here is an argument the tool never receives. Absent or null input is
// the bare `session_start()` case and decodes to an empty object; anything
// that is not an object cannot carry a stamp and is left alone.
func decodeHookToolInput(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return map[string]json.RawMessage{}, true
	}
	if trimmed[0] != '{' {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// orderedRawObject is a json.Marshaler that emits raw values in sorted key
// order without decoding them.
type orderedRawObject map[string]json.RawMessage

func (o orderedRawObject) MarshalJSON() ([]byte, error) {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(bytes.TrimSpace(o[k]))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func mustJSONString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// The version-skew guard.
//
// The hook runs whatever `plumb` binary settings.json names, while the daemon
// is a process that was started earlier — on a developer machine `make build`
// replaces the binary and the daemon keeps running the old one. A daemon that
// predates the argument channel rejects every stamped call as an unknown
// parameter, which would turn an upgrade into a total plumb outage until the
// restart. So the stamp is gated on what the running daemon reports over the
// control socket — its version, and the identity keys it lifts — cached for a
// minute: one probe (two dials) per minute per machine, not one per tool call.

const (
	// identityChannelMinVersion is the first plumb whose daemon lifts the
	// argument-carried identity out of tools/call arguments.
	identityChannelMinVersion = "0.19.1"
	identityProbeTTL          = time.Minute
	identityProbeTimeout      = 300 * time.Millisecond
	// identityProbeCacheFile is new with the declared-key probe: a hook binary
	// from before it writes records with no declared_key to the old name, and
	// sharing that file would have the new hook read them as "no" for a minute.
	identityProbeCacheFile = "daemon-identity-keys.json"
)

// identityProbeRecord is the cached answer.
type identityProbeRecord struct {
	DaemonVersion string `json:"daemon_version"`
	// DeclaredKey reports that the daemon listed mcp.ArgLogicalAgentDeclaredKey
	// in its `identity-keys` answer. It is asked, not inferred from the
	// version: a development build of main carries the last release's version
	// label, so a version threshold kept the declared key off exactly the
	// builds that had it.
	DeclaredKey bool `json:"declared_key,omitempty"`
	// DaemonInstance is the daemonInstanceMarker read BEFORE the probe that
	// produced this record. A cached record answers only while the marker
	// still matches, so a daemon swapped inside the minute — for an older
	// build that rejects plumb_agent, say — is a cache miss, not a minute of
	// every call refused as an unknown parameter.
	DaemonInstance string    `json:"daemon_instance,omitempty"`
	CheckedAt      time.Time `json:"checked_at"`
	// uncertain marks an answer whose identity-keys probe failed on I/O (not
	// a real "no"): it is used for this call but not cached, so the next call
	// asks again instead of stripping stamps for a minute.
	uncertain bool
}

// claudeIdentityDaemon is the production gate: what the running daemon
// accepts, from the cached control-socket probe (zero when it cannot be had).
func claudeIdentityDaemon() identityProbeRecord {
	return daemonIdentity(probeDaemonIdentity, daemonInstanceMarker(), filepath.Join(wakeDir(), identityProbeCacheFile), time.Now())
}

// identityStampKey picks the argument key the stamp goes under for the running
// daemon, or "" when the call must not be stamped. The declarable key is
// preferred because it is the one a host that forwards only declared arguments
// lets through (see mcp.ArgLogicalAgentDeclaredKey), but only a daemon that
// says it lifts that key gets it: an older one rejects it as an unknown
// parameter.
func identityStampKey(daemon func() identityProbeRecord) string {
	if daemon == nil {
		return ""
	}
	rec := daemon()
	if !daemonVersionAcceptsStamp(rec.DaemonVersion) {
		return ""
	}
	if rec.DeclaredKey {
		return mcp.ArgLogicalAgentDeclaredKey
	}
	return mcp.ArgLogicalAgentKey
}

// daemonIdentity returns what the daemon reports — its version and whether it
// lifts the declared key — from the cache when it is fresh AND was written for
// the same daemon instance, otherwise from probe, refreshing the cache. Every
// failure (no daemon, a daemon too old to answer, an unreadable cache) is the
// zero record, which no threshold accepts: an unstamped call is what the
// client would have sent anyway.
//
// instance is the daemonInstanceMarker the caller read before calling, and it
// must be read before the probe: the probe then answers for that instance or
// a later one, so an answer can only ever be filed under an instance at or
// before the one that gave it — a mismatch on the next call, never a false
// hit. An empty instance (the marker was unreadable) never hits and is never
// written: without it the cache cannot tell one daemon from the next.
func daemonIdentity(probe func() (identityProbeRecord, error), instance, cachePath string, now time.Time) identityProbeRecord {
	if rec, ok := readIdentityProbe(cachePath); ok && instance != "" && rec.DaemonInstance == instance &&
		now.Sub(rec.CheckedAt) < identityProbeTTL && now.After(rec.CheckedAt) {
		return rec
	}
	if probe == nil {
		return identityProbeRecord{}
	}
	rec, err := probe()
	if err != nil {
		return identityProbeRecord{}
	}
	if rec.uncertain || instance == "" {
		return rec
	}
	rec.CheckedAt = now
	rec.DaemonInstance = instance
	writeIdentityProbe(cachePath, rec)
	return rec
}

// daemonInstanceMarker names the running daemon INSTANCE, so the probe cache
// can tell a restarted or swapped daemon from the one it asked. It runs on
// every plumb tool call, so it costs one small file read and one stat — no
// dial — and it fails safe: a control socket that cannot be stat'd is "",
// which daemonIdentity treats as a miss.
//
// The marker is the PID file's contents plus the control socket's inode and
// modification time, because each alone can repeat across two daemons. A PID
// is reused — in a container the daemon can get the same PID on every start —
// and a filesystem hands a freed inode to the next file (ext4 does so
// readily), while an mtime is only as fine as the filesystem's clock tick. A
// new daemon rewrites the PID file and re-binds the socket (a fresh inode and
// mtime), so matching all three means the same process. Asking the daemon for
// its start time instead would cost a third dial per probe and, worse, an
// older daemon cannot answer it — and an older daemon is the swap this
// exists to catch. Both files are written by every daemon since the identity
// channel, so the marker needs nothing from the daemon being asked.
//
// The daemon only logs a warning when it cannot write its PID file, so an
// unreadable or empty one falls back to the socket alone rather than to "":
// "" would never hit, and every tool call would pay two dials for as long as
// that daemon runs. The socket half still moves on every re-bind, so the
// fallback stays a miss across a swap, and it cannot match a marker that had
// a PID, which always starts with one.
func daemonInstanceMarker() string {
	fi, err := os.Stat(daemonCtrlSocketPath())
	if err != nil {
		return ""
	}
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = st.Ino
	}
	sock := fmt.Sprintf("ino=%d mtime=%d", ino, fi.ModTime().UnixNano())
	pid, err := os.ReadFile(daemonPIDPath())
	if id := strings.TrimSpace(string(pid)); err == nil && id != "" {
		return "pid=" + id + " " + sock
	}
	return sock
}

// daemonVersionAcceptsStamp: a release older than the threshold does not;
// anything else — a newer release, or an unparseable development build, which
// is the developer's own tree — does.
func daemonVersionAcceptsStamp(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	return !versionOlder(version, identityChannelMinVersion)
}

func readIdentityProbe(path string) (identityProbeRecord, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is plumb's own cache file under wakeDir
	if err != nil {
		return identityProbeRecord{}, false
	}
	var rec identityProbeRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.CheckedAt.IsZero() {
		return identityProbeRecord{}, false
	}
	return rec, true
}

// writeIdentityProbe writes the cache atomically and silently: a cache that
// cannot be written costs one probe per call, never a stamp.
func writeIdentityProbe(path string, rec identityProbeRecord) {
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = fsync.AtomicWrite(path, data, fsync.Options{Label: "hooks"})
}

// probeDaemonVersion asks the running daemon its version over the control
// socket (`version`, added with the channel — an older daemon answers with an
// unknown-command error, which reads as "too old"). Bounded by a dial and a
// read deadline so a wedged daemon cannot hold a tool call for the hook's
// whole timeout.
//
// The failure is CLASSIFIED rather than flattened, because the two kinds have
// opposite remedies and a third has neither. No socket (or one refusing
// connections) is a daemon that is not running: it starts on the next `plumb
// serve` and stamping resumes by itself. An answer that is not a version is a
// daemon that predates the channel: it needs `plumb restart`. Anything else —
// a permission error on the socket, a dial timeout against a wedged listener,
// a short read — observed no version at all, and saying either of the first
// two would be a guess dressed as a fact.
func probeDaemonVersion() (string, error) {
	conn, err := net.DialTimeout("unix", daemonCtrlSocketPath(), identityProbeTimeout)
	if err != nil {
		return "", classifyDialError(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(identityProbeTimeout))
	if _, err := conn.Write([]byte("version\n")); err != nil {
		return "", fmt.Errorf("asking the daemon its version: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading the daemon's version reply: %w", err)
	}
	return parseDaemonVersionReply(line)
}

// probeDaemonIdentity asks the daemon its version and, separately, which
// identity argument keys it lifts. A daemon that predates `identity-keys`
// answers "unknown command", which reads as "not the declared key" — the safe
// answer, since such a daemon would reject it.
func probeDaemonIdentity() (identityProbeRecord, error) {
	version, err := probeDaemonVersion()
	if err != nil {
		return identityProbeRecord{}, err
	}
	declared, err := probeDaemonDeclaredKey()
	return identityProbeRecord{DaemonVersion: version, DeclaredKey: declared, uncertain: err != nil}, nil
}

// probeDaemonDeclaredKey reports whether the daemon's `identity-keys` answer
// lists mcp.ArgLogicalAgentDeclaredKey. A reply without it — including an
// older daemon's "unknown command" — is a real "no". An I/O failure is an
// error: the caller treats it as "no" for this call only (the safe key) and
// does not cache it.
func probeDaemonDeclaredKey() (bool, error) {
	conn, err := net.DialTimeout("unix", daemonCtrlSocketPath(), identityProbeTimeout)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(identityProbeTimeout))
	if _, err := conn.Write([]byte(ctrlIdentityKeysCommand + "\n")); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return false, err
	}
	return identityKeysReplyHasDeclared(line), nil
}

// identityKeysReplyHasDeclared parses `ok <key> <key>...`.
func identityKeysReplyHasDeclared(line string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ok ")
	if !ok {
		return false
	}
	return slices.Contains(strings.Fields(rest), mcp.ArgLogicalAgentDeclaredKey)
}

// classifyDialError separates "there is no daemon" from "there is something
// there and plumb could not talk to it". Only a missing socket file and a
// refused connection mean the daemon is down; a permission error, a dial
// timeout against a wedged listener and anything else observed no such thing,
// and the caller must not be told to wait for a start that already happened.
func classifyDialError(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return errDaemonNotRunning
	}
	return fmt.Errorf("dialling the daemon control socket: %w", err)
}

// parseDaemonVersionReply accepts the `version` command's `ok <version>` line
// and treats anything else — an `error: unknown command` from an older daemon
// included — as no answer.
func parseDaemonVersionReply(line string) (string, error) {
	line = strings.TrimSpace(line)
	if v, ok := strings.CutPrefix(line, "ok "); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), nil
	}
	return "", errDaemonVersionUnknown
}

// The two ways a probe fails are told apart because their remedies differ:
// a daemon that is not running starts on the next `plumb serve` and stamping
// resumes by itself, while one that answers but predates the channel needs
// `plumb restart`.
var (
	errDaemonNotRunning     = errors.New("no daemon is listening on the control socket")
	errDaemonVersionUnknown = errors.New("the daemon did not report a version, so it predates the identity channel")
)
