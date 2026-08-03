package tools

// find_files_details.go holds find_files' entry model, ordering, and the two
// output renderings. The detailed one is list_directory's, carried over intact
// when that tool was folded into find_files: a [FILE]/[DIR]/[LINK] marker, a
// size column, a modified column, and a directory/file tally.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// findFileHit is one entry find_files matched. The stat-derived fields are
// filled in only when the call needs them (include_details, or a size/modified
// sort) — an extra lstat per hit is not worth paying for a plain path list.
type findFileHit struct {
	rel      string
	isDir    bool
	symlink  bool
	target   string // symlink target, raw as stored; empty for non-links
	size     int64
	modified int64 // UnixNano
}

// newFindFileHit builds a hit, stat-ing the entry only when stat is true. A
// failed stat leaves the zero values rather than dropping the entry: the path
// is still a true result, only its metadata is unavailable.
func newFindFileHit(path, rel string, d fs.DirEntry, isDir, stat bool) findFileHit {
	h := findFileHit{rel: rel, isDir: isDir}
	if !stat {
		return h
	}
	if fi, err := d.Info(); err == nil {
		// A directory's stat size is its inode's own bookkeeping, not the size of
		// anything the caller can see: the detailed rendering leaves the column
		// blank for a directory, so ranking one by it would order the list by a
		// number that is never shown. Zero keeps directories in walk order among
		// themselves under sort_by="size".
		if !isDir {
			h.size = fi.Size()
		}
		h.modified = fi.ModTime().UnixNano()
	}
	if d.Type()&os.ModeSymlink != 0 {
		h.symlink = true
		if tgt, err := os.Readlink(path); err == nil {
			h.target = tgt
		}
	}
	return h
}

// sortFindFileHits orders the flat result list. The orders are list_directory's
// — directories first for name and size, newest first for modified — applied to
// the whole, possibly recursive, result set rather than to one directory level.
// Under the default type ("file") no directory is ever in the list, so the
// dirs-first rule only fires for a caller who explicitly asked for them.
func sortFindFileHits(hits []findFileHit, sortBy string) {
	switch sortBy {
	case "size":
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].isDir != hits[j].isDir {
				return hits[i].isDir
			}
			return hits[i].size > hits[j].size
		})
	case "modified":
		sort.SliceStable(hits, func(i, j int) bool {
			return hits[i].modified > hits[j].modified
		})
	default: // "name"
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].isDir != hits[j].isDir {
				return hits[i].isDir
			}
			return hits[i].rel < hits[j].rel
		})
	}
}

// formatFindFilesOutput renders a non-empty result set: a bare relative-path
// list by default, or the detailed table when include_details is set.
func formatFindFilesOutput(hits []findFileHit, a findFilesArgs, root string, truncated bool, walkErr error) string {
	var sb strings.Builder
	if a.IncludeDetails {
		writeFindFilesDetailRows(&sb, hits, root)
	} else {
		for _, h := range hits {
			sb.WriteString(h.rel)
			sb.WriteByte('\n')
		}
	}
	writeFindFilesSummary(&sb, a, hits, truncated, walkErr)
	return sb.String()
}

func writeFindFilesDetailRows(sb *strings.Builder, hits []findFileHit, root string) {
	fmt.Fprintf(sb, "%s\n\n", root)
	for _, h := range hits {
		mt := time.Unix(0, h.modified).Format("2006-01-02 15:04")
		name := h.rel
		if h.symlink && h.target != "" {
			name = h.rel + " -> " + h.target
		}
		switch {
		case h.symlink:
			fmt.Fprintf(sb, "[LINK] %-40s  %12s  %s\n", name, "", mt)
		case h.isDir:
			fmt.Fprintf(sb, "[DIR]  %-40s  %12s  %s\n", name, "", mt)
		default:
			fmt.Fprintf(sb, "[FILE] %-40s  %12s  %s\n", name, textfmt.HumanBytes(h.size), mt)
		}
	}
}

func writeFindFilesSummary(sb *strings.Builder, a findFilesArgs, hits []findFileHit, truncated bool, walkErr error) {
	count := findFilesCountLabel(a, hits)
	switch {
	case truncated:
		// The tally leads here too: a truncation note ALONE loses the one number
		// every other branch reports, and a detailed listing would lose its
		// directory/file split entirely.
		fmt.Fprintf(sb, "\n%s (truncated at %d result%s — use a more specific pattern or set max_depth)",
			count, a.MaxResults, textfmt.Plural(a.MaxResults, "", "s"))
	case errors.Is(walkErr, context.DeadlineExceeded):
		fmt.Fprintf(sb, "\n%s (partial — walk timed out after %s; narrow with path or max_depth)", count, findFilesDefaultDeadline)
	case walkErr != nil:
		fmt.Fprintf(sb, "\n%s (partial — walk stopped: %v)", count, walkErr)
	default:
		fmt.Fprintf(sb, "\n%s", count)
	}
}

// findFilesCountLabel is "N result(s)" for the plain list and list_directory's
// "N directories, M files" tally for the detailed one. A symlink counts as
// neither, exactly as list_directory counted it.
func findFilesCountLabel(a findFilesArgs, hits []findFileHit) string {
	if !a.IncludeDetails {
		return fmt.Sprintf("%d result(s)", len(hits))
	}
	dirs, files := 0, 0
	for _, h := range hits {
		switch {
		case h.symlink: // counted as neither
		case h.isDir:
			dirs++
		default:
			files++
		}
	}
	return fmt.Sprintf("%d director%s, %d file%s",
		dirs, textfmt.Plural(dirs, "y", "ies"),
		files, textfmt.Plural(files, "", "s"),
	)
}
