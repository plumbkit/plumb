package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tui"
)

var stopFlagForce bool

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the background daemon",
	RunE:  runStop,
}

func init() {
	stopCmd.Flags().BoolVar(&stopFlagForce, "force", false, "stop without asking for confirmation")
}

// daemonActionPrompt parameterises the active-sessions confirmation so `stop`
// and `restart` share the same gate with action-appropriate wording.
type daemonActionPrompt struct {
	verb        string // lower-case action name, e.g. "stop" / "restart"
	consequence string // the muted explanation line shown above the Yes/No prompt
}

var stopActionPrompt = daemonActionPrompt{
	verb:        "stop",
	consequence: "Stopping the daemon will terminate all active sessions.",
}

var restartActionPrompt = daemonActionPrompt{
	verb:        "restart",
	consequence: "Restarting the daemon briefly drops active sessions; resilient clients reconnect automatically.",
}

func runStop(_ *cobra.Command, _ []string) error {
	PrintLogo()
	pids := findAllDaemonPIDs()
	if len(pids) == 0 {
		fmt.Println("Daemon is not running.")
		return nil
	}
	prompted := false
	if !stopFlagForce {
		ok, shown, err := confirmDaemonActionWithActiveSessions(stopActionPrompt)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("\nStop cancelled.")
			return nil
		}
		prompted = shown
	}
	if len(pids) > 1 {
		fmt.Printf("Found %d daemon process(es) — stopping all.\n", len(pids))
	}
	var lastErr error
	for i, pid := range pids {
		if err := stopByPID(pid, prompted && i == 0); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func confirmDaemonActionWithActiveSessions(p daemonActionPrompt) (bool, bool, error) {
	active, err := session.List()
	if err != nil {
		return false, false, fmt.Errorf("listing active sessions: %w", err)
	}
	if len(active) == 0 {
		return true, false, nil
	}

	if err := renderSessions(active, 0); err != nil {
		return false, false, err
	}
	fmt.Println()
	ok, err := runActionConfirmationSelector(len(active), p)
	return ok, true, err
}

func runActionConfirmationSelector(sessionCount int, p daemonActionPrompt) (bool, error) {
	prog := tea.NewProgram(newStopConfirmationModel(sessionCount, p))
	finalModel, err := prog.Run()
	if err != nil {
		return false, fmt.Errorf("confirming %s: %w", p.verb, err)
	}
	m, ok := finalModel.(stopConfirmationModel)
	if !ok {
		return false, nil
	}
	return m.confirmed, nil
}

type stopConfirmationModel struct {
	sessionCount int
	consequence  string
	cursor       int // 0 yes, 1 no
	confirmed    bool
}

func newStopConfirmationModel(sessionCount int, p daemonActionPrompt) stopConfirmationModel {
	return stopConfirmationModel{
		sessionCount: sessionCount,
		consequence:  p.consequence,
		cursor:       1, // default to No
	}
}

func (m stopConfirmationModel) Init() tea.Cmd { return nil }

func (m stopConfirmationModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k", "left", "h", "down", "j", "right", "l", "tab", "shift+tab":
			if m.cursor == 0 {
				m.cursor = 1
			} else {
				m.cursor = 0
			}
		case "y", "Y":
			m.cursor = 0
			m.confirmed = true
			return m, tea.Quit
		case "n", "N", "q", "esc", "ctrl+c":
			m.cursor = 1
			m.confirmed = false
			return m, tea.Quit
		case "enter", " ":
			m.confirmed = m.cursor == 0
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m stopConfirmationModel) View() tea.View {
	return tea.NewView(renderStopConfirmationPrompt(m.consequence, m.sessionCount, m.cursor))
}

func renderStopConfirmationPrompt(consequence string, sessionCount, cursor int) string {
	tui.RebuildStyles()
	infoBadge := lipgloss.NewStyle().
		Foreground(tui.ActiveTheme.SelectionBackground).
		Background(tui.ActiveTheme.Accent).
		Bold(true).
		Render(" i ")
	sessionLabel := pluralSessionCount(sessionCount)
	muted := tui.MutedStyle
	selected := tui.SelectedStyle
	options := []string{"Yes", "No"}
	optionLines := make([]string, 0, len(options))
	for i, option := range options {
		marker := "  "
		style := tui.HintStyle
		if i == cursor {
			marker = "❯ "
			style = selected
		}
		optionLines = append(optionLines, tui.SepStyle.Render("    ┊ ")+style.Render(marker+option))
	}
	return fmt.Sprintf(
		"%s %s\n%s\n%s\n%s\n%s\n",
		infoBadge,
		tui.ItemStyle.Render(fmt.Sprintf("You have %s.", sessionLabel)),
		muted.Render("    "+consequence),
		tui.ItemStyle.Render("    Confirm?"),
		optionLines[0],
		optionLines[1],
	)
}

func pluralSessionCount(n int) string {
	if n == 1 {
		return "1 active session"
	}
	return fmt.Sprintf("%d active sessions", n)
}

// findAllDaemonPIDs locates every running daemon PID using three strategies,
// deduplicating across all sources so each process is stopped exactly once:
//  1. PID file written by the current binary.
//  2. lsof on the daemon socket (covers binary-path changes).
//  3. pgrep on the command-line pattern (covers socket-path changes, older
//     binaries, and any other fallback case).
func findAllDaemonPIDs() []int {
	seen := make(map[int]bool)
	var pids []int

	add := func(pid int) {
		if pid > 0 && !seen[pid] && processAlive(pid) {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}

	// 1. PID file — read once, reuse for the stale-file cleanup below.
	filePID := readDaemonPID()
	if filePID > 0 {
		add(filePID)
	}
	// Clean up stale PID file if the recorded process is gone.
	if filePID > 0 && !seen[filePID] {
		_ = os.Remove(daemonPIDPath())
	}

	// 2. lsof on the socket.
	add(findPIDViaSocket(daemonSocketPath()))

	// 3. pgrep fallback — returns all matches.
	for _, pid := range findAllDaemonByArgs() {
		add(pid)
	}

	return pids
}

// findAllDaemonByArgs uses pgrep to find ALL "plumb daemon" processes
// regardless of which socket or PID file path they were started with.
func findAllDaemonByArgs() []int {
	out, err := exec.Command("pgrep", "-f", "plumb daemon").Output()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && pid > 0 && pid != self && isPlumbDaemonProcess(pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// isPlumbDaemonProcess verifies a pgrep candidate really is a `plumb daemon`
// process — its executable basename is "plumb" and its first argument is
// "daemon" — so `plumb stop` never signals an unrelated process whose argv
// merely contains the substring "plumb daemon" (e.g. `vim plumb daemon.go`).
func isPlumbDaemonProcess(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output() //nolint:gosec // G204: pid is a strconv-formatted int from pgrep, not user input
	if err != nil {
		return false
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return false
	}
	return filepath.Base(fields[0]) == "plumb" && fields[1] == "daemon"
}

// processAlive returns true if a process with the given PID exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// readDaemonPID reads the PID file written by the current daemon. Returns 0
// if the file does not exist or cannot be parsed.
func readDaemonPID() int {
	data, err := os.ReadFile(daemonPIDPath())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// findPIDViaSocket uses lsof to find the PID of the process that owns
// socketPath. Works on macOS and Linux without root privileges.
func findPIDViaSocket(socketPath string) int {
	out, err := exec.Command("lsof", "-t", socketPath).Output()
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// daemonStopWait must stay longer than shutdownHardDeadline so the daemon's
// own watchdog wins the race before the CLI warns about a slow graceful stop.
const daemonStopWait = shutdownHardDeadline + time.Second

// stopByPID sends SIGTERM to pid and waits up to daemonStopWait for it to exit.
func stopByPID(pid int, leadingBlank bool) error {
	proc, err := os.FindProcess(pid)
	if err != nil || proc.Signal(syscall.Signal(0)) != nil {
		fmt.Println("Daemon is not running (stale reference cleaned up).")
		_ = os.Remove(daemonPIDPath())
		return nil
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sending SIGTERM to daemon (PID %d): %w", pid, err)
	}

	if leadingBlank {
		fmt.Println()
	}
	fmt.Printf("Stopping daemon (PID %d) ...", pid)
	deadline := time.Now().Add(daemonStopWait)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if proc.Signal(syscall.Signal(0)) != nil {
			fmt.Println(" stopped.")
			return nil
		}
		fmt.Print(".")
	}
	fmt.Println()
	fmt.Printf("Warning: daemon (PID %d) did not stop within %s; it may still be running.\n", pid, daemonStopWait)
	return nil
}

// forceKillIfAlive escalates to SIGKILL when a daemon ignored the SIGTERM that
// stopByPID already sent. `plumb restart`'s contract is a *fresh* daemon, so one
// that will not stop must be killed outright — otherwise respawnDaemon would just
// dial the still-running stuck daemon and falsely report a restart. `plumb stop`
// deliberately does NOT escalate; it is the graceful path.
func forceKillIfAlive(pid int) {
	if !processAlive(pid) {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	fmt.Printf("Daemon (PID %d) ignored SIGTERM — sending SIGKILL.\n", pid)
	_ = proc.Signal(syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
