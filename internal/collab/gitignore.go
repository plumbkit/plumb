package collab

import (
	"github.com/plumbkit/plumb/internal/paths"
)

// ensureGitignore makes sure dir/.gitignore excludes collab.db and its SQLite
// sidecar files. Like the topology index, collab.db holds ephemeral, machine-
// local advisory data that must never be committed, even in a workspace that
// deliberately tracks .plumb/. Idempotent: it appends only the missing entries
// and is a no-op once they are all present. Best-effort — the caller logs and
// continues on error.
func ensureGitignore(dir string) error {
	return paths.EnsureGitignoreEntries(dir,
		"# plumb cross-agent sharing (ephemeral; do not commit)",
		[]string{"collab.db", "collab.db-wal", "collab.db-shm"})
}
