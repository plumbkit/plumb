package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
)

// Version is set by the cli package before calling Run so it appears in the header.
var Version string

const (
	defaultLeftWidth = 26
	minLeftWidth     = 16
	pollInterval     = 2 * time.Second
)

// pollMsg is sent by the periodic refresh tick.
type pollMsg struct{}

// panelFocus identifies which panel consumes navigation keys.
type panelFocus int

const (
	focusSessions panelFocus = iota // j/k moves the session cursor (default)
	focusStats                      // j/k moves the recent-calls cursor
)

// Model is the root Bubble Tea model for the sessions dashboard.
// Concurrency: single goroutine (Bubble Tea runtime).
type Model struct {
	sessions       []session.Info
	hiddenCount    int                  // sessions filtered out for lacking a workspace
	statsDBs       map[string]*stats.DB // cached per-workspace DBs (read-only)
	toolStats      []stats.ToolStat     // stats for currently selected session
	recentCalls    []stats.RecentCall
	cursor         int        // index into m.sessions
	statsCursor    int        // index into m.recentCalls when focusPanel == focusStats
	focusPanel     panelFocus // which panel j/k controls
	showCallDetail bool       // when true, right panel shows full detail for selected recent call
	leftWidth      int        // width of left panel content column (excludes border chars)
	width          int
	height         int
	ready          bool
	loadErr        string
	showHidden     bool // toggle to display unresolved sessions
}

// NewModel returns the initial model, loading sessions immediately.
func NewModel() Model {
	m := Model{leftWidth: defaultLeftWidth, statsDBs: make(map[string]*stats.DB)}
	m.refresh()
	return m
}

func (m *Model) refresh() {
	all, err := session.List()
	if err != nil {
		m.loadErr = err.Error()
		return
	}
	m.loadErr = ""

	// Filter out sessions that haven't resolved a workspace yet — they're
	// noise (e.g. dormant Claude Desktop connections). The TUI shows only
	// what's actively being worked on.
	var visible []session.Info
	hidden := 0
	for _, s := range all {
		if s.Folder == "" && !m.showHidden {
			hidden++
			continue
		}
		visible = append(visible, s)
	}
	m.sessions = visible
	m.hiddenCount = hidden

	if m.cursor >= len(m.sessions) && m.cursor > 0 {
		m.cursor = len(m.sessions) - 1
	}
	m.refreshStats()
}

// dbFor returns the read-only stats DB for a workspace, opening it lazily.
// Returns nil if no DB exists yet (no calls recorded for that workspace).
//
// We only cache non-nil handles. If the DB file doesn't exist yet — for
// example, the TUI was opened before the first tool call created
// <workspace>/.plumb/stats.db — caching nil would freeze the panel at
// "No calls recorded yet" forever, because the daemon creates the file
// after we've already stored a nil. Re-attempting Open each poll while
// nil is cheap (one os.Stat) and lets the TUI pick up writes once they
// start.
func (m *Model) dbFor(workspace string) *stats.DB {
	if workspace == "" {
		return nil
	}
	if db, ok := m.statsDBs[workspace]; ok && db != nil {
		return db
	}
	db, _ := stats.OpenReadOnly(stats.DBPathFor(workspace))
	if db != nil {
		m.statsDBs[workspace] = db
	}
	return db
}

