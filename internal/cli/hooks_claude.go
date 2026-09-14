package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Claude Code's half of `plumb hooks`: the two handlers plumb installs, and the
// runtime behind `plumb hooks run-claude` that they invoke.
//
// The Stop handler is the one place plumb can genuinely push. Contract it
// depends on, verified on Claude Code 2.1.233 (PLAN-320) and dogfooded in a
// multi-agent workspace since (PLAN-321/333/338):
//
//   - With "async": true and "asyncRewake": true the client queues a task
//     notification when the hook exits 2, and that notification reaches a
//     session with no turn in flight. Exit 2's STDERR is the payload; a
//     successful hook's stdout is discarded.
//   - stop_hook_active is true on the woken turn's Stop, so guarding on it is
//     what stops a continuation loop — but an outright stand-down drops the
//     cadence of a real back-and-forth (measured: a second note sat unread for
//     38s with no watcher alive), so a woken turn that CONSUMED mail re-arms
//     one more watcher and an ignored wake cannot chain.
//   - `plumb mail` never claims. The count is all this reports; the bodies stay
//     undelivered and arrive through check_messages, labelled as what they are.
//     Pasting a peer's text into hook feedback would be a direct injection
//     channel into the agent.
//
// Failure policy, everywhere in this file: every failure allows the stop. No
// linkage, no session, an ambiguous workspace, a dead daemon — all fall through
// to a silent exit 0. A wake hook that failed closed would strand turns on an
// unrelated fault.
//
// This is a port of scripts/hooks/plumb-mail-wake.sh, the shell recipe the
// plumb-chat skill documented and this workspace ran by hand. Running inside
// the plumb binary drops the jq dependency and the `plumb mail` subprocess per
// poll; the stamp files it writes keep the shell recipe's format because they
// are a published interface (peer-reachability tooling parses them).

// The watch WINDOWS — how long a watcher polls, and the config behind that —
// live in hooks_claude_window.go.
const (
	claudeWakeChainMaxDefault = 10
	claudeWakeExitCode        = 2
	// claudeWakeStampTTL is how long a stamp or re-arm record may sit unrewritten
	// before the sweep treats it as debris. A stamp is rewritten on EVERY turn
	// end, so a week without one means the session it belonged to is long gone.
	claudeWakeStampTTL = 7 * 24 * time.Hour
	// claudeWakeSweepMax bounds one sweep. Housekeeping runs on a turn-end path
	// and must never be the reason a turn is slow, so a directory that somehow
	// grew enormous is cleaned over several turns instead of one long one.
	claudeWakeSweepMax = 64
)

// claudeHookEntries renders Claude Code's two handlers.
//
// Neither carries a matcher. SessionStart's is omitted so it fires on every
// start reason — startup, resume, clear, compact and fork all rebuild the
// context the linkage sentence belongs in — and Stop has no matcher support at
// all.
//
// Stop's timeout is derived from the watch CEILING in this process — the
// longest a watcher could run, which is the peer-extended window rather than
// the base one. A timeout covering only the base would kill exactly the
// long-idle watch the ceiling exists to allow, and kill it invisibly.
//
// The windows are therefore read at install time for the entry and at run time
// for the watcher: re-tune and re-install together, or the client cancels the
// watcher mid-watch and the wake is lost with nothing to see. `plumb hooks`
// reports the mismatch as `stale`, which is the intended way to notice. The
// same asymmetry is why project config may only NARROW its windows: this entry
// is machine-wide and knows nothing about any one workspace.
func claudeHookEntries(plumbBin string) []hookEntry {
	command := plumbHookCommand(plumbBin, claudeHookVerb)
	return []hookEntry{
		{event: "SessionStart", label: "session linkage", handler: map[string]any{
			"type":    "command",
			"command": command,
			"timeout": float64(5),
		}},
		{event: "Stop", label: "mailbox wake", handler: map[string]any{
			"type":        "command",
			"command":     command,
			"timeout":     float64((globalWakeWindows().ceiling() + claudeStopTimeoutSlack) / time.Second),
			"async":       true,
			"asyncRewake": true,
		}},
		// The identity hook is matched to plumb's own tools: it runs once per
		// plumb call, reads stdin, writes one JSON document, and touches no
		// network beyond a cached daemon-version probe. See hooks_claude_identity.go.
		{event: "PreToolUse", label: "agent identity", matcher: claudeIdentityMatcher, handler: map[string]any{
			"type":    "command",
			"command": command,
			"timeout": float64(5),
		}},
	}
}

