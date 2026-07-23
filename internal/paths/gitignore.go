package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureGitignoreEntries makes sure dir/.gitignore lists every entry, writing
// header once above the entries it must append. It is idempotent: only missing
// entries are added, and it is a no-op (returns nil) once all are present. Any
// existing content is preserved, with a trailing newline inserted first if the
// file lacks one. A read error other than "not found" is returned; a
// non-existent file is created. header may be empty to append entries with no
// banner. Best-effort by design — callers that treat it as advisory can ignore
// the error.
func EnsureGitignoreEntries(dir, header string, entries []string) error {
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
	if header != "" && !have[header] {
		b.WriteString(header)
		b.WriteByte('\n')
	}
	for _, e := range missing {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644) //nolint:gosec // G306: .gitignore is a normal repo file; 0644 is intentional
}
