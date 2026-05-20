package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/golimpio/plumb/internal/monitor"
	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
)

// Version is set by the cli package before calling Run so it appears in the header.
var Version string

// pollMsg is sent by the periodic refresh tick.
type pollMsg struct{}

type scrollBounds struct {
	maxDash        int
	maxLeft        int
	maxRight       int
	maxPopupLeft   int
	maxPopupDetail int
	maxLogDetail   int
}

type logDetailCopyResetMsg struct{}

// Model is the root Bubble Tea model for the sessions dashboard.
type Model struct {
	scrollBounds    *scrollBounds
	sessions        []session.Info
	globalDB        *stats.DB
	statsErr        string
	toolStats       []stats.ToolStat
	recentCalls     []stats.RecentCall
	activity        stats.ActivitySummary
	tokenSavings    int64
	daemonMetrics   monitor.DaemonMetrics
	daemonMetricsOK bool
	daemonCPU       []float64
	cursor          int
	statsCursor     int
	toolStatsCursor int
	focusPanel      panelFocus
	leftScroll      int
	rightScroll     int
	leftWidth       int
	width           int
	height          int
	ready           bool
	draggingDivider bool
	lastActivityAt  time.Time
	activitySession string // DEPRECATED: no longer used for activity caching since it's global
	loadErr         string

	// UI Overlays
	showPopup bool
	showHelp  bool

	sectionMenuOpen   bool
	sectionMenuCursor int
	currentSection    int
	rightTab          int // 0=Details 1=Tools 2=History 3=Diagnostics

	popupTool         string
	popupCalls        []stats.RecentCall
	popupCallCursor   int
	popupRightFocus   bool
	popupDetailScroll int
	popupLeftScroll   int
	popupLeftWidth    int
	popupDetail       popupDetailCache

	statsTableBodyRow     int
	recentTableBodyRow    int
	lastDiagnosticsOutput string

	// Control socket path for live daemon queries.
	ctrlPath string

	// Log viewer state (Logs section, index 3).
	logPath    string
	logEntries []logEntry
	logFilter  string
	logScroll  int
	logCursor  int
	logOffset  int64
	logFollow  bool // true = auto-scroll to the newest entry
	logInitd   bool // true = initLogTail has been called

	logDetailOpen   bool
	logDetailScroll int
	logDetailCopied bool

	// Dashboard section (section 0).
	dashLifetimeCalls    int64
	dashLifetimeSessions int64
	dashLifetimeTokens   int64
	dashLifetimeFirstAt  time.Time
	dashLifetimeTopTools []stats.ToolStat
	dashUptimeTopTools   []stats.ToolStat
	dashLifetimeBuckets  []int64
	dashDaemBuckets      []int64
	dashChartWidth       int
	dashProjectFolder    string
	dashProjectCalls     int64
	dashProjectSessions  int64
	dashProjectTokens    int64
	dashProjectTopTools  []stats.ToolStat
	dashScroll           int
}

func NewModel(logPath, ctrlPath string) Model {
	m := Model{
		leftWidth:         defaultLeftWidth,
		currentSection:    0,
		sectionMenuCursor: 0,
		logPath:           logPath,
		ctrlPath:          ctrlPath,
		logFollow:         true,
		dashProjectFolder: detectWorkspaceFolder(),
		scrollBounds:      &scrollBounds{},
	}
	m.refresh()
	return m
}

func Run(logPath, ctrlPath string) error {
	RebuildStyles()
	p := tea.NewProgram(NewModel(logPath, ctrlPath))
	_, err := p.Run()
	return err
}
