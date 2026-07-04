package collab

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ensureGitignore makes sure dir/.gitignore excludes collab.db and its SQLite
// sidecar files. Like the topology index, collab.db holds ephemeral, machine-
// local advisory data that must never be committed, even in a workspace that
// deliberately tracks .plumb/. Idempotent: it appends only the missing entries
// and is a no-op once they are all present. Best-effort — the caller logs and
// continues on error.
func ensureGitignore(dir string) error {
	const header = "# plumb cross-agent sharing (ephemeral; do not commit)"
	entries := []string{"collab.db", "collab.db-wal", "collab.db-shm"}

	path := filepath.Join(dir, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read .gitignore: %w", err)
	}

	have := make(map[string]bool)
	for line := range strings.SplitSeq(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}

	var missing []string
	for _, e := range entries {
		if !have[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteByte('\n')
	}
	if !have[header] {
		b.WriteString(header)
		b.WriteByte('\n')
	}
	for _, e := range missing {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644) //nolint:gosec // G306: .gitignore is a normal repo file; 0644 is intentional
}
