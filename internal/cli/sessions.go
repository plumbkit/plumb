package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/session"
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
	PrintLogo("s ᴇ s s ɪ ᴏ ɴ s")

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

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tLANGUAGE\tFOLDER\tADAPTER\tPID\tSTARTED")
	fmt.Fprintln(w, strings.Repeat("-", 8)+"\t"+
		strings.Repeat("-", 8)+"\t"+
		strings.Repeat("-", 6)+"\t"+
		strings.Repeat("-", 7)+"\t"+
		strings.Repeat("-", 3)+"\t"+
		strings.Repeat("-", 7))

	for _, s := range sessions {
		folder := contractSessionPath(s.Folder)
		if folder == "" {
			folder = "(resolving…)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			s.ID,
			s.Language,
			folder,
			s.Adapter,
			s.PID,
			s.StartedAt.Format("2006-01-02 15:04:05"),
		)
	}
	if err := w.Flush(); err != nil {
		return err
	}
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
