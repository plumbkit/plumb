package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tui"
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

	sessions, hidden := filterSessions(all, sessionsFlagAll)
	if err := renderSessions(sessions, hidden); err != nil {
		return err
	}
	return nil
}

func filterSessions(all []session.Info, includeAll bool) ([]session.Info, int) {
	var sessions []session.Info
	hidden := 0
	for _, s := range all {
		if s.Folder == "" && !includeAll {
			hidden++
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions, hidden
}

func renderSessions(sessions []session.Info, hidden int) error {
	if len(sessions) == 0 {
		if hidden > 0 {
			fmt.Printf("No sessions with a resolved workspace. (%d pending — use --all to show)\n", hidden)
		} else {
			fmt.Println("No active sessions.")
		}
		return nil
	}

	tui.RebuildStyles()

	t := render.DottedTableBase(tui.SepStyle, tui.HintStyle).
		Headers("ID", "Name", "Language", "Folder", "Adapter", "PID", "Started")

	for _, s := range sessions {
		folder := render.ContractPath(s.Folder)
		if folder == "" {
			folder = "(resolving…)"
		}
		t.Row(
			shortSessionID(s.ID),
			s.Name,
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
