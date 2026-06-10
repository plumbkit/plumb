package memory

import (
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/redact"
)

// Provenance describes where a generated memory came from. It is serialised into
// the memory's markdown frontmatter (so it survives a memory.db rebuild) and
// mirrored into the index for ranking and display.
type Provenance struct {
	Confidence    Confidence
	SourceSession string
	SourcePaths   []string
	SourceSymbols []string
	SourceCalls   []string
	CreatedAt     time.Time
	StaleAfter    time.Time
}

// WriteGenerated writes a machine-generated memory: it redacts the body, emits a
// provenance frontmatter block, writes the file, and updates the index. The body
// is always passed through redact.Redact first, so a secret captured in a tool
// argument can never reach durable storage. A nil index degrades to a plain write.
func WriteGenerated(ix *Index, workspace, name, description, content string, prov Provenance) error {
	cleaned, _ := redact.Redact(content)
	if prov.Confidence == "" {
		prov.Confidence = ConfidenceGenerated
	}
	if prov.CreatedAt.IsZero() {
		prov.CreatedAt = time.Now()
	}
	full := buildProvenanceFrontmatter(name, description, prov) + cleaned
	// description "" so Write does not inject its own frontmatter — ours is already in full.
	if err := Write(workspace, name, full, ""); err != nil {
		return err
	}
	if ix == nil {
		return nil
	}
	if rec, err := recordFromFile(workspace, name); err == nil {
		_ = ix.Upsert(rec)
	}
	return nil
}

func buildProvenanceFrontmatter(name, description string, prov Provenance) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("name: " + name + "\n")
	if description != "" {
		sb.WriteString("description: " + strings.ReplaceAll(description, "\n", " ") + "\n")
	}
	sb.WriteString("confidence: " + string(prov.Confidence) + "\n")
	// `paths:` makes the generated memory hint-match the files it was distilled
	// from (deduped) — the same frontmatter key a hand-written memory uses for
	// auto-attach. source_paths below is the raw provenance trail (kept verbatim).
	writeListLine(&sb, "paths", dedupeStrings(prov.SourcePaths))
	writeListLine(&sb, "source_session", []string{prov.SourceSession})
	writeListLine(&sb, "source_paths", prov.SourcePaths)
	writeListLine(&sb, "source_symbols", prov.SourceSymbols)
	writeListLine(&sb, "source_calls", prov.SourceCalls)
	if !prov.CreatedAt.IsZero() {
		sb.WriteString("created_at: " + prov.CreatedAt.Format(time.RFC3339) + "\n")
	}
	if !prov.StaleAfter.IsZero() {
		sb.WriteString("stale_after: " + prov.StaleAfter.Format(time.RFC3339) + "\n")
	}
	sb.WriteString("---\n\n")
	return sb.String()
}

// dedupeStrings returns values with blanks dropped and first-seen order
// preserved. Used for the generated `paths:` glob list.
func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func writeListLine(sb *strings.Builder, key string, values []string) {
	var nonEmpty []string
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			nonEmpty = append(nonEmpty, v)
		}
	}
	if len(nonEmpty) == 0 {
		return
	}
	sb.WriteString(key + ": " + strings.Join(nonEmpty, ", ") + "\n")
}

// ReadMeta returns a memory's record (description, paths, and any provenance
// parsed from frontmatter) without going through the index. Used to display a
// provenance footer for a generated memory.
func ReadMeta(workspace, name string) (Record, error) {
	return recordFromFile(workspace, name)
}
