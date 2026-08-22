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
	"syscall"
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

const (
	claudeWakeWindowDefault   = 300 * time.Second
	claudeWakeIntervalDefault = 7 * time.Second
	claudeWakeChainMaxDefault = 10
	claudeWakeExitCode        = 2
	// claudeStopTimeoutSlack keeps the client's own hook timeout above the
	// watcher's window: a timeout at or below it would kill the watcher before
	// it finished watching, which looks exactly like a wake that never fires.
	claudeStopTimeoutSlack = 30 * time.Second
)

// claudeHookEntries renders Claude Code's two handlers.
//
// Neither carries a matcher. SessionStart's is omitted so it fires on every
// start reason — startup, resume, clear, compact and fork all rebuild the
// context the linkage sentence belongs in — and Stop has no matcher support at
// all.
//
// Stop's timeout is derived from the watch window IN THIS PROCESS, so a tuned
// PLUMB_WAKE_WINDOW writes an entry that outlives its own watcher. The window is
// therefore read at install time for the entry and at run time for the watcher:
// re-tune and re-install together, or the client cancels the watcher mid-watch
// and the wake is lost with nothing to see. `plumb hooks` reports the mismatch
// as `stale`, which is the intended way to notice.
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
			"timeout":     float64((wakeWindow() + claudeStopTimeoutSlack) / time.Second),
			"async":       true,
			"asyncRewake": true,
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
}