var hooksRunClaudeCmd = &cobra.Command{
	Use:         "run-claude",
	Short:       "Run a Claude Code lifecycle hook",
	Hidden:      true,
	Annotations: map[string]string{annoSkipLogo: "true"}, // stdout belongs to the client
	Args:        cobra.NoArgs,
	RunE:        runClaudeHook,
}

type claudeHookInput struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	Event          string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
	// PreToolUse fields. AgentID is present only inside a subagent; ToolInput
	// is the tool's arguments, which the identity hook echoes back with one
	// key added (updatedInput replaces the whole input).
	AgentID   string          `json:"agent_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// claudeHookStdinCap bounds the hook's stdin. A PreToolUse payload embeds the
// tool's whole input — a write_file body can be megabytes — and a cap that
// truncated it would fail the decode, stamp nothing, and get that one write
// refused on a shared connection with no visible reason.
const claudeHookStdinCap = 32 << 20

func runClaudeHook(_ *cobra.Command, _ []string) error {
	var input claudeHookInput
	if err := json.NewDecoder(io.LimitReader(os.Stdin, claudeHookStdinCap)).Decode(&input); err != nil {
		return nil // Hook failures must never strand a turn.
	}
	switch input.Event {
	case "PreToolUse":
		// One JSON document on stdout, or nothing at all. Exit 0 either way:
		// only exit 2 blocks a call, and an unstamped call is the client's own
		// behaviour, not a failure.
		if out, ok := claudePreToolUseOutput(input, os.Getenv, claudeIdentityDaemonAccepts); ok {
			_ = json.NewEncoder(os.Stdout).Encode(out)
		}
		return nil
	case "SessionStart":
		// Plain stdout reaches the agent for this event, so the linkage
		// sentence needs no JSON envelope.
		if id := strings.TrimSpace(input.SessionID); id != "" {
			fmt.Println(sessionLinkageSentence(id, "conversation"))
		}
		return nil
	case "Stop":
		wake := claudeStopHook(input, hookWakeProbe)
		if wake == nil {
			return nil
		}
		// Exit 2 with one line on stderr is the payload asyncRewake turns into
		// a task notification. No other exit code carries a wake, and returning
		// an error would exit 1 — hence the explicit exit.
		fmt.Fprintln(os.Stderr, wakeSentence(*wake))
		os.Exit(claudeWakeExitCode)
	}
	return nil
}

// claudeStopHook runs the whole Stop path and returns the report to wake for,
// or nil to allow the stop. It is separated from the command body so tests can
// drive every branch — including a real wake — without exiting the process.
func claudeStopHook(input claudeHookInput, probe wakeProbe) *mailReport {
	// PLAN-338: settings.json is user-scoped, so this hook runs for EVERY
	// Claude Code session on the machine. A session outside a plumb workspace
	// has no plumb mailbox to wake for — stand down before writing a stamp or
	// arming a watcher, so a global install only ever changes plumb sessions.
	// The .plumb marker walk is cheap and needs no daemon.
	root, inside := plumbWorkspaceRoot(input.CWD)
	if !inside {
		return nil
	}
	if probe == nil {
		return nil
	}

	report, peers, ok := probe(input.SessionID, input.CWD)
	key := wakeStampKey(report, input.SessionID)
	if key == "" {
		return nil
	}
	dir := wakeDir()
	writeWakeStamp(dir, key, report, input)
	windows := runtimeWakeWindows(root)
	// Housekeeping, here because this is the one point every turn end reaches
	// with the directory already open and the wake decision not yet made. It is
	// bounded and every failure is ignored; see sweepWakeDir.
	sweepWakeDir(dir)

	rearm := filepath.Join(dir, key+".rearm")
	if input.StopHookActive {
		// This Stop ends a WOKEN turn. Re-arm only when that turn actually
		// consumed mail; anything else stands down, which is what stops a
		// continuation loop.
		if !wakeChainContinues(rearm, report, ok) {
			_ = os.Remove(rearm)
			return nil
		}
		// Consumed: fall through with the stamp in place, so the chain counter
		// carries forward into the next wake.
	} else {
		// A non-woken turn end resets the chain. A stamp surviving to here
		// means its wake never produced a woken Stop (a dropped notification, a
		// client restart); this run re-arms fresh either way, so the stale
		// stamp must not leak in.
		_ = os.Remove(rearm)
	}

	lock, held := acquireWakeLock(dir, key, input.SessionID)
	if !held {
		return nil
	}
	defer lock.release()

	wake := watchForPeerMail(input, report, peers, ok, probe, windows)
	if wake == nil {
		return nil
	}
	recordWake(rearm, *wake)
	// Released explicitly: the caller exits the process, and a leaked lock
	// directory would leave the next turn of this session unwatchable.
	lock.release()
	return wake
}

// wakeProbe is one poll: this session's mail, how many live peers share its
// workspace, and whether the session resolved at all. The peer count travels
// with the mail report because both come from the same look at the session
// list, and because the watcher has to re-decide the extension on every poll.
type wakeProbe func(sessionID, cwd string) (mailReport, int, bool)

// watchForPeerMail polls until mail appears, the window closes, or the session
// it is watching for stops existing. It returns the report to wake for, or nil.
//
// The deadline is not fixed. It starts one base window out and slides forward by
// another base window on every poll that sees a live peer, capped at the
// ceiling. Two properties follow, and both are the point:
//
//   - A session whose peer goes away exits within one base window of the last
//     sighting, rather than holding a resident process to the ceiling for a peer
//     that is no longer there.
//   - A session that never RESOLVED never extends. Its probe cannot succeed — no
//     linkage, or a daemon that is down — so it has no peer count to trust and
//     no mailbox anyone could wake it for. Before the extension existed this
//     case merely wasted the base window; at the ceiling it would be an
//     hour-long orphan holding this session's lock, since the stand-down below
//     is unreachable while resolved is false.
func watchForPeerMail(
	input claudeHookInput,
	report mailReport,
	peers int,
	ok bool,
	probe wakeProbe,
	windows wakeWindowPair,
) *mailReport {
	interval := wakeInterval(windows.base)
	start := time.Now()
	hardStop := start.Add(windows.ceiling())
	live := watchLiveness{resolved: ok}
	// The probe that armed this watcher already counted the peers, so the first
	// extension is decided before the first sleep.
	deadline := windows.slideDeadline(start.Add(windows.base), hardStop, start, live.resolved, peers)

	for {
		if ok && report.Count > 0 {
			found := report
			return &found
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		time.Sleep(interval)
		report, peers, ok = probe(input.SessionID, input.CWD)
		if live.observe(ok) {
			return nil
		}
		deadline = windows.slideDeadline(deadline, hardStop, time.Now(), live.resolved, peers)
	}
}

// claudeWakeGoneMisses is how many CONSECUTIVE unresolved polls retire a
// watcher. More than one, so a transient daemon blip does not kill a live watch.
const claudeWakeGoneMisses = 2

// watchLiveness tracks whether the session a watcher is watching for still
// exists.
//
// An async hook is reparented to init and keeps its own process group, so
// nothing kills this watcher when its client exits — without this it would hold
// the session's lock, and poll for a mailbox nobody can read, for the rest of
// the window. It only applies once the session HAS resolved: a session that
// never linked never resolves, and must keep watching rather than exit on its
// first poll.
type watchLiveness struct {
	resolved bool // have we ever seen this session live?
	gone     int
}

// observe folds one poll's outcome in and reports whether to stand down.
func (l *watchLiveness) observe(ok bool) (standDown bool) {
	switch {
	case !l.resolved && ok:
		l.resolved = true
	case l.resolved && !ok:
		l.gone++
		return l.gone >= claudeWakeGoneMisses
	case l.resolved && ok:
		l.gone = 0
	}
	return false
}

// wakeSentence is the stderr payload. It reports a count and an age, never a
// body, and names check_messages as the delivery path so the woken agent's
// first move is the one that actually claims the mail.
func wakeSentence(report mailReport) string {
	oldest := "?"
	if len(report.AgesSeconds) > 0 {
		oldest = strconv.Itoa(report.AgesSeconds[0])
	}
	return fmt.Sprintf(
		"plumb mailbox: %d unread message(s) from peer agent(s) are waiting for this session (oldest %ss ago). "+
			"They are still unclaimed — check_messages is the delivery path that reads them.",
		report.Count, oldest)
}

// plumbWorkspaceRoot returns the nearest ancestor of dir (or dir itself) holding
// a .plumb marker directory. It returns the ROOT rather than a bare bool because
// that path is also where this workspace's project config lives, and the walk
// that finds one has already found the other.
func plumbWorkspaceRoot(dir string) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", false
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, ".plumb")); err == nil && info.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// wakeStampKey keys a session's stamp, lock and re-arm files by its plumb
// session name once linkage resolves one, and by the conversation id otherwise.
// A conversation-id-keyed stamp means "hooked, but not linked to a plumb
// session", which is itself the thing a peer needs to know.
// A key that could escape the wake dir is refused rather than sanitised — the
// same call this codebase makes about path traversal elsewhere. The conversation
// id arrives from the client, and the lock path is the sharp end:
// acquireWakeLock removes the directory it derives. No wake is a better failure
// than a delete outside the directory plumb owns.
func wakeStampKey(report mailReport, sessionID string) string {
	key := strings.TrimSpace(report.Session)
	if key == "" {
		key = strings.TrimSpace(sessionID)
	}
	if key == "." || key == ".." || strings.ContainsAny(key, `/\`) || strings.Contains(key, "..") {
		return ""
	}
	return key
}

// wakeDir is where the stamps, locks and re-arm records live. It stays under
// ~/.claude by default because the files are per-client wake state, and because
// tooling that reports which peers are reachable already reads them there.
func wakeDir() string {
	if dir := strings.TrimSpace(os.Getenv("PLUMB_WAKE_DIR")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "plumb-wake")
	}
	return filepath.Join(home, ".claude", "plumb-wake")
}

// isPlumbProcess reports whether pid is a running plumb. It shells out to ps
// for the same reason the daemon's own liveness check does: there is no
// portable way to read another process's name, and the alternative is
// signalling blind.
func isPlumbProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output() //nolint:gosec // G204: pid is a strconv-formatted int read from plumb's own lock file, not user input
	if err != nil {
		return false
	}
	return filepath.Base(strings.TrimSpace(string(out))) == "plumb"
}

func wakeChainMax() int {
	if raw := strings.TrimSpace(os.Getenv("PLUMB_WAKE_CHAIN_MAX")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return claudeWakeChainMaxDefault
}

// writeWakeStamp records that this session was hooked, on every run. The format
// is a published interface — peer-reachability tooling parses these files — so
// the key set is a compatibility contract, not an internal detail. Failures are
// ignored: a stamp is diagnostics, and no diagnostic is worth stranding a turn.
func writeWakeStamp(dir, key string, report mailReport, input claudeHookInput) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	body := fmt.Sprintf("epoch=%d\nplumb_session=%s\nconversation_id=%s\ncwd=%s\nhook=plumb-mail-wake\n",
		time.Now().Unix(),
		orDash(report.Session),
		orDash(input.SessionID),
		orDash(input.CWD))
	_ = os.WriteFile(filepath.Join(dir, key+".stamp"), []byte(body), 0o600)
}

// sweepWakeDir removes debris from the wake directory: stamps and re-arm records
// nothing has rewritten in claudeWakeStampTTL, and lock directories the existing
// reclaim ladder already judges dead.
//
// Nothing else ever cleaned this directory, so it grew without bound — a
// long-running fleet accumulates one stamp per conversation forever, plus a lock
// for every watcher killed before it could release one. Harmless individually;
// the reason to fix it now is that a watcher living to an hour makes leaked
// locks both more likely and longer-lived.
//
// Debris is decided by reclaimableLock with an EMPTY session id, which is
// precisely the question being asked here: not "may this session take the lock"
// but "could any live watcher still hold it". An empty id also short-circuits
// before the evict-a-previous-tenant branch, so a sweep never signals a process
// — housekeeping must not be able to kill a watcher.
//
// Every failure is ignored and the work is capped: this runs on a turn-end path,
// where no amount of tidiness is worth delaying a turn.
func sweepWakeDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	swept := 0
	for _, entry := range entries {
		if swept >= claudeWakeSweepMax {
			return
		}
		name := entry.Name()
		path := filepath.Join(dir, name)
		switch {
		case entry.IsDir() && strings.HasSuffix(name, ".lock"):
			if reclaimableLock(path, "", isPlumbProcess) {
				_ = os.RemoveAll(path)
				swept++
			}
		case strings.HasSuffix(name, ".stamp"), strings.HasSuffix(name, ".rearm"):
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) > claudeWakeStampTTL {
				_ = os.Remove(path)
				swept++
			}
		}
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// wakeChainContinues decides whether a woken turn earned another watcher.
//
// The watcher that fired left a .rearm record holding the pending count it woke
// for; a lower count now is proof the turn read some of it, since the probe
// never claims. Every ambiguous reading — no record, a failed probe, no drop, an
// unreadable counter — reads as "not consumed" and stands the chain down, and
// the chain cap bounds the rest. A drop is evidence, not proof: a note expiring
// mid-turn, or a peer winning the claim race on a "next" note, drops the count
// too and buys one duplicate wake before the chain ends.
func wakeChainContinues(rearm string, report mailReport, ok bool) bool {
	pending, chain, found := readRearm(rearm)
	if !found || !ok {
		return false
	}
	return report.Count < pending && chain < wakeChainMax()
}

// readRearm parses a re-arm record. An unreadable or malformed chain counter
// reads as spent (the cap), never as zero — the fail-safe direction.
func readRearm(path string) (pending, chain int, found bool) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is inside plumb's own wake dir
	if err != nil {
		return 0, 0, false
	}
	chain = wakeChainMax()
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, hasSep := strings.Cut(strings.TrimSpace(line), "=")
		if !hasSep {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			continue
		}
		switch key {
		case "pending":
			pending = n
		case "chain":
			chain = n
		}
	}
	return pending, chain, true
}

// recordWake stamps the wake before it is delivered: the woken turn's Stop
// re-arms only when the pending count dropped from what was seen here. A record
// left in place by the re-arm decision carries the chain count forward; a fresh
// wake starts the chain at 1.
func recordWake(rearm string, report mailReport) {
	chain := 1
	if _, prev, found := readRearm(rearm); found && prev < wakeChainMax() {
		chain = prev + 1
	}
	_ = os.WriteFile(rearm, fmt.Appendf(nil, "pending=%d\nchain=%d\n", report.Count, chain), 0o600)
}