func (m *Model) refreshStats() {
	if len(m.sessions) == 0 {
		m.toolStats = nil
		m.recentCalls = nil
		return
	}
	s := m.sessions[m.cursor]
	db := m.dbFor(s.Folder)
	if db == nil {
		m.toolStats = nil
		m.recentCalls = nil
		return
	}
	filter := stats.Filter{SessionID: s.ID}
	m.toolStats, _ = db.Summary(filter)
	m.recentCalls, _ = db.Recent(50, filter)
	if m.statsCursor >= len(m.recentCalls) {
		m.statsCursor = 0
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pollMsg:
		m.refresh()
		return m, tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true

	case tea.MouseClickMsg:
		// Rows: 0=header, 1=frame-top, 2..height-3=content, height-2=frame-bottom, height-1=footer
		contentRow := msg.Y - 2
		bodyHeight := m.height - 4
		if contentRow >= 0 && contentRow < bodyHeight {
			// Cols: 0=│, 1..leftWidth=left-panel, leftWidth+1=┆, leftWidth+2..=right-panel
			if msg.X >= 1 && msg.X <= m.leftWidth {
				// leftLines: index 0=blank, index 1+=sessions
				sessionIdx := contentRow - 1
				if sessionIdx >= 0 && sessionIdx < len(m.sessions) {
					m.cursor = sessionIdx
					m.focusPanel = focusSessions
					m.refreshStats()
				}
			}
		}

	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelDown {
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}
		} else if msg.Button == tea.MouseWheelUp {
			if m.cursor > 0 {
				m.cursor--
			}
		}

	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			// Close call detail view if open; otherwise do nothing.
			m.showCallDetail = false
		case "enter":
			// Open detail view for the selected recent call.
			if m.focusPanel == focusStats && len(m.recentCalls) > 0 {
				m.showCallDetail = true
			}
		case "tab":
			// Toggle focus between the sessions list (left) and the
			// recent-calls list (right). Only meaningful when the right
			// panel actually has rows to navigate.
			m.showCallDetail = false
			if m.focusPanel == focusSessions && len(m.recentCalls) > 0 {
				m.focusPanel = focusStats
			} else {
				m.focusPanel = focusSessions
			}
		case "up", "k":
			if m.focusPanel == focusStats {
				if m.statsCursor > 0 {
					m.statsCursor--
					m.showCallDetail = false
				}
			} else if m.cursor > 0 {
				m.cursor--
				m.refreshStats()
			}
		case "down", "j":
			if m.focusPanel == focusStats {
				if m.statsCursor < len(m.recentCalls)-1 {
					m.statsCursor++
					m.showCallDetail = false
				}
			} else if m.cursor < len(m.sessions)-1 {
				m.cursor++
				m.refreshStats()
			}
		case "a":
			m.showHidden = !m.showHidden
			m.refresh()
		case "[":
			m.leftWidth -= 2
			if m.leftWidth < minLeftWidth {
				m.leftWidth = minLeftWidth
			}
		case "]":
			m.leftWidth += 2
			maxLeft := m.width - 23
			if maxLeft < minLeftWidth {
				maxLeft = minLeftWidth
			}
			if m.leftWidth > maxLeft {
				m.leftWidth = maxLeft
			}
		}
	}
	return m, nil
}

func (m Model) View() tea.View {
	content := "Loading…"
	if m.ready {
		content = m.render()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m Model) render() string {
	rightWidth := m.width - m.leftWidth - 3 // left-│ + ┆ + right-│
	if rightWidth < 10 {
		rightWidth = 10
	}
	bodyHeight := m.height - 4 // header + frame-top + frame-bottom + footer
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	var sb strings.Builder

	// Header
	titleText := "plumb"
	if Version != "" {
		titleText += " " + Version
	}
	title := TitleStyle.Render(titleText)
	hint := HintStyle.Render("↑↓/jk navigate · tab focus panel · enter detail · esc back · a all · [/] resize · q quit")
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(hint)
	if gap < 1 {
		gap = 1
	}
	sb.WriteString(title + strings.Repeat(" ", gap) + hint + "\n")

	// Frame
	sb.WriteString(m.renderTopBorder(rightWidth) + "\n")

	leftLines := m.leftLines()
	rightLines := m.rightLines(rightWidth)
	for i := range bodyHeight {
		l, r := "", ""
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}
		leftCell := lipgloss.NewStyle().Width(m.leftWidth).Render(l)
		rightCell := lipgloss.NewStyle().Width(rightWidth).Render(r)
		sb.WriteString(SepStyle.Render("│") + leftCell + SepStyle.Render("┆") + rightCell + SepStyle.Render("│") + "\n")
	}

	sb.WriteString(m.renderBottomBorder(rightWidth) + "\n")

	// Footer — sum across the visible sessions' per-project DBs.
	var totalCalls, savedTok int64
	for _, s := range m.sessions {
		db := m.dbFor(s.Folder)
		if db == nil {
			continue
		}
		totalCalls += db.TotalCalls(stats.Filter{})
		savedTok += db.TotalTokensSaved(stats.Filter{})
	}
	footer := fmt.Sprintf("%d session(s)  ·  %d tool calls  ·  ~%s tokens saved",
		len(m.sessions), totalCalls, stats.FormatSavings(int(savedTok)))
	if m.hiddenCount > 0 {
		footer += fmt.Sprintf("  ·  %d hidden (press 'a' to show)", m.hiddenCount)
	}
	if m.loadErr != "" {
		footer += "  ·  " + m.loadErr
	}
	sb.WriteString(MutedStyle.Render(footer))

	return sb.String()
}

