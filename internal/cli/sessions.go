package cli

import (
	"fmt"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/tui"
)

var sessionsFlagAll bool

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "List active plumb sessions",
	RunE:  runSessions,
}

func init() {
	sessionsCmd.Flags().BoolVar(&sessionsFlagAll, "all", false, "include sessions without a resolved workspace")
}

func runSessions(_ *cobra.Command, _ []string) error {
	PrintLogo()

	all, err := session.List()
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	var sessions []session.Info
	hidden := 0
	for _, s := range all {
		if s.Folder == "" && !sessionsFlagAll {
			hidden++
			continue
		}
		sessions = append(sessions, s)
	}

	if len(sessions) == 0 {
		if hidden > 0 {
			fmt.Printf("No sessions with a resolved workspace. (%d pending — use --all to show)\n", hidden)
		} else {
			fmt.Println("No active sessions.")
		}
		return nil
	}

	tui.RebuildStyles()

	t := table.New().
		Border(DottedBorder).
		BorderRow(false).
		BorderColumn(false).
		BorderLeft(false).
		BorderRight(false).
		BorderTop(true).
		BorderBottom(false).
		BorderStyle(tui.SepStyle).
		Headers("id", "language", "folder", "adapter", "pid", "started").
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().PaddingRight(2)
			if row == table.HeaderRow {
				return s.Inherit(tui.HintStyle)
			}
			return s
		})

	for _, s := range sessions {
		folder := contractSessionPath(s.Folder)
		if folder == "" {
			folder = "(resolving…)"
		}
		t.Row(
			s.ID,
			s.Language,
			folder,
			s.Adapter,
			fmt.Sprintf("%d", s.PID),
			s.StartedAt.Format("2006-01-02 15:04:05"),
		)
	}
	fmt.Println(t.Render())

	if hidden > 0 {
		fmt.Printf("\n(%d session(s) hidden — pending workspace; use --all to show)\n", hidden)
	}
	return nil
}

// contractSessionPath shortens a path for terminal display.
func contractSessionPath(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(p, home) {
		p = "~" + p[len(home):]
	}
	return p
}
