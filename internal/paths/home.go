package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandHome expands environment variables in s ($HOME, $GOPATH, …) and then a
// leading "~" or "~/" to the current user's home directory, so a path written
// into config is portable across machines. Environment expansion runs first, so
// a variable that resolves to a tilde path still expands. Returns s (after env
// expansion) unchanged when the home directory cannot be resolved.
func ExpandHome(s string) string {
	s = os.ExpandEnv(s)
	switch {
	case s == "~":
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	case strings.HasPrefix(s, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, s[2:])
		}
	}
	return s
}