func runClaudeHook(_ *cobra.Command, _ []string) error {
	var input claudeHookInput
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&input); err != nil {
		return nil // Hook failures must never strand a turn.
	}
	switch input.Event {
	case "SessionStart":
		// Plain stdout reaches the agent for this event, so the linkage
		// sentence needs no JSON envelope.
		if id := strings.TrimSpace(input.SessionID); id != "" {
			fmt.Println(sessionLinkageSentence(id, "conversation"))
		}
		return nil
	case "Stop":
		wake := claudeStopHook(input, hookMailReport)
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
func claudeStopHook(input claudeHookInput, probe func(string, string) (mailReport, bool)) *mailReport {
	// PLAN-338: settings.json is user-scoped, so this hook runs for EVERY
	// Claude Code session on the machine. A session outside a plumb workspace
	// has no plumb mailbox to wake for — stand down before writing a stamp or
	// arming a watcher, so a global install only ever changes plumb sessions.
	// The .plumb marker walk is cheap and needs no daemon.
	if !insidePlumbWorkspace(input.CWD) {
		return nil
	}
	if probe == nil {
		return nil
	}

	report, ok := probe(input.SessionID, input.CWD)
	key := wakeStampKey(report, input.SessionID)
	if key == "" {
		return nil
	}
	dir := wakeDir()
	writeWakeStamp(dir, key, report, input)

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

	wake := watchForPeerMail(input, report, ok, probe)
	if wake == nil {
		return nil
	}
	recordWake(rearm, *wake)
	// Released explicitly: the caller exits the process, and a leaked lock
	// directory would leave the next turn of this session unwatchable.
	lock.release()
	return wake
}

// watchForPeerMail polls until mail appears, the window closes, or the session
// it is watching for stops existing. It returns the report to wake for, or nil.
func watchForPeerMail(
	input claudeHookInput,
	report mailReport,
	ok bool,
	probe func(string, string) (mailReport, bool),
) *mailReport {
	interval := wakeInterval()
	deadline := time.Now().Add(wakeWindow())
	resolved := ok // have we ever seen this session live?
	gone := 0

	for {
		if ok && report.Count > 0 {
			found := report
			return &found
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		time.Sleep(interval)
		report, ok = probe(input.SessionID, input.CWD)

		// Stand down when the session we are watching for stops existing. An
		// async hook is reparented to init and keeps its own process group, so
		// nothing kills this watcher when its client exits — it would otherwise
		// hold the lock, and poll for a mailbox nobody can read, for the rest
		// of the window. Only applies once the session HAS resolved: a session
		// that never linked never resolves, and must keep watching rather than
		// exit on its first poll. Two consecutive misses, so a transient daemon
		// blip does not retire a live watcher.
		switch {
		case !resolved && ok:
			resolved = true
		case resolved && !ok:
			gone++
			if gone >= 2 {
				return nil
			}
		case resolved && ok:
			gone = 0
		}
	}
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

// insidePlumbWorkspace reports whether dir, or any ancestor, holds a .plumb
// marker directory.
func insidePlumbWorkspace(dir string) bool {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, ".plumb")); err == nil && info.IsDir() {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
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

func wakeWindow() time.Duration {
	return envSeconds("PLUMB_WAKE_WINDOW", claudeWakeWindowDefault)
}

// wakeInterval is clamped to the window: a poll gap longer than the watch it
// paces would park the watcher — holding this session's lock, so the session
// cannot arm another — well past its own deadline, since the loop re-checks the
// deadline only after sleeping.
func wakeInterval() time.Duration {
	interval := envSeconds("PLUMB_WAKE_INTERVAL", claudeWakeIntervalDefault)
	if window := wakeWindow(); interval > window {
		return window
	}
	return interval
}

// envSeconds reads a whole-second duration override, ignoring anything that is
// not a positive integer — a malformed tuning value must not disable the hook.
func envSeconds(name string, fallback time.Duration) time.Duration {
	if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return fallback
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
	for _, line := range strings.Split(string(data), "\n") {
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
	_ = os.WriteFile(rearm, []byte(fmt.Sprintf("pending=%d\nchain=%d\n", report.Count, chain)), 0o600)
}

// wakeLock is the single-instance guard. Repeated turns must not stack
// watchers: a busy workspace runs many sessions, and one leaked watcher process
// per turn is not acceptable. mkdir is the atomic primitive; the pid inside
// lets a dead lock be reclaimed.
type wakeLock struct {
	dir      string
	conv     string
	released bool
}

// release drops this watcher's lock, once — but only while the lock still
// records the conversation that took it. A lock reclaimed by a later tenant of
// a reused session name (see acquireWakeLock) belongs to that watcher, and
// releasing it out from under them would leave the session with no
// single-instance guard at all. The conversation is the identity that
// distinguishes them; the pid does not, since the evicted watcher may be a
// goroutine of the very process that took over.
func (l *wakeLock) release() {
	if l == nil || l.released {
		return
	}
	l.released = true
	// Delete only when ownership is PROVABLE. An unreadable conv file is the
	// successor's window between mkdir and its own stamp — failing open here
	// would delete the lock it has just taken, which is the bug this guard
	// exists to prevent. A lock left behind is reclaimed by age instead.
	owner, err := os.ReadFile(filepath.Join(l.dir, "conv")) //nolint:gosec // G304: inside plumb's own wake dir
	if err != nil || strings.TrimSpace(string(owner)) != l.conv {
		return
	}
	_ = os.RemoveAll(l.dir)
}

// acquireWakeLock takes this session's watcher slot, or reports that another
// watcher already holds it.
//
// The lock records the conversation that owns it, and that is load-bearing. The
// lock is keyed by the plumb session name once linkage resolves one — and plumb
// session names are explicitly reusable, while an async hook's watcher outlives
// its own client (reparented to init, in its own process group, so killing the
// session's process group does not reach it). A watcher outliving session
// "swift-heron" therefore still holds swift-heron.lock, with a LIVE pid, for the
// rest of its window. Without the conversation check the next session to take
// that name would find a live pid, stand down, and never arm a watcher —
// silently unwakeable, with nothing in any output saying so. A lock held by a
// different conversation is stale by definition: if the name resolved to us, we
// ARE that session now.
func acquireWakeLock(dir, key, sessionID string) (*wakeLock, bool) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false
	}
	lock := filepath.Join(dir, key+".lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		if !reclaimableLock(lock, sessionID) {
			return nil, false
		}
		_ = os.RemoveAll(lock)
		if err := os.Mkdir(lock, 0o755); err != nil {
			return nil, false
		}
	}
	_ = os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	_ = os.WriteFile(filepath.Join(lock, "conv"), []byte(orDash(sessionID)), 0o600)
	return &wakeLock{dir: lock, conv: orDash(sessionID)}, true
}

// reclaimableLock decides whether an existing lock may be taken over.
func reclaimableLock(lock, sessionID string) bool {
	pidRaw, _ := os.ReadFile(filepath.Join(lock, "pid"))   //nolint:gosec // G304: inside plumb's own wake dir
	convRaw, _ := os.ReadFile(filepath.Join(lock, "conv")) //nolint:gosec // G304: inside plumb's own wake dir

	pid, err := strconv.Atoi(strings.TrimSpace(string(pidRaw)))
	if err != nil {
		// No readable pid. Most likely a watcher that took the lock moments ago
		// and has not finished stamping it, so stand down — stealing it would
		// let two watchers run for one session. But a watcher that DIED in that
		// window leaves a lock nothing can ever claim, and a session that can
		// never arm a watcher is silently unwakeable with nothing in any output
		// saying so. No live watcher outlives its own window, so an unstamped
		// lock older than one is debris, not a tenant.
		return lockOutlivedAnyWatcher(lock)
	}
	if !processAlive(pid) {
		return true
	}
	owner := strings.TrimSpace(string(convRaw))
	if sessionID == "" || owner == "" || owner == sessionID {
		return false // our own watcher is already running
	}
	terminate(pid) // a previous tenant of this reused session name
	return true
}

// lockOutlivedAnyWatcher reports whether a lock is older than the longest a live
// watcher could still be holding it.
func lockOutlivedAnyWatcher(lock string) bool {
	info, err := os.Stat(lock)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > wakeWindow()+claudeStopTimeoutSlack
}

// terminate asks a stale watcher to stop. Failure is ignored: the lock is
// reclaimed either way, and a watcher that outlives its signal only polls a
// mailbox it can no longer wake anyone for.
//
// Our own pid is never signalled. One process is one hook run and therefore one
// conversation, so a lock recording this pid under a different conversation is
// not a stale tenant to evict — and signalling it would kill the very watcher
// about to be armed.
//
// Nor is a pid that is no longer a plumb process. A lock survives a crash, a
// SIGKILL and a reboot, after which the recorded pid is very likely to belong to
// something unrelated — "the pid exists" is not evidence it is ours. Signalling
// a stranger's process is a far worse failure than leaving a dead lock behind,
// which the caller reclaims anyway.
func terminate(pid int) {
	if pid <= 0 || pid == os.Getpid() || !isPlumbProcess(pid) {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGTERM)
	}
}
