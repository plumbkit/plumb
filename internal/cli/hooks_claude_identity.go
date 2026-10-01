package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// event, or reports false when the call must be left untouched. It is
// claudePreToolUseDecision without the reason, for callers that only need to
// know whether a stamp was made.
func claudePreToolUseOutput(input claudeHookInput, env func(string) string, daemon func() identityProbeRecord) (map[string]any, bool) {
	out, _ := claudePreToolUseDecision(input, env, daemon)
	return out, out != nil
}

// claudePreToolUseDecision is the pure core of the PreToolUse hook: the
// environment and the daemon check are injected so every branch is testable
// without a daemon. It returns the stdout document, or nil plus the reason the
// call is left unstamped. An empty reason with a nil document is a call that
// was never the hook's business (a foreign tool), which has nothing to report.
//
// The matcher is not trusted: users edit settings.json, and a hook that
// stamped a non-plumb tool would hand a foreign server an argument it never
// declared. The stamp also rewrites session_start's own session_id to the
// derived identity, so a subagent never needs to remember one and a
// model-typed value ("subagent-7") cannot become a phantom third identity that
// clobbers the connection's linkage.
func claudePreToolUseDecision(input claudeHookInput, env func(string) string, daemon func() identityProbeRecord) (map[string]any, string) {
	if !strings.HasPrefix(input.ToolName, claudeIdentityPrefix) {
		return nil, ""
	}
	if strings.EqualFold(strings.TrimSpace(env(claudeIdentityKillSwitch)), "off") {
		return nil, claudeIdentityKillSwitch + "=off"
	}
	id := claudeIdentity(input.SessionID, input.AgentID)
	if id == "" {
		return nil, "the hook input carries no session_id"
	}
	args, ok := decodeHookToolInput(input.ToolInput)
	if !ok {
		return nil, "tool_input is not a JSON object"
	}
	key, why := identityStampDecision(daemon)
	if key == "" {
		return nil, why
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
	}, ""
}

