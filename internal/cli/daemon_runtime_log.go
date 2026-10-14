package cli

import (
	"log/slog"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/paths"
)

// logResolvedRuntimeLocation says which mode this daemon resolved and where it
// will serve from, in one line, because "where is my daemon?" is the first
// question an operator asks and the answer is not derivable from the pid file
// they are holding.
//
// The two fields can disagree in exactly one case — a `plumb daemon` started by
// hand, which takes its locks before it can read any configuration — and that is
// the case the line exists to make visible rather than mysterious.
//
// It lives here rather than in daemon.go because that file is at its line cap:
// the rule the cap enforces is about responsibility, and "what this daemon
// resolved and where it serves" is a self-contained thing to say.
func logResolvedRuntimeLocation() {
	slog.Info("daemon: runtime location resolved",
		"account_authority", account.AuthorityResolved(),
		"runtime_dir", paths.RuntimeDir())
}