// renderTopBorder builds: ╭─ Sessions (N) ─────────────┬─ Detail ──────────────╮
func (m Model) renderTopBorder(rightWidth int) string {
	leftTitle := fmt.Sprintf(" Sessions (%d) ", len(m.sessions))
	leftTitleVis := len(leftTitle) // all ASCII
	// left section = 1 opening dash + title + fill, total = m.leftWidth
	leftFill := m.leftWidth - 1 - leftTitleVis
	if leftFill < 0 {
		avail := m.leftWidth - 2
		if avail > 0 {
			leftTitle = leftTitle[:avail] + " "
		} else {
			leftTitle = ""
		}
		leftTitleVis = len(leftTitle)
		leftFill = m.leftWidth - 1 - leftTitleVis
		if leftFill < 0 {
			leftFill = 0
		}
	}

	rightTitle := " Session + Stats "
	rightTitleVis := len(rightTitle)
	// right section = 1 opening dash + title + fill, total = rightWidth
	rightFill := rightWidth - 1 - rightTitleVis
	if rightFill < 0 {
		rightTitle = ""
		rightFill = rightWidth - 1
		if rightFill < 0 {
			rightFill = 0
		}
	}

	return SepStyle.Render("╭─") +
		PanelHeaderStyle.Render(leftTitle) +
		SepStyle.Render(strings.Repeat("─", leftFill)+"┬─") +
		PanelHeaderStyle.Render(rightTitle) +
		SepStyle.Render(strings.Repeat("─", rightFill)+"╮")
}

// renderBottomBorder builds: ╰──────────────────────────┴────────────────────────╯
func (m Model) renderBottomBorder(rightWidth int) string {
	return SepStyle.Render(
		"╰" + strings.Repeat("─", m.leftWidth) + "┴" + strings.Repeat("─", rightWidth) + "╯",
	)
}

// leftLines returns content rows for the left panel.
// Index 0 is blank padding; index 1+ are session items (or empty-state text).
func (m Model) leftLines() []string {
	lines := []string{""}

	if len(m.sessions) == 0 {
		if daemonRunning() {
			lines = append(lines,
				MutedStyle.Render("  Daemon running."),
				MutedStyle.Render("  Call a tool to begin."),
			)
		} else {
			lines = append(lines,
				MutedStyle.Render("  No active sessions."),
				MutedStyle.Render("  Open Claude Desktop."),
			)
		}
		return lines
	}

	for i, s := range m.sessions {
		prefix := "  "
		style := ItemStyle
		if i == m.cursor {
			prefix = "▸ "
			style = SelectedStyle
		}
		langPrefix := s.Language + ": "
		maxFolder := m.leftWidth - 3 - len([]rune(langPrefix))
		if maxFolder < 0 {
			maxFolder = 0
		}
		folder := s.Folder
		if folder == "" {
			folder = "(resolving…)"
		} else {
			folder = contractPath(folder, maxFolder)
		}
		lines = append(lines, style.Render(prefix+langPrefix+folder))
	}
	return lines
}