// runClaudePreToolUse is the PreToolUse command body: one JSON document on
// stdout, or nothing at all, and always a clean exit — only exit 2 blocks a
// call, and an unstamped call is the client's own behaviour, not a failure.
//
// A call left unstamped leaves one line on stderr naming the reason, the tool
// and its tool_use_id. Claude Code shows a successful hook's stderr only in its
// debug output, so this costs a normal session nothing, and it is the one place
// a missing stamp can be told apart afterwards: a hook that ran and found no
// stamp to give says so here, while a call with no line was one Claude Code
// never ran the hook for (a tool it loaded late through ToolSearch, say).
func runClaudePreToolUse(input claudeHookInput, env func(string) string, daemon func() identityProbeRecord, stdout, stderr io.Writer) {
	out, why := claudePreToolUseDecision(input, env, daemon)
	if out != nil {
		_ = json.NewEncoder(stdout).Encode(out)
		return
	}
	if why != "" {
		// %q on the ids and a collapse of the reason keep this one line however
		// odd the input or the error text is.
		fmt.Fprintf(stderr, "plumb identity hook: left the call unstamped: tool_name=%q tool_use_id=%q reason=%s\n",
			input.ToolName, input.ToolUseID, strings.Join(strings.Fields(why), " "))
	}
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
// control socket — its version, and the identity keys it lifts — cached per
// daemon instance: one probe per ten minutes per machine, not one per tool
// call, and one dial of it against a current daemon.

const (
	// identityChannelMinVersion is the first plumb whose daemon lifts the
	// argument-carried identity out of tools/call arguments.
	identityChannelMinVersion = "0.19.1"

	// identityProbeTTL is how long a cached answer is served without asking the
	// daemon again. The cache is already keyed on the daemon instance (see
	// daemonInstanceMarker), so a daemon that is restarted or swapped is a miss
	// at once whatever the age; the TTL only bounds how long a record outlives a
	// change the marker somehow could not see. It was a minute, which meant the
	// first call after any pause in a session paid a cold probe, and a cold probe
	// under load is exactly where the hook used to fail open. Ten minutes keeps
	// the cache warm across an agent's ordinary think-time and review pauses
	// while a missed change still heals inside one sitting.
	identityProbeTTL = 10 * time.Minute

	// The probe's time limits. The hook's own timeout is 5 s (claudeHookEntries),
	// and a hook killed by it leaves the call unstamped and gets no chance to
	// fall back to the cache, so the probe has to finish well inside it:
	//
	//   - identityProbeDialTimeout and identityProbeReplyTimeout are the two
	//     phases of one ask, a second each. They were 300 ms each, which a
	//     machine at full CPU (the daemon's accept loop and this process both
	//     starved of a time slice) misses often: 11 of 200 parallel hooks got no
	//     stamp in the measurement behind issue #556. A second is long enough
	//     for a starved but alive daemon and short enough that one wedged ask
	//     cannot hold a tool call.
	//   - identityHookBudget is the hook's whole allowance, counted from the
	//     process's start rather than from the probe's: under load start-up
	//     alone has taken 1.77 s, and a budget that began only when the probe
	//     did could carry start-up plus probe past the five seconds. Four
	//     seconds leaves a second for the cache write and the output.
	//   - identityProbeBudget is the same cap for a caller that is not a
	//     short-lived hook (the status check), counted from its own start.
	identityProbeDialTimeout  = time.Second
	identityProbeReplyTimeout = time.Second
	identityHookBudget        = 4 * time.Second
	identityProbeBudget       = 3 * time.Second

	// identityProbeCacheFile is new with the declared-key probe: a hook binary
	// from before it writes records with no declared_key to the old name, and
	// sharing that file would have the new hook read them as "no" for the TTL.
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
	// asks again instead of stripping stamps for the whole TTL.
	uncertain bool
	// probeFailure is why daemonIdentity could produce no answer at all — the
	// probe's error, when no cached record could stand in — kept so the hook's
	// breadcrumb can say what went wrong. Never written to the cache.
	probeFailure string
}

// claudeIdentityDaemon is the production gate: what the running daemon
// accepts, from the cached control-socket probe (zero when it cannot be had).
func claudeIdentityDaemon() identityProbeRecord {
	deadline := hookProcessStart.Add(identityHookBudget)
	probe := func(haveRecord bool) (identityProbeRecord, error) {
		return probeDaemonIdentityBy(deadline, haveRecord)
	}
	return daemonIdentity(probe, daemonInstanceMarker, filepath.Join(wakeDir(), identityProbeCacheFile), time.Now())
}

// hookProcessStart is when this process began, as far as Go code can tell:
// package initialisation runs before main, so it is the earliest instant the
// hook can observe. The kernel's exec before it is not counted, which the
// second of slack in identityHookBudget absorbs.
var hookProcessStart = time.Now()

// identityStampKey picks the argument key the stamp goes under for the running
// daemon, or "" when the call must not be stamped. The declarable key is
// preferred because it is the one a host that forwards only declared arguments
// lets through (see mcp.ArgLogicalAgentDeclaredKey), but only a daemon that
// says it lifts that key gets it: an older one rejects it as an unknown
// parameter.
func identityStampKey(daemon func() identityProbeRecord) string {
	key, _ := identityStampDecision(daemon)
	return key
}

// identityStampDecision is identityStampKey plus, when the key is "", the
// reason: the hook's breadcrumb reports it.
func identityStampDecision(daemon func() identityProbeRecord) (key, why string) {
	if daemon == nil {
		return "", "no daemon check is wired in"
	}
	rec := daemon()
	if !daemonVersionAcceptsStamp(rec.DaemonVersion) {
		if strings.TrimSpace(rec.DaemonVersion) == "" {
			why = "the daemon's version is unknown"
			if rec.probeFailure != "" {
				why += ": " + rec.probeFailure + ", and no cached answer belongs to this daemon instance"
			}
			return "", why
		}
		return "", fmt.Sprintf("the daemon is %s, which predates the identity channel (needs %s)", rec.DaemonVersion, identityChannelMinVersion)
	}
	if rec.DeclaredKey {
		return mcp.ArgLogicalAgentDeclaredKey, ""
	}
	return mcp.ArgLogicalAgentKey, ""
}

// daemonIdentity returns what the daemon reports — its version and whether it
// lifts the declared key — from the cache when it is fresh AND was written for
// the same daemon instance, otherwise from probe, refreshing the cache.
//
// A probe that fails does not discard what is already known. The cache is
// keyed on the instance, so a record written for THIS instance describes this
// very process, whose version and identity keys cannot have changed since; it
// is served however old it is, the way a stale DNS answer is when the resolver
// is down. The probe fails exactly when the daemon is busiest — a saturated
// machine starves its accept loop, and on macOS a full listen queue answers
// ECONNREFUSED, which reads as "no daemon" — and answering "unknown" there
// would leave a call unstamped on a connection that refuses unstamped writes.
// A record for a DIFFERENT instance is never served: that is a swapped daemon,
// possibly an older build that rejects the stamp, and the one thing the cache
// must not do is vouch for a process it did not ask. An answer the probe could
// only half give (uncertain) yields to such a record for the same reason —
// unless it read a version that differs from the record's, which is fresh
// evidence that the record no longer describes the daemon.
//
// With no cache to stand in, every failure (no daemon, a daemon too old to
// answer, an unreadable cache) is a record with no version, which no threshold
// accepts: an unstamped call is what the client would have sent anyway.
//
// readInstance yields the daemonInstanceMarker. It is read before the probe:
// the probe then answers for that instance or a later one, so an answer can
// only ever be filed under an instance at or before the one that gave it — a
// mismatch on the next call, never a false hit. It is read again just before
// the cache write, which is skipped if the marker moved: a hook that probed
// slowly across a restart would otherwise file its answer under the old
// instance over the fresh record another hook had written for the new one,
// removing the stale-on-error fallback right after a restart. An empty
// instance (the marker was unreadable) never hits and is never written:
// without it the cache cannot tell one daemon from the next.
//
// probe is told whether a record for this very instance is on file, so a
// wedged daemon is not asked twice for what the record already answers.
func daemonIdentity(probe func(haveRecord bool) (identityProbeRecord, error), readInstance func() string, cachePath string, now time.Time) identityProbeRecord {
	instance := readInstance()
	cached, haveCache := readIdentityProbe(cachePath)
	sameInstance := haveCache && instance != "" && cached.DaemonInstance == instance
	if sameInstance && now.Sub(cached.CheckedAt) < identityProbeTTL && now.After(cached.CheckedAt) {
		return cached
	}
	if probe == nil {
		return identityProbeRecord{probeFailure: "no daemon probe is wired in"}
	}
	rec, err := probe(sameInstance)
	switch {
	case err != nil:
		if sameInstance {
			return cached
		}
		return identityProbeRecord{probeFailure: err.Error()}
	case rec.uncertain:
		if sameInstance && cached.DaemonVersion == rec.DaemonVersion {
			return cached
		}
		return rec
	case instance == "":
		return rec
	}
	rec.CheckedAt = now
	rec.DaemonInstance = instance
	if readInstance() == instance {
		writeIdentityProbe(cachePath, rec)
	}
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
