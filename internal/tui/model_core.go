package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/memory"
	"github.com/golimpio/plumb/internal/monitor"
	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
	"github.com/golimpio/plumb/internal/topology"
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

	// Topology index status for the selected session's workspace, fetched
	// read-only per refresh from <folder>/.plumb/topology.db (zero when absent).
	topoStatus       topology.Status
	topoStatusOK     bool      // true when the selected workspace has an on-disk index
	topoStatusFolder string    // folder topoStatus was fetched for
	topoStatusAt     time.Time // when topoStatus was last fetched (debounce guard)

	// UI Overlays
	showPopup bool
	showHelp  bool

	renameModal     *renameSessionModal
	renameSessionFn func(string) (string, error)

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

	// Memory section (section 2).
	memories            []memory.Memory
	memoryCursor        int
	memoryBodyCache     string
	memoryBodyCacheName string

	// Settings section (section 4) — grouped settings screen + theme popup.
	settingsCfg       config.Config // global config snapshot, loaded on entering the section
	settingsItems     []settingItem // selectable rows (group headers excluded)
	settingsCursor    int           // index into settingsItems for the highlighted row
	settingsScroll    int           // first visible scrollable line in the settings list
	settingsStatus    string        // transient status line ("saved", "applies on restart", …)
	showThemePicker   bool          // theme-picker popup overlay open
	themePickerCursor int           // index into ThemeNames() for the highlighted theme

	// Dashboard section (section 0).
	dashLifetimeCalls       int64
	dashLifetimeSessions    int64
	dashLifetimeTokens      int64
	dashLifetimeFirstAt     time.Time
	dashLifetimeTopTools    []stats.ToolStat
	dashUptimeTopTools      []stats.ToolStat
	dashLifetimeBuckets     []int64
	dashDaemBuckets         []int64
	dashChartWidth          int
	dashCachedLifetimeCalls int64
	dashCachedDaemCalls     int64
	dashCachedChartWidth    int
	dashLastBucketRefresh   time.Time
	dashProjectFolder       string
	dashProjectCalls        int64
	dashProjectSessions     int64
	dashProjectTokens       int64
	dashProjectTopTools     []stats.ToolStat
	dashScroll              int
	waitingForQuit          bool
	quitMessageID           int
}

func NewModel(logPath, ctrlPath string) Model {
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Defaults()
	}
	m := Model{
		leftWidth:         defaultLeftWidth,
		currentSection:    0,
		sectionMenuCursor: 0,
		logPath:           logPath,
		ctrlPath:          ctrlPath,
		logFollow:         true,
		dashProjectFolder: detectWorkspaceFolder(),
		scrollBounds:      &scrollBounds{},
		settingsCfg:       cfg,
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