// rightLines returns content rows for the right panel.
// Index 0 is blank padding; index 1+ are detail rows.
// rightWidth is used to contract long paths so they fit without wrapping.
func (m Model) rightLines(rightWidth int) []string {
	lines := []string{""}

	if len(m.sessions) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("Select a session to view details."))
		return lines
	}

	// ─── Call detail view (enter to open, esc/j/k to close) ──────────────
	if m.showCallDetail && m.focusPanel == focusStats && len(m.recentCalls) > 0 {
		c := m.recentCalls[m.statsCursor]
		status := OkStyle.Render("✓ success")
		if !c.Success {
			status = WarnStyle.Render("✗ failed")
		}
		lines = append(lines,
			"  "+SepStyle.Render("── Call Detail ──"),
			"",
			detailRow("Tool", c.Tool),
			detailRow("Status", status),
			detailRow("Called at", c.CalledAt.Format("2006-01-02 15:04:05")),
			detailRow("Duration", fmt.Sprintf("%d ms", c.DurationMs)),
			detailRow("Input", fmt.Sprintf("%d bytes", c.InputBytes)),
			detailRow("Output", fmt.Sprintf("%d bytes", c.OutputBytes)),
			detailRow("Workspace", contractPath(c.Workspace, rightWidth-16)),
		)
		if c.ErrorMsg != "" {
			lines = append(lines, "", "  "+WarnStyle.Render("Error:"))
			for _, w := range wrapText(c.ErrorMsg, rightWidth-4) {
				lines = append(lines, "    "+WarnStyle.Render(w))
			}
		}
		lines = append(lines, "", "  "+MutedStyle.Render("esc · back to list"))
		return lines
	}

	const keyColWidth = 14
	maxVal := rightWidth - keyColWidth
	if maxVal < 8 {
		maxVal = 8
	}

	s := m.sessions[m.cursor]

	// ─── Session detail ───────────────────────────────────────────────────
	folder := s.Folder
	if folder == "" {
		folder = MutedStyle.Render("(resolving workspace…)")
	} else {
		folder = contractPath(folder, maxVal)
	}
	lines = append(lines,
		detailRow("ID", s.ID),
		detailRow("Language", s.Language),
		detailRow("Folder", folder),
		detailRow("Adapter", s.Adapter),
		detailRow("PID", fmt.Sprintf("%d", s.PID)),
	)
	if s.DaemonVersion != "" {
		lines = append(lines, detailRow("Daemon", s.DaemonVersion))
	}
	lines = append(lines, detailRow("Started", s.StartedAt.Format("2006-01-02 15:04:05")))

	client := s.ClientName
	if s.ClientVersion != "" {
		client += " " + s.ClientVersion
	}
	if client == "" {
		client = MutedStyle.Render("unknown")
	}
	lines = append(lines, detailRow("Client", client))

	// ─── Tool stats ───────────────────────────────────────────────────────
	lines = append(lines, "", "  "+SepStyle.Render("── Tool Statistics ──"))

	if len(m.toolStats) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("No calls recorded yet."))
		return lines
	}

	// Show top 5 tools
	show := m.toolStats
	if len(show) > 5 {
		show = show[:5]
	}
	for _, ts := range show {
		errBadge := ""
		if ts.Errors > 0 {
			errBadge = " " + WarnStyle.Render(fmt.Sprintf("[%d err]", ts.Errors))
		}
		line := fmt.Sprintf("  %-20s %s calls  %s avg%s",
			ts.Tool,
			OkStyle.Render(fmt.Sprintf("%d", ts.Calls)),
			MutedStyle.Render(fmt.Sprintf("%.0fms", ts.AvgMs)),
			errBadge,
		)
		lines = append(lines, line)
	}

	// ─── Recent calls ─────────────────────────────────────────────────────
	if len(m.recentCalls) > 0 {
		header := "── Recent ──"
		if m.focusPanel == focusStats {
			header += "  " + MutedStyle.Render("(tab to leave · j/k navigate)")
		} else {
			header += "  " + MutedStyle.Render("(tab to focus)")
		}
		lines = append(lines, "", "  "+SepStyle.Render(header))
		for i, c := range m.recentCalls {
			ok := OkStyle.Render("✓")
			if !c.Success {
				ok = WarnStyle.Render("✗")
			}
			age := humanAgeTUI(c.CalledAt)
			prefix := "  "
			selected := m.focusPanel == focusStats && i == m.statsCursor
			if selected {
				prefix = "▸ "
			}
			line := fmt.Sprintf("%s%s  %-22s %s %s",
				prefix,
				ok,
				c.Tool,
				MutedStyle.Render(fmt.Sprintf("%dms", c.DurationMs)),
				MutedStyle.Render(age),
			)
			if selected {
				line = SelectedStyle.Render(line)
			}
			lines = append(lines, line)

			// Inline-expand the error message for the selected failed row.
			// Wrapped to the remaining right-panel width so it doesn't push
			// past the panel boundary.
			if selected && !c.Success && c.ErrorMsg != "" {
				for _, w := range wrapText(c.ErrorMsg, rightWidth-6) {
					lines = append(lines, "      "+WarnStyle.Render(w))
				}
			}
		}
	}

	return lines
}

// wrapText breaks s into lines no wider than width, splitting on spaces.
// Words longer than width are kept whole (the line will overflow rather
// than mid-word break). Newlines in s are converted to spaces first so a
// multi-line error renders as one wrapped paragraph.
func wrapText(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	s = strings.ReplaceAll(s, "\n", " ")
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
		} else {
			cur += " " + w
		}
	}
	lines = append(lines, cur)
	return lines
}

func detailRow(key, val string) string {
	return "  " + KeyStyle.Render(key) + ValStyle.Render(val)
}

// contractPath shortens a file path for display:
//  1. Replaces the home directory prefix with ~.
//  2. If still longer than max runes, truncates from the LEFT (keeping the
//     tail of the path) so the meaningful end remains visible:
//     /Users/gilberto/Projects/plumb/testdata → …/plumb/testdata
func contractPath(p string, max int) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		p = "~" + p[len(home):]
	}
	runes := []rune(p)
	if len(runes) <= max {
		return p
	}
	if max <= 1 {
		return "…"
	}
	return "…" + string(runes[len(runes)-(max-1):])
}

func humanAgeTUI(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("Jan 2")
	}
}

// daemonRunning reports whether the plumb daemon socket exists. The socket path
// mirrors cli.daemonSocketPath — duplicated here to avoid an import cycle.
// Must stay in sync with cli.plumbRuntimeDir.
func daemonRunning() bool {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	socketPath := filepath.Join(base, "plumb", "plumb.sock")
	_, err = os.Stat(socketPath)
	return err == nil
}

// Run starts the Bubble Tea sessions dashboard.
func Run() error {
	RebuildStyles()
	p := tea.NewProgram(NewModel())
	_, err := p.Run()
	return err
}
